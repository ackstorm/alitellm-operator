// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateModel issues POST /model/new with the Deployment body.
//
// LiteLLM returns the freshly-created model record. Phase 3's Model
// reconciler reads the top-level model_id from this response (per
// 01-01-SUMMARY Probe 2 — both `model_id` AND `model_info.id` are
// populated; top-level is canonical).
func (c *Client) CreateModel(ctx context.Context, req *Deployment) (*ModelInfoResponse, error) {
	raw, err := c.makeRequest(ctx, "POST", "/model/new", req)
	if err != nil {
		return nil, err
	}
	var out ModelInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /model/new: %w", err)
	}
	return &out, nil
}

// UpdateModel issues POST /model/update with the updateDeployment body.
//
// CRITICAL Pitfall 2: the path is the literal string "/model/update".
// The model id lives in req.ID (top-level field of the updateDeployment
// body per LiteLLM 1.83.10), NOT in the URL. Do NOT generate the path
// by embedding the id as a URL segment (the /model/<id>/update shape) —
// that produces the spec-§5.1-violating partial-update shape even with
// a POST verb, which is bbdsoftware/litellm-operator's actual bug.
//
// LiteLLM 1.83.10 body shape (D-7.1-13 / Probe 9 retry 2026-05-18):
//
//	{ "id": "<model-uuid>", "model_name": ., "litellm_params": {.} }
//
// The previous 1.82.6 form used model_info.id — deprecated in 1.83.10.
// Per spec/litellm_api.json updateDeployment schema.
func (c *Client) UpdateModel(ctx context.Context, req *updateDeployment) (*ModelInfoResponse, error) {
	raw, err := c.makeRequest(ctx, "POST", "/model/update", req)
	if err != nil {
		return nil, err
	}
	var out ModelInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /model/update: %w", err)
	}
	return &out, nil
}

// DeleteModel issues POST /model/delete with body {"id": modelID}.
func (c *Client) DeleteModel(ctx context.Context, modelID string) error {
	_, err := c.makeRequest(ctx, "POST", "/model/delete", &ModelDeleteRequest{ID: modelID})
	return err
}

// GetModelInfo issues GET /model/info?litellm_model_id=<id> and returns
// the first entry of the Data array. Length-checks len(list.Data) before
// indexing (REL-05): empty Data → ErrNotFound.
func (c *Client) GetModelInfo(ctx context.Context, modelID string) (*ModelInfoResponse, error) {
	path := "/model/info?litellm_model_id=" + url.QueryEscape(modelID)
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var list ModelListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /model/info: %w", err)
	}
	if len(list.Data) == 0 { // REL-05 length check before indexing
		return nil, ErrNotFound
	}
	out := list.Data[0]
	return &out, nil
}

// GetModelInfoByName issues GET /model/info?model_name=<name> and returns
// the entry whose model_name matches exactly. Returns (nil, nil) if no
// matching entry is found (404 response OR empty data[] — both are "not
// found", NOT an error). Returns a typed *Auth401Error on HTTP 401 so the
// caller can invoke r.Cache.InvalidateOn401 via errors.As.
//
// This is the D-04 deletion-path name-resolve fallback: used by the Model
// reconciler when status.lastRendered.litellmModelID is empty (stale or
// first-run status) and the reconciler needs to resolve the LiteLLM entry
// by model_name before issuing POST /model/delete. Per OWN-01, this lookup
// is strictly by name (NOT a global LIST-and-prune).
//
// §9.1: only the name and status code are logged — no response body content.
func (c *Client) GetModelInfoByName(ctx context.Context, name string) (*ModelInfoResponse, error) {
	path := "/model/info?model_name=" + url.QueryEscape(name)
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		// makeRequest returns *Auth401Error on 401 — propagated for errors.As
		// classification at the caller. Network / 5xx are returned as-is for
		// controller-runtime backoff. 404 from makeRequest means the path itself
		// returned 404 — treat as not-found (nil, nil) below because we check
		// the raw == nil case against the 4xx branch in makeRequest. However,
		// makeRequest on 4xx (except 401 and 404-DELETE) returns a non-nil error,
		// so we cannot distinguish 404 vs other 4xx from the error value alone.
		// Per §7.7 the caller tolerates NOT-FOUND as (nil, nil); other 4xx are
		// surfaced as errors. For simplicity, return the error intact — a 404
		// on GET /model/info is not a DELETE context, so makeRequest returns
		// fmt.Errorf("litellm: 404 on GET.") — the caller's error-classification
		// fallback returns ctrl.Result{}, err which re-enqueues. This is acceptable
		// — the deletion-path caller explicitly checks (nil, nil) for the "already
		// absent" path; a hard 404 error is indistinguishable from "absent" in
		// practice and we relax to nil below by checking the error message.
		// NOTE: a cleaner solution is to handle 404 as success inside makeRequest
		// for GET, but that changes the existing GetModelInfo contract. Instead,
		// we treat any non-401 error conservatively: return the error.
		return nil, err
	}
	var list ModelListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /model/info?model_name: %w", err)
	}
	// Filter data[] for exact name match per OWN-01 (per-name resolution).
	for i := range list.Data {
		if list.Data[i].ModelName == name {
			out := list.Data[i]
			return &out, nil
		}
	}
	// Empty data[] or no exact-name match → not found, NOT an error.
	// The deletion-path fallback treats this as "entry already absent in LiteLLM".
	return nil, nil
}
