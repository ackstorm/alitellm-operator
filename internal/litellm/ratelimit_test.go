// SPDX-License-Identifier: Apache-2.0

// ratelimit_test.go — FIX2.txt M-10 regression guards on the
// WithRateLimit ClientOption. Asserts:
//
//  1. WithRateLimit(rps, burst) installs a limiter that gates outbound
//     HTTP requests at the configured token-bucket rate.
//  2. WithRateLimit(0, .) disables the limiter (no token wait — fast
//     path identical to the unconfigured default).
//  3. ctx cancellation during the token wait surfaces as a wrapped
//     error AND short-circuits without firing the HTTP request.
//
// The thundering-herd scenario the option fixes (~30 boot-time POSTs
// into a modestly-stressed proxy) is end-to-end exercised by the
// envtest manager wiring; this file locks the option's contract in
// isolation.

package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestWithRateLimit_GatesOutboundRate(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	// rps=5, burst=1 — second call must wait ~200ms after the first
	// drains the burst bucket.
	c := NewClient(srv.URL, testMasterKey, logr.Discard(), WithRateLimit(5, 1))

	ctx := context.Background()
	start := time.Now()
	if _, err := c.ProbeConnection(ctx); err != nil {
		t.Fatalf("ProbeConnection #1: %v", err)
	}
	if _, err := c.ProbeConnection(ctx); err != nil {
		t.Fatalf("ProbeConnection #2: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("two probes at rps=5 burst=1 elapsed %v; want >= 150ms (token bucket should gate the 2nd request)", elapsed)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits: got %d, want 2", got)
	}
}

func TestWithRateLimit_DisabledOnZeroRPS(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testMasterKey, logr.Discard(), WithRateLimit(0, 1))
	if c.limiter != nil {
		t.Fatalf("WithRateLimit(0, 1) left limiter non-nil; want disabled")
	}

	// Five back-to-back probes should land within a small window when
	// the limiter is off (no token wait).
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := c.ProbeConnection(ctx); err != nil {
			t.Fatalf("ProbeConnection #%d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("five probes with limiter disabled took %v; want < 500ms (no token wait)", elapsed)
	}
	if got := hits.Load(); got != 5 {
		t.Errorf("server hits: got %d, want 5", got)
	}
}

func TestWithRateLimit_ContextCancelShortCircuits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	// rps=0.1, burst=1 — first probe drains the burst; second probe
	// would block for ~10s waiting for the next token. Cancel the
	// context after 50ms; the limiter wait must surface a wrapped
	// cancellation error and NOT fire the HTTP request.
	c := NewClient(srv.URL, testMasterKey, logr.Discard(), WithRateLimit(0.1, 1))

	ctx := context.Background()
	if _, err := c.ProbeConnection(ctx); err != nil {
		t.Fatalf("ProbeConnection #1: %v", err)
	}

	ctx2, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ProbeConnection(ctx2)
	if err == nil {
		t.Fatalf("ProbeConnection #2: want error from ctx cancel, got nil")
	}
	// golang.org/x/time/rate returns its own "would exceed context
	// deadline" string instead of chaining context.DeadlineExceeded, so
	// match by substring (locks the wrap-and-prefix the client code does
	// in client.go:163).
	if !strings.Contains(err.Error(), "rate limiter wait") {
		t.Errorf("err message should mention rate limiter wait: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits after cancelled #2: got %d, want 1 (cancelled probe must NOT reach the server)", got)
	}
}
