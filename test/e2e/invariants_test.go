//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cross-cutting invariants ported from the hand-run production UAT runbook
// (test/uat/uat-runbook.sh).
//
// These held only by convention before: nothing in the suite asserted them, so
// a regression surfaced at the earliest during a manual post-release pass
// against the production cluster. They are deterministic and mock-friendly, so
// they belong on the PR gate instead — the runbook keeps only what genuinely
// requires prod (real provider credentials, scale, elapsed time).

var msdiscoveryGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmcpserverdiscoveries",
}

const generatedByLabel = "litellm.ackstorm.ai/generated-by"

// litellmModelIDs returns every model_info.id currently registered in LiteLLM,
// as a set. GET /model/info returns {"data":[{...,"model_info":{"id":...}}]}.
func litellmModelIDs() map[string]struct{} {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "modelinfo-poke", '{',
		"curl", "-sS", "--max-time", "20",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/model/info",
	)
	var resp struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				ID string `json:"id"`
			} `json:"model_info"`
		} `json:"data"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	ids := make(map[string]struct{}, len(resp.Data))
	for _, m := range resp.Data {
		ids[m.ModelInfo.ID] = struct{}{}
	}
	return ids
}

// isRouterModel reports whether a Model CR is a router pseudo-model. LiteLLM
// keeps auto_router deployments in its in-memory router, NOT the DB model
// table, so they never appear in GET /model/info. The operator deliberately
// skips its existence probe for them; the status-truth audit must skip them
// too or it reports a false positive on every router model.
func isRouterModel(obj *unstructured.Unstructured) bool {
	m, _, _ := unstructured.NestedString(obj.Object, "spec", "params", "model")
	return strings.HasPrefix(m, "auto_router/")
}

func readyCond(obj *unstructured.Unstructured) (status, reason, lastTransition string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, _ := c.(map[string]interface{})
		if t, _ := cm["type"].(string); t == "Ready" {
			status, _ = cm["status"].(string)
			reason, _ = cm["reason"].(string)
			lastTransition, _ = cm["lastTransitionTime"].(string)
		}
	}
	return
}

var _ = Describe("Operator invariants (ported from prod UAT)", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	// Own probe model. Ginkgo randomizes TOP-LEVEL containers, so this suite
	// may run before LiteLLMModel's — and 04-hydration ships only a
	// LiteLLMConnection, no Models. Depending on another container's fixture
	// would make these specs pass or fail on the luck of the seed.
	const probeModel = "invariants-probe"

	BeforeAll(func() {
		_ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(ctx, probeModel, metav1.DeleteOptions{})
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, probeModel, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "probe model still terminating")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(probeModel, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, probeModel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "probe modelID not yet populated")
			status, _, _ := readyCond(obj)
			g.Expect(status).To(Equal("True"), "probe model not Ready yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		_ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(context.Background(), probeModel, metav1.DeleteOptions{})
	})

	// UAT-S1. A CR reporting Ready=True while its tracked LiteLLM row is gone
	// is the single most misleading failure this operator can produce: every
	// dashboard reads green and inference 404s. The runbook sampled 10 random
	// models in prod; here the population is small enough to check ALL of them.
	It("UAT-S1 status-truth: every Ready Model's tracked modelID exists in LiteLLM", func() {
		Eventually(func(g Gomega) {
			list, err := dyn.Resource(modelGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			live := litellmModelIDs()
			var checked int
			var missing []string
			for i := range list.Items {
				obj := &list.Items[i]
				if isRouterModel(obj) {
					continue // never listed by /model/info — see isRouterModel
				}
				status, _, _ := readyCond(obj)
				id := modelID(obj)
				if status != "True" || id == "" {
					continue // only Ready CRs claim a live row
				}
				checked++
				if _, ok := live[id]; !ok {
					missing = append(missing,
						fmt.Sprintf("%s(id=%s)", obj.GetName(), id))
				}
			}
			g.Expect(checked).To(BeNumerically(">", 0),
				"no Ready Model with a modelID found — BeforeAll's %q probe should guarantee at least one",
				probeModel)
			g.Expect(missing).To(BeEmpty(),
				"%d/%d Ready Models claim a modelID absent from LiteLLM: %v",
				len(missing), checked, missing)
		}, 90*time.Second, 5*time.Second).Should(Succeed())
	})

	// UAT-D1. status.generatedCount is what operators and dashboards read to
	// decide a discovery "worked". If it drifts from the actual child count,
	// every consumer of that number is silently wrong.
	It("UAT-D1 discovery fan-out: status.generatedCount matches actual children", func() {
		type disc struct {
			gvr  schema.GroupVersionResource
			kind string
			// children of a ModelDiscovery are Models; of an
			// MCPServerDiscovery, MCPServers.
			childGVR schema.GroupVersionResource
		}
		for _, d := range []disc{
			{mdiscGVR, "LiteLLMModelDiscovery", modelGVR},
			{msdiscoveryGVR, "LiteLLMMCPServerDiscovery", mcpsrvGVR},
		} {
			list, err := dyn.Resource(d.gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for i := range list.Items {
				parent := list.Items[i].GetName()
				want, found, _ := unstructured.NestedInt64(
					list.Items[i].Object, "status", "generatedCount")
				if !found {
					continue // not yet reconciled
				}
				// Eventually: a child write and the parent's count update are
				// separate API calls, so they are briefly inconsistent.
				Eventually(func(g Gomega) {
					kids, err := dyn.Resource(d.childGVR).Namespace(ns).List(ctx,
						metav1.ListOptions{LabelSelector: generatedByLabel + "=" + parent})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(int64(len(kids.Items))).To(Equal(want),
						"%s/%s: status.generatedCount=%d but %d labelled children",
						d.kind, parent, want, len(kids.Items))
				}, 60*time.Second, 3*time.Second).Should(Succeed())
			}
		}
	})

	// UAT-R1. On restart the operator re-lists every CR and re-reconciles it.
	// Those reconciles must be no-ops for unchanged specs: if a reconcile
	// rewrites Ready, lastTransitionTime moves, which fires "condition changed"
	// alerts and destroys the ability to age a condition. This is exactly the
	// class of bug #102 was — a stale Ready that nothing rewrote — in reverse.
	It("UAT-R1 restart idempotency: Ready lastTransitionTime survives an operator restart", func() {
		before := map[string]string{}
		list, err := dyn.Resource(modelGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for i := range list.Items {
			obj := &list.Items[i]
			if status, _, lt := readyCond(obj); status == "True" && lt != "" {
				before[obj.GetName()] = lt
			}
		}
		Expect(before).NotTo(BeEmpty(), "no Ready Models to observe across a restart")

		out, err := exec.Command("kubectl", "-n", ns,
			"rollout", "restart", "deploy/alitellm-operator").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))
		// Bounded wait — never a naked poll loop.
		out, err = exec.Command("kubectl", "-n", ns, "rollout", "status",
			"deploy/alitellm-operator", "--timeout=180s").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))

		// Give the fresh manager a full re-list + reconcile pass to do damage.
		time.Sleep(20 * time.Second)

		Consistently(func(g Gomega) {
			for name, wantLT := range before {
				obj, err := dyn.Resource(modelGVR).Namespace(ns).
					Get(ctx, name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				status, _, gotLT := readyCond(obj)
				g.Expect(status).To(Equal("True"), "%s went not-Ready after restart", name)
				g.Expect(gotLT).To(Equal(wantLT),
					"%s Ready flapped across restart: %s -> %s", name, wantLT, gotLT)
			}
		}, 15*time.Second, 5*time.Second).Should(Succeed())
	})
})
