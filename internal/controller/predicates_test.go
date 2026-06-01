// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func newConn(condStatus metav1.ConditionStatus) *litellmv1alpha1.LiteLLMConnection {
	return &litellmv1alpha1.LiteLLMConnection{
		Status: litellmv1alpha1.LiteLLMConnectionStatus{
			Conditions: []metav1.Condition{
				{Type: conditionTypeReady, Status: condStatus, Reason: "Synced"},
			},
		},
	}
}

// TestConnectionReadyTransition_FalseToTrueFires asserts the predicate
// fires only when Ready transitions False → True. Regression for FIX.txt
// M-3b: without the gate, dependent controllers would re-enqueue on every
// Connection probe-tick update.
func TestConnectionReadyTransition_FalseToTrueFires(t *testing.T) {
	pred := connectionReadyTransition()

	cases := []struct {
		name string
		old  *litellmv1alpha1.LiteLLMConnection
		new  *litellmv1alpha1.LiteLLMConnection
		want bool
	}{
		{"false → true: fires", newConn(metav1.ConditionFalse), newConn(metav1.ConditionTrue), true},
		{"true → true: suppressed (probe-tick noise)", newConn(metav1.ConditionTrue), newConn(metav1.ConditionTrue), false},
		{"true → false: suppressed (degradation, not recovery)", newConn(metav1.ConditionTrue), newConn(metav1.ConditionFalse), false},
		{"false → false: suppressed (still down)", newConn(metav1.ConditionFalse), newConn(metav1.ConditionFalse), false},
		{"unknown → true: fires (initial probe success)", newConn(metav1.ConditionUnknown), newConn(metav1.ConditionTrue), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("Update(%s): got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestConnectionReadyTransition_CreateOnReadyFires asserts a Create with a
// Connection that already shows Ready=True fans out (covers operator
// restart after a Synced cache).
func TestConnectionReadyTransition_CreateOnReadyFires(t *testing.T) {
	pred := connectionReadyTransition()
	if !pred.Create(event.CreateEvent{Object: newConn(metav1.ConditionTrue)}) {
		t.Error("Create on Ready=True did not fire")
	}
	if pred.Create(event.CreateEvent{Object: newConn(metav1.ConditionFalse)}) {
		t.Error("Create on Ready=False unexpectedly fired")
	}
}

// TestConnectionReadyTransition_DeleteSuppressed asserts Delete events
// are dropped (they don't represent recovery).
func TestConnectionReadyTransition_DeleteSuppressed(t *testing.T) {
	pred := connectionReadyTransition()
	if pred.Delete(event.DeleteEvent{Object: newConn(metav1.ConditionTrue)}) {
		t.Error("Delete unexpectedly fired")
	}
}

// TestConnectionReadyTransition_GenericFiresOnReady asserts that an
// externally-injected GenericEvent carrying a Ready=True Connection
// fans out — the contract added for FIX2.txt M-3 to let an upstream
// publisher (e.g. connection cache snapshot-flip) trigger child fan-in
// without requiring a status write on the Connection CR.
func TestConnectionReadyTransition_GenericFiresOnReady(t *testing.T) {
	pred := connectionReadyTransition()
	if !pred.Generic(event.GenericEvent{Object: newConn(metav1.ConditionTrue)}) {
		t.Error("Generic on Ready=True did not fire")
	}
	if pred.Generic(event.GenericEvent{Object: newConn(metav1.ConditionFalse)}) {
		t.Error("Generic on Ready=False unexpectedly fired")
	}
}
