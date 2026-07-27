// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CreateMCPToolset issues POST /v1/mcp/toolset.
//
// LiteLLM mints the toolset_id — a caller-supplied one is IGNORED (verified
// on 1.93.0), so the caller must read it from the response, exactly like
// A2A's agent_id. A duplicate toolset_name returns 409.
func (c *Client) CreateMCPToolset(ctx context.Context, req *MCPToolsetRequest) (*MCPToolsetEntry, error) {
	if req.Tools == nil {
		req.Tools = []MCPToolsetTool{}
	}
	raw, err := c.makeRequest(ctx, "POST", "/v1/mcp/toolset", req)
	if err != nil {
		return nil, err
	}
	var out MCPToolsetEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /v1/mcp/toolset: %w", err)
	}
	return &out, nil
}

// UpdateMCPToolset issues PUT /v1/mcp/toolset.
//
// The toolset_id travels in the BODY, not the path — this endpoint diverges
// from every other LiteLLM update the operator calls. Hitting
// /v1/mcp/toolset/<id> with PUT is a 405.
func (c *Client) UpdateMCPToolset(ctx context.Context, req *MCPToolsetUpdateRequest) (*MCPToolsetEntry, error) {
	if req.ToolsetID == "" {
		return nil, fmt.Errorf("litellm: UpdateMCPToolset: empty toolset_id")
	}
	if req.Tools == nil {
		req.Tools = []MCPToolsetTool{}
	}
	raw, err := c.makeRequest(ctx, "PUT", "/v1/mcp/toolset", req)
	if err != nil {
		return nil, err
	}
	var out MCPToolsetEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode PUT /v1/mcp/toolset: %w", err)
	}
	return &out, nil
}

// DeleteMCPToolset issues DELETE /v1/mcp/toolset/{toolsetID}. M-SEC3: guard
// the empty id (it would collapse to the collection route) and PathEscape so
// a `/`, `?`, or `#` cannot alter the path — mirrors DeleteMCPServer.
func (c *Client) DeleteMCPToolset(ctx context.Context, toolsetID string) error {
	if toolsetID == "" {
		return fmt.Errorf("litellm: DeleteMCPToolset: empty toolset_id")
	}
	_, err := c.makeRequest(ctx, "DELETE", "/v1/mcp/toolset/"+url.PathEscape(toolsetID), nil)
	return err
}

// ListMCPToolsets issues GET /v1/mcp/toolset. LiteLLM returns a bare array;
// we wrap into MCPToolsetListResponse{Data: ...} for length-check uniformity
// per REL-05, returning ErrNotFound on an empty set.
func (c *Client) ListMCPToolsets(ctx context.Context) ([]MCPToolsetEntry, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v1/mcp/toolset", nil)
	if err != nil {
		return nil, err
	}
	var arr []MCPToolsetEntry
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v1/mcp/toolset: %w", err)
	}
	list := MCPToolsetListResponse{Data: arr}
	if len(list.Data) == 0 { // REL-05 length check before indexing
		return nil, ErrNotFound
	}
	return list.Data, nil
}
