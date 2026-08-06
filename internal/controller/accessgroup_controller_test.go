// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim pins the three
// dimensions' differing treatment: models pass through, MCP servers and agents
// are resolved name→id because LiteLLM matches those on ids and silently
// ignores names.
func TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		Models:     []string{"gpt-4", "claude-opus"},
		MCPServers: []string{"slack"},
		Agents:     []string{"finops"},
	}
	serverIDs := map[string]string{"slack": "srv-1"}
	agentIDs := map[string]string{"finops": "agt-1"}

	got, missing := renderAccessGroup(spec, serverIDs, agentIDs)
	if len(missing.MCPServers)+len(missing.Agents) != 0 {
		t.Fatalf("unexpected missing: %+v", missing)
	}
	if len(got.Models) != 2 || got.Models[0] != "claude-opus" {
		t.Errorf("Models = %v, want the two spec names (sorted)", got.Models)
	}
	if len(got.MCPServerIDs) != 1 || got.MCPServerIDs[0] != "srv-1" {
		t.Errorf("MCPServerIDs = %v, want [srv-1]", got.MCPServerIDs)
	}
	if len(got.AgentIDs) != 1 || got.AgentIDs[0] != "agt-1" {
		t.Errorf("AgentIDs = %v, want [agt-1]", got.AgentIDs)
	}
}

// TestRenderAccessGroup_EmptySpecRendersNonNilLists guards the CLEAR contract:
// nil slices would serialize as null/absent and KEEP a stale grant upstream.
func TestRenderAccessGroup_EmptySpecRendersNonNilLists(t *testing.T) {
	got, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{}, nil, nil)
	if got.Models == nil || got.MCPServerIDs == nil || got.AgentIDs == nil {
		t.Fatalf("nil slice in %+v — an omitted list KEEPS the stale value upstream", got)
	}
}

// TestRenderAccessGroup_ReportsUnresolvedNames drives the parking path: an
// unresolved name must be reported, never silently dropped (a dropped name is
// a silent authorization gap).
func TestRenderAccessGroup_ReportsUnresolvedNames(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		MCPServers: []string{"slack", "ghost"},
		Agents:     []string{"nobody"},
	}
	_, missing := renderAccessGroup(spec, map[string]string{"slack": "srv-1"}, nil)
	if len(missing.MCPServers) != 1 || missing.MCPServers[0] != "ghost" {
		t.Errorf("missing.MCPServers = %v, want [ghost]", missing.MCPServers)
	}
	if len(missing.Agents) != 1 || missing.Agents[0] != "nobody" {
		t.Errorf("missing.Agents = %v, want [nobody]", missing.Agents)
	}
}

// TestAccessGroupHash_StableAcrossDeclarationOrder guards the steady-state
// short-circuit: a reordered spec must not look like drift and trigger a PUT.
func TestAccessGroupHash_StableAcrossDeclarationOrder(t *testing.T) {
	a, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m1", "m2"}}, nil, nil)
	b, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m2", "m1"}}, nil, nil)
	if accessGroupHash(a) != accessGroupHash(b) {
		t.Error("hash differs on declaration order — every reconcile would PUT")
	}
}
