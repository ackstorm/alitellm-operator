// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildReadyCondition(t *testing.T) {
	c := buildReadyCondition(7, metav1.ConditionTrue, "Synced", "ok")
	if c.Type != conditionTypeReady {
		t.Errorf("Type = %q; want Ready", c.Type)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %q; want True", c.Status)
	}
	if c.Reason != "Synced" || c.Message != "ok" {
		t.Errorf("Reason/Message = %q/%q", c.Reason, c.Message)
	}
	if c.ObservedGeneration != 7 {
		t.Errorf("ObservedGeneration = %d; want 7", c.ObservedGeneration)
	}
	if c.LastTransitionTime.IsZero() {
		t.Error("LastTransitionTime not set")
	}
}
