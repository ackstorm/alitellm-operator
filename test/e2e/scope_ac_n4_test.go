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
		Expect(litellmModelID(obj)).To(BeEmpty(),
			"AC-N4 violation: operator wrote litellmModelID into Model in non-watched ns")

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
})
