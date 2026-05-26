// SPDX-License-Identifier: Apache-2.0

package conflict

import (
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Key returns the canonical sort key "<namespace>/<name>" used for
// every conflict tie-break in this operator.
func Key(o client.Object) string {
	return o.GetNamespace() + "/" + o.GetName()
}

// ResolveWinner sorts candidates by Key ASC and returns the LAST one.
// Nil input or empty slice yields nil. The input slice is sorted
// in place; callers that need the original order must copy first.
func ResolveWinner(candidates []client.Object) client.Object {
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return Key(candidates[i]) < Key(candidates[j])
	})
	return candidates[len(candidates)-1]
}

// IsLoser reports whether self is NOT the winner. A nil winner means
// there is no conflict and self is therefore not a loser.
func IsLoser(self client.Object, winner client.Object) bool {
	return winner != nil && Key(self) != Key(winner)
}
