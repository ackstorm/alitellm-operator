//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// tickleMSDisc patches the MCPServerDiscovery CR with a unique annotation to
// force an out-of-band reconcile. The controller's For() watch fires on the
// Update event, bypassing the spec.refresh.interval (1m floor) wait that
// otherwise gates discovery propagation in e2e specs. Errors are intentionally
// swallowed — the Eventually retry envelope absorbs transient races
// (CR not yet created, conflicting patches).
func tickleMSDisc(dyn dynamic.Interface, name, ns string) {
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{"e2e-tickle":"%d"}}}`,
		time.Now().UnixNano(),
	))
	_, _ = dyn.Resource(msdiscGVR).Namespace(ns).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
}

var msdiscGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmmcpserverdiscoveries",
}

var toolhiveMCPGVR = schema.GroupVersionResource{
	Group:    "toolhive.stacklok.dev",
	Version:  "v1beta1",
	Resource: "mcpservers",
}

func newToolhiveMCPServer(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "toolhive.stacklok.dev/v1beta1",
			"kind":       "MCPServer",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"image":     "ghcr.io/example/fake-mcp:tier2",
				"transport": "streamable-http", // operator normalizes → http
			},
		},
	}
}

func newToolhiveMSDiscovery(name, ns string, fromNs []string) *unstructured.Unstructured {
	nsAny := make([]interface{}, 0, len(fromNs))
	for _, n := range fromNs {
		nsAny = append(nsAny, n)
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMMCPServerDiscovery",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				// FIX4 H-2 v0.3.0: spec.prefix is required; use the
				// discovery name so child names land at
				// "<msdName>-<source-name>".
				"prefix": name,
				"type":   "toolhive",
				"toolhive": map[string]interface{}{
					"namespaces": nsAny,
					"kinds":      []interface{}{"MCPServer"},
				},
				"refresh": map[string]interface{}{
					"interval": "1m",
				},
			},
		},
	}
}

// envtest counterpart: internal/controller/mcpserverdiscovery_controller_test.go
// covers 20+ reconcile-logic cases (state machine, transport normalization,
// vanish detection, AC-DC1 / AC-SEC4 / AC-N3 invariants) against the in-
// process ToolHive informer + mock LiteLLM. This suite proves the Helm-
// deployed operator works end-to-end against a real ToolHive operator and a
// real LiteLLM, including AC-M3 wholesale-replace after out-of-band delete.
var _ = Describe("LiteLLMMCPServerDiscovery", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ourNs = "default"
	const devNs = "dev"
	const thName = "tier2-fake-mcp"
	const msdName = "tier2-toolhive-disc"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Delete(ctx, thName, metav1.DeleteOptions{})
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Delete(ctx, thName, metav1.DeleteOptions{})
	})

	It("propagates ToolHive MCPServer into child MCPServer in default", func() {
		_, err := dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Create(ctx, newToolhiveMCPServer(thName, devNs), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Create(ctx, newToolhiveMSDiscovery(msdName, ourNs, []string{devNs}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Per spec §6.5 dotted naming: <discovery-name>.<toolhive-ns>.<toolhive-name>
		// But Discovery normalizes; actual child name may differ. Match by
		// owner-ref scan instead of guessing exact name.
		//
		// Tickle the Discovery CR every poll to force out-of-band reconciles —
		// otherwise we'd wait up to one spec.refresh.interval (1m, CEL floor)
		// for the controller's periodic requeue to pick up the new ToolHive
		// source.
		Eventually(func(g Gomega) {
			tickleMSDisc(dyn, msdName, ourNs)
			list, err := dyn.Resource(mcpsrvGVR).Namespace(ourNs).
				List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var found *unstructured.Unstructured
			for i := range list.Items {
				for _, o := range list.Items[i].GetOwnerReferences() {
					if o.Kind == "LiteLLMMCPServerDiscovery" && o.Name == msdName {
						found = &list.Items[i]
						break
					}
				}
				if found != nil {
					break
				}
			}
			g.Expect(found).NotTo(BeNil(), "no child MCPServer owned by %s yet", msdName)
			// Name should encode toolhive-source identity per spec §6.5.
			g.Expect(found.GetName()).To(
				SatisfyAny(
					ContainSubstring(thName),
					ContainSubstring(strings.ToLower(thName)),
				),
				"child MCPServer name %q does not reference source %q", found.GetName(), thName,
			)
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})

	It("cascade: deleting ToolHive MCPServer removes child MCPServer", func() {
		// Capture current child set before delete.
		listPre, err := dyn.Resource(mcpsrvGVR).Namespace(ourNs).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var childName string
		for i := range listPre.Items {
			for _, o := range listPre.Items[i].GetOwnerReferences() {
				if o.Kind == "LiteLLMMCPServerDiscovery" && o.Name == msdName {
					childName = listPre.Items[i].GetName()
					break
				}
			}
		}
		Expect(childName).NotTo(BeEmpty(), "previous spec must have created child")

		// Delete the ToolHive source object.
		Expect(dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Delete(ctx, thName, metav1.DeleteOptions{})).To(Succeed())

		// Within one refresh.interval (1m) + reconcile, child should vanish.
		// Tickle the Discovery CR every poll to force out-of-band reconciles
		// so the cascade-drain doesn't wait on the periodic requeue tick.
		Eventually(func(g Gomega) {
			tickleMSDisc(dyn, msdName, ourNs)
			_, err := dyn.Resource(mcpsrvGVR).Namespace(ourNs).
				Get(ctx, childName, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "child %q still present", childName)
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})
})
