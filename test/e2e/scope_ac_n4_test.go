//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AC-N4: a CR in a namespace the operator does NOT watch must not
// trigger any LiteLLM-side mutation. The operator's watchNamespace
// is `default` (operator.values.yaml); `dev` is provisioned but
// never reconciled.
//
// envtest counterpart: internal/controller/scope_ac_n4_test.go (renamed
// from watchnamespace_test.go in Phase 4 / Task 4.1) covers the same
// invariant via the in-process manager. This suite is the wholesale
// check against the Helm-deployed operator with its watch-namespace
// env-var injection.
var _ = Describe("Scope AC-N4 non-watched namespace", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "dev"
	const modelName = "tier2-ac-n4-unwatched"

	BeforeAll(func() {
		_ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(ctx, modelName, metav1.DeleteOptions{})
	})

	AfterAll(func() {
		_ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(ctx, modelName, metav1.DeleteOptions{})
	})

	It("Model CR in dev namespace is not reconciled and not registered in LiteLLM", func() {
		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(modelName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Wait > 2× SAFETY_RELIST_INTERVAL (=10s) to give the operator
		// every chance to pick it up if its scope were broken.
		time.Sleep(25 * time.Second)

		// CR status must remain empty (no operator wrote to it).
		obj, err := dyn.Resource(modelGVR).Namespace(ns).
			Get(ctx, modelName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			Fail("CR vanished before assertion — apiserver hiccup")
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(modelID(obj)).To(BeEmpty(),
			"AC-N4 violation: operator wrote modelID into Model in non-watched ns")

		// Cross-check: LiteLLM has no model registered under this name.
		podName := fmt.Sprintf("ac-n4-probe-%d", time.Now().UnixNano())
		out, err := exec.Command("kubectl", "-n", "litellm-system", "run", podName,
			"--rm", "-i", "--restart=Never", "--quiet",
			"--image=curlimages/curl:8.10.1", "--",
			"curl", "-sS", "--max-time", "10",
			"-H", "Authorization: Bearer sk-test-master-key",
			"http://litellm.litellm-system.svc.cluster.local:4000/model/info",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))

		// Strip "warning: couldn't attach." prefix.
		idx := bytes.IndexByte(out, '{')
		Expect(idx).To(BeNumerically(">=", 0), "no JSON in: %s", string(out))
		var resp struct {
			Data []map[string]interface{} `json:"data"`
		}
		Expect(json.Unmarshal(out[idx:], &resp)).
			To(Succeed(), "raw=%s", string(out[idx:]))
		for _, m := range resp.Data {
			name, _ := m["model_name"].(string)
			Expect(strings.Contains(name, modelName)).To(BeFalse(),
				"AC-N4 violation: LiteLLM has model_name=%q for unwatched-ns CR", name)
		}
	})

	It("operator manager-role is a namespaced Role (not ClusterRole)", func() {
		// Issue #21 — Helm release installs the operator into
		// alitellm-operator-system. Assert:
		//  1. A Role named alitellm-operator-role exists in that ns.
		//  2. A RoleBinding named alitellm-operator-rolebinding exists.
		//  3. Legacy ClusterRole/Binding names are absent.
		//  4. Retained ClusterRoles still present.
		const opNS = "default"

		out, err := exec.Command("kubectl", "-n", opNS, "get", "role",
			"alitellm-operator-role", "-o", "name").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(),
			"alitellm-operator-role Role missing in %s: %s", opNS, string(out))
		Expect(strings.TrimSpace(string(out))).To(Equal(
			"role.rbac.authorization.k8s.io/alitellm-operator-role"))

		out, err = exec.Command("kubectl", "-n", opNS, "get", "rolebinding",
			"alitellm-operator-rolebinding", "-o", "name").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(),
			"alitellm-operator-rolebinding missing: %s", string(out))

		for _, legacy := range []struct {
			kind string
			name string
		}{
			{"clusterrole", "alitellm-operator-manager-role"},
			{"clusterrolebinding", "alitellm-operator-manager-rolebinding"},
		} {
			err := exec.Command("kubectl", "get", legacy.kind, legacy.name).Run()
			Expect(err).To(HaveOccurred(),
				"legacy %s %s still exists — RBAC scope-down incomplete",
				legacy.kind, legacy.name)
		}

		for _, cr := range []string{
			"alitellm-operator-metrics-auth-role",
			"alitellm-operator-metrics-reader",
		} {
			err := exec.Command("kubectl", "get", "clusterrole", cr).Run()
			Expect(err).NotTo(HaveOccurred(),
				"retained ClusterRole %s missing", cr)
		}
	})

	It("operator cannot access Secrets outside WATCH_NAMESPACE", func() {
		// Confirm the apiserver authorizer enforces the namespace
		// boundary via `kubectl auth can-i`. The operator's SA must
		// be allowed secrets list in the watched ns (default) and
		// denied in the out-of-watch ns (dev).
		const opNS = "default"
		const saName = "alitellm-operator"
		sa := fmt.Sprintf("system:serviceaccount:%s:%s", opNS, saName)

		// In-watch: allowed.
		out, err := exec.Command("kubectl", "auth", "can-i",
			"list", "secrets", "--as", sa, "-n", "default").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))
		Expect(strings.TrimSpace(string(out))).To(Equal("yes"))

		// Out-of-watch: denied. `can-i no` exits 1 — treat as success.
		out, err = exec.Command("kubectl", "auth", "can-i",
			"list", "secrets", "--as", sa, "-n", "dev").CombinedOutput()
		exitCode := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
		Expect(exitCode).To(Equal(1),
			"expected can-i to exit 1 (no), got %d. Output: %s",
			exitCode, string(out))
		Expect(strings.TrimSpace(string(out))).To(Equal("no"))
	})
})
