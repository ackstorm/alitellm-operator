// SPDX-License-Identifier: MIT

package controller

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestClosedTeamGrant_NoGroupsIsFullyClosed(t *testing.T) {
	raw, err := json.Marshal(closedTeamGrant(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"access_group_ids":[],"models":["no-default-models"],` +
		`"object_permission":{"agent_access_groups":[],"agents":["00000000-0000-0000-0000-000000000000"],` +
		`"mcp_access_groups":[],"mcp_servers":[],"mcp_toolsets":[]}}`
	if string(raw) != want {
		t.Errorf("closed grant:\n got %s\nwant %s", raw, want)
	}
}

func TestClosedTeamGrant_MirrorsGroupModelsAfterSentinel(t *testing.T) {
	got := closedTeamGrant([]string{"id-a", "id-b"}, []string{"gpt-4o", "openai"})
	if ids := got["access_group_ids"].([]string); len(ids) != 2 || ids[0] != "id-a" || ids[1] != "id-b" {
		t.Errorf("access_group_ids: got %v", ids)
	}
	want := []string{modelDenyAllSentinel, "gpt-4o", "openai"}
	if m := got["models"].([]string); !slices.Equal(m, want) {
		t.Errorf("models: got %v want %v", m, want)
	}
}

func TestAttachedGroupModels(t *testing.T) {
	byName := map[string][]string{
		"g1": {"openai", "gpt-4o", "*"},
		"g2": {"gpt-4o", "anthropic", "all-proxy-models", "openai/*", modelDenyAllSentinel, ""},
		"g3": {"never-attached"},
	}
	got := attachedGroupModels([]string{"g2", "g1", "missing"}, byName)
	want := []string{"anthropic", "gpt-4o", "openai"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := attachedGroupModels(nil, byName); got != nil {
		t.Errorf("no names: got %v want nil", got)
	}
}
