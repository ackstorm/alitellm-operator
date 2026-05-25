// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLiteLLMModelAlias_TypeShape(t *testing.T) {
	a := &LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ackstorm-bundle", Namespace: "ackstorm"},
		Spec: LiteLLMModelAliasSpec{
			Aliases: []ModelAliasEntry{
				{Name: "ackstorm.smart", Value: "GEMINI.gemini-3-pro-preview"},
				{Name: "ackstorm.fast", Value: "GEMINI.gemini-3-flash"},
			},
		},
	}
	if len(a.Spec.Aliases) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(a.Spec.Aliases))
	}
	if a.Spec.Aliases[0].Name != "ackstorm.smart" || a.Spec.Aliases[0].Value != "GEMINI.gemini-3-pro-preview" {
		t.Fatalf("entry 0 roundtrip failed: %+v", a.Spec.Aliases[0])
	}
	if a.Spec.Aliases[1].Name != "ackstorm.fast" || a.Spec.Aliases[1].Value != "GEMINI.gemini-3-flash" {
		t.Fatalf("entry 1 roundtrip failed: %+v", a.Spec.Aliases[1])
	}
}

func TestLiteLLMModelAliasStatus_PerEntryShape(t *testing.T) {
	st := LiteLLMModelAliasStatus{
		ObservedGeneration: 7,
		AliasStatuses: []AliasEntryStatus{
			{Name: "ackstorm.smart", Applied: true, AppliedValue: "GEMINI.gemini-3-pro-preview"},
			{Name: "ackstorm.fast", Applied: false, ConflictsWith: "other-ns/other-name#0"},
		},
	}
	if len(st.AliasStatuses) != 2 {
		t.Fatalf("expected 2 status rows, got %d", len(st.AliasStatuses))
	}
	if !st.AliasStatuses[0].Applied || st.AliasStatuses[0].AppliedValue == "" {
		t.Fatalf("winner row not preserved: %+v", st.AliasStatuses[0])
	}
	if st.AliasStatuses[1].Applied || st.AliasStatuses[1].ConflictsWith == "" {
		t.Fatalf("loser row not preserved: %+v", st.AliasStatuses[1])
	}
}

func TestLiteLLMModelAliasList_TypeShape(t *testing.T) {
	list := &LiteLLMModelAliasList{Items: []LiteLLMModelAlias{{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
	}}}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(list.Items))
	}
}
