//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var modelAliasGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmodelaliases",
}

// newModelAliasCR builds a multi-entry LiteLLMModelAlias CR.
func newModelAliasCR(name, ns string, entries [][2]string) *unstructured.Unstructured {
	aliases := make([]interface{}, 0, len(entries))
	for _, kv := range entries {
		aliases = append(aliases, map[string]interface{}{
			"name":  kv[0],
			"value": kv[1],
		})
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMModelAlias",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"aliases": aliases,
			},
		},
	}
}

var _ = Describe("LiteLLMModelAlias", Label("modelalias"), Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const crName = "e2e-aliases"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(modelAliasGVR).Namespace(ns).
			Delete(ctx, crName, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(modelAliasGVR).Namespace(ns).
			Delete(ctx, crName, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	It("multi-entry alias CR reaches Ready=True with per-entry status rows", func() {
		cr := newModelAliasCR(crName, ns, [][2]string{
			{"e2e.smart", "gemini.gemini-flash-latest"},
			{"e2e.fast", "gemini.gemini-flash-latest"},
		})
		_, err := dyn.Resource(modelAliasGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Create CR")

		Eventually(func(g Gomega) {
			got, err := dyn.Resource(modelAliasGVR).Namespace(ns).
				Get(ctx, crName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			ready := false
			for _, c := range conds {
				cm, _ := c.(map[string]interface{})
				if cm["type"] == "Ready" && cm["status"] == "True" && cm["reason"] == "Synced" {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue(), "Ready=True/Synced expected; conditions=%v", conds)

			rows, _, _ := unstructured.NestedSlice(got.Object, "status", "aliasStatuses")
			g.Expect(rows).To(HaveLen(2), "aliasStatuses should have one row per spec.aliases entry")
			for _, r := range rows {
				rm, _ := r.(map[string]interface{})
				g.Expect(rm["applied"]).To(BeTrue(), "row %v not applied", rm)
				g.Expect(rm["appliedValue"]).NotTo(BeEmpty(), "row %v missing appliedValue", rm)
			}
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})
})
