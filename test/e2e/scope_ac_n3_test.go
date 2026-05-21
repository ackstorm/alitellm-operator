//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os/exec"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// AC-N3: the operator never reaches LiteLLM's /user/* or /key/* paths.
// Tail the full LiteLLM access log and assert no HTTP request line
// references those prefixes. LiteLLM logs every HTTP request as:
// INFO: <ip>:<port> - "<METHOD> <PATH> HTTP/1.1" <STATUS> .
// so a per-line regex against the path token is sufficient.
var forbiddenLiteLLMPathRE = regexp.MustCompile(
	`"[A-Z]+\s+/(user|key)(/|\s|\?)`,
)

// envtest counterparts: internal/controller/{model,mcpserver,mcpserverdiscovery,modeldiscovery,a2aagent}_ac_n3_test.go
// cover the AC-N3 invariant (no /user/* or /key/* HTTP calls) per kind
// against the in-process mock LiteLLM's recorded-call tracker. This suite
// is the wholesale check against the real LiteLLM access log inside the
// Helm-deployed cluster, catching any path token the per-kind envtests
// might miss.
var _ = Describe("Scope AC-N3", func() {

	It("LiteLLM access log shows zero /user/ or /key/ calls across the suite", func() {
		// Exclude Job-owned pods (e.g. the chart's prisma-migrations Job,
		// which may be in ImagePullBackoff if Helm cached a stale tag —
		// it shares app.kubernetes.io/name=litellm with the Deployment
		// pod and would otherwise fail the multi-pod log fetch).
		out, err := exec.Command("kubectl", "-n", "litellm-system", "logs",
			"-l", "app.kubernetes.io/name=litellm,!batch.kubernetes.io/job-name",
			"--tail=-1", "--all-containers=true",
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl logs failed: %s", string(out))

		matches := forbiddenLiteLLMPathRE.FindAllString(string(out), -1)
		Expect(matches).To(BeEmpty(),
			"AC-N3 violation: operator hit /user/ or /key/ paths: %v", matches)
	})
})
