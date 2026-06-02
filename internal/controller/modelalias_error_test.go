// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// TestModelAliasErrorReason verifies the error->reason classifier picks
// reasonModelAliasRejected only for deterministic 4xx (non-401), and
// reasonLiteLLMUnavailable for transient (5xx / network / 401) errors —
// so the condition reason and the reconcile_total metric bucket agree
// with the actual failure class (review #4).
func TestModelAliasErrorReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"4xx rejected", &litellm.RejectedError{Method: "POST", Path: "/config/update", Status: 400, Code: "400"}, reasonModelAliasRejected},
		{"401 transient", &litellm.Auth401Error{Path: "/get/config/callbacks"}, reasonLiteLLMUnavailable},
		{"5xx transient", &litellm.RejectedError{Method: "GET", Path: "/get/config/callbacks", Status: 503, Code: "503"}, reasonLiteLLMUnavailable},
		{"network transient", errors.New("dial tcp: connection refused"), reasonLiteLLMUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelAliasErrorReason(tc.err); got != tc.want {
				t.Errorf("modelAliasErrorReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
