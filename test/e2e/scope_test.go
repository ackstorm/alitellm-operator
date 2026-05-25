//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Expected 9 operator-owned kinds (spec §10 kind enum +
// LiteLLMGuardRail added in v0.3.1 + LiteLLMModelAlias added in v0.5.0).
var expectedKinds = map[string]bool{
	"LiteLLMConnection":         true,
	"LiteLLMModel":              true,
	"LiteLLMModelDiscovery":     true,
	"LiteLLMMCPServer":          true,
	"LiteLLMMCPServerDiscovery": true,
	"LiteLLMA2AAgent":           true,
	"LiteLLMTeam":               true,
	"LiteLLMGuardRail":          true,
	"LiteLLMModelAlias":         true,
}

var _ = Describe("Scope and metrics", Ordered, ContinueOnFailure, func() {

	It("exposes exactly 9 in-scope CRDs and no dropped-kind controllers in logs (AC-N1+N2)", func() {
		out, err := exec.Command("kubectl", "get", "crds",
			"-o", `jsonpath={range .items[?(@.spec.group=="litellm.ackstorm.ai")]}{.spec.names.kind}{"\n"}{end}`,
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))

		got := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			got[line] = true
		}
		Expect(got).To(Equal(expectedKinds),
			"unexpected CRD set under litellm.ackstorm.ai: got=%v want=%v", got, expectedKinds)

		// Tail the full operator log and assert no dropped-kind controller
		// chatter. "skipping reconcile for kind" / "unknown kind" would
		// indicate the manager is sieving out an unexpected GVK.
		logs, err := exec.Command("kubectl", "-n", "default", "logs",
			"-l", "control-plane=alitellm-operator",
			"--tail=-1", "--all-containers=true",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "logs cmd: %s", string(logs))
		s := string(logs)
		for _, pat := range []string{
			"skipping reconcile for kind",
			"unknown kind",
			"unsupported kind",
		} {
			Expect(s).NotTo(ContainSubstring(pat),
				"operator log contains forbidden pattern %q", pat)
		}
	})

	It("LiteLLM Pod runs the operator-targeted image tag :v1.83.10-stable (chart-pin override smoke)", func() {
		out, err := exec.Command("kubectl", "-n", "litellm-system",
			"get", "pod", "-l", "app.kubernetes.io/name=litellm",
			"-o", "jsonpath={.items[0].spec.containers[*].image}",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))
		img := strings.TrimSpace(string(out))
		Expect(img).To(HaveSuffix(":v1.83.10-stable"),
			"LiteLLM image %q does not pin v1.83.10-stable — chart bump regressed", img)
	})
})
