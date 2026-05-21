//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var modelGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmodels",
}

func newOpenAIMockModel(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind": "LiteLLMModel",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"params": map[string]interface{}{
					"model":    "openai/gpt-4o-mini",
					"api_key":  "sk-mock-key",
					"api_base": "http://openai-mock.mocks.svc.cluster.local:8080",
				},
			},
		},
	}
}

func litellmModelID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "litellmModelID")
	return id
}

// envtest counterpart: internal/controller/model_controller_test.go covers
// reconcile logic, secret substitution, finalizer, and the AC-N3 / SEC
// invariants against an in-process mock LiteLLM. This suite proves the
// Helm-deployed operator round-trips a real Model CR end-to-end through
// the real LiteLLM (real PUT wholesale-replace, real persistence, AC-M3
// drift after out-of-band delete).
var _ = Describe("LiteLLMModel", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const modelName = "tier2-openai"

	BeforeAll(func() {
		_, _ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(ctx, modelName, metav1.DeleteOptions{}), Eventually(func(g Gomega) {
			_, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "Model still present")
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("registers via POST /model/new (status.lastRendered.litellmModelID populated)", func() {
		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(modelName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(litellmModelID(obj)).NotTo(BeEmpty(), "litellmModelID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("wholesale-replaces drift within one safety re-list tick (AC-M3 conformance)", func() {
		// Ensure the model from the previous spec is still here.
		obj, err := dyn.Resource(modelGVR).Namespace(ns).
			Get(ctx, modelName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		oldID := litellmModelID(obj)
		Expect(oldID).NotTo(BeEmpty(), "AC-M1 spec must have populated litellmModelID")

		// Delete the model from LiteLLM out-of-band via POST /model/delete.
		// Run curl inside the cluster (host has no kubectl-routable curl).
		podName := fmt.Sprintf("drift-poke-%d", time.Now().UnixNano())
		body := fmt.Sprintf(`{"id":"%s"}`, oldID)
		out, err := exec.Command("kubectl", "-n", "litellm-system", "run", podName,
			"--rm", "-i", "--restart=Never", "--quiet",
			"--image=curlimages/curl:8.10.1", "--",
			"curl", "-sS", "-X", "POST",
			"-H", "Authorization: Bearer sk-test-master-key",
			"-H", "Content-Type: application/json",
			"--data", body,
			"http://litellm.litellm-system.svc.cluster.local:4000/model/delete",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))

		// Within one safety re-list tick (10s) + a reconcile (a few s),
		// expect the litellmModelID to be re-issued (wholesale-replace).
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			newID := litellmModelID(obj)
			g.Expect(newID).NotTo(BeEmpty())
			g.Expect(newID).NotTo(Equal(oldID), "litellmModelID still equals oldID — no replace yet")
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})
})
