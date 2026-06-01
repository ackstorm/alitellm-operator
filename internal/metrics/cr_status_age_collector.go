// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// crStatusAgeKey is the map key for the in-memory timestamp table.
type crStatusAgeKey struct {
	Kind string
	Name string
}

// CRStatusAgeCollector records the wall-clock time of each CR's most recent
// successful status write and implements prometheus.Collector.
//
// The package-level var metrics.CRStatusAgeTracker (type *CRStatusAgeCollector)
// is the production instance registered in metrics.go init.
//
// Usage:
// - Call RecordSuccess(kind, name) on every successful status write.
// - Call Forget(kind, name) in every finalizer, immediately BEFORE
// controllerutil.RemoveFinalizer(.), to prevent stale /metrics labels.
//
// OBS-03 (spec §10) + 07-CONTEXT.md option (i) custom Collector.
// Forget on finalize prevents monotonic /metrics cardinality growth
// (07-PATTERNS.md WARNING — no pre-existing cleanup exists, this is all-new).
// crStatusAgeDefaultMaxEntries bounds the number of tracked (kind, name)
// series. M-B8: Forget runs in finalizers, but a CR deleted while the
// operator is DOWN never gets Forget'd, so its series would leak forever.
// When the table exceeds this cap, the stalest entry (oldest last-success,
// i.e. the longest without a status write — the best deletion proxy) is
// evicted. 10000 comfortably exceeds any realistic live CR count.
const crStatusAgeDefaultMaxEntries = 10000

type CRStatusAgeCollector struct {
	mu         sync.RWMutex
	timestamps map[crStatusAgeKey]time.Time
	desc       *prometheus.Desc
	maxEntries int
}

// NewCRStatusAgeTracker constructs a zero-entry CRStatusAgeCollector and
// initialises the cr_status_age_seconds descriptor.
func NewCRStatusAgeTracker() *CRStatusAgeCollector {
	return &CRStatusAgeCollector{
		timestamps: make(map[crStatusAgeKey]time.Time),
		maxEntries: crStatusAgeDefaultMaxEntries,
		desc: prometheus.NewDesc(
			"cr_status_age_seconds",
			"Wall-clock age of each CR's most recent successful status write (spec §10).",
			[]string{"kind", "name"},
			nil,
		),
	}
}

// RecordSuccess stores time.Now for the given (kind, name) pair.
// Subsequent Collect calls will emit the elapsed duration since this
// timestamp as the sample value.
func (t *CRStatusAgeCollector) RecordSuccess(kind, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timestamps[crStatusAgeKey{Kind: kind, Name: name}] = time.Now()
	t.evictOverCapLocked()
}

// evictOverCapLocked drops the stalest entries until the table is within
// maxEntries. Caller must hold t.mu. maxEntries <= 0 disables eviction.
func (t *CRStatusAgeCollector) evictOverCapLocked() {
	if t.maxEntries <= 0 {
		return
	}
	for len(t.timestamps) > t.maxEntries {
		var stalestKey crStatusAgeKey
		var stalest time.Time
		first := true
		for k, ts := range t.timestamps {
			if first || ts.Before(stalest) {
				stalest, stalestKey, first = ts, k, false
			}
		}
		delete(t.timestamps, stalestKey)
	}
}

// Forget removes the (kind, name) entry from the tracker. Safe to call
// when the entry is absent (no-op). MUST be called in every finalizer
// immediately before controllerutil.RemoveFinalizer to prevent stale
// /metrics labels.
func (t *CRStatusAgeCollector) Forget(kind, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.timestamps, crStatusAgeKey{Kind: kind, Name: name})
}

// Describe implements prometheus.Collector. Emits the single descriptor
// for cr_status_age_seconds.
func (t *CRStatusAgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- t.desc
}

// Collect implements prometheus.Collector. Emits one GaugeValue sample per
// live entry: time.Since(lastSuccess).Seconds for each (kind, name) pair.
// Entries that have been Forget'd are not emitted.
func (t *CRStatusAgeCollector) Collect(ch chan<- prometheus.Metric) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	for k, lastSuccess := range t.timestamps {
		ch <- prometheus.MustNewConstMetric(
			t.desc,
			prometheus.GaugeValue,
			now.Sub(lastSuccess).Seconds(),
			k.Kind,
			k.Name,
		)
	}
}
