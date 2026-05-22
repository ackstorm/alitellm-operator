// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestTransientBackoffOptions_CappedAt30s asserts that the rate limiter
// returned by transientBackoffOptions never produces a delay above the
// 30s cap, even after a long failure burst. Regression for FIX.txt M-3a
// (2026-05-22 prod: an existing MCPServer stuck in the backoff queue
// after a brief LiteLLM blip, recovery time unbounded).
func TestTransientBackoffOptions_CappedAt30s(t *testing.T) {
	opts := transientBackoffOptions()
	if opts.RateLimiter == nil {
		t.Fatal("RateLimiter unset")
	}
	req := reconcile.Request{}
	var maxDelay time.Duration
	// 100 failure events drives the exponential well past the cap
	// (without the cap, 200ms × 2^99 overflows).
	for i := 0; i < 100; i++ {
		d := opts.RateLimiter.When(req)
		if d > maxDelay {
			maxDelay = d
		}
	}
	if maxDelay > transientBackoffMaxDelay {
		t.Fatalf("backoff exceeded cap: max=%v want ≤ %v (FIX.txt M-3a)", maxDelay, transientBackoffMaxDelay)
	}
	// Confirm the limiter actually backs off (sanity — not pinned at 0).
	if maxDelay < transientBackoffBaseDelay {
		t.Errorf("backoff never grew above base delay (got %v); rate limiter degenerate?", maxDelay)
	}
}
