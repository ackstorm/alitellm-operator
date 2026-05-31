//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// AC-O1: the operator's /metrics endpoint exposes the 12 metric names
// from spec §10 (regardless of value). Pulled from the cluster-internal
// metrics service via a one-shot curl pod.
var expectedMetricNames = []string{
	"reconcile_total",
	"reconcile_duration_seconds",
	"litellm_api_request_duration_seconds",
	"litellm_api_errors_total",
	"discovery_refresh_total",
	"discovery_generated_count",
	"discovery_skipped_total",
	"discovery_failed_total",
	"child_cr_writes_total",
	"drift_corrected_total",
	"connection_ready",
	"cr_status_age_seconds",
}

// envtest counterpart: internal/controller/metrics_scrape_test.go covers
// the §10 metric surface, pre-touched label combos, and reconcile-counter
// invariants against the in-process metrics registry. This suite proves
// the operator's ServiceMonitor + real /metrics endpoint serve the same
// surface inside a Helm-deployed cluster.
var _ = Describe("Metrics AC-O1", func() {

	It("/metrics exposes all 12 spec §10 metric names", func() {
		helpRE := regexp.MustCompile(`(?m)^# HELP (\S+)`)
		// curlPodBody retries past the kubectl-run attach race (which can
		// drop the body to empty); accept once the exposition carries at
		// least one HELP line, then assert the §10 surface below.
		out := curlPodBody("default", "metrics-poke",
			func(b []byte) bool { return helpRE.Match(b) },
			"curl", "-sS", "--max-time", "10",
			"http://alitellm-operator-metrics.default.svc.cluster.local:8080/metrics",
		)
		body := string(out)
		got := map[string]bool{}
		for _, m := range helpRE.FindAllStringSubmatch(body, -1) {
			got[m[1]] = true
		}

		var missing []string
		for _, want := range expectedMetricNames {
			if !got[want] {
				missing = append(missing, want)
			}
		}
		Expect(missing).To(BeEmpty(),
			"AC-O1: missing metric names from /metrics: %s\nfound: %d HELP lines",
			strings.Join(missing, ", "), len(got))
	})
})
