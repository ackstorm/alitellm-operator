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
//
// OPERATOR-SOURCE attribution: the invariant is about the OPERATOR, not the
// whole cluster. The access log is shared — other suites legitimately drive
// /key/* as TEST clients (e.g. the deny-by-default enforcement spec in
// team_test.go mints and revokes a team-scoped key via /key/generate +
// /key/delete). Those come from throwaway curl-pod IPs, not the operator pod.
// So a forbidden line is a violation only when its <ip> matches the operator
// pod's IP. This makes the check strictly MORE precise than a blanket
// "nobody hits /key/*" (which would false-positive on test-driver traffic and
// could not tell an operator regression from a test client).
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
// operatorPodIP resolves the Helm-deployed operator pod's IP (namespace-
// agnostic, via the deployment's control-plane label) so AC-N3 can attribute
// forbidden access-log lines to the operator and ignore test-driver traffic.
func operatorPodIP() string {
	out, err := exec.Command("kubectl", "get", "pods", "-A",
		"-l", "control-plane=alitellm-operator",
		"-o", "jsonpath={.items[0].status.podIP}").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var _ = Describe("Scope AC-N3", func() {

	It("LiteLLM access log shows zero /user/ or /key/ calls across the suite", func() {
		operatorIP := operatorPodIP()
		Expect(operatorIP).NotTo(BeEmpty(),
			"could not resolve operator pod IP (control-plane=alitellm-operator) — AC-N3 needs it to attribute /key//user calls to the operator")

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
			// Only the operator's own requests count — test-driver /key/*
			// traffic (deny-by-default enforcement spec) comes from other pods.
			if !strings.Contains(line, operatorIP+":") {
				continue
			}
			violations = append(violations, line)
		}
		Expect(violations).To(BeEmpty(),
			"AC-N3 violation: operator (%s) hit /user/ or /key/ paths (excluding /key/health probe): %v", operatorIP, violations)
	})
})
