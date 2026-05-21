// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
)

// CreateMCPServer issues POST /v1/mcp/server — the admin-immediate
// path (NOT the user-submission path under /v1/mcp/server/, which is
// for end-user submissions that require admin approval).
// LiteLLM 1.83.10 fixed BerriAI/litellm#23869
// (the Prisma approval_status defect that blocked Probe 6 on 1.82.6
// RC2); verified on 1.83.10 this endpoint now
// returns 202 cleanly.
func (c *Client) CreateMCPServer(ctx context.Context, req *MCPServerRequest) (*MCPServerEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/v1/mcp/server", req)
	if err != nil {
		return nil, err
	}
	var out MCPServerEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /v1/mcp/server: %w", err)
	}
	return &out, nil
}

// UpdateMCPServer issues PUT /v1/mcp/server.
func (c *Client) UpdateMCPServer(ctx context.Context, req *MCPServerUpdateRequest) (*MCPServerEntry, error) {
	raw, err := c.makeRequest(ctx, "PUT", "/v1/mcp/server", req)
	if err != nil {
		return nil, err
	}
	var out MCPServerEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode PUT /v1/mcp/server: %w", err)
	}
	return &out, nil
}

// DeleteMCPServer issues DELETE /v1/mcp/server/{serverID}.
func (c *Client) DeleteMCPServer(ctx context.Context, serverID string) error {
	_, err := c.makeRequest(ctx, "DELETE", "/v1/mcp/server/"+serverID, nil)
	return err
}

// ListMCPServers issues GET /v1/mcp/server. LiteLLM returns a bare
// array; we unmarshal into []MCPServerEntry and wrap it in
// MCPServerListResponse{Data: .} for length-check uniformity per
// REL-05 (Pattern 4 in 01-RESEARCH).
//
// Length-checks len(list.Data) before indexing → ErrNotFound on empty.
func (c *Client) ListMCPServers(ctx context.Context) ([]MCPServerEntry, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v1/mcp/server", nil)
	if err != nil {
		return nil, err
	}
	// LiteLLM returns a bare array; wrap into the Data envelope.
	var arr []MCPServerEntry
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v1/mcp/server: %w", err)
	}
	list := MCPServerListResponse{Data: arr}
	if len(list.Data) == 0 { // REL-05 length check before indexing
		return nil, ErrNotFound
	}
	return list.Data, nil
}
