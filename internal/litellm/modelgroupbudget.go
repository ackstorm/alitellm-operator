// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Model access-group (TAG) budgets — LiteLLM 1.100.0+. The path segment is
// a model_info.access_groups tag, NOT a /v1/access_group id. Shapes measured
// in references/litellm-model-group-budget-api.md.

// ModelGroupBudget is the `budget` object of GET /access_group/{tag}/budget.
type ModelGroupBudget struct {
	BudgetID       string   `json:"budget_id,omitempty"`
	MaxBudget      *float64 `json:"max_budget"`
	SoftBudget     *float64 `json:"soft_budget"`
	BudgetDuration *string  `json:"budget_duration"`
}

// ModelGroupBudgetRequest is the PUT body. Unset fields are omitted, never
// sent as null (LiteLLM ignores null; PUT merges per field). Clearing a field
// is the caller's DELETE-then-PUT. LiteLLM rejects any other key with 422.
type ModelGroupBudgetRequest struct {
	MaxBudget      *float64 `json:"max_budget,omitempty"`
	SoftBudget     *float64 `json:"soft_budget,omitempty"`
	BudgetDuration string   `json:"budget_duration,omitempty"`
}

func modelGroupBudgetPath(group string) (string, error) {
	if group == "" {
		return "", fmt.Errorf("litellm: empty model access group")
	}
	return "/access_group/" + url.PathEscape(group) + "/budget", nil
}

// GetModelGroupBudget returns (nil, nil) when the tag exists but carries no
// budget, and a 404 RejectedError (IsNotFound) when no model carries the tag.
func (c *Client) GetModelGroupBudget(ctx context.Context, group string) (*ModelGroupBudget, error) {
	path, err := modelGroupBudgetPath(group)
	if err != nil {
		return nil, err
	}
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Budget *ModelGroupBudget `json:"budget"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET %s: %w", path, err)
	}
	return out.Budget, nil
}

// PutModelGroupBudget upserts (merges into) the tag's budget.
func (c *Client) PutModelGroupBudget(ctx context.Context, group string, req *ModelGroupBudgetRequest) error {
	path, err := modelGroupBudgetPath(group)
	if err != nil {
		return err
	}
	_, err = c.makeRequest(ctx, "PUT", path, req)
	return err
}

// DeleteModelGroupBudget clears the tag's budget (the tag itself stays).
// A 404 is nil: makeRequest treats DELETE 404 as idempotent success.
func (c *Client) DeleteModelGroupBudget(ctx context.Context, group string) error {
	path, err := modelGroupBudgetPath(group)
	if err != nil {
		return err
	}
	_, err = c.makeRequest(ctx, "DELETE", path, nil)
	return err
}
