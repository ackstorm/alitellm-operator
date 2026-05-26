// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func TestAggregateModelAliases_MultiEntryLastWinsAlphabetical(t *testing.T) {
	items := []litellmv1alpha1.LiteLLMModelAlias{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a-first", Namespace: "ns1"},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{
					{Name: "shared", Value: "first"},
					{Name: "uniq-a", Value: "va"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "z-last", Namespace: "ns1"},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{
					{Name: "shared", Value: "last-wins"},
					{Name: "uniq-z", Value: "vz"},
				},
			},
		},
	}
	res := AggregateModelAliases(items)
	wantMap := map[string]string{
		"shared": "last-wins",
		"uniq-a": "va",
		"uniq-z": "vz",
	}
	if !reflect.DeepEqual(res.Desired, wantMap) {
		t.Fatalf("desired mismatch\nwant=%v\ngot=%v", wantMap, res.Desired)
	}
	if got := res.WinnerOf("shared"); got != "ns1/z-last#0" {
		t.Fatalf("winner of shared: want ns1/z-last#0 got %s", got)
	}
	if losers := res.LosersOf("shared"); !reflect.DeepEqual(losers, []string{"ns1/a-first#0"}) {
		t.Fatalf("losers of shared: want [ns1/a-first#0] got %v", losers)
	}
	if got := res.LosersOf("uniq-a"); len(got) != 0 {
		t.Fatalf("expected no losers for uniq-a, got %v", got)
	}
}

func TestAggregateModelAliases_NamespaceTieBreakIsAlphabetical(t *testing.T) {
	items := []litellmv1alpha1.LiteLLMModelAlias{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "ns-z"},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k", Value: "z-wins"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "ns-a"},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k", Value: "a-loses"}},
			},
		},
	}
	res := AggregateModelAliases(items)
	if res.Desired["k"] != "z-wins" {
		t.Fatalf("ns-z should win on alphabetical-last, got %q", res.Desired["k"])
	}
	if res.WinnerOf("k") != "ns-z/same#0" {
		t.Fatalf("winner: want ns-z/same#0 got %s", res.WinnerOf("k"))
	}
}

func TestAggregateModelAliases_IntraCRArrayOrderLastWinsWithinCR(t *testing.T) {
	// Defensive: CEL rejects intra-CR dups at admission, but the aggregator
	// must still be deterministic if it ever sees one (e.g., from a stale
	// informer cache during a CEL-disabled apiserver in tests).
	items := []litellmv1alpha1.LiteLLMModelAlias{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "only", Namespace: "ns1"},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{
					{Name: "k", Value: "first"},
					{Name: "k", Value: "later-wins"},
				},
			},
		},
	}
	res := AggregateModelAliases(items)
	if res.Desired["k"] != "later-wins" {
		t.Fatalf("intra-CR array-order last-wins broken: %q", res.Desired["k"])
	}
	if res.WinnerOf("k") != "ns1/only#1" {
		t.Fatalf("winner identifier: want ns1/only#1 got %s", res.WinnerOf("k"))
	}
	if losers := res.LosersOf("k"); !reflect.DeepEqual(losers, []string{"ns1/only#0"}) {
		t.Fatalf("losers: want [ns1/only#0] got %v", losers)
	}
}

func TestAggregateModelAliases_EmptyInputProducesEmptyMap(t *testing.T) {
	res := AggregateModelAliases(nil)
	if len(res.Desired) != 0 {
		t.Fatalf("expected empty desired, got %v", res.Desired)
	}
}

func TestAggregateModelAliases_ResolveCRProducesPerEntryStatuses(t *testing.T) {
	winner := litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "z-last", Namespace: "ns1"},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{
				{Name: "shared", Value: "last-wins"},
				{Name: "uniq-z", Value: "vz"},
			},
		},
	}
	loser := litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "a-first", Namespace: "ns1"},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{
				{Name: "shared", Value: "first"},
				{Name: "uniq-a", Value: "va"},
			},
		},
	}
	res := AggregateModelAliases([]litellmv1alpha1.LiteLLMModelAlias{loser, winner})

	wStat := res.ResolveCR(winner)
	if len(wStat) != 2 || !wStat[0].Applied || wStat[0].AppliedValue != "last-wins" || !wStat[1].Applied {
		t.Fatalf("winner CR status: %+v", wStat)
	}

	lStat := res.ResolveCR(loser)
	if len(lStat) != 2 {
		t.Fatalf("loser CR status len: %d", len(lStat))
	}
	if lStat[0].Applied || lStat[0].ConflictsWith != "ns1/z-last#0" {
		t.Fatalf("loser entry 0 status wrong: %+v", lStat[0])
	}
	if !lStat[1].Applied || lStat[1].AppliedValue != "va" {
		t.Fatalf("loser entry 1 (no conflict) should still apply: %+v", lStat[1])
	}
}
