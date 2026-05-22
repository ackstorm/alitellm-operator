// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// teamFinalizer is the finalizer name managed by the LiteLLMTeam reconciler.
// Wires the protected-default branch (re-applies the implicit
// empty body via POST /team/update — NEVER POST /team/delete — when
// metadata.name=="default"). will replace the non-default
// branch's placeholder with the real POST /team/delete drain.
const teamFinalizer = "teams.litellm.ackstorm.ai/finalizer"

// teamKind is the metric label for LiteLLMTeam CRs (already in metrics.allKinds —
// see internal/metrics/metrics.go line 183).
const teamKind = "LiteLLMTeam"

// teamAliasDefault is the singleton implicit-default Team alias
// (spec §6.7 / TEAM-07). Shared with the synthetic enqueue helper.
const teamAliasDefault = "default"

// rateLimitTypeBestEffort is the only rpm_limit_type / tpm_limit_type
// value supported by LiteLLM 1.83.10 (Feature 01 §2.1). Operator
// hardcodes it whenever the corresponding *_limit is non-null.
const rateLimitTypeBestEffort = "best_effort_throughput"

// TeamSecretRefIndexField is the field indexer path registered in
// cmd/main.go for reverse-mapping Secret names back to Teams that
// reference them (Phase 3 D-06 pattern carry-forward for SEC-09 rotation
// propagation). The literal path string is identical to MCPServer's
// because field indexers are scoped per-type.
const TeamSecretRefIndexField = ".spec.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexTeamSecretRefs is the field indexer function for
// TeamSecretRefIndexField. Mirrors IndexMCPServerSecretRefs verbatim,
// specialized for the LiteLLMTeam type.
func IndexTeamSecretRefs(o client.Object) []string {
	team, ok := o.(*litellmv1alpha1.LiteLLMTeam)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(team.Spec.Secrets))
	for _, s := range team.Spec.Secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// Events RBAC marker inheritance (Phase 5 Task 0 audit, recorded
// in 05-01-SUMMARY.md): the package-wide
// `+kubebuilder:rbac:groups="",resources=events,verbs=create;patch` marker
// lives on internal/controller/mcpserver_controller.go. kubebuilder marker
// scope is per-package — TeamReconciler INHERITS the events grant and
// MUST NOT duplicate it (duplication is a no-op but obscures the single
// source of truth).

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmteams,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmteams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmteams/finalizers,verbs=update

// TeamReconciler reconciles LiteLLMTeam CRs against LiteLLM 1.83.10 per spec §6.7
// + §7.4 and Phase 5 CONTEXT.md D-01.D-10 (inherited as Phase 6 baseline).
//
// State machine (per-reconcile) — Pipeline A. Mirrors MCPServerReconciler
// shape with Team-specific divergences (see Step list below):
//
// - Step 1: Fetch the CR (NotFound → return nil).
// - Step 1.5: DeletionTimestamp set → return nil. Finalizer wiring lives
// in (deletion path + AddFinalizer). reconciles
// non-deleting Teams only.
// - Step 2: Connection-gating per Phase 3 D-08: !snap.Ready → writeStatus
// (LiteLLMUnavailable, echo-reason) → return nil. Zero LiteLLM calls.
// - Step 2.5: SEC-03 uniqueness of spec.secrets[].as values (runtime
// check mirroring Phase 3 LiteLLMModel / Phase 5).
// - Step 3: Resolve spec.secrets[] → secretMap. Missing Secret or
// missing key → SecretNotFound, zero LiteLLM calls.
// - Step 4: Decode spec.params + single-pass substitution. Team uses
// single-pass (only spec.params has placeholders — there is no
// spec.agentCard sibling bag like A2A's D-04 two-pass).
// - Step 5: SEC-07 UnusedSecretRef Event for each declared `as` not
// referenced by any {{NAME}} match in spec.params.
// - Step 6: ProjectionOverride emission for seven structural-overlay
// collision keys (spec §6.7 + Feature 01 §2.1):
// (a) team_alias — overlay wins; metadata.name replaces user value.
// (b) max_budget — overlay wins; spec.budget.limit OR JSON null.
// (c) budget_duration — overlay wins; spec.budget.period OR JSON null.
// (d) rpm_limit — overlay wins; spec.rateLimits.rpm OR JSON null.
// (e) tpm_limit — overlay wins; spec.rateLimits.tpm OR JSON null.
// (f) rpm_limit_type — overlay hardcodes "best_effort_throughput"
// iff rpm_limit non-null; key absent on the wire otherwise.
// (g) tpm_limit_type — overlay hardcodes "best_effort_throughput"
// iff tpm_limit non-null; key absent on the wire otherwise.
// team_id is NOT a collision point — TeamSpec has no team_id field, and
// the overlay is server-assigned on CREATE / pinned-by-status on UPDATE.
// - Step 7: Build the merged body as map[string]any (NOT the typed
// NewTeamRequest struct — its `,omitempty` JSON tags would drop nil
// pointers and violate spec §6.7 line 1194's clearing-budget contract
// which requires explicit null on absent max_budget / budget_duration —
// and equivalently Feature 01 §2.1 for absent rpm_limit / tpm_limit).
// Structural overlays applied AFTER copying paramsMap so they always
// win. 5 keys always-emit (team_alias + max_budget + budget_duration +
// rpm_limit + tpm_limit), 2 *_type keys conditional-add (omitted when
// corresponding *_limit is null — encoding/json then drops the key from
// the wire entirely, per Feature 01 §2.1).
// - Step 8: Compute currentRenderedHash (SHA-256 of canonical JSON of
// the merged body, per Phase 3 D-01).
// - Step 9: Hash-equal steady-state short-circuit (no mutation when
// hash + teamID + observedGeneration all match).
// - Step 10: Branch CREATE (ListTeamsByAlias empty) vs UPDATE
// (ListTeamsByAlias non-empty → smallest-team_id duplicate rule per
// spec §7.1). CREATE arm: POST /team/new via CreateTeamRaw (no
// team_id in body — server-assigned). UPDATE arm: POST /team/update
// via UpdateTeamRaw with team_id pinned in body. Wholesale-replace
// per spec §5.1 Q10 — no delete-and-recreate path on LiteLLMTeam.
// - Step 11: Update status (LastRendered.Hash / ParamsKeys / TeamID /
// At + Ready=Synced) on success.
//
// Anti-patterns avoided (Phase 5 PATTERNS.md):
// - NO RequeueAfter anywhere (REL-02 — Team is event-driven only).
// - NO Owns(.) — Team has no child resources.
// - NO finalizer logic.
// - NO synthetic LiteLLMTeam/default enqueue.
// - NO comparison against LiteLLM response (Phase 3 D-01 — operator-side
// hash only; the mock's POST /team/update returns `{}`, which is fine).
// - NO typed NewTeamRequest body construction (spec §6.7 line 1194 — body
// MUST be map[string]any to preserve JSON null for absent budget keys).
type TeamReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMTeam object —
	// Normal/UnusedSecretRef (SEC-07) + Warning/ProjectionOverride
	// (spec §6.7 + Feature 01 §2.1; seven call sites: team_alias,
	// max_budget, budget_duration, rpm_limit, tpm_limit,
	// rpm_limit_type, tpm_limit_type). Non-nil in production;
	// tests pass mgr.GetEventRecorderFor("team-controller").
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger

	// implicitDefaultMu guards the implicitDefault* fields below. The
	// synthetic LiteLLMTeam/default reconcile runs without a
	// Kubernetes CR — there is no status subresource to persist the
	// hash + teamID onto, so the reconciler caches them in-memory.
	// reconcileImplicitDefault reads + writes; the protected-deletion
	// branch reads the teamID as a fallback when status.lastRendered
	// is empty.
	implicitDefaultMu     sync.Mutex
	implicitDefaultHash   string
	implicitDefaultTeamID string
}

// Reconcile implements the Team state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the §7.4 mapping.
func (r *TeamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("team", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var team litellmv1alpha1.LiteLLMTeam
	if err := r.Get(ctx, req.NamespacedName, &team); err != nil {
		if apierrors.IsNotFound(err) {
			// — synthetic LiteLLMTeam/default bootstrap path.
			// When the TeamDefaultRunnable enqueues
			// reconcile.Request{NamespacedName:{Namespace, "default"}}
			// and no Kubernetes LiteLLMTeam/default CR exists, divert to
			// reconcileImplicitDefault which renders + applies the
			// implicit empty body (no budget). The K8s API server is
			// NEVER asked to create a LiteLLMTeam/default CR — spec §7.4
			// line 1313 is explicit on this.
			if req.NamespacedName.Name == teamAliasDefault && req.NamespacedName.Namespace == r.Namespace {
				return r.reconcileImplicitDefault(ctx, logger)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 1.5: Protected-deletion + finalizer-add ─────────
	//
	// When DeletionTimestamp is set, branch on team.Name=="default":
	//
	// (a) default: AC-T4 invariant. Re-apply the implicit empty body via
	// POST /team/update — the team-delete endpoint is NEVER called
	// against the default-aliased team_id. The LiteLLM team aliased
	// `default` is preserved for the lifetime of the operator. Then
	// remove the finalizer so the CR can be garbage-collected.
	// (b) non-default: wires the real team-delete drain.
	// Leaves this as a placeholder that just removes the
	// finalizer (so non-default-deletion tests do not regress).
	//
	// When DeletionTimestamp is zero AND the finalizer is absent, add it
	// (Step 1.6). This was deferred from — wires it
	// in service of the deletion path above.
	if !team.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &team, logger)
	}
	// ─── Step 1.6: Finalizer-add ───────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&team, teamFinalizer) {
		controllerutil.AddFinalizer(&team, teamFinalizer)
		if err := r.Update(ctx, &team); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				// Optimistic-concurrency conflict — next reconcile retries.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		// The Update event will re-enqueue; no further work this pass.
		return ctrl.Result{}, nil
	}

	// ─── Step 2: Connection-gating (Phase 3 D-08) ──────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Ready {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 2.5: SEC-03 uniqueness of spec.secrets[].as values ──────────
	{
		seen := make(map[string]struct{}, len(team.Spec.Secrets))
		for _, entry := range team.Spec.Secrets {
			if _, exists := seen[entry.As]; exists {
				msg := fmt.Sprintf("spec.secrets[]: duplicate as value %q (SEC-03: must be unique within a LiteLLMTeam)", entry.As)
				if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
				}
				metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			seen[entry.As] = struct{}{}
		}
	}

	// ─── Step 3: Resolve Secrets referenced by spec.secrets[] ─────────────
	secretMap := make(map[string]string)
	for _, entry := range team.Spec.Secrets {
		var secret corev1.Secret
		secretKey := types.NamespacedName{
			Namespace: team.Namespace,
			Name:      entry.SecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				msg := team.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
				if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
				}
				metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		val, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			msg := team.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
			if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			return ctrl.Result{}, nil
		}
		secretMap[entry.As] = string(val)
	}

	// ─── Step 4: Decode spec.params + single-pass substitution ────────────
	paramsMap := make(map[string]any)
	if len(team.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(team.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}

	// Team is single-pass on spec.params (no spec.agentCard sibling bag).
	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	if len(missingParams) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as", missingParams[0])
		if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 5: SEC-07 UnusedSecretRef detection ──────────────────────────
	referencedSet := make(map[string]struct{})
	for _, n := range referencedParams {
		referencedSet[n] = struct{}{}
	}
	for _, entry := range team.Spec.Secrets {
		if _, ok := referencedSet[entry.As]; !ok {
			r.Recorder.Eventf(&team, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params",
				entry.As)
		}
	}

	// ─── Step 6: ProjectionOverride emission (seven collision keys) ──────
	//
	// Each Event fires at most once per reconcile pass. Operator structural
	// overlays ALWAYS win over identically-keyed entries in spec.params per
	// spec §6.7 + Feature 01 §2.1; the Event surfaces the override for user
	// observability. The seven call sites MUST each have a DISTINCT message
	// string so consumers can grep on the colliding key (Phase 5 A2A
	// pattern; D-06 acceptance of worst-case 7 events per reconcile).
	//
	// (1) `team_alias` — user-set spec.params.team_alias is overridden by
	// metadata.name (the bare-name-as-alias contract).
	if _, hasUserAlias := paramsMap["team_alias"]; hasUserAlias {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays metadata.name per spec §6.7)",
			"team_alias")
	}
	// (2) `max_budget` — user-set spec.params.max_budget is overridden by
	// spec.budget.limit (operator structural overlay always wins;
	// absent budget → JSON null per §6.7 line 1194).
	if _, hasUserMaxBudget := paramsMap["max_budget"]; hasUserMaxBudget {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.budget.limit per spec §6.7)",
			"max_budget")
	}
	// (3) `budget_duration` — user-set spec.params.budget_duration is
	// overridden by spec.budget.period (operator structural overlay
	// always wins; absent budget → JSON null per §6.7 line 1194).
	if _, hasUserBudgetDuration := paramsMap["budget_duration"]; hasUserBudgetDuration {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.budget.period per spec §6.7)",
			"budget_duration")
	}
	// (4) `rpm_limit` — user-set spec.params.rpm_limit is overridden by
	// spec.rateLimits.rpm (operator structural overlay always wins;
	// absent rateLimits → JSON null per Feature 01 §2.1).
	if _, hasUserRPMLimit := paramsMap["rpm_limit"]; hasUserRPMLimit {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.rateLimits.rpm per Feature 01 §2.1)",
			"rpm_limit")
	}
	// (5) `tpm_limit` — user-set spec.params.tpm_limit is overridden by
	// spec.rateLimits.tpm (operator structural overlay always wins;
	// absent rateLimits → JSON null per Feature 01 §2.1).
	if _, hasUserTPMLimit := paramsMap["tpm_limit"]; hasUserTPMLimit {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.rateLimits.tpm per Feature 01 §2.1)",
			"tpm_limit")
	}
	// (6) `rpm_limit_type` — user-set spec.params.rpm_limit_type is
	// overridden by the operator-hardcoded "best_effort_throughput"
	// value (Feature 01 §1.2 — the *_type fields are not exposed as CR
	// knobs in v1alpha1; only "best_effort_throughput" is supported).
	if _, hasUserRPMLimitType := paramsMap["rpm_limit_type"]; hasUserRPMLimitType {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator hardcodes best_effort_throughput per Feature 01 §1.2)",
			"rpm_limit_type")
	}
	// (7) `tpm_limit_type` — user-set spec.params.tpm_limit_type is
	// overridden by the operator-hardcoded "best_effort_throughput"
	// value (Feature 01 §1.2 — the *_type fields are not exposed as CR
	// knobs in v1alpha1; only "best_effort_throughput" is supported).
	if _, hasUserTPMLimitType := paramsMap["tpm_limit_type"]; hasUserTPMLimitType {
		r.Recorder.Eventf(&team, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator hardcodes best_effort_throughput per Feature 01 §1.2)",
			"tpm_limit_type")
	}

	// ─── Step 7: Build merged body as map[string]any ──────────────────────
	//
	// Start with a copy of paramsMap, then overlay the operator's
	// structural keys so they win on collision. Using map[string]any
	// preserves JSON null on the wire for absent budget / rate-limit keys
	// (spec §6.7 line 1194: max_budget + budget_duration are always present
	// in the body with explicit null when the CR omits spec.budget;
	// Feature 01 §2.1: rpm_limit + tpm_limit are always present with
	// explicit null when the CR omits spec.rateLimits or the corresponding
	// leaf).
	//
	// Capacity hint +7: team_alias + max_budget + budget_duration +
	// rpm_limit + tpm_limit + rpm_limit_type + tpm_limit_type. The two
	// *_type keys are conditional-add (omitted when corresponding *_limit
	// is null) but we hint for the worst case to avoid one map grow.
	body := make(map[string]any, len(paramsMap)+7)
	for k, v := range paramsMap {
		body[k] = v
	}
	body["team_alias"] = team.Name
	// max_budget: nil when absent (preserved as JSON null by encoding/json).
	if team.Spec.Budget != nil && team.Spec.Budget.Limit != nil {
		body["max_budget"] = *team.Spec.Budget.Limit
	} else {
		body["max_budget"] = nil
	}
	// budget_duration: nil when absent (preserved as JSON null).
	if team.Spec.Budget != nil && team.Spec.Budget.Period != "" {
		body["budget_duration"] = team.Spec.Budget.Period
	} else {
		body["budget_duration"] = nil
	}
	// rpm_limit / tpm_limit: always-emit (nil on clear). *_type keys diverge —
	// emit only when corresponding *_limit is non-nil (Feature 01 §2.1); when
	// *_limit is null, OMIT the *_type key from the body (do NOT set to nil)
	// so encoding/json drops it from the wire entirely. The map-key-absence
	// vs map[key]=nil distinction is load-bearing: body["rpm_limit"] = nil
	// round-trips as JSON `null`; delete(body, "rpm_limit_type") (or simply
	// never inserting it) round-trips as the key being ABSENT from the JSON
	// object.
	if team.Spec.RateLimits != nil && team.Spec.RateLimits.RPM != nil {
		body["rpm_limit"] = *team.Spec.RateLimits.RPM
		body["rpm_limit_type"] = rateLimitTypeBestEffort
	} else {
		body["rpm_limit"] = nil
		// rpm_limit_type intentionally OMITTED (not set to nil) when
		// rpm_limit is null — Feature 01 §2.1 contract.
	}
	if team.Spec.RateLimits != nil && team.Spec.RateLimits.TPM != nil {
		body["tpm_limit"] = *team.Spec.RateLimits.TPM
		body["tpm_limit_type"] = rateLimitTypeBestEffort
	} else {
		body["tpm_limit"] = nil
		// tpm_limit_type intentionally OMITTED (not set to nil) when
		// tpm_limit is null — Feature 01 §2.1 contract.
	}
	// CR-10 / D-7.1-10: drop "blocked": false from the body before calling
	// LiteLLM 1.83.10. Sending blocked=false (the schema default) triggers
	// HTTP 403 because LiteLLM 1.83.10 enforces an admin-only restriction on
	// setting the blocked flag at team creation time. The implicit LiteLLMTeam/default
	// (working path) never sends blocked, confirming the delta. Only send
	// blocked when the user explicitly sets it to true (which IS a meaningful
	// operator action — it prevents the team from making LiteLLM calls).
	if v, ok := body["blocked"]; ok {
		blocked, isBool := v.(bool)
		if isBool && !blocked {
			delete(body, "blocked")
		}
	}

	// ─── Step 8: Compute currentRenderedHash (Phase 3 D-01) ───────────────
	canonicalBytes, err := canonicalJSON(body)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("team controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 9: Hash-equal steady state ──────────────────────────────────
	if team.Status.LastRendered.Hash == currentRenderedHash &&
		team.Status.LastRendered.TeamID != "" &&
		team.Status.ObservedGeneration == team.Generation {
		metrics.CRStatusAgeTracker.RecordSuccess(teamKind, team.Name)
		return ctrl.Result{}, nil
	}

	// ─── Step 10: Branch CREATE vs UPDATE ─────────────────────────────────
	//
	// Phase 3 OWN-04 / Phase 5 first-reconcile sentinel: on
	// ObservedGeneration == 0 OR lastRendered.hash == "", drift counters
	// are suppressed (the user's initial registration is not a "drift
	// correction"). The create_missing counter additionally requires
	// ObservedGeneration > 0 (two-gate suppression — defense in depth
	// against future Status-shape changes silently losing the OWN-04
	// suppression).
	firstReconcile := team.Status.ObservedGeneration == 0 || team.Status.LastRendered.Hash == ""

	// Resolve team_id via /v2/team/list?team_alias=. with the spec §7.1
	// smallest-team_id duplicate rule. An empty exact-match slice → CREATE
	// arm (operator owns the alias). Non-empty → UPDATE arm against the
	// existing entry (overwrite-on-collision per AC-DC4 + spec line 1211).
	entries, listErr := snap.Client.ListTeamsByAlias(ctx, team.Name)
	if listErr != nil && !errors.Is(listErr, litellm.ErrNotFound) {
		// Per spec §7.7 line 1432: 404 on a LIST is permanent
		// LiteLLMRejected with message "LiteLLM API surface mismatch on
		// <path>". This is distinct from 4xx-non-404 (also LiteLLMRejected
		// but with a generic message) — the LIST-404 wording surfaces a
		// likely upstream API drift that requires operator inspection.
		// 401 is handled by classifyMutationError (anti-storm fast-path).
		var auth401 *litellm.Auth401Error
		if !errors.As(listErr, &auth401) && is4xxNon401Status(listErr, 404) {
			msg := "LiteLLM API surface mismatch on /v2/team/list"
			if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
			}
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			return ctrl.Result{}, nil
		}
		// 401 → fast-path; other transient → backoff.
		return r.classifyMutationError(ctx, &team, logger, listErr, "GET /v2/team/list")
	}
	// ErrNotFound is treated as empty for CREATE arm.
	if errors.Is(listErr, litellm.ErrNotFound) {
		entries = nil
	}

	var newTeamID string
	if len(entries) == 0 {
		// CREATE arm — operator does not pre-set team_id in the body
		// (server-assigned). Build body without team_id.
		delete(body, "team_id")
		result, cerr := snap.Client.CreateTeamRaw(ctx, body)
		if cerr != nil {
			return r.classifyMutationError(ctx, &team, logger, cerr, "POST /team/new")
		}
		newTeamID = result.TeamID
		// Two-gate first-reconcile suppression:
		// drift_corrected_total{action=create_missing} only increments
		// when !firstReconcile AND ObservedGeneration > 0 (the latter
		// condition is structurally redundant under the former, but is
		// retained verbatim from the Phase 5 pattern as defense in
		// depth against future Status-shape changes).
		if !firstReconcile && team.Status.ObservedGeneration > 0 {
			metrics.DriftCorrectedTotal.WithLabelValues("team", "create_missing").Inc()
		}
		logger.V(1).Info("team created in LiteLLM", "teamID", newTeamID)
	} else {
		// UPDATE arm — apply spec §7.1 smallest-team_id duplicate rule.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].TeamID < entries[j].TeamID
		})
		existing := entries[0]
		// Pin team_id in the body so LiteLLM's required-field schema is
		// satisfied (spec §6.7 — UpdateTeam requires team_id).
		body["team_id"] = existing.TeamID
		if _, uerr := snap.Client.UpdateTeamRaw(ctx, body); uerr != nil {
			return r.classifyMutationError(ctx, &team, logger, uerr, "POST /team/update")
		}
		newTeamID = existing.TeamID
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("team", "update_drifted").Inc()
		}
		logger.V(1).Info("team updated in LiteLLM (wholesale-replace POST /team/update)", "teamID", newTeamID)
	}

	// ─── Step 11: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	team.Status.LastRendered = litellmv1alpha1.TeamLastRenderedStatus{
		Hash:       currentRenderedHash,
		ParamsKeys: sortedKeys(paramsMap),
		TeamID:     newTeamID,
		At:         &now,
	}
	if werr := r.writeStatus(ctx, &team, metav1.ConditionTrue, reasonSynced, "team registered"); werr != nil {
		logStatusUpdateErr(logger, werr, "reason", reasonSynced)
		if apierrors.IsConflict(werr) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// will re-enqueue with fresh state. Avoid surfacing through
			// controller-runtime's ERROR-level "Reconciler error" log
			// and its exponential backoff.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, werr
	}
	metrics.CRStatusAgeTracker.RecordSuccess(teamKind, team.Name)
	metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
	logger.V(1).Info("team reconciled", "teamID", newTeamID, "hash", currentRenderedHash)

	return ctrl.Result{}, nil
}

// reconcileImplicitDefault is invoked from Step 1 when the synthetic
// LiteLLMTeam/default reconcile.Request is processed and no Kubernetes
// LiteLLMTeam/default CR exists. Spec §7.4 line 1313 + §6.7 lines 1215–1229.
//
// The implicit body is fixed:
//
//	{ "team_alias": "default", "max_budget": null, "budget_duration": null }
//
// No spec.params merge, no spec.secrets resolution, no status subresource
// (there is no K8s object to write to — the hash + teamID are cached on
// the reconciler struct under implicitDefaultMu).
//
// Behavior summary:
// - Connection-gate (Phase 3 D-08): !snap.Ready → no LiteLLM call.
// - ListTeamsByAlias("default") → empty: CREATE arm (POST /team/new).
// - ListTeamsByAlias("default") → non-empty: UPDATE arm (POST /team/update)
// with the smallest-team_id (spec §7.1 dedup) and the empty body.
// - drift_corrected_total{action=create_missing} is NOT incremented on
// the very FIRST synthetic reconcile (the implicit default is
// bootstrapping, not correcting drift — same spirit as OWN-04
// first-reconcile suppression). Subsequent CREATE arms (after an
// out-of-band delete in LiteLLM) DO increment the counter.
// - drift_corrected_total{action=update_drifted} increments only if the
// UPDATE arm fires AND the rendered hash differs from the cached
// implicitDefaultHash (i.e. someone mutated the team out-of-band).
func (r *TeamReconciler) reconcileImplicitDefault(ctx context.Context, logger logr.Logger) (ctrl.Result, error) {
	// ─── Connection-gate ──────────────────────────────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Ready {
		// No status subresource to write — the runnable retries on the
		// next ticker fire / Ready transition. Suppress the log when
		// snap.Reason is empty: that's the zero-value cache state during
		// startup (LiteLLMConnection not yet probed), produces high-rate
		// noise under the envtest 100ms TeamDefaultRunnable cadence, and
		// carries no diagnostic value. Reasons that DO matter
		// (BadMasterKey, Unreachable, Absent, SecretNotFound, Connecting)
		// still log at V(1).
		if snap.Reason != "" {
			logger.V(1).Info("reconcileImplicitDefault: connection not Ready; skipping",
				"reason", snap.Reason)
		}
		metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Build implicit body (no params, no overlays beyond structural) ───
	body := map[string]any{
		"team_alias":      teamAliasDefault,
		"max_budget":      nil,
		"budget_duration": nil,
		"rpm_limit":       nil, // Feature 01 §2.1 — always-emit nil on clear (synthetic-default never carries rateLimits)
		"tpm_limit":       nil, // Feature 01 §2.1 — always-emit nil on clear
		// rpm_limit_type / tpm_limit_type INTENTIONALLY ABSENT — conditional-add
		// per Feature 01 §2.1: when *_limit is nil, *_type is OMITTED from the
		// body (not set to nil) so encoding/json drops the key from the wire.
	}

	// ─── Hash + steady-state short-circuit (in-memory cache) ──────────────
	canonicalBytes, err := canonicalJSON(body)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("team controller: implicit-default canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	r.implicitDefaultMu.Lock()
	cachedHash := r.implicitDefaultHash
	cachedTeamID := r.implicitDefaultTeamID
	r.implicitDefaultMu.Unlock()

	// Resolve via ListTeamsByAlias to detect (a) first bootstrap (empty
	// list → CREATE arm) vs (b) UPDATE arm. The list is also our source of
	// truth for the team_id when the cached value is empty (e.g. operator
	// restarted; LiteLLM team aliased `default` already exists).
	entries, listErr := snap.Client.ListTeamsByAlias(ctx, teamAliasDefault)
	if listErr != nil && !errors.Is(listErr, litellm.ErrNotFound) {
		return r.classifyMutationError(ctx, nil, logger, listErr, "GET /v2/team/list (implicit default)")
	}
	if errors.Is(listErr, litellm.ErrNotFound) {
		entries = nil
	}

	// Track whether this is the very first time the implicit reconcile
	// runs (no cached hash → first call since process start). Used to
	// suppress drift_corrected_total{action=create_missing} on the
	// initial bootstrap (mirrors the per-CR OWN-04 first-reconcile spirit).
	firstBootstrap := cachedHash == "" && cachedTeamID == ""

	var newTeamID string
	if len(entries) == 0 {
		// CREATE arm.
		result, cerr := snap.Client.CreateTeamRaw(ctx, body)
		if cerr != nil {
			return r.classifyMutationError(ctx, nil, logger, cerr, "POST /team/new (implicit default)")
		}
		newTeamID = result.TeamID
		if !firstBootstrap {
			metrics.DriftCorrectedTotal.WithLabelValues("team", "create_missing").Inc()
		}
		logger.Info("implicit Team/default reconciled (CREATE)", "teamID", newTeamID)
	} else {
		// UPDATE arm — spec §7.1 smallest-team_id dedup.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].TeamID < entries[j].TeamID
		})
		existing := entries[0]
		// Hash-equal short-circuit: if the rendered body matches the
		// cached one AND the cached teamID matches the resolved one, skip
		// the UPDATE — steady-state no-op (T-06-03-01 mitigation).
		if cachedHash == currentRenderedHash && cachedTeamID == existing.TeamID {
			logger.V(1).Info("implicit Team/default: hash-equal steady state; no LiteLLM call",
				"teamID", existing.TeamID)
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			return ctrl.Result{}, nil
		}
		// Body needs team_id pin for POST /team/update (spec §6.7).
		updateBody := map[string]any{
			"team_alias":      teamAliasDefault,
			"team_id":         existing.TeamID,
			"max_budget":      nil,
			"budget_duration": nil,
			"rpm_limit":       nil, // Feature 01 §2.1 — always-emit nil; required for wholesale-replace clear semantics
			"tpm_limit":       nil, // Feature 01 §2.1 — always-emit nil
			// rpm_limit_type / tpm_limit_type INTENTIONALLY ABSENT (conditional-add).
		}
		if _, uerr := snap.Client.UpdateTeamRaw(ctx, updateBody); uerr != nil {
			return r.classifyMutationError(ctx, nil, logger, uerr, "POST /team/update (implicit default)")
		}
		newTeamID = existing.TeamID
		// drift_corrected_total{action=update_drifted} fires only when
		// the rendered hash differs from the cached one (drift detected).
		// On the very first bootstrap with a pre-existing LiteLLM entry,
		// firstBootstrap=true → no increment (same spirit as OWN-04).
		if !firstBootstrap && cachedHash != currentRenderedHash {
			metrics.DriftCorrectedTotal.WithLabelValues("team", "update_drifted").Inc()
		}
		logger.Info("implicit Team/default reconciled (UPDATE)", "teamID", newTeamID)
	}

	// ─── Update in-memory cache ───────────────────────────────────────────
	r.implicitDefaultMu.Lock()
	r.implicitDefaultHash = currentRenderedHash
	r.implicitDefaultTeamID = newTeamID
	r.implicitDefaultMu.Unlock()

	metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
	return ctrl.Result{}, nil
}

// reconcileDeletion handles Step 1.5 — the protected-default deletion
// branch (AC-T4) and the non-default POST /team/delete drain.
//
// Default-team behavior:
//
// - Resolve teamID from status.lastRendered.teamID; on empty status
// fall back to ListTeamsByAlias("default") + smallest-team_id rule
// (matches the implicit-bootstrap resolve path).
// - POST /team/update with body
// `{team_alias:"default", team_id:<resolved>, max_budget:null, budget_duration:null}` —
// re-applies the implicit empty body so the LiteLLM team aliased
// `default` is preserved per AC-T4.
// - Remove the finalizer + Update; the CR is reaped by K8s GC.
// - NEVER POST /team/delete.
//
// Non-default behavior:
// - Issue POST /team/delete against the resolved teamID.
// - Remove the finalizer + Update.
//
// 401-on-connection: classifyMutationError invalidates the cache. We
// remove the finalizer anyway (anti-storm — the team_id is durable in
// LiteLLM; subsequent reconciles or operator restarts re-resolve via
// ListTeamsByAlias on the bootstrap path).
//
//nolint:gocyclo // Two-branch deletion path (default vs non-default) with classifier-driven cache invalidation; splitting obscures the §7.5 mapping.
func (r *TeamReconciler) reconcileDeletion(ctx context.Context, team *litellmv1alpha1.LiteLLMTeam, logger logr.Logger) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(team, teamFinalizer) {
		// Already drained (or never had the finalizer added) — K8s GC
		// will reap the CR. Nothing to do.
		return ctrl.Result{}, nil
	}

	if team.Name == teamAliasDefault {
		// AC-T4 PROTECTED PATH — re-apply the implicit empty body.
		snap := r.Cache.Snapshot()
		if !snap.Ready {
			// Cannot drain right now; surface as transient and let the
			// connection-Ready event re-enqueue.
			logger.V(1).Info("Team/default deletion: connection not Ready; retrying",
				"reason", snap.Reason)
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			return ctrl.Result{}, nil
		}

		// Resolve team_id. Prefer status.lastRendered.teamID; fall back
		// to ListTeamsByAlias when status is empty (e.g. the CR was
		// created and deleted before the controller could populate
		// status, OR the operator restarted between CR-create and
		// CR-delete and lost the status due to a status-Update race).
		resolvedTeamID := team.Status.LastRendered.TeamID
		if resolvedTeamID == "" {
			entries, listErr := snap.Client.ListTeamsByAlias(ctx, teamAliasDefault)
			if listErr != nil && !errors.Is(listErr, litellm.ErrNotFound) {
				return r.classifyMutationError(ctx, team, logger, listErr,
					"GET /v2/team/list (default-deletion resolve)")
			}
			if len(entries) > 0 {
				sort.Slice(entries, func(i, j int) bool {
					return entries[i].TeamID < entries[j].TeamID
				})
				resolvedTeamID = entries[0].TeamID
			}
		}

		if resolvedTeamID != "" {
			body := map[string]any{
				"team_alias":      teamAliasDefault,
				"team_id":         resolvedTeamID,
				"max_budget":      nil,
				"budget_duration": nil,
				"rpm_limit":       nil, // Feature 01 §2.1 — AC-T4 re-applies implicit empty spec; clears any user-set rateLimits on the wire
				"tpm_limit":       nil, // Feature 01 §2.1 — AC-T4 re-applies implicit empty spec
				// rpm_limit_type / tpm_limit_type INTENTIONALLY ABSENT (conditional-add).
			}
			if _, uerr := snap.Client.UpdateTeamRaw(ctx, body); uerr != nil {
				// 401 fast-path: cache invalidated; remove finalizer
				// anyway (anti-storm — re-bootstrap on next reconcile).
				var auth401 *litellm.Auth401Error
				if errors.As(uerr, &auth401) {
					r.Cache.InvalidateOn401()
					logger.Info("Team/default deletion: 401 on /team/update; cache invalidated, removing finalizer anyway")
				} else {
					// Transient (5xx / network) — bubble up for backoff.
					// 4xx surfaces under classifyMutationError logic
					// but for the deletion path we treat 4xx as
					// terminal: log and proceed to finalizer-remove
					// (avoid stuck-deleted CRs on operator-side bugs).
					if isTransientLiteLLMError(uerr) {
						return ctrl.Result{}, uerr
					}
					logger.Info("Team/default deletion: terminal LiteLLM error on /team/update; removing finalizer anyway",
						"error", uerr.Error())
				}
			} else {
				logger.Info("Team/default deletion: implicit empty spec re-applied; team aliased `default` preserved per AC-T4",
					"teamID", resolvedTeamID)
				// Update in-memory cache so subsequent synthetic ticks
				// observe steady state (the rendered body did not
				// actually change between this UPDATE and the synthetic
				// reconcile's implicit body).
				canonicalBytes, _ := canonicalJSON(map[string]any{
					"team_alias":      teamAliasDefault,
					"max_budget":      nil,
					"budget_duration": nil,
					"rpm_limit":       nil, // CR-01 — must mirror reconcileImplicitDefault CREATE-arm body (line 639) so hash cache aligns
					"tpm_limit":       nil,
				})
				sum := sha256.Sum256(canonicalBytes)
				r.implicitDefaultMu.Lock()
				r.implicitDefaultHash = fmt.Sprintf("%x", sum)
				r.implicitDefaultTeamID = resolvedTeamID
				r.implicitDefaultMu.Unlock()
			}
		} else {
			// No team_id resolvable — LiteLLM has no `default`-aliased
			// team (it was never bootstrapped OR was deleted out-of-band).
			// Nothing to re-apply; remove the finalizer.
			logger.Info("Team/default deletion: no LiteLLM team aliased `default` found; finalizer removed without re-apply")
		}
	} else {
		// Non-default deletion drain. Resolve team_id by
		// preferring status.lastRendered.teamID (Phase 3 D-04 pin); on
		// empty pin, fall back to ListTeamsByAlias + spec §7.1
		// smallest-team_id rule. Empty exact-match is treated as success
		// (the team is already absent). Then POST /team/delete and
		// classify the response per spec §7.5 + §7.7:
		//
		// - 200/2xx → success; increment drift_corrected_total{
		// team,delete_vanished} and proceed to
		// RemoveFinalizer.
		// - 404 on POST → success per spec §7.5 line 1332 ("a 404 on a
		// delete is treated as success: the LiteLLM
		// resource is considered cleaned up"). Same
		// drift_corrected_total increment + finalizer
		// removal.
		// - 4xx non-401 → LiteLLMRejected status write; finalizer is
		// NOT removed (CR stays in Terminating). Next
		// CR event MAY retry. Deterministic per §7.7.
		// - 401 → cache.InvalidateOn401; finalizer is removed
		// anyway (anti-storm — the operator cannot
		// block CR GC on an auth failure that may be
		// permanent). Log warns "LiteLLM entry MAY
		// persist".
		// - 5xx/network → return err for controller-runtime backoff;
		// finalizer NOT removed.
		//
		// 404 on the LIST endpoint is treated as PERMANENT LiteLLMRejected
		// per spec §7.7 line 1432 ("A 404 on a LIST is permanent
		// Ready=False, reason=LiteLLMRejected with message: 'LiteLLM API
		// surface mismatch on <path>'") — NOT success. Finalizer stays.
		//
		// Connection-unavailable at deletion time: warn + remove finalizer
		// anyway (anti-storm — cannot block CR GC on connection failure).
		snap := r.Cache.Snapshot()
		if !snap.Ready {
			logger.Info("LiteLLM unavailable on deletion; finalizer removed; team entry MAY persist until next reconcile with valid connection",
				"team", team.Name, "reason", snap.Reason)
			// Fall through to RemoveFinalizer below (anti-storm).
		} else {
			// Resolve team_id: prefer the status pin, then ListTeamsByAlias.
			teamID := team.Status.LastRendered.TeamID
			if teamID == "" {
				entries, listErr := snap.Client.ListTeamsByAlias(ctx, team.Name)
				if listErr != nil {
					// 401 → fast-path anti-storm (cache invalidate +
					// remove finalizer).
					var auth401 *litellm.Auth401Error
					if errors.As(listErr, &auth401) {
						r.Cache.InvalidateOn401()
						logger.Info("delete-resolve: 401 fast-path; cache invalidated; finalizer removed anyway (anti-storm)",
							"team", team.Name, "path", auth401.Path)
						// Fall through to RemoveFinalizer.
					} else if is4xxNon401Status(listErr, 404) || errors.Is(listErr, litellm.ErrNotFound) {
						// 404 on the LIST endpoint = permanent LiteLLMRejected
						// per spec §7.7 line 1432. Finalizer NOT removed.
						// The GET 404 surfaces as the raw "litellm: 404 on."
						// error (the litellm client wraps 404 as ErrNotFound
						// only for the DELETE method); is4xxNon401Status
						// catches it via the error-string prefix.
						msg := "LiteLLM API surface mismatch on /v2/team/list"
						if werr := r.writeStatus(ctx, team, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
							logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
						}
						metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
						return ctrl.Result{}, nil
					} else if isTransientLiteLLMError(listErr) {
						// 5xx / network — return err for controller-runtime
						// backoff; finalizer stays.
						return ctrl.Result{}, listErr
					} else {
						// 4xx non-401 non-404 — LiteLLMRejected; finalizer
						// NOT removed.
						msg := fmt.Sprintf("LiteLLM rejected GET /v2/team/list: %s", listErr.Error())
						if werr := r.writeStatus(ctx, team, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
							logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
						}
						metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
						return ctrl.Result{}, nil
					}
				} else if len(entries) > 0 {
					// spec §7.1 smallest-team_id rule on the
					// exact-match-filtered entries.
					sort.Slice(entries, func(i, j int) bool {
						return entries[i].TeamID < entries[j].TeamID
					})
					teamID = entries[0].TeamID
				}
				// empty entries → teamID stays "" → falls through to the
				// "already absent" log below.
			}

			if teamID != "" {
				if delErr := snap.Client.DeleteTeam(ctx, []string{teamID}); delErr != nil {
					var auth401 *litellm.Auth401Error
					if errors.As(delErr, &auth401) {
						r.Cache.InvalidateOn401()
						logger.Info("delete: 401 fast-path; cache invalidated; finalizer removed anyway (anti-storm); LiteLLM entry MAY persist",
							"team", team.Name, "teamID", teamID, "path", auth401.Path)
						// Fall through to RemoveFinalizer.
					} else if is4xxNon401Status(delErr, 404) {
						// 404 on POST /team/delete = success per spec
						// §7.5 line 1332. Mirror the happy-path metric.
						metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished").Inc()
						logger.V(1).Info("team already absent on /team/delete (404); finalizer removed",
							"team", team.Name, "teamID", teamID)
						// Fall through to RemoveFinalizer.
					} else if isTransientLiteLLMError(delErr) {
						// 5xx / network — return err for controller-
						// runtime backoff; finalizer NOT removed.
						return ctrl.Result{}, delErr
					} else {
						// 4xx non-401 non-404 — LiteLLMRejected;
						// finalizer NOT removed (CR stays in Terminating
						// per spec §7.7 deterministic permanent failure).
						msg := fmt.Sprintf("LiteLLM rejected POST /team/delete: %s", delErr.Error())
						if werr := r.writeStatus(ctx, team, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
							logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
						}
						metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
						return ctrl.Result{}, nil
					}
				} else {
					// Happy path — POST /team/delete returned 2xx.
					metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished").Inc()
					logger.Info("finalizer removed; LiteLLM team deleted",
						"team", team.Name, "teamID", teamID)
				}
			} else {
				logger.V(1).Info("team already absent in LiteLLM (empty resolve); finalizer removed",
					"team", team.Name)
			}
		}
	}

	// Remove the finalizer + Update.
	// OBS-03: drop the cr_status_age_seconds label before the CR is gone (T-07-01-01).
	metrics.CRStatusAgeTracker.Forget(teamKind, team.Name)
	controllerutil.RemoveFinalizer(team, teamFinalizer)
	if err := r.Update(ctx, team); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
	return ctrl.Result{}, nil
}

// ResetImplicitDefaultCache clears the in-memory implicit-default hash +
// teamID cache on the TeamReconciler.
//
// Phase 6 cross-suite flake fix per 07-CONTEXT.md Claude's Discretion
// §Phase-6-flake option (α) + 06-04-SUMMARY.md.
//
// Root cause: mockServer.ResetTeams clears the mock's team list but does
// NOT reset the in-memory cache populated by reconcileImplicitDefault.
// When a subsequent test runs, the reconciler's stale implicitDefaultHash
// causes reconcileImplicitDefault to skip re-creating the implicit default
// team, leading to flaky failures in TestTeamReconciler_DriftSuppressedOnFirstCreate
// and related tests.
//
// Callers MUST invoke this alongside mockServer.ResetTeams to keep the
// operator's in-memory view and the mock's LiteLLM state coherent.
func (r *TeamReconciler) ResetImplicitDefaultCache() {
	r.implicitDefaultMu.Lock()
	defer r.implicitDefaultMu.Unlock()
	r.implicitDefaultHash = ""
	r.implicitDefaultTeamID = ""
}

// is4xxNon401Status returns true if err is a litellm.Client error whose
// HTTP status equals `wantStatus` (a 4xx code other than 401 — caller's
// responsibility to ensure that constraint). Mirrors the inline error-
// string prefix check used by classifyMutationError and isTransientLiteLLMError
// (the litellm client's makeRequest formats 4xx errors as
// `"litellm: <code> on <method> <path> (code=<code>)"`).
//
// Used by the non-default Team deletion path to distinguish:
// - 404 on POST /team/delete → spec §7.5 line 1332 success (drift counter
// fires, finalizer removed).
// - other 4xx (e.g. 400, 403, 422) → LiteLLMRejected, finalizer NOT removed.
func is4xxNon401Status(err error, wantStatus int) bool {
	if err == nil {
		return false
	}
	prefix := fmt.Sprintf("litellm: %d on", wantStatus)
	errStr := err.Error()
	return len(errStr) >= len(prefix) && errStr[:len(prefix)] == prefix
}

// isTransientLiteLLMError returns true if err looks like a 5xx / network
// transient that should bubble up for controller-runtime exponential
// backoff. 4xx (non-401) is treated as terminal so the finalizer-remove
// path can proceed without leaving the CR stuck in Terminating forever.
func isTransientLiteLLMError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// 4xx → terminal (deterministic).
	for code := 400; code < 500; code++ {
		prefix := fmt.Sprintf("litellm: %d on", code)
		if len(errStr) >= len(prefix) && errStr[:len(prefix)] == prefix {
			return false
		}
	}
	// Default to transient (5xx / network / context).
	return true
}

// classifyMutationError handles §7.7 error classification for LiteLLM
// mutation calls (CreateTeamRaw / UpdateTeamRaw / ListTeamsByAlias):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return
// (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff.
func (r *TeamReconciler) classifyMutationError(ctx context.Context, team *litellmv1alpha1.LiteLLMTeam, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	// team may be nil when called from reconcileImplicitDefault — the
	// synthetic LiteLLMTeam/default reconcile has no K8s CR to write status onto.
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		r.Cache.InvalidateOn401()
		if team != nil {
			msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
			if werr := r.writeStatus(ctx, team, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
			}
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	errStr := err.Error()
	is4xx := false
	for code := 400; code < 500; code++ {
		prefix := fmt.Sprintf("litellm: %d on", code)
		if len(errStr) >= len(prefix) && errStr[:len(prefix)] == prefix {
			is4xx = true
			break
		}
	}

	if is4xx {
		if team != nil {
			msg := fmt.Sprintf("LiteLLM rejected %s: %s", opDesc, errStr)
			if werr := r.writeStatus(ctx, team, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
			}
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", errStr)
		metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// 5xx / network transient — return err for controller-runtime backoff.
	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", errStr)
	metrics.ReconcileTotal.WithLabelValues(teamKind, "error").Inc()
	return ctrl.Result{}, err
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact. Callers MUST ensure no secret material reaches `message`.
func (r *TeamReconciler) writeStatus(
	ctx context.Context,
	team *litellmv1alpha1.LiteLLMTeam,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Uses Update (not Patch + MergeFrom) because callers mutate
	// team.Status.LastRendered in-memory BEFORE invoking writeStatus.
	// A MergeFrom orig captured here would already include the mutation
	// in both sides of the diff, so the patch body would omit
	// LastRendered and the server would keep an empty TeamID. The next
	// reconcile would then see Hash=="" and re-POST /team/new
	// (duplicate-write regression observed in TestTeamHubSeam_AC_DC1
	// and the AC_T3/T6/RateLimitsClearing/ProjectionOverride suite).
	// The 409 conflict noise this Update path can emit is demoted to
	// V(1) by logStatusUpdateErr at each call site.
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: team.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&team.Status.Conditions, cond)
	team.Status.ObservedGeneration = team.Generation
	return r.Status().Update(ctx, team)
}

// secretToTeams maps a Secret update event to the set of LiteLLMTeam CRs that
// reference it via spec.secrets[].secretRef.name (Phase 3 D-06
// rotation-propagation pattern). Uses the field indexer registered in
// cmd/main.go.
func (r *TeamReconciler) secretToTeams(ctx context.Context, obj client.Object) []reconcile.Request {
	var teamList litellmv1alpha1.LiteLLMTeamList
	if err := r.List(ctx, &teamList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{TeamSecretRefIndexField: obj.GetName()},
	); err != nil {
		r.Log.V(1).Info("secretToTeams: list failed; skipping", "error", err)
		return nil
	}
	out := make([]reconcile.Request, 0, len(teamList.Items))
	for i := range teamList.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&teamList.Items[i]),
		})
	}
	return out
}

// SetupWithManager registers the TeamReconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMTeam{}) — primary watch.
// - Watches(&Secret{}, secretToTeams) — SEC-09 rotation propagation
// for placeholders in spec.params.
// - WatchesRawSource(source.TypedFunc) — optional synthetic
// LiteLLMTeam/default request channel. The TeamDefaultRunnable enqueues
// reconcile.Request{NamespacedName:{Namespace, "default"}} onto this
// channel from a wait-for-Ready + 30-min ticker loop (spec §7.4 line
// 1313). The optional-variadic shape mirrors ModelReconciler.
//
// Named("team") — controller registry name.
// No Owns(.) — Team has no child resources.
func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager, requeueCh ...chan reconcile.Request) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMTeam{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToTeams),
		).
		WithOptions(transientBackoffOptions()).
		Named("team")

	// Wire optional LiteLLMTeam/default synthetic reconcile channel as a
	// typed-func source. Mirrors the ModelReconciler Phase 3 	// pattern — drain in a goroutine so controller-runtime's Start
	// completes synchronously.
	if len(requeueCh) > 0 && requeueCh[0] != nil {
		ch := requeueCh[0]
		b = b.WatchesRawSource(source.TypedFunc[reconcile.Request](
			func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case req, ok := <-ch:
							if !ok {
								return
							}
							q.Add(req)
						}
					}
				}()
				return nil
			},
		))
	}

	return b.Complete(r)
}
