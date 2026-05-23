// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/ackstorm/alitellm-operator/internal/identity"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// modelFinalizer is the finalizer name managed by the LiteLLMModel reconciler
// per spec §7.5 and canonical-refs. Issuing POST /model/delete against LiteLLM
// removes the model entry from LiteLLM before the CR is fully removed from etcd.
const modelFinalizer = "models.litellm.ackstorm.ai/finalizer"

// modelKind is the metric label for LiteLLMModel CRs.
const modelKind = "LiteLLMModel"

// SecretRefIndexField is the field indexer path registered in cmd/main.go
// for reverse-mapping Secret names back to Models that reference them
// (D-06 — SEC-09 rotation propagation). Exported so cmd/main.go can use
// the same constant for both the IndexField registration and the MatchingFields
// selector in the secret-to-models mapper.
const SecretRefIndexField = ".spec.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexModelSecretRefs is the field indexer function for SecretRefIndexField.
// Returns the list of Secret names referenced by model.spec.secrets[*].secretRef.name.
// Exported so cmd/main.go can pass it to mgr.GetFieldIndexer.IndexField.
func IndexModelSecretRefs(o client.Object) []string {
	model, ok := o.(*litellmv1alpha1.LiteLLMModel)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(model.Spec.Secrets))
	for _, s := range model.Spec.Secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// ModelReconciler reconciles LiteLLMModel CRs against LiteLLM 1.82.6 per spec §7.2
// and CONTEXT.md D-01.D-08.
//
// State machine (per-reconcile):
// - Step 1: fetch the CR (NotFound → return nil).
// - Step 2a: DeletionTimestamp set → issue POST /model/delete (with
// name-resolve fallback via GetModelInfoByName if modelID is empty)
// → RemoveFinalizer → Update.
// - Step 2b: finalizer absent → AddFinalizer → Update → return.
// - Step 3: connection-gating: r.Cache.Snapshot. !snap.Ready →
// writeStatus(LiteLLMUnavailable) + return nil.
// - Step 4: resolve spec.secrets[] → secretMap.
// - Step 5: decode spec.params + spec.info → apply substitution.
// - Step 6: SEC-07 UnusedSecretRef detection → Event per unused as.
// - Step 7: compute currentRenderedHash (SHA-256 of canonical JSON body
// excluding model_info.id per D-01).
// - Step 8: hash-equal steady-state → return nil (AC-R1).
// - Step 9: branch CREATE vs UPDATE (or delete-and-recreate on shrinkage per D-02).
// - Step 10: classify error from LiteLLM mutation call.
// - Step 11: write status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided (confirmed via bbdsoftware review + CONTEXT.md):
// - NO comparison against LiteLLM response (D-01 — operator-side hash only).
// - NO typed UpdateLiteLLMParams struct (LiteLLMParams = map[string]any).
// - NO PATCH /model/{id}/update (spec §7.2 forbids; POST /model/update only).
// - NO global LIST-and-prune (OWN-01 — one name per reconcile).
// - NO RequeueAfter (REL-02 — Model is event-driven only).
type ModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface — NEVER the concrete *connection.Cache type.
	// Tests substitute *FakeConnectionCache without code change (D-12).
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMModel object — Normal/UnusedSecretRef
	// (SEC-07) and Warning/ProjectionOverride (spec §5.1 Identity tier). Must be
	// non-nil in production; tests pass mgr.GetEventRecorderFor("model-controller")
	// from the envtest manager.
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
}

// Reconcile implements the LiteLLMModel state machine.
//
//nolint:gocyclo // Linear state machine — splitting into helpers obscures the §7.2 mapping.
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("model", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var model litellmv1alpha1.LiteLLMModel
	if err := r.Get(ctx, req.NamespacedName, &model); err != nil {
		if apierrors.IsNotFound(err) {
			// CR deleted — finalizer cleanup will have already run.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !model.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&model, modelFinalizer) {
			snap := r.Cache.Snapshot()
			if snap.Ready {
				modelID := model.Status.LastRendered.ModelID
				if modelID != "" {
					// D-04: use persisted LiteLLM UUID.
					if err := snap.Client.DeleteModel(ctx, modelID); err != nil {
						var auth401 *litellm.Auth401Error
						if errors.As(err, &auth401) {
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							// Anti-storm: return nil (REL-06). We remove the finalizer anyway
							// so the CR can be garbage-collected — LiteLLM entry may linger
							// until the next reconcile cycle with a valid connection.
						} else {
							// Transient error — return for backoff. Finalizer stays until delete succeeds.
							return ctrl.Result{}, err
						}
					} else {
						// OWN-03: increment delete_vanished on every successful finalizer-time DELETE.
						// Phase 4 LiteLLMModelDiscovery vanish detection enriches this counter via the same
						// code path (Discovery deletes the child CR → child finalizer runs this path).
						// NO first-reconcile suppression — every successful delete is a real drift correction.
						metrics.DriftCorrectedTotal.WithLabelValues("model", "delete_vanished").Inc()
						logger.Info("finalizer removed; LiteLLM model deleted", "modelID", modelID)
					}
				} else {
					// D-04 stale-status fallback: resolve by name.
					resolved, err := snap.Client.GetModelInfoByName(ctx, model.Name)
					if err != nil {
						var auth401 *litellm.Auth401Error
						if errors.As(err, &auth401) {
							r.Cache.InvalidateOn401()
							logger.Info("deletion name-resolve: 401 fast-path; cache invalidated", "path", auth401.Path)
							// Anti-storm: fall through to remove finalizer.
						} else {
							return ctrl.Result{}, err
						}
					} else if resolved != nil {
						if err := snap.Client.DeleteModel(ctx, resolved.ModelInfo.ID); err != nil {
							var auth401 *litellm.Auth401Error
							if errors.As(err, &auth401) {
								r.Cache.InvalidateOn401()
								logger.Info("deletion: 401 fast-path after name-resolve; cache invalidated", "path", auth401.Path)
							} else {
								return ctrl.Result{}, err
							}
						} else {
							// OWN-03: increment delete_vanished (same site as the direct-ID path above).
							metrics.DriftCorrectedTotal.WithLabelValues("model", "delete_vanished").Inc()
							logger.Info("finalizer removed; LiteLLM model deleted (via name-resolve)", "modelID", resolved.ModelInfo.ID)
						}
					} else {
						// resolved == nil → entry already absent.
						logger.Info("finalizer removed; LiteLLM entry already absent", "name", model.Name)
					}
				}
			} else {
				// LiteLLM unavailable on deletion — remove finalizer anyway (pragmatic).
				// The LiteLLM entry may persist until manually reconciled.
				logger.Info("LiteLLM unavailable on deletion; finalizer removed; entry MAY persist in LiteLLM until manually reconciled")
			}

			// OBS-03: drop the cr_status_age_seconds label before the CR is gone
			// so /metrics cardinality never grows monotonically (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(modelKind, model.Name)
			controllerutil.RemoveFinalizer(&model, modelFinalizer)
			if err := r.Update(ctx, &model); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&model, modelFinalizer) {
		controllerutil.AddFinalizer(&model, modelFinalizer)
		if err := r.Update(ctx, &model); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connection-gating (D-08) ──────────────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Ready {
		reason := snap.Reason
		if reason == "" {
			// Zero-value snapshot — no probe has completed yet.
			// Per Phase 2 D-07 entry-state convention, use "Connecting"
			// as the D-08 default so the message is not misleadingly empty.
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &model, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 3.5: SEC-03 uniqueness of spec.secrets[].as values ──────────
	// Detect duplicate spec.secrets[].as values before secret resolution.
	// Two entries sharing the same .as value would silently overwrite each
	// other in secretMap (a Go map), masking the first Secret — a silent data
	// error. Reject early with Ready=False, reason=InvalidConfig so the user
	// sees a clear diagnostic message naming the duplicated identifier.
	//
	// Reason reuse: InvalidConfig is already the established reason for "spec
	// is semantically wrong" (see lines below for spec.params/spec.info JSON
	// parse errors using the same reason). Consistent with OWN-09 — duplicate
	// .as is a deterministic spec error, NOT a transient failure, so we
	// return nil (not err) after writeStatus.
	//
	// Note: no r.Recorder.Eventf on this path — status condition Ready=False
	// is the user-facing surface; an Event would be redundant.
	{
		seen := make(map[string]struct{}, len(model.Spec.Secrets))
		for _, entry := range model.Spec.Secrets {
			if _, exists := seen[entry.As]; exists {
				msg := fmt.Sprintf("spec.secrets[]: duplicate as value %q (SEC-03: must be unique within a LiteLLMModel)", entry.As)
				if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
				}
				metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			seen[entry.As] = struct{}{}
		}
	}

	// ─── Step 4: Resolve Secrets referenced by spec.secrets[] ─────────────
	// Per-reconcile cache in a local variable (CONTEXT.md Claude's Discretion item 2).
	secretMap := make(map[string]string)
	for _, entry := range model.Spec.Secrets {
		var secret corev1.Secret
		secretKey := types.NamespacedName{
			Namespace: model.Namespace,
			Name:      entry.SecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				msg := model.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
				if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
				}
				metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
				return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
			}
			return ctrl.Result{}, err
		}
		val, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			msg := model.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
			if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
		}
		secretMap[entry.As] = string(val)
	}

	// ─── Step 5: Decode spec.params + spec.info into map[string]any ────────
	paramsMap := make(map[string]any)
	if len(model.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(model.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}
	infoMap := make(map[string]any)
	if len(model.Spec.Info.Raw) > 0 {
		if err := json.Unmarshal(model.Spec.Info.Raw, &infoMap); err != nil {
			msg := "spec.info: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}

	// Apply substitution to params.
	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	// Apply substitution to info.
	referencedInfo, missingInfo, _ := substitution.Substitute(infoMap, secretMap)

	// Collect all missing placeholders.
	allMissing := append(missingParams, missingInfo...)
	if len(allMissing) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as", allMissing[0])
		if werr := r.writeStatus(ctx, &model, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 6: SEC-07 UnusedSecretRef detection ──────────────────────────
	// Compute the union of referenced as-names across both bags.
	referencedSet := make(map[string]struct{})
	for _, n := range referencedParams {
		referencedSet[n] = struct{}{}
	}
	for _, n := range referencedInfo {
		referencedSet[n] = struct{}{}
	}
	for _, entry := range model.Spec.Secrets {
		if _, ok := referencedSet[entry.As]; !ok {
			// §9.1: Event message contains only the as NAME (CEL-validated uppercase); never the secret value or secretRef coordinates.
			r.Recorder.Eventf(&model, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params or spec.info",
				entry.As)
		}
	}

	// ─── Step 7: Compute currentRenderedHash (D-01) ────────────────────────
	// REMOVE model_info.id from infoMap before hashing (D-01: exclude the
	// operator-set overlay so create-vs-update reconciles don't oscillate).
	// If user-supplied spec.info.id is present, emit ProjectionOverride Warning.
	if _, hasUserID := infoMap["id"]; hasUserID {
		// spec.info.id collides with the operator overlay — emit Warning Event
		// (per spec §5.1 Identity tier — operator-set field). The operator's
		// overlay always wins (null on create, resolved UUID on update).
		r.Recorder.Eventf(&model, corev1.EventTypeWarning, "ProjectionOverride",
			"user-supplied spec.info.id overridden by operator-managed model_info.id (spec §5.1 Identity tier — operator overlay always wins)")
	}
	delete(infoMap, "id") // always remove before hashing and body construction

	// Build merged canonical body.
	merged := map[string]any{
		"litellm_params": paramsMap,
		"model_info":     infoMap,
	}
	canonicalBytes, err := canonicalJSON(merged)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("model controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 7b: Existence probe (D-03 / AC-M3 conformance) ────────────────
	// When the safety re-list enqueues a CR whose status already pins a
	// ModelID, verify the entry still exists in LiteLLM. On
	// not-found OR id-drift, clear ModelID locally so the CREATE
	// branch fires in Step 9 and the create_missing metric increments.
	// Without this probe the operator only detects spec-side drift (user
	// CR edits) and silently misses out-of-band deletes — defeating the
	// safety re-list.
	//
	// Implementation uses GetModelInfoByName (name-resolve) because
	// LiteLLM 1.83.10 rejects GET /model/info?litellm_model_id=<id> with
	// HTTP 400. The by-name endpoint returns (nil, nil) on not-found.
	//
	// Cost: 1 GET /model/info per LiteLLMModel per safety re-list tick. At the
	// 30m production interval = 2 calls/hour/LiteLLMModel. At Tier 2's 10s
	// interval = 6 calls/min/LiteLLMModel — acceptable.
	//
	// Skipped on first reconcile (status.lastRendered.hash still empty)
	// so initial bootstrap does not pay the probe; the CREATE path runs
	// by virtue of ModelID being empty already.
	if model.Status.LastRendered.ModelID != "" && model.Status.LastRendered.Hash != "" {
		entry, probeErr := snap.Client.GetModelInfoByName(ctx, model.Name)
		if probeErr != nil {
			// 401 / 5xx / network — let controller-runtime back off.
			return ctrl.Result{}, probeErr
		}
		switch {
		case entry == nil:
			logger.Info("safety re-list detected out-of-band delete; clearing ModelID",
				"lastID", model.Status.LastRendered.ModelID)
			model.Status.LastRendered.ModelID = ""
		case entry.ModelInfo.ID != model.Status.LastRendered.ModelID:
			logger.Info("safety re-list detected id drift; clearing ModelID",
				"lastID", model.Status.LastRendered.ModelID,
				"currentID", entry.ModelInfo.ID)
			model.Status.LastRendered.ModelID = ""
		}
		// Hash left populated so firstReconcile=false → create_missing metric increments.
	}

	// ─── Step 8: Hash-equal steady state (AC-R1) ───────────────────────────
	if model.Status.LastRendered.Hash == currentRenderedHash &&
		model.Status.LastRendered.ModelID != "" &&
		model.Status.ObservedGeneration == model.Generation {
		// Steady state — no mutation needed.
		metrics.CRStatusAgeTracker.RecordSuccess(modelKind, model.Name)
		return ctrl.Result{}, nil
	}

	// ─── Step 9: Branch CREATE vs UPDATE (or delete-and-recreate per D-02) ──
	//
	// OWN-04 first-reconcile sentinel: when observedGeneration == 0 OR
	// lastRendered.hash == "", this is the first reconcile for the CR. Per
	// OWN-04, first-reconcile name-collision with a pre-existing LiteLLM entry
	// is silently overwritten — NO drift_corrected_total increment, NO Event,
	// NO Warning. The sentinel suppresses both create_missing and update_drifted
	// increments on this path.
	firstReconcile := model.Status.ObservedGeneration == 0 || model.Status.LastRendered.Hash == ""

	var newModelID string
	if model.Status.LastRendered.ModelID == "" {
		// CREATE path — first reconcile or stale status (out-of-band DELETE after
		// lastRendered was populated triggers this branch when ModelID is empty).
		//
		// Idempotency probe (OWN-04 / D-03 robustness): if a prior reconcile
		// POSTed successfully but the status write lost the optimistic-lock
		// race ("object has been modified"), controller-runtime retries
		// Reconcile with stale status — ModelID empty, firstReconcile
		// true. Without this probe we would POST a duplicate entry on every
		// retry. Resolve by name first; if LiteLLM already has a deployment
		// for this LiteLLMModel name, adopt its id and skip the POST. OWN-04 silent-
		// overwrite semantics already permit name collisions on first
		// reconcile, so adoption is consistent and does NOT increment
		// drift_corrected_total.
		if existing, probeErr := snap.Client.GetModelInfoByName(ctx, model.Name); probeErr != nil {
			return ctrl.Result{}, probeErr
		} else if existing != nil && existing.ModelInfo.ID != "" {
			newModelID = existing.ModelInfo.ID
			logger.V(1).Info("model already exists in LiteLLM; adopting id (idempotency probe)", "modelID", newModelID)
			// FIX4.txt H-1 (2026-05-22): the adoption branch skips POST
			// /model/new, so pre-v0.2.0 entries (or any out-of-band entries
			// created without a model_info.created_by) never received an
			// identity stamp — that's exactly what the prod smoke-test
			// observed under v0.2.0 (UI "Created By: Unknown" on every
			// operator-managed row). Re-issue POST /model/update with
			// model_info.updated_by so the next UI refresh shows
			// alitellm-operator/<version> on adopted rows. CreatedBy is
			// intentionally NOT stamped — LiteLLM 1.83.x keeps the original
			// creator across updates and the operator is by definition not
			// the original creator on the adoption path.
			// FIX7 H-1 (2026-05-23): LiteLLM 1.85.1 requires the model id
			// inside model_info — not at the top level. Sending root-level
			// id triggers a 400 "Authentication Error, model not found"
			// from LiteLLM's body parser.
			if _, uerr := snap.Client.UpdateModel(ctx, &litellm.UpdateDeployment{
				ModelName:     model.Name,
				LiteLLMParams: litellm.LiteLLMParams(paramsMap),
				ModelInfo: litellm.ModelInfo{
					ID:        newModelID,
					UpdatedBy: identity.Operator(),
				},
			}); uerr != nil {
				return r.classifyMutationError(ctx, &model, logger, uerr, "POST /model/update (FIX4 H-1 adoption stamp)")
			}
		} else {
			// CR-16 / D-7.1-16 (2026-05-19): leave ModelInfo zero-valued so the
			// JSON body omits both `model_info` (empty struct → still emitted but
			// with no id field since ModelInfo.ID is omitempty per types.go fix)
			// and prevents LiteLLM 1.83.10 from storing model_id="" in the DB.
			req := &litellm.Deployment{
				ModelName:     model.Name,
				LiteLLMParams: litellm.LiteLLMParams(paramsMap),
				// FIX2.txt M-8 (2026-05-22): stamp operator identity so
				// the LiteLLM UI "Created By" column shows
				// alitellm-operator/<version> instead of "Unknown".
				ModelInfo: litellm.ModelInfo{
					CreatedBy: identity.Operator(),
					UpdatedBy: identity.Operator(),
				},
			}
			result, err := snap.Client.CreateModel(ctx, req)
			if err != nil {
				return r.classifyMutationError(ctx, &model, logger, err, "POST /model/new")
			}
			newModelID = result.ModelInfo.ID
			// OBS-02 / D-03: increment create_missing when the safety re-list detects
			// an out-of-band DELETE (i.e. lastRendered was populated on a prior reconcile
			// but ModelID is now empty because status was cleared after an
			// out-of-band delete was discovered). Suppressed on first reconcile (OWN-04).
			if !firstReconcile {
				metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing").Inc()
			}
			logger.V(1).Info("model created in LiteLLM", "modelID", newModelID)
		}
	} else {
		// UPDATE path (or delete-and-recreate on key shrinkage per D-02).
		currentParamsKeys := sortedKeys(paramsMap)
		currentInfoKeys := sortedKeys(infoMap)

		// Detect key shrinkage per D-02 (delete-and-recreate semantics).
		removedParamsKeys := setDiff(model.Status.LastRendered.ParamsKeys, currentParamsKeys)
		removedInfoKeys := setDiff(model.Status.LastRendered.InfoKeys, currentInfoKeys)
		shrinkage := len(removedParamsKeys) > 0 || len(removedInfoKeys) > 0

		if shrinkage {
			// D-02: delete-and-recreate on key shrinkage (Probe 9 ✗ + Probe 9b ✗).
			oldID := model.Status.LastRendered.ModelID
			logger.V(1).Info("D-02 shrinkage detected; delete-and-recreate",
				"oldModelID", oldID,
				"removedParamsKeys", removedParamsKeys,
				"removedInfoKeys", removedInfoKeys)

			if err := snap.Client.DeleteModel(ctx, oldID); err != nil {
				return r.classifyMutationError(ctx, &model, logger, err, "POST /model/delete (D-02 shrinkage)")
			}

			createReq := &litellm.Deployment{
				ModelName:     model.Name,
				LiteLLMParams: litellm.LiteLLMParams(paramsMap),
				// FIX2.txt M-8: stamp identity on D-02 recreate too.
				ModelInfo: litellm.ModelInfo{
					ID:        "",
					CreatedBy: identity.Operator(),
					UpdatedBy: identity.Operator(),
				},
			}
			result, err := snap.Client.CreateModel(ctx, createReq)
			if err != nil {
				return r.classifyMutationError(ctx, &model, logger, err, "POST /model/new (D-02 recreate)")
			}
			newModelID = result.ModelInfo.ID
			// D-02 shrinkage delete+recreate is treated as an update_drifted correction
			// (operator-detected spec change, not safety-re-list-detected missing entry).
			// Suppressed on first reconcile (OWN-04).
			if !firstReconcile {
				metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted").Inc()
			}
			logger.V(1).Info("D-02 model re-created in LiteLLM", "newModelID", newModelID)
		} else {
			// No shrinkage — normal POST /model/update (no nulls per D-02 Probe 9b positive path).
			// FIX7 H-1 (2026-05-23): LiteLLM 1.85.1 schema requires the model
			// id inside model_info, NOT at the top level. The prior D-7.1-13
			// claim (1.83.10 top-level id) was retired — sending a root-level
			// id triggers a 400 "Authentication Error, model not found".
			updateReq := &litellm.UpdateDeployment{
				ModelName:     model.Name,
				LiteLLMParams: litellm.LiteLLMParams(paramsMap),
				// FIX2.txt M-8: stamp updated_by on every UPDATE. We do
				// NOT touch CreatedBy here — LiteLLM keeps the original
				// creator across updates.
				ModelInfo: litellm.ModelInfo{
					ID:        model.Status.LastRendered.ModelID,
					UpdatedBy: identity.Operator(),
				},
			}
			if _, err := snap.Client.UpdateModel(ctx, updateReq); err != nil {
				return r.classifyMutationError(ctx, &model, logger, err, "POST /model/update")
			}
			// OBS-02 / D-01: increment update_drifted when currentRenderedHash !=
			// status.lastRendered.hash AND the entry exists in LiteLLM (UPDATE path
			// is reached precisely when hash differs AND ModelID is set).
			// Suppressed on first reconcile (OWN-04).
			if !firstReconcile {
				metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted").Inc()
			}
			// UPDATE keeps the same LiteLLM UUID.
			newModelID = model.Status.LastRendered.ModelID
			logger.V(1).Info("model updated in LiteLLM", "modelID", newModelID)
		}
	}

	// ─── Step 11: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	model.Status.LastRendered = litellmv1alpha1.LastRenderedStatus{
		Hash:       currentRenderedHash,
		ParamsKeys: sortedKeys(paramsMap),
		InfoKeys:   sortedKeys(infoMap),
		ModelID:    newModelID,
		At:         &now,
	}

	if err := r.writeStatus(ctx, &model, metav1.ConditionTrue, reasonSynced, "model registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state. The retry.RetryOnConflict
			// loop inside writeStatus already exhausted client-go's
			// default backoff before bubbling this up, so further
			// controller-runtime retries would only emit ERROR log noise.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(modelKind, model.Name)
	metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
	logger.V(1).Info("model reconciled", "modelID", newModelID, "hash", currentRenderedHash)

	return ctrl.Result{}, nil
}

// classifyMutationError handles the §7.7 error classification for LiteLLM
// mutation calls (CreateModel / UpdateModel / DeleteModel):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff (REL-02).
func (r *ModelReconciler) classifyMutationError(ctx context.Context, model *litellmv1alpha1.LiteLLMModel, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		r.Cache.InvalidateOn401()
		msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
		if werr := r.writeStatus(ctx, model, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
		return ctrl.Result{}, nil // anti-storm: return nil, NOT err
	}

	// Check if it's a 4xx (non-401) — these appear in the error string as "litellm: 4XX on ."
	// Use a simple check: if error string contains a 4xx code.
	errStr := err.Error()
	is4xx := false
	for _, prefix := range []string{"litellm: 4", "litellm: 42", "litellm: 40", "litellm: 41", "litellm: 43", "litellm: 44", "litellm: 45"} {
		if len(errStr) > len(prefix) && errStr[:len(prefix)] == prefix {
			is4xx = true
			break
		}
	}
	// More robust check: parse the HTTP status code from the error string
	if !is4xx {
		for code := 400; code < 500; code++ {
			prefix := fmt.Sprintf("litellm: %d on", code)
			if len(errStr) >= len(prefix) && errStr[:len(prefix)] == prefix {
				is4xx = true
				break
			}
		}
	}

	if is4xx {
		// Deterministic 4xx — LiteLLMRejected. FIX2.txt M-5: surface the
		// parsed envelope message in condition.Message when available
		// (clipped to 200 bytes for kubectl-friendly wide output).
		msg := rejectedMessage(opDesc, err, errStr)
		if werr := r.writeStatus(ctx, model, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", errStr)
		metrics.ReconcileTotal.WithLabelValues(modelKind, "success").Inc()
		// FIX2.txt H-2: periodic requeue on deterministic 4xx.
		return ctrl.Result{RequeueAfter: r.Cache.Snapshot().NormalizedRequeueOnRejectedAfter()}, nil
	}

	// 5xx / network transient — return err for controller-runtime backoff (REL-02).
	// Do NOT writeStatus on transient path — per OWN-09, leave previous status unchanged.
	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", errStr)
	metrics.ReconcileTotal.WithLabelValues(modelKind, "error").Inc()
	return ctrl.Result{}, err
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact. Callers MUST ensure no secret material reaches `message`.
func (r *ModelReconciler) writeStatus(
	ctx context.Context,
	model *litellmv1alpha1.LiteLLMModel,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: model.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// Retry on optimistic-lock conflict ("the object has been modified").
	// Without this, a 409 leaks to controller-runtime which re-enters
	// Reconcile with stale status (firstReconcile=true), causing duplicate
	// POST /model/new calls before the idempotency probe (Step 9) absorbs
	// them. Re-fetch on each attempt so we apply the condition + intent
	// onto the latest resourceVersion. The intent — Conditions delta,
	// ObservedGeneration, and LastRendered — is carried from the in-memory
	// model the caller built.
	//
	// NOTE on Patch + MergeFrom: a previous refactor attempted to switch
	// the inner write to a merge patch. That broke status semantics
	// because callers mutate model.Status.LastRendered BEFORE invoking
	// writeStatus; an orig captured here already carries the mutation,
	// the patch body omits LastRendered, and the next reconcile sees
	// Hash=="" and re-POSTs. Stay on Update; the 409 noise this path
	// emits is demoted to V(1) by logStatusUpdateErr at the call site.
	desiredLastRendered := model.Status.LastRendered
	desiredObservedGen := model.Generation

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh litellmv1alpha1.LiteLLMModel
		if err := r.Get(ctx, client.ObjectKeyFromObject(model), &fresh); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&fresh.Status.Conditions, cond)
		fresh.Status.ObservedGeneration = desiredObservedGen
		fresh.Status.LastRendered = desiredLastRendered
		if updErr := r.Status().Update(ctx, &fresh); updErr != nil {
			return updErr
		}
		// Propagate back to caller so subsequent reads (logger, metrics) see persisted state.
		model.Status = fresh.Status
		model.ResourceVersion = fresh.ResourceVersion
		return nil
	})
	metrics.LitellmOperatorReconcileTotal.WithLabelValues(
		modelKind, model.Namespace, metrics.ReasonToReconcileResult(reason),
	).Inc()
	return err
}

// secretToModels maps a Secret update event to the set of LiteLLMModel CRs that
// reference it via spec.secrets[].secretRef.name (D-06 — SEC-09 rotation
// propagation). Uses the field indexer registered in cmd/main.go.
func (r *ModelReconciler) secretToModels(ctx context.Context, obj client.Object) []reconcile.Request {
	var modelList litellmv1alpha1.LiteLLMModelList
	if err := r.List(ctx, &modelList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{SecretRefIndexField: obj.GetName()},
	); err != nil {
		r.Log.V(1).Info("secretToModels: list failed; skipping", "error", err)
		return nil
	}
	out := make([]reconcile.Request, 0, len(modelList.Items))
	for i := range modelList.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&modelList.Items[i]),
		})
	}
	return out
}

// SetupWithManager registers the ModelReconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMModel{}) — primary watch.
// - Watches(&Secret{}, secretToModels) — SEC-09 rotation propagation via D-06 indexer.
// - WatchesRawSource(TypedFunc) — optional §7.6 safety re-list channel enqueues.
//
// Named("model") — controller registry name.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMModel{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToModels),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToModels),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("model")

	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}

	// Wire optional safety re-list channel as a typed-func source so the
	// Runnable can enqueue Models without adding a RequeueAfter path
	// (REL-02 compliance). The source.TypedFunc must return promptly
	// after spawning the drain loop as a goroutine, because controller-runtime
	// calls TypedFunc.Start synchronously and a blocking implementation
	// prevents the controller from completing its Start phase.
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

// canonicalJSON produces RFC 8785-approximated canonical JSON for hashing.
// Sorted keys + standard json.Marshal. For the operator's use case (no float
// precision concerns, integer/string values from LiteLLM params), this is
// sufficient for the AC-R1 steady-state hash (D-01 Claude's Discretion).
func canonicalJSON(v any) ([]byte, error) {
	return canonicalMarshal(v)
}

func canonicalMarshal(v any) ([]byte, error) {
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		result := []byte{'{'}
		for i, k := range keys {
			keyBytes, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			valBytes, err := canonicalMarshal(typed[k])
			if err != nil {
				return nil, err
			}
			if i > 0 {
				result = append(result, ',')
			}
			result = append(result, keyBytes...)
			result = append(result, ':')
			result = append(result, valBytes...)
		}
		result = append(result, '}')
		return result, nil

	case []any:
		result := []byte{'['}
		for i, elem := range typed {
			b, err := canonicalMarshal(elem)
			if err != nil {
				return nil, err
			}
			if i > 0 {
				result = append(result, ',')
			}
			result = append(result, b...)
		}
		result = append(result, ']')
		return result, nil

	default:
		return json.Marshal(v)
	}
}

// sortedKeys returns the sorted list of keys in a map[string]any.
// Used to populate status.lastRendered.paramsKeys / infoKeys (D-02).
func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setDiff returns elements in `a` that are NOT in `b`.
// Used for D-02 shrinkage detection: removedKeys = persistedKeys \ desiredKeys.
func setDiff(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, k := range b {
		bSet[k] = struct{}{}
	}
	var diff []string
	for _, k := range a {
		if _, ok := bSet[k]; !ok {
			diff = append(diff, k)
		}
	}
	return diff
}

// ModelSafetyRelistRunnable implements manager.Runnable for the §7.6
// 30-min safety re-list. On each tick it lists all LiteLLMModel CRs in the
// operator's namespace and enqueues them for reconciliation. This causes
// the Model reconciler to re-run the full hash-compare + LiteLLM existence
// check (D-03 existence-only scope), recovering from out-of-band DELETEs
// that the event-driven watch would not otherwise detect.
//
// The reconciler itself handles the drift_corrected_total{action=create_missing}
// increment when the existence check reveals the LiteLLM entry is gone.
//
// The Interval is configurable: 30*time.Minute in production (cmd/main.go),
// 100*time.Millisecond in envtests so tests don't time out.
//
// REL-02 compliance: the safety re-list uses a time.Ticker inside a Runnable
// rather than RequeueAfter, so it does NOT add a RequeueAfter path to the
// reconciler. The grep gate stays at exactly 1.
type ModelSafetyRelistRunnable struct {
	Client    client.Client
	Namespace string
	Interval  time.Duration
	Log       logr.Logger
	// RequeueCh is the channel the runnable writes reconcile.Requests to.
	// SetupWithManager wires this as a Source.Channel watch.
	RequeueCh chan reconcile.Request
}

// Start implements manager.Runnable. It ticks at Interval, listing all
// LiteLLMModel CRs in Namespace and enqueuing each via RequeueCh.
func (r *ModelSafetyRelistRunnable) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var modelList litellmv1alpha1.LiteLLMModelList
			if err := r.Client.List(ctx, &modelList, client.InNamespace(r.Namespace)); err != nil {
				r.Log.V(1).Info("safety re-list: list failed; skipping tick", "error", err)
				continue
			}
			for i := range modelList.Items {
				req := reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&modelList.Items[i]),
				}
				select {
				case r.RequeueCh <- req:
				default:
					// Channel full — skip this item; it will be retried on next tick.
				}
			}
			r.Log.V(1).Info("safety re-list: enqueued models", "count", len(modelList.Items))
		}
	}
}
