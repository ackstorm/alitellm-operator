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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/conflict"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/identity"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// mcpServerFinalizer is the finalizer name managed by the LiteLLMMCPServer reconciler
// per spec §7.5. Issuing DELETE /v1/mcp/server/<server_id> against LiteLLM
// removes the MCP server entry from LiteLLM before the CR is fully removed
// from etcd.
const mcpServerFinalizer = "mcpservers.litellm.ackstorm.ai/finalizer"

// mcpSafetyRelistInterval bounds how often the MCPServer controller
// re-runs the Step 7 safety-relist (probe LiteLLM by name + clear
// stale ServerID on out-of-band deletion). Returned as RequeueAfter
// on every successful reconcile. Pre-v0.4.3 the Owns watch on
// Discovery children fired on every status / managedFields event
// (effectively continuous polling) so safety-relist swept ~5x/sec;
// the v0.4.3 predicate filter (generation-only) stopped that, so we
// re-introduce explicit periodic polling here at a sane cadence.
// 5min matches the dominant Discovery refresh-interval; bumps to
// LiteLLM API drift get corrected within ~5min worst-case.
// mcpSafetyRelistInterval is package-level so cmd/main.go can override
// it at startup via SetSafetyRelistIntervals (env-driven, Helm-exposed).
// Default 10m (raised from 5m in v0.4.7: production fleet sizes don't
// need sub-10m drift detection — every CR also fires immediately on
// spec edits + Connection-ready transitions + Secret rotations, so
// safety-relist is only the floor for purely external state divergence).
// NOT for runtime mutation — set once before reconcilers start.
var mcpSafetyRelistInterval = 10 * time.Minute

// mcpServerKind is the metric label for LiteLLMMCPServer CRs.
const mcpServerKind = "LiteLLMMCPServer"

// MCPServerSecretRefIndexField is the field indexer path registered in
// cmd/main.go for reverse-mapping Secret names back to MCPServers that
// reference them (Phase 3 D-06 pattern carry-forward for SEC-09 rotation
// propagation).
const MCPServerSecretRefIndexField = ".spec.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexMCPServerSecretRefs is the field indexer function for
// MCPServerSecretRefIndexField. Mirrors IndexModelSecretRefs verbatim,
// specialized for the LiteLLMMCPServer type.
func IndexMCPServerSecretRefs(o client.Object) []string {
	mcp, ok := o.(*litellmv1alpha1.LiteLLMMCPServer)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(mcp.Spec.Secrets))
	for _, s := range mcp.Spec.Secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// Task 0 audit (per checker WARNING #6 — read-only events RBAC inheritance
// check): zero existing kubebuilder:rbac:events markers in
// internal/controller/ AND zero events stanzas in config/rbac/role.yaml
// (verified 2026-05-16; audit captured in /tmp/05-01-task0-rbac.out).
// MCPServer reconciler ADDS the events marker below as the package-wide
// grant. Phase 5 plans 05-03 (A2A) and 05-04 (MSDisc) INHERIT this marker
// — they do NOT duplicate it (kubebuilder marker scope is per-package,
// not per-file). Documented in 05-01-SUMMARY.md.

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers/finalizers,verbs=update

// MCPServerReconciler reconciles LiteLLMMCPServer CRs against LiteLLM 1.83.10 per
// spec §6.4 + §7.3 and Phase 5 CONTEXT.md D-01.D-10.
//
// State machine (per-reconcile) — mirrors ModelReconciler shape:
//
// - Step 1: Fetch the CR (NotFound → return nil).
// - Step 2a: DeletionTimestamp set → issue DELETE /v1/mcp/server/<id>
// (with name-resolve fallback via ListMCPServers + in-memory filter
// when ServerID is empty) → RemoveFinalizer → Update.
// - Step 2b: Finalizer absent → AddFinalizer → Update → return.
// - Step 3: Connection-gating per Phase 3 D-08: !snap.Ready → writeStatus
// (LiteLLMUnavailable, echo-reason) → return nil.
// - Step 3.5: SEC-03 uniqueness of spec.secrets[].as values (runtime check
// mirroring Phase 3 LiteLLMModel).
// - Step 4: Resolve spec.secrets[] → secretMap.
// - Step 5: Decode spec.params into paramsMap; apply single-pass
// substitution (Phase 5 D-04 — MCP is single-pass; A2A is two-pass).
// - Step 6: SEC-07 UnusedSecretRef detection → Event per unused as.
// - Step 7: Compute currentRenderedHash (SHA-256 of canonical JSON body
// {server_name, url, transport, params}).
// - Step 8: Hash-equal steady-state short-circuit (no mutation).
// - Step 9: Branch CREATE (status.lastRendered.ServerID == "") vs UPDATE.
// UPDATE arm is the simple PUT /v1/mcp/server per Probe 10c verdict ✓
// on LiteLLM 1.83.10-stable (Phase 5 D-01 "If positive" branch — NO
// delete-and-recreate path is committed; the single-arm-enforcement
// grep gate documented in 05-01-PLAN.md acceptance criteria asserts
// the absence of removedKeys/setDiff/shrinkage signatures).
// - Step 10: Classify mutation errors per §7.7 — 401 → InvalidateOn401 +
// nil return (anti-storm REL-06); 4xx → LiteLLMRejected + nil return;
// 5xx/network → return err for backoff.
// - Step 11: Update status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided (Phase 5 PATTERNS.md L527+):
// - NO RequeueAfter anywhere (REL-02 — MCPServer is event-driven only).
// - NO Owns(.) — child resources are not owned by this controller;
// the MCPServerDiscovery reconciler is the parent for generated children
// (cascade-delete via blockOwnerDeletion=true, see).
// - NO ProjectionOverride Event emission — LiteLLMMCPServer has no structural
// overlay collisions (only A2A does, per Phase 5 D-05).
// - NO comparison against LiteLLM response (Phase 3 D-01 — operator-side
// hash only).
// - NO global LIST-and-prune (Phase 3 OWN-01 — one name per reconcile).
//
// Verdict ✓ per 05-00-SUMMARY.md — UPDATE arm uses simple PUT /v1/mcp/server.
type MCPServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMMCPServer object —
	// Normal/UnusedSecretRef (SEC-07). Non-nil in production; tests pass
	// mgr.GetEventRecorderFor("mcpserver-controller").
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
}

// Reconcile implements the LiteLLMMCPServer state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the §7.3 mapping.
func (r *MCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("mcpserver", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var mcp litellmv1alpha1.LiteLLMMCPServer
	if err := r.Get(ctx, req.NamespacedName, &mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !mcp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&mcp, mcpServerFinalizer) {
			// Issue #23: resolve effective deletion policy once.
			policy := deletionpolicy.Resolve(&mcp, mcp.Spec.DeletionPolicy)
			onAckMissing := func(reason string) error {
				if policy == deletionpolicy.Delete {
					metrics.DeletionBlocked.Record(mcpServerKind, mcp.Namespace, mcp.Name)
					r.Recorder.Eventf(&mcp, corev1.EventTypeWarning, "LiteLLMDeleteBlocked",
						"deletionPolicy=Delete and LiteLLM ack missing (%s); finalizer retained", reason)
					return fmt.Errorf("delete blocked: %s", reason)
				}
				metrics.DeletionOrphanedTotal.WithLabelValues(mcpServerKind).Inc()
				r.Recorder.Eventf(&mcp, corev1.EventTypeNormal, "LiteLLMDeleteOrphaned",
					"deletionPolicy=Orphan and LiteLLM ack missing (%s); finalizer removed; entry may persist", reason)
				return nil
			}

			snap := r.Cache.Snapshot()
			if snap.Ready {
				serverID := mcp.Status.LastRendered.ServerID
				if serverID == "" {
					// Phase 5 D-02 stale-status fallback: re-resolve by name
					// via ListMCPServers + in-memory filter on metadata.name.
					// FIX2.txt H-9: probe sanitized first, then original
					// metadata.name (orphan-adoption fallback).
					sanitized := litellm.SanitizeMCPServerName(mcp.Name, snap.MCPToolPrefixSeparator)
					if resolved := r.resolveServerIDByName(ctx, snap.Client, sanitized, mcp.Name, logger); resolved != "" {
						serverID = resolved
					}
				}
				if serverID != "" {
					if err := snap.Client.DeleteMCPServer(ctx, serverID); err != nil {
						var auth401 *litellm.Auth401Error
						if errors.As(err, &auth401) {
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							if gerr := onAckMissing("401 on DeleteMCPServer"); gerr != nil {
								return ctrl.Result{}, gerr
							}
						} else {
							// Transient error — return for backoff. Finalizer stays.
							return ctrl.Result{}, err
						}
					} else {
						metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished").Inc()
						metrics.DeletionBlocked.Forget(mcpServerKind, mcp.Namespace, mcp.Name)
						logger.Info("finalizer removed; LiteLLM MCP server deleted", "serverID", serverID)
					}
				} else {
					metrics.DeletionBlocked.Forget(mcpServerKind, mcp.Namespace, mcp.Name)
					logger.Info("finalizer removed; LiteLLM entry already absent (no pinned ID, name-resolve returned empty)", "name", mcp.Name)
				}
			} else {
				// Issue #23: gate on resolved policy.
				if err := onAckMissing("LiteLLM unavailable"); err != nil {
					return ctrl.Result{}, err
				}
			}

			// OBS-03: drop the cr_status_age_seconds label before the CR is gone
			// so /metrics cardinality never grows monotonically (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(mcpServerKind, mcp.Name)
			// Issue #23: idempotent Forget — clears DeletionBlocked gauge.
			metrics.DeletionBlocked.Forget(mcpServerKind, mcp.Namespace, mcp.Name)
			controllerutil.RemoveFinalizer(&mcp, mcpServerFinalizer)
			if err := r.Update(ctx, &mcp); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&mcp, mcpServerFinalizer) {
		controllerutil.AddFinalizer(&mcp, mcpServerFinalizer)
		if err := r.Update(ctx, &mcp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Snapshot is hoisted above the Connection gate so the conflict
	// resolver below can run even when LiteLLM is unreachable — a loser
	// must short-circuit unconditionally so it never issues an HTTP
	// mutation, regardless of snap.Ready.
	snap := r.Cache.Snapshot()

	// ─── Conflict resolution (alpha-last-wins, sanitization-aware) ───────
	// Two LiteLLMMCPServer CRs in the same namespace can have distinct
	// metadata.name values that sanitize to the same LiteLLM server_name
	// (e.g. foo.bar and foo-bar both sanitize to foo-bar when separator
	// is "."). The CR whose <namespace>/<name> sorts LAST wins; every
	// other candidate short-circuits with Ready=False/Reason=Conflict
	// BEFORE any LiteLLM HTTP mutation. See docs/concepts/conflict-resolution.md.
	//
	// Because the operator caches ONE LiteLLMConnection at a time, every
	// MCPServer in the namespace shares the same separator at this point
	// in time, so candidacy can be decided by recomputing the sanitized
	// name in-memory per CR — no field indexer is needed. The self-watch
	// in SetupWithManager fans Create/Delete events back to siblings so
	// the loser→winner promotion and winner→loser appearance fire
	// promptly.
	sep := snap.MCPToolPrefixSeparator
	if sep == "" {
		sep = litellm.MCPToolPrefixSeparatorDefault
	}
	sanitizedName := litellm.SanitizeMCPServerName(mcp.Name, sep)
	var siblings litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &siblings, client.InNamespace(mcp.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list mcpservers for conflict resolution: %w", err)
	}
	candidates := make([]client.Object, 0, len(siblings.Items))
	for i := range siblings.Items {
		s := &siblings.Items[i]
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		if litellm.SanitizeMCPServerName(s.Name, sep) == sanitizedName {
			candidates = append(candidates, s)
		}
	}
	priorReason := ""
	if c := apimeta.FindStatusCondition(mcp.Status.Conditions, "Ready"); c != nil {
		priorReason = c.Reason
	}
	winner := conflict.ResolveWinner(candidates)
	if conflict.IsLoser(&mcp, winner) {
		conflict.ApplyLoserCondition(&mcp.Status.Conditions, mcp.Generation, conflict.Key(winner))
		if r.Recorder != nil {
			r.Recorder.Eventf(&mcp, corev1.EventTypeNormal, "ConflictDetected",
				"superseded by %s for sanitized server_name %q", conflict.Key(winner), sanitizedName)
		}
		mcp.Status.ObservedGeneration = mcp.Generation
		if err := r.Status().Update(ctx, &mcp); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("set Conflict condition: %w", err)
		}
		return ctrl.Result{}, nil
	}
	conflict.ClearLoserCondition(&mcp.Status.Conditions)
	if priorReason == conflict.ConditionReasonConflict && r.Recorder != nil {
		r.Recorder.Eventf(&mcp, corev1.EventTypeNormal, "ConflictWon",
			"promoted to winner for sanitized server_name %q", sanitizedName)
	}

	// ─── Step 3: Connection-gating (Phase 3 D-08) ──────────────────────────
	if !snap.Ready {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		// Periodic safety relist on soft-fail path: connectionReadyTransition
		// re-enqueues on Connection recovery, but the safety-relist cadence
		// is the floor so a missed transition still recovers (review #1
		// "Issue 2" + review #2 §3).
		return ctrl.Result{RequeueAfter: withJitter(mcpSafetyRelistInterval)}, nil
	}

	// ─── Step 3.5: SEC-03 uniqueness of spec.secrets[].as values ──────────
	{
		seen := make(map[string]struct{}, len(mcp.Spec.Secrets))
		for _, entry := range mcp.Spec.Secrets {
			if _, exists := seen[entry.As]; exists {
				msg := fmt.Sprintf("spec.secrets[]: duplicate as value %q (SEC-03: must be unique within a LiteLLMMCPServer)", entry.As)
				if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
				}
				metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			seen[entry.As] = struct{}{}
		}
	}

	// ─── Step 4: Resolve Secrets referenced by spec.secrets[] ─────────────
	secretMap := make(map[string]string)
	for _, entry := range mcp.Spec.Secrets {
		var secret corev1.Secret
		secretKey := types.NamespacedName{
			Namespace: mcp.Namespace,
			Name:      entry.SecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				msg := mcp.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
				if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
				}
				metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
				return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
			}
			return ctrl.Result{}, err
		}
		val, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			msg := mcp.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
			if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
		}
		secretMap[entry.As] = string(val)
	}

	// ─── Step 5: Decode spec.params + single-pass substitution ────────────
	paramsMap := make(map[string]any)
	if len(mcp.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(mcp.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}
	// FIX5 H-1: drop reserved structural keys from paramsMap BEFORE
	// substitution / hash / extraction so downstream state (hash,
	// status.lastRendered.ParamsKeys, the extracted struct) is consistent.
	// Reserved keys (server_id, server_name, alias, url, transport,
	// spec_path) are stamped from the CR — a user-supplied value here
	// would be a hijack vector, so we ignore the bag entries silently.
	for k := range reservedMCPParamKeys {
		delete(paramsMap, k)
	}

	// Phase 5 D-04: MCP is single-pass on spec.params (vs A2A's two-pass
	// across spec.params + spec.agentCard).
	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	if len(missingParams) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as", missingParams[0])
		if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 6: SEC-07 UnusedSecretRef detection ──────────────────────────
	referencedSet := make(map[string]struct{})
	for _, n := range referencedParams {
		referencedSet[n] = struct{}{}
	}
	for _, entry := range mcp.Spec.Secrets {
		if _, ok := referencedSet[entry.As]; !ok {
			r.Recorder.Eventf(&mcp, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params",
				entry.As)
		}
	}

	// ─── Step 7: Compute currentRenderedHash (Phase 3 D-01) ───────────────
	// Build the canonical body: structural overlays {server_name, url,
	// transport} + the pass-through params bag. The structural overlays
	// participate in the hash so endpoint/transport edits drive UPDATE.
	//
	// FIX.txt H-1 (2026-05-22): LiteLLM rejects its MCP_TOOL_PREFIX_SEPARATOR
	// inside server_name. The reconciler sanitizes the K8s metadata.name
	// per LiteLLMConnection.spec.mcpToolPrefixSeparator (the snapshot
	// carries the value). The hash is computed on the SANITIZED form so
	// drift detection matches what LiteLLM actually stores; K8s
	// metadata.name remains untouched.
	//
	// FIX9 H-1 (2026-05-23): the hash includes a render-version tag so
	// that a bump in extractMCPParams() (i.e. the operator now extracts a
	// field it previously dropped) invalidates every persisted hash. The
	// previously-saved status.lastRendered.hash will no longer match the
	// recomputed hash, the steady-state shortcut in Step 8 falls through,
	// and Step 9's UPDATE path pushes the freshly-extracted body to
	// LiteLLM. Without this tag, post-upgrade drift between what the
	// operator USED to render and what it RENDERS NOW stays masked — the
	// only recovery is a manual `kubectl patch --subresource=status` to
	// clear lastRendered.hash (observed in prod on the v0.3.0 → v0.4.0
	// FIX5 H-1 propagation gap; 12/26 children carried stale empty fields
	// until hash-cleared).
	//
	// BUMP mcpRenderVersion when extractMCPParams adds/removes/changes a
	// field, or when any other step between paramsMap and the LiteLLM
	// request struct changes shape. Cost: every existing CR's next
	// reconcile re-renders + UPDATEs in LiteLLM (one PUT each). Cheap.
	// sanitizedName is hoisted from the conflict-resolution block above
	// — both blocks must agree on the wire-side name. When snap.Ready
	// is true (the path that reaches here), snap.MCPToolPrefixSeparator
	// equals the resolver's `sep` and so sanitizedName is identical.
	merged := map[string]any{
		"render_version": mcpRenderVersion,
		"server_name":    sanitizedName,
		"url":            mcp.Spec.Endpoint,
		"transport":      mcp.Spec.Transport,
		"params":         paramsMap,
	}
	canonicalBytes, err := canonicalJSON(merged)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mcpserver controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 7b: Existence probe (vanish-detection, v0.4.5) ──────────────
	// When the safety-relist enqueues a CR whose status already pins a
	// ServerID, verify the entry still exists in LiteLLM. On not-found OR
	// id-drift, clear ServerID locally so the CREATE branch fires in
	// Step 9 and the create_missing metric increments. Without this probe
	// the operator only detects spec-side drift (user CR edits) and
	// silently misses out-of-band deletes from POST/DELETE /v1/mcp/server
	// — defeating the safety-relist (observed prod chaos 2026-05-23:
	// 9 MCPServers Ready=True/Synced in K8s but absent from LiteLLM
	// because mass-delete via API direct + hash-equal short-circuit
	// skipped re-POST forever).
	//
	// Mirrors model_controller.go Step 7b shape. Implementation uses
	// ListMCPServers + name filter because LiteLLM 1.83.10 has no
	// GET /v1/mcp/server/<id> endpoint. resolveServerIDByName probes
	// sanitized first, original name as fallback (FIX2.txt H-9).
	//
	// Errors are surfaced for controller-runtime backoff EXCEPT 401
	// (cache-invalidate fast-path; treat as "unknown" → leave ServerID).
	//
	// Cost: 1 LIST /v1/mcp/server per LiteLLMMCPServer per safety re-list
	// tick (5min + jitter). 26 CRs ≈ 5 LISTs/min. Acceptable.
	//
	// Skipped on first reconcile (lastRendered.hash still empty) — bootstrap
	// CREATE runs via ServerID empty already, and the orphan-adoption probe
	// in Step 9 covers the upgrade-orphan scenario.
	if mcp.Status.LastRendered.ServerID != "" && mcp.Status.LastRendered.Hash != "" {
		clear, probeErr := probeVanishedResourceID(ctx,
			mcp.Status.LastRendered.ServerID,
			func(c context.Context) (string, error) {
				// v0.4.6: CachedListMCPServers dedupes concurrent
				// vanish-probes across all MCPServer CRs at the
				// litellm.DefaultListCacheTTL granularity (~30s).
				entries, err := snap.Client.CachedListMCPServers(c)
				if err != nil {
					return "", err
				}
				for _, e := range entries {
					if e.ServerName == sanitizedName || (mcp.Name != sanitizedName && e.ServerName == mcp.Name) {
						return e.ServerID, nil
					}
				}
				return "", nil
			},
			r.Cache.InvalidateOn401, logger, "mcpserver")
		if probeErr != nil {
			return ctrl.Result{}, probeErr
		}
		if clear {
			mcp.Status.LastRendered.ServerID = ""
		}
		// Hash left populated so firstReconcile=false → create_missing increments.
	}

	// ─── Step 8: Hash-equal steady state ──────────────────────────────────
	if mcp.Status.LastRendered.Hash == currentRenderedHash &&
		mcp.Status.LastRendered.ServerID != "" &&
		mcp.Status.ObservedGeneration == mcp.Generation {
		// Stale-status heal — see model_controller.go Step 8 for the
		// connection-flap rationale. Same shape.
		if ready := apimeta.FindStatusCondition(mcp.Status.Conditions, "Ready"); ready == nil ||
			ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
			if err := r.writeStatus(ctx, &mcp, metav1.ConditionTrue, reasonSynced, "mcp server registered"); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
		}
		metrics.CRStatusAgeTracker.RecordSuccess(mcpServerKind, mcp.Name)
		// Periodic safety-relist requeue: with Owns predicate filtering
		// to generation-changes only (v0.4.3), the child no longer
		// reconciles on Discovery refresh ticks. Out-of-band LiteLLM
		// API deletions would never reach the safety-relist sweep in
		// Step 7. RequeueAfter ensures the controller self-ticks at a
		// known interval so drift detection still fires. Overrides the
		// pre-v0.4.3 REL-02 "event-driven only" intent — the OWN watch
		// was the de-facto polling channel; we re-introduce explicit
		// periodic requeue now that the watch is properly filtered.
		return ctrl.Result{RequeueAfter: withJitter(mcpSafetyRelistInterval)}, nil
	}

	// ─── Step 9: Branch CREATE vs UPDATE (simple PUT — verdict ✓) ─────────
	//
	// Phase 3 OWN-04 first-reconcile sentinel inherited: on observedGeneration
	// == 0 OR lastRendered.hash == "", drift counters are suppressed
	// (the first CREATE is not a "drift correction"; it's the user's
	// initial registration).
	firstReconcile := mcp.Status.ObservedGeneration == 0 || mcp.Status.LastRendered.Hash == ""

	// Construct the body. FIX5 H-1: every top-level key in spec.params that
	// corresponds to a modeled field on litellm.MCPServerRequest /
	// MCPServerUpdateRequest is forwarded to LiteLLM. Reserved structural
	// keys (server_id, server_name, alias, url, transport, spec_path) are
	// stamped from the CR — see reservedMCPParamKeys.
	ext := extractMCPParams(paramsMap)

	var newServerID string
	// FIX2.txt H-9 orphan adoption (2026-05-22): when ServerID is empty,
	// probe LiteLLM for an existing record under EITHER the sanitized name
	// or the original K8s metadata.name BEFORE issuing a CREATE. If found,
	// adopt the existing record and switch to the UPDATE arm — this heals
	// the v0.1.1 → v0.1.3 upgrade-orphan scenario without requiring a
	// kubectl probe-and-edit dance.
	if mcp.Status.LastRendered.ServerID == "" {
		if adopted := r.resolveServerIDByName(ctx, snap.Client, sanitizedName, mcp.Name, logger); adopted != "" {
			mcp.Status.LastRendered.ServerID = adopted
		}
	}
	// Verdict ✓ per 05-00-SUMMARY.md — UPDATE arm uses simple PUT;
	// delete-and-recreate (with paramsKeys shrinkage detection) is NOT
	// present in this codebase.
	if mcp.Status.LastRendered.ServerID == "" {
		// CREATE path — first reconcile or stale status.
		// CR-10 / D-7.1-10: set Alias = ServerName per LiteLLM 1.83.10 requirement.
		// Probe 10c (HTTP 201) sent alias=server_name; the dogfood LiteLLMMCPServer/exa-mcp
		// (HTTP 400) did not include alias. The spec marks alias optional, but
		// 1.83.10 validates it at create time (diagnostic-first diff, 2026-05-19).
		// FIX4.txt H-1: stamp operator identity into the mcp_info bag.
		// LiteLLM 1.83.10 /v1/mcp/server has no native audit field at the
		// top level, but mcp_info is freeform and persisted verbatim.
		// CREATE stamps both created_by + updated_by.
		ext.MCPInfo = stampMCPIdentity(ext.MCPInfo, true)
		createReq := &litellm.MCPServerRequest{
			ServerName:                sanitizedName,
			Alias:                     sanitizedName, // alias = server_name per 1.83.10 (D-7.1-10)
			URL:                       mcp.Spec.Endpoint,
			Transport:                 mcp.Spec.Transport,
			Description:               ext.Description,
			AuthType:                  ext.AuthType,
			Credentials:               ext.Credentials,
			MCPInfo:                   ext.MCPInfo,
			MCPAccessGroups:           ext.MCPAccessGroups,
			AllowedTools:              ext.AllowedTools,
			ToolNameToDisplayName:     ext.ToolNameToDisplayName,
			ToolNameToDescription:     ext.ToolNameToDescription,
			ExtraHeaders:              ext.ExtraHeaders,
			StaticHeaders:             ext.StaticHeaders,
			Command:                   ext.Command,
			Args:                      ext.Args,
			Env:                       ext.Env,
			AuthorizationURL:          ext.AuthorizationURL,
			TokenURL:                  ext.TokenURL,
			RegistrationURL:           ext.RegistrationURL,
			OAuth2Flow:                ext.OAuth2Flow,
			AllowAllKeys:              ext.AllowAllKeys,
			AvailableOnPublicInternet: ext.AvailableOnPublicInternet,
		}
		result, err := snap.Client.CreateMCPServer(ctx, createReq)
		if err != nil {
			return r.classifyMutationError(ctx, &mcp, logger, err, "POST /v1/mcp/server")
		}
		newServerID = result.ServerID
		// Phase 3 OWN-04 + Phase 5 : suppress create_missing on the
		// very first reconcile (ObservedGeneration == 0) — the user's initial
		// POST is not a "drift correction". On subsequent re-creates (after a
		// delete-and-recreate cycle OR external-vanish recovery where
		// ObservedGeneration > 0 already), the counter DOES increment.
		if !firstReconcile && mcp.Status.ObservedGeneration > 0 {
			metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing").Inc()
		}
		logger.V(1).Info("mcp server created in LiteLLM", "serverID", newServerID)
	} else {
		// UPDATE path — simple PUT /v1/mcp/server (verdict ✓ per
		// 05-00-SUMMARY.md; PUT IS wholesale-replace on 1.83.10-stable).
		// FIX4.txt H-1: stamp updated_by only on UPDATE (LiteLLM keeps the
		// original creator).
		ext.MCPInfo = stampMCPIdentity(ext.MCPInfo, false)
		updateReq := &litellm.MCPServerUpdateRequest{
			ServerID:                  mcp.Status.LastRendered.ServerID,
			ServerName:                sanitizedName,
			URL:                       mcp.Spec.Endpoint,
			Transport:                 mcp.Spec.Transport,
			Description:               ext.Description,
			AuthType:                  ext.AuthType,
			Credentials:               ext.Credentials,
			MCPInfo:                   ext.MCPInfo,
			MCPAccessGroups:           ext.MCPAccessGroups,
			AllowedTools:              ext.AllowedTools,
			ToolNameToDisplayName:     ext.ToolNameToDisplayName,
			ToolNameToDescription:     ext.ToolNameToDescription,
			ExtraHeaders:              ext.ExtraHeaders,
			StaticHeaders:             ext.StaticHeaders,
			Command:                   ext.Command,
			Args:                      ext.Args,
			Env:                       ext.Env,
			AuthorizationURL:          ext.AuthorizationURL,
			TokenURL:                  ext.TokenURL,
			RegistrationURL:           ext.RegistrationURL,
			AllowAllKeys:              ext.AllowAllKeys,
			AvailableOnPublicInternet: ext.AvailableOnPublicInternet,
		}
		if _, err := snap.Client.UpdateMCPServer(ctx, updateReq); err != nil {
			return r.classifyMutationError(ctx, &mcp, logger, err, "PUT /v1/mcp/server")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("mcp", "update_drifted").Inc()
		}
		// UPDATE keeps the same LiteLLM server_id (verdict ✓ — PUT
		// preserves the row's identity).
		newServerID = mcp.Status.LastRendered.ServerID
		logger.V(1).Info("mcp server updated in LiteLLM (simple PUT)", "serverID", newServerID)
	}

	// ─── Step 11: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	mcp.Status.LastRendered = litellmv1alpha1.MCPServerLastRenderedStatus{
		Hash:       currentRenderedHash,
		ParamsKeys: sortedKeys(paramsMap),
		ServerID:   newServerID,
		At:         &now,
	}
	if err := r.writeStatus(ctx, &mcp, metav1.ConditionTrue, reasonSynced, "mcp server registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state; suppress controller-runtime's
			// ERROR "Reconciler error" log + backoff for this error class.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(mcpServerKind, mcp.Name)
	metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
	logger.V(1).Info("mcp server reconciled", "serverID", newServerID, "hash", currentRenderedHash)

	// Periodic safety-relist requeue — see Step 8 rationale.
	return ctrl.Result{RequeueAfter: withJitter(mcpSafetyRelistInterval)}, nil
}

// resolveServerIDByName re-resolves a LiteLLMMCPServer's LiteLLM server_id
// from a metadata.name lookup via ListMCPServers + in-memory filter.
//
// Probes `sanitizedName` first. If no match AND `originalName` differs
// from sanitizedName, probes `originalName` as a fallback (FIX2.txt
// HIGH-9 orphan adoption, 2026-05-22): a pre-v0.1.2 LiteLLM record was
// created under the K8s metadata.name without sanitization; the v0.1.2
// sanitizer mangled the name and broke the link. With the v0.1.3
// no-op-on-safe sanitizer, sanitized==original for inputs without the
// forbidden char, so the fallback only triggers for inputs that DID
// contain the forbidden char (rare, but heals the upgrade-orphan
// scenario without operator intervention).
//
// Used by:
//   - finalizer path when status.lastRendered.ServerID is empty (Phase 5
//     D-02 stale-status fallback).
//   - CREATE-vs-UPDATE branch in the main reconcile loop (FIX2.txt H-9):
//     probe before POSTing so an orphaned LiteLLM record gets adopted as
//     an UPDATE instead of triggering a 4xx duplicate-name CREATE.
//
// Returns "" if neither name matches or the LIST call fails non-fatally.
// The caller decides what to do with "" (finalizer drains anyway; the
// CREATE branch proceeds with POST).
func (r *MCPServerReconciler) resolveServerIDByName(ctx context.Context, llm *litellm.Client, sanitizedName, originalName string, logger logr.Logger) string {
	entries, err := llm.ListMCPServers(ctx)
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
		logger.V(1).Info("name-resolve: ListMCPServers failed; treating as absent", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.ServerName == sanitizedName {
			return e.ServerID
		}
	}
	if originalName != "" && originalName != sanitizedName {
		for _, e := range entries {
			if e.ServerName == originalName {
				logger.Info("name-resolve: adopted orphan under pre-sanitize name (FIX2.txt H-9)",
					"sanitizedName", sanitizedName, "originalName", originalName, "serverID", e.ServerID)
				return e.ServerID
			}
		}
	}
	return ""
}

// classifyMutationError handles §7.7 error classification for LiteLLM
// mutation calls (CreateMCPServer / UpdateMCPServer / DeleteMCPServer):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return
// (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff.
func (r *MCPServerReconciler) classifyMutationError(ctx context.Context, mcp *litellmv1alpha1.LiteLLMMCPServer, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		r.Cache.InvalidateOn401()
		msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
		if werr := r.writeStatus(ctx, mcp, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
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
		msg := rejectedMessage(opDesc, err, errStr)
		if werr := r.writeStatus(ctx, mcp, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", errStr)
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		// FIX2.txt H-2: periodically re-reconcile so an upstream fix
		// (or a CR edit landing during the rate-limiter quiet window)
		// heals without external poke.
		return ctrl.Result{RequeueAfter: r.Cache.Snapshot().NormalizedRequeueOnRejectedAfter()}, nil
	}

	// 5xx / network transient — return err for controller-runtime backoff.
	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", errStr)
	metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "error").Inc()
	return ctrl.Result{}, err
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact. Callers MUST ensure no secret material reaches `message`.
func (r *MCPServerReconciler) writeStatus(
	ctx context.Context,
	mcp *litellmv1alpha1.LiteLLMMCPServer,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Uses Update (not Patch + MergeFrom) because callers mutate
	// mcp.Status.LastRendered before this call; a MergeFrom orig captured
	// here would already carry the mutation and the resulting patch would
	// omit ServerID, leaving the server with an empty value and causing a
	// duplicate POST /mcp/server/add on the next reconcile. 409 conflict
	// noise on this Update path is demoted to V(1) by logStatusUpdateErr
	// at each call site.
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: mcp.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&mcp.Status.Conditions, cond)
	mcp.Status.ObservedGeneration = mcp.Generation
	// FIX2.txt LOW-6: per-CR reconcile-outcome counter labeled by
	// kind/namespace/result. Fired here so every status-write also
	// surfaces on the prometheus dashboard without an extra call site.
	metrics.LitellmOperatorReconcileTotal.WithLabelValues(
		mcpServerKind, mcp.Namespace, metrics.ReasonToReconcileResult(reason),
	).Inc()
	return r.Status().Update(ctx, mcp)
}

// secretToMCPServers maps a Secret update event to the set of LiteLLMMCPServer
// CRs that reference it via spec.secrets[].secretRef.name (Phase 3 D-06
// rotation-propagation pattern). Uses the field indexer registered in
// cmd/main.go.
func (r *MCPServerReconciler) secretToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
	var mcpList litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &mcpList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{MCPServerSecretRefIndexField: obj.GetName()},
	); err != nil {
		r.Log.V(1).Info("secretToMCPServers: list failed; skipping", "error", err)
		return nil
	}
	out := make([]reconcile.Request, 0, len(mcpList.Items))
	for i := range mcpList.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&mcpList.Items[i]),
		})
	}
	return out
}

// SetupWithManager registers the MCPServerReconciler with controller-runtime.
//
// Watches:
//   - For(&LiteLLMMCPServer{}) — primary watch.
//   - Watches(&Secret{}, secretToMCPServers) — SEC-09 rotation propagation.
//   - Watches(&LiteLLMMCPServer{}, enqueueMCPServerSiblings) — sibling
//     fan-in for the alpha-last-wins conflict resolver. On Create/Delete
//     of any MCPServer in the namespace, re-enqueue every OTHER MCPServer
//     so loser→winner promotion (winner deleted) and new-winner
//     appearance (a name that sorts later created) fire promptly. Update
//     events are filtered out — metadata.name is immutable and any
//     separator change is already covered by the Connection-fanin above,
//     so per-status-write Updates would only add reconcile noise.
//
// Named("mcpserver") — controller registry name (Phase 5 PATTERNS.md L506).
// No Owns(.) and no safety re-list channel — Phase 5 may add
// these if cross-CR vanish detection requires them (Phase 7 dogfood gate).
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMMCPServer{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToMCPServers),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToMCPServers),
			builder.WithPredicates(connectionReadyTransition()),
		).
		Watches(
			&litellmv1alpha1.LiteLLMMCPServer{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueMCPServerSiblings),
			builder.WithPredicates(mcpServerSiblingCreateDelete()),
		).
		WithOptions(transientBackoffOptions()).
		Named("mcpserver")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}

// enqueueMCPServerSiblings returns reconcile requests for every OTHER
// LiteLLMMCPServer in the namespace of obj. Used by the self-watch in
// SetupWithManager to drive the alpha-last-wins conflict resolver on
// Create/Delete events: a new sibling may collapse onto the same
// sanitized name (forcing a winner re-pick), and a deleted sibling may
// promote a previously-losing CR to winner.
func (r *MCPServerReconciler) enqueueMCPServerSiblings(ctx context.Context, obj client.Object) []reconcile.Request {
	me, ok := obj.(*litellmv1alpha1.LiteLLMMCPServer)
	if !ok {
		return nil
	}
	var list litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &list, client.InNamespace(me.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].UID == me.UID {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out
}

// mcpServerSiblingCreateDelete fires only on Create/Delete events for
// the sibling watch. Update events are dropped — metadata.name is
// immutable, so an Update on a sibling never changes its sanitized
// name (separator changes fan in via the Connection watch instead),
// and per-reconcile status writes would create a reconcile storm
// otherwise. Generic events are likewise irrelevant for this fan-in.
func mcpServerSiblingCreateDelete() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		UpdateFunc:  func(_ event.UpdateEvent) bool { return false },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
}

// stampMCPIdentity injects identity.Operator() into the mcp_info bag.
// LiteLLM 1.83.10 /v1/mcp/server has no native audit field at the top
// level, but mcp_info is freeform and persisted verbatim. On CREATE
// both created_by + updated_by are stamped; on UPDATE only updated_by
// (LiteLLM-side audit semantics — original creator is immutable).
// FIX4.txt H-1.
func stampMCPIdentity(mcpInfo map[string]any, includeCreatedBy bool) map[string]any {
	if mcpInfo == nil {
		mcpInfo = map[string]any{}
	}
	if includeCreatedBy {
		mcpInfo["created_by"] = identity.Operator()
	}
	mcpInfo["updated_by"] = identity.Operator()
	return mcpInfo
}
