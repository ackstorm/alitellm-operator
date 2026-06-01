// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// buildReadyCondition builds the "Ready" status condition shared by every
// reconciler's writeStatus prologue (M-Q5). The five non-connection
// reconcilers (Team, Model, MCPServer, A2AAgent, GuardRail) constructed a
// byte-identical literal; their divergent write tail (plain Update vs
// RetryOnConflict) stays at the call site. The connection reconciler's
// writeStatus is excluded — it has a different signature and a one-hot
// ConnectionReady gauge.
func buildReadyCondition(gen int64, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	}
}

// recordReconcileMetric increments the per-kind reconcile-result counter
// (M-Q5) — the metrics tail shared by the same five writeStatus methods.
func recordReconcileMetric(kind, ns, reason string) {
	metrics.LitellmOperatorReconcileTotal.WithLabelValues(
		kind, ns, metrics.ReasonToReconcileResult(reason),
	).Inc()
}
