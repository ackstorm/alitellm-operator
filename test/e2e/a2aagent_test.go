//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var a2aGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellma2aagents",
}

func newA2AAgent(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind": "LiteLLMA2AAgent",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				// Bogus endpoint: if LiteLLM ever ran a real health
				// probe against it (i.e. operator failed to pass
				// health_check=false on its lifecycle calls), the
				// reconcile would time out and Ready/agentID would
				// never land — that's how this spec proves §6.6.
				"endpoint": "http://example.invalid/a2a",
				"agentCard": map[string]interface{}{
					"name":        "tier2-a2a",
					"description": "Tier 2 A2AAgent §6.6 health_check=false canary",
					"capabilities": map[string]interface{}{
						"streaming": false,
					},
					"defaultInputModes":  []interface{}{"text"},
					"defaultOutputModes": []interface{}{"text"},
				},
			},
		},
	}
}

func a2aAgentID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "agentID")
	return id
}

// envtest counterpart: internal/controller/a2aagent_controller_test.go +
// a2aagent_*_test.go siblings cover reconcile logic, conflict-explicit,
// 401 fastpath, AC-DC1 hand-managed coexistence, AC-N3 path enforcement,
// and propagation-on-secret-rotation against an in-process mock LiteLLM.
// This suite proves the Helm-deployed operator handles A2AAgent end-to-end
// against a real LiteLLM (real persistence, real chart Service DNS).
var _ = Describe("LiteLLMA2AAgent", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const name = "tier2-a2a"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(a2aGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(a2aGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	It("registers with bogus endpoint (operator passes health_check=false on GET /v1/agents)", func() {
		_, err := dyn.Resource(a2aGVR).Namespace(ns).
			Create(ctx, newA2AAgent(name, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Ready=True + agentID populated within 60s. With a bogus
		// endpoint URL, this only succeeds if the operator passes
		// health_check=false to LiteLLM (per spec §6.6) — otherwise
		// LiteLLM blocks on the upstream probe.
		var agentID string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(a2aGVR).Namespace(ns).
				Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			agentID = a2aAgentID(obj)
			g.Expect(agentID).NotTo(BeEmpty(), "agentID not yet populated")
			s, r, _ := readyCondition(obj)
			g.Expect(s).To(Equal("True"), "Ready=%s reason=%s", s, r)
			g.Expect(r).To(Equal("Synced"))
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Cross-check: GET /v1/agents?health_check=false (the same
		// query LiteLLM exposes; the operator uses this too) returns
		// quickly and our agent is in the list.
		podName := fmt.Sprintf("a2a-list-poke-%d", time.Now().UnixNano())
		out, err := exec.Command("kubectl", "-n", "litellm-system", "run", podName,
			"--rm", "-i", "--restart=Never", "--quiet",
			"--image=curlimages/curl:8.10.1", "--",
			"curl", "-sS", "--max-time", "10",
			"-H", "Authorization: Bearer sk-test-master-key",
			"http://litellm.litellm-system.svc.cluster.local:4000/v1/agents?health_check=false",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))

		// kubectl run --rm may prepend "warning: couldn't attach to pod ."
		// lines on stderr/stdout merge. Strip everything before the first
		// JSON array byte.
		idx := bytes.IndexByte(out, '[')
		Expect(idx).To(BeNumerically(">=", 0), "no JSON array in: %s", string(out))
		body := out[idx:]
		var entries []map[string]interface{}
		Expect(json.Unmarshal(body, &entries)).To(Succeed(), "raw=%s", string(body))
		var hit bool
		for _, e := range entries {
			if id, _ := e["agent_id"].(string); id == agentID {
				hit = true
				break
			}
		}
		Expect(hit).To(BeTrue(),
			"agent %q not in /v1/agents response: %s", agentID, string(body))
	})
})
