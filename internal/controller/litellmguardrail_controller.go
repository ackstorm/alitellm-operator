// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
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
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// guardrailFinalizer is the finalizer the reconciler holds on every
// LiteLLMGuardRail CR — DELETE /guardrails/{id} fires from the deletion
// path before the CR is garbage-collected.
const guardrailFinalizer = "guardrails.litellm.ackstorm.ai/finalizer"

// guardrailKind is the metric label.
const guardrailKind = "LiteLLMGuardRail"

// GuardrailSecretRefIndexField is the field indexer path used by the
// Secret-watch fan-in (SEC-09 rotation propagation — same pattern as
// LiteLLMModel). Exported so cmd/main.go can wire the IndexField call.
const GuardrailSecretRefIndexField = ".spec.guardrails.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexGuardrailSecretRefs is the field indexer for GuardrailSecretRefIndexField.
func IndexGuardrailSecretRefs(o client.Object) []string {
	gr, ok := o.(*litellmv1alpha1.LiteLLMGuardRail)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(gr.Spec.Secrets))
	for _, s := range gr.Spec.Secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// Reason values surfaced on the Ready condition. Reused reasons (Synced,
// LiteLLMUnavailable, SecretNotFound) match the LiteLLMModel reconciler
// for cross-kind consistency; the guardrail-specific reasons are listed
// below.
const (
	// reasonConflictsWithConfigGuardrail — a row with the same
	// guardrailName already exists in LiteLLM with definition_location =
	// "config" (loaded from the LiteLLM config file). Such rows are NOT
	// addressable via POST/PUT/DELETE /guardrails; the operator surfaces
	// Ready=False and skips all mutation.
	reasonConflictsWithConfigGuardrail = "ConflictsWithConfigGuardrail"

	// reasonInvalidMode — the spec.mode combination is rejected by the
	// reconciler (e.g. realtime_input_transcription not alone).
	reasonInvalidMode = "InvalidMode"

	// reasonPoolProviderMismatch — two CRs share guardrailName but
	// declare different spec.provider values. LiteLLM cannot
	// load-balance across heterogeneous backends; the second-reconciled
	// CR is held back with this reason.
	reasonPoolProviderMismatch = "PoolProviderMismatch"
)

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmguardrails,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmguardrails/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmguardrails/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// GuardRailReconciler reconciles LiteLLMGuardRail CRs against the LiteLLM
// /guardrails HTTP surface. State machine per reconcile:
//
//  1. Fetch the CR (NotFound → no-op).
//  2. Deletion path: if DeletionTimestamp is set and the finalizer is held,
//     DELETE /guardrails/{persisted-id} (404 treated as success); remove the
//     finalizer.
//  3. Finalizer-add path: stamp the finalizer if missing, return.
//  4. Connection-gating via r.Cache.Snapshot(); LiteLLMConnection/default
//     must be Ready before any mutation can fire.
//  5. Validate spec.mode (realtime_input_transcription must be alone).
//  6. Resolve spec.secrets[]; substitute {{NAME}} placeholders in
//     spec.params + spec.info.
//  7. Build the rendered Guardrail body (typed wrapper around the
//     pass-through bag); compute SHA-256 over the canonical-JSON body to
//     drive drift detection.
//  8. Discover any existing entry via GET /v2/guardrails/list:
//     * CONFIG row → ConflictsWithConfigGuardrail (no mutation).
//     * DB row + first reconcile → adopt the guardrail_id and PUT.
//     * Hash matches → steady state; no mutation.
//     * Hash differs → PUT (drift correction).
//     * No row exists → POST (create_missing on subsequent reconciles).
//  9. Persist status.lastRendered (hash, paramsKeys, infoKeys, guardrailID,
//     definitionLocation, poolSize, at) and set Ready=True / reason=Synced.
//
// Anti-patterns avoided (mirrors ModelReconciler):
//   - NO RequeueAfter on steady-state paths (REL-02 — event-driven only).
//   - NO comparison against LiteLLM response for drift (server masks
//     sensitive litellm_params; operator hash is the source of truth).
//   - NO safety re-list (deferred to a follow-up phase).
type GuardRailReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Cache      connection.ConnectionCache
	Recorder   record.EventRecorder
	Namespace  string
	Log        logr.Logger
	BootEvents <-chan event.GenericEvent
	// ConnectionRebuilt is the cache.Rebuilt() channel; fires when the
	// LiteLLMConnection cache transitions to Ready=true. Wired via
	// ConnectionRebuiltSource so dependent CRs re-enqueue as soon as
	// the snapshot is populated, closing the boot-time race the
	// connectionReadyTransition predicate cannot catch (issue #44).
	// nil-safe — tests using FakeConnectionCache leave this unset.
	ConnectionRebuilt <-chan event.GenericEvent
}

// Reconcile implements the LiteLLMGuardRail state machine.
//
//nolint:gocyclo // Linear state machine; splitting into helpers obscures the contract.
func (r *GuardRailReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("guardrail", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var gr litellmv1alpha1.LiteLLMGuardRail
	if err := r.Get(ctx, req.NamespacedName, &gr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2: Deletion path ─────────────────────────────────────────────
	if !gr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&gr, guardrailFinalizer) {
			// Issue #23: resolve effective deletion policy once.
			policy := deletionpolicy.Resolve(&gr, gr.Spec.DeletionPolicy)
			onAckMissing := func(reason string) error {
				if policy == deletionpolicy.Delete {
					metrics.DeletionBlocked.Record(guardrailKind, gr.Namespace, gr.Name)
					r.Recorder.Eventf(&gr, corev1.EventTypeWarning, "LiteLLMDeleteBlocked",
						"deletionPolicy=Delete and LiteLLM ack missing (%s); finalizer retained", reason)
					return fmt.Errorf("delete blocked: %s", reason)
				}
				metrics.DeletionOrphanedTotal.WithLabelValues(guardrailKind).Inc()
				r.Recorder.Eventf(&gr, corev1.EventTypeNormal, "LiteLLMDeleteOrphaned",
					"deletionPolicy=Orphan and LiteLLM ack missing (%s); finalizer removed; entry may persist", reason)
				return nil
			}

			snap := r.Cache.Snapshot()
			guardrailID := gr.Status.LastRendered.GuardrailID
			if snap.Usable() && guardrailID != "" {
				if err := snap.Client.DeleteGuardrail(ctx, guardrailID); err != nil {
					var auth401 *litellm.Auth401Error
					switch {
					case errors.As(err, &auth401):
						r.Cache.InvalidateOn401()
						logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
						// Issue #23: gate on resolved policy.
						if gerr := onAckMissing("401 on DeleteGuardrail"); gerr != nil {
							return ctrl.Result{}, gerr
						}
					case is4xxStatus(err):
						// 404 / 4xx — treat as success; entry is already gone.
						// Not ack-missing — LiteLLM positively reports the entry
						// is gone, so finalizer removal is safe under both
						// policies.
						metrics.DeletionBlocked.Forget(guardrailKind, gr.Namespace, gr.Name)
						logger.V(1).Info("deletion: 4xx from LiteLLM; treating as already-absent",
							"guardrailID", guardrailID, "error", err.Error())
					default:
						// Transient — leave finalizer for backoff.
						return ctrl.Result{}, err
					}
				} else {
					metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "delete_vanished").Inc()
					metrics.DeletionBlocked.Forget(guardrailKind, gr.Namespace, gr.Name)
					logger.Info("finalizer removed; LiteLLM guardrail deleted",
						"guardrailID", guardrailID)
				}
			} else if !snap.Usable() {
				// LiteLLM unavailable — cannot confirm absence; gate on policy.
				if err := onAckMissing("LiteLLM unavailable"); err != nil {
					return ctrl.Result{}, err
				}
			} else {
				// snap.Usable() && guardrailID == "": the operator never
				// persisted an ID, so it never confirmed a create. Treat as
				// confirmed-absent and drain regardless of policy — mirrors the
				// model controller's onConfirmedAbsent fix; routing this through
				// onAckMissing stranded deletionPolicy=Delete CRs in Terminating.
				metrics.DeletionBlocked.Forget(guardrailKind, gr.Namespace, gr.Name)
				logger.Info("finalizer removed; guardrail_id never persisted (confirmed absent)",
					"name", gr.Name)
			}

			metrics.CRStatusAgeTracker.Forget(guardrailKind, gr.Name)
			// Issue #23: idempotent Forget — clears DeletionBlocked gauge.
			metrics.DeletionBlocked.Forget(guardrailKind, gr.Namespace, gr.Name)
			controllerutil.RemoveFinalizer(&gr, guardrailFinalizer)
			if err := r.Update(ctx, &gr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Finalizer add path ────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&gr, guardrailFinalizer) {
		controllerutil.AddFinalizer(&gr, guardrailFinalizer)
		if err := r.Update(ctx, &gr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 4: Connection-gating ─────────────────────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 5: Validate spec.mode ────────────────────────────────────────
	if msg, ok := validateGuardrailMode(gr.Spec.Mode); !ok {
		if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonInvalidMode, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonInvalidMode)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 5.5: spec.secrets[].as uniqueness ────────────────────────────
	{
		seen := make(map[string]struct{}, len(gr.Spec.Secrets))
		for _, entry := range gr.Spec.Secrets {
			if _, exists := seen[entry.As]; exists {
				msg := fmt.Sprintf("spec.secrets[]: duplicate as value %q (must be unique within a LiteLLMGuardRail)", entry.As)
				if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
				}
				metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			seen[entry.As] = struct{}{}
		}
	}

	// ─── Step 6: Resolve spec.secrets[] ────────────────────────────────────
	secretMap, missMsg, err := resolveSecretMap(ctx, r.Client, gr.Namespace, gr.Spec.Secrets)
	if err != nil {
		return ctrl.Result{}, err
	}
	if missMsg != "" {
		if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonSecretNotFound, missMsg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 7: Decode + substitute spec.params and spec.info ─────────────
	paramsMap := make(map[string]any)
	if len(gr.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(gr.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}
	infoMap := make(map[string]any)
	if len(gr.Spec.Info.Raw) > 0 {
		if err := json.Unmarshal(gr.Spec.Info.Raw, &infoMap); err != nil {
			msg := "spec.info: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}

	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	referencedInfo, missingInfo, _ := substitution.Substitute(infoMap, secretMap)

	if len(missingParams)+len(missingInfo) > 0 {
		first := append(missingParams, missingInfo...)[0]
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as", first)
		if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// Strip reserved keys the user may have mirrored into spec.params —
	// the canonical source for these is the typed field, and a duplicate
	// inside the bag would silently override our overlay below. Emit a
	// Warning Event the first time we strip so the user sees the diagnostic.
	stripReservedGuardrailKeys(paramsMap, r.Recorder, &gr)

	// SEC-07 UnusedSecretRef detection.
	referenced := make(map[string]struct{})
	for _, n := range referencedParams {
		referenced[n] = struct{}{}
	}
	for _, n := range referencedInfo {
		referenced[n] = struct{}{}
	}
	for _, entry := range gr.Spec.Secrets {
		if _, ok := referenced[entry.As]; !ok {
			r.Recorder.Eventf(&gr, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params or spec.info",
				entry.As)
		}
	}

	// ─── Step 8: Render the wire body ──────────────────────────────────────
	body := renderGuardrailBody(&gr, paramsMap, infoMap)
	canonicalBytes, err := canonicalJSON(map[string]any{
		"guardrail_name":  body.GuardrailName,
		"litellm_params":  map[string]any(body.LitellmParams),
		"guardrail_info":  body.GuardrailInfo,
		"policy_template": body.PolicyTemplate,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("guardrail controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentHash := fmt.Sprintf("%x", sum)

	// ─── Step 8.5: Discovery — read /v2/guardrails/list once ───────────────
	existing, err := snap.Client.GetGuardrailByName(ctx, gr.Spec.GuardrailName)
	if err != nil {
		return r.classifyMutationError(ctx, &gr, logger, err, "GET /v2/guardrails/list")
	}

	// ─── Step 8.6: Existence probe (out-of-band DELETE detection) ──────────
	// When safety-re-list (or any event) enqueues a CR whose status pins a
	// GuardrailID, verify the row still exists in LiteLLM. Without this, an
	// out-of-band DELETE (UI / curl) goes undetected — by-name discovery
	// would surface a SIBLING in an LB pool, masking the missing row.
	// On not-found, clear persistedID locally so the CREATE branch fires
	// below and drift_corrected_total{action=create_missing} increments.
	//
	// Skipped on first reconcile (Hash == "") so initial bootstrap pays no
	// probe cost — CREATE fires anyway because GuardrailID is empty.
	if gr.Status.LastRendered.GuardrailID != "" && gr.Status.LastRendered.Hash != "" {
		row, probeErr := snap.Client.GetGuardrailByID(ctx, gr.Status.LastRendered.GuardrailID)
		if probeErr != nil {
			return r.classifyMutationError(ctx, &gr, logger, probeErr, "GET /v2/guardrails/list (existence probe)")
		}
		if row == nil {
			logger.Info("safety re-list detected out-of-band delete; clearing GuardrailID",
				"lastID", gr.Status.LastRendered.GuardrailID)
			gr.Status.LastRendered.GuardrailID = ""
			// Force re-discovery: don't trust the by-name lookup either,
			// since the sibling adoption guard depends on persistedID
			// (already cleared) plus existing != nil. By-name `existing`
			// was loaded before the probe; if it pointed at the just-deleted
			// row, refresh by re-running the by-name lookup.
			existing, err = snap.Client.GetGuardrailByName(ctx, gr.Spec.GuardrailName)
			if err != nil {
				return r.classifyMutationError(ctx, &gr, logger, err, "GET /v2/guardrails/list (post-probe refresh)")
			}
		}
	}

	// CONFIG row blocks all mutation.
	if existing != nil && existing.GuardrailDefinitionLocation == litellm.GuardrailDefinitionLocationConfig {
		msg := fmt.Sprintf("guardrail_name %q is loaded from the LiteLLM config file (definition_location=config); operator cannot mutate config-loaded rows",
			gr.Spec.GuardrailName)
		// Combined writer: stage LastRendered first so writeStatus's
		// retry loop persists the Ready condition AND the discovery
		// detail in a single status Patch. Pre-fix this used a
		// trailing Status().Update() second-write whose Patch raced
		// with `pollGuardrailCondition` poll loops in
		// TestGuardRail_ConflictsWithConfigGuardrail and produced an
		// intermittent CI flake (`definitionLocation: got "" want
		// config`). Mirrors the LiteLLMConnection combined-writer
		// pattern (writeReadyAndLoggingHealthy).
		gr.Status.LastRendered.DefinitionLocation = litellm.GuardrailDefinitionLocationConfig
		if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonConflictsWithConfigGuardrail, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonConflictsWithConfigGuardrail)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	firstReconcile := gr.Status.ObservedGeneration == 0 || gr.Status.LastRendered.Hash == ""

	// Pool-size + provider-homogeneity + sibling-ownership tracking.
	poolSize, providerMismatch, anySiblingOwns, err := r.checkGuardrailPool(ctx, &gr, snap.Client)
	if err != nil {
		return r.classifyMutationError(ctx, &gr, logger, err, "pool check (GET /v2/guardrails/list)")
	}
	if providerMismatch {
		msg := fmt.Sprintf("guardrail_name %q is shared with a sibling CR declaring a different spec.provider; LiteLLM cannot load-balance across heterogeneous providers",
			gr.Spec.GuardrailName)
		if werr := r.writeStatus(ctx, &gr, metav1.ConditionFalse, reasonPoolProviderMismatch, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonPoolProviderMismatch)
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 9: Steady state / drift / create ─────────────────────────────
	persistedID := gr.Status.LastRendered.GuardrailID

	// Adopt existing row if first-reconcile / persistedID stale and the
	// guardrail_name is already known to LiteLLM (DB row). Skip adoption
	// when a sibling CR has already taken ownership — otherwise both CRs
	// would compete to PUT the same row, defeating the LB-pool semantics
	// where each CR owns its own LiteLLM entry under a shared name.
	if persistedID == "" && existing != nil && existing.GuardrailID != "" && !anySiblingOwns && poolSize <= 1 {
		persistedID = existing.GuardrailID
		logger.V(1).Info("adopted existing LiteLLM guardrail (idempotency probe)",
			"guardrailID", persistedID)
	}

	// Steady-state.
	if persistedID != "" &&
		gr.Status.LastRendered.Hash == currentHash &&
		gr.Status.ObservedGeneration == gr.Generation {
		metrics.CRStatusAgeTracker.RecordSuccess(guardrailKind, gr.Name)
		// Refresh PoolSize on steady-state too — sibling membership may
		// have changed without a spec edit (a sibling CR was added).
		// M-B4: route through RetryOnConflict + re-Get and capture the error
		// (the previous plain Status().Update with a discarded error silently
		// lost the refresh under a conflict).
		if poolSize != gr.Status.LastRendered.PoolSize {
			gr.Status.LastRendered.PoolSize = poolSize
			if uerr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var fresh litellmv1alpha1.LiteLLMGuardRail
				if err := r.Get(ctx, client.ObjectKeyFromObject(&gr), &fresh); err != nil {
					return err
				}
				fresh.Status.LastRendered.PoolSize = poolSize
				return r.Status().Update(ctx, &fresh)
			}); uerr != nil {
				logStatusUpdateErr(logger, uerr, "reason", "PoolSizeRefresh")
			}
		}
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	now := metav1.NewTime(time.Now())
	if persistedID == "" {
		// CREATE.
		created, cerr := snap.Client.CreateGuardrail(ctx, body)
		if cerr != nil {
			return r.classifyMutationError(ctx, &gr, logger, cerr, "POST /guardrails")
		}
		persistedID = created.GuardrailID
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "create_missing").Inc()
		}
		logger.V(1).Info("guardrail created in LiteLLM", "guardrailID", persistedID)
	} else {
		// UPDATE.
		body.GuardrailID = persistedID
		if _, uerr := snap.Client.UpdateGuardrail(ctx, persistedID, body); uerr != nil {
			return r.classifyMutationError(ctx, &gr, logger, uerr, "PUT /guardrails/{id}")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "update_drifted").Inc()
		}
		logger.V(1).Info("guardrail updated in LiteLLM", "guardrailID", persistedID)
	}

	// ─── Step 10: Persist status ───────────────────────────────────────────
	gr.Status.LastRendered = litellmv1alpha1.GuardRailLastRenderedStatus{
		Hash:               currentHash,
		ParamsKeys:         sortedKeys(paramsMap),
		InfoKeys:           sortedKeys(infoMap),
		GuardrailID:        persistedID,
		DefinitionLocation: litellm.GuardrailDefinitionLocationDB,
		PoolSize:           poolSize,
		At:                 &now,
	}
	if err := r.writeStatus(ctx, &gr, metav1.ConditionTrue, reasonSynced, "guardrail registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(guardrailKind, gr.Name)
	metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
	logger.V(1).Info("guardrail reconciled", "guardrailID", persistedID, "hash", currentHash)
	return ctrl.Result{}, nil
}

// renderGuardrailBody overlays the typed fields onto the pass-through
// litellm_params bag. The typed fields always win — a user who places
// "guardrail" / "mode" / "default_on" / "policy_template" inside spec.params
// will have those keys stripped upstream (see stripReservedGuardrailKeys).
func renderGuardrailBody(
	gr *litellmv1alpha1.LiteLLMGuardRail,
	paramsMap, infoMap map[string]any,
) *litellm.GuardrailBody {
	params := litellm.LiteLLMGuardrailParams{}
	for k, v := range paramsMap {
		params[k] = v
	}

	// Provider — required by upstream.
	params["guardrail"] = gr.Spec.Provider

	// Mode — emit scalar when len == 1, list otherwise. Lowercase to
	// match LiteLLM's server-side normalize_lowercase validator so the
	// operator-side hash and the server-side row never disagree on
	// trivial casing differences.
	switch len(gr.Spec.Mode) {
	case 1:
		params["mode"] = litellm.NormalizeGuardrailMode(string(gr.Spec.Mode[0]))
	default:
		modes := make([]any, 0, len(gr.Spec.Mode))
		for _, m := range gr.Spec.Mode {
			modes = append(modes, litellm.NormalizeGuardrailMode(string(m)))
		}
		params["mode"] = modes
	}

	// default_on — *bool semantics: nil omits the key.
	if gr.Spec.DefaultOn != nil {
		params["default_on"] = *gr.Spec.DefaultOn
	}

	body := &litellm.GuardrailBody{
		GuardrailName:  gr.Spec.GuardrailName,
		LitellmParams:  params,
		PolicyTemplate: gr.Spec.PolicyTemplate,
	}
	if len(infoMap) > 0 {
		body.GuardrailInfo = infoMap
	}
	return body
}

// stripReservedGuardrailKeys removes reserved keys from a user-supplied
// spec.params bag. The reserved keyset matches what renderGuardrailBody
// overlays — leaving these keys in the bag would either silently mask
// the typed field (renderGuardrailBody runs after) or, worse, drift on
// the next reconcile because the hash sees both copies. Stripping is
// the safer contract.
//
// On any strip, a one-shot Warning Event is emitted naming the keys.
// This is deliberately not a hard rejection — most users will set them
// via the typed field, but a copy-paste from raw LiteLLM YAML might
// land them in the bag, and a Warning is a gentler escape hatch than a
// Ready=False.
func stripReservedGuardrailKeys(
	params map[string]any,
	rec record.EventRecorder,
	gr *litellmv1alpha1.LiteLLMGuardRail,
) {
	reserved := []string{"guardrail", "mode", "default_on", "policy_template", "guardrail_name"}
	var stripped []string
	for _, k := range reserved {
		if _, ok := params[k]; ok {
			delete(params, k)
			stripped = append(stripped, k)
		}
	}
	if len(stripped) > 0 && rec != nil {
		rec.Eventf(gr, corev1.EventTypeWarning, "ReservedKeyStripped",
			"reserved key(s) %s removed from spec.params; canonical source is the typed spec field",
			strings.Join(stripped, ", "))
	}
}

// validateGuardrailMode enforces the realtime-exclusivity rule:
// realtime_input_transcription must be the only element of spec.mode.
// All other combinations are permitted (admission already enforces the
// per-element enum + MinItems/MaxItems).
func validateGuardrailMode(modes []litellmv1alpha1.GuardRailMode) (string, bool) {
	if len(modes) == 0 {
		return "spec.mode must contain at least one entry", false
	}
	for _, m := range modes {
		if m == "realtime_input_transcription" && len(modes) > 1 {
			return "spec.mode: realtime_input_transcription is mutually exclusive with other slots", false
		}
	}
	return "", true
}

// checkGuardrailPool counts siblings sharing spec.guardrailName in the
// same namespace and detects heterogeneous-provider pools. The count
// includes the current CR, so a single-entry pool returns 1.
//
// Implementation: list LiteLLMGuardRail CRs in the operator namespace
// and filter client-side by spec.guardrailName. The kubebuilder
// field-indexer alternative would require a second IndexField; for
// guardrails the list-and-filter cost is bounded by the count of
// guardrail CRs which is small in practice (≤ low tens).
func (r *GuardRailReconciler) checkGuardrailPool(
	ctx context.Context,
	cr *litellmv1alpha1.LiteLLMGuardRail,
	_ *litellm.Client,
) (poolSize int32, providerMismatch bool, anySiblingOwns bool, err error) {
	var list litellmv1alpha1.LiteLLMGuardRailList
	if err := r.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
		return 0, false, false, err
	}
	for i := range list.Items {
		sib := &list.Items[i]
		if sib.Spec.GuardrailName != cr.Spec.GuardrailName {
			continue
		}
		// Skip CRs in deletion; they will leave the pool shortly.
		if !sib.DeletionTimestamp.IsZero() {
			continue
		}
		// Saturate at math.MaxInt32 so gosec G115 (int→int32 narrowing) is
		// statically defused — len(list.Items) is bounded by the apiserver
		// page size and could never realistically approach this cap, but
		// counting in int32 here also lets the CRD status field hold the
		// value without a runtime conversion.
		if poolSize < math.MaxInt32 {
			poolSize++
		}
		// Provider homogeneity check excludes the CR itself.
		if sib.UID != cr.UID && sib.Spec.Provider != cr.Spec.Provider {
			providerMismatch = true
		}
		// "Sibling ownership" — a different CR sharing this name already
		// holds a persisted GuardrailID. Used to disable adoption so the
		// current CR creates its own LiteLLM row instead of stealing the
		// sibling's row (would silently degrade an LB pool to one entry).
		if sib.UID != cr.UID && sib.Status.LastRendered.GuardrailID != "" {
			anySiblingOwns = true
		}
	}
	return poolSize, providerMismatch, anySiblingOwns, nil
}

// classifyMutationError maps an HTTP error from a guardrail mutation
// call onto the §7.7 reason set:
//   - Auth401Error → cache invalidate + LiteLLMUnavailable + nil return.
//   - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
//   - 5xx / network → return err for controller-runtime backoff.
func (r *GuardRailReconciler) classifyMutationError(
	ctx context.Context,
	gr *litellmv1alpha1.LiteLLMGuardRail,
	logger logr.Logger,
	err error,
	opDesc string,
) (ctrl.Result, error) {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		r.Cache.InvalidateOn401()
		msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
		if werr := r.writeStatus(ctx, gr, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	if is4xxStatus(err) {
		msg := rejectedMessage(opDesc, err, err.Error())
		if werr := r.writeStatus(ctx, gr, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", err.Error())
		metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success").Inc()
		return ctrl.Result{RequeueAfter: r.Cache.Snapshot().NormalizedRequeueOnRejectedAfter()}, nil
	}

	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", err.Error())
	metrics.ReconcileTotal.WithLabelValues(guardrailKind, "error").Inc()
	return ctrl.Result{}, err
}

// writeStatus sets the Ready condition + observedGeneration + LastRendered
// on the latest resourceVersion of the CR. Retries on optimistic-lock
// conflicts (same shape as ModelReconciler.writeStatus).
func (r *GuardRailReconciler) writeStatus(
	ctx context.Context,
	gr *litellmv1alpha1.LiteLLMGuardRail,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	cond := buildReadyCondition(gr.Generation, status, reason, message)
	desiredLR := gr.Status.LastRendered
	desiredObs := gr.Generation

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh litellmv1alpha1.LiteLLMGuardRail
		if err := r.Get(ctx, client.ObjectKeyFromObject(gr), &fresh); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&fresh.Status.Conditions, cond)
		fresh.Status.ObservedGeneration = desiredObs
		fresh.Status.LastRendered = desiredLR
		if u := r.Status().Update(ctx, &fresh); u != nil {
			return u
		}
		gr.Status = fresh.Status
		gr.ResourceVersion = fresh.ResourceVersion
		return nil
	})
	recordReconcileMetric(guardrailKind, gr.Namespace, reason)
	return err
}

// secretToGuardrails maps a Secret update event to the set of
// LiteLLMGuardRail CRs that reference it.
func (r *GuardRailReconciler) secretToGuardrails(ctx context.Context, obj client.Object) []reconcile.Request {
	return secretToRequests(ctx, r.Client, r.Log, &litellmv1alpha1.LiteLLMGuardRailList{}, obj.GetNamespace(), obj.GetName(), GuardrailSecretRefIndexField, "secretToGuardrails")
}

// connectionToGuardrails enqueues every guardrail when the
// LiteLLMConnection transitions to Ready=True (mirrors the Model fan-in).
func (r *GuardRailReconciler) connectionToGuardrails(ctx context.Context, obj client.Object) []reconcile.Request {
	// IN-03: route through connectionFanIn so all five connection fan-in
	// mappers share the same trigger-namespace contract — informer path
	// uses conn.Namespace, raw-source path falls back to r.Namespace.
	return connectionFanIn(ctx, r.Client, obj, &litellmv1alpha1.LiteLLMGuardRailList{}, r.Namespace, "LiteLLMGuardRail")
}

// SetupWithManager registers the GuardRailReconciler with the manager.
//
// Optional safetyRelistCh — when non-nil, wired as a source.TypedFunc so
// the GuardRailSafetyRelistRunnable can enqueue reconcile.Requests
// without adding a RequeueAfter path (REL-02 compliance).
func (r *GuardRailReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMGuardRail{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToGuardrails),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToGuardrails),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("litellmguardrail")

	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}

	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToGuardrails); src != nil {
		b = b.WatchesRawSource(src)
	}

	if len(safetyRelistCh) > 0 && safetyRelistCh[0] != nil {
		ch := safetyRelistCh[0]
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

// GuardRailSafetyRelistRunnable implements manager.Runnable for the
// guardrail safety re-list (out-of-band DELETE recovery). On each tick
// it lists every LiteLLMGuardRail CR in the operator's namespace and
// enqueues it; the reconciler's existence probe (Step 8.6) detects rows
// that vanished in LiteLLM and falls through to CREATE, incrementing
// drift_corrected_total{domain=guardrail,action=create_missing}.
//
// Interval is configurable: 30m in production (cmd/main.go), 100ms in
// envtests. REL-02 compliance: the runnable uses a ticker + channel
// rather than the reconciler's RequeueAfter so the grep gate stays at
// exactly 1.
type GuardRailSafetyRelistRunnable struct {
	Client    client.Client
	Namespace string
	Interval  time.Duration
	Log       logr.Logger
	// RequeueCh is the channel the runnable writes reconcile.Requests to.
	// SetupWithManager wires this as a source.TypedFunc raw source.
	RequeueCh chan reconcile.Request
}

// Start implements manager.Runnable. Ticks at Interval, lists all
// guardrails in Namespace, enqueues each. Channel-full drops are
// tolerated — the next tick will re-enqueue.
func (r *GuardRailSafetyRelistRunnable) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var list litellmv1alpha1.LiteLLMGuardRailList
			if err := r.Client.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
				r.Log.V(1).Info("guardrail safety re-list: list failed; skipping tick", "error", err)
				continue
			}
			for i := range list.Items {
				req := reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
				}
				select {
				case r.RequeueCh <- req:
				default:
					// Channel full — skip; next tick retries.
				}
			}
			r.Log.V(1).Info("guardrail safety re-list: enqueued", "count", len(list.Items))
		}
	}
}
