// SPDX-License-Identifier: Apache-2.0

package conflict_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ackstorm/alitellm-operator/internal/controller/conflict"
)

func obj(ns, name string) client.Object {
	cm := &corev1.ConfigMap{}
	cm.SetNamespace(ns)
	cm.SetName(name)
	cm.SetCreationTimestamp(metav1.Now())
	return cm
}

func TestKey_FormatsNamespaceSlashName(t *testing.T) {
	if got := conflict.Key(obj("ns-a", "foo")); got != "ns-a/foo" {
		t.Fatalf("Key=%q, want %q", got, "ns-a/foo")
	}
}

func TestResolveWinner_NilOnEmpty(t *testing.T) {
	if w := conflict.ResolveWinner(nil); w != nil {
		t.Fatalf("want nil winner on empty input, got %v", w)
	}
}

func TestResolveWinner_SingleCandidateWins(t *testing.T) {
	only := obj("ns", "only")
	w := conflict.ResolveWinner([]client.Object{only})
	if conflict.Key(w) != "ns/only" {
		t.Fatalf("got %q, want ns/only", conflict.Key(w))
	}
}

func TestResolveWinner_LastInLexOrderWins(t *testing.T) {
	candidates := []client.Object{
		obj("alpha", "z"),
		obj("zeta", "a"),
		obj("beta", "m"),
	}
	// sort ASC: alpha/z, beta/m, zeta/a — LAST is zeta/a.
	w := conflict.ResolveWinner(candidates)
	if conflict.Key(w) != "zeta/a" {
		t.Fatalf("got %q, want zeta/a", conflict.Key(w))
	}
}

func TestResolveWinner_NamespaceBreaksTiesBeforeName(t *testing.T) {
	candidates := []client.Object{
		obj("ns-a", "z"),
		obj("ns-b", "a"),
	}
	// "ns-a/z" < "ns-b/a" — LAST is ns-b/a.
	w := conflict.ResolveWinner(candidates)
	if conflict.Key(w) != "ns-b/a" {
		t.Fatalf("got %q, want ns-b/a", conflict.Key(w))
	}
}

func TestResolveWinner_StableAcrossInputPermutation(t *testing.T) {
	a, b, c := obj("ns", "a"), obj("ns", "b"), obj("ns", "c")
	for _, in := range [][]client.Object{
		{a, b, c},
		{c, b, a},
		{b, a, c},
	} {
		if conflict.Key(conflict.ResolveWinner(in)) != "ns/c" {
			t.Fatalf("ResolveWinner not stable for permutation %v", in)
		}
	}
}

func TestIsLoser_FalseWhenSelfIsWinner(t *testing.T) {
	self := obj("ns", "z")
	winner := obj("ns", "z")
	if conflict.IsLoser(self, winner) {
		t.Fatal("self==winner must not be a loser")
	}
}

func TestIsLoser_TrueWhenSelfIsNotWinner(t *testing.T) {
	self := obj("ns", "a")
	winner := obj("ns", "z")
	if !conflict.IsLoser(self, winner) {
		t.Fatal("self!=winner must be a loser")
	}
}

func TestIsLoser_FalseWhenWinnerIsNil(t *testing.T) {
	self := obj("ns", "a")
	if conflict.IsLoser(self, nil) {
		t.Fatal("nil winner means no conflict; self must not be a loser")
	}
}
