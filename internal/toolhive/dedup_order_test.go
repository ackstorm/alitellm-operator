// SPDX-License-Identifier: Apache-2.0

package toolhive

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestDedupStore_Upsert_V1alpha1WinsRegardlessOfOrder is the M-B6 regression:
// v1alpha1 must win a cross-version collision no matter which version was
// stored first, and the collision must be recorded in both directions. The
// previous Upsert only handled existing=v1alpha1 + incoming=v1beta1, so when
// v1beta1 arrived first an incoming v1alpha1 overwrote it WITHOUT recording
// the collision.
func TestDedupStore_Upsert_V1alpha1WinsRegardlessOfOrder(t *testing.T) {
	mk := func(version string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "toolhive.stacklok.dev", Version: version, Kind: "MCPServer",
		})
		o.SetNamespace("ns")
		o.SetName("x")
		_ = unstructured.SetNestedField(o.Object, "url-"+version, "status", "url")
		return o
	}

	alphaFirst := newDedupStore()
	alphaFirst.Upsert(mk("v1alpha1"))
	alphaFirst.Upsert(mk("v1beta1"))

	betaFirst := newDedupStore()
	betaFirst.Upsert(mk("v1beta1"))
	betaFirst.Upsert(mk("v1alpha1"))

	for name, s := range map[string]*dedupStore{"alpha-first": alphaFirst, "beta-first": betaFirst} {
		list := s.List("MCPServer", "")
		if len(list) != 1 {
			t.Fatalf("%s: want 1 deduped item, got %d", name, len(list))
		}
		url, _, _ := unstructured.NestedString(list[0].Object, "status", "url")
		if url != "url-v1alpha1" {
			t.Errorf("%s: v1alpha1 must win, got url %q", name, url)
		}
		if len(s.collisions) != 1 {
			t.Errorf("%s: want 1 recorded collision, got %d", name, len(s.collisions))
		}
	}
}
