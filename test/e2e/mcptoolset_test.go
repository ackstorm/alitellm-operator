//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var mcpToolsetGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmcptoolsets",
}

func toolsetID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "toolsetID")
	return id
}

// litellmToolset is one row of GET /v1/mcp/toolset (a BARE array).
type litellmToolset struct {
	ToolsetID   string `json:"toolset_id"`
	ToolsetName string `json:"toolset_name"`
	Tools       []struct {
		ServerID string `json:"server_id"`
		ToolName string `json:"tool_name"`
	} `json:"tools"`
}

// litellmToolsetByName runs an in-cluster curl against GET /v1/mcp/toolset and
// returns the entry whose toolset_name matches, plus whether it was found.
//
// The endpoint returns a BARE ARRAY, so the JSON marker is '[' — passing '{'
// (the object marker used by /v2/team/list) would never match and the helper
// would spin until its retry budget ran out.
func litellmToolsetByName(name string) (litellmToolset, bool) {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "toolset-list-poke", '[',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/v1/mcp/toolset",
	)
	var all []litellmToolset
	Expect(json.Unmarshal(body, &all)).To(Succeed(), "raw=%s", string(body))
	for _, ts := range all {
		if ts.ToolsetName == name {
			return ts, true
		}
	}
	return litellmToolset{}, false
}

func toolsetCR(ns, name string, from []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMMCPToolset",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"description": "e2e toolset",
				"from":        from,
			},
		},
	}
}

// envtest counterpart: internal/controller/mcptoolset_controller_test.go
// covers reconcile logic against an in-process mock. This suite proves the
// Helm-deployed operator round-trips a real toolset through the real LiteLLM
// /v1/mcp/toolset API (real persistence, real DNS, real server-minted id).
var _ = Describe("LiteLLMMCPToolset", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const name = "e2e-toolset"
	const srvName = "e2e-toolset-server"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(mcpsrvGVR).Namespace(ns).
			Delete(ctx, srvName, metav1.DeleteOptions{PropagationPolicy: &fg})

		// Deletion is async (the finalizer must drain), so a Create issued
		// straight after the Delete above races into AlreadyExists on a KEPT
		// cluster. Wait for both names to actually disappear first.
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Get(ctx, srvName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "backing MCPServer still terminating")
			_, err = dyn.Resource(mcpToolsetGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "toolset still terminating")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// A backing MCP server so spec.from[].server resolves to a real
		// server_id. The toolset does NOT need the server reachable — LiteLLM
		// stores the reference without contacting it.
		srv := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "litellm.ackstorm.ai/v1alpha1",
				"kind":       "LiteLLMMCPServer",
				"metadata": map[string]interface{}{
					"name":      srvName,
					"namespace": ns,
				},
				"spec": map[string]interface{}{
					"endpoint":  "http://example.invalid/mcp",
					"transport": "http",
				},
			},
		}
		_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Create(ctx, srv, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Get(ctx, srvName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mcpServerID(obj)).NotTo(BeEmpty(), "backing serverID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(mcpsrvGVR).Namespace(ns).
			Delete(ctx, srvName, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	It("TOOLSET-01 create: CR reaches Ready=Synced and appears in LiteLLM", func() {
		cr := toolsetCR(ns, name, []interface{}{
			map[string]interface{}{
				"server": srvName,
				"tools":  []interface{}{"alpha_tool", "beta_tool"},
			},
		})
		_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		var gotID string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			gotID = toolsetID(obj)
			g.Expect(gotID).NotTo(BeEmpty(), "toolsetID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// The id is SERVER-MINTED — it must not be metadata.name.
		Expect(gotID).NotTo(Equal(name),
			"toolset_id must come from the LiteLLM response, not metadata.name")

		Eventually(func(g Gomega) {
			ts, found := litellmToolsetByName(name)
			g.Expect(found).To(BeTrue(), "toolset not present in GET /v1/mcp/toolset")
			g.Expect(ts.ToolsetID).To(Equal(gotID), "id mismatch between CR status and LiteLLM")
			g.Expect(ts.Tools).To(HaveLen(2))
			names := []string{ts.Tools[0].ToolName, ts.Tools[1].ToolName}
			g.Expect(names).To(ConsistOf("alpha_tool", "beta_tool"))
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	It("TOOLSET-02 update: patching spec.from is reflected in LiteLLM", func() {
		patch := []byte(`{"spec":{"from":[{"server":"` + srvName + `","tools":["gamma_tool"]}]}}`)
		_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			ts, found := litellmToolsetByName(name)
			g.Expect(found).To(BeTrue())
			g.Expect(ts.Tools).To(HaveLen(1))
			g.Expect(ts.Tools[0].ToolName).To(Equal("gamma_tool"))
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// The revocation-leak guard. An emptied `tools` must arrive as an explicit
	// `[]`; an OMITTED field would leave LiteLLM's stale tool list in place.
	It("TOOLSET-03 revoke-to-empty: clearing spec.from empties tools in LiteLLM", func() {
		patch := []byte(`{"spec":{"from":[]}}`)
		_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			ts, found := litellmToolsetByName(name)
			g.Expect(found).To(BeTrue(), "toolset vanished; it should still exist with zero tools")
			g.Expect(ts.Tools).To(BeEmpty(),
				"tools NOT cleared — an omitted `tools` field leaves the stale grant in LiteLLM")
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// LiteLLM accepts a nonexistent server_id and tool_name with 201 and simply
	// grants nothing. Neither the operator nor LiteLLM validates them, so the CR
	// must go Ready=Synced rather than parking.
	It("TOOLSET-04 bogus refs are inert, not fatal", func() {
		const bogusName = "e2e-toolset-bogus"
		fg := metav1.DeletePropagationForeground
		defer func() {
			_ = dyn.Resource(mcpToolsetGVR).Namespace(ns).
				Delete(ctx, bogusName, metav1.DeleteOptions{PropagationPolicy: &fg})
		}()

		cr := toolsetCR(ns, bogusName, []interface{}{
			map[string]interface{}{
				"server": "no-such-server-anywhere",
				"tools":  []interface{}{"no_such_tool_anywhere"},
			},
		})
		_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).Get(ctx, bogusName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(toolsetID(obj)).NotTo(BeEmpty(),
				"a bogus server/tool ref must still produce a created toolset")
			conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			var reason string
			for _, c := range conds {
				cm, _ := c.(map[string]interface{})
				if t, _ := cm["type"].(string); t == "Ready" {
					reason, _ = cm["reason"].(string)
				}
			}
			g.Expect(reason).To(Equal("Synced"),
				"bogus refs must be inert, not fatal — got reason=%s", reason)
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("TOOLSET-05 delete: CR deletion removes the toolset from LiteLLM", func() {
		fg := metav1.DeletePropagationForeground
		err := dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "CR still present; finalizer never drained")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			_, found := litellmToolsetByName(name)
			g.Expect(found).To(BeFalse(), "toolset still present in LiteLLM after CR delete")
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})
})
