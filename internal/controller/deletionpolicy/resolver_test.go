// SPDX-License-Identifier: Apache-2.0

package deletionpolicy

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func TestResolve_DefaultsToOrphan(t *testing.T) {
	m := &litellmv1alpha1.LiteLLMModel{ObjectMeta: metav1.ObjectMeta{Name: "m"}}
	if got := Resolve(m, ""); got != Orphan {
		t.Fatalf("empty spec policy should resolve to Orphan, got %q", got)
	}
}

func TestResolve_SpecHonored(t *testing.T) {
	m := &litellmv1alpha1.LiteLLMModel{ObjectMeta: metav1.ObjectMeta{Name: "m"}}
	if got := Resolve(m, "Delete"); got != Delete {
		t.Fatalf("spec=Delete should resolve to Delete, got %q", got)
	}
}

func TestResolve_AnnotationOverridesSpec(t *testing.T) {
	m := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "m",
			Annotations: map[string]string{AnnotationOverride: "Orphan"},
		},
	}
	if got := Resolve(m, "Delete"); got != Orphan {
		t.Fatalf("annotation Orphan should override spec Delete, got %q", got)
	}
}

func TestResolve_InvalidAnnotationIgnored(t *testing.T) {
	m := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "m",
			Annotations: map[string]string{AnnotationOverride: "Wat"},
		},
	}
	if got := Resolve(m, "Delete"); got != Delete {
		t.Fatalf("invalid annotation should be ignored, got %q", got)
	}
}

func TestResolve_DiscoveryOwnedForcesOrphan(t *testing.T) {
	ctrl := true
	m := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "m",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: litellmv1alpha1.GroupVersion.String(),
				Kind:       "LiteLLMModelDiscovery",
				Name:       "parent",
				UID:        "abc",
				Controller: &ctrl,
			}},
		},
	}
	if got := Resolve(m, "Delete"); got != Orphan {
		t.Fatalf("discovery-owned child must resolve to Orphan, got %q", got)
	}
}

func TestResolve_DiscoveryOwnerNonControllerIgnored(t *testing.T) {
	ctrl := false
	m := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "m",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: litellmv1alpha1.GroupVersion.String(),
				Kind:       "LiteLLMModelDiscovery",
				Name:       "parent",
				UID:        "abc",
				Controller: &ctrl,
			}},
		},
	}
	if got := Resolve(m, "Delete"); got != Delete {
		t.Fatalf("non-controller discovery owner must NOT force Orphan, got %q", got)
	}
}
