// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
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

// mcpToolsetFinalizer is the finalizer name managed by the LiteLLMMCPToolset
// reconciler. Issuing DELETE /v1/mcp/toolset/<toolset_id> against LiteLLM
// removes the toolset before the CR is fully removed from etcd.
const mcpToolsetFinalizer = "mcptoolsets.litellm.ackstorm.ai/finalizer"

// mcpToolsetKind is the metric label for LiteLLMMCPToolset CRs.
const mcpToolsetKind = "LiteLLMMCPToolset"

// Events RBAC marker inheritance: the package-wide
// `+kubebuilder:rbac:groups="",resources=events,verbs=create;patch` marker
// lives on internal/controller/mcpserver_controller.go. kubebuilder marker
// scope is per-package — this reconciler INHERITS the events grant and MUST
// NOT duplicate it.

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcptoolsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcptoolsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcptoolsets/finalizers,verbs=update

// MCPToolsetReconciler reconciles LiteLLMMCPToolset CRs against LiteLLM's
// /v1/mcp/toolset endpoints. It mirrors A2AAgentReconciler's shape (the
// closest sibling: server-minted id, name-based adoption, vanish probe, churn
// breaker) with the secret/substitution/projection machinery stripped — this
// CRD has no spec.secrets and no free-form spec.params bag.
//
// State machine (per-reconcile):
//
//   - Step 1:  Fetch the CR (NotFound → return nil).
//   - Step 2a: DeletionTimestamp set → DELETE /v1/mcp/toolset/<id> (with
//     name-resolve fallback when ToolsetID is empty) → RemoveFinalizer.
//   - Step 2b: Finalizer absent → AddFinalizer → return.
//   - Step 3:  Connection-gating on snap.Usable() (issue #74 — NOT snap.Ready;
//     a Ready-with-nil-Client snapshot is reachable and nil-derefs).
//   - Step 7.5: Render spec.from into LiteLLM {server_id, tool_name} pairs.
//   - Step 8:  Compute currentRenderedHash over a stable map.
//   - Step 8b: Existence probe (vanish-detection) against GET /v1/mcp/toolset.
//   - Step 9:  Hash-equal steady-state short-circuit, INCLUDING the
//     Ready-condition heal (issue #102 — mandatory in every
//     steady-state short-circuit).
//   - Step 10: Branch CREATE (POST, id read from the RESPONSE) vs UPDATE
//     (PUT with the id in the BODY). A 409 on CREATE means a toolset
//     with this name already exists → adopt it by name.
//   - Step 11: Classify mutation errors per §7.7.
//   - Step 12: Update status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided:
//   - NO RequeueAfter for periodic drift (the SafetyRelistRunnable owns the
//     tick — issue #102).
//   - NO validation of spec.from[].server or .tools. LiteLLM accepts bogus
//     refs with 201 and grants nothing; the operator never parks on them.
type MCPToolsetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMMCPToolset object. No
	// Event is emitted on the happy path today, but the deletion-policy
	// newAckMissingFn requires a non-nil recorder.
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
	// ConnectionRebuilt — issue #44 cache-population race close. nil-safe.
	ConnectionRebuilt <-chan event.GenericEvent
	// Churn trips the RecreateThrottled breaker on a created-but-not-listed
	// toolset. SetupWithManager wires newChurnGuard(); tests may leave it nil.
	Churn *churnGuard
	// RecreateLimit is the per-CR recreates-per-minute ceiling. <= 0 →
	// DefaultRecreateLimitPerMin.
	RecreateLimit int
}

// Reconcile implements the LiteLLMMCPToolset state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the step mapping.
func (r *MCPToolsetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("mcptoolset", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var ts litellmv1alpha1.LiteLLMMCPToolset
	if err := r.Get(ctx, req.NamespacedName, &ts); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !ts.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ts, mcpToolsetFinalizer) {
			policy := deletionpolicy.Resolve(&ts, ts.Spec.DeletionPolicy)
			onAckMissing := newAckMissingFn(r.Recorder, &ts, mcpToolsetKind, ts.Namespace, ts.Name, policy)

			snap := r.Cache.Snapshot()
			if snap.Usable() {
				toolsetID := ts.Status.LastRendered.ToolsetID
				if toolsetID == "" {
					// Stale-status fallback: re-resolve by name via
					// GET /v1/mcp/toolset + in-memory filter.
					if resolved := r.resolveToolsetIDByName(ctx, snap.Client, ts.Name, logger); resolved != "" {
						toolsetID = resolved
					}
				}
				if toolsetID != "" {
					if err := snap.Client.DeleteMCPToolset(ctx, toolsetID); err != nil {
						var auth401 *litellm.Auth401Error
						switch {
						case errors.As(err, &auth401):
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							if gerr := onAckMissing("401 on DeleteMCPToolset"); gerr != nil {
								return ctrl.Result{}, gerr
							}
						case is4xxStatus(err):
							logger.Info("deletion: deterministic 4xx on DeleteMCPToolset; ack-missing", "error", err.Error())
							if gerr := onAckMissing("4xx on DeleteMCPToolset: " + err.Error()); gerr != nil {
								return ctrl.Result{}, gerr
							}
						default:
							// Transient error — return for backoff. Finalizer stays.
							return ctrl.Result{}, err
						}
					} else {
						metrics.DriftCorrectedTotal.WithLabelValues("mcptoolset", "delete_vanished").Inc()
						metrics.DeletionBlocked.Forget(mcpToolsetKind, ts.Namespace, ts.Name)
						logger.Info("finalizer removed; LiteLLM toolset deleted", "toolsetID", toolsetID)
					}
				} else {
					metrics.DeletionBlocked.Forget(mcpToolsetKind, ts.Namespace, ts.Name)
					logger.Info("finalizer removed; LiteLLM entry already absent (no pinned ID, name-resolve returned empty)", "name", ts.Name)
				}
			} else {
				if err := onAckMissing("LiteLLM unavailable"); err != nil {
					return ctrl.Result{}, err
				}
			}

			metrics.CRStatusAgeTracker.Forget(mcpToolsetKind, ts.Name)
			metrics.DeletionBlocked.Forget(mcpToolsetKind, ts.Namespace, ts.Name)
			controllerutil.RemoveFinalizer(&ts, mcpToolsetFinalizer)
			if err := r.Update(ctx, &ts); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&ts, mcpToolsetFinalizer) {
		controllerutil.AddFinalizer(&ts, mcpToolsetFinalizer)
		if err := r.Update(ctx, &ts); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connection-gating ─────────────────────────────────────────
	//
	// Usable(), NOT Ready (issue #74): Cache.Rebuild does not enforce the
	// Ready=true ⇒ Client!=nil invariant, so a Ready snapshot with a nil
	// Client is reachable and would nil-deref below.
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &ts, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(mcpToolsetKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 7.5: Build the request body ─────────────────────────────────
	//
	// No secrets, no substitution, no projection Events — spec.from is a
	// closed, typed shape. The only translation is server NAME → server_id,
	// which is best-effort and never fails (see serverIDResolver).
	resolve := serverIDResolver(ctx, r.Client, ts.Namespace)
	tools := renderToolsetTools(ts.Spec.From, resolve)
	body := &litellm.MCPToolsetRequest{
		ToolsetName: ts.Name,
		Description: ts.Spec.Description,
		Tools:       tools,
	}

	// ─── Step 8: Compute currentRenderedHash ──────────────────────────────
	//
	// Hash a stable map rather than the struct so the digest is independent
	// of Go field ordering.
	//
	// The hash covers the RESOLVED server ids on purpose: when a referenced
	// LiteLLMMCPServer finishes its first reconcile and gains a serverID, the
	// toolset's hash changes and the next reconcile rewrites LiteLLM with the
	// resolved id. That is the self-healing path for the unsynced-CR verbatim
	// fallback in serverIDResolver.
	hashInput := map[string]any{
		"toolset_name": ts.Name,
		"description":  ts.Spec.Description,
		"tools":        tools,
	}
	canonicalBytes, err := canonicalJSON(hashInput)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mcptoolset controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 8b: Existence probe (vanish-detection) ──────────────────────
	// Verify ToolsetID still exists in LiteLLM. On not-found OR id-drift,
	// clear ToolsetID so the Step 10 CREATE arm fires. Without this probe the
	// operator silently misses out-of-band deletes.
	//
	// Skipped on first reconcile (lastRendered.hash empty).
	if ts.Status.LastRendered.ToolsetID != "" && ts.Status.LastRendered.Hash != "" {
		clear, probeErr := probeVanishedResourceID(ctx,
			ts.Status.LastRendered.ToolsetID,
			func(c context.Context) (string, error) {
				entries, lerr := snap.Client.ListMCPToolsets(c)
				if lerr != nil {
					if errors.Is(lerr, litellm.ErrNotFound) {
						// Empty toolset set — the entry is confirmed absent.
						return "", nil
					}
					return "", lerr
				}
				for _, e := range entries {
					if e.ToolsetName == ts.Name {
						return e.ToolsetID, nil
					}
				}
				return "", nil
			},
			r.Cache.InvalidateOn401, logger, "mcptoolset")
		if probeErr != nil {
			return ctrl.Result{}, probeErr
		}
		if clear {
			ts.Status.LastRendered.ToolsetID = ""
		}
	}

	// ─── Step 9: Hash-equal steady state ──────────────────────────────────
	if ts.Status.LastRendered.Hash == currentRenderedHash &&
		ts.Status.LastRendered.ToolsetID != "" &&
		ts.Status.ObservedGeneration == ts.Generation {
		// Stale-status heal (issue #102): a Step 3 connection-gate write
		// stamps observedGeneration alongside Ready=False and leaves
		// lastRendered intact, so once the connection recovers all three
		// predicates hold and the reconciler would short-circuit forever on a
		// stale Ready=False. Every steady-state short-circuit MUST heal.
		if ready := apimeta.FindStatusCondition(ts.Status.Conditions, conditionTypeReady); ready == nil ||
			ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
			if err := r.writeStatus(ctx, &ts, metav1.ConditionTrue, reasonSynced, "toolset registered"); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
		}
		metrics.CRStatusAgeTracker.RecordSuccess(mcpToolsetKind, ts.Name)
		r.Churn.Forget(req.NamespacedName)
		// No RequeueAfter: the SafetyRelistRunnable owns the periodic tick.
		return ctrl.Result{}, nil
	}

	// ─── Step 10: Branch CREATE vs UPDATE ─────────────────────────────────
	//
	// First-reconcile sentinel: on observedGeneration == 0 OR
	// lastRendered.hash == "", drift counters are suppressed.
	firstReconcile := ts.Status.ObservedGeneration == 0 || ts.Status.LastRendered.Hash == ""

	var newToolsetID string
	if ts.Status.LastRendered.ToolsetID == "" {
		// CREATE path. LiteLLM MINTS the toolset_id — a supplied one is
		// ignored on 1.93.0 — so it must be read from the response, never
		// derived from metadata.name (unlike team_id / MCP server_id).
		//
		// Recreate circuit breaker: a recreate (not first reconcile) means the
		// vanish probe cleared a populated ToolsetID. If that repeats faster
		// than RecreateLimit/min the entry is created-but-not-listed; park the
		// CR instead of storming LiteLLM.
		if !firstReconcile {
			limit := r.RecreateLimit
			if limit <= 0 {
				limit = DefaultRecreateLimitPerMin
			}
			if n := r.Churn.Count(req.NamespacedName); n >= limit {
				msg := fmt.Sprintf("recreate throttled: %d recreates within %s (limit %d); "+
					"LiteLLM accepts POST /v1/mcp/toolset but the entry never appears on the existence probe "+
					"(created-but-not-listed); parked to avoid a reconcile storm. Retrying after %s.",
					n, churnWindow, limit, recreateThrottleBackoff)
				if werr := r.writeStatus(ctx, &ts, metav1.ConditionFalse, reasonRecreateThrottled, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonRecreateThrottled)
				}
				metrics.ReconcileTotal.WithLabelValues(mcpToolsetKind, "success").Inc()
				return ctrl.Result{RequeueAfter: recreateThrottleBackoff}, nil
			}
		}

		created, cerr := snap.Client.CreateMCPToolset(ctx, body)
		switch {
		case cerr == nil:
			newToolsetID = created.ToolsetID
			if !firstReconcile && ts.Status.ObservedGeneration > 0 {
				metrics.DriftCorrectedTotal.WithLabelValues("mcptoolset", "create_missing").Inc()
				r.Churn.Record(req.NamespacedName)
			}
			logger.V(1).Info("toolset created in LiteLLM", "toolsetID", newToolsetID)

		case rejectedStatus(cerr) == http.StatusConflict:
			// toolset_name is unique server-side. A 409 means a toolset with
			// this name already exists (operator restart, or an out-of-band
			// create). Adopt it by name and push our rendered state onto it
			// rather than parking the CR forever.
			adopted := r.resolveToolsetIDByName(ctx, snap.Client, ts.Name, logger)
			if adopted == "" {
				// 409 but the name is not listed — cannot adopt; surface the
				// original rejection.
				return r.classifyMutationError(ctx, &ts, logger, cerr, "POST /v1/mcp/toolset")
			}
			if _, uerr := snap.Client.UpdateMCPToolset(ctx, updateRequestFor(adopted, body)); uerr != nil {
				return r.classifyMutationError(ctx, &ts, logger, uerr, "PUT /v1/mcp/toolset (adopt)")
			}
			newToolsetID = adopted
			logger.V(1).Info("adopted pre-existing LiteLLM toolset by name", "toolsetID", adopted)

		default:
			return r.classifyMutationError(ctx, &ts, logger, cerr, "POST /v1/mcp/toolset")
		}
	} else {
		// UPDATE path — PUT /v1/mcp/toolset with the id in the BODY.
		if _, err := snap.Client.UpdateMCPToolset(ctx, updateRequestFor(ts.Status.LastRendered.ToolsetID, body)); err != nil {
			return r.classifyMutationError(ctx, &ts, logger, err, "PUT /v1/mcp/toolset")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("mcptoolset", "update_drifted").Inc()
		}
		newToolsetID = ts.Status.LastRendered.ToolsetID
		logger.V(1).Info("toolset updated in LiteLLM", "toolsetID", newToolsetID)
	}

	// ─── Step 12: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	ts.Status.LastRendered = litellmv1alpha1.MCPToolsetLastRenderedStatus{
		Hash:      currentRenderedHash,
		ToolsetID: newToolsetID,
		At:        &now,
	}
	if err := r.writeStatus(ctx, &ts, metav1.ConditionTrue, reasonSynced, "toolset registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(mcpToolsetKind, ts.Name)
	metrics.ReconcileTotal.WithLabelValues(mcpToolsetKind, "success").Inc()
	logger.V(1).Info("toolset reconciled", "toolsetID", newToolsetID, "hash", currentRenderedHash)

	// No RequeueAfter: the SafetyRelistRunnable owns the periodic tick.
	return ctrl.Result{}, nil
}

// updateRequestFor converts a rendered create body into the PUT body for an
// existing toolset. The id travels in the BODY on this endpoint, and Tools is
// carried through verbatim so an emptied list is sent as an explicit `[]`
// clear (ALWAYS-EMIT — an omitted field would keep the stale tool list).
func updateRequestFor(toolsetID string, body *litellm.MCPToolsetRequest) *litellm.MCPToolsetUpdateRequest {
	return &litellm.MCPToolsetUpdateRequest{
		ToolsetID:   toolsetID,
		ToolsetName: body.ToolsetName,
		Description: body.Description,
		Tools:       body.Tools,
	}
}

// resolveToolsetIDByName re-resolves a toolset's LiteLLM toolset_id from a
// metadata.name lookup via ListMCPToolsets + in-memory filter. Used by the
// finalizer path when status.lastRendered.ToolsetID is empty, and by the
// CREATE arm's 409 adoption branch. Returns "" if the entry is absent or the
// LIST call fails non-fatally.
func (r *MCPToolsetReconciler) resolveToolsetIDByName(ctx context.Context, llm *litellm.Client, name string, logger logr.Logger) string {
	entries, err := llm.ListMCPToolsets(ctx)
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
		logger.V(1).Info("name-resolve: ListMCPToolsets failed; treating as absent", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.ToolsetName == name {
			return e.ToolsetID
		}
	}
	return ""
}

// classifyMutationError handles §7.7 error classification for LiteLLM toolset
// mutation calls. See the shared classifyMutationError helper.
func (r *MCPToolsetReconciler) classifyMutationError(ctx context.Context, ts *litellmv1alpha1.LiteLLMMCPToolset, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	snap := r.Cache.Snapshot()
	return classifyMutationError(ctx, logger, err, opDesc, mcpToolsetKind,
		func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
			return r.writeStatus(c, ts, s, reason, msg)
		},
		r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact.
func (r *MCPToolsetReconciler) writeStatus(
	ctx context.Context,
	ts *litellmv1alpha1.LiteLLMMCPToolset,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	cond := buildReadyCondition(ts.Generation, status, reason, message)
	desiredLastRendered := ts.Status.LastRendered
	desiredObservedGen := ts.Generation

	var fresh litellmv1alpha1.LiteLLMMCPToolset
	err := writeStatusWithRetry(ctx, r.Client, ts, &fresh, func(f *litellmv1alpha1.LiteLLMMCPToolset) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = desiredObservedGen
		f.Status.LastRendered = desiredLastRendered
	})
	if err == nil {
		ts.Status = fresh.Status
		ts.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(mcpToolsetKind, ts.Namespace, reason)
	return err
}

// mcpServerToToolsets maps a LiteLLMMCPServer event to every toolset in the
// watched namespace.
//
// A toolset's rendered hash covers the RESOLVED server ids, so when a
// referenced MCPServer finishes its first reconcile and gains a serverID the
// dependent toolsets must re-render. Enqueuing ALL toolsets (rather than
// indexing spec.from[].server) is deliberate: toolset counts are small, the
// reconcile is a cheap no-op when the hash is unchanged, and an index would
// have to key on both CR names AND raw UUIDs to be correct.
func (r *MCPToolsetReconciler) mcpServerToToolsets(ctx context.Context, _ client.Object) []reconcile.Request {
	reqs, err := ListMCPToolsetRequests(ctx, r.Client, r.Namespace)
	if err != nil {
		r.Log.V(1).Info("mcpServerToToolsets: list failed", "error", err)
		return nil
	}
	return reqs
}

// SetupWithManager registers the MCPToolsetReconciler with controller-runtime.
//
// Watches:
//   - For(&LiteLLMMCPToolset{}) — primary watch.
//   - Watches(&LiteLLMConnection{}) — connection fan-in.
//   - Watches(&LiteLLMMCPServer{}) — re-render dependent toolsets when a
//     referenced server gains its serverID (A2A has no equivalent).
//
// No Secret watch — this CRD has no spec.secrets.
func (r *MCPToolsetReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error {
	if r.Churn == nil {
		r.Churn = newChurnGuard()
	}
	if r.RecreateLimit <= 0 {
		r.RecreateLimit = DefaultRecreateLimitPerMin
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMMCPToolset{}, builder.WithPredicates()).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToMCPToolsets),
			builder.WithPredicates(connectionReadyTransition()),
		).
		Watches(
			&litellmv1alpha1.LiteLLMMCPServer{},
			handler.EnqueueRequestsFromMapFunc(r.mcpServerToToolsets),
		).
		WithOptions(transientBackoffOptions()).
		Named("mcptoolset")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToMCPToolsets); src != nil {
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

// ListMCPToolsetRequests lists every LiteLLMMCPToolset in namespace and
// returns their reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests —
// see ListModelRequests for the shared contract.
func ListMCPToolsetRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMMCPToolsetList
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
