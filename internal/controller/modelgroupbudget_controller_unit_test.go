// SPDX-License-Identifier: MIT

package controller

import (
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// TestModelGroupBudget_MatchAndNeedsClear — pure table test for the drift
// predicates: match → no PUT; needsClear → DELETE before PUT.
func TestModelGroupBudget_MatchAndNeedsClear(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	s := func(v string) *string { return &v }
	spec := func(limit, soft *float64, period string) *litellmv1alpha1.ModelGroupBudgetSpec {
		return &litellmv1alpha1.ModelGroupBudgetSpec{Limit: limit, SoftLimit: soft, Period: period}
	}
	got := func(limit, soft *float64, period *string) *litellm.ModelGroupBudget {
		return &litellm.ModelGroupBudget{MaxBudget: limit, SoftBudget: soft, BudgetDuration: period}
	}
	for _, tc := range []struct {
		name       string
		spec       *litellmv1alpha1.ModelGroupBudgetSpec
		got        *litellm.ModelGroupBudget
		match, clr bool
	}{
		{"all match", spec(f(10), f(5), "30d"), got(f(10), f(5), s("30d")), true, false},
		{"limit 0 vs 0.0", spec(f(0), nil, ""), got(f(0.0), nil, nil), true, false},
		{"period empty vs nil", spec(f(1), nil, ""), got(f(1), nil, nil), true, false},
		{"got nil budget", spec(f(1), nil, ""), nil, false, false},
		{"limit set in spec only", spec(f(1), nil, ""), got(nil, nil, nil), false, false},
		{"limit set in LiteLLM only", spec(nil, nil, ""), got(f(1), nil, nil), false, true},
		{"limit differs", spec(f(2), nil, ""), got(f(1), nil, nil), false, false},
		{"soft set in spec only", spec(f(1), f(5), ""), got(f(1), nil, nil), false, false},
		{"soft set in LiteLLM only", spec(f(1), nil, ""), got(f(1), f(5), nil), false, true},
		{"period set in spec only", spec(f(1), nil, "1d"), got(f(1), nil, nil), false, false},
		{"period set in LiteLLM only", spec(f(1), nil, ""), got(f(1), nil, s("1d")), false, true},
		{"period differs", spec(f(1), nil, "1d"), got(f(1), nil, s("7d")), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if m := budgetMatches(tc.spec, tc.got); m != tc.match {
				t.Errorf("budgetMatches = %v, want %v", m, tc.match)
			}
			if c := budgetNeedsClear(tc.spec, tc.got); c != tc.clr {
				t.Errorf("budgetNeedsClear = %v, want %v", c, tc.clr)
			}
		})
	}
}
