// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func aliasTestMD(suffix string) *litellmv1alpha1.LiteLLMModelDiscovery {
	return &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-discovery", UID: "uid-1"},
		Spec:       litellmv1alpha1.ModelDiscoverySpec{AliasSuffix: suffix},
	}
}

func TestBuildDiscoveryAliases_EmptySuffix(t *testing.T) {
	if got := buildDiscoveryAliases(aliasTestMD(""), []string{"claude-opus-5-5"}, "ns"); got != nil {
		t.Fatalf("empty suffix: got %d CRs, want nil", len(got))
	}
}

func TestBuildDiscoveryAliases_NoChildren(t *testing.T) {
	if got := buildDiscoveryAliases(aliasTestMD("[1m]"), nil, "ns"); got != nil {
		t.Fatalf("no children: got %d CRs, want nil", len(got))
	}
}

func TestBuildDiscoveryAliases_Entries(t *testing.T) {
	got := buildDiscoveryAliases(aliasTestMD("[1m]"), []string{"claude-opus-5-5", "claude-sonnet-5"}, "ns")
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

func TestBuildDiscoveryAliases_Chunks(t *testing.T) {
	children := make([]string, aliasChunkSize+1)
	for i := range children {
		children[i] = fmt.Sprintf("m-%03d", i)
	}
	got := buildDiscoveryAliases(aliasTestMD("[1m]"), children, "ns")
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
