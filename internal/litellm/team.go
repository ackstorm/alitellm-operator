// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateTeam issues POST /team/new.
func (c *Client) CreateTeam(ctx context.Context, req *NewTeamRequest) (*TeamListEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/team/new", req)
	if err != nil {
		return nil, err
	}
	var out TeamListEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /team/new: %w", err)
	}
	return &out, nil
}

// UpdateTeam issues POST /team/update (POST — never the partial-update
// verb; see Pitfall 2).
func (c *Client) UpdateTeam(ctx context.Context, req *UpdateTeamRequest) (*TeamListEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/team/update", req)
	if err != nil {
		return nil, err
	}
	var out TeamListEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /team/update: %w", err)
	}
	return &out, nil
}

// DeleteTeam issues POST /team/delete with body {"team_ids": [.]}.
func (c *Client) DeleteTeam(ctx context.Context, teamIDs []string) error {
	_, err := c.makeRequest(ctx, "POST", "/team/delete", &DeleteTeamRequest{TeamIDs: teamIDs})
	return err
}

// CreateTeamRaw issues POST /team/new with a freeform map[string]any body.
//
// Used by the Team reconciler so the operator's "clearing
// budget" wire contract (spec §6.7 line 1194: "the operator's POST
// /team/update body always includes max_budget and budget_duration keys —
// set to the configured value when present, or to null when absent") is
// honored. The typed CreateTeam helper drops nil pointers via
// `,omitempty` JSON tags — that violates the explicit-null requirement.
// CreateTeamRaw bypasses the typed NewTeamRequest struct and posts the
// caller-built body verbatim, preserving JSON null for absent budget keys.
//
// On success, decodes the response into a (possibly sparse) *TeamListEntry.
// LiteLLM's POST /team/new response carries team_id + team_alias +
// max_budget + budget_duration; the operator persists team_id into
// status.lastRendered.teamID per Phase 5 D-02 / DEF-§6.4/§6.6-ID-PERSIST.
func (c *Client) CreateTeamRaw(ctx context.Context, body map[string]any) (*TeamListEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/team/new", body)
	if err != nil {
		return nil, err
	}
	var out TeamListEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /team/new (raw): %w", err)
	}
	return &out, nil
}

// UpdateTeamRaw issues POST /team/update with a freeform map[string]any
// body. Companion of CreateTeamRaw — see that helper's doc comment for
// the null-preservation rationale (spec §6.7 line 1194).
//
// The caller is responsible for setting body["team_id"] to the pinned
// status.lastRendered.teamID before calling (REQUIRED by the LiteLLM
// schema). The response body may be sparse — the operator's reconciler
// holds the canonical team_id in status, so a sparse response is benign.
func (c *Client) UpdateTeamRaw(ctx context.Context, body map[string]any) (*TeamListEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/team/update", body)
	if err != nil {
		return nil, err
	}
	var out TeamListEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /team/update (raw): %w", err)
	}
	return &out, nil
}

// ListTeamsByAlias issues GET /v2/team/list?team_alias=<alias>&page_size=100
// and returns ONLY teams whose TeamAlias exactly matches the requested
// alias. LiteLLM's server-side filter is partial (substring) per spec
// §6.7; the operator must apply an exact-match filter client-side to
// preserve the "operator never touches names it did not declare" invariant.
//
// Returns a (possibly empty) slice. Empty is NOT ErrNotFound — callers
// decide whether absence is a soft success (e.g. "create a new team") or
// an error.
func (c *Client) ListTeamsByAlias(ctx context.Context, alias string) ([]TeamListEntry, error) {
	path := "/v2/team/list?team_alias=" + url.QueryEscape(alias) + "&page_size=100"
	raw, err := c.makeRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var list TeamListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v2/team/list: %w", err)
	}
	// Client-side exact-match filter (§6.7).
	out := make([]TeamListEntry, 0, len(list.Teams))
	for _, t := range list.Teams {
		if t.TeamAlias == alias {
			out = append(out, t)
		}
	}
	return out, nil
}
