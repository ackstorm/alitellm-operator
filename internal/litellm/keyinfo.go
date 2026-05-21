// SPDX-License-Identifier: Apache-2.0

package litellm

import "context"

// ProbeConnection issues GET /models with the master key and returns
// nil on HTTP 200, *Auth401Error on 401, or a transient error on
// 5xx / network failure. Used by the LiteLLMConnection reconciler
// (Phase 2) to set Ready=True / Ready=False.
//
// Why /models (NOT the legacy spec-§6.1 key-info path): the spike
// empirically verified that LITELLM_MASTER_KEY
// env var does NOT auto-store the key in the database, so the legacy
// path returns 404 "Key not found in database" with the master key.
// /models is
// auth-protected, returns 200 cheaply when LiteLLM is up AND the key
// is honored, and serves as both liveness and auth-validation in one
// call. .
//
// The filename keyinfo.go is retained for git-history continuity — the
// file owns the "connection probing" concern, not the specific endpoint.
func (c *Client) ProbeConnection(ctx context.Context) error {
	_, err := c.makeRequest(ctx, "GET", "/models", nil)
	return err
}
