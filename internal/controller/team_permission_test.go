// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func TestProjectPermission_MergesModelsAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4o", "claude-opus"},
		ModelGroups: []string{"anthropic"},
	}
	models, op, missing := projectPermission(perm, nil)
	if want := []string{"gpt-4o", "claude-opus", "anthropic"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models: want %v, got %v", want, models)
	}
	if len(op) != 0 {
		t.Errorf("objectPermission: want empty, got %v", op)
	}
	if len(missing) != 0 {
		t.Errorf("missingAgents: want none, got %v", missing)
	}
}

func TestProjectPermission_McpAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		McpServers: []string{"hindsight"},
		McpGroups:  []string{"team-a"},
	}
	_, op, _ := projectPermission(perm, nil)
	if !reflect.DeepEqual(op["mcp_servers"], []string{"hindsight"}) {
		t.Errorf("mcp_servers: got %v", op["mcp_servers"])
	}
	if !reflect.DeepEqual(op["mcp_access_groups"], []string{"team-a"}) {
		t.Errorf("mcp_access_groups: got %v", op["mcp_access_groups"])
	}
}

func TestProjectPermission_AgentsResolvedToUUIDs(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "coder"}}
	m := map[string]string{"planner": "uuid-1", "coder": "uuid-2"}
	_, op, missing := projectPermission(perm, m)
	if len(missing) != 0 {
		t.Fatalf("missingAgents: want none, got %v", missing)
	}
	if !reflect.DeepEqual(op["agents"], []string{"uuid-1", "uuid-2"}) {
		t.Errorf("agents: want resolved UUIDs, got %v", op["agents"])
	}
}

func TestProjectPermission_UnresolvedAgentReported(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "ghost"}}
	m := map[string]string{"planner": "uuid-1"}
	_, op, missing := projectPermission(perm, m)
	if !reflect.DeepEqual(missing, []string{"ghost"}) {
		t.Errorf("missingAgents: want [ghost], got %v", missing)
	}
	// Resolved agents still projected (caller decides to requeue on missing).
	if !reflect.DeepEqual(op["agents"], []string{"uuid-1"}) {
		t.Errorf("agents: want [uuid-1], got %v", op["agents"])
	}
}

func TestProjectPermission_AgentGroupsProjectedVerbatim(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{AgentGroups: []string{"grp-a"}}
	_, op, _ := projectPermission(perm, nil)
	if !reflect.DeepEqual(op["agent_access_groups"], []string{"grp-a"}) {
		t.Errorf("agent_access_groups: got %v", op["agent_access_groups"])
	}
}

func TestProjectPermission_EmptySublistsOmitted(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:     []string{},
		McpServers: []string{},
		Agents:     []string{},
	}
	models, op, missing := projectPermission(perm, nil)
	if len(models) != 0 {
		t.Errorf("models: want empty, got %v", models)
	}
	if len(op) != 0 {
		t.Errorf("objectPermission: want empty (no [] keys), got %v", op)
	}
	if len(missing) != 0 {
		t.Errorf("missingAgents: want none, got %v", missing)
	}
}
