//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var mdiscGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmodeldiscoveries",
}

func newOpenAIDiscovery(name, ns, secretName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind": "LiteLLMModelDiscovery",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"type":   "openai",
				"prefix": "openai",
				"credentialsSecretRef": map[string]interface{}{
					"name": secretName,
				},
				// Operator hits ${baseURL}/models (NOT /v1/models). Include /v1
				// suffix so the mock's /v1/models handler resolves.
				"baseUrl": "http://openai-mock.mocks.svc.cluster.local:8080/v1",
				"refresh": map[string]interface{}{
					"interval": "1m",
				},
			},
		},
	}
}

func conditionStatus(obj *unstructured.Unstructured, condType string) (string, string) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return "", ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == condType {
			s, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			return s, r
		}
	}
	return "", ""
}

// envtest counterpart: internal/controller/modeldiscovery_controller_test.go
// covers state-machine logic (atomic-refresh, provider-error preservation,
// adoption recognition, name derivation) against an in-process mock
// provider HTTP server. This suite proves the Helm-deployed operator
// drives a real ModelDiscovery CR against a real upstream-mock and
// reconciles child Model CRs end-to-end.
var _ = Describe("LiteLLMModelDiscovery", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const mdName = "tier2-openai-disc"
	const secretName = "tier2-openai-creds"

	BeforeAll(func() {
		// Provider credentials Secret (key OPENAI_API_KEY per provider table).
		_, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			StringData: map[string]string{"OPENAI_API_KEY": "sk-mock-discovery"},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Fail(err.Error())
		}
		// Best-effort cleanup of any leftover ModelDiscovery from a prior run.
		_ = dyn.Resource(mdiscGVR).Namespace(ns).Delete(ctx, mdName, metav1.DeleteOptions{})
	})

	AfterAll(func() {
		// Foreground propagation: K8s GC sets DeletionTimestamp on owned
		// children immediately, so their finalizers run and they drain.
		// Background (default) deadlocks against the operator's finalizer
		// which waits for children to drain.
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(mdiscGVR).Namespace(ns).
			Delete(ctx, mdName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = cs.CoreV1().Secrets(ns).Delete(ctx, secretName, metav1.DeleteOptions{})
	})

	flipMock := func(mode string) {
		out, err := exec.Command("kubectl", "-n", "mocks", "run",
			"flip-poke-"+mode,
			"--rm", "-i", "--restart=Never", "--quiet",
			"--image=curlimages/curl:8.10.1", "--",
			"curl", "-sS", "-X", "POST",
			"-H", "Content-Type: application/json",
			"--data", `{"mode":"`+mode+`"}`,
			"http://openai-mock.mocks.svc.cluster.local:8080/__mock/auth-mode",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "flip out=%s", string(out))
	}

	It("openai: discovers child Model CRs via mock /v1/models", func() {
		_, err := dyn.Resource(mdiscGVR).Namespace(ns).
			Create(ctx, newOpenAIDiscovery(mdName, ns, secretName), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Expect child Models named openai.<id> (gpt-4o-mini, gpt-4o, gpt-3.5-turbo)
		// with ownerRef back to the ModelDiscovery within 60s.
		expectedNames := []string{
			"openai.gpt-4o-mini",
			"openai.gpt-4o",
			"openai.gpt-3.5-turbo",
		}
		Eventually(func(g Gomega) {
			for _, n := range expectedNames {
				obj, err := dyn.Resource(modelGVR).Namespace(ns).
					Get(ctx, n, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "child Model %q not yet created", n)
				owners := obj.GetOwnerReferences()
				g.Expect(owners).NotTo(BeEmpty(), "child Model %q has no ownerRefs", n)
				g.Expect(owners[0].Kind).To(Equal("LiteLLMModelDiscovery"))
				g.Expect(owners[0].Name).To(Equal(mdName))
			}
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Top-level Ready=True/Synced + SourceReachable=True/Ok.
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mdiscGVR).Namespace(ns).
				Get(ctx, mdName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			s, r := conditionStatus(obj, "Ready")
			g.Expect(s).To(Equal("True"), "Ready=%s reason=%s", s, r)
			g.Expect(r).To(Equal("Synced"))
			s2, r2 := conditionStatus(obj, "SourceReachable")
			g.Expect(s2).To(Equal("True"), "SourceReachable=%s reason=%s", s2, r2)
			g.Expect(r2).To(Equal("Ok"))
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("openai: 401 fast-path → SourceReachable=AuthFailed, no child writes", func() {
		// Snapshot child Models that exist now (before flip) so we can later
		// assert no new ones were created and existing ones survived.
		preChildren, err := dyn.Resource(modelGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		preNames := map[string]bool{}
		for _, c := range preChildren.Items {
			for _, o := range c.GetOwnerReferences() {
				if o.Kind == "LiteLLMModelDiscovery" && o.Name == mdName {
					preNames[c.GetName()] = true
				}
			}
		}
		Expect(len(preNames)).To(BeNumerically(">=", 3), "AC-MD1 children missing")

		flipMock("reject-401")
		DeferCleanup(func() {
			flipMock("accept")
			// Sanity-check the MDisc still exists at cleanup time. If 404, the
			// AfterAll cascade-delete raced ahead and there's nothing left to
			// wait on — skip the Ok wait.
			if _, err := dyn.Resource(mdiscGVR).Namespace(ns).
				Get(ctx, mdName, metav1.GetOptions{}); err != nil {
				return
			}
			Eventually(func(g Gomega) {
				obj, err := dyn.Resource(mdiscGVR).Namespace(ns).
					Get(ctx, mdName, metav1.GetOptions{})
				if err != nil {
					return // MDisc deleted by AfterAll mid-wait — accept.
				}
				s, _ := conditionStatus(obj, "SourceReachable")
				g.Expect(s).To(Equal("True"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})

		// Bump spec to force immediate reconcile (waiting one full
		// refresh.interval=1m would still work, but slow).
		patch := []byte(`[{"op":"replace","path":"/spec/refresh/interval","value":"2m"}]`)
		_, err = dyn.Resource(mdiscGVR).Namespace(ns).
			Patch(ctx, mdName, "application/json-patch+json", patch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		// SourceReachable=False/AuthFailed within 15s.
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mdiscGVR).Namespace(ns).
				Get(ctx, mdName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			s, r := conditionStatus(obj, "SourceReachable")
			g.Expect(s).To(Equal("False"), "SourceReachable=%s reason=%s", s, r)
			g.Expect(r).To(Equal("AuthFailed"))
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		// No new child Model CRs were created and existing ones still here.
		postChildren, err := dyn.Resource(modelGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		postNames := map[string]bool{}
		for _, c := range postChildren.Items {
			for _, o := range c.GetOwnerReferences() {
				if o.Kind == "LiteLLMModelDiscovery" && o.Name == mdName {
					postNames[c.GetName()] = true
				}
			}
		}
		Expect(postNames).To(Equal(preNames), "child Model set changed during AuthFailed")
	})
})
