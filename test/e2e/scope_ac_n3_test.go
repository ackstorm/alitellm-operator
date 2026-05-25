//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"os/exec"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// AC-N3: the operator never reaches LiteLLM's /user/* or /key/* paths
// EXCEPT the auth-gated probe at POST /key/health (owned by the
// LiteLLMConnection reconciler — see internal/litellm/keyinfo.go for
// the rationale). Tail the full LiteLLM access log and assert no HTTP
// request line references the forbidden subset. LiteLLM logs every
// HTTP request as:
//
//	INFO: <ip>:<port> - "<METHOD> <PATH> HTTP/1.1" <STATUS> .
//
// Line-level matching: each log line is checked for a forbidden path
// token. The /key/health probe is allow-listed by a separate match
// because RE2 lacks lookahead.
var (
	forbiddenLiteLLMPathRE = regexp.MustCompile(
		`"[A-Z]+\s+/(user|key)(/|\s|\?)`,
	)
	probePathAllowedRE = regexp.MustCompile(
		`"[A-Z]+\s+/key/health(\s|/|\?)`,
	)
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

		// Line-level filter: a forbidden /user/* or /key/* match is
		// downgraded to a violation only when the SAME line does not
		// carry the allow-listed /key/health probe. This avoids the
		// captured-fragment pitfall where the forbidden regex returns
		// just `"POST /key/` (no `health` suffix) and the allow-list
		// regex can never match against the truncated fragment.
		var violations []string
		for _, line := range strings.Split(string(out), "\n") {
			if !forbiddenLiteLLMPathRE.MatchString(line) {
				continue
			}
			if probePathAllowedRE.MatchString(line) {
				continue
			}
			violations = append(violations, line)
		}
		Expect(violations).To(BeEmpty(),
			"AC-N3 violation: operator hit /user/ or /key/ paths (excluding /key/health probe): %v", violations)
	})
})
