// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestResolvedSafetyRelistInterval — H5: the single resolver main.go calls
// for BOTH the package vars and all three runnables. Unset → canonical
// default; sub-floor → error; valid override → parsed value.
func TestResolvedSafetyRelistInterval(t *testing.T) {
	d, err := ResolvedSafetyRelistInterval("") // unset → canonical default
	if err != nil || d != DefaultSafetyRelistInterval {
		t.Fatalf("unset: got %v, %v; want %v", d, err, DefaultSafetyRelistInterval)
	}
	if _, err := ResolvedSafetyRelistInterval("3s"); err == nil {
		t.Fatal("sub-floor must error")
	}
	if _, err := ResolvedSafetyRelistInterval("notaduration"); err == nil {
		t.Fatal("malformed must error")
	}
	d, _ = ResolvedSafetyRelistInterval("45m")
	if d != 45*time.Minute {
		t.Fatalf("override: got %v, want 45m", d)
	}
}

// TestParseSafetyRelistInterval_EmptyReturnsZero — empty input keeps
// the package defaults (zero signals "no override").
func TestParseSafetyRelistInterval_EmptyReturnsZero(t *testing.T) {
	got, err := ParseSafetyRelistInterval("")
	if err != nil {
		t.Fatalf("empty: unexpected err: %v", err)
	}
	if got != 0 {
		t.Errorf("empty: want 0, got %v", got)
	}
}

// TestParseSafetyRelistInterval_Valid — duration strings within bounds
// parse and round-trip.
func TestParseSafetyRelistInterval_Valid(t *testing.T) {
	for _, raw := range []string{"5s", "10s", "30s", "1m", "10m", "1h", "24h"} {
		got, err := ParseSafetyRelistInterval(raw)
		if err != nil {
			t.Errorf("%q: unexpected err: %v", raw, err)
			continue
		}
		want, _ := time.ParseDuration(raw)
		if got != want {
			t.Errorf("%q: got %v, want %v", raw, got, want)
		}
	}
}

// TestParseSafetyRelistInterval_BelowFloor — sub-5s rejected.
func TestParseSafetyRelistInterval_BelowFloor(t *testing.T) {
	for _, raw := range []string{"1s", "4s", "1ms"} {
		if _, err := ParseSafetyRelistInterval(raw); err == nil {
			t.Errorf("%q: expected floor rejection, got nil err", raw)
		}
	}
}

// TestParseSafetyRelistInterval_Malformed — non-duration strings error.
func TestParseSafetyRelistInterval_Malformed(t *testing.T) {
	for _, raw := range []string{"abc", "10", "30 seconds", "-5m"} {
		if _, err := ParseSafetyRelistInterval(raw); err == nil {
			t.Errorf("%q: expected parse error, got nil err", raw)
		}
	}
}

// TestSetSafetyRelistIntervals_SetsAllFour — single setter updates the
// four package-level vars. Restore defaults after to avoid bleed-through
// to other tests in the package.
func TestSetSafetyRelistIntervals_SetsAllFour(t *testing.T) {
	origMCP := mcpSafetyRelistInterval
	origModel := modelSafetyRelistInterval
	origTeam := teamSafetyRelistInterval
	origA2A := a2aAgentSafetyRelistInterval
	t.Cleanup(func() {
		mcpSafetyRelistInterval = origMCP
		modelSafetyRelistInterval = origModel
		teamSafetyRelistInterval = origTeam
		a2aAgentSafetyRelistInterval = origA2A
	})

	SetSafetyRelistIntervals(7 * time.Minute)

	for name, got := range map[string]time.Duration{
		"mcp":   mcpSafetyRelistInterval,
		"model": modelSafetyRelistInterval,
		"team":  teamSafetyRelistInterval,
		"a2a":   a2aAgentSafetyRelistInterval,
	} {
		if got != 7*time.Minute {
			t.Errorf("%s: want 7m, got %v", name, got)
		}
	}
}

// TestSetSafetyRelistIntervals_ZeroIsNoop — zero / negative keep
// existing values (signals "no override" path from ParseSafetyRelistInterval).
func TestSetSafetyRelistIntervals_ZeroIsNoop(t *testing.T) {
	origMCP := mcpSafetyRelistInterval
	t.Cleanup(func() { mcpSafetyRelistInterval = origMCP })

	mcpSafetyRelistInterval = 13 * time.Minute
	SetSafetyRelistIntervals(0)
	if mcpSafetyRelistInterval != 13*time.Minute {
		t.Errorf("zero-noop: want 13m preserved, got %v", mcpSafetyRelistInterval)
	}
	SetSafetyRelistIntervals(-1 * time.Second)
	if mcpSafetyRelistInterval != 13*time.Minute {
		t.Errorf("negative-noop: want 13m preserved, got %v", mcpSafetyRelistInterval)
	}
}

// TestSafetyRelistRunnable_EnqueueIsLossless — the tick loop must not drop
// items when RequeueCh is full. ListRequests yields its batch exactly once,
// so any dropped item is never re-offered and the receive below times out.
func TestSafetyRelistRunnable_EnqueueIsLossless(t *testing.T) {
	reqs := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns"}},
		{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns"}},
		{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns"}},
	}
	var served atomic.Bool
	ch := make(chan reconcile.Request, 1) // cap 1 < len(reqs): forces the full-channel path

	r := &SafetyRelistRunnable{
		Interval:  10 * time.Millisecond,
		Log:       logr.Discard(),
		RequeueCh: ch,
		ListRequests: func(_ context.Context, _ client.Client, _ string) ([]reconcile.Request, error) {
			if served.CompareAndSwap(false, true) {
				return reqs, nil
			}
			return nil, nil
		},
		LogLabel: "test",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	got := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(got) < len(reqs) {
		select {
		case req := <-ch:
			got[req.Name] = true
		case <-deadline:
			t.Fatalf("lossy enqueue: got %d/%d requests (%v)", len(got), len(reqs), got)
		}
	}
}

// TestListRequests_CoversEveryKind — each List*Requests must return one
// reconcile.Request per CR of its kind in the namespace. These feed the
// SafetyRelistRunnable; a kind missing from the list is a kind with no
// safety net.
func TestListRequests_CoversEveryKind(t *testing.T) {
	ctx := context.Background()

	team := teamSampleCR("relist-team")
	mcp := mcpServerSampleCR("relist-mcp")
	a2a := a2aSampleCR("relist-a2a")

	for _, o := range []client.Object{team, mcp, a2a} {
		if err := k8sClient.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T: %v", o, err)
		}
		o := o
		t.Cleanup(func() {
			o.SetFinalizers(nil)
			_ = k8sClient.Update(context.Background(), o)
			_ = k8sClient.Delete(context.Background(), o)
		})
	}

	cases := []struct {
		name string
		fn   func(context.Context, client.Client, string) ([]reconcile.Request, error)
		want string
	}{
		{"teams", ListTeamRequests, "relist-team"},
		{"mcpservers", ListMCPServerRequests, "relist-mcp"},
		{"a2aagents", ListA2AAgentRequests, "relist-a2a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := tc.fn(ctx, k8sClient, WatchNamespace)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for _, r := range reqs {
				if r.Name == tc.want && r.Namespace == WatchNamespace {
					return
				}
			}
			t.Fatalf("%s: %q not in %v", tc.name, tc.want, reqs)
		})
	}
}
