// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"sort"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// ModelAliasAggregate is the immutable output of AggregateModelAliases.
//
// Desired       — the map the operator wants written into
//
//	router_settings.model_group_alias.
//
// Winners       — alias name → "<namespace>/<name>#<entry-index>" of the
//
//	(CR, entry) that won the alphabetical-last-wins tie-break
//	and owns the slot.
//
// LosersByAlias — alias name → ordered list of
//
//	"<namespace>/<name>#<entry-index>" identifiers for
//	(CR, entry) tuples overwritten by the winner. The list is
//	in the order entries were overwritten (NOT sorted) —
//	caller may sort if it needs determinism for output.
type ModelAliasAggregate struct {
	Desired       map[string]string
	Winners       map[string]string
	LosersByAlias map[string][]string
}

// WinnerOf returns the winner identifier for alias.
// Returns "" if alias has no entry.
func (a ModelAliasAggregate) WinnerOf(alias string) string {
	return a.Winners[alias]
}

// LosersOf returns the ordered list of loser identifiers for alias.
// Empty slice if no conflict.
func (a ModelAliasAggregate) LosersOf(alias string) []string {
	return a.LosersByAlias[alias]
}

// ResolveCR produces one AliasEntryStatus per spec.aliases entry in the
// given CR (in declared array order). For each entry:
//
//   - Applied=true and AppliedValue=entry.Value iff (cr, entry-index) won
//     the slot.
//   - Applied=false and ConflictsWith=<winner-id> iff some OTHER CR+entry
//     won the slot.
//
// Note: ConflictsWith is set even when the loser's value is identical to
// the winner's — exposing the ownership truth to operators.
func (a ModelAliasAggregate) ResolveCR(cr litellmv1alpha1.LiteLLMModelAlias) []litellmv1alpha1.AliasEntryStatus {
	out := make([]litellmv1alpha1.AliasEntryStatus, len(cr.Spec.Aliases))
	for i, e := range cr.Spec.Aliases {
		ownerID := entryID(cr, i)
		winnerID := a.Winners[e.Name]
		row := litellmv1alpha1.AliasEntryStatus{Name: e.Name}
		if winnerID == ownerID {
			row.Applied = true
			row.AppliedValue = e.Value
		} else {
			row.Applied = false
			row.ConflictsWith = winnerID
		}
		out[i] = row
	}
	return out
}

// AggregateModelAliases collates ALL LiteLLMModelAlias CRs into the merged
// desired map per MALIAS-02:
//
//  1. Sort items ASC by "<namespace>/<name>".
//  2. For each CR, iterate spec.aliases in declared array order.
//  3. Overwrite Desired[entry.Name] with entry.Value — last write wins.
//  4. Track each alias's winner and the ordered list of overwritten losers.
//
// The input slice is NOT mutated.
func AggregateModelAliases(items []litellmv1alpha1.LiteLLMModelAlias) ModelAliasAggregate {
	out := ModelAliasAggregate{
		Desired:       map[string]string{},
		Winners:       map[string]string{},
		LosersByAlias: map[string][]string{},
	}
	if len(items) == 0 {
		return out
	}
	sorted := make([]litellmv1alpha1.LiteLLMModelAlias, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return crKey(sorted[i]) < crKey(sorted[j])
	})
	for _, cr := range sorted {
		for i, e := range cr.Spec.Aliases {
			owner := entryID(cr, i)
			if prev, ok := out.Winners[e.Name]; ok {
				out.LosersByAlias[e.Name] = append(out.LosersByAlias[e.Name], prev)
			}
			out.Desired[e.Name] = e.Value
			out.Winners[e.Name] = owner
		}
	}
	return out
}

func crKey(m litellmv1alpha1.LiteLLMModelAlias) string {
	return m.Namespace + "/" + m.Name
}

func entryID(m litellmv1alpha1.LiteLLMModelAlias, idx int) string {
	return fmt.Sprintf("%s/%s#%d", m.Namespace, m.Name, idx)
}
