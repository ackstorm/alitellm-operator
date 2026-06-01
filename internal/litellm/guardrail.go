// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// CreateGuardrail issues POST /guardrails with body {"guardrail":Guardrail}.
//
// LiteLLM 2026-05 surface (BerriAI/litellm/proxy/guardrails/guardrail_endpoints.py):
// admin-only endpoint that persists the row to the DB and synchronously
// performs an in-memory initialization. On config error the server rolls
// back the DB entry, so the operator may retry idempotently. The response
// is the upstream `Guardrail` record with server-assigned guardrail_id.
func (c *Client) CreateGuardrail(ctx context.Context, req *GuardrailBody) (*GuardrailEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/guardrails", &CreateGuardrailRequest{Guardrail: req})
	if err != nil {
		return nil, err
	}
	return decodeGuardrailResponse(raw, "POST /guardrails")
}

// UpdateGuardrail issues PUT /guardrails/{guardrail_id} with body
// {"guardrail":Guardrail}. The server best-effort syncs the in-memory
// handler; failures only emit warnings server-side (no rollback). The
// operator should re-PUT on any subsequent reconcile to recover.
func (c *Client) UpdateGuardrail(ctx context.Context, guardrailID string, req *GuardrailBody) (*GuardrailEntry, error) {
	if guardrailID == "" {
		return nil, fmt.Errorf("litellm: UpdateGuardrail: empty guardrail_id")
	}
	path := "/guardrails/" + url.PathEscape(guardrailID)
	raw, err := c.makeRequest(ctx, "PUT", path, &CreateGuardrailRequest{Guardrail: req})
	if err != nil {
		return nil, err
	}
	return decodeGuardrailResponse(raw, "PUT /guardrails/{id}")
}

// DeleteGuardrail issues DELETE /guardrails/{guardrail_id}.
//
// LiteLLM removes the row from the DB and the in-memory handler. A DELETE
// that 404s is treated as success by makeRequest (idempotent delete, §7.7),
// so the finalizer path sees a nil error for an already-absent guardrail —
// there is no 4xx to inspect on this path.
func (c *Client) DeleteGuardrail(ctx context.Context, guardrailID string) error {
	if guardrailID == "" {
		return fmt.Errorf("litellm: DeleteGuardrail: empty guardrail_id")
	}
	path := "/guardrails/" + url.PathEscape(guardrailID)
	_, err := c.makeRequest(ctx, "DELETE", path, nil)
	return err
}

// ListGuardrails issues GET /v2/guardrails/list and returns every guardrail
// the caller's master key is allowed to see. Both DB-persisted rows (created
// via POST /guardrails) AND config-file rows are merged into the response;
// the entry's guardrail_definition_location field distinguishes the two.
//
// The legacy `/guardrails/list` endpoint is intentionally NOT called — it
// returns config-only rows and lacks the definition_location field needed
// by the operator's drift-correction loop.
//
// Use ListGuardrailByName for the operator's per-name resolution path; this
// raw helper is exposed for tests and for callers that need the full set
// (e.g. PoolSize sibling counting).
func (c *Client) ListGuardrails(ctx context.Context) (*GuardrailListResponse, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v2/guardrails/list", nil)
	if err != nil {
		return nil, err
	}
	var out GuardrailListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v2/guardrails/list: %w", err)
	}
	return &out, nil
}

// GetGuardrailByName issues GET /v2/guardrails/list and returns the first
// entry whose guardrail_name matches exactly. Returns (nil, nil) when no
// entry exists — the operator's idempotency probe (adoption path) treats
// this as "not yet created in LiteLLM" and falls through to POST.
//
// When multiple entries share guardrailName (load-balancing pool), the
// FIRST entry is returned. Callers that need the full pool should invoke
// ListGuardrails directly.
//
// 401 propagates as *Auth401Error via makeRequest. Other transport errors
// are returned as-is for controller-runtime backoff.
func (c *Client) GetGuardrailByName(ctx context.Context, guardrailName string) (*GuardrailEntry, error) {
	list, err := c.ListGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list.Guardrails {
		if list.Guardrails[i].GuardrailName == guardrailName {
			out := list.Guardrails[i]
			return &out, nil
		}
	}
	return nil, nil
}

// GetGuardrailByID issues GET /v2/guardrails/list and returns the entry
// whose guardrail_id matches exactly. Returns (nil, nil) when no entry
// exists — used by the operator's existence probe (safety re-list path)
// to detect out-of-band DELETEs of a specific LiteLLM row.
//
// Distinct from GetGuardrailByName because LB pools share guardrail_name
// across multiple rows; ID is the only stable per-row handle once the
// pool has more than one member.
func (c *Client) GetGuardrailByID(ctx context.Context, guardrailID string) (*GuardrailEntry, error) {
	list, err := c.ListGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list.Guardrails {
		if list.Guardrails[i].GuardrailID == guardrailID {
			out := list.Guardrails[i]
			return &out, nil
		}
	}
	return nil, nil
}

// decodeGuardrailResponse is a shared decoder for POST + PUT responses.
// LiteLLM returns the upstream `Guardrail` record either as a top-level
// object OR wrapped in {"guardrail": {.}} depending on the route version
// — accept both shapes (Probe-style robustness).
func decodeGuardrailResponse(raw []byte, opDesc string) (*GuardrailEntry, error) {
	// Try wrapped shape first.
	var wrapped struct {
		Guardrail *GuardrailEntry `json:"guardrail"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Guardrail != nil && wrapped.Guardrail.GuardrailName != "" {
		return wrapped.Guardrail, nil
	}
	// Fall back to flat shape (no wrapper).
	var flat GuardrailEntry
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("litellm: decode %s: %w", opDesc, err)
	}
	return &flat, nil
}

// NormalizeGuardrailMode lowercases a single mode value to match LiteLLM's
// server-side `normalize_lowercase` field validator in LitellmParams.mode.
// Used by the controller before hashing so a user-supplied "PRE_CALL"
// produces the same status.lastRendered.hash as "pre_call".
func NormalizeGuardrailMode(m string) string {
	return strings.ToLower(m)
}
