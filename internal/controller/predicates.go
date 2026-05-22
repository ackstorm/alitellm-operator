// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// connectionReadyTransition fires on a LiteLLMConnection event ONLY when
// status.conditions[type=Ready] flips False → True (or appears True for
// the first time). Used by mcpserver / model / a2aagent / team
// controllers to re-enqueue all child CRs in the same namespace after an
// upstream LiteLLM restart recovers (FIX.txt M-3b, 2026-05-22).
//
// Why a transition predicate (not "any change"): without the False→True
// gate every Connection status write — including the noisy probe-tick
// updates that don't change Ready — would re-enqueue every CR in the
// namespace. The transition predicate caps fan-out to genuine recovery
// events.
func connectionReadyTransition() predicate.Predicate {
	return predicate.Funcs{
		// On Create: a Connection appearing already Ready is treated as a
		// transition (covers operator restart with a Synced cache).
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
		DeleteFunc:  func(_ event.DeleteEvent) bool { return false },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
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
		if cond.Type == "Ready" {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
