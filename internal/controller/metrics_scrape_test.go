// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMetricsExposeS10Set — OBS-01 / AC-O1. The manager's metrics
// endpoint MUST expose every §10 metric name and every enumerated
// label-value combination on the FIRST scrape (Assumption A5: init-time
// pre-touching). No reconcile increments are needed.
//
// Procedure:
//
// 1. HTTP-GET http://127.0.0.1:18080/metrics (the test manager's
// metrics server, bound in suite_test.go via MetricsAddr).
// 2. Parse the scrape (Prometheus text exposition format).
// 3. Assert each of the 8 §10 metric names is present.
// 4. Assert alitellm_operator_drift_corrected_total has ≥12 enumerated label combos
// (4 domains × 3 actions).
// 5. Assert connection_ready has ≥6 reason combos.
func TestMetricsExposeS10Set(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals")
	}

	// Allow a small startup window for the metrics listener to bind.
	var resp *http.Response
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(MetricsURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("metrics scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics scrape: HTTP %d (want 200)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	scrape := string(body)

	// All 8 §10 metric names must be present in the scrape.
	wantNames := []string{
		"alitellm_operator_drift_corrected_total",
		"reconcile_total",
		"alitellm_operator_discovery_refresh_total",
		"alitellm_operator_discovery_generated_count",
		"alitellm_operator_discovery_failed_total",
		"connection_ready",
		"alitellm_operator_cr_status_age_seconds",
		"alitellm_operator_child_cr_writes_total",
	}
	for _, name := range wantNames {
		// Look for "# HELP <name>" — Prometheus emits a HELP line per
		// metric family. This is more precise than substring matching
		// "name" anywhere in the scrape (which could match label values
		// from another family).
		if !strings.Contains(scrape, "# HELP "+name+" ") &&
			!strings.Contains(scrape, "# TYPE "+name+" ") {
			t.Errorf("§10 metric %q not present in /metrics scrape", name)
		}
	}

	// alitellm_operator_drift_corrected_total: the full enumeration is now 5 domains ×
	// 4 actions (incl. duplicate_pruned) = 20 combos. This loop spot-checks
	// the original 12-combo subset (4 domains × 3 actions); the guardrail
	// domain and duplicate_pruned action are covered by the pre-touch test.
	driftDomains := []string{"model", "mcp", "a2a", "team"}
	driftActions := []string{"create_missing", "update_drifted", "delete_vanished"}
	for _, d := range driftDomains {
		for _, a := range driftActions {
			// Prometheus text format renders the labelset as e.g.:
			// alitellm_operator_drift_corrected_total{action="create_missing",domain="model"} 0
			// Label order is alphabetical (action before domain).
			want := `alitellm_operator_drift_corrected_total{action="` + a + `",domain="` + d + `"}`
			if !strings.Contains(scrape, want) {
				t.Errorf("alitellm_operator_drift_corrected_total label combo missing: %s", want)
			}
		}
	}

	// connection_ready: 6 enumerated reasons.
	connReasons := []string{reasonSynced, reasonConnecting, reasonAbsent, reasonUnreachable, reasonBadMasterKey, reasonSecretNotFound}
	for _, r := range connReasons {
		want := `connection_ready{reason="` + r + `"}`
		if !strings.Contains(scrape, want) {
			t.Errorf("connection_ready{reason=%q} not present in /metrics", r)
		}
	}

	// reconcile_total: 7 kinds × 3 results = 21 combos. Sample-spot-check:
	// {kind="LiteLLMModel", result="success"} must be present (one of the 21).
	if !strings.Contains(scrape, `reconcile_total{kind="LiteLLMModel",result="success"}`) {
		t.Errorf(`reconcile_total{kind="LiteLLMModel",result="success"} not present in /metrics`)
	}
}
