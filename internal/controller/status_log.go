// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusReadyUnchanged reports whether the proposed (status, reason, message)
// would produce a no-op write against the existing Ready condition AND the
// recorded ObservedGeneration already matches the live Generation. Callers
// short-circuit `Status().Update`/`Patch` on a true return to avoid pointless
// resourceVersion bumps and the 409 storms that follow.
func statusReadyUnchanged(
	conds []metav1.Condition,
	observedGen, gen int64,
	status metav1.ConditionStatus,
	reason, message string,
) bool {
	if observedGen != gen {
		return false
	}
	cur := apimeta.FindStatusCondition(conds, "Ready")
	if cur == nil {
		return false
	}
	return cur.Status == status && cur.Reason == reason && cur.Message == message
}

// logStatusUpdateErr emits the standard WR-03 capture-and-log line for a
// failed status subresource update. Conflict errors (HTTP 409, normal in
// envtest and during competing reconciles) are demoted to V(1) so they no
// longer appear at the default verbosity. All other errors stay at Error
// level so genuine storms remain observable.
func logStatusUpdateErr(logger logr.Logger, err error, keysAndValues ...any) {
	if err == nil {
		return
	}
	if apierrors.IsConflict(err) {
		// Pass err itself (not err.Error()) so structured logr backends can
		// render the typed error or extract status details if they choose.
		kv := append([]any{"error", err}, keysAndValues...)
		logger.V(1).Info("status update conflict (expected; retried by controller-runtime)", kv...)
		return
	}
	logger.Error(err, "status update failed", keysAndValues...)
}
