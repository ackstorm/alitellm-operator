//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var accessGroupGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmaccessgroups",
}

func newAccessGroupCR(name, ns string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMAccessGroup",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": spec,
		},
	}
}

func accessGroupID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "accessGroupID")
	return id
}

// litellmAccessGroup is one row of GET /v1/access_group.
//
// AssignedTeamIDs is decoded for documentation only — NEVER assert on it to
// verify a team attachment. A team-side write to team.access_group_ids does NOT
// propagate here (measured on 1.93.0: the group keeps reading []), which is
// exactly why the operator writes only the team mirror. Read
// litellmTeamAccessGroupIDs instead.
type litellmAccessGroup struct {
	AccessGroupID      string   `json:"access_group_id"`
	AccessGroupName    string   `json:"access_group_name"`
	Description        string   `json:"description"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
	AssignedTeamIDs    []string `json:"assigned_team_ids"`
}

// litellmAccessGroupByName reads GET /v1/access_group and returns the entry
// whose access_group_name matches, plus whether it was found.
//
// The endpoint returns a BARE ARRAY, so the JSON marker is '[' — passing '{'
// (the object marker used by /v2/team/list) would never match and the helper
// would spin until its retry budget ran out.
func litellmAccessGroupByName(name string) (litellmAccessGroup, bool) {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "ag-list-poke", '[',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/v1/access_group",
	)
	var all []litellmAccessGroup
	Expect(json.Unmarshal(body, &all)).To(Succeed(), "raw=%s", string(body))
	for _, g := range all {
		if g.AccessGroupName == name {
			return g, true
		}
	}
	return litellmAccessGroup{}, false
}

// litellmTeamAccessGroupIDs reads the team's attached access-group ids from
// GET /team/info (team_info.access_group_ids).
//
// This is the ONLY reliable read of an attachment: the group side's
// assigned_team_ids stays [] after a team-side write, and /v2/team/list does
// not expand object_permission. Verified on LiteLLM 1.93.0.
func litellmTeamAccessGroupIDs(teamID string) []string {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "team-ag-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/team/info?team_id="+url.QueryEscape(teamID),
	)
	var resp struct {
		TeamInfo struct {
			AccessGroupIDs []string `json:"access_group_ids"`
		} `json:"team_info"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	return resp.TeamInfo.AccessGroupIDs
}

// envtest counterpart: internal/controller/accessgroup_controller_test.go
// covers the reconcile state machine (CREATE/UPDATE arms, 409 adoption,
// name-resolution parking, vanish probe, finalizer) against an in-process mock
// LiteLLM. This suite proves the Helm-deployed operator round-trips a real
// LiteLLMAccessGroup CR through the real /v1/access_group endpoints, and that
// the security consequence of an attachment (groups only ADD) is what the docs
// claim it is.
var _ = Describe("LiteLLMAccessGroup", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"

	fg := metav1.DeletePropagationForeground
	// deleteAndWait is fire-and-WAIT: a bare Delete returns while the finalizer
	// is still draining, and the next Create then fails "object is being
	// deleted: <name> already exists". Every pre-clean and DeferCleanup below
	// goes through this.
	deleteAndWait := func(gvr schema.GroupVersionResource, name string) {
		c := context.Background()
		_ = dyn.Resource(gvr).Namespace(ns).
			Delete(c, name, metav1.DeleteOptions{PropagationPolicy: &fg})
		Eventually(func() bool {
			_, err := dyn.Resource(gvr).Namespace(ns).Get(c, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 60*time.Second, 2*time.Second).Should(BeTrue(),
			"%s/%s never finished deleting", gvr.Resource, name)
	}
	deleteAG := func(name string) { deleteAndWait(accessGroupGVR, name) }
	deleteTeam := func(name string) { deleteAndWait(teamGVR, name) }

	// waitAGSynced blocks until the CR carries an accessGroupID and returns it.
	waitAGSynced := func(name string) string {
		GinkgoHelper()
		var id string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(accessGroupGVR).Namespace(ns).
				Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			id = accessGroupID(obj)
			g.Expect(id).NotTo(BeEmpty(), "accessGroupID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
		return id
	}

	waitTeamID := func(name string) string {
		GinkgoHelper()
		var tid string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			tid = teamID(obj)
			g.Expect(tid).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
		return tid
	}

	// AG-01: the full CRUD round-trip against the real endpoints —
	// POST /v1/access_group (201, server-minted id), PUT on a spec change,
	// DELETE on finalizer drain.
	It("AG-01 CRUD: creates, updates and deletes the LiteLLM access group", func() {
		const agName = "e2e-ag-crud"
		deleteAG(agName)

		_, err := dyn.Resource(accessGroupGVR).Namespace(ns).Create(ctx,
			newAccessGroupCR(agName, ns, map[string]interface{}{
				"description": "e2e CRUD group",
				"models":      []interface{}{"e2e-ag-model-a"},
			}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAG(agName) })

		wantID := waitAGSynced(agName)

		By("the rendered group is live in LiteLLM under metadata.name")
		Eventually(func(g Gomega) {
			grp, found := litellmAccessGroupByName(agName)
			g.Expect(found).To(BeTrue(), "group %q absent from GET /v1/access_group", agName)
			// access_group_id is SERVER-minted — the CR must carry back exactly
			// what LiteLLM assigned, never a value derived from metadata.name.
			g.Expect(grp.AccessGroupID).To(Equal(wantID))
			g.Expect(grp.Description).To(Equal("e2e CRUD group"))
			g.Expect(grp.AccessModelNames).To(ConsistOf("e2e-ag-model-a"))
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		By("patching spec.models lands upstream via PUT")
		obj, err := dyn.Resource(accessGroupGVR).Namespace(ns).
			Get(ctx, agName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedStringSlice(obj.Object,
			[]string{"e2e-ag-model-a", "e2e-ag-model-b"}, "spec", "models")).To(Succeed())
		_, err = dyn.Resource(accessGroupGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			grp, found := litellmAccessGroupByName(agName)
			g.Expect(found).To(BeTrue())
			g.Expect(grp.AccessModelNames).To(ConsistOf("e2e-ag-model-a", "e2e-ag-model-b"))
			// The id must be STABLE across the update — an UPDATE that
			// recreated the row would churn every attached team's grant.
			g.Expect(grp.AccessGroupID).To(Equal(wantID))
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		By("deleting the CR removes the group from LiteLLM")
		Expect(dyn.Resource(accessGroupGVR).Namespace(ns).
			Delete(ctx, agName, metav1.DeleteOptions{PropagationPolicy: &fg})).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(accessGroupGVR).Namespace(ns).
				Get(ctx, agName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "CR still present (err=%v)", err)
			_, found := litellmAccessGroupByName(agName)
			g.Expect(found).To(BeFalse(), "LiteLLM still lists access group %q", agName)
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// AG-02: the omit-vs-clear regression guard.
	//
	// PUT /v1/access_group/<id> merges per field: an OMITTED list KEEPS the
	// stored value, `[]` CLEARS it (measured on 1.93.0). The three managed
	// lists therefore carry no `omitempty` in AccessGroupUpdateRequest. If that
	// ever regresses, shrinking a grant to empty silently fails to revoke —
	// which is a security bug, not a cosmetic one. This spec removes
	// spec.models entirely (the sharpest form: the reconciler sees a nil slice
	// and must still emit `[]`).
	It("AG-02 revocation: emptying spec.models clears the grant upstream", func() {
		const agName = "e2e-ag-revoke"
		deleteAG(agName)

		_, err := dyn.Resource(accessGroupGVR).Namespace(ns).Create(ctx,
			newAccessGroupCR(agName, ns, map[string]interface{}{
				"models": []interface{}{"e2e-ag-revoke-model"},
			}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAG(agName) })

		waitAGSynced(agName)
		Eventually(func(g Gomega) {
			grp, found := litellmAccessGroupByName(agName)
			g.Expect(found).To(BeTrue())
			g.Expect(grp.AccessModelNames).To(ConsistOf("e2e-ag-revoke-model"))
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		By("removing spec.models entirely")
		obj, err := dyn.Resource(accessGroupGVR).Namespace(ns).
			Get(ctx, agName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		unstructured.RemoveNestedField(obj.Object, "spec", "models")
		_, err = dyn.Resource(accessGroupGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			grp, found := litellmAccessGroupByName(agName)
			g.Expect(found).To(BeTrue(), "group vanished instead of being cleared")
			g.Expect(grp.AccessModelNames).To(BeEmpty(),
				"REVOCATION LEAK: access_model_names still %v — the PUT omitted the list instead of sending []",
				grp.AccessModelNames)
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// AG-03: spec.permission.accessGroups resolves NAMES to server-minted ids
	// and writes them to the team's top-level access_group_ids.
	It("AG-03 attachment: a team reaches the group by name, read from /team/info", func() {
		const agName = "e2e-ag-attach-group"
		const teamName = "e2e-ag-attach-team"
		deleteAG(agName)
		deleteTeam(teamName)

		_, err := dyn.Resource(accessGroupGVR).Namespace(ns).Create(ctx,
			newAccessGroupCR(agName, ns, map[string]interface{}{
				"models": []interface{}{"e2e-ag-attach-model"},
			}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAG(agName) })
		wantID := waitAGSynced(agName)

		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["permission"] = map[string]interface{}{
			"accessGroups": []interface{}{agName},
		}
		_, err = dyn.Resource(teamGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteTeam(teamName) })
		tid := waitTeamID(teamName)

		// Read the TEAM mirror, never the group side: a team-side write does
		// not propagate to access_group.assigned_team_ids, so an assertion
		// there can never pass (measured 1.93.0).
		Eventually(func(g Gomega) {
			g.Expect(litellmTeamAccessGroupIDs(tid)).To(ConsistOf(wantID),
				"team.access_group_ids must carry the resolved UUID %q, not the name %q", wantID, agName)
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// AG-04: the security headline. An attached group only ADDS — it OVERRIDES
	// the attaching team's deny-by-default sentinel. This is LiteLLM semantics
	// colliding with this repo's fail-closed posture, and it is DOCUMENTED, not
	// fixed (docs/user-guide/access-group.md). The spec exists so a future
	// change to the sentinel or to projectPermission cannot quietly alter this
	// blast radius without a red test.
	//
	// ASSERT BY ERROR TYPE, NEVER BY STATUS CODE. LiteLLM drifted the denial
	// status 401→403 across 1.83.10→1.93.0 for the identical condition; the
	// stable contract is `team_model_access_denied` + the sentinel echo. The
	// "allowed" half therefore asserts the ABSENCE of those markers rather than
	// a success code, so it stays honest whatever the upstream provider does.
	It("AG-04 bypass is real: an attached group overrides the deny-all sentinel", func() {
		const agName = "e2e-ag-bypass-group"
		const teamName = "e2e-ag-bypass-team"
		const modelName = "e2e-ag-bypass-model"
		deleteAG(agName)
		deleteTeam(teamName)
		_ = dyn.Resource(modelGVR).Namespace(ns).Delete(ctx, modelName, metav1.DeleteOptions{})

		// Own the model: Ginkgo randomizes top-level containers, so this spec
		// cannot borrow one another suite happens to have created.
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "bypass model still terminating")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(modelName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(modelGVR).Namespace(ns).
				Delete(context.Background(), modelName, metav1.DeleteOptions{})
		})
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "bypass model not registered yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		By("baseline: a present-but-empty spec.permission denies the model")
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["permission"] = map[string]interface{}{}
		_, err = dyn.Resource(teamGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteTeam(teamName) })
		tid := waitTeamID(teamName)

		denyKey := generateTeamKey(tid)
		DeferCleanup(func() { deleteLiteLLMKey(denyKey) })
		Eventually(func(g Gomega) {
			out := string(curlPodJSON("litellm-system", "ag-deny-complete", '{',
				"curl", "-sS", "--max-time", "20", "-X", "POST",
				"-H", "Authorization: Bearer "+denyKey,
				"-H", "Content-Type: application/json",
				"-d", `{"model":"`+modelName+`","messages":[{"role":"user","content":"hi"}]}`,
				litellmBase+"/v1/chat/completions",
			))
			g.Expect(out).To(ContainSubstring("team_model_access_denied"),
				"baseline denial must cite team_model_access_denied, got: %s", out)
			g.Expect(out).To(ContainSubstring("__deny_all__"),
				"baseline denial must echo the deny-all sentinel, got: %s", out)
		}, 90*time.Second, 5*time.Second).Should(Succeed())

		By("attaching a group that grants the model")
		_, err = dyn.Resource(accessGroupGVR).Namespace(ns).Create(ctx,
			newAccessGroupCR(agName, ns, map[string]interface{}{
				"models": []interface{}{modelName},
			}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAG(agName) })
		wantID := waitAGSynced(agName)

		obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedStringSlice(obj.Object,
			[]string{agName}, "spec", "permission", "accessGroups")).To(Succeed())
		_, err = dyn.Resource(teamGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(litellmTeamAccessGroupIDs(tid)).To(ConsistOf(wantID))
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		By("the SAME team's key is no longer denied — the sentinel is bypassed")
		// A fresh key removes LiteLLM's key-object cache from the equation; the
		// principal under test is the TEAM, whose models list is unchanged and
		// still `["__deny_all__"]`.
		allowKey := generateTeamKey(tid)
		DeferCleanup(func() { deleteLiteLLMKey(allowKey) })
		Eventually(func(g Gomega) {
			out := string(curlPodJSON("litellm-system", "ag-allow-complete", '{',
				"curl", "-sS", "--max-time", "20", "-X", "POST",
				"-H", "Authorization: Bearer "+allowKey,
				"-H", "Content-Type: application/json",
				"-d", `{"model":"`+modelName+`","messages":[{"role":"user","content":"hi"}]}`,
				litellmBase+"/v1/chat/completions",
			))
			g.Expect(out).NotTo(ContainSubstring("team_model_access_denied"),
				"group grant did not override the deny-all sentinel, got: %s", out)
			g.Expect(out).NotTo(ContainSubstring("__deny_all__"),
				"group grant did not override the deny-all sentinel, got: %s", out)
		}, 120*time.Second, 5*time.Second).Should(Succeed())

		// Sanity on the OTHER direction: the team's own models list was never
		// widened. The bypass is LiteLLM's additive group semantics, not the
		// operator quietly dropping the sentinel.
		matches := litellmTeamsByAlias(teamName)
		Expect(matches).NotTo(BeEmpty())
		Expect(matches[0]["models"]).To(ConsistOf("__deny_all__"),
			"team.models must still carry the sentinel — the widening comes from the group, got %v",
			matches[0]["models"])
	})

	// AG-05: ordering dependency. Same shape as AgentNotFound / ToolsetNotFound
	// — park rather than under-grant silently, and self-heal once the CR lands.
	It("AG-05 ordering: a team referencing a missing group parks, then self-heals", func() {
		const agName = "e2e-ag-order-group"
		const teamName = "e2e-ag-order-team"
		deleteAG(agName)
		deleteTeam(teamName)

		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["permission"] = map[string]interface{}{
			"accessGroups": []interface{}{agName},
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteTeam(teamName) })

		By("the team parks Ready=False reason=AccessGroupNotFound")
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			status, reason, found := readyCondition(obj)
			g.Expect(found).To(BeTrue(), "no Ready condition yet")
			g.Expect(status).To(Equal("False"))
			g.Expect(reason).To(Equal("AccessGroupNotFound"))
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		By("creating the LiteLLMAccessGroup heals it")
		_, err = dyn.Resource(accessGroupGVR).Namespace(ns).Create(ctx,
			newAccessGroupCR(agName, ns, map[string]interface{}{
				"models": []interface{}{"e2e-ag-order-model"},
			}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { deleteAG(agName) })
		wantID := waitAGSynced(agName)

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			status, reason, found := readyCondition(obj)
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("True"), "team still parked (reason=%s)", reason)
			g.Expect(reason).To(Equal("Synced"))
			tid := teamID(obj)
			g.Expect(tid).NotTo(BeEmpty())
			g.Expect(litellmTeamAccessGroupIDs(tid)).To(ConsistOf(wantID))
		}, 120*time.Second, 3*time.Second).Should(Succeed())
	})
})
