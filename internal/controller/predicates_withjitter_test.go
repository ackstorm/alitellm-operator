// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"
)

// TestWithJitter_SubTenNanos is the M-B5 regression: rand.Int63n panics when
// its argument is <= 0, and int64(base/10) truncates to 0 for 0 < base < 10ns.
// withJitter must return such a base unchanged instead of panicking.
func TestWithJitter_SubTenNanos(t *testing.T) {
	for _, b := range []time.Duration{0, 1, 5, 9} {
		if got := withJitter(b); got != b {
			t.Errorf("withJitter(%v)=%v; want %v (no panic, unchanged)", b, got, b)
		}
	}
	// For base >= 10ns the result stays within [base, base+base/10).
	base := 100 * time.Millisecond
	got := withJitter(base)
	if got < base || got >= base+base/10 {
		t.Errorf("withJitter(%v)=%v outside [base, base+base/10)", base, got)
	}
}
