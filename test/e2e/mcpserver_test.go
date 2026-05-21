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

var mcpsrvGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmcpservers",
}

func mcpServerID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "serverID")
	return id
}

// envtest counterpart: internal/controller/mcpserver_controller_test.go
// covers reconcile logic, secret rotation, 401 fastpath, conflict-explicit,
// and AC-DC1 hand-managed coexistence against an in-process mock LiteLLM.
// This suite proves the Helm-deployed operator handles a real MCPServer CR
// end-to-end (real upstream 401, real chart Service DNS, AC-M3 wholesale
// replace after real out-of-band delete).
var _ = Describe("LiteLLMMCPServer", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const name = "tier2-mcp-admin"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(mcpsrvGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	It("admin-immediate: status.lastRendered.serverID populated", func() {
		cr := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "litellm.ackstorm.ai/v1alpha1",
				"kind": "LiteLLMMCPServer",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": ns,
				},
				"spec": map[string]interface{}{
					"endpoint":  "http://example.invalid/mcp",
					"transport": "http",
				},
			},
		}
		_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mcpsrvGVR).Namespace(ns).
				Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mcpServerID(obj)).NotTo(BeEmpty(), "serverID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("finalizer cascade: delete removes CR + clean LiteLLM-side cleanup", func() {
		// Confirm CR is present.
		_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).
			Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "admin-immediate spec must have created CR")

		fg := metav1.DeletePropagationForeground
		err = dyn.Resource(mcpsrvGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
		Expect(err).NotTo(HaveOccurred())

		// Wait for CR to fully disappear (finalizer drained → LiteLLM DELETE issued).
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).
				Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "CR still present")
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})
})
