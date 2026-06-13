// SPDX-License-Identifier: Apache-2.0

package controller

// shared_helpers.go holds package-level helpers extracted from the
// per-controller copy-paste (DRY consolidation, finding #14). Each helper
// is behavior-identical to the inline code it replaces.

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// rejectedStatus extracts the HTTP status of a *litellm.RejectedError, or 0
// if err is not a RejectedError (nil, transport/5xx, or Auth401Error).
func rejectedStatus(err error) int {
	var rej *litellm.RejectedError
	if errors.As(err, &rej) {
		return rej.Status
	}
	return 0
}

// is4xxStatus reports whether err is a deterministic LiteLLM 4xx rejection.
// Uses the typed RejectedError.Status (errors.As-based) so it survives error
// wrapping — unlike the legacy error-string prefix scan it replaces. Note an
// Auth401Error is NOT a RejectedError, so 401 reads as "not 4xx" here; callers
// gate the 401 fast-path before reaching this helper (same contract as the
// legacy is4xxError / 400-loop scanners, which never matched Auth401Error's
// "litellm: 401 unauthorized on ." string either).
func is4xxStatus(err error) bool {
	s := rejectedStatus(err)
	return s >= 400 && s < 500
}

// newAckMissingFn builds the deletion-path "ack-missing" handler shared by all
// controllers. Under deletionPolicy=Delete it records DeletionBlocked, emits a
// Warning Event, and returns a non-nil error (finalizer retained); otherwise
// (Orphan) it increments DeletionOrphanedTotal, emits a Normal Event, and
// returns nil (caller drains the finalizer). Behavior-identical to the inline
// closures it replaces — only kind/obj/namespace/name/policy are parameterized.
func newAckMissingFn(
	rec record.EventRecorder,
	obj client.Object,
	kind, namespace, name string,
	policy deletionpolicy.Policy,
) func(reason string) error {
	return func(reason string) error {
		if policy == deletionpolicy.Delete {
			metrics.DeletionBlocked.Record(kind, namespace, name)
			rec.Eventf(obj, corev1.EventTypeWarning, "LiteLLMDeleteBlocked",
				"deletionPolicy=Delete and LiteLLM ack missing (%s); finalizer retained", reason)
			return fmt.Errorf("delete blocked: %s", reason)
		}
		metrics.DeletionOrphanedTotal.WithLabelValues(kind).Inc()
		rec.Eventf(obj, corev1.EventTypeNormal, "LiteLLMDeleteOrphaned",
			"deletionPolicy=Orphan and LiteLLM ack missing (%s); finalizer removed; entry may persist", reason)
		return nil
	}
}
