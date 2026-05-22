// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// connectionFinalizer is the finalizer name managed by this reconciler
// per CONN-09 and spec §6.1: deleting a LiteLLMConnection CR invalidates
// the manager-level cache (Rebuild with Reason="Absent") and removes
// the finalizer with NO LiteLLM API call (no GET, no POST, no PUT,
// no DELETE — cache cleanup is purely in-process). The finalizer is
// added on the first reconcile of every CR and removed on
// DeletionTimestamp.
const connectionFinalizer = "litellm.ackstorm.ai/connection-cache-cleanup"

// Status-condition Reason constants — §6.0 + §10 set, shared across all
// CR-kind reconcilers. Single source of truth so goconst stays quiet.
const (
	reasonSynced             = "Synced"
	reasonConnecting         = "Connecting"
	reasonAbsent             = "Absent"
	reasonUnreachable        = "Unreachable"
	reasonBadMasterKey       = "BadMasterKey"
	reasonSecretNotFound     = "SecretNotFound"
	reasonLiteLLMUnavailable = "LiteLLMUnavailable"
)

// Event reason constants — recorded via record.EventRecorder.Eventf.
// Single source of truth so goconst stays quiet across reconcilers.
const (
	eventReasonProjectionOverride = "ProjectionOverride"
)

// connNotReadyUnreachableMsg is the human-readable substring assertion
// shared by the Pipeline A reconcilers' connection-gate tests
// (Model/MCPServer/A2AAgent/Team) when LiteLLMConnection probe fails
// with Unreachable. Single source of truth so goconst stays quiet.
const connNotReadyUnreachableMsg = "LiteLLMConnection/default not Ready (reason: Unreachable)"

// connectionReasonAll is the full §6.0 + §10 reason set for the
// metrics.ConnectionReady one-hot gauge. Reasons emitted by this
// reconciler are exactly {Synced, Connecting, Unreachable, BadMasterKey,
// SecretNotFound}; "Absent" is reserved for finalizer path
// and is NEVER set by writeStatus in this plan.
var connectionReasonAll = []string{
	reasonSynced, reasonConnecting, reasonAbsent, reasonUnreachable, reasonBadMasterKey, reasonSecretNotFound,
}

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmconnections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmconnections/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// LiteLLMConnectionReconciler reconciles the singleton
// LiteLLMConnection/default per spec §6.0 / §6.1 + 02-CONTEXT.md D-02.D-13.
//
// Behavior:
//
// - Step 1: fetch the CR (NotFound → return nil — deletion will have
// cleared the cache via the finalizer path below).
// - Step 2a: DeletionTimestamp set → invalidate cache (Reason "Absent"),
// remove finalizer, NO LiteLLM API call (CONN-09 / §6.1).
// - Step 2b: finalizer absent on a non-deleting CR → AddFinalizer + Update,
// re-trigger via the resulting resourceVersion bump.
// - Step 3: Connecting-on-entry write (D-07) when Ready is unset or
// stale-for-this-generation.
// - Step 4: resolve the masterKeySecretRef Secret (missing → SecretNotFound).
// - Step 5: build fresh *litellm.Client (D-03 — never pooled).
// - Step 6: probe via Client.ProbeConnection (GET /models per D-13);
// classify outcome:
// - 401 (*Auth401Error) → Ready=False, BadMasterKey, return nil (anti-storm).
// - transient (non-401) → Ready=False, Unreachable, return err
// (controller-runtime's ItemExponentialFailureRateLimiter retries).
// - success → Ready=True, Synced, return RequeueAfter 5m
// (D-05 — the ONE permitted RequeueAfter in production code).
//
// The cache (connection.ConnectionCache — interface, not concrete type
// per D-12) is Rebuilt on every probe outcome.
type LiteLLMConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface — wires *connection.Cache; tests
	// wire either *connection.Cache or *FakeConnectionCache. Never use
	// the concrete type here (D-12 — Phase 1 test compatibility).
	Cache connection.ConnectionCache
	// Namespace is the WATCH_NAMESPACE the reconciler is scoped to
	// (CR-03 — must match cmd/main.go's watchNS and the manager's
	// cache.Options.DefaultNamespaces). The 401 fast-path channel
	// handler enqueues `<Namespace>/default` via the fastPathRequest
	// helper. Empty string means the test harness forgot to set it —
	// the fast-path enqueue produces an unresolvable NamespacedName
	// (`/default`) which the informer cache cannot match.
	Namespace string
	Log       logr.Logger
}

// fastPathRequest returns the reconcile.Request the 401 fast-path
// channel handler enqueues. It is split out as a helper so unit tests
// can lock the namespace-routing logic against the original CR-03
// defect (a hardcoded "default" constant) without standing up an
// envtest manager bound to a non-default WATCH_NAMESPACE.
//
// Per CONN-02 the CR is a CEL-enforced singleton named "default";
// per CR-03 the namespace comes from r.Namespace, sourced from
// cmd/main.go's watchNS (WATCH_NAMESPACE env). Tests construct a
// reconciler with Namespace="<non-default-ns>" and assert the
// returned request's Namespace matches — proving the helper
// honors the field instead of hardcoding "default".
func (r *LiteLLMConnectionReconciler) fastPathRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: r.Namespace,
		Name:      "default",
	}}
}

// Reconcile is the §6.0 LiteLLMConnection state machine.
//
//nolint:gocyclo // The state machine is intentionally linear; splitting into helpers would obscure the §6.0 / §7.6 / §7.7 mapping.
func (r *LiteLLMConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("litellmconnection", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────
	var conn litellmv1alpha1.LiteLLMConnection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			// CR deleted — finalizer cleanup will have
			// already invalidated the cache. Nothing for the reconciler
			// to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path (CONN-09) ──────────────────────────
	// When the CR has a DeletionTimestamp set, the finalizer's only job
	// is to invalidate the manager-level cache (Rebuild with Reason
	// "Absent" — the §6.0 / §10 reason reserved for the deletion path)
	// and remove the finalizer. There is NO LiteLLM API call on delete
	// per spec §6.1 — the operator does not own the upstream master key,
	// it only consumes it; deleting the CR signals "operator no longer
	// tracks this endpoint", nothing more.
	if !conn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&conn, connectionFinalizer) {
			// Cache invalidation — cache-only, never an API call.
			r.Cache.Rebuild(connection.ConnectionSnapshot{
				Ready:      false,
				Reason:     "Absent",
				Generation: conn.Generation,
			})
			// §10 one-hot gauge: clear all six labels, set "Absent" to 1.
			for _, rk := range connectionReasonAll {
				metrics.ConnectionReady.WithLabelValues(rk).Set(0)
			}
			metrics.ConnectionReady.WithLabelValues("Absent").Set(1)

			// OBS-03: drop the cr_status_age_seconds label before the CR is gone (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget("LiteLLMConnection", conn.Name)
			controllerutil.RemoveFinalizer(&conn, connectionFinalizer)
			if err := r.Update(ctx, &conn); err != nil {
				return ctrl.Result{}, err
			}
			// §9.1: log carries no master key, no body content.
			logger.Info("finalizer removed; cache invalidated; no LiteLLM API call")
		}
		// Finalizer either was not present (already finalized) or has
		// just been removed — either way, nothing else to do.
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path (CONN-09) ─────────────────────
	// Add the finalizer on the first reconcile of every CR. The Update
	// bumps resourceVersion and controller-runtime re-triggers Reconcile
	// — this keeps the finalizer Update from interleaving with the
	// later Status.Update in the same reconcile call (Update + Status
	// update on the same object in one Reconcile is technically allowed
	// but error-prone; splitting into two reconciles is the idiomatic
	// pattern).
	if !controllerutil.ContainsFinalizer(&conn, connectionFinalizer) {
		controllerutil.AddFinalizer(&conn, connectionFinalizer)
		if err := r.Update(ctx, &conn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connecting-on-entry write (D-07 + WR-01) ──────────
	// Write Ready=False, reason=Connecting BEFORE the Secret resolution
	// AND BEFORE the probe IFF:
	// - no Ready condition exists yet (first reconcile of this CR), OR
	// - the CR's generation changed since the last terminal write
	// (status.observedGeneration mismatch) — REGARDLESS of whether
	// the previous Ready was True (Synced) or False (terminal).
	//
	// WR-01 broadening: the original guard only fired when the previous
	// Ready was False. A generation-change reconcile that went straight
	// from Synced to a terminal reason (e.g., a previously-good CR whose
	// new spec points at a missing Secret) would skip Connecting and
	// dependents reading cache.Snapshot in the one-reconcile window
	// observed stale Synced from the PREVIOUS probe. Broadening covers
	// that path — dependents now observe Connecting instead of stale
	// Synced before SecretNotFound or BadMasterKey lands.
	//
	// The inner guard (curReady.Reason != reasonConnecting) preserves the
	// no-op-patch optimization for repeated Connecting writes — avoids
	// resourceVersion churn that would trigger another reconcile and
	// loop the watch chain.
	curReady := apimeta.FindStatusCondition(conn.Status.Conditions, "Ready")
	if curReady == nil || conn.Status.ObservedGeneration != conn.Generation {
		if curReady == nil || curReady.Reason != reasonConnecting {
			// WR-03: status-write errors are captured and logged at error
			// level so apierrors.IsConflict storms are observable. The
			// write is non-fatal (continue to probe so the terminal-reason
			// write below can land); the logger.Error makes the failure
			// visible without blocking forward progress.
			if err := r.writeStatus(ctx, &conn, metav1.ConditionFalse, reasonConnecting, "probing endpoint"); err != nil {
				// Non-fatal: the terminal-reason write below will set the final Ready condition.
				logStatusUpdateErr(logger, err, "reason", reasonConnecting)
			}
		}
	}

	// ─── Step 4: Resolve the masterKeySecretRef Secret ─────────────
	var secret corev1.Secret
	secretKey := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      conn.Spec.MasterKeySecretRef.Name,
	}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			msg := "Secret " + req.Namespace + "/" + conn.Spec.MasterKeySecretRef.Name + " not found"
			// WR-03: capture-and-log so apierrors.IsConflict storms are observable.
			if werr := r.writeStatus(ctx, &conn, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
			}
			r.Cache.Rebuild(connection.ConnectionSnapshot{
				Ready:      false,
				Reason:     reasonSecretNotFound,
				Generation: conn.Generation,
			})
			metrics.ReconcileTotal.WithLabelValues("LiteLLMConnection", "success").Inc()
			// SecretNotFound is operator-action-required (not transient),
			// so return nil — controller-runtime should NOT exponentially
			// retry. The Secret watch (secretToConnection) will re-enqueue
			// when the user creates the Secret.
			return ctrl.Result{}, nil
		}
		// Other GET errors are transient — wrap and return for backoff.
		return ctrl.Result{}, err
	}

	masterKeyValue, ok := secret.Data[conn.Spec.MasterKeySecretRef.Key]
	if !ok {
		// §9.1: the message includes the Secret coordinates
		// (<ns>/<name>:<key>) but NEVER the value.
		msg := "Secret " + req.Namespace + "/" + conn.Spec.MasterKeySecretRef.Name +
			":" + conn.Spec.MasterKeySecretRef.Key + " key not found"
		// WR-03: capture-and-log so apierrors.IsConflict storms are observable.
		if werr := r.writeStatus(ctx, &conn, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		r.Cache.Rebuild(connection.ConnectionSnapshot{
			Ready:      false,
			Reason:     reasonSecretNotFound,
			Generation: conn.Generation,
		})
		metrics.ReconcileTotal.WithLabelValues("LiteLLMConnection", "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 5: Build fresh *litellm.Client (D-03) ────────────────
	// New http.Client, new transport, new redacting RoundTripper on
	// EVERY probe. No transport pooling — operator traffic is too
	// low-rate to need warm connections, and the §9.1 redacting
	// invariant is preserved across rebuilds because newHTTPClient
	// (inside NewClient) wraps the transport every time.
	liteLLMClient := litellm.NewClient(conn.Spec.Endpoint, string(masterKeyValue), r.Log.WithName("probe"))

	// ─── Step 6: Probe + classify ──────────────────────────────────
	probeErr := liteLLMClient.ProbeConnection(ctx)

	// 401 — BadMasterKey (permanent classification per D-06).
	var auth401 *litellm.Auth401Error
	if errors.As(probeErr, &auth401) {
		// §9.1: include the path the 401 came from (auth401.Path) but
		// NEVER the response body field — it may contain key-hash
		// fragments. The grep canary verifies zero references to that
		// field name in this file.
		msg := "401 from " + auth401.Path
		// WR-03: capture-and-log so apierrors.IsConflict storms are observable.
		if werr := r.writeStatus(ctx, &conn, metav1.ConditionFalse, "BadMasterKey", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "BadMasterKey")
		}
		r.Cache.Rebuild(connection.ConnectionSnapshot{
			Ready:      false,
			Reason:     "BadMasterKey",
			Generation: conn.Generation,
		})
		// success label — the RECONCILER completed; the probe failure
		// is the deterministic outcome, not a reconcile failure.
		metrics.ReconcileTotal.WithLabelValues("LiteLLMConnection", "success").Inc()
		logger.Info("probe: BadMasterKey", "path", auth401.Path)
		// Anti-storm: return nil, NOT err (REL-06). 401 is operator-
		// action (rotate Secret); exponential backoff would amplify
		// the storm across dependent CRs.
		return ctrl.Result{}, nil
	}

	// Non-401 error — transient/Unreachable (D-06).
	if probeErr != nil {
		// probeErr.Error from internal/litellm/client.go is already
		// redacted at the wire layer (transport.go's redacting
		// RoundTripper + processLitellmError that strips bodies). Safe
		// to include in the message.
		msg := "probe failed: " + probeErr.Error()
		// WR-03: capture-and-log so apierrors.IsConflict storms are observable.
		if werr := r.writeStatus(ctx, &conn, metav1.ConditionFalse, "Unreachable", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "Unreachable")
		}
		r.Cache.Rebuild(connection.ConnectionSnapshot{
			Ready:      false,
			Reason:     "Unreachable",
			Generation: conn.Generation,
		})
		metrics.ReconcileTotal.WithLabelValues("LiteLLMConnection", "error").Inc()
		logger.V(1).Info("probe: Unreachable; returning err for controller-runtime backoff", "error", probeErr)
		// D-06: return err so the default ItemExponentialFailureRateLimiter
		// retries. NO RequeueAfter on this path (REL-02 grep gate stays at
		// 1 — exactly the success-path RequeueAfter below).
		return ctrl.Result{}, probeErr
	}

	// ─── Synced — success path ─────────────────────────────────────
	// WR-03: capture-and-log so apierrors.IsConflict storms are observable.
	if werr := r.writeStatus(ctx, &conn, metav1.ConditionTrue, reasonSynced, "probe ok"); werr != nil {
		logStatusUpdateErr(logger, werr, "reason", reasonSynced)
	}
	r.Cache.Rebuild(connection.ConnectionSnapshot{
		Ready:                  true,
		Reason:                 reasonSynced,
		Client:                 liteLLMClient,
		Generation:             conn.Generation,
		MCPToolPrefixSeparator: conn.Spec.MCPToolPrefixSeparator,
	})
	metrics.ReconcileTotal.WithLabelValues("LiteLLMConnection", "success").Inc()
	// OBS-03: RecordSuccess tracks time of last successful status write;
	// Collect emits time.Since(lastSuccess).Seconds on every scrape.
	metrics.CRStatusAgeTracker.RecordSuccess("LiteLLMConnection", conn.Name)
	logger.V(1).Info("probe: Synced", "endpoint", conn.Spec.Endpoint)

	// D-05: the §7.6-permitted exception for periodic kinds. This is the
	// ONLY RequeueAfter in production code. REL-02 grep gate now allows
	// exactly 1 occurrence here.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// writeStatus is the single status-write helper. Builds the standard
// metav1.Condition shape (Type="Ready" + given Status/Reason/Message +
// ObservedGeneration), sets it via apimeta.SetStatusCondition (idempotent
// transitions), updates the one-hot metrics.ConnectionReady gauge, and
// patches the CR's status subresource.
//
// §9.1: the message parameter is the caller's responsibility — this
// helper does not redact. Callers MUST ensure no master key value
// reaches `message`.
//
// Reasons emitted by this method are exactly: Synced, Connecting,
// Unreachable, BadMasterKey, SecretNotFound. "Absent" is NEVER written
// here (reserved for finalizer path).
func (r *LiteLLMConnectionReconciler) writeStatus(
	ctx context.Context,
	conn *litellmv1alpha1.LiteLLMConnection,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Skip-when-equal: identical Ready condition + ObservedGeneration
	// already matches Generation → no-op write would only churn the
	// resourceVersion and feed the 409 storm the WR-03 gate observes.
	// The metric gauge is also a no-op on this path (same labels would
	// be re-set to the same values), so the guard sits ahead of it.
	if statusReadyUnchanged(conn.Status.Conditions, conn.Status.ObservedGeneration, conn.Generation, status, reason, message) {
		return nil
	}

	// §10: one-hot ConnectionReady gauge. Clear all six reasons to 0,
	// then set the active reason to 1.
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}
	metrics.ConnectionReady.WithLabelValues(reason).Set(1)

	orig := conn.DeepCopy()
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: conn.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&conn.Status.Conditions, cond)
	conn.Status.ObservedGeneration = conn.Generation

	// Patch (MergeFrom) instead of Update: status subresource merge patches
	// do not embed resourceVersion so concurrent reconciles do not collide
	// with HTTP 409. Conflict logs become genuinely rare instead of routine.
	return r.Status().Patch(ctx, conn, client.MergeFrom(orig))
}

// secretToConnection is the handler.MapFunc that maps a Secret update
// event back to the owning LiteLLMConnection CR (CONN-05 part (b) —
// Secret rotation triggers reconcile).
//
// The mapper lists all LiteLLMConnection CRs in the manager-cached scope
// (cache.Options.DefaultNamespaces is configured in cmd/main.go and the
// suite, so this list is already WATCH_NAMESPACE-scoped) and emits a
// reconcile.Request for every CR whose masterKeySecretRef.Name +
// namespace match the Secret. In v1alpha1's singleton model this is
// always at most one CR (the `default` singleton), but the mapper is
// written generally to tolerate future relaxation of the singleton rule.
func (r *LiteLLMConnectionReconciler) secretToConnection(ctx context.Context, obj client.Object) []reconcile.Request {
	var list litellmv1alpha1.LiteLLMConnectionList
	if err := r.List(ctx, &list); err != nil {
		// Best-effort — log via the reconciler's logger if available,
		// then return empty. controller-runtime will not retry the
		// mapper itself; the Secret event will be missed for this tick
		// but the next status read or watch event will recover.
		r.Log.V(1).Info("secretToConnection: list failed; skipping", "error", err)
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		cr := &list.Items[i]
		if cr.Spec.MasterKeySecretRef.Name == obj.GetName() && cr.Namespace == obj.GetNamespace() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		}
	}
	return out
}

// SetupWithManager registers the reconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMConnection{}) — primary watch.
// - Watches(&Secret{}, secretToConnection) — masterKeySecretRef
// rotation triggers reconcile (CONN-05 part (b)).
// - WatchesRawSource(source.Channel(ch, .)) — 401 fast-path channel
// (D-09). wires a real channel from cmd/main.go; Plan
// 02-02 Task 3's suite_test.go wires the cache's own channel. The
// handler enqueues a reconcile of `<watchNs>/default` (the singleton
// enforced by CEL).
//
// Named("litellmconnection") is the controller's name in the
// controller-runtime registry.
func (r *LiteLLMConnectionReconciler) SetupWithManager(mgr ctrl.Manager, ch <-chan event.GenericEvent) error {
	// Build a TypedFuncs[client.Object, reconcile.Request] handler for
	// the channel source. We only need the Generic case (events on the
	// channel carry empty event.GenericEvent values — InvalidateOn401 in
	// internal/connection/cache.go sends event.GenericEvent{}).
	channelHandler := handler.Funcs{
		GenericFunc: func(
			_ context.Context,
			_ event.TypedGenericEvent[client.Object],
			q workqueue.TypedRateLimitingInterface[reconcile.Request],
		) {
			// CR-03: route through r.fastPathRequest so the
			// namespace comes from the receiver's Namespace field
			// (set from WATCH_NAMESPACE in cmd/main.go) rather than
			// a hardcoded constant. The receiver `r` is in scope
			// inside the closure because SetupWithManager is a
			// method on *LiteLLMConnectionReconciler.
			q.Add(r.fastPathRequest())
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMConnection{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToConnection),
		).
		WatchesRawSource(source.Channel(ch, channelHandler)).
		Named("litellmconnection").
		Complete(r)
}
