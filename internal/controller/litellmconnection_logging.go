// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// reasonNoCallbacksReported is the LoggingHealthy condition reason
// surfaced when LiteLLM /key/health returns logging_callbacks: null
// (UAT LOW-01). Constant so the helper, the unit tests, and the
// envtest regression check stay in lockstep.
const reasonNoCallbacksReported = "NoCallbacksReported"

// computeLoggingHealthy maps a successful ProbeConnection result onto
// the secondary `LoggingHealthy` condition surfaced on LiteLLMConnection.
//
// Branches:
//   - LoggingStatus == "healthy"    → True,  Healthy
//   - LoggingStatus == "unhealthy"  → False, Unhealthy   (Details echoed verbatim)
//   - LoggingStatus == ""           → Unknown, NoCallbacksReported
//     (LiteLLM /key/health returned `logging_callbacks: null`; UAT LOW-01)
//   - any other token               → Unknown, Unknown
//
// Pure function; no I/O. Lives outside the controller method so the
// branch matrix can be unit-tested without spinning up envtest.
func computeLoggingHealthy(pr litellm.ProbeResult) (metav1.ConditionStatus, string, string) {
	switch pr.LoggingStatus {
	case "healthy":
		return metav1.ConditionTrue, "Healthy", "all logging callbacks healthy"
	case "unhealthy":
		msg := pr.LoggingDetails
		if msg == "" {
			msg = "one or more logging callbacks reported unhealthy"
		}
		return metav1.ConditionFalse, "Unhealthy", msg
	case "":
		return metav1.ConditionUnknown,
			reasonNoCallbacksReported,
			"LiteLLM /key/health returned no logging callbacks (logging_callbacks: null)"
	default:
		return metav1.ConditionUnknown,
			"Unknown",
			"unrecognized logging status: " + pr.LoggingStatus
	}
}
