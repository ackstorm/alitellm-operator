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
	Version:  "v1alpha1",
	Resource: "mcpservers",
}

// toolhiveMCPGVRv1beta1 is the v1beta1 GVR for ToolHive MCPServer objects.
// The v1beta1 CRD version is not shipped by the published OCI chart; it is
// hydrated from test/e2e/fixtures/toolhive-v1beta1-crds.yaml by cluster.sh
// (Phase 9 Task 09-08).
var toolhiveMCPGVRv1beta1 = schema.GroupVersionResource{
	Group:    "toolhive.stacklok.dev",
	Version:  "v1beta1",
	Resource: "mcpservers",
}

func newToolhiveMCPServer(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "toolhive.stacklok.dev/v1alpha1",
			"kind": "MCPServer",
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

// newToolhiveMCPServerV1beta1 constructs a v1beta1 ToolHive MCPServer using
// the same spec fields as newToolhiveMCPServer. image and transport are
// present in both v1alpha1 and v1beta1 schemas (no breaking schema change
// between versions in v0.28.0 upstream source).
func newToolhiveMCPServerV1beta1(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "toolhive.stacklok.dev/v1beta1",
			"kind": "MCPServer",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"image":     "ghcr.io/example/fake-mcp:tier2",
				"transport": "streamable-http", // same field set as v1alpha1
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
			"kind": "LiteLLMMCPServerDiscovery",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"type": "toolhive",
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
// deployed operator works end-to-end against a real ToolHive operator
// (both v1alpha1 chart-shipped + v1beta1 fixture-hydrated CRDs) and a
// real LiteLLM, including AC-M3 wholesale-replace after out-of-band delete.
var _ = Describe("LiteLLMMCPServerDiscovery", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ourNs = "default"
	const devNs = "dev"
	const thName = "tier2-fake-mcp"
	const msdName = "tier2-toolhive-disc"
	// v1beta1 dual-version test uses a distinct namespace (prod) to prevent
	// the v1alpha1 MCPServerDiscovery (watching dev) from also discovering the
	// v1beta1 source object, which would create extra children and break the
	// cascade-delete It that runs after both propagation tests.
	const prodNs = "prod"
	const thNameV1beta1 = "tier2-fake-mcp-v1beta1"
	const msdNameV1beta1 = "tier2-toolhive-disc-v1beta1"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Delete(ctx, thName, metav1.DeleteOptions{})
		// Pre-clean v1beta1 resources (prod namespace).
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdNameV1beta1, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVRv1beta1).Namespace(prodNs).
			Delete(ctx, thNameV1beta1, metav1.DeleteOptions{})
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVR).Namespace(devNs).
			Delete(ctx, thName, metav1.DeleteOptions{})
		// Clean v1beta1 resources (prod namespace).
		_ = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Delete(ctx, msdNameV1beta1, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(toolhiveMCPGVRv1beta1).Namespace(prodNs).
			Delete(ctx, thNameV1beta1, metav1.DeleteOptions{})
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

	// propagates v1beta1 ToolHive MCPServer into child MCPServer (dual-version coverage)
	//
	// Exercises the dual-version informer landed in Phase 9 Task 09-07:
	// a ToolHive MCPServer created under v1beta1 (via the vendored fixture CRD from
	// test/e2e/fixtures/toolhive-v1beta1-crds.yaml) should produce the same child
	// litellm.ackstorm.ai/v1alpha1 MCPServer as the v1alpha1 path does.
	//
	// The v1beta1 CRD is not shipped by the published OCI chart; it is hydrated by
	// scripts/cluster.sh after the toolhive-operator-crds chart install (Task 09-08).
	It("propagates v1beta1 ToolHive MCPServer into child MCPServer (dual-version coverage)", func() {
		// Create the source MCPServer via the v1beta1 API in the prod namespace.
		// Using prod (not dev) so the v1alpha1 MCPServerDiscovery (tier2-toolhive-disc,
		// watching dev) does not discover this object and create extra children that
		// would break the cascade-delete It that follows.
		_, err := dyn.Resource(toolhiveMCPGVRv1beta1).Namespace(prodNs).
			Create(ctx, newToolhiveMCPServerV1beta1(thNameV1beta1, prodNs), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Create a MCPServerDiscovery scoped to prodNs — same spec as the v1alpha1
		// test but targeting the prod namespace where the v1beta1 source lives.
		_, err = dyn.Resource(msdiscGVR).Namespace(ourNs).
			Create(ctx, newToolhiveMSDiscovery(msdNameV1beta1, ourNs, []string{prodNs}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Assert the operator emits a child litellm.ackstorm.ai/v1alpha1 MCPServer
		// owned by the MCPServerDiscovery — same ownership rule as the v1alpha1 path.
		//
		// Tickle the Discovery CR every poll (see analogous It above for rationale).
		Eventually(func(g Gomega) {
			tickleMSDisc(dyn, msdNameV1beta1, ourNs)
			list, err := dyn.Resource(mcpsrvGVR).Namespace(ourNs).
				List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var found *unstructured.Unstructured
			for i := range list.Items {
				for _, o := range list.Items[i].GetOwnerReferences() {
					if o.Kind == "LiteLLMMCPServerDiscovery" && o.Name == msdNameV1beta1 {
						found = &list.Items[i]
						break
					}
				}
				if found != nil {
					break
				}
			}
			g.Expect(found).NotTo(BeNil(),
				"no child MCPServer owned by %s yet (v1beta1 source)", msdNameV1beta1)
			// Child name must reference the v1beta1 source object name.
			g.Expect(found.GetName()).To(
				SatisfyAny(
					ContainSubstring(thNameV1beta1),
					ContainSubstring(strings.ToLower(thNameV1beta1)),
				),
				"child MCPServer name %q does not reference v1beta1 source %q",
				found.GetName(), thNameV1beta1,
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
