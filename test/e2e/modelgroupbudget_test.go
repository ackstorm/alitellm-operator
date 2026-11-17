//go:build e2e

// SPDX-License-Identifier: MIT

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var modelGroupBudgetGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmodelgroupbudgets",
}

func newModelGroupBudgetCR(name, ns string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMModelGroupBudget",
			"metadata":   map[string]interface{}{"name": name, "namespace": ns},
			"spec":       spec,
		},
	}
}

// mgbBudget is the part of a stored tag budget the specs assert on.
type mgbBudget struct {
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
}

// litellmModelGroupBudget reads GET /access_group/<tag>/budget. budget is nil
// when LiteLLM holds none (200 budget:null).
func litellmModelGroupBudget(tag string) *mgbBudget {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "mgb-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/access_group/"+url.PathEscape(tag)+"/budget",
	)
	var resp struct {
		Budget *mgbBudget `json:"budget"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	return resp.Budget
}

// litellmTeamModels reads team_info.models from GET /team/info.
func litellmTeamModels(teamID string) []string {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "team-models-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/team/info?team_id="+url.QueryEscape(teamID),
	)
	var resp struct {
		TeamInfo struct {
			Models []string `json:"models"`
		} `json:"team_info"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	return resp.TeamInfo.Models
}

// litellmModelsForKey lists GET /v1/models ids as a virtual key sees them.
func litellmModelsForKey(key string) []string {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "key-models-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer "+key,
		litellmBase+"/v1/models",
	)
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	ids := make([]string, 0, len(resp.Data))
	for _, d := range resp.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

// envtest counterpart: internal/controller/modelgroupbudget_controller_test.go
// covers the reconcile state machine against an in-process mock. This suite
// proves the real LiteLLM stores the budget, CHARGES team traffic to it, and
// that team.models mirrors the attached group's tags (the attribution rule:
// LiteLLM charges a tag budget only when the tag is literally in the key's or
// team's models).
var _ = Describe("LiteLLMModelGroupBudget", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	// tag is unique per run: LiteLLM keeps a tag's spend across budget
	// DELETE/re-PUT, so on the KEPT cluster a fixed tag starts the next run
	// already over budget and BUDGET-02's first call is refused.
	tag := fmt.Sprintf("e2e-budget-tag-%d", time.Now().Unix())
	const (
		modelName = "e2e-budget-model"
		agName    = "e2e-budget-group"
		teamName  = "e2e-budget-team"
		budgetCR  = "e2e-budget"
	)
	fg := metav1.DeletePropagationForeground

	deleteAndWait := func(gvr schema.GroupVersionResource, name string) {
		_ = dyn.Resource(gvr).Namespace(ns).
			Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &fg})
		Eventually(func() bool {
			_, err := dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 60*time.Second, 2*time.Second).Should(BeTrue(),
			"%s/%s never finished deleting", gvr.Resource, name)
	}
	create := func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
		GinkgoHelper()
		name := obj.GetName()
		deleteAndWait(gvr, name)
		_, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAndWait(gvr, name) })
	}
	// budgetModel is the mock-backed model carrying the tag. Per-token prices
	// make each call cost $2 (the mock reports 1 prompt + 1 completion token).
	budgetModel := func(priced bool) *unstructured.Unstructured {
		m := newOpenAIMockModel(modelName, ns)
		spec := m.Object["spec"].(map[string]interface{})
		spec["info"] = map[string]interface{}{"access_groups": []interface{}{tag}}
		if priced {
			p := spec["params"].(map[string]interface{})
			p["input_cost_per_token"] = 1.0
			p["output_cost_per_token"] = 1.0
		}
		return m
	}
	waitModelRegistered := func() {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "model not registered yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())
	}
	waitBudgetSynced := func() {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGroupBudgetGVR).Namespace(ns).Get(ctx, budgetCR, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			var reason, status string
			for _, c := range conds {
				if cm, ok := c.(map[string]interface{}); ok && cm["type"] == "Ready" {
					reason, _ = cm["reason"].(string)
					status, _ = cm["status"].(string)
				}
			}
			g.Expect(status).To(Equal("True"), "Ready reason=%s", reason)
			g.Expect(reason).To(Equal("Synced"))
		}, 90*time.Second, 3*time.Second).Should(Succeed())
	}
	budgetSpec := func() map[string]interface{} {
		return map[string]interface{}{"modelGroup": tag, "limit": 1.0, "period": "30d"}
	}

	It("BUDGET-01 CR lifecycle: projects the budget and clears it on delete", func() {
		create(modelGVR, budgetModel(false))
		waitModelRegistered()
		create(modelGroupBudgetGVR, newModelGroupBudgetCR(budgetCR, ns, budgetSpec()))
		waitBudgetSynced()

		Eventually(func(g Gomega) {
			b := litellmModelGroupBudget(tag)
			g.Expect(b).NotTo(BeNil(), "budget not stored")
			g.Expect(b.MaxBudget).NotTo(BeNil())
			g.Expect(*b.MaxBudget).To(BeNumerically("==", 1))
			g.Expect(b.BudgetDuration).To(Equal("30d"))
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		deleteAndWait(modelGroupBudgetGVR, budgetCR)
		Eventually(func() *mgbBudget {
			return litellmModelGroupBudget(tag)
		}, 60*time.Second, 3*time.Second).Should(BeNil(), "budget must be removed from LiteLLM")
	})

	It("BUDGET-02 team traffic is charged against the tag budget", func() {
		create(modelGVR, budgetModel(true))
		waitModelRegistered()
		create(accessGroupGVR, newAccessGroupCR(agName, ns, map[string]interface{}{
			"modelGroups": []interface{}{tag},
		}))
		create(modelGroupBudgetGVR, newModelGroupBudgetCR(budgetCR, ns, budgetSpec()))
		waitBudgetSynced()

		cr := newTeamCR(teamName, ns)
		cr.Object["spec"].(map[string]interface{})["accessGroups"] = []interface{}{agName}
		create(teamGVR, cr)

		var tid string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			tid = teamID(obj)
			g.Expect(tid).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// The mirror is what makes LiteLLM charge the tag budget.
		Eventually(func() []string { return litellmTeamModels(tid) },
			60*time.Second, 3*time.Second).Should(ConsistOf("no-default-models", tag))

		key := generateTeamKey(tid)
		DeferCleanup(func() { deleteLiteLLMKey(key) })

		// The mirrored tag is also what puts the REAL model in the team's
		// catalog: LiteLLM expands a tag in team.models into its models, but
		// lists a tag reached through access_group_ids verbatim (measured on
		// 1.106.0, so the tag itself still shows up as a phantom entry).
		Eventually(func() []string { return litellmModelsForKey(key) },
			90*time.Second, 5*time.Second).Should(ContainElement(modelName))

		complete := func() string {
			return string(curlPodJSON("litellm-system", "budget-complete", '{',
				"curl", "-sS", "--max-time", "20",
				"-w", "\nHTTP_STATUS:%{http_code}",
				"-X", "POST",
				"-H", "Authorization: Bearer "+key,
				"-H", "Content-Type: application/json",
				"-d", `{"model":"`+modelName+`","messages":[{"role":"user","content":"hi"}]}`,
				litellmBase+"/v1/chat/completions",
			))
		}
		// First call is under budget (LiteLLM may briefly 401 a fresh key).
		Eventually(complete, 90*time.Second, 5*time.Second).Should(
			ContainSubstring("E2E-MOCK-COMPLETION-OK"))
		// Spend is recorded asynchronously; the cap trips on a later call.
		Eventually(complete, 90*time.Second, 5*time.Second).Should(
			ContainSubstring("Model access group="+tag),
			"budget never tripped for tag %s", tag)
	})

	// TEAM-MIRROR-01 is independent of spend: no model, key or budget needed.
	It("TEAM-MIRROR-01 revocation: emptying the group's modelGroups drops the mirrored tag", func() {
		const (
			mTag  = "e2e-mirror-tag"
			mAG   = "e2e-mirror-group"
			mTeam = "e2e-mirror-team"
		)
		create(accessGroupGVR, newAccessGroupCR(mAG, ns, map[string]interface{}{
			"modelGroups": []interface{}{mTag},
		}))
		cr := newTeamCR(mTeam, ns)
		cr.Object["spec"].(map[string]interface{})["accessGroups"] = []interface{}{mAG}
		create(teamGVR, cr)

		var tid string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, mTeam, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			tid = teamID(obj)
			g.Expect(tid).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
		Eventually(func() []string { return litellmTeamModels(tid) },
			60*time.Second, 3*time.Second).Should(ConsistOf("no-default-models", mTag))

		obj, err := dyn.Resource(accessGroupGVR).Namespace(ns).Get(ctx, mAG, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedSlice(obj.Object, []interface{}{}, "spec", "modelGroups")).To(Succeed())
		_, err = dyn.Resource(accessGroupGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []string { return litellmTeamModels(tid) },
			30*time.Second, 2*time.Second).Should(ConsistOf("no-default-models"))
	})
})
