// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
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
	if modelID == "" {
		return fmt.Errorf("litellm: DeleteModel: empty model_id")
	}
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
// reconciler when status.lastRendered.modelID is empty (stale or
// first-run status) and the reconciler needs to resolve the LiteLLM entry
// by model_name before issuing POST /model/delete. Per OWN-01, this lookup
// is strictly by name (NOT a global LIST-and-prune).
//
// §9.1: only the name and status code are logged — no response body content.
func (c *Client) GetModelInfoByName(ctx context.Context, name string) (*ModelInfoResponse, error) {
	path := "/model/info?model_name=" + url.QueryEscape(name)
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		// 401 — propagate the typed error for the §7.7 fast-path.
		var auth401 *Auth401Error
		if errors.As(err, &auth401) {
			return nil, err
		}
		// 404 — entry absent. The deletion-path caller treats this as
		// "already deleted in LiteLLM" and removes the finalizer
		// without issuing DELETE. Post-2026-05-26 review F4: pre-fix,
		// the function returned the *RejectedError unchanged, stranding
		// finalizers on CRs whose status.lastRendered.modelID was empty.
		if IsNotFound(err) {
			return nil, nil
		}
		// Other 4xx + 5xx + network — propagate for the caller's
		// classification (LiteLLMRejected vs controller-runtime backoff).
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

// GetModelIDsByName returns the model_info.id of EVERY row LiteLLM lists
// under `name`. LiteLLM allows multiple deployments per model_name (POST
// /model/new is NOT idempotent), so the safety-relist must reason about the
// whole set — "does our tracked id still exist?" — rather than the first row
// only. Resolving by first-row id is what drove the id-drift duplicate-
// amplifier churn: a stray duplicate whose id != the tracked id was read as
// "id drift", clearing the tracked id → CREATE → another duplicate row →
// self-perpetuating (observed: 1718 rows for one model over 9 days). See
// vanish_probe + model_controller Step 7b.
//
// Order follows LiteLLM's response and is NOT guaranteed stable. Empty ids
// are skipped (defensive — a row with no model_info.id is not addressable
// for delete/probe). Error handling mirrors GetModelInfoByName exactly:
// 401 → typed *Auth401Error propagated; 404 OR empty data[] → (nil, nil)
// (the name is simply absent, NOT an error); other 4xx/5xx/network →
// propagated for the caller's classification.
//
// §9.1: only the name and status code are logged — no response body content.
func (c *Client) GetModelIDsByName(ctx context.Context, name string) ([]string, error) {
	path := "/model/info?model_name=" + url.QueryEscape(name)
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		// 401 — propagate the typed error for the §7.7 fast-path.
		var auth401 *Auth401Error
		if errors.As(err, &auth401) {
			return nil, err
		}
		// 404 — name absent. Treated as "not found", NOT an error.
		if IsNotFound(err) {
			return nil, nil
		}
		// Other 4xx + 5xx + network — propagate for classification.
		return nil, err
	}
	var list ModelListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /model/info?model_name: %w", err)
	}
	// Collect ALL exact-name matches (per OWN-01 per-name resolution),
	// skipping rows with an empty id. nil result on no matches → "not found".
	var ids []string
	for i := range list.Data {
		if list.Data[i].ModelName == name && list.Data[i].ModelInfo.ID != "" {
			ids = append(ids, list.Data[i].ModelInfo.ID)
		}
	}
	return ids, nil
}
