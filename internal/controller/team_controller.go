// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
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
// - Step 7: Build the body as map[string]any (NOT a typed struct with
// `,omitempty` — that would drop the explicit nulls the budget / rate-limit
// clearing contract needs, spec §6.7 line 1194 + Feature 01 §2.1). 5 keys
// always-emit (team_alias + max_budget + budget_duration + rpm_limit +
// tpm_limit), 2 *_type keys conditional-add.
// - Step 7b: Resolve spec.accessGroups → ids (unresolved → park
// AccessGroupNotFound + requeue) and merge closedTeamGrant: the team is
// always closed; only attached groups open it.
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
// - Step 11: Update status (LastRendered.Hash / TeamID /
// At + Ready=Synced) on success.
//
// Anti-patterns avoided (Phase 5 PATTERNS.md):
// - NO RequeueAfter anywhere (REL-02 — Team is event-driven only).
// - NO Owns(.) — Team has no child resources.
// - NO finalizer logic.
// - NO synthetic LiteLLMTeam/default enqueue.
// - NO comparison against LiteLLM response (Phase 3 D-01 — operator-side
// hash only; the mock's POST /team/update returns `{}`, which is fine).
// - NO typed body construction (spec §6.7 line 1194 — body MUST be
// map[string]any to preserve JSON null for absent budget keys).
type TeamReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMTeam object (deletion
	// ack-missing). Non-nil in production; tests pass
	// mgr.GetEventRecorderFor("team-controller").
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger

	// BootEvents (FIX2.txt H-2) is optionally wired by main.go to the
	// BootSweeper's per-kind channel. When non-nil, SetupWithManager
	// adds a source.Channel watch so a sweep-time enqueue fires a
	// reconcile. Nil-safe: nil channel = no boot sweep wiring.
	BootEvents <-chan event.GenericEvent

	// ConnectionRebuilt — see GuardRailReconciler.ConnectionRebuilt
	// (issue #44 cache-population race close). nil-safe.
	ConnectionRebuilt <-chan event.GenericEvent

	// Churn tracks per-CR recreate frequency to trip the RecreateThrottled
	// breaker on a created-but-not-listed team (finding #9). SetupWithManager
	// wires newChurnGuard(); tests may leave it nil (no-throttle path).
	Churn *churnGuard
	// RecreateLimit is the per-CR recreates-per-minute ceiling. <= 0 →
	// DefaultRecreateLimitPerMin.
	RecreateLimit int

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
	if !snap.Usable() {
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

	// ─── Step 7: Build body as map[string]any ─────────────────────────────
	//
	// map[string]any (NOT a typed struct with omitempty) preserves JSON null on
	// the wire for absent budget / rate-limit keys (spec §6.7 line 1194;
	// Feature 01 §2.1).
	body := make(map[string]any, 10)
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
	// ─── Step 7b: closed baseline + attached access groups ────────────────
	//
	// Applied BEFORE the Step 8 hash so the resolved ids participate in drift
	// detection. A group appearing/disappearing changes the hash → one update.
	var groupIDs []string
	if len(team.Spec.AccessGroups) > 0 {
		groups, gerr := snap.Client.ListAccessGroups(ctx)
		if gerr != nil && !errors.Is(gerr, litellm.ErrNotFound) {
			return r.classifyMutationError(ctx, &team, logger, gerr, "GET /v1/access_group")
		}
		// ErrNotFound → zero groups registered → every name missing below.
		nameToID := make(map[string]string, len(groups))
		for _, g := range groups {
			nameToID[g.AccessGroupName] = g.AccessGroupID
		}
		var missing []string
		groupIDs, missing = resolveNames(team.Spec.AccessGroups, nameToID)
		sort.Strings(groupIDs) // order-independent hash, like renderAccessGroup
		if len(missing) > 0 {
			msg := fmt.Sprintf("spec.accessGroups not yet registered in LiteLLM: %s", strings.Join(missing, ", "))
			if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonAccessGroupNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonAccessGroupNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
			// Ordering dependency with LiteLLMAccessGroup CRs — requeue.
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
		}
	}
	for k, v := range closedTeamGrant(groupIDs) {
		body[k] = v
	}

	// ─── Step 8: Compute currentRenderedHash (Phase 3 D-01) ───────────────
	canonicalBytes, err := canonicalJSON(body)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("team controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 8b: Existence probe (vanish-detection, v0.4.5) ──────────────
	// Mirror model_controller.go Step 7b / mcpserver_controller.go Step 7b.
	// On safety-relist re-entry, verify the LiteLLM team_alias still
	// resolves to a row. On not-found → clear TeamID so Step 10 CREATE
	// path fires. On id-drift → clear TeamID so Step 10 UPDATE arm
	// re-targets the current row. Without this probe the operator only
	// detects spec-side drift and silently misses out-of-band /team/delete.
	//
	// Skipped on first reconcile (lastRendered.hash empty) — bootstrap
	// CREATE runs via TeamID empty already.
	if team.Status.LastRendered.TeamID != "" && team.Status.LastRendered.Hash != "" {
		clear, probeErr := probeVanishedResourceID(ctx,
			team.Status.LastRendered.TeamID,
			func(c context.Context) (string, error) {
				entries, err := snap.Client.ListTeamsByAlias(c, team.Name)
				if err != nil {
					return "", err
				}
				// Multiple entries can share an alias (AC-DC4 smallest-team_id
				// duplicate rule). Treat "still present" as "any entry under
				// this alias matches our lastID" — drift to a different ID
				// under the same alias means an admin recreated the team and
				// we should re-target by clearing.
				for _, e := range entries {
					if e.TeamID == team.Status.LastRendered.TeamID {
						return e.TeamID, nil
					}
				}
				return "", nil
			},
			r.Cache.InvalidateOn401, logger, "team")
		if probeErr != nil {
			return ctrl.Result{}, probeErr
		}
		if clear {
			team.Status.LastRendered.TeamID = ""
		}
	}

	// ─── Step 9: Hash-equal steady state ──────────────────────────────────
	if team.Status.LastRendered.Hash == currentRenderedHash &&
		team.Status.LastRendered.TeamID != "" &&
		team.Status.ObservedGeneration == team.Generation {
		// Stale-status heal — see model_controller.go Step 8 for the
		// connection-flap rationale. Same shape.
		if ready := apimeta.FindStatusCondition(team.Status.Conditions, conditionTypeReady); ready == nil ||
			ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
			if err := r.writeStatus(ctx, &team, metav1.ConditionTrue, reasonSynced, "team registered"); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
		}
		metrics.CRStatusAgeTracker.RecordSuccess(teamKind, team.Name)
		// Reached steady state — drop any recreate-churn history so a team
		// that recovered is not throttled by stale counts.
		r.Churn.Forget(req.NamespacedName)
		// No RequeueAfter: the per-kind SafetyRelistRunnable owns the periodic
		// vanish-probe tick (cmd/main.go). A requeue here would only fire on
		// the paths that happen to carry one — the Runnable fires regardless
		// of which branch the reconcile took, which is the point of a safety
		// net (issue #102).
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
			// FIX2.txt H-2: periodic requeue on deterministic 4xx.
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
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
		// CREATE arm — pin team_id to the CR name so new teams get a
		// human-readable, collision-free ID. Existing teams reach the
		// UPDATE arm via ListTeamsByAlias (alias == team.Name) and keep
		// their original server-assigned UUID — this change is CREATE-only.
		// LiteLLM accepts a caller-supplied team_id on POST /team/new
		// (verified in prod: team "platform" has team_id "team-platform").
		// Single-namespace assumption: two LiteLLMTeam CRs sharing a
		// metadata.name across namespaces would collide on team_id —
		// out of scope (see CLAUDE.md / docs).
		body["team_id"] = team.Name
		// FIX4.txt H-1 inventory case (b): /team/new has no native
		// audit field at the top level. Empirically (e2e AC-T4 against
		// LiteLLM 1.83.10), injecting `metadata.created_by/updated_by`
		// breaks the bootstrap path. Skip Team stamping until LiteLLM
		// ships a native audit column for /team/* — symmetry-only goal
		// is not worth a CREATE regression.
		// Recreate circuit breaker (finding #9): a recreate (not first
		// reconcile) means the vanish probe cleared a populated TeamID. If
		// this repeats faster than RecreateLimit/min the entry is
		// created-but-not-listed; park the CR instead of storming LiteLLM.
		if !firstReconcile {
			limit := r.RecreateLimit
			if limit <= 0 {
				limit = DefaultRecreateLimitPerMin
			}
			if n := r.Churn.Count(req.NamespacedName); n >= limit {
				msg := fmt.Sprintf("recreate throttled: %d recreates within %s (limit %d); "+
					"LiteLLM accepts POST /team/new but the entry never appears on the existence probe "+
					"(created-but-not-listed); parked to avoid a reconcile storm. Retrying after %s.",
					n, churnWindow, limit, recreateThrottleBackoff)
				if werr := r.writeStatus(ctx, &team, metav1.ConditionFalse, reasonRecreateThrottled, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonRecreateThrottled)
				}
				metrics.ReconcileTotal.WithLabelValues(teamKind, "success").Inc()
				return ctrl.Result{RequeueAfter: recreateThrottleBackoff}, nil
			}
		}
		if _, cerr := snap.Client.CreateTeamRaw(ctx, body); cerr != nil {
			return r.classifyMutationError(ctx, &team, logger, cerr, "POST /team/new")
		}
		// Pin the persisted teamID to the name we supplied, not the create
		// response. LiteLLM echoes the caller-supplied team_id, but pinning
		// from team.Name keeps status.lastRendered.teamID correct even if a
		// future LiteLLM response omits or rewrites the field — and it must
		// match what the post-restart alias lookup will re-adopt.
		newTeamID = team.Name
		// Two-gate first-reconcile suppression:
		// alitellm_operator_drift_corrected_total{action=create_missing} only increments
		// when !firstReconcile AND ObservedGeneration > 0 (the latter
		// condition is structurally redundant under the former, but is
		// retained verbatim from the Phase 5 pattern as defense in
		// depth against future Status-shape changes).
		if !firstReconcile && team.Status.ObservedGeneration > 0 {
			metrics.DriftCorrectedTotal.WithLabelValues("team", "create_missing").Inc()
			// Record this recreate so the breaker trips on a storm.
			r.Churn.Record(req.NamespacedName)
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
		// FIX4.txt H-1 inventory case (b): skip Team stamping (see
		// CREATE branch above for rationale).
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
		Hash:   currentRenderedHash,
		TeamID: newTeamID,
		At:     &now,
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

	// No RequeueAfter: the per-kind SafetyRelistRunnable owns the periodic
	// vanish-probe tick (cmd/main.go). A requeue here would only fire on
	// the paths that happen to carry one — the Runnable fires regardless
	// of which branch the reconcile took, which is the point of a safety
	// net (issue #102).
	return ctrl.Result{}, nil
}

// implicitDefaultBody is the rendered body of the implicit Team/default (no
// CR): no budget / rate limits, closed, no access groups. Linking it to an
// access group is the user's job — declare LiteLLMTeam/default. Shared by the
// synthetic reconcile and the Team/default deletion fallback so their hashes
// stay aligned.
func implicitDefaultBody() map[string]any {
	body := map[string]any{
		"team_alias":      teamAliasDefault,
		"max_budget":      nil,
		"budget_duration": nil,
		"rpm_limit":       nil, // Feature 01 §2.1 — always-emit nil on clear
		"tpm_limit":       nil,
		// rpm_limit_type / tpm_limit_type INTENTIONALLY ABSENT — conditional-add
		// per Feature 01 §2.1: omitted (not nil) when *_limit is nil.
	}
	for k, v := range closedTeamGrant(nil) {
		body[k] = v
	}
	return body
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
// - alitellm_operator_drift_corrected_total{action=create_missing} is NOT incremented on
// the very FIRST synthetic reconcile (the implicit default is
// bootstrapping, not correcting drift — same spirit as OWN-04
// first-reconcile suppression). Subsequent CREATE arms (after an
// out-of-band delete in LiteLLM) DO increment the counter.
// - alitellm_operator_drift_corrected_total{action=update_drifted} increments only if the
// UPDATE arm fires AND the rendered hash differs from the cached
// implicitDefaultHash (i.e. someone mutated the team out-of-band).
func (r *TeamReconciler) reconcileImplicitDefault(ctx context.Context, logger logr.Logger) (ctrl.Result, error) {
	// ─── Connection-gate ──────────────────────────────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
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

	// ─── Build implicit body ──────────────────────────────────────────────
	body := implicitDefaultBody()

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
	// suppress alitellm_operator_drift_corrected_total{action=create_missing} on the
	// initial bootstrap (mirrors the per-CR OWN-04 first-reconcile spirit).
	firstBootstrap := cachedHash == "" && cachedTeamID == ""

	var newTeamID string
	if len(entries) == 0 {
		// CREATE arm — FIX4.txt H-1 case (b): no Team stamping (see
		// reconcile() CREATE branch for rationale).
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
		updateBody := implicitDefaultBody()
		updateBody["team_id"] = existing.TeamID
		if _, uerr := snap.Client.UpdateTeamRaw(ctx, updateBody); uerr != nil {
			return r.classifyMutationError(ctx, nil, logger, uerr, "POST /team/update (implicit default)")
		}
		newTeamID = existing.TeamID
		// alitellm_operator_drift_corrected_total{action=update_drifted} fires only when
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

	// Issue #23: resolve effective deletion policy once. Used by the
	// non-default branch's onAckMissing closure below. The AC-T4
	// default-team branch intentionally bypasses this gate because its
	// contract is "re-apply implicit empty body" (not delete) — the
	// finalizer removal there preserves the LiteLLM-side team aliased
	// `default`, so blocking it on ack-missing would deadlock cluster
	// bootstrap.
	policy := deletionpolicy.Resolve(team, team.Spec.DeletionPolicy)
	// onAckMissing returns nil on the Orphan branch (caller falls through
	// to RemoveFinalizer) and a non-nil error on the Delete branch (caller
	// returns the error for controller-runtime backoff).
	onAckMissing := newAckMissingFn(r.Recorder, team, teamKind, team.Namespace, team.Name, policy)

	if team.Name == teamAliasDefault {
		// AC-T4 PROTECTED PATH — re-apply the implicit empty body.
		snap := r.Cache.Snapshot()
		if !snap.Usable() {
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
			body := implicitDefaultBody()
			body["team_id"] = resolvedTeamID
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
				canonicalBytes, cerr := canonicalJSON(implicitDefaultBody())
				// M-B3: skip seeding the cache on a marshal error rather than
				// caching a hash over empty bytes (which would cause false drift
				// + an extra UPDATE next reconcile). The static nil/string body
				// cannot fail to marshal today, so there is no red test — this
				// is defense-in-depth against a future mutation of the body map.
				if cerr != nil {
					logger.Error(cerr, "Team/default: canonicalJSON failed; skipping implicit-default hash seed")
				} else {
					sum := sha256.Sum256(canonicalBytes)
					r.implicitDefaultMu.Lock()
					r.implicitDefaultHash = fmt.Sprintf("%x", sum)
					r.implicitDefaultTeamID = resolvedTeamID
					r.implicitDefaultMu.Unlock()
				}
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
		// - 200/2xx → success; increment alitellm_operator_drift_corrected_total{
		// team,delete_vanished} and proceed to
		// RemoveFinalizer.
		// - 404 on POST → success per spec §7.5 line 1332 ("a 404 on a
		// delete is treated as success: the LiteLLM
		// resource is considered cleaned up"). Same
		// alitellm_operator_drift_corrected_total increment + finalizer
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
		if !snap.Usable() {
			// Issue #23: gate on resolved policy.
			if err := onAckMissing("LiteLLM unavailable"); err != nil {
				return ctrl.Result{}, err
			}
			// Orphan branch — fall through to RemoveFinalizer below.
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
						logger.Info("delete-resolve: 401 fast-path; cache invalidated",
							"team", team.Name, "path", auth401.Path)
						// Issue #23: gate on resolved policy.
						if err := onAckMissing("401 on ListTeamsByAlias"); err != nil {
							return ctrl.Result{}, err
						}
						// Orphan branch — fall through to RemoveFinalizer.
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
						// FIX2.txt H-2: periodic requeue so an upstream fix
						// unblocks finalizer drain without external poke.
						return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
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
						return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
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
						logger.Info("delete: 401 fast-path; cache invalidated",
							"team", team.Name, "teamID", teamID, "path", auth401.Path)
						// Issue #23: gate on resolved policy.
						if err := onAckMissing("401 on DeleteTeam"); err != nil {
							return ctrl.Result{}, err
						}
						// Orphan branch — fall through to RemoveFinalizer.
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
						return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
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
	// OBS-03: drop the alitellm_operator_cr_status_age_seconds label before the CR is gone (T-07-01-01).
	metrics.CRStatusAgeTracker.Forget(teamKind, team.Name)
	// Issue #23: idempotent Forget — clears DeletionBlocked gauge
	// whenever the finalizer actually leaves.
	metrics.DeletionBlocked.Forget(teamKind, team.Namespace, team.Name)
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

// is4xxNon401Status returns true if err is a *litellm.RejectedError whose
// HTTP status equals `wantStatus` (a 4xx code other than 401 — caller's
// responsibility to ensure that constraint). Uses the typed
// RejectedError.Status via rejectedStatus (errors.As-based), so it survives
// error wrapping.
//
// Used by the non-default Team deletion path to distinguish:
// - 404 on POST /team/delete → spec §7.5 line 1332 success (drift counter
// fires, finalizer removed).
// - other 4xx (e.g. 400, 403, 422) → LiteLLMRejected, finalizer NOT removed.
func is4xxNon401Status(err error, wantStatus int) bool {
	return rejectedStatus(err) == wantStatus
}

// isTransientLiteLLMError returns true if err looks like a 5xx / network
// transient that should bubble up for controller-runtime exponential
// backoff. 4xx (non-401) is treated as terminal so the finalizer-remove
// path can proceed without leaving the CR stuck in Terminating forever.
func isTransientLiteLLMError(err error) bool {
	// 4xx (incl. 404) → terminal (deterministic); nil → not transient.
	// Anything else (5xx / network / context, and Auth401Error which the
	// caller fast-paths before reaching here) → transient.
	return err != nil && !is4xxStatus(err)
}

// classifyMutationError handles §7.7 error classification for LiteLLM
// mutation calls (CreateTeamRaw / UpdateTeamRaw / ListTeamsByAlias):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return
// (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff.
func (r *TeamReconciler) classifyMutationError(ctx context.Context, team *litellmv1alpha1.LiteLLMTeam, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	// team may be nil when called from reconcileImplicitDefault — the synthetic
	// LiteLLMTeam/default reconcile has no K8s CR to write status onto, so the
	// writeStatus closure no-ops on nil (preserving the legacy `if team != nil`
	// guard). All other side effects (cache-invalidate, logging, metrics,
	// requeue) stay unconditional, matching the prior inline behavior.
	snap := r.Cache.Snapshot()
	return classifyMutationError(ctx, logger, err, opDesc, teamKind,
		func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
			if team == nil {
				return nil
			}
			return r.writeStatus(c, team, s, reason, msg)
		},
		r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
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
	// Uses Update-on-fresh (not Patch + MergeFrom) via the shared
	// writeStatusWithRetry core. Callers mutate team.Status.LastRendered
	// (TeamID) in-memory BEFORE this call; capturing it as desiredLR and
	// re-applying onto the freshly-Get'd object preserves it (a MergeFrom orig
	// would omit LastRendered → Hash=="" → duplicate re-POST /team/new, the
	// regression seen in TestTeamHubSeam_AC_DC1 and the AC_T3/T6 suite).
	// Standardized onto RetryOnConflict (previously a bare Status().Update):
	// a 409 is now resolved by re-Get + re-apply instead of leaking to
	// controller-runtime.
	cond := buildReadyCondition(team.Generation, status, reason, message)
	desiredLR := team.Status.LastRendered
	desiredObs := team.Generation

	var fresh litellmv1alpha1.LiteLLMTeam
	err := writeStatusWithRetry(ctx, r.Client, team, &fresh, func(f *litellmv1alpha1.LiteLLMTeam) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = desiredObs
		// A vanish-probe clear (Step 7b/8b) lives in memory only — never let it
		// blank an ID already persisted. Any error path between the probe and
		// the create would otherwise write the cleared ID through, stranding the
		// CR with no way back to the UPDATE arm (#131).
		desiredLR.TeamID = keepPersistedID(desiredLR.TeamID, f.Status.LastRendered.TeamID)
		f.Status.LastRendered = desiredLR
	})
	if err == nil {
		team.Status = fresh.Status
		team.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(teamKind, team.Namespace, reason)
	return err
}

// SetupWithManager registers the TeamReconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMTeam{}) — primary watch.
// - WatchesRawSource(source.TypedFunc) — optional synthetic
// LiteLLMTeam/default request channel. The TeamDefaultRunnable enqueues
// reconcile.Request{NamespacedName:{Namespace, "default"}} onto this
// channel from a wait-for-Ready + 30-min ticker loop (spec §7.4 line
// 1313). The optional-variadic shape mirrors ModelReconciler.
//
// Named("team") — controller registry name.
// No Owns(.) — Team has no child resources.
func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager, requeueCh ...chan reconcile.Request) error {
	if r.Churn == nil {
		r.Churn = newChurnGuard()
	}
	if r.RecreateLimit <= 0 {
		r.RecreateLimit = DefaultRecreateLimitPerMin
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMTeam{}, builder.WithPredicates()).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToTeams),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("team")

	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}

	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToTeams); src != nil {
		b = b.WatchesRawSource(src)
	}

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

// ListTeamRequests lists every LiteLLMTeam in namespace and returns their
// reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests — see
// ListModelRequests for the shared contract.
func ListTeamRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMTeamList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return reqs, nil
}
