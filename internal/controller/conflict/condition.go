// SPDX-License-Identifier: Apache-2.0

package conflict

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReasonConflict is the Reason value written on the Ready
// condition of a loser CR. Stable string — used by users, dashboards,
// and CI assertions. Do not rename without a CHANGELOG entry.
const ConditionReasonConflict = "Conflict"

// NewLoserCondition returns the canonical Ready=False/Conflict condition
// payload for a CR that was superseded by another CR sharing the same
// natural key.
func NewLoserCondition(generation int64, winnerKey string) metav1.Condition {
	return metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             ConditionReasonConflict,
		Message:            "superseded by " + winnerKey,
		ObservedGeneration: generation,
	}
}

// ApplyLoserCondition sets the Ready=False/Conflict condition on the
// given conditions slice. Existing transition timestamps are preserved
// when the condition payload is unchanged.
func ApplyLoserCondition(conds *[]metav1.Condition, generation int64, winnerKey string) {
	meta.SetStatusCondition(conds, NewLoserCondition(generation, winnerKey))
}

// ClearLoserCondition removes the Ready=False/Conflict condition from
// the slice. Other Ready conditions (Ready=True, Ready=False with a
// different Reason) are left untouched — the caller is expected to set
// the correct post-recovery condition itself.
func ClearLoserCondition(conds *[]metav1.Condition) {
	c := meta.FindStatusCondition(*conds, "Ready")
	if c != nil && c.Reason == ConditionReasonConflict {
		meta.RemoveStatusCondition(conds, "Ready")
	}
}
