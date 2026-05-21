// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmodels/finalizers,verbs=update

// FakeConnectionCache is a Phase-1 stub for the LiteLLMConnection cache
// that Phase 2 will land. Today it carries a single atomic.Bool flipped
// when the §7.7 401 fast-path fires. Tests assert on Invalidated.Load.
type FakeConnectionCache struct {
	Invalidated atomic.Bool
}

// NewFakeConnectionCache returns a ready-to-use cache stub. Reset semantics
// are intentionally simple: tests call Invalidated.Store(false) directly
// when they want to observe a transition.
func NewFakeConnectionCache() *FakeConnectionCache {
	return &FakeConnectionCache{}
}

// Snapshot is the D-12 shim added by. FakeConnectionCache must
// satisfy connection.ConnectionCache so the same interface field can hold
// either the real *connection.Cache or this Phase 1 stub.
//
// Derived semantics: Invalidated=true ⇒ BadMasterKey (a 401 was observed);
// Invalidated=false ⇒ Synced. The Phase 1 envtests assert on
// Invalidated.Load directly — they NEVER call Snapshot — so the
// derived values here only need to satisfy compile-time interface
// satisfaction. The 4 envtests (fastpath_test.go, idempotency_test.go,
// idempotency_long_test.go, metrics_scrape_test.go) keep working without
// modification.
func (f *FakeConnectionCache) Snapshot() connection.ConnectionSnapshot {
	if f.Invalidated.Load() {
		return connection.ConnectionSnapshot{Ready: false, Reason: "BadMasterKey"}
	}
	return connection.ConnectionSnapshot{Ready: true, Reason: reasonSynced}
}

// InvalidateOn401 is the D-12 shim added by. The existing
// NoOpReconciler.Reconcile path calls r.Cache.Invalidated.Store(true)
// directly (line 107 of this file); this method is added solely to
// satisfy the connection.ConnectionCache interface. No Phase 1 envtest
// calls it.
func (f *FakeConnectionCache) InvalidateOn401() {
	f.Invalidated.Store(true)
}

// Rebuild is the D-12 shim added by (CR-01 close). The
// connection.ConnectionCache interface now declares Rebuild alongside
// Snapshot and InvalidateOn401 so the LiteLLMConnection reconciler can
// route every probe-outcome write through the interface — eliminating
// the six runtime type assertions that previously panicked when a
// *FakeConnectionCache was substituted for *connection.Cache.
//
// The body is intentionally a no-op: the Phase 1 envtests
// (fastpath_test.go, idempotency_test.go, idempotency_long_test.go,
// metrics_scrape_test.go) never call Rebuild — they only assert on
// Invalidated.Load. Phase 3+ tests that need a meaningful fake (e.g.
// that observe stored snapshots) can extend this struct or add their
// own implementation.
func (f *FakeConnectionCache) Rebuild(_ connection.ConnectionSnapshot) {}

// NoOpReconciler is the Phase-1 smoke target. It increments a call
// counter, performs one LiteLLM.ProbeConnection per reconcile to exercise
// the §7.7 401 fast-path, and otherwise returns ctrl.Result{}, nil.
//
// REL-02 enforcement: this reconciler MUST NOT use RequeueAfter anywhere.
// Transient errors return ctrl.Result{}, err so controller-runtime's
// default workqueue rate limiter (ItemExponentialFailureRateLimiter +
// BucketRateLimiter) handles backoff. The 401 fast-path returns nil (NOT
// err) to break the auth-loop after master-key rotation — anti-storm rule.
type NoOpReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// LiteLLM is the per-instance REST client. The probe
	// path is /models per the spike pivot.
	LiteLLM *litellm.Client

	// Cache is the Phase-1 stub for the §7.7 connection cache. Phase 2
	// swaps this for the real LiteLLMConnection.Status cache.
	Cache *FakeConnectionCache

	// ReconcileCalls is incremented every time Reconcile fires. Tests
	// observe this counter to assert AC-N4 (zero calls for non-watched
	// CRs) and AC-R1 smoke (steady-state count).
	ReconcileCalls atomic.Int64

	// SafetyRelistInterval is the §7.6 safety re-list cadence. Production
	// uses 30 minutes; Phase 1's accelerated AC-R1 smoke sets it to 1s
	// to compress the test window. Currently unused by the reconcile
	// loop (controller-runtime's default workqueue handles requeue on
	// returned errors); kept as a field so Phase 7's elevated test can
	// flip it without changing the reconciler API.
	SafetyRelistInterval time.Duration

	// Log is the per-reconciler logger.
	Log logr.Logger
}

// Reconcile is the no-op loop. Steps:
// 1. Increment the call counter (test observability).
// 2. Probe LiteLLM (GET /models — keyinfo.go).
// 3. If 401: invalidate cache, return nil (REL-06 anti-storm).
// 4. If transient error: return err for controller-runtime's exponential
// backoff (REL-02 default rate limiter; NO RequeueAfter).
// 5. Else: return ctrl.Result{}, nil — no requeue, event-driven only.
func (r *NoOpReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.ReconcileCalls.Add(1)
	logger := log.FromContext(ctx).WithValues("noop-reconciler", req.NamespacedName)

	err := r.LiteLLM.ProbeConnection(ctx)
	if err != nil {
		// REL-06 §7.7 fast-path: if the error is the typed *Auth401Error,
		// take the anti-storm exit — invalidate the cache stub, log at
		// debug, and return ctrl.Result{}, nil (NOT err).
		var auth401 *litellm.Auth401Error
		if errors.As(err, &auth401) {
			r.Cache.Invalidated.Store(true)
			// REL-06 anti-storm: return nil, not err — controller-runtime
			// would otherwise enqueue the request via the rate limiter
			// for retry, and across many CRs the 401 storm would amplify
			// any backend outage caused by a key rotation. The fast-path
			// is the recovery: invalidate cache, drop the work item,
			// wait for the next event (e.g. operator re-bootstrap after
			// secret reload).
			logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path)
			return ctrl.Result{}, nil
		}
		// Non-401: return the error and let controller-runtime's default
		// workqueue rate limiter handle the backoff. REL-02 invariant:
		// NO RequeueAfter anywhere.
		logger.V(1).Info("probe failed; returning to workqueue for backoff", "error", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the no-op reconciler against the manager's
// Model informer. Uses controller-runtime's default workqueue rate limiter
// — no WithOptions(controller.Options{.}) override (REL-02 / Pattern 2).
func (r *NoOpReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMModel{}, builder.WithPredicates()).
		Named("noop").
		Complete(r)
}

// Compile-time assertion: *FakeConnectionCache satisfies the
// connection.ConnectionCache interface (D-12 shim —). The 4
// Phase 1 envtests bind to FakeConnectionCache via NoOpReconciler.Cache
// (the concrete-type field); Phase 2's LiteLLMConnectionReconciler.Cache
// is typed as connection.ConnectionCache (the interface), and any
// downstream code that pivots to the interface field type can plug this
// fake in unchanged.
var _ connection.ConnectionCache = (*FakeConnectionCache)(nil)
