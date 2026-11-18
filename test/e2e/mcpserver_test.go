//go:build e2e

// SPDX-License-Identifier: MIT

package e2e_test

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// litellmMCPServer is one row of GET /v1/mcp/server (a BARE array). LiteLLM
// builds the row from its in-memory registry, i.e. the values it enforces.
type litellmMCPServer struct {
	ServerID        string         `json:"server_id"`
	CreatedAt       string         `json:"created_at"`
	MCPAccessGroups []string       `json:"mcp_access_groups"`
	AllowedTools    []string       `json:"allowed_tools"`
	StaticHeaders   map[string]any `json:"static_headers"`
}

// litellmMCPServerByID runs an in-cluster curl against GET /v1/mcp/server and
// returns the row whose server_id matches, plus whether it was found. The
// endpoint returns a bare array, so the JSON marker is '['.
func litellmMCPServerByID(id string) (litellmMCPServer, bool) {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "mcp-list-poke", '[',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/v1/mcp/server",
	)
	var all []litellmMCPServer
	Expect(json.Unmarshal(body, &all)).To(Succeed(), "raw=%s", string(body))
	for _, s := range all {
		if s.ServerID == id {
			return s, true
		}
	}
	return litellmMCPServer{}, false
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
				"kind":       "LiteLLMMCPServer",
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

	// LiteLLM applies PUT /v1/mcp/server as a partial update: a field left out
	// of the body keeps its stored value. The operator therefore sends the
	// list and map params empty when the spec does not set them. This spec
	// removes spec.params entirely (the reconciler sees no params at all and
	// must still clear all three) and reads the row back from LiteLLM.
	It("param removal: static_headers, access_groups and allowed_tools are cleared upstream", func() {
		const crName = "e2e-mcp-clear-params"
		fg := metav1.DeletePropagationForeground
		// Fire-and-wait: a bare Delete returns while the finalizer is still
		// draining, and the Create below would then hit "object is being deleted".
		deleteCR := func() {
			c := context.Background()
			_ = dyn.Resource(mcpsrvGVR).Namespace(ns).
				Delete(c, crName, metav1.DeleteOptions{PropagationPolicy: &fg})
			Eventually(func() bool {
				_, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Get(c, crName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 60*time.Second, 2*time.Second).Should(BeTrue(), "%s never finished deleting", crName)
		}
		// waitSynced blocks until the CR is Ready=Synced for generation gen: a
		// Synced condition left over from the previous generation does not count.
		waitSynced := func(gen int64) *unstructured.Unstructured {
			GinkgoHelper()
			var obj *unstructured.Unstructured
			Eventually(func(g Gomega) {
				var err error
				obj, err = dyn.Resource(mcpsrvGVR).Namespace(ns).
					Get(ctx, crName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
				g.Expect(observed).To(Equal(gen), "status not yet at generation %d", gen)
				status, reason, _ := readyCond(obj)
				g.Expect(status).To(Equal("True"))
				g.Expect(reason).To(Equal("Synced"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())
			return obj
		}

		deleteCR()
		created, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Create(ctx, &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "litellm.ackstorm.ai/v1alpha1",
				"kind":       "LiteLLMMCPServer",
				"metadata": map[string]interface{}{
					"name":      crName,
					"namespace": ns,
				},
				"spec": map[string]interface{}{
					"endpoint":  "http://example.invalid/mcp",
					"transport": "http",
					"params": map[string]interface{}{
						"static_headers": map[string]interface{}{"x-e2e-clear": "1"},
						"access_groups":  []interface{}{"e2e-clear-group"},
						"allowed_tools":  []interface{}{"e2e_clear_tool"},
					},
				},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(deleteCR)

		serverID := mcpServerID(waitSynced(created.GetGeneration()))
		Expect(serverID).NotTo(BeEmpty())
		var createdAt string
		Eventually(func(g Gomega) {
			row, found := litellmMCPServerByID(serverID)
			g.Expect(found).To(BeTrue())
			g.Expect(row.StaticHeaders).To(HaveKey("x-e2e-clear"))
			g.Expect(row.MCPAccessGroups).To(ConsistOf("e2e-clear-group"))
			g.Expect(row.AllowedTools).To(ConsistOf("e2e_clear_tool"))
			g.Expect(row.CreatedAt).NotTo(BeEmpty())
			createdAt = row.CreatedAt
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		By("removing spec.params entirely")
		obj, err := dyn.Resource(mcpsrvGVR).Namespace(ns).
			Get(ctx, crName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		unstructured.RemoveNestedField(obj.Object, "spec", "params")
		updated, err := dyn.Resource(mcpsrvGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(mcpServerID(waitSynced(updated.GetGeneration()))).To(Equal(serverID),
			"serverID changed across the update")
		Eventually(func(g Gomega) {
			row, found := litellmMCPServerByID(serverID)
			g.Expect(found).To(BeTrue(), "server vanished instead of being cleared")
			g.Expect(row.StaticHeaders).To(BeEmpty(),
				"static_headers still %v — the PUT left the field out instead of sending {}", row.StaticHeaders)
			g.Expect(row.MCPAccessGroups).To(BeEmpty(),
				"REVOCATION LEAK: mcp_access_groups still %v — the PUT left the list out instead of sending []",
				row.MCPAccessGroups)
			g.Expect(row.AllowedTools).To(BeEmpty(),
				"allowed_tools still %v — the PUT left the list out instead of sending []", row.AllowedTools)
			// created_at survives an update; a delete + create would mint a new one
			// under the same server_id, which is pinned to the CR name.
			g.Expect(row.CreatedAt).To(Equal(createdAt), "server was recreated, not updated")
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})
})
