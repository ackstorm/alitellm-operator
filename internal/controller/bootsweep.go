// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// BootSweeper is a one-shot manager.Runnable that, on operator startup,
// enqueues every project CR whose `status.observedGeneration` matches
// `metadata.generation` but `Ready != True`. FIX2.txt HIGH-2
// (2026-05-22) — heals the "upgrade fixes things but status sticky"
// scenario where a CR landed Ready=False under v0.1.2 and the rate-
// limited workqueue dropped the retry item, so even though v0.1.3 fixes
// the upstream cause, an external poke (kubectl annotate, restart-2, …)
// was required to re-enqueue.
//
// Each project controller owns its own kind-specific channel; the
// sweeper writes typed events into the right channel based on the CR
// kind. Each controller subscribes via source.Channel in its
// SetupWithManager — so the GenericEvent enqueues a Reconcile.Request
// onto only the matching controller's queue.
//
// Behavior:
//   - Waits 2s after Start so the manager cache hydrates before listing.
//   - Lists cluster-scoped per kind; per-namespace filtering relies on
//     each controller's own cache-namespace gating.
//   - Best-effort: list errors and per-kind failures are logged at V(1)
//     and skipped; the sweeper never panics or returns an error.
type BootSweeper struct {
	Client client.Client

	// Per-kind event channels. Each project controller's
	// SetupWithManager subscribes its own channel via source.Channel
	// with EnqueueRequestForObject{}. Buffer size 256 absorbs the
	// initial burst without blocking the sweeper.
	TeamEvents               chan event.GenericEvent
	ModelEvents              chan event.GenericEvent
	A2AAgentEvents           chan event.GenericEvent
	MCPServerEvents          chan event.GenericEvent
	ModelDiscoveryEvents     chan event.GenericEvent
	MCPServerDiscoveryEvents chan event.GenericEvent
	GuardRailEvents          chan event.GenericEvent
	MCPToolsetEvents         chan event.GenericEvent
	AccessGroupEvents        chan event.GenericEvent
}

// NewBootSweeper constructs a BootSweeper with all per-kind channels
// pre-sized for one-shot startup bursts.
func NewBootSweeper(c client.Client) *BootSweeper {
	mkChan := func() chan event.GenericEvent {
		return make(chan event.GenericEvent, 256)
	}
	return &BootSweeper{
		Client:                   c,
		TeamEvents:               mkChan(),
		ModelEvents:              mkChan(),
		A2AAgentEvents:           mkChan(),
		MCPServerEvents:          mkChan(),
		ModelDiscoveryEvents:     mkChan(),
		MCPServerDiscoveryEvents: mkChan(),
		GuardRailEvents:          mkChan(),
		MCPToolsetEvents:         mkChan(),
		AccessGroupEvents:        mkChan(),
	}
}

// Start satisfies controller-runtime's manager.Runnable interface.
func (b *BootSweeper) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("bootsweep")

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(2 * time.Second):
	}

	type kindSlot struct {
		listObj client.ObjectList
		ch      chan event.GenericEvent
	}
	slots := []kindSlot{
		{&litellmv1alpha1.LiteLLMTeamList{}, b.TeamEvents},
		{&litellmv1alpha1.LiteLLMModelList{}, b.ModelEvents},
		{&litellmv1alpha1.LiteLLMA2AAgentList{}, b.A2AAgentEvents},
		{&litellmv1alpha1.LiteLLMMCPServerList{}, b.MCPServerEvents},
		{&litellmv1alpha1.LiteLLMModelDiscoveryList{}, b.ModelDiscoveryEvents},
		{&litellmv1alpha1.LiteLLMMCPServerDiscoveryList{}, b.MCPServerDiscoveryEvents},
		{&litellmv1alpha1.LiteLLMGuardRailList{}, b.GuardRailEvents},
		{&litellmv1alpha1.LiteLLMMCPToolsetList{}, b.MCPToolsetEvents},
		{&litellmv1alpha1.LiteLLMAccessGroupList{}, b.AccessGroupEvents},
	}

	total, enqueued := 0, 0
	for _, slot := range slots {
		if err := b.Client.List(ctx, slot.listObj); err != nil {
			logger.V(1).Info("bootsweep: list failed; skipping kind",
				"err", err.Error())
			continue
		}
		if err := apimeta.EachListItem(slot.listObj, func(o runtime.Object) error {
			obj, ok := o.(client.Object)
			if !ok {
				return nil
			}
			total++
			if !isStuckReadyFalse(obj) {
				return nil
			}
			select {
			case slot.ch <- event.GenericEvent{Object: obj}:
				enqueued++
			default:
				logger.V(1).Info("bootsweep: channel full; dropping",
					"kind", obj.GetObjectKind().GroupVersionKind().Kind,
					"namespace", obj.GetNamespace(),
					"name", obj.GetName())
			}
			return nil
		}); err != nil {
			logger.V(1).Info("bootsweep: EachListItem failed; partial sweep",
				"err", err.Error())
			continue
		}
	}
	logger.Info("bootsweep: completed", "total", total, "enqueued", enqueued)
	return nil
}

// BootEventsSource returns a controller-runtime source that drains a
// BootSweeper's per-kind channel and enqueues a Reconcile.Request for
// each received GenericEvent. Use from each controller's
// SetupWithManager: b.WatchesRawSource(BootEventsSource(events)).
//
// Pass nil to skip wiring (BootSweeper feature gate).
func BootEventsSource(ch <-chan event.GenericEvent) source.Source {
	if ch == nil {
		return nil
	}
	return source.TypedFunc[reconcile.Request](
		func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case ev, ok := <-ch:
						if !ok {
							return
						}
						if ev.Object == nil {
							continue
						}
						q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ev.Object)})
					}
				}
			}()
			return nil
		},
	)
}

// isStuckReadyFalse returns true iff observedGeneration matches
// metadata.generation AND the Ready condition is False or absent.
func isStuckReadyFalse(obj client.Object) bool {
	var conds []metav1.Condition
	var observedGen int64
	switch t := obj.(type) {
	case *litellmv1alpha1.LiteLLMTeam:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMModel:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMA2AAgent:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMMCPServer:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMModelDiscovery:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMMCPServerDiscovery:
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	case *litellmv1alpha1.LiteLLMGuardRail:
		// Missing case caused the boot-time stuck-Ready=False bug on
		// operator restart: BootSweeper.Start enumerates GuardRailList
		// (above) but isStuckReadyFalse returned false via the default
		// arm, so no GuardRail ever got re-enqueued. Combined with the
		// connectionReadyTransition predicate firing on the initial-list
		// Create event BEFORE the Connection reconciler's first probe
		// populated the cache, the very first reconcile wrote
		// Ready=False/LiteLLMUnavailable and nothing nudged it again
		// until the next Spec edit.
		conds, observedGen = t.Status.Conditions, t.Status.ObservedGeneration
	default:
		return false
	}
	if observedGen != obj.GetGeneration() {
		return false
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return false
	}
	ready := apimeta.FindStatusCondition(conds, conditionTypeReady)
	if ready == nil {
		return true
	}
	return ready.Status != metav1.ConditionTrue
}
