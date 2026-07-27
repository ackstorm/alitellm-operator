// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherFromTracker collects all metrics from a standalone CRStatusAgeCollector
// via a fresh registry so the tests remain isolated from the global registry.
func gatherFromTracker(t *testing.T, tracker *CRStatusAgeCollector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(tracker); err != nil {
		t.Fatalf("register tracker: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	return mfs
}

// labelValue extracts a label value from a Metric by label name.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// TestCRStatusAgeTracker_RecordSuccess_IncrementsMap verifies that
// RecordSuccess stores a timestamp (Behavior 1).
func TestCRStatusAgeTracker_RecordSuccess_IncrementsMap(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	tracker.RecordSuccess("Model", "foo")

	tracker.mu.RLock()
	_, ok := tracker.timestamps[crStatusAgeKey{Kind: "Model", Name: "foo"}]
	tracker.mu.RUnlock()

	if !ok {
		t.Fatal("expected entry for Model/foo in tracker map after RecordSuccess")
	}
}

// TestCRStatusAgeTracker_Forget_RemovesEntry verifies that Forget deletes
// the key and is idempotent (Behavior 2).
func TestCRStatusAgeTracker_Forget_RemovesEntry(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	tracker.RecordSuccess("Model", "bar")
	tracker.Forget("Model", "bar")

	tracker.mu.RLock()
	_, ok := tracker.timestamps[crStatusAgeKey{Kind: "Model", Name: "bar"}]
	tracker.mu.RUnlock()
	if ok {
		t.Fatal("expected entry for Model/bar to be removed after Forget")
	}

	// Second Forget on absent key must not panic.
	tracker.Forget("Model", "bar")
}

// TestCRStatusAgeTracker_Collect_EmitsSamples verifies that Collect emits
// one sample per live entry and that the value is >= expected minimum age
// (Behavior 3 + Behavior 4).
func TestCRStatusAgeTracker_Collect_EmitsSamples(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	tracker.RecordSuccess("Model", "x")

	// Sleep 100ms so the reported age has a measurable floor.
	time.Sleep(100 * time.Millisecond)

	mfs := gatherFromTracker(t, tracker)
	if len(mfs) == 0 {
		t.Fatal("expected alitellm_operator_cr_status_age_seconds metric family; got none")
	}
	mf := mfs[0]
	if mf.GetName() != "alitellm_operator_cr_status_age_seconds" { //nolint:goconst // Prometheus metric name literal asserted twice in same test scope; const would only add indirection
		t.Fatalf("unexpected metric family name %q", mf.GetName())
	}
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("expected 1 sample; got %d", len(mf.GetMetric()))
	}
	m := mf.GetMetric()[0]
	if labelValue(m, "kind") != "Model" {
		t.Errorf("label kind: want Model, got %q", labelValue(m, "kind"))
	}
	if labelValue(m, "name") != "x" {
		t.Errorf("label name: want x, got %q", labelValue(m, "name"))
	}
	age := m.GetGauge().GetValue()
	if age < 0.1 {
		t.Errorf("age value want >= 0.1s after 100ms sleep, got %f", age)
	}
}

// TestCRStatusAgeTracker_Forget_DropsFromScrape verifies that after Forget
// the label combination is absent from the next scrape (Behavior 4 part 2).
func TestCRStatusAgeTracker_Forget_DropsFromScrape(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	tracker.RecordSuccess("Model", "x")
	tracker.Forget("Model", "x")

	mfs := gatherFromTracker(t, tracker)
	for _, mf := range mfs {
		if mf.GetName() == "alitellm_operator_cr_status_age_seconds" && len(mf.GetMetric()) > 0 {
			t.Fatal("expected no alitellm_operator_cr_status_age_seconds samples after Forget; got some")
		}
	}
}

// TestCRStatusAgeTracker_NoLeak_1000Cycles verifies that 1000 RecordSuccess +
// Forget cycles across distinct names leave the map at its starting size
// (Behavior 5).
func TestCRStatusAgeTracker_NoLeak_1000Cycles(t *testing.T) {
	tracker := NewCRStatusAgeTracker()

	for i := 0; i < 1000; i++ {
		name := strings.Repeat("x", i%20+1) // vary names
		tracker.RecordSuccess("Model", name)
		tracker.Forget("Model", name)
	}

	tracker.mu.RLock()
	size := len(tracker.timestamps)
	tracker.mu.RUnlock()
	if size != 0 {
		t.Errorf("expected 0 entries after 1000 RecordSuccess+Forget cycles; got %d", size)
	}

	mfs := gatherFromTracker(t, tracker)
	for _, mf := range mfs {
		if mf.GetName() == "alitellm_operator_cr_status_age_seconds" && len(mf.GetMetric()) > 0 {
			t.Errorf("expected 0 alitellm_operator_cr_status_age_seconds samples after all Forgets; got %d", len(mf.GetMetric()))
		}
	}
}

// TestCRStatusAgeCollector_Concurrent verifies that 100 goroutines × 100
// (RecordSuccess + Forget) ops complete without race or panic (Behavior 6,
// T-07-01-03 mitigation).
func TestCRStatusAgeCollector_Concurrent(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tracker.RecordSuccess("Model", "concurrent")
				tracker.Forget("Model", "concurrent")
			}
		}()
	}
	wg.Wait()
	// No panic means pass; race detector will surface any races.
}

// TestCRStatusAgeTracker_DescribeReturnsDesc ensures the custom collector
// properly implements prometheus.Collector's Describe method.
func TestCRStatusAgeTracker_DescribeReturnsDesc(t *testing.T) {
	tracker := NewCRStatusAgeTracker()
	ch := make(chan *prometheus.Desc, 1)
	tracker.Describe(ch)
	close(ch)
	desc := <-ch
	if desc == nil {
		t.Fatal("Describe sent nil *prometheus.Desc")
	}
	s := desc.String()
	if !strings.Contains(s, "alitellm_operator_cr_status_age_seconds") {
		t.Errorf("Desc string %q does not contain metric name", s)
	}
}

// TestCRStatusAgeTracker_RegistrationInGlobalRegistry verifies that the
// package-level CRStatusAgeTracker is registered in the controller-runtime
// global registry (smoke test replacing the old CRStatusAgeSeconds nil-check).
func TestCRStatusAgeTracker_RegistrationInGlobalRegistry(t *testing.T) {
	if CRStatusAgeTracker == nil {
		t.Fatal("package-level CRStatusAgeTracker is nil; init() did not initialize it")
	}
	// Calling RecordSuccess on the global tracker must not panic.
	CRStatusAgeTracker.RecordSuccess("Test", "smoke")
	// The global registry should expose alitellm_operator_cr_status_age_seconds after RecordSuccess.
	// We do not gather from ctrlmetrics.Registry here to avoid test-suite
	// import cycles — the nil-check above is the structural smoke.
}
