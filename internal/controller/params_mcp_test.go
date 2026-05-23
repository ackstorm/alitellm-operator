// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"testing"
)

func TestExtractMCPParams_AuthAndGroups(t *testing.T) {
	in := map[string]any{
		"auth_type":         "api_key",
		"mcp_access_groups": []any{"group-a", "group-b"},
		"allow_all_keys":    true,
		// reserved: must NOT propagate
		"server_id":   "should-be-dropped",
		"server_name": "should-be-dropped",
		"url":         "https://nope.example",
		"transport":   "stdio",
		"spec_path":   "/nope",
		"alias":       "nope",
	}
	got := extractMCPParams(in)

	if got.AuthType != "api_key" {
		t.Errorf("AuthType: want %q, got %q", "api_key", got.AuthType)
	}
	if !reflect.DeepEqual(got.MCPAccessGroups, []string{"group-a", "group-b"}) {
		t.Errorf("MCPAccessGroups: want [group-a group-b], got %v", got.MCPAccessGroups)
	}
	if got.AllowAllKeys == nil || *got.AllowAllKeys != true {
		t.Errorf("AllowAllKeys: want *true, got %v", got.AllowAllKeys)
	}
	// Reserved keys must not leak into any extracted field. Spot-check the
	// three string fields where they could collide.
	if got.AuthType == "should-be-dropped" || got.AuthorizationURL == "https://nope.example" {
		t.Errorf("reserved keys leaked into extracted struct: %+v", got)
	}
}
