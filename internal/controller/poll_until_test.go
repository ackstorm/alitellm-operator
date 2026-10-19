// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pollCR polls k8sClient.Get(WatchNamespace/name) and returns the
// first observed CR for which `pred` returns true. Returns the most
// recently observed CR on timeout (callers may then assert specific
// failures off the stale value for better diagnostics).
//
// Why this exists — flake reduction. The per-CR `poll*Condition`
// helpers across controller tests poll a SINGLE predicate (typically
// "Ready.Reason == X") and return immediately, leaving secondary
// fields up to the caller to read post-return. When a controller
// emits status in two separate Patches (the "two-write race"),
// the secondary field may not have landed yet → intermittent CI
// flake (`got "" want config` etc.).
//
// `pollCR` lets the caller express the FULL atomic predicate as a
// single closure — e.g. "Ready.Reason == X AND LastRendered.Y != ”"
// — so the return only happens once every field the test cares about
// has been observed. Controllers SHOULD use combined writers
// (writeStatus that sets all fields in a single retry-on-conflict
// Patch — see writeReadyAndLoggingHealthy on LiteLLMConnection) so
// `pollCR` is defense-in-depth rather than the only line of defense.
//
// Caller pattern:
//
//	got := pollCR[litellmv1alpha1.LiteLLMGuardRail](t, name,
//	    func(gr *litellmv1alpha1.LiteLLMGuardRail) bool {
//	        c := apimeta.FindStatusCondition(gr.Status.Conditions, "Ready")
//	        return c != nil && c.Reason == reasonConflictsWithConfigGuardrail &&
//	            gr.Status.LastRendered.DefinitionLocation != ""
//	    }, 5*time.Second)
//
// Existing per-CR helpers (pollGuardrailCondition, pollTeamCondition,
// pollMCPServerCondition, …) can migrate to thin wrappers over
// pollCR incrementally. They retain their "match Ready.Reason"-only
// behavior for callers that don't care about secondary fields.
//
// The `*T / T` constraint pair lets the helper allocate a fresh
// zero-value T per attempt and pass &T to client.Get — equivalent to
// `var obj T; k8sClient.Get(..., &obj)` but generic over the concrete
// CR type.
func pollCR[T any, PT interface {
	*T
	client.Object
}](
	t *testing.T,
	name string,
	pred func(PT) bool,
	timeout time.Duration,
) PT {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var last T
	for time.Now().Before(deadline) {
		var obj T
		pt := PT(&obj)
		if err := k8sClient.Get(context.Background(), key, pt); err == nil {
			if pred(pt) {
				return pt
			}
			last = obj
		}
		time.Sleep(50 * time.Millisecond)
	}
	return PT(&last)
}

// waitQuiet blocks until read() holds one value for `quiet`, then returns
// it; fails the test if that never happens within `timeout`. Use it to open
// a "nothing more may happen" window only after the reconcile burst that
// precedes it has drained: a fixed settle sleep is too short once the
// shared apiserver is under load, and the burst's tail then lands inside
// the window and reads as a storm/loop.
func waitQuiet(t *testing.T, read func() int64, quiet, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last, since := read(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if v := read(); v != last {
			last, since = v, time.Now()
		} else if time.Since(since) >= quiet {
			return last
		}
	}
	t.Fatalf("counter not quiet for %s within %s (last=%d)", quiet, timeout, last)
	return last
}

// mockReadsSince counts mock reads recorded at or after index `from`
// (a prior len(mockServer.Recorded())), skipping GET /v2/team/list: the
// suite's ungated TeamDefaultRunnable re-lists the implicit Team/default
// every 100ms whenever the connection cache is Ready, so that path is
// background noise for any read-count window.
func mockReadsSince(from int) int {
	n := 0
	for _, c := range mockServer.Recorded()[from:] {
		isRead := c.Method == http.MethodGet || c.Method == http.MethodHead ||
			(c.Method == http.MethodPost && c.Path == "/key/health")
		if isRead && c.Path != "/v2/team/list" {
			n++
		}
	}
	return n
}

// stripOwnerRefs performs the user's adoption gesture (drop the child's
// ownerReferences) and repeats it until the refs stay gone for `settle`.
// A Discovery reconcile already in flight when the strip lands still holds
// the pre-strip cache view and SSA-applies with ForceOwnership, which puts
// the controller ownerRef straight back; a single strip then silently
// un-adopts the child and the test waits for a skip that never comes.
// This works around a known PRODUCTION race (a real user's adoption is lost
// the same way), not a test artifact: tracked in GitHub issue
// https://github.com/ackstorm/alitellm-operator/issues/141. See CLAUDE.md
// "Adopting a Discovery child while a Discovery reconcile is in flight".
func stripOwnerRefs[T any, PT interface {
	*T
	client.Object
}](t *testing.T, key client.ObjectKey, settle, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var clearSince time.Time
	for time.Now().Before(deadline) {
		var obj T
		pt := PT(&obj)
		if err := k8sClient.Get(ctx, key, pt); err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		switch {
		case len(pt.GetOwnerReferences()) > 0:
			clearSince = time.Time{}
			pt.SetOwnerReferences(nil)
			if err := k8sClient.Update(ctx, pt); err != nil && !apierrors.IsConflict(err) {
				t.Fatalf("strip ownerReferences on %s: %v", key, err)
			}
		case clearSince.IsZero():
			clearSince = time.Now()
		case time.Since(clearSince) >= settle:
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ownerReferences on %s kept coming back for %s", key, timeout)
}
