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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

var connGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmconnections",
}

func dynClient() dynamic.Interface {
	cfg, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		cfg, err = ctrl.GetConfig()
		Expect(err).NotTo(HaveOccurred())
	}
	d, err := dynamic.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	return d
}

func readyCondition(obj *unstructured.Unstructured) (status, reason string, found bool) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return "", "", false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "Ready" {
			s, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			return s, r, true
		}
	}
	return "", "", false
}

// envtest counterpart: internal/controller/litellmconnection_{controller,fastpath,finalizer,proxy}_test.go
// cover reconcile logic (probe loop, fastpath cache, finalizer, AC-N3 paths)
// against an in-process mock LiteLLM. This suite proves the Helm-deployed
// operator handles the real LiteLLM Pod end-to-end (CONN-04 happy path,
// real 401 propagation, in-cluster DNS).
var _ = Describe("LiteLLMConnection", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()

	It("becomes Ready against in-cluster LiteLLM", func() {
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(connGVR).Namespace("default").
				Get(ctx, "default", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			status, reason, found := readyCondition(obj)
			g.Expect(found).To(BeTrue(), "Ready condition not yet present")
			g.Expect(status).To(Equal("True"), "Ready=%s reason=%s", status, reason)
			g.Expect(reason).To(Equal("Synced"))
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("triggers 401 fast-path when master key is rotated", func() {
		const ns = "default"
		const secName = "litellm-master-key"
		const origKey = "sk-test-master-key"
		const badKey = "sk-wrong-key-tier2"

		// Rotate the Secret to a value LiteLLM will reject. Reuse Secret-on-CR
		// flow via cs (kubernetes clientset).
		patch := []byte(`{"stringData":{"master-key":"` + badKey + `"}}`)
		_, err := cs.CoreV1().Secrets(ns).Patch(ctx, secName,
			"application/strategic-merge-patch+json", patch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Restore at end of It so subsequent specs keep working.
		DeferCleanup(func() {
			restore := []byte(`{"stringData":{"master-key":"` + origKey + `"}}`)
			_, _ = cs.CoreV1().Secrets(ns).Patch(ctx, secName,
				"application/strategic-merge-patch+json", restore, metav1.PatchOptions{})
			// Wait for operator to re-sync.
			Eventually(func(g Gomega) {
				obj, err := dyn.Resource(connGVR).Namespace(ns).
					Get(ctx, "default", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				status, reason, _ := readyCondition(obj)
				g.Expect(status).To(Equal("True"), "Ready=%s reason=%s", status, reason)
				g.Expect(reason).To(Equal("Synced"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})

		// Expect Ready=False with reason BadMasterKey within 5s of rotation
		// (operator's 401 fast-path per spec §7.7 / AC-C3c).
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(connGVR).Namespace(ns).
				Get(ctx, "default", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			status, reason, found := readyCondition(obj)
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("False"), "still Ready=True after rotation")
			g.Expect(reason).To(Equal("BadMasterKey"), "Ready=%s reason=%s", status, reason)
		}, 5*time.Second, 250*time.Millisecond).Should(Succeed())
	})
})
