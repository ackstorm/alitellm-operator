// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"fmt"
)

// CreateAgent issues POST /v1/agents.
func (c *Client) CreateAgent(ctx context.Context, req *AgentConfig) (*AgentEntry, error) {
	raw, err := c.makeRequest(ctx, "POST", "/v1/agents", req)
	if err != nil {
		return nil, err
	}
	c.invalidateAgentsCache() // v0.4.6: own write makes cached LIST stale.
	var out AgentEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode POST /v1/agents: %w", err)
	}
	return &out, nil
}

// UpdateAgent issues PUT /v1/agents/{agentID}. PUT here IS wholesale-
// replace empirically verified — the only kind where §5.1 holds.
func (c *Client) UpdateAgent(ctx context.Context, agentID string, req *AgentConfig) (*AgentEntry, error) {
	raw, err := c.makeRequest(ctx, "PUT", "/v1/agents/"+agentID, req)
	if err != nil {
		return nil, err
	}
	c.invalidateAgentsCache() // v0.4.6: own write makes cached LIST stale.
	var out AgentEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("litellm: decode PUT /v1/agents/{id}: %w", err)
	}
	return &out, nil
}

// DeleteAgent issues DELETE /v1/agents/{agentID}.
func (c *Client) DeleteAgent(ctx context.Context, agentID string) error {
	_, err := c.makeRequest(ctx, "DELETE", "/v1/agents/"+agentID, nil)
	if err == nil {
		c.invalidateAgentsCache() // v0.4.6: own write makes cached LIST stale.
	}
	return err
}

// ListAgents issues GET /v1/agents?health_check=false. LiteLLM returns
// a bare array; we wrap into AgentListResponse{Data: .} for length-
// check uniformity per REL-05.
//
// Length-checks len(list.Data) before indexing → ErrNotFound on empty.
func (c *Client) ListAgents(ctx context.Context) ([]AgentEntry, error) {
	raw, err := c.makeRequest(ctx, "GET", "/v1/agents?health_check=false", nil)
	if err != nil {
		return nil, err
	}
	var arr []AgentEntry
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("litellm: decode GET /v1/agents: %w", err)
	}
	list := AgentListResponse{Data: arr}
	if len(list.Data) == 0 { // REL-05 length check before indexing
		return nil, ErrNotFound
	}
	return list.Data, nil
}
