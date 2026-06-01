// SPDX-License-Identifier: Apache-2.0

// bootsweep_test.go — FIX2.txt HIGH-2 regression guards on the
// isStuckReadyFalse classifier. The full Start() flow is exercised by
// envtest manager wiring (each project controller subscribes to its
// kind-specific channel and the bootsweep reconciles every stuck CR on
// startup). The classifier itself is a pure function — testing it in
// isolation locks the contract that drives the sweep decision so future
// refactors of the Ready-condition shape are caught here first.

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// readyCond returns a Ready condition with the given status — convenience
// constructor for the table cases below.
func readyCond(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: conditionTypeReady, Status: status, Reason: reason}
}

func TestIsStuckReadyFalse(t *testing.T) {
	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "ready_true_generation_observed_not_stuck",
			obj: &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.ModelStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionTrue, "Synced")},
				},
			},
			want: false,
		},
		{
			name: "ready_false_observed_matches_is_stuck",
			obj: &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Status: litellmv1alpha1.ModelStatus{
					ObservedGeneration: 3,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "LiteLLMRejected")},
				},
			},
			want: true,
		},
		{
			name: "ready_absent_observed_matches_is_stuck",
			obj: &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status:     litellmv1alpha1.ModelStatus{ObservedGeneration: 2},
			},
			want: true,
		},
		{
			name: "observedGen_mismatch_not_stuck",
			obj: &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
				Status: litellmv1alpha1.ModelStatus{
					ObservedGeneration: 4,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "x")},
				},
			},
			want: false,
		},
		{
			name: "deletion_timestamp_set_not_stuck",
			obj: &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{
					Generation:        1,
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Status: litellmv1alpha1.ModelStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "x")},
				},
			},
			want: false,
		},
		{
			name: "team_kind_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMTeam{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.TeamStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "y")},
				},
			},
			want: true,
		},
		{
			name: "a2a_kind_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMA2AAgent{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.A2AAgentStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "y")},
				},
			},
			want: true,
		},
		{
			name: "mcp_kind_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMMCPServer{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.MCPServerStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "y")},
				},
			},
			want: true,
		},
		{
			name: "modeldiscovery_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMModelDiscovery{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.ModelDiscoveryStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "y")},
				},
			},
			want: true,
		},
		{
			name: "mcpserverdiscovery_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMMCPServerDiscovery{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.MCPServerDiscoveryStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "y")},
				},
			},
			want: true,
		},
		{
			name: "guardrail_ready_false_is_stuck",
			obj: &litellmv1alpha1.LiteLLMGuardRail{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: litellmv1alpha1.GuardRailStatus{
					ObservedGeneration: 1,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionFalse, "LiteLLMUnavailable")},
				},
			},
			want: true,
		},
		{
			name: "guardrail_ready_true_not_stuck",
			obj: &litellmv1alpha1.LiteLLMGuardRail{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status: litellmv1alpha1.GuardRailStatus{
					ObservedGeneration: 2,
					Conditions:         []metav1.Condition{readyCond(metav1.ConditionTrue, "Synced")},
				},
			},
			want: false,
		},
		{
			name: "unknown_kind_not_stuck",
			obj:  &litellmv1alpha1.LiteLLMConnection{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStuckReadyFalse(tc.obj); got != tc.want {
				t.Errorf("isStuckReadyFalse: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewBootSweeper_AllChannelsAllocated locks the constructor's
// per-kind channel allocation contract — the manager-wiring code in
// cmd/main.go reads each channel by name and a nil read would
// silently no-op the sweep, defeating FIX2 HIGH-2.
func TestNewBootSweeper_AllChannelsAllocated(t *testing.T) {
	b := NewBootSweeper(nil)
	chans := map[string]chan struct{}{}
	_ = chans
	if b.TeamEvents == nil ||
		b.ModelEvents == nil ||
		b.A2AAgentEvents == nil ||
		b.MCPServerEvents == nil ||
		b.ModelDiscoveryEvents == nil ||
		b.MCPServerDiscoveryEvents == nil {
		t.Fatalf("NewBootSweeper: some per-kind channels are nil: %+v", b)
	}
}
