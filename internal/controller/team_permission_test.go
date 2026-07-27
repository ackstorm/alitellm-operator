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
	models, op, missing := projectPermission(perm, nil, nil)
	if want := []string{"gpt-4o", "claude-opus", "anthropic"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models: want %v, got %v", want, models)
	}
	// mcp + agent_access_groups present but empty; agents deny-by-default →
	// nil-UUID sentinel (spec.permission.agents unset on a models-only block
	// must NOT fail-open to every agent).
	for _, k := range []string{"mcp_servers", "mcp_access_groups", "agent_access_groups"} {
		if got := opStrings(t, op, k); len(got) != 0 {
			t.Errorf("object_permission[%q]: want empty, got %v", k, got)
		}
	}
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{agentDenyAllSentinel}) {
		t.Errorf("agents: want deny-all sentinel [%s], got %v", agentDenyAllSentinel, got)
	}
	if len(missing.Agents) != 0 {
		t.Errorf("missing.Agents: want none, got %v", missing.Agents)
	}
}

func TestProjectPermission_McpAndGroups(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		McpServers: []string{"hindsight"},
		McpGroups:  []string{"team-a"},
	}
	_, op, _ := projectPermission(perm, nil, nil)
	if got := opStrings(t, op, "mcp_servers"); !reflect.DeepEqual(got, []string{"hindsight"}) {
		t.Errorf("mcp_servers: got %v", got)
	}
	if got := opStrings(t, op, "mcp_access_groups"); !reflect.DeepEqual(got, []string{"team-a"}) {
		t.Errorf("mcp_access_groups: got %v", got)
	}
	// agents unset → deny-all sentinel; agent_access_groups unset → [] (no-op field).
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{agentDenyAllSentinel}) {
		t.Errorf("agents: want deny-all sentinel [%s], got %v", agentDenyAllSentinel, got)
	}
	if got := opStrings(t, op, "agent_access_groups"); len(got) != 0 {
		t.Errorf("agent_access_groups: want empty, got %v", got)
	}
}

func TestProjectPermission_AgentsResolvedToUUIDs(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "coder"}}
	m := map[string]string{"planner": "uuid-1", "coder": "uuid-2"}
	_, op, missing := projectPermission(perm, m, nil)
	if len(missing.Agents) != 0 {
		t.Fatalf("missing.Agents: want none, got %v", missing.Agents)
	}
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{"uuid-1", "uuid-2"}) {
		t.Errorf("agents: want resolved UUIDs, got %v", got)
	}
}

func TestProjectPermission_UnresolvedAgentReported(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"planner", "ghost"}}
	m := map[string]string{"planner": "uuid-1"}
	_, op, missing := projectPermission(perm, m, nil)
	if !reflect.DeepEqual(missing.Agents, []string{"ghost"}) {
		t.Errorf("missing.Agents: want [ghost], got %v", missing.Agents)
	}
	// Resolved agents still projected (caller decides to requeue on missing).
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{"uuid-1"}) {
		t.Errorf("agents: want [uuid-1], got %v", got)
	}
}

func TestProjectPermission_AgentGroupsProjectedVerbatim(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{AgentGroups: []string{"grp-a"}}
	_, op, _ := projectPermission(perm, nil, nil)
	if got := opStrings(t, op, "agent_access_groups"); !reflect.DeepEqual(got, []string{"grp-a"}) {
		t.Errorf("agent_access_groups: got %v", got)
	}
}

// TestProjectPermission_DenyByDefault_EmptyBlock is the security regression:
// an all-empty present block must FAIL CLOSED. models and agents are the two
// LiteLLM fail-open fields (an empty list disables the filter entirely, so the
// team sees the master-key ceiling), so they carry their deny-all sentinels;
// mcp_servers/mcp_access_groups/agent_access_groups stay `[]` (already
// fail-closed / no-op in LiteLLM 1.83.10). Nothing may be omitted or `null`.
func TestProjectPermission_DenyByDefault_EmptyBlock(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{} // every sublist empty
	models, op, missing := projectPermission(perm, nil, nil)
	if len(missing.Agents) != 0 {
		t.Fatalf("missing.Agents: want none, got %v", missing.Agents)
	}
	if !reflect.DeepEqual(models, []string{modelDenyAllSentinel}) {
		t.Errorf("models: want deny-all sentinel [%s], got %v", modelDenyAllSentinel, models)
	}
	if got := opStrings(t, op, "agents"); !reflect.DeepEqual(got, []string{agentDenyAllSentinel}) {
		t.Errorf("agents: want deny-all sentinel [%s], got %v", agentDenyAllSentinel, got)
	}
	for _, k := range []string{"mcp_servers", "mcp_access_groups", "agent_access_groups"} {
		if got := opStrings(t, op, k); len(got) != 0 {
			t.Errorf("object_permission[%q]: want empty non-nil, got %v", k, got)
		}
	}
	// Serialization canary: fail-closed fields carry the sentinel; the fail-safe
	// fields are [] on the wire — never null (LiteLLM merges null as "absent" →
	// stale grant kept).
	raw, err := json.Marshal(map[string]any{"models": models, "object_permission": op})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		`"models":["__deny_all__"]`,
		`"agents":["00000000-0000-0000-0000-000000000000"]`,
		`"mcp_servers":[]`, `"mcp_access_groups":[]`, `"agent_access_groups":[]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled body missing %s. got=%s", want, got)
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("marshaled body contains null — every permission field must be [] or a sentinel, not null. got=%s", got)
	}
}

// TestProjectPermission_ModelsSentinelWhenUnset covers the models fail-open
// case in isolation: a block with only a non-model field set still denies all
// models via the sentinel.
func TestProjectPermission_ModelsSentinelWhenUnset(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{McpServers: []string{"hindsight"}}
	models, _, _ := projectPermission(perm, nil, nil)
	if !reflect.DeepEqual(models, []string{modelDenyAllSentinel}) {
		t.Errorf("models: want deny-all sentinel [%s], got %v", modelDenyAllSentinel, models)
	}
}

// TestProjectPermission_ModelsShrinkNoSentinel: a non-empty models list is
// projected verbatim — the sentinel is ONLY for the shrunk-to-empty case.
func TestProjectPermission_ModelsShrinkNoSentinel(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Models: []string{"a"}}
	models, _, _ := projectPermission(perm, nil, nil)
	if !reflect.DeepEqual(models, []string{"a"}) {
		t.Errorf("models: want [a] (non-empty → no sentinel), got %v", models)
	}
}

// TestProjectPermission_AllAgentsMissingNoSentinel: a non-empty perm.Agents
// that fails to resolve reports the missing names and leaves agents empty — it
// must NOT be replaced by the deny-all sentinel. The sentinel is exclusively
// the len(perm.Agents)==0 branch; the caller aborts (AgentNotFound) on missing.
func TestProjectPermission_AllAgentsMissingNoSentinel(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Agents: []string{"ghost"}}
	_, op, missing := projectPermission(perm, map[string]string{}, nil)
	if !reflect.DeepEqual(missing.Agents, []string{"ghost"}) {
		t.Fatalf("missing.Agents: want [ghost], got %v", missing.Agents)
	}
	if got := opStrings(t, op, "agents"); len(got) != 0 {
		t.Errorf("agents: want empty (missing path, not sentinel), got %v", got)
	}
}

// mcp_toolsets is FAIL-CLOSED in LiteLLM (the handler denies when the grant
// list is None or lacks the id), so it is emitted as [] when empty — it must
// NOT get a deny-all sentinel the way models/agents do.
func TestProjectPermission_McpToolsetsEmptyIsPlainEmptyList(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{Models: []string{"gpt-4"}}
	_, op, _ := projectPermission(perm, nil, nil)
	if got := opStrings(t, op, "mcp_toolsets"); len(got) != 0 {
		t.Errorf("mcp_toolsets = %v, want [] (fail-closed; NO sentinel)", got)
	}
}

// Names are resolved to toolset_id UUIDs — LiteLLM matches on the id.
func TestProjectPermission_McpToolsetsResolvedToUUIDs(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4"},
		McpToolsets: []string{"research-tools"},
	}
	toolsetNameToID := map[string]string{"research-tools": "ts-uuid-1"}
	_, op, missing := projectPermission(perm, nil, toolsetNameToID)
	if len(missing.Toolsets) != 0 {
		t.Fatalf("unexpected missing: %v", missing.Toolsets)
	}
	if got := opStrings(t, op, "mcp_toolsets"); !reflect.DeepEqual(got, []string{"ts-uuid-1"}) {
		t.Errorf("mcp_toolsets = %v, want [ts-uuid-1]", got)
	}
}

// An unresolved name is reported so the caller can requeue — it must NOT be
// silently dropped (that would be a silent under-grant) and must NOT be
// replaced by a sentinel.
func TestProjectPermission_McpToolsetsMissingReported(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{McpToolsets: []string{"nope"}}
	_, op, missing := projectPermission(perm, nil, map[string]string{})
	if !reflect.DeepEqual(missing.Toolsets, []string{"nope"}) {
		t.Errorf("missing.Toolsets = %v, want [nope]", missing.Toolsets)
	}
	if got := opStrings(t, op, "mcp_toolsets"); len(got) != 0 {
		t.Errorf("mcp_toolsets = %v, want empty (missing path, never a sentinel)", got)
	}
}

// Serialization canary for the new field: it must reach the wire as [] when
// empty, never null (LiteLLM's per-field merge treats null as "absent" and
// keeps the stale grant).
func TestProjectPermission_McpToolsetsSerializesAsEmptyArray(t *testing.T) {
	perm := &litellmv1alpha1.PermissionSpec{}
	_, op, _ := projectPermission(perm, nil, nil)
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"mcp_toolsets":[]`) {
		t.Errorf("marshaled object_permission missing `\"mcp_toolsets\":[]`. got=%s", raw)
	}
}
