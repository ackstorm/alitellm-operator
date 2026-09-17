//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// CAT-01 — the one path envtest cannot cover: the OpenCode catalog ConfigMap
// written into a namespace OTHER than the operator's (production shape:
// operator in ackstorm, ConfigMap in alitellm-auth). Needs the uncached
// writer (manager cache is scoped to WATCH_NAMESPACE) AND the chart's
// Role/RoleBinding in the target namespace — without the latter the write
// is a 403 and the ConfigMap never appears. Values live in
// test/e2e/cluster/02-operator/operator.values.yaml; the namespace in
// 00-namespaces.
const (
	catalogNS   = "e2e-catalog"
	catalogName = "opencode-catalog"
)

func newCatalogTargetModel(name, ns, model string) *unstructured.Unstructured {
	m := newOpenAIMockModel(name, ns)
	m.Object["spec"].(map[string]interface{})["params"].(map[string]interface{})["model"] = model
	return m
}

// catalogModel returns the alias row from the served api.json, or nil.
func catalogModel(alias string) (map[string]interface{}, error) {
	cm, err := cs.CoreV1().ConfigMaps(catalogNS).Get(ctx, catalogName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var doc map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(cm.Data["api.json"]), &doc); err != nil {
		return nil, err
	}
	models, _ := doc["ackstorm"]["models"].(map[string]interface{})
	row, _ := models[alias].(map[string]interface{})
	return row, nil
}

var _ = Describe("OpenCode catalog (CAT-01)", Label("catalog"), Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const crName = "e2e-catalog-alias"
	const alias = "e2e.catalog"
	// Two chat targets that differ in a capability LiteLLM's cost map knows:
	// gpt-4o supports_vision=true, gpt-4 does not.
	const visionModel, textModel = "cat-vision", "cat-text"

	cleanup := func() {
		_ = dyn.Resource(modelAliasGVR).Namespace(ns).Delete(ctx, crName, metav1.DeleteOptions{})
		for _, m := range []string{visionModel, textModel} {
			_ = dyn.Resource(modelGVR).Namespace(ns).Delete(ctx, m, metav1.DeleteOptions{})
		}
	}
	BeforeAll(func() {
		cleanup()
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(modelAliasGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "alias %s still present", crName)
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})
	AfterAll(cleanup)

	It("writes the alias into the cross-namespace ConfigMap and follows a retarget", func() {
		for name, model := range map[string]string{visionModel: "openai/gpt-4o", textModel: "openai/gpt-4"} {
			_, err := dyn.Resource(modelGVR).Namespace(ns).
				Create(ctx, newCatalogTargetModel(name, ns, model), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create model %s", name)
		}
		// Targets must be in LiteLLM before the alias reconciles, or the
		// renderer drops the alias as unresolved.
		for _, name := range []string{visionModel, textModel} {
			Eventually(func(g Gomega) {
				obj, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(modelID(obj)).NotTo(BeEmpty(), "%s not yet in LiteLLM", name)
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		}

		_, err := dyn.Resource(modelAliasGVR).Namespace(ns).
			Create(ctx, newModelAliasCR(crName, ns, [][2]string{{alias, visionModel}}), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "create alias")

		Eventually(func(g Gomega) {
			row, err := catalogModel(alias)
			g.Expect(err).NotTo(HaveOccurred(), "ConfigMap %s/%s (403 here = chart Role/RoleBinding missing)", catalogNS, catalogName)
			g.Expect(row).NotTo(BeNil(), "alias %q not in catalog yet", alias)
			g.Expect(row["attachment"]).To(BeTrue(), "vision target should render attachment=true: %v", row)
			g.Expect(row["tool_call"]).To(BeTrue(), "%v", row)
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		// Retarget → the catalog must follow (the production half that matters).
		patch := []byte(`[{"op":"replace","path":"/spec/aliases/0/value","value":"` + textModel + `"}]`)
		_, err = dyn.Resource(modelAliasGVR).Namespace(ns).
			Patch(ctx, crName, types.JSONPatchType, patch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred(), "patch alias")

		Eventually(func(g Gomega) {
			row, err := catalogModel(alias)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(row).NotTo(BeNil())
			g.Expect(row["attachment"]).To(BeFalse(), "text target should render attachment=false: %v", row)
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})
})
