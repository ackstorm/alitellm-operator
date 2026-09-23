// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func aliasTestMD(rules ...litellmv1alpha1.ModelDiscoveryAliasRule) *litellmv1alpha1.LiteLLMModelDiscovery {
	return &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-discovery", UID: "uid-1"},
		Spec:       litellmv1alpha1.ModelDiscoverySpec{Aliases: rules},
	}
}

// aliasMap flattens the rendered CRs into name → value.
func aliasMap(t *testing.T, crs []*litellmv1alpha1.LiteLLMModelAlias) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, cr := range crs {
		for _, e := range cr.Spec.Aliases {
			out[e.Name] = e.Value
		}
	}
	return out
}

func mustBuild(t *testing.T, md *litellmv1alpha1.LiteLLMModelDiscovery, generated []string) []*litellmv1alpha1.LiteLLMModelAlias {
	t.Helper()
	got, err := buildDiscoveryAliases(md, generated, "ns")
	if err != nil {
		t.Fatalf("buildDiscoveryAliases: %v", err)
	}
	return got
}

func TestBuildDiscoveryAliases_NoRules(t *testing.T) {
	if got := mustBuild(t, aliasTestMD(), []string{"claude-opus-5-5"}); got != nil {
		t.Fatalf("no rules: got %d CRs, want nil", len(got))
	}
}

func TestBuildDiscoveryAliases_NoChildren(t *testing.T) {
	if got := mustBuild(t, aliasTestMD(litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]"}), nil); got != nil {
		t.Fatalf("no children: got %d CRs, want nil", len(got))
	}
}

func TestBuildDiscoveryAliases_Entries(t *testing.T) {
	got := mustBuild(t, aliasTestMD(litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]"}),
		[]string{"claude-opus-5-5", "claude-sonnet-5"})
	if len(got) != 1 {
		t.Fatalf("CR count: got %d, want 1", len(got))
	}
	a := got[0]
	if a.Name != "anthropic-discovery-aliases-0" || a.Namespace != "ns" {
		t.Errorf("name/ns: got %s/%s", a.Namespace, a.Name)
	}
	if a.Labels[generatedByLabel] != "anthropic-discovery" {
		t.Errorf("label: got %v", a.Labels)
	}
	if len(a.OwnerReferences) != 1 || a.OwnerReferences[0].UID != "uid-1" ||
		a.OwnerReferences[0].Controller == nil || !*a.OwnerReferences[0].Controller ||
		a.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Errorf("ownerRef: got %+v (want controller=true, blockOwnerDeletion unset)", a.OwnerReferences)
	}
	want := []litellmv1alpha1.ModelAliasEntry{
		{Name: "claude-opus-5-5[1m]", Value: "claude-opus-5-5"},
		{Name: "claude-sonnet-5[1m]", Value: "claude-sonnet-5"},
	}
	if fmt.Sprint(a.Spec.Aliases) != fmt.Sprint(want) {
		t.Errorf("aliases: got %v, want %v", a.Spec.Aliases, want)
	}
}

// Rules are independent: a filtered suffix rule, an unfiltered prefix rule,
// and a combined prefix+suffix rule each emit their own aliases.
func TestBuildDiscoveryAliases_Rules(t *testing.T) {
	md := aliasTestMD(
		litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]", Include: []string{"claude-(opus|sonnet)"}},
		litellmv1alpha1.ModelDiscoveryAliasRule{Prefix: "anthropic/"},
		litellmv1alpha1.ModelDiscoveryAliasRule{Prefix: "x/", Suffix: "-y", Include: []string{"claude-haiku"}},
	)
	got := aliasMap(t, mustBuild(t, md, []string{"claude-haiku-4-5", "claude-opus-5-5", "claude-sonnet-5"}))
	want := map[string]string{
		"claude-opus-5-5[1m]":        "claude-opus-5-5",
		"claude-sonnet-5[1m]":        "claude-sonnet-5",
		"anthropic/claude-haiku-4-5": "claude-haiku-4-5",
		"anthropic/claude-opus-5-5":  "claude-opus-5-5",
		"anthropic/claude-sonnet-5":  "claude-sonnet-5",
		"x/claude-haiku-4-5-y":       "claude-haiku-4-5",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("aliases:\n got %v\nwant %v", got, want)
	}
}

// Include is anchored at the start like spec.filters, and a pattern that
// matches nothing is not an error.
func TestBuildDiscoveryAliases_IncludeAnchoredNoMatchOK(t *testing.T) {
	md := aliasTestMD(litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]", Include: []string{"opus", "claude-fable"}})
	if got := mustBuild(t, md, []string{"claude-opus-5-5"}); got != nil {
		t.Fatalf("unanchored/absent patterns must match nothing; got %v", aliasMap(t, got))
	}
}

func TestBuildDiscoveryAliases_InvalidPattern(t *testing.T) {
	md := aliasTestMD(litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]", Include: []string{"("}})
	if _, err := buildDiscoveryAliases(md, []string{"claude-opus-5-5"}, "ns"); err == nil {
		t.Fatal("invalid RE2 pattern: want error, got nil")
	}
}

// Two rules producing the same alias name collapse to one entry (the alias
// CRD rejects duplicate names within a CR); the later rule wins.
func TestBuildDiscoveryAliases_DuplicateLaterRuleWins(t *testing.T) {
	md := aliasTestMD(
		litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "-b"},
		litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "b"},
	)
	got := mustBuild(t, md, []string{"a", "a-"})
	// "a" + "-b" == "a-" + "b" == "a-b"; the second rule maps it to "a-".
	if m := aliasMap(t, got); m["a-b"] != "a-" || len(got[0].Spec.Aliases) != 3 {
		t.Errorf("duplicate resolution: got %v", m)
	}
}

func TestBuildDiscoveryAliases_Chunks(t *testing.T) {
	children := make([]string, aliasChunkSize+1)
	for i := range children {
		children[i] = fmt.Sprintf("m-%03d", i)
	}
	got := mustBuild(t, aliasTestMD(litellmv1alpha1.ModelDiscoveryAliasRule{Suffix: "[1m]"}), children)
	if len(got) != 2 {
		t.Fatalf("CR count: got %d, want 2", len(got))
	}
	if len(got[0].Spec.Aliases) != aliasChunkSize || len(got[1].Spec.Aliases) != 1 {
		t.Errorf("chunk sizes: got %d,%d", len(got[0].Spec.Aliases), len(got[1].Spec.Aliases))
	}
	if got[1].Name != "anthropic-discovery-aliases-1" || got[1].Spec.Aliases[0].Value != "m-128" {
		t.Errorf("second chunk: got %s %v", got[1].Name, got[1].Spec.Aliases)
	}
}
