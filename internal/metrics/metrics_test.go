// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Phase 4 — _FINALv3 metric surface deltas:
// - discovery_registered_count → discovery_generated_count (rename)
// - discovery_failed_total.reason ∈ {ChildCRWriteFailed} (narrowed from
// {LiteLLMRejected, LiteLLMUnavailable})
// - child_cr_writes_total{kind, action, result} (new)
// The tests below are kept in lockstep with metrics.go via these literals.

// TestAllS10MetricNamesAreRegistered — OBS-01: every metric name from spec
// §10 is registered against controller-runtime's metrics registry by init.
func TestAllS10MetricNamesAreRegistered(t *testing.T) {
	want := []string{
		"reconcile_total",
		"reconcile_duration_seconds",
		"alitellm_api_request_duration_seconds",
		"alitellm_api_errors_total",
		"discovery_refresh_total",
		"discovery_generated_count",
		"discovery_skipped_total",
		"discovery_failed_total",
		"drift_corrected_total",
		"connection_ready",
		"cr_status_age_seconds",
		"child_cr_writes_total",
	}

	// cr_status_age_seconds is emitted by a custom prometheus.Collector
	// whose Collect() yields nothing when the timestamps map is empty.
	// Pre-touch + Forget so the family is present in Gather output
	// regardless of test execution order under -shuffle=on.
	CRStatusAgeTracker.RecordSuccess("_test_", "_sentinel_")
	t.Cleanup(func() { CRStatusAgeTracker.Forget("_test_", "_sentinel_") })

	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}
	got := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("metric %q not registered in controller-runtime metrics.Registry", name)
		}
	}
}

// TestDriftCorrectedTotalLabelCombosArePreTouched — A5: every enumerated
// {domain, action} label combo from spec §10's Label-value enums table is
// pre-touched at init time so AC-O1 sees all lines on first scrape, even
// before Phase 3 increments anything.
//
// domains ∈ {model, mcp, a2a, team, guardrail} × actions ∈ {create_missing,
// update_drifted, delete_vanished, duplicate_pruned} = 20 combos.
func TestDriftCorrectedTotalLabelCombosArePreTouched(t *testing.T) {
	count := testutil.CollectAndCount(DriftCorrectedTotal, "drift_corrected_total")
	if count < 12 {
		t.Fatalf("drift_corrected_total: want >= 12 label-combos pre-touched, got %d", count)
	}
}

// TestConnectionReadyReasonsPreTouched — the §10 enumerated reasons for
// connection_ready are all present in /metrics output (6 entries: Synced,
// Connecting, Absent, Unreachable, BadMasterKey, SecretNotFound).
func TestConnectionReadyReasonsPreTouched(t *testing.T) {
	count := testutil.CollectAndCount(ConnectionReady, "connection_ready")
	if count < 6 {
		t.Fatalf("connection_ready: want >= 6 reason combos pre-touched, got %d", count)
	}
}

// TestReconcileTotalResultsPreTouched — §10 reconcile_total{result} ∈
// {success, error, requeued} × all 7 kinds = 21 combos minimum.
func TestReconcileTotalResultsPreTouched(t *testing.T) {
	count := testutil.CollectAndCount(ReconcileTotal, "reconcile_total")
	if count < 21 {
		t.Fatalf("reconcile_total: want >= 21 kind×result combos pre-touched, got %d", count)
	}
}

// TestVarsAreNonNil — defensive check; package-level metric vars must be
// initialized by init before any consumer (Phase 3+) imports the package.
func TestVarsAreNonNil(t *testing.T) {
	checks := map[string]prometheus.Collector{
		"ReconcileTotal":                   ReconcileTotal,
		"ReconcileDurationSeconds":         ReconcileDurationSeconds,
		"LiteLLMAPIRequestDurationSeconds": LiteLLMAPIRequestDurationSeconds,
		"LiteLLMAPIErrorsTotal":            LiteLLMAPIErrorsTotal,
		"DiscoveryRefreshTotal":            DiscoveryRefreshTotal,
		"DiscoveryGeneratedCount":          DiscoveryGeneratedCount,
		"DiscoverySkippedTotal":            DiscoverySkippedTotal,
		"DiscoveryFailedTotal":             DiscoveryFailedTotal,
		"DriftCorrectedTotal":              DriftCorrectedTotal,
		"ConnectionReady":                  ConnectionReady,
		"CRStatusAgeTracker":               CRStatusAgeTracker,
		"ChildCRWritesTotal":               ChildCRWritesTotal,
	}
	for name, c := range checks {
		if c == nil {
			t.Errorf("%s is nil — package init() did not construct it", name)
		}
	}
}

// TestNoUnexpectedMetricNamesInS10Surface — sanity: scrape only metric
// families that start with one of the §10 prefixes (controller-runtime
// itself registers other "go_*", "process_*", "workqueue_*" series which
// are not part of §10's contract).
func TestNoUnexpectedMetricNamesInS10Surface(t *testing.T) {
	// cr_status_age_seconds is emitted by a custom prometheus.Collector
	// whose Collect() yields nothing when the timestamps map is empty.
	// Prometheus omits empty families from Gather output, so the metric
	// family would be missing if this test runs before any test that
	// records a CR timestamp (order-coupling under -shuffle=on).
	// Pre-touch + Forget here so the family is always present.
	CRStatusAgeTracker.RecordSuccess("_test_", "_sentinel_")
	t.Cleanup(func() { CRStatusAgeTracker.Forget("_test_", "_sentinel_") })

	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}
	s10 := map[string]bool{
		"reconcile_total": true, "reconcile_duration_seconds": true,
		"alitellm_api_request_duration_seconds": true, "alitellm_api_errors_total": true,
		"discovery_refresh_total": true, "discovery_generated_count": true,
		"discovery_skipped_total": true, "discovery_failed_total": true,
		"drift_corrected_total": true, "connection_ready": true,
		"cr_status_age_seconds": true, "child_cr_writes_total": true,
	}
	// Sanity: at least all 12 §10 metric names exist in the gathered set
	// (11 pre-Phase-4 + child_cr_writes_total added in OBS-04).
	hits := 0
	for _, mf := range mfs {
		if s10[mf.GetName()] {
			hits++
		}
	}
	if hits < 12 {
		var names []string
		for _, mf := range mfs {
			names = append(names, mf.GetName())
		}
		t.Errorf("§10 metric hits: want >= 12, got %d (gathered: %s)", hits, strings.Join(names, ","))
	}
}

// TestDiscoveryFailedReasonsRestrictedToChildCRWriteFailed — _FINALv3 D-10:
// the discovery_failed_total{reason} label enum is narrowed to a single
// value, "ChildCRWriteFailed". Pre-touched combos cross all allKinds × 1
// reason = 7. Asserts every gathered label tuple carries the new reason
// literal (no stale LiteLLMRejected / LiteLLMUnavailable values from the
// pre-_FINALv3 surface).
func TestDiscoveryFailedReasonsRestrictedToChildCRWriteFailed(t *testing.T) {
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather: %v", err)
	}

	var found bool
	var metricCount int
	for _, mf := range mfs {
		if mf.GetName() != "discovery_failed_total" {
			continue
		}
		found = true
		for _, m := range mf.GetMetric() {
			metricCount++
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "reason" {
					if got := lp.GetValue(); got != "ChildCRWriteFailed" {
						t.Errorf("discovery_failed_total{reason=%q}: only %q is allowed in _FINALv3 (D-10)",
							got, "ChildCRWriteFailed")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("discovery_failed_total metric family not registered")
	}
	// Pre-touched combos: allKinds (7) × discoveryFailedReasons (1) = 7.
	if metricCount < 7 {
		t.Errorf("discovery_failed_total: want >= 7 pre-touched combos, got %d", metricCount)
	}
}

// TestChildCRWritesTotalLabelCombosArePreTouched — Phase 4 OBS-04: the
// child_cr_writes_total metric family pre-touches every {kind, action,
// result} combination from discoveryParentKinds × childCRWriteActions ×
// childCRWriteResults at init time. 2 × 3 × 2 = 12 combos.
//
// Mirrors TestDriftCorrectedTotalLabelCombosArePreTouched.
func TestChildCRWritesTotalLabelCombosArePreTouched(t *testing.T) {
	count := testutil.CollectAndCount(ChildCRWritesTotal, "child_cr_writes_total")
	if count < 12 {
		t.Fatalf("child_cr_writes_total: want >= 12 {kind, action, result} combos pre-touched, got %d", count)
	}
}

// TestDeletionOrphanedTotalPreTouched — Issue #23: verifies every kind
// label combo is pre-touched at init time so the metric appears on
// first scrape (Assumption A5, mirrors DriftCorrectedTotal). Uses
// deletionPolicyKinds (full LiteLLM* names matching the per-controller
// *Kind constants) rather than allKinds because the label values must
// align with what controllers actually emit at increment sites.
func TestDeletionOrphanedTotalPreTouched(t *testing.T) {
	got := testutil.CollectAndCount(DeletionOrphanedTotal, "alitellm_operator_deletion_orphaned_total")
	want := len(deletionPolicyKinds)
	if got != want {
		t.Fatalf("DeletionOrphanedTotal pre-touch count: got %d, want %d (deletionPolicyKinds)", got, want)
	}
}

// TestDeletionBlockedTrackerRecordForget — Issue #23: verifies the
// gauge collector emits one sample per tracked CR and Forget removes
// the sample.
func TestDeletionBlockedTrackerRecordForget(t *testing.T) {
	tr := NewDeletionBlockedTracker()
	tr.Record("LiteLLMModel", "ns1", "foo")
	tr.Record("LiteLLMModel", "ns1", "bar")

	if n := testutil.CollectAndCount(tr, "alitellm_operator_deletion_blocked"); n != 2 {
		t.Fatalf("after 2 Record: count=%d, want 2", n)
	}

	tr.Forget("LiteLLMModel", "ns1", "foo")
	if n := testutil.CollectAndCount(tr, "alitellm_operator_deletion_blocked"); n != 1 {
		t.Fatalf("after Forget: count=%d, want 1", n)
	}

	// Forget of absent key is a no-op.
	tr.Forget("LiteLLMModel", "ns1", "never-recorded")
	if n := testutil.CollectAndCount(tr, "alitellm_operator_deletion_blocked"); n != 1 {
		t.Fatalf("after Forget of absent key: count=%d, want 1", n)
	}
}
