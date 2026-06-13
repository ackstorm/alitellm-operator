// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/identity"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// a2aAgentFinalizer is the finalizer name managed by the LiteLLMA2AAgent reconciler
// per spec §7.5. Issuing DELETE /v1/agents/<agent_id> against LiteLLM
// removes the A2A agent entry from LiteLLM before the CR is fully removed
// from etcd.
const a2aAgentFinalizer = "a2aagents.litellm.ackstorm.ai/finalizer"

// a2aAgentKind is the metric label for LiteLLMA2AAgent CRs.
const a2aAgentKind = "LiteLLMA2AAgent"

// a2aAgentSafetyRelistInterval bounds how often the A2AAgent controller
// re-runs the Step 7b vanish-probe (LIST + name filter to detect
// out-of-band /v1/agents/<id> drift). Returned as RequeueAfter on every
// successful reconcile. v0.4.5: introduced together with vanish
// detection to recover from external LiteLLM resets / admin deletes
// without operator intervention. 5min matches the
// MCPServer / Model / Team cadence.
// a2aAgentSafetyRelistInterval is package-level so cmd/main.go can override
// via SetSafetyRelistIntervals (env-driven, Helm-exposed). Default 10m
// (v0.4.7: matches MCPServer/Model/Team cadence).
var a2aAgentSafetyRelistInterval = 10 * time.Minute

// A2AAgentSecretRefIndexField is the field indexer path registered in
// cmd/main.go for reverse-mapping Secret names back to A2AAgents that
// reference them (Phase 3 D-06 pattern carry-forward for SEC-09 rotation
// propagation across BOTH spec.params and spec.agentCard bags — the same
// SecretSubstitution entry covers both).
const A2AAgentSecretRefIndexField = ".spec.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexA2AAgentSecretRefs is the field indexer function for
// A2AAgentSecretRefIndexField. Mirrors IndexMCPServerSecretRefs verbatim,
// specialized for the LiteLLMA2AAgent type.
func IndexA2AAgentSecretRefs(o client.Object) []string {
	a2a, ok := o.(*litellmv1alpha1.LiteLLMA2AAgent)
	if !ok {
		return nil
	}
	return secretRefNames(a2a.Spec.Secrets)
}

// Events RBAC marker inheritance (Phase 5 Task 0 audit, recorded
// in 05-01-SUMMARY.md "Task 0 Events RBAC Audit Outcome"): the package-wide
// `+kubebuilder:rbac:groups="",resources=events,verbs=create;patch` marker
// lives on internal/controller/mcpserver_controller.go. kubebuilder marker
// scope is per-package — A2AAgent reconciler INHERITS the events grant and
// MUST NOT duplicate it (duplication is a no-op but obscures the single
// source of truth).

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellma2aagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellma2aagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellma2aagents/finalizers,verbs=update

// A2AAgentReconciler reconciles LiteLLMA2AAgent CRs against LiteLLM per spec §6.6 +
// §7.3 and Phase 5 CONTEXT.md D-01.D-10.
//
// State machine (per-reconcile) — mirrors MCPServerReconciler shape with
// A2A-specific divergences for two-pass substitution (D-04) and the
// four-collision ProjectionOverride Event taxonomy (D-05):
//
// - Step 1: Fetch the CR (NotFound → return nil).
// - Step 2a: DeletionTimestamp set → issue DELETE /v1/agents/<id>
// (with name-resolve fallback via ListAgents + in-memory filter
// when AgentID is empty) → RemoveFinalizer → Update.
// - Step 2b: Finalizer absent → AddFinalizer → Update → return.
// - Step 3: Connection-gating per Phase 3 D-08: !snap.Ready → writeStatus
// (LiteLLMUnavailable, echo-reason) → return nil.
// - Step 3.5: SEC-03 uniqueness of spec.secrets[].as values (runtime check
// mirroring Phase 3 LiteLLMModel / Phase 5).
// - Step 4: Resolve spec.secrets[] → secretMap (SHARED across both
// substitution passes per D-04 — a Secret referenced in BOTH
// spec.params and spec.agentCard is fetched from the Kubernetes API
// exactly once per reconcile).
// - Step 5: Decode spec.params + spec.agentCard into separate maps.
// - Step 5.5: Two-pass substitution per Phase 5 D-04:
// (a) Substitute(paramsMap, secretMap) — first pass.
// (b) Substitute(agentCardMap, secretMap) — second pass, shared map.
// missing = union(missingParams, missingAgentCard); referenced =
// union(refsParams, refsAgentCard).
// - Step 6: SEC-07 UnusedSecretRef detection → Event per declared `as`
// unreferenced in the UNION of both passes.
// - Step 7: Four-collision ProjectionOverride Event emission per Phase 5
// D-05 (each fires at most once per reconcile pass):
// (a) `agent_name` — spec.params.agent_name override.
// (b) `agent_card_params` — spec.params.agent_card_params override.
// (c) `agent_card_params.url` — spec.agentCard.url override.
// (d) `model_info` — spec.params.model_info reserved-key warning.
// Then apply structural overlays into the merged body.
// - Step 8: Compute currentRenderedHash (SHA-256 of canonical JSON of
// mergedBody).
// - Step 9: Hash-equal steady-state short-circuit (no mutation).
// - Step 10: Branch CREATE (status.lastRendered.AgentID == "") vs UPDATE.
// UPDATE arm is the simple PUT /v1/agents/<id> — A2A's PUT is
// empirically wholesale-replace per Phase 1 Probe 7 ✓ (verified on
// 1.82.6; not impacted by Prisma defect that gated MCP). NO
// delete-and-recreate path is committed.
// - Step 11: Classify mutation errors per §7.7 — 401 → InvalidateOn401 +
// nil return (anti-storm REL-06); 4xx → LiteLLMRejected + nil return;
// 5xx/network → return err for backoff.
// - Step 12: Update status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided:
// - NO RequeueAfter anywhere (REL-02 — A2AAgent is event-driven only).
// - NO Owns(.) — A2AAgent does not own child resources.
// - NO delete-and-recreate path (Probe 7 ✓ — simple PUT is wholesale-replace).
// - NO comparison against LiteLLM response (Phase 3 D-01 — operator-side hash only).
type A2AAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMA2AAgent object —
	// Normal/UnusedSecretRef (SEC-07) + Warning/ProjectionOverride
	// (Phase 5 D-05; four call sites). Non-nil in production; tests pass
	// mgr.GetEventRecorderFor("a2aagent-controller").
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
	// ConnectionRebuilt — see GuardRailReconciler.ConnectionRebuilt
	// (issue #44 cache-population race close). nil-safe.
	ConnectionRebuilt <-chan event.GenericEvent
	// Churn tracks per-CR recreate frequency to trip the RecreateThrottled
	// breaker on a created-but-not-listed agent (finding #9). SetupWithManager
	// wires newChurnGuard(); tests may leave it nil (no-throttle path).
	Churn *churnGuard
	// RecreateLimit is the per-CR recreates-per-minute ceiling. <= 0 →
	// DefaultRecreateLimitPerMin.
	RecreateLimit int
}

// Reconcile implements the LiteLLMA2AAgent state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the §7.3 mapping.
func (r *A2AAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("a2aagent", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var a2a litellmv1alpha1.LiteLLMA2AAgent
	if err := r.Get(ctx, req.NamespacedName, &a2a); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !a2a.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&a2a, a2aAgentFinalizer) {
			// Issue #23: resolve effective deletion policy once.
			policy := deletionpolicy.Resolve(&a2a, a2a.Spec.DeletionPolicy)
			onAckMissing := newAckMissingFn(r.Recorder, &a2a, a2aAgentKind, a2a.Namespace, a2a.Name, policy)

			snap := r.Cache.Snapshot()
			if snap.Usable() {
				agentID := a2a.Status.LastRendered.AgentID
				if agentID == "" {
					// Phase 5 D-02 stale-status fallback: re-resolve by name
					// via ListAgents + in-memory filter on metadata.name.
					if resolved := r.resolveAgentIDByName(ctx, snap.Client, a2a.Name, logger); resolved != "" {
						agentID = resolved
					}
				}
				if agentID != "" {
					if err := snap.Client.DeleteAgent(ctx, agentID); err != nil {
						var auth401 *litellm.Auth401Error
						switch {
						case errors.As(err, &auth401):
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							if gerr := onAckMissing("401 on DeleteAgent"); gerr != nil {
								return ctrl.Result{}, gerr
							}
						case is4xxStatus(err):
							logger.Info("deletion: deterministic 4xx on DeleteAgent; ack-missing", "error", err.Error())
							if gerr := onAckMissing("4xx on DeleteAgent: " + err.Error()); gerr != nil {
								return ctrl.Result{}, gerr
							}
						default:
							// Transient error — return for backoff. Finalizer stays.
							return ctrl.Result{}, err
						}
					} else {
						metrics.DriftCorrectedTotal.WithLabelValues("a2a", "delete_vanished").Inc()
						metrics.DeletionBlocked.Forget(a2aAgentKind, a2a.Namespace, a2a.Name)
						logger.Info("finalizer removed; LiteLLM A2A agent deleted", "agentID", agentID)
					}
				} else {
					metrics.DeletionBlocked.Forget(a2aAgentKind, a2a.Namespace, a2a.Name)
					logger.Info("finalizer removed; LiteLLM entry already absent (no pinned ID, name-resolve returned empty)", "name", a2a.Name)
				}
			} else {
				// Issue #23: gate on resolved policy.
				if err := onAckMissing("LiteLLM unavailable"); err != nil {
					return ctrl.Result{}, err
				}
			}

			// OBS-03: drop the cr_status_age_seconds label before the CR is gone
			// so /metrics cardinality never grows monotonically (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(a2aAgentKind, a2a.Name)
			// Issue #23: idempotent Forget — clears DeletionBlocked gauge.
			metrics.DeletionBlocked.Forget(a2aAgentKind, a2a.Namespace, a2a.Name)
			controllerutil.RemoveFinalizer(&a2a, a2aAgentFinalizer)
			if err := r.Update(ctx, &a2a); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&a2a, a2aAgentFinalizer) {
		controllerutil.AddFinalizer(&a2a, a2aAgentFinalizer)
		if err := r.Update(ctx, &a2a); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connection-gating (Phase 3 D-08) ──────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 3.5: SEC-03 uniqueness of spec.secrets[].as values ──────────
	if msg := checkDuplicateSecretAs(a2a.Spec.Secrets, "SEC-03: must be unique within an A2AAgent"); msg != "" {
		if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
		}
		metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 4: Resolve Secrets referenced by spec.secrets[] ─────────────
	//
	// Phase 5 D-04: the resolved secretMap is SHARED across both substitution
	// passes (spec.params first, spec.agentCard second). A Secret referenced
	// from BOTH bags is fetched from the Kubernetes API exactly once per
	// reconcile (load-bearing optimization called out in CONTEXT.md D-04 and
	// asserted by TestA2AAgentReconciler_TwoPassSubstitution).
	secretMap, missMsg, err := resolveSecretMap(ctx, r.Client, a2a.Namespace, a2a.Spec.Secrets)
	if err != nil {
		return ctrl.Result{}, err
	}
	if missMsg != "" {
		if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, reasonSecretNotFound, missMsg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 5: Decode spec.params + spec.agentCard into separate maps ──
	paramsMap := make(map[string]any)
	if len(a2a.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(a2a.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}
	agentCardMap := make(map[string]any)
	if len(a2a.Spec.AgentCard.Raw) > 0 {
		if err := json.Unmarshal(a2a.Spec.AgentCard.Raw, &agentCardMap); err != nil {
			msg := "spec.agentCard: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}

	// ─── Step 5.5: Two-pass substitution (Phase 5 D-04) ───────────────────
	// First pass: spec.params with shared secretMap.
	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	// Second pass: spec.agentCard with the SAME secretMap (shared per D-04).
	referencedAgentCard, missingAgentCard, _ := substitution.Substitute(agentCardMap, secretMap)

	// Union of missing placeholders across both passes; first-error wins per
	// Phase 3 D-05 contract — surface the first missing name in the message.
	if len(missingParams) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as (in spec.params)", missingParams[0])
		if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}
	if len(missingAgentCard) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as (in spec.agentCard)", missingAgentCard[0])
		if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 6: SEC-07 UnusedSecretRef detection (union of both passes) ──
	referencedSet := make(map[string]struct{})
	for _, n := range referencedParams {
		referencedSet[n] = struct{}{}
	}
	for _, n := range referencedAgentCard {
		referencedSet[n] = struct{}{}
	}
	for _, entry := range a2a.Spec.Secrets {
		if _, ok := referencedSet[entry.As]; !ok {
			r.Recorder.Eventf(&a2a, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params or spec.agentCard",
				entry.As)
		}
	}

	// ─── Step 7: Four-collision ProjectionOverride Events (Phase 5 D-05) ──
	//
	// Each Event fires at most once per reconcile pass. The four call sites
	// MUST each have a DISTINCT message string so consumers can grep on the
	// collision key. Tests assert the message substring for each independently.
	//
	// (1) `agent_name` collision — user-set spec.params.agent_name is
	// overridden by metadata.name (operator structural overlay always wins).
	if _, hasUserAgentName := paramsMap["agent_name"]; hasUserAgentName {
		r.Recorder.Eventf(&a2a, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays metadata.name per spec §6.6)",
			"agent_name")
	}
	// (2) `agent_card_params` collision — user-set spec.params.agent_card_params
	// is overridden by spec.agentCard (operator structural overlay always wins).
	if _, hasUserAgentCardParams := paramsMap["agent_card_params"]; hasUserAgentCardParams {
		r.Recorder.Eventf(&a2a, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.agentCard per spec §6.6)",
			"agent_card_params")
	}
	// (3) `agent_card_params.url` collision — user-set spec.agentCard.url is
	// overridden by spec.endpoint (operator structural overlay always wins).
	if _, hasUserAgentCardURL := agentCardMap["url"]; hasUserAgentCardURL {
		r.Recorder.Eventf(&a2a, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q overridden by typed-field projection (operator overlays spec.endpoint per spec §6.6)",
			"agent_card_params.url")
	}
	// (4) `model_info` LiteLLM-reserved-key warning. The operator does NOT
	// overlay model_info — the user's value is preserved on its way to
	// LiteLLM; LiteLLM may itself reject the body. The Event surfaces the
	// reserved-key warning for user observability (per spec §6.6).
	if _, hasUserModelInfo := paramsMap["model_info"]; hasUserModelInfo {
		r.Recorder.Eventf(&a2a, corev1.EventTypeWarning, eventReasonProjectionOverride,
			"key %q is reserved by LiteLLM per spec §6.6 — operator forwards user value verbatim; LiteLLM behavior is undefined",
			"model_info")
	}

	// ─── Step 7.5: Build mergedBody with structural overlays ──────────────
	//
	// mergedBody starts as a copy of paramsRendered (spec.params after
	// substitution, AT TOP LEVEL — diverges from MCP). Then:
	// - agent_name ← metadata.name (overwrites any user value).
	// - agent_card_params ← agentCardRendered (overwrites any user value).
	// - agent_card_params.url ← spec.endpoint (overwrites any user value).
	// model_info is left as-is (LiteLLM reserved; no overlay).
	mergedBody := make(map[string]any, len(paramsMap)+3)
	for k, v := range paramsMap {
		mergedBody[k] = v
	}
	mergedBody["agent_name"] = a2a.Name
	// M-B10: snapshot the USER-declared agentCard keys BEFORE the structural
	// url overlay, so status.lastRendered.agentCardKeys reflects what the user
	// set — not the operator-injected "url" the user never declared.
	userAgentCardKeys := sortedKeys(agentCardMap)
	// Apply the agent_card_params.url overlay to the agentCardMap before
	// projecting it into mergedBody.
	agentCardMap["url"] = a2a.Spec.Endpoint
	mergedBody["agent_card_params"] = agentCardMap

	// ─── Step 8: Compute currentRenderedHash (Phase 3 D-01) ───────────────
	canonicalBytes, err := canonicalJSON(mergedBody)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("a2aagent controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 8b: Existence probe (vanish-detection, v0.4.5) ──────────────
	// Mirror model_controller.go Step 7b / mcpserver_controller.go Step 7b.
	// Verify AgentID still exists in LiteLLM. On not-found OR id-drift,
	// clear AgentID so Step 10 CREATE arm fires. Without this probe the
	// operator silently misses out-of-band /v1/agents/<id> deletes.
	//
	// Skipped on first reconcile (lastRendered.hash empty).
	if a2a.Status.LastRendered.AgentID != "" && a2a.Status.LastRendered.Hash != "" {
		clear, probeErr := probeVanishedResourceID(ctx,
			a2a.Status.LastRendered.AgentID,
			func(c context.Context) (string, error) {
				// v0.4.6: CachedListAgents dedupes concurrent
				// vanish-probes (currently 1 A2A CR, but scales as
				// fleet grows). Same TTL as MCP.
				entries, err := snap.Client.CachedListAgents(c)
				if err != nil {
					return "", err
				}
				for _, e := range entries {
					if e.AgentName == a2a.Name {
						return e.AgentID, nil
					}
				}
				return "", nil
			},
			r.Cache.InvalidateOn401, logger, "a2aagent")
		if probeErr != nil {
			return ctrl.Result{}, probeErr
		}
		if clear {
			a2a.Status.LastRendered.AgentID = ""
		}
	}

	// ─── Step 9: Hash-equal steady state ──────────────────────────────────
	if a2a.Status.LastRendered.Hash == currentRenderedHash &&
		a2a.Status.LastRendered.AgentID != "" &&
		a2a.Status.ObservedGeneration == a2a.Generation {
		// Stale-status heal — see model_controller.go Step 8 for the
		// connection-flap rationale. Same shape.
		if ready := apimeta.FindStatusCondition(a2a.Status.Conditions, conditionTypeReady); ready == nil ||
			ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
			if err := r.writeStatus(ctx, &a2a, metav1.ConditionTrue, reasonSynced, "a2a agent registered"); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
		}
		metrics.CRStatusAgeTracker.RecordSuccess(a2aAgentKind, a2a.Name)
		// Reached steady state — drop any recreate-churn history so an agent
		// that recovered is not throttled by stale counts.
		r.Churn.Forget(req.NamespacedName)
		// v0.4.5: periodic safety-relist requeue so Step 8b vanish probe
		// runs even when the CR spec is stable.
		return ctrl.Result{RequeueAfter: withJitter(a2aAgentSafetyRelistInterval)}, nil
	}

	// ─── Step 10: Branch CREATE vs UPDATE (simple PUT — Probe 7 ✓) ────────
	//
	// Phase 3 OWN-04 first-reconcile sentinel inherited: on observedGeneration
	// == 0 OR lastRendered.hash == "", drift counters are suppressed.
	firstReconcile := a2a.Status.ObservedGeneration == 0 || a2a.Status.LastRendered.Hash == ""

	// Construct the AgentConfig body. spec.params keys flow to top level
	// (NOT inside agent_card_params — diverges from MCP). Typed AgentConfig
	// fields are extracted from mergedBody; everything else stays in the
	// raw body (the litellm client serializes via the typed struct, so
	// extras outside the typed fields are dropped — that's the user's
	// contract per spec §6.6).
	agentConfig := buildAgentConfigFromMerged(mergedBody, a2a.Name, agentCardMap)

	var newAgentID string
	if a2a.Status.LastRendered.AgentID == "" {
		// CREATE path — first reconcile or stale status.
		// FIX4.txt H-1: stamp operator identity into agent_card_params.
		// LiteLLM 1.83.10 /v1/agents has no native audit field at the top
		// level, but agent_card_params is freeform and persisted verbatim.
		// CREATE stamps both created_by + updated_by.
		stampA2AIdentity(agentConfig, true)
		// Recreate circuit breaker (finding #9): a recreate (not first
		// reconcile) means the vanish probe cleared a populated AgentID. If
		// this repeats faster than RecreateLimit/min the entry is
		// created-but-not-listed; park the CR instead of storming LiteLLM.
		if !firstReconcile {
			limit := r.RecreateLimit
			if limit <= 0 {
				limit = DefaultRecreateLimitPerMin
			}
			if n := r.Churn.Count(req.NamespacedName); n >= limit {
				msg := fmt.Sprintf("recreate throttled: %d recreates within %s (limit %d); "+
					"LiteLLM accepts POST /v1/agents but the entry never appears on the existence probe "+
					"(created-but-not-listed); parked to avoid a reconcile storm. Retrying after %s.",
					n, churnWindow, limit, recreateThrottleBackoff)
				if werr := r.writeStatus(ctx, &a2a, metav1.ConditionFalse, reasonRecreateThrottled, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonRecreateThrottled)
				}
				metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
				return ctrl.Result{RequeueAfter: recreateThrottleBackoff}, nil
			}
		}
		result, err := snap.Client.CreateAgent(ctx, agentConfig)
		if err != nil {
			return r.classifyMutationError(ctx, &a2a, logger, err, "POST /v1/agents")
		}
		newAgentID = result.AgentID
		// Phase 3 OWN-04 + Phase 5 : suppress create_missing on the
		// very first reconcile (ObservedGeneration == 0) — the user's initial
		// POST is not a "drift correction". On subsequent re-creates (after a
		// delete-and-recreate cycle OR external-vanish recovery where
		// ObservedGeneration > 0 already), the counter DOES increment.
		if !firstReconcile && a2a.Status.ObservedGeneration > 0 {
			metrics.DriftCorrectedTotal.WithLabelValues("a2a", "create_missing").Inc()
			// Record this recreate so the breaker trips on a storm.
			r.Churn.Record(req.NamespacedName)
		}
		logger.V(1).Info("a2a agent created in LiteLLM", "agentID", newAgentID)
	} else {
		// UPDATE path — simple PUT /v1/agents/<id> (Phase 1 Probe 7 ✓;
		// PUT IS wholesale-replace on A2A).
		// FIX4.txt H-1: stamp updated_by only on UPDATE.
		stampA2AIdentity(agentConfig, false)
		if _, err := snap.Client.UpdateAgent(ctx, a2a.Status.LastRendered.AgentID, agentConfig); err != nil {
			return r.classifyMutationError(ctx, &a2a, logger, err, "PUT /v1/agents/<id>")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("a2a", "update_drifted").Inc()
		}
		// UPDATE keeps the same LiteLLM agent_id (Probe 7 ✓ — PUT preserves
		// the row's identity).
		newAgentID = a2a.Status.LastRendered.AgentID
		logger.V(1).Info("a2a agent updated in LiteLLM (simple PUT)", "agentID", newAgentID)
	}

	// ─── Step 12: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	a2a.Status.LastRendered = litellmv1alpha1.A2ALastRenderedStatus{
		Hash:          currentRenderedHash,
		ParamsKeys:    sortedKeys(paramsMap),
		AgentCardKeys: userAgentCardKeys, // M-B10: pre-url-overlay snapshot
		AgentID:       newAgentID,
		At:            &now,
	}
	if err := r.writeStatus(ctx, &a2a, metav1.ConditionTrue, reasonSynced, "a2a agent registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state; suppress controller-runtime's
			// ERROR "Reconciler error" log + backoff for this error class.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(a2aAgentKind, a2a.Name)
	metrics.ReconcileTotal.WithLabelValues(a2aAgentKind, "success").Inc()
	logger.V(1).Info("a2a agent reconciled", "agentID", newAgentID, "hash", currentRenderedHash)

	// v0.4.5: periodic safety-relist requeue — see Step 8b rationale.
	return ctrl.Result{RequeueAfter: withJitter(a2aAgentSafetyRelistInterval)}, nil
}

// buildAgentConfigFromMerged extracts the typed AgentConfig fields from the
// rendered mergedBody. agent_name and agent_card_params are operator-overlaid
// inputs; the rest of mergedBody is pulled from the AgentConfig modeled-field
// set (tpm_limit, rpm_limit, session_*_limit, static_headers, extra_headers,
// object_permission, litellm_params). Anything else in mergedBody is silently
// dropped by the AgentConfig JSON serialization — that's the user's contract
// per spec §6.6 (modeled-field projection).
func buildAgentConfigFromMerged(mergedBody map[string]any, agentName string, agentCardMap map[string]any) *litellm.AgentConfig {
	cfg := &litellm.AgentConfig{
		AgentName:       agentName,
		AgentCardParams: agentCardMap,
	}
	// Extract typed fields from mergedBody (paramsMap pass-through).
	if v, ok := mergedBody["tpm_limit"]; ok {
		if n, ok2 := toInt(v); ok2 {
			cfg.TPMLimit = &n
		}
	}
	if v, ok := mergedBody["rpm_limit"]; ok {
		if n, ok2 := toInt(v); ok2 {
			cfg.RPMLimit = &n
		}
	}
	if v, ok := mergedBody["session_tpm_limit"]; ok {
		if n, ok2 := toInt(v); ok2 {
			cfg.SessionTPMLimit = &n
		}
	}
	if v, ok := mergedBody["session_rpm_limit"]; ok {
		if n, ok2 := toInt(v); ok2 {
			cfg.SessionRPMLimit = &n
		}
	}
	if v, ok := mergedBody["static_headers"]; ok {
		if m, ok2 := v.(map[string]any); ok2 {
			cfg.StaticHeaders = m
		}
	}
	if v, ok := mergedBody["extra_headers"]; ok {
		if m, ok2 := v.(map[string]any); ok2 {
			cfg.ExtraHeaders = m
		}
	}
	if v, ok := mergedBody["object_permission"]; ok {
		if m, ok2 := v.(map[string]any); ok2 {
			cfg.ObjectPermission = m
		}
	}
	if v, ok := mergedBody["litellm_params"]; ok {
		if m, ok2 := v.(map[string]any); ok2 {
			cfg.LiteLLMParams = litellm.LiteLLMParams(m)
		}
	}
	return cfg
}

// toInt coerces JSON-decoded numeric values (which arrive as float64 from
// encoding/json) into int. Returns ok=false on non-numeric inputs.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// resolveAgentIDByName re-resolves a LiteLLMA2AAgent's LiteLLM agent_id from a
// metadata.name lookup via ListAgents + in-memory filter. Used by the
// finalizer path when status.lastRendered.AgentID is empty (Phase 5 D-02
// stale-status fallback). Returns "" if the entry is absent or the LIST
// call fails non-fatally.
func (r *A2AAgentReconciler) resolveAgentIDByName(ctx context.Context, llm *litellm.Client, name string, logger logr.Logger) string {
	entries, err := llm.ListAgents(ctx)
	if err != nil {
		if errors.Is(err, litellm.ErrNotFound) {
			return ""
		}
		var auth401 *litellm.Auth401Error
		if errors.As(err, &auth401) {
			r.Cache.InvalidateOn401()
			logger.Info("name-resolve: 401 fast-path", "path", auth401.Path)
			return ""
		}
		logger.V(1).Info("name-resolve: ListAgents failed; treating as absent", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.AgentName == name {
			return e.AgentID
		}
	}
	return ""
}

// classifyMutationError handles §7.7 error classification for LiteLLM
// mutation calls (CreateAgent / UpdateAgent / DeleteAgent):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return
// (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff.
func (r *A2AAgentReconciler) classifyMutationError(ctx context.Context, a2a *litellmv1alpha1.LiteLLMA2AAgent, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	snap := r.Cache.Snapshot()
	return classifyMutationError(ctx, logger, err, opDesc, a2aAgentKind,
		func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
			return r.writeStatus(c, a2a, s, reason, msg)
		},
		r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact. Callers MUST ensure no secret material reaches `message`.
func (r *A2AAgentReconciler) writeStatus(
	ctx context.Context,
	a2a *litellmv1alpha1.LiteLLMA2AAgent,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Uses Update-on-fresh (NOT Patch + MergeFrom) wrapped in
	// RetryOnConflict. Callers mutate a2a.Status.LastRendered (notably
	// AgentID) before this call; a MergeFrom orig captured here would
	// already carry the mutation, so the patch body would omit AgentID and
	// leave the server with an empty value — the next reconcile then sees
	// no AgentID and fires a duplicate POST /agent. Re-Getting `fresh` each
	// attempt and re-applying the captured intent prevents the conflict
	// from silently dropping the write (H3): a swallowed 409 at the call
	// site used to lose the AgentID assignment entirely.
	cond := buildReadyCondition(a2a.Generation, status, reason, message)
	desiredLastRendered := a2a.Status.LastRendered
	desiredObservedGen := a2a.Generation

	var fresh litellmv1alpha1.LiteLLMA2AAgent
	err := writeStatusWithRetry(ctx, r.Client, a2a, &fresh, func(f *litellmv1alpha1.LiteLLMA2AAgent) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = desiredObservedGen
		f.Status.LastRendered = desiredLastRendered
	})
	if err == nil {
		// Propagate persisted state back so callers (logger, metrics) see it.
		a2a.Status = fresh.Status
		a2a.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(a2aAgentKind, a2a.Namespace, reason)
	return err
}

// secretToA2AAgents maps a Secret update event to the set of LiteLLMA2AAgent
// CRs that reference it via spec.secrets[].secretRef.name (Phase 3 D-06
// rotation-propagation pattern). Uses the field indexer registered in
// cmd/main.go.
func (r *A2AAgentReconciler) secretToA2AAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	return secretToRequests(ctx, r.Client, r.Log, &litellmv1alpha1.LiteLLMA2AAgentList{}, obj.GetNamespace(), obj.GetName(), A2AAgentSecretRefIndexField, "secretToA2AAgents")
}

// SetupWithManager registers the A2AAgentReconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMA2AAgent{}) — primary watch.
// - Watches(&Secret{}, secretToA2AAgents) — SEC-09 rotation propagation
// for placeholders in EITHER spec.params or spec.agentCard.
//
// Named("a2aagent") — controller registry name.
// No Owns(.) and no safety re-list channel — Phase 5 may add
// these if cross-CR vanish detection requires them (Phase 7 dogfood gate).
func (r *A2AAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Churn == nil {
		r.Churn = newChurnGuard()
	}
	if r.RecreateLimit <= 0 {
		r.RecreateLimit = DefaultRecreateLimitPerMin
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMA2AAgent{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToA2AAgents),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToA2AAgents),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("a2aagent")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToA2AAgents); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}

// stampA2AIdentity injects identity.Operator() into the
// agent_card_params bag of an AgentConfig. LiteLLM 1.83.10 /v1/agents
// has no native audit field at the top level; agent_card_params is
// freeform and persisted verbatim. CREATE stamps both created_by +
// updated_by; UPDATE stamps only updated_by (LiteLLM-side audit
// semantics — original creator is immutable). FIX4.txt H-1.
func stampA2AIdentity(cfg *litellm.AgentConfig, includeCreatedBy bool) {
	if cfg.AgentCardParams == nil {
		cfg.AgentCardParams = map[string]any{}
	}
	if includeCreatedBy {
		cfg.AgentCardParams["created_by"] = identity.Operator()
	}
	cfg.AgentCardParams["updated_by"] = identity.Operator()
}
