// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// DeletionBlockedTracker is a prometheus.Collector that emits one
// GaugeValue per CR currently stuck in Terminating because
// deletionPolicy=Delete and the LiteLLM-side ack has not succeeded.
//
// Mirrors the CRStatusAgeTracker pattern (OBS-03): callers Record on
// every blocked reconcile and Forget when the CR finally clears (ack
// received) OR when the finalizer is forcibly removed.
//
// Issue #23 — see internal/controller/deletionpolicy for the resolver
// that decides whether to gate finalizer removal.
type DeletionBlockedTracker struct {
	mu   sync.Mutex
	keys map[string]struct{} // "kind\x00namespace\x00name"
	desc *prometheus.Desc
}

// NewDeletionBlockedTracker returns a ready-to-use tracker.
func NewDeletionBlockedTracker() *DeletionBlockedTracker {
	return &DeletionBlockedTracker{
		keys: map[string]struct{}{},
		desc: prometheus.NewDesc(
			"alitellm_operator_deletion_blocked",
			"1 per CR currently in Terminating because deletionPolicy=Delete and LiteLLM ack is missing.",
			[]string{"kind", "namespace", "name"},
			nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (t *DeletionBlockedTracker) Describe(ch chan<- *prometheus.Desc) {
	ch <- t.desc
}

// Collect emits one sample per tracked key.
func (t *DeletionBlockedTracker) Collect(ch chan<- prometheus.Metric) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.keys {
		kind, ns, name := splitDeletionBlockedKey(k)
		ch <- prometheus.MustNewConstMetric(t.desc, prometheus.GaugeValue, 1, kind, ns, name)
	}
}

// Record marks (kind, namespace, name) as currently blocked.
func (t *DeletionBlockedTracker) Record(kind, namespace, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys[makeDeletionBlockedKey(kind, namespace, name)] = struct{}{}
}

// Forget drops the (kind, namespace, name) entry. Safe to call when
// absent.
func (t *DeletionBlockedTracker) Forget(kind, namespace, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.keys, makeDeletionBlockedKey(kind, namespace, name))
}

func makeDeletionBlockedKey(kind, ns, name string) string {
	return kind + "\x00" + ns + "\x00" + name
}

// splitDeletionBlockedKey decomposes a key produced by
// makeDeletionBlockedKey back into (kind, namespace, name). The key
// normally has exactly two NUL separators; SplitN caps the field count
// at 3 so a stray NUL byte embedded in any input cannot overflow the
// result array (an extra separator just lands inside the third field).
func splitDeletionBlockedKey(k string) (string, string, string) {
	parts := strings.SplitN(k, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

// DeletionBlocked is the process-wide singleton, registered in
// metrics.go's MustRegister block.
var DeletionBlocked = NewDeletionBlockedTracker()
