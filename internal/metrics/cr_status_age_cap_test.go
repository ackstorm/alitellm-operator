// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"
)

// TestCRStatusAgeCollector_EvictsOverCap is the M-B8 regression: a CR deleted
// while the operator is down never gets Forget'd, so its series would leak
// forever. When the table exceeds maxEntries, the stalest entry (oldest
// last-success) is evicted, bounding cardinality.
func TestCRStatusAgeCollector_EvictsOverCap(t *testing.T) {
	c := &CRStatusAgeCollector{
		timestamps: make(map[crStatusAgeKey]time.Time),
		maxEntries: 3,
	}
	// Monotonic clock guarantees strictly increasing timestamps, so "a" is
	// the stalest and must be the one evicted when "d" pushes over the cap.
	c.RecordSuccess("Model", "a")
	c.RecordSuccess("Model", "b")
	c.RecordSuccess("Model", "c")
	c.RecordSuccess("Model", "d")

	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.timestamps) != 3 {
		t.Fatalf("want 3 entries after cap eviction, got %d", len(c.timestamps))
	}
	if _, ok := c.timestamps[crStatusAgeKey{Kind: "Model", Name: "a"}]; ok {
		t.Error("stalest entry 'a' should have been evicted")
	}
	if _, ok := c.timestamps[crStatusAgeKey{Kind: "Model", Name: "d"}]; !ok {
		t.Error("newest entry 'd' should be present")
	}
}
