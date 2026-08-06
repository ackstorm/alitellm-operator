// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateAccessGroup issues POST /v1/access_group and returns the created
// entry. LiteLLM MINTS access_group_id — a caller-supplied one is ignored
// (verified 1.93.0), so the caller must read it from the response, exactly
// like MCP toolset_id and A2A agent_id. The endpoint answers 201.
//
// The reconciler is expected to call GetAccessGroupByName first and only POST
// on a nil result: "already exists" semantics live at the controller layer.
func (c *Client) CreateAccessGroup(ctx context.Context, req *AccessGroupCreateRequest) (*AccessGroupEntry, error) {
	if req.AccessGroupName == "" {
		return nil, fmt.Errorf("litellm: CreateAccessGroup: empty access_group_name")
	}
	nonNilAccessGroupLists(&req.AccessModelNames, &req.AccessMCPServerIDs, &req.AccessAgentIDs)
	raw, err := c.makeRequest(ctx, "POST", "/v1/access_group", req)
	if err != nil {
		return nil, err
	}
	var out AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /v1/access_group: %w", err)
	}
	return &out, nil
}

// ListAccessGroups issues GET /v1/access_group. The response is a BARE array
// (the legacy /access_group/list is the one that wraps in an object).
func (c *Client) ListAccessGroups(ctx context.Context) ([]AccessGroupEntry, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v1/access_group", nil)
	if err != nil {
		return nil, err
	}
	var out []AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v1/access_group: %w", err)
	}
	return out, nil
}

// GetAccessGroupByName returns the entry whose access_group_name matches, or
// (nil, nil) when absent. Absence is NOT an error: the reconciler branches on
// nil to take the CREATE arm. access_group_id is server-minted and not
// derivable from metadata.name, so name lookup is the only adoption path.
func (c *Client) GetAccessGroupByName(ctx context.Context, name string) (*AccessGroupEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("litellm: GetAccessGroupByName: empty name")
	}
	list, err := c.ListAccessGroups(ctx)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range list {
		if list[i].AccessGroupName == name {
			out := list[i]
			return &out, nil
		}
	}
	return nil, nil
}

// UpdateAccessGroup issues PUT /v1/access_group/{id}. The three managed lists
// are always sent (see the AccessGroupUpdateRequest doc): omitted means KEEP,
// so a nil slice would silently preserve a stale grant.
func (c *Client) UpdateAccessGroup(ctx context.Context, id string, req *AccessGroupUpdateRequest) (*AccessGroupEntry, error) {
	if id == "" {
		return nil, fmt.Errorf("litellm: UpdateAccessGroup: empty access_group_id")
	}
	nonNilAccessGroupLists(&req.AccessModelNames, &req.AccessMCPServerIDs, &req.AccessAgentIDs)
	raw, err := c.makeRequest(ctx, "PUT", "/v1/access_group/"+url.PathEscape(id), req)
	if err != nil {
		return nil, err
	}
	var out AccessGroupEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode PUT /v1/access_group/%s: %w", id, err)
	}
	return &out, nil
}

// DeleteAccessGroup issues DELETE /v1/access_group/{id}. M-SEC3: guard the
// empty id (it would collapse to the collection route) and PathEscape so a
// `/`, `?`, or `#` cannot alter the path — mirrors DeleteMCPToolset.
func (c *Client) DeleteAccessGroup(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("litellm: DeleteAccessGroup: empty access_group_id")
	}
	_, err := c.makeRequest(ctx, "DELETE", "/v1/access_group/"+url.PathEscape(id), nil)
	return err
}

// nonNilAccessGroupLists coerces nil slices to empty ones so encoding/json
// renders `[]` (an explicit LiteLLM clear) rather than `null`. Both clear on
// this endpoint, but `[]` is the shape we measured and assert on.
func nonNilAccessGroupLists(lists ...*[]string) {
	for _, l := range lists {
		if *l == nil {
			*l = []string{}
		}
	}
}
