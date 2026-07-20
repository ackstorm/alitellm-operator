// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// opStrings extracts an object_permission sub-field as a []string for
// assertions (projectPermission stores every sub-field as a non-nil []string).
func opStrings(t *testing.T, op map[string]any, key string) []string {
	t.Helper()
	v, ok := op[key]
	if !ok {
		t.Fatalf("object_permission[%q] MISSING — the ALWAYS-EMIT contract requires every sub-field present (as [] when empty)", key)
	}
	s, ok := v.([]string)
	if !ok {
		t.Fatalf("object_permission[%q]: want []string, got %T (%v)", key, v, v)
	}
	return s
}

func TestProjectPermission_MergesModelsAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4o", "claude-opus"},
		ModelGroups: []string{"anthropic"},
	}
	models, op, missing := projectPermission(perm, nil)
	if want := []string{"gpt-4o", "claude-opus", "anthropic"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models: want %v, got %v", want, models)
	}
	// object_permission sub-fields are all present but empty (models-only block).
	for _, k := range []string{"mcp_servers", "mcp_access_groups", "agents", "agent_access_groups"} {
		if got := opStrings(t, op, k); len(got) != 0 {
			t.Errorf("object_permission[%q]: want empty, got %v", k, got)
		}
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
	if got := opStrings(t, op, "mcp_servers"); !reflect.DeepEqual(got, []string{"hindsight"}) {
		t.Errorf("mcp_servers: got %v", got)
	}
	if got := opStrings(t, op, "mcp_access_groups"); !reflect.DeepEqual(got, []string{"team-a"}) {
		t.Errorf("mcp_access_groups: got %v", got)
	}
	// The unset sub-fields are still emitted as empty (clear-on-wire).
	if got := opStrings(t, op, "agents"); len(got) != 0 {
		t.Errorf("agents: want empty, got %v", got)
	}
	if got := opStrings(t, op, "agent_access_groups"); len(got) != 0 {
		t.Errorf("agent_access_groups: want empty, got %v", got)
	}
}

func TestProjectPermission_AgentsResolvedToUUIDs(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "coder"}}
	m := map[string]string{"planner": "uuid-1", "coder": "uuid-2"}
	_, op, missing := projectPermission(perm, m)
	if len(missing) != 0 {
		t.Fatalf("missingAgents: want none, got %v", missing)
	}
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{"uuid-1", "uuid-2"}) {
		t.Errorf("agents: want resolved UUIDs, got %v", got)
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
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{"uuid-1"}) {
		t.Errorf("agents: want [uuid-1], got %v", got)
	}
}

func TestProjectPermission_AgentGroupsProjectedVerbatim(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{AgentGroups: []string{"grp-a"}}
	_, op, _ := projectPermission(perm, nil)
	if got := opStrings(t, op, "agent_access_groups"); !reflect.DeepEqual(got, []string{"grp-a"}) {
		t.Errorf("agent_access_groups: got %v", got)
	}
}

// TestProjectPermission_EmptyEmittedAsEmptyArrayNotNull is the security
// regression: an all-empty present block must render every field as `[]` on
// the wire, never omitted and never `null`. Omitting/null-ing lets LiteLLM's
// per-field /team/update merge keep the stale (revoked) value.
func TestProjectPermission_EmptyEmittedAsEmptyArrayNotNull(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{} // every sublist empty
	models, op, missing := projectPermission(perm, nil)
	if len(missing) != 0 {
		t.Fatalf("missingAgents: want none, got %v", missing)
	}
	if models == nil {
		t.Error("models is nil — must be non-nil empty slice so it marshals to [] not null")
	}
	for _, k := range []string{"mcp_servers", "mcp_access_groups", "agents", "agent_access_groups"} {
		if got := opStrings(t, op, k); len(got) != 0 {
			t.Errorf("object_permission[%q]: want empty non-nil, got %v", k, got)
		}
	}
	// Serialization canary: [] on the wire, never null (LiteLLM merges null as
	// "absent" → stale grant kept).
	raw, err := json.Marshal(map[string]any{"models": models, "object_permission": op})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		`"models":[]`, `"mcp_servers":[]`, `"mcp_access_groups":[]`,
		`"agents":[]`, `"agent_access_groups":[]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled body missing %s (leak risk). got=%s", want, got)
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("marshaled body contains null — every permission field must be [] not null. got=%s", got)
	}
}
