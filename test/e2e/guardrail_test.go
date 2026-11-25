//go:build e2e

// SPDX-License-Identifier: MIT

package e2e_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var guardrailGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmguardrails",
}

// litellmGuardrailModes returns the litellm_params.mode of the GET
// /v2/guardrails/list row named name, normalized to a list (LiteLLM stores a
// single mode as a string), and whether the row was found.
func litellmGuardrailModes(name string) ([]string, bool) {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "guardrail-list-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/v2/guardrails/list",
	)
	var resp struct {
		Guardrails []struct {
			GuardrailName string `json:"guardrail_name"`
			LitellmParams struct {
				Mode json.RawMessage `json:"mode"`
			} `json:"litellm_params"`
		} `json:"guardrails"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	for _, g := range resp.Guardrails {
		if g.GuardrailName != name {
			continue
		}
		var one string
		if json.Unmarshal(g.LitellmParams.Mode, &one) == nil {
			return []string{one}, true
		}
		var many []string
		Expect(json.Unmarshal(g.LitellmParams.Mode, &many)).To(Succeed(),
			"mode=%s", string(g.LitellmParams.Mode))
		return many, true
	}
	return nil, false
}

// post_mcp_call runs a guardrail on an MCP tool result before it reaches the
// model. The e2e cluster has no live MCP server, so this proves only that
// LiteLLM accepts and stores the mode, not that a tool result is masked.
var _ = Describe("LiteLLMGuardRail", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const name = "e2e-guardrail-post-mcp"

	BeforeAll(func() {
		_ = dyn.Resource(guardrailGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	})

	It("mode post_mcp_call: CR goes Ready and LiteLLM lists the guardrail with that mode", func() {
		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMGuardRail",
			"metadata":   map[string]interface{}{"name": name, "namespace": ns},
			"spec": map[string]interface{}{
				"guardrailName":  name,
				"provider":       "litellm_content_filter",
				"deletionPolicy": "Delete",
				"mode":           []interface{}{"post_mcp_call"},
				"params": map[string]interface{}{
					"patterns": []interface{}{map[string]interface{}{
						"pattern_type": "prebuilt",
						"pattern_name": "aws_access_key",
						"action":       "MASK",
					}},
				},
			},
		}}
		_, err := dyn.Resource(guardrailGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(guardrailGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			status, reason, _ := readyCondition(obj)
			g.Expect(status).To(Equal("True"), "Ready reason=%s", reason)
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		modes, found := litellmGuardrailModes(name)
		Expect(found).To(BeTrue(), "guardrail %q not in GET /v2/guardrails/list", name)
		Expect(modes).To(Equal([]string{"post_mcp_call"}))
	})

	It("delete removes the CR and the LiteLLM row", func() {
		Expect(dyn.Resource(guardrailGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(guardrailGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "CR still present")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
		_, found := litellmGuardrailModes(name)
		Expect(found).To(BeFalse(), "LiteLLM row survived the CR delete")
	})
})
