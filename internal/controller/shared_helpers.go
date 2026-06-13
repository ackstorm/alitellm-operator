// SPDX-License-Identifier: Apache-2.0

package controller

// shared_helpers.go holds package-level helpers extracted from the
// per-controller copy-paste (DRY consolidation, finding #14). Each helper
// is behavior-identical to the inline code it replaces.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
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

// classifyMutationError is the shared §7.7 LiteLLM-mutation error classifier
// used by the model/mcpserver/a2aagent/team/guardrail reconcilers (each keeps a
// thin per-type method that binds its CR and delegates here):
//   - Auth401Error → invalidate() + Ready=False/LiteLLMUnavailable via
//     writeStatusFn, then nil return (anti-storm REL-06: return nil, NOT err).
//   - deterministic 4xx (non-401) → Ready=False/LiteLLMRejected via
//     writeStatusFn + periodic requeue (FIX2 H-2), then nil return.
//   - 5xx / network transient → return err for controller-runtime backoff;
//     status left unchanged (OWN-09).
//
// writeStatusFn is the caller's writeStatus bound to its CR. The team
// controller's CR may be nil on the synthetic implicit-default reconcile, so
// its closure no-ops on nil — preserving the legacy `if team != nil` guard.
// invalidate is r.Cache.InvalidateOn401; requeueAfter is
// snap.NormalizedRequeueOnRejectedAfter. Behavior-identical to the inline
// per-controller copies it replaces.
func classifyMutationError(
	ctx context.Context,
	logger logr.Logger,
	err error,
	opDesc, kind string,
	writeStatusFn func(ctx context.Context, status metav1.ConditionStatus, reason, message string) error,
	invalidate func(),
	requeueAfter func() time.Duration,
) (ctrl.Result, error) {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		invalidate()
		msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
		if werr := writeStatusFn(ctx, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(kind, "success").Inc()
		return ctrl.Result{}, nil // anti-storm: return nil, NOT err
	}

	errStr := err.Error()
	if is4xxStatus(err) {
		// Deterministic 4xx — LiteLLMRejected. rejectedMessage surfaces the
		// parsed envelope detail when available (FIX2 M-5).
		msg := rejectedMessage(opDesc, err, errStr)
		if werr := writeStatusFn(ctx, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", errStr)
		metrics.ReconcileTotal.WithLabelValues(kind, "success").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter()}, nil
	}

	// 5xx / network transient — return err for controller-runtime backoff
	// (REL-02). Do NOT writeStatus on transient path (OWN-09: leave previous
	// status unchanged).
	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", errStr)
	metrics.ReconcileTotal.WithLabelValues(kind, "error").Inc()
	return ctrl.Result{}, err
}

// checkDuplicateSecretAs enforces SEC-03 uniqueness of spec.secrets[].as
// values. It returns a non-empty InvalidConfig message naming the first
// duplicated `as` value (the first entry whose `as` was already seen), or ""
// if all are unique. The parenthetical wording differs slightly across
// controllers (e.g. "SEC-03: must be unique within a LiteLLMModel",
// "must be unique within an A2AAgent", "must be unique within a
// LiteLLMGuardRail"), so the caller passes it verbatim as uniquenessPhrase to
// keep the message bytes identical to the inline blocks this replaces.
func checkDuplicateSecretAs(secrets []litellmv1alpha1.SecretSubstitution, uniquenessPhrase string) string {
	seen := make(map[string]struct{}, len(secrets))
	for _, e := range secrets {
		if _, dup := seen[e.As]; dup {
			return fmt.Sprintf("spec.secrets[]: duplicate as value %q (%s)", e.As, uniquenessPhrase)
		}
		seen[e.As] = struct{}{}
	}
	return ""
}

// secretRefNames returns the SecretRef.Name of each entry in a secrets slice,
// for field-indexer registration. Behavior-identical to the inline loops in
// the per-kind Index*SecretRefs funcs: every entry's name is appended
// unconditionally (no empty-name skip, no dedup), and an empty slice is
// returned (non-nil) when secrets is empty.
func secretRefNames(secrets []litellmv1alpha1.SecretSubstitution) []string {
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// writeStatusWithRetry is the shared optimistic-locked status-write core used
// by the per-controller writeStatus methods. On each attempt it re-Gets a
// fresh copy of obj into fresh (so a 409 conflict is resolved by re-applying
// onto the latest resourceVersion rather than leaking to controller-runtime
// and re-entering Reconcile with stale status — which previously caused
// duplicate LiteLLM POSTs), runs apply to mutate fresh's status, and persists
// it via Status().Update. On success `fresh` holds the persisted object
// (post-update resourceVersion); the caller propagates fresh.Status back onto
// its in-memory CR. Extracted verbatim from the byte-identical
// model/a2aagent/guardrail bodies; mcpserver/team are standardized onto it
// (they previously used a bare Status().Update with no conflict-retry).
func writeStatusWithRetry[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	fresh T,
	apply func(fresh T),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
			return err
		}
		apply(fresh)
		return c.Status().Update(ctx, fresh)
	})
}

// SafetyRelistRunnable is the §7.6 safety re-list runnable shared by the model
// and guardrail controllers. On each Interval tick it lists all CRs of one
// kind in Namespace (via ListRequests) and non-blockingly enqueues their
// reconcile.Requests on RequeueCh (a full channel skips the item — the next
// tick retries). REL-02: uses a time.Ticker inside a manager.Runnable, never
// RequeueAfter. Replaces the byte-identical ModelSafetyRelistRunnable and
// GuardRailSafetyRelistRunnable; only ListRequests (the kind-specific list)
// and LogLabel differ.
type SafetyRelistRunnable struct {
	Client    client.Client
	Namespace string
	Interval  time.Duration
	Log       logr.Logger
	// RequeueCh is the channel the runnable writes reconcile.Requests to.
	// SetupWithManager wires this as a source.Channel / source.TypedFunc watch.
	RequeueCh chan reconcile.Request
	// ListRequests lists every CR of one kind in namespace and returns their
	// reconcile.Requests, or an error to skip this tick.
	ListRequests func(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error)
	// LogLabel names the kind in the V(1) debug lines (e.g. "models").
	LogLabel string
}

func (r *SafetyRelistRunnable) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			reqs, err := r.ListRequests(ctx, r.Client, r.Namespace)
			if err != nil {
				r.Log.V(1).Info("safety re-list: list failed; skipping tick", "kind", r.LogLabel, "error", err)
				continue
			}
			for _, req := range reqs {
				select {
				case r.RequeueCh <- req:
				default:
					// Channel full — skip this item; retried on next tick.
				}
			}
			r.Log.V(1).Info("safety re-list: enqueued", "kind", r.LogLabel, "count", len(reqs))
		}
	}
}
