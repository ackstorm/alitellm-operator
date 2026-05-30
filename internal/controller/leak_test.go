//go:build longidempotency
// +build longidempotency

// SPDX-License-Identifier: Apache-2.0

// Package controller — REL-03 1000-reconcile FD + goroutine leak harness.
//
// Build tag: longidempotency (not part of `make test-full`; runs via `make
// test-leak-soak` on nightly CI cadence).
//
// Procedure per 07-CONTEXT.md Claude's Discretion §AC-R3:
// 1. Create 1 Model CR; wait for 10 successful reconciles + 60s settle.
// 2. Sample baseline goroutine count + open FD count.
// 3. Trigger 1000 reconciles via annotation bumps (20ms spacing).
// 4. Wait 60s settle.
// 5. Assert goroutine + FD deltas within max(±10% baseline, ±5 absolute).
package controller

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestLeakHarness_1000Reconciles — REL-03 AC-R3: 1000-reconcile FD +
// goroutine leak harness.
//
// Assertion threshold: delta <= max(baseline * 10%, 5 absolute) for
// BOTH goroutines and open file descriptors (Linux-only via /proc/self/fd;
// the FD assertion is skipped on non-Linux platforms when FD count fails).
//
// Build-tag-gated (longidempotency); nightly CI via .github/workflows/nightly.yml.
func TestLeakHarness_1000Reconciles(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals — TestMain ordering bug")
	}
	if testing.Short() {
		t.Skip("skipping long-running leak harness in -short mode")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	resetConnCacheSnapshot()

	// Create the canary Model CR.
	canary := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "leak-canary",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{},
	}
	if err := k8sClient.Create(ctx, canary); err != nil {
		t.Fatalf("create leak-canary Model: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), canary, &client.DeleteOptions{}) })

	// ─── Phase 1: Baseline ────────────────────────────────────────────────
	// Wait for at least 10 successful reconciles of the canary CR.
	priorCalls := reconcileCalls.Load()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if reconcileCalls.Load() >= priorCalls+10 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reconcileCalls.Load() < priorCalls+10 {
		t.Fatalf("leak-canary: did not reach 10 reconciles within 30s (got %d calls, want %d)",
			reconcileCalls.Load()-priorCalls, 10)
	}

	// 60s settle: let any transient goroutines (e.g. informer caches) stabilise.
	t.Log("REL-03: waiting 60s for baseline to settle")
	time.Sleep(60 * time.Second)

	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := countOpenFDs(t)
	t.Logf("REL-03 baseline: goroutines=%d fds=%d", baselineGoroutines, baselineFDs)

	// ─── Phase 2: 1000 reconciles via annotation bumps ────────────────────
	t.Log("REL-03: triggering 1000 annotation-bump reconciles")
	for i := 0; i < 1000; i++ {
		var cur litellmv1alpha1.LiteLLMModel
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "leak-canary", Namespace: WatchNamespace}, &cur); err != nil {
			t.Fatalf("bump %d: Get leak-canary: %v", i, err)
		}
		if cur.Annotations == nil {
			cur.Annotations = make(map[string]string)
		}
		cur.Annotations["bump"] = strconv.Itoa(i)
		if err := k8sClient.Update(ctx, &cur); err != nil {
			// Conflict is benign — the next bump will pick up the latest version.
			t.Logf("bump %d: Update conflict (benign): %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ─── Phase 3: Post-settle measurement ────────────────────────────────
	t.Log("REL-03: waiting 60s for post-bump settle")
	time.Sleep(60 * time.Second)

	endGoroutines := runtime.NumGoroutine()
	endFDs := countOpenFDs(t)
	t.Logf("REL-03 end: goroutines=%d fds=%d", endGoroutines, endFDs)

	// ─── Phase 4: Assertions ──────────────────────────────────────────────
	if !withinLeakThreshold(baselineGoroutines, endGoroutines) {
		// Capture up to 20 lines of goroutine stack for debugging.
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		stackSnippet := string(buf[:n])
		if len(stackSnippet) > 3000 {
			stackSnippet = stackSnippet[:3000] + "\n... (truncated)"
		}
		t.Errorf("REL-03 FAIL: goroutine leak: baseline=%d end=%d (delta=%d, threshold=%d)\n%s",
			baselineGoroutines, endGoroutines, endGoroutines-baselineGoroutines,
			leakThreshold(baselineGoroutines), stackSnippet)
	}
	if baselineFDs > 0 && !withinLeakThreshold(baselineFDs, endFDs) {
		t.Errorf("REL-03 FAIL: FD leak: baseline=%d end=%d (delta=%d, threshold=%d)",
			baselineFDs, endFDs, endFDs-baselineFDs, leakThreshold(baselineFDs))
	}
}

// leakThreshold computes max(baseline * 10%, 5) — the allowable delta
// per 07-CONTEXT.md §AC-R3 specification.
func leakThreshold(baseline int) int {
	pct := baseline * 10 / 100
	if pct < 5 {
		return 5
	}
	return pct
}

// withinLeakThreshold returns true if end is within leakThreshold(baseline)
// of baseline (both under and over are acceptable — we are checking for leak,
// not shrinkage, so technically only the "over" case matters; but the
// symmetric form matches the spec language).
func withinLeakThreshold(baseline, end int) bool {
	delta := end - baseline
	if delta < 0 {
		delta = -delta
	}
	return delta <= leakThreshold(baseline)
}

// countOpenFDs reads /proc/self/fd to count open file descriptors.
// Linux-only: on non-Linux platforms (or when /proc is restricted) the
// function logs+returns 0 rather than t.Skipf'ing — the caller's
// `if baselineFDs > 0` guard at line ~136 handles a zero return safely,
// so the goroutine leak assertion (the primary test purpose) is preserved
// even when FD counting is unavailable.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Logf("REL-03: FD count unavailable (%v) — FD leak assertion will be skipped", err)
		return 0 // caller's `if baselineFDs > 0` guard handles this safely
	}
	return len(entries)
}
