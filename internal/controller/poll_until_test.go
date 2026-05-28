// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

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
