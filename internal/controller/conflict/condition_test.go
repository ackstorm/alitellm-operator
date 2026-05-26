// SPDX-License-Identifier: Apache-2.0

package conflict_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ackstorm/alitellm-operator/internal/controller/conflict"
)

func TestNewLoserCondition_SetsReasonAndMessage(t *testing.T) {
	cond := conflict.NewLoserCondition(7, "ns-b/winner")
	if cond.Type != "Ready" {
		t.Fatalf("Type=%q, want Ready", cond.Type)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Status=%v, want False", cond.Status)
	}
	if cond.Reason != "Conflict" {
		t.Fatalf("Reason=%q, want Conflict", cond.Reason)
	}
	if cond.Message != "superseded by ns-b/winner" {
		t.Fatalf("Message=%q, want superseded by ns-b/winner", cond.Message)
	}
	if cond.ObservedGeneration != 7 {
		t.Fatalf("ObservedGeneration=%d, want 7", cond.ObservedGeneration)
	}
}

func TestApplyLoserCondition_IdempotentWriteAndClear(t *testing.T) {
	var conds []metav1.Condition
	conflict.ApplyLoserCondition(&conds, 1, "ns/winner")
	if c := meta.FindStatusCondition(conds, "Ready"); c == nil || c.Reason != conflict.ConditionReasonConflict {
		t.Fatalf("expected Conflict condition set")
	}
	conflict.ClearLoserCondition(&conds)
	if c := meta.FindStatusCondition(conds, "Ready"); c != nil && c.Reason == conflict.ConditionReasonConflict {
		t.Fatalf("expected Conflict condition cleared")
	}
}
