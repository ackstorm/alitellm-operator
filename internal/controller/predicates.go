// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// discoverySpecChanged is the predicate attached to Discovery primary
// For() watches. Filters status-subresource writes (no generation
// bump) so the reconciler doesn't re-enqueue itself.
//
// Fires on:
//   - Create / Delete / Generic — always (lifecycle signals).
//   - Update — only if generation, labels, or annotations changed.
//
// Why labels + annotations: tests use annotation nudges via
// touchMCPServerDiscovery to force a re-reconcile, ops uses
// annotations / labels for adoption + grouping signals, and the
// reconciler itself does NOT mutate either, so this widening cannot
// re-introduce the self-loop. The only writes the reconciler does on
// itself are status-subresource (filtered) and metadata.finalizers
// — finalizer adds are also metadata-only but the reconciler
// already paths them via explicit ctrl.Result{Requeue: true} after
// the AddFinalizer + Update (see mcpserverdiscovery_controller.go
// Step 2b note).
func discoverySpecChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			if !mapsEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) {
				return true
			}
			if !mapsEqual(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()) {
				return true
			}
			return false
		},
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || vb != va {
			return false
		}
	}
	return true
}

// connectionReadyTransition fires on a LiteLLMConnection event for any
// of three cases that together fully cover Connection-Ready recovery
// from the perspective of dependent child CRs:
//
//   - Create: the watched Connection arrives Ready=True. This is the
//     cold-start case (operator restart with a Synced cache, or a
//     fresh Connection created already Ready).
//   - Update: status.conditions[type=Ready] flips False → True. The
//     transient-recovery case (upstream LiteLLM restart, 401-rotation,
//     cache rebuild).
//   - Generic: an external publisher (currently reserved; the
//     connection cache may emit a GenericEvent on snapshot transition
//     in a future revision) signals that the Connection's effective
//     readiness changed. Treated identically to Create: fire iff the
//     object is Ready=True.
//
// Used by mcpserver / model / a2aagent / team controllers to
// re-enqueue all child CRs in the same namespace after an upstream
// LiteLLM restart recovers (FIX.txt M-3b, 2026-05-22; FIX2.txt M-3
// Option B, 2026-05-22).
//
// Why a transition predicate (not "any change"): without the False→True
// gate every Connection status write — including the noisy probe-tick
// updates that don't change Ready — would re-enqueue every CR in the
// namespace. The transition predicate caps fan-out to genuine recovery
// events.
func connectionReadyTransition() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			conn, ok := e.Object.(*litellmv1alpha1.LiteLLMConnection)
			return ok && isConnReady(conn)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldConn, ok1 := e.ObjectOld.(*litellmv1alpha1.LiteLLMConnection)
			newConn, ok2 := e.ObjectNew.(*litellmv1alpha1.LiteLLMConnection)
			if !ok1 || !ok2 {
				return false
			}
			return !isConnReady(oldConn) && isConnReady(newConn)
		},
		DeleteFunc: func(_ event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool {
			conn, ok := e.Object.(*litellmv1alpha1.LiteLLMConnection)
			return ok && isConnReady(conn)
		},
	}
}

// ownedChildSpecChanged is a predicate for Discovery controllers' Owns()
// watch on their child CRs. The Owns watch by default fires on EVERY
// child event — including the status-subresource writes the child
// reconciler emits on every successful pass + the managedFields-only
// bumps SSA Patch produces when re-applying an identical spec. Both
// are spurious from the Discovery's perspective: the Discovery only
// cares about child spec edits (generation bump) and child lifecycle
// (create/delete). Without this filter, post-FIX9 (v0.4.2) hash
// invalidation caused a feedback loop — child Step 11 status write
// fires parent watch, parent Step 8 SSA Patch fires child watch, ad
// infinitum (observed 232 reconciles/min on mcpserverdiscovery + 52
// on modeldiscovery in prod after v0.4.2 rolled out).
//
// Events kept:
//   - Create: always (new child landing — cascade/discovery integration).
//   - Delete: always (cascade-delete + vanish-detection upstream signal).
//   - Update: only when metadata.generation changes (i.e. user OR
//     SSA Patch wrote a spec field that diffs). Status-subresource
//     writes do NOT bump generation. ManagedFields-only changes do
//     NOT bump generation.
//
// Adoption hook trade-off: when a user kubectl-strips the controller
// ownerRef on a child, that's a metadata-only change without
// generation bump. The parent now learns about it on the next
// refresh-interval reconcile (5–10m depending on Discovery type)
// instead of in real time. Acceptable cost — adoption is a rare
// admin operation and the 5m latency does not affect correctness.
func ownedChildSpecChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
}

// isConnReady returns true iff the Connection's Ready condition is
// status=True. Empty conditions list, missing Ready type, or
// status="False"/"Unknown" all return false.
func isConnReady(c *litellmv1alpha1.LiteLLMConnection) bool {
	if c == nil {
		return false
	}
	for _, cond := range c.Status.Conditions {
		if cond.Type == conditionTypeReady {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
