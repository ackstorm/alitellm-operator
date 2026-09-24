// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"
)

func TestClosedTeamGrant_NoGroupsIsFullyClosed(t *testing.T) {
	raw, err := json.Marshal(closedTeamGrant(nil))
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

func TestClosedTeamGrant_GroupsOnlyTouchAccessGroupIDs(t *testing.T) {
	got := closedTeamGrant([]string{"id-a", "id-b"})
	if ids := got["access_group_ids"].([]string); len(ids) != 2 || ids[0] != "id-a" || ids[1] != "id-b" {
		t.Errorf("access_group_ids: got %v", ids)
	}
	if m := got["models"].([]string); len(m) != 1 || m[0] != modelDenyAllSentinel {
		t.Errorf("models must stay the sentinel with groups attached, got %v", m)
	}
}
