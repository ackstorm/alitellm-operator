// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// Tests for computeLoggingHealthy. Maps internal/litellm.ProbeResult →
// (ConditionStatus, reason, message) for the LoggingHealthy condition
// on LiteLLMConnection. Contract:
//   - status "healthy"            → True,    Healthy
//   - status "unhealthy"          → False,   Unhealthy   (Details echoed)
//   - status "" + Details ""      → Unknown, NoCallbacksReported
//     (LiteLLM /key/health returned logging_callbacks: null — see LOW-01)
//   - anything else               → Unknown, Unknown
func TestComputeLoggingHealthy_Healthy(t *testing.T) {
	s, r, m := computeLoggingHealthy(litellm.ProbeResult{LoggingStatus: "healthy"})
	if s != metav1.ConditionTrue || r != "Healthy" || m == "" {
		t.Fatalf("healthy branch: got (%v,%q,%q)", s, r, m)
	}
}

func TestComputeLoggingHealthy_Unhealthy_EchoesDetails(t *testing.T) {
	s, r, m := computeLoggingHealthy(litellm.ProbeResult{
		LoggingStatus:  "unhealthy",
		LoggingDetails: "langfuse: 401",
	})
	if s != metav1.ConditionFalse || r != "Unhealthy" {
		t.Fatalf("unhealthy branch: got (%v,%q)", s, r)
	}
	if m != "langfuse: 401" {
		t.Fatalf("expected Details to be surfaced verbatim, got %q", m)
	}
}

func TestComputeLoggingHealthy_NoCallbacksReported(t *testing.T) {
	// LiteLLM /key/health returns `logging_callbacks: null` when no
	// callbacks are configured. ProbeConnection parses this into
	// empty Status + empty Details. Before LOW-01 the message was
	// "logging callbacks: " (trailing space, no value).
	s, r, m := computeLoggingHealthy(litellm.ProbeResult{})
	if s != metav1.ConditionUnknown {
		t.Fatalf("empty status: want Unknown, got %v", s)
	}
	if r != "NoCallbacksReported" {
		t.Fatalf("empty status: want reason NoCallbacksReported, got %q", r)
	}
	if m == "" || m == "logging callbacks: " {
		t.Fatalf("empty status: message must be non-trivial, got %q", m)
	}
}

func TestComputeLoggingHealthy_UnknownStatusFallsThrough(t *testing.T) {
	s, r, m := computeLoggingHealthy(litellm.ProbeResult{LoggingStatus: "weird-future-value"})
	if s != metav1.ConditionUnknown || r != "Unknown" {
		t.Fatalf("unknown status: got (%v,%q)", s, r)
	}
	if m == "" {
		t.Fatalf("unknown status: message must be non-empty (carries the unknown token), got %q", m)
	}
}
