// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatusReadyUnchanged(t *testing.T) {
	readyTrue := []metav1.Condition{{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "Synced",
		Message: "ok",
	}}

	tests := []struct {
		name        string
		conds       []metav1.Condition
		observedGen int64
		gen         int64
		status      metav1.ConditionStatus
		reason      string
		message     string
		want        bool
	}{
		{
			name:        "exact match returns true",
			conds:       readyTrue,
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionTrue,
			reason:      "Synced",
			message:     "ok",
			want:        true,
		},
		{
			name:        "generation drift returns false",
			conds:       readyTrue,
			observedGen: 1,
			gen:         2,
			status:      metav1.ConditionTrue,
			reason:      "Synced",
			message:     "ok",
			want:        false,
		},
		{
			name:        "missing Ready condition returns false",
			conds:       nil,
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionTrue,
			reason:      "Synced",
			message:     "ok",
			want:        false,
		},
		{
			name:        "status flip returns false",
			conds:       readyTrue,
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionFalse,
			reason:      "Synced",
			message:     "ok",
			want:        false,
		},
		{
			name:        "reason diff returns false",
			conds:       readyTrue,
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionTrue,
			reason:      "Unreachable",
			message:     "ok",
			want:        false,
		},
		{
			name:        "message-only diff returns false",
			conds:       readyTrue,
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionTrue,
			reason:      "Synced",
			message:     "different",
			want:        false,
		},
		{
			name: "non-Ready condition only returns false",
			conds: []metav1.Condition{{
				Type:    "SourceReachable",
				Status:  metav1.ConditionTrue,
				Reason:  "Ok",
				Message: "",
			}},
			observedGen: 1,
			gen:         1,
			status:      metav1.ConditionTrue,
			reason:      "Synced",
			message:     "ok",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusReadyUnchanged(tt.conds, tt.observedGen, tt.gen, tt.status, tt.reason, tt.message)
			if got != tt.want {
				t.Fatalf("statusReadyUnchanged = %v, want %v", got, tt.want)
			}
		})
	}
}
