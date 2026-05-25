// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
)

// KeyHealthResponse is the decoded body of POST /key/health.
//
// Shape (LiteLLM 1.83.10+):
//
//	{
//	  "key": "healthy",
//	  "logging_callbacks": {
//	    "callbacks": [...],
//	    "status":  "healthy" | "unhealthy",
//	    "details": "<free-form>"
//	  }
//	}
//
// The operator only consumes "key" (probe outcome — 200 here means the
// proxy honored the master key) and "logging_callbacks.status"/details
// (surfaced as the secondary LoggingHealthy condition on
// LiteLLMConnection).
type KeyHealthResponse struct {
	Key              string `json:"key"`
	LoggingCallbacks struct {
		Status  string `json:"status"`
		Details string `json:"details"`
	} `json:"logging_callbacks"`
}

// ProbeResult is the structured probe outcome returned by ProbeConnection.
//
// LoggingStatus / LoggingDetails are populated when ProbeConnection
// successfully reaches /key/health and the proxy returns a parseable
// body. On non-200 outcomes both fields are zero values and the caller
// must rely on the returned error.
type ProbeResult struct {
	LoggingStatus  string
	LoggingDetails string
}

// ProbeConnection issues POST /key/health with the master key and returns
// a ProbeResult plus nil on HTTP 200, *Auth401Error on 401, or a transient
// error on 5xx / network failure. Used by the LiteLLMConnection
// reconciler to set Ready=True / Ready=False AND a secondary
// LoggingHealthy condition.
//
// Why /key/health (and not /models or /health/{readiness,liveliness}):
//
//   - /health/liveliness and /health/readiness are public — no master-key
//     validation — so a wrong key on a healthy proxy returns 200 and
//     BadMasterKey detection collapses.
//   - /models is auth-gated but its 200 response carries no actionable
//     diagnostic for the operator status surface.
//   - /key/health is auth-gated (401 surfaces BadMasterKey identically
//     to the prior /models probe) AND returns the proxy's view of
//     itself: the master key was accepted, and the configured logging
//     callbacks are healthy. The latter is surfaced verbatim as the
//     LoggingHealthy condition.
//
// The empty POST body is intentional — the endpoint authenticates the
// caller (admin/master key) and reports the caller's own key health,
// not a body-supplied target.
//
// The filename keyinfo.go is retained for git-history continuity — the
// file owns the "connection probing" concern, not the specific endpoint.
func (c *Client) ProbeConnection(ctx context.Context) (ProbeResult, error) {
	body, err := c.makeRequest(ctx, "POST", "/key/health", nil)
	if err != nil {
		return ProbeResult{}, err
	}
	var parsed KeyHealthResponse
	// Best-effort decode: a malformed body on HTTP 200 is treated as
	// "logging health unknown" rather than a probe failure. The Ready
	// condition is driven by the HTTP status, not the body shape.
	_ = json.Unmarshal(body, &parsed)
	return ProbeResult{
		LoggingStatus:  parsed.LoggingCallbacks.Status,
		LoggingDetails: parsed.LoggingCallbacks.Details,
	}, nil
}
