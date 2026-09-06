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

func TestExtractMCPParams_AllModeledFields(t *testing.T) {
	in := map[string]any{
		"description":                  "desc",
		"credentials":                  map[string]any{"api_key": "x"},
		"allowed_tools":                []any{"tool-a", "tool-b"},
		"tool_name_to_display_name":    map[string]any{"tool-a": "Tool A"},
		"tool_name_to_description":     map[string]any{"tool-a": "doc"},
		"extra_headers":                []any{"x-litellm-api-key"}, // LIST shape (HIGH-2)
		"static_headers":               map[string]any{"x-foo": "bar"},
		"mcp_info":                     map[string]any{"env": "prod"},
		"command":                      "npx",
		"args":                         []any{"-y", "@modelcontextprotocol/server-foo"},
		"env":                          map[string]any{"TOKEN": "x"},
		"authorization_url":            "https://auth.example/authorize",
		"token_url":                    "https://auth.example/token",
		"registration_url":             "https://auth.example/register",
		"oauth2_flow":                  "authorization_code",
		"available_on_public_internet": false, // explicit false (tri-state)
		"oauth_passthrough":            true,
	}
	got := extractMCPParams(in)

	if got.Description != "desc" {
		t.Errorf("Description: %q", got.Description)
	}
	if got.Credentials["api_key"] != "x" {
		t.Errorf("Credentials: %v", got.Credentials)
	}
	if !reflect.DeepEqual(got.AllowedTools, []string{"tool-a", "tool-b"}) {
		t.Errorf("AllowedTools: %v", got.AllowedTools)
	}
	if got.ToolNameToDisplayName["tool-a"] != "Tool A" {
		t.Errorf("ToolNameToDisplayName: %v", got.ToolNameToDisplayName)
	}
	if got.ToolNameToDescription["tool-a"] != "doc" {
		t.Errorf("ToolNameToDescription: %v", got.ToolNameToDescription)
	}
	// extra_headers must be forwarded VERBATIM as a []any.
	if eh, ok := got.ExtraHeaders.([]any); !ok || len(eh) != 1 || eh[0] != "x-litellm-api-key" {
		t.Errorf("ExtraHeaders list shape lost: %#v", got.ExtraHeaders)
	}
	if got.StaticHeaders["x-foo"] != "bar" {
		t.Errorf("StaticHeaders: %v", got.StaticHeaders)
	}
	if got.MCPInfo["env"] != "prod" {
		t.Errorf("MCPInfo: %v", got.MCPInfo)
	}
	if got.Command != "npx" {
		t.Errorf("Command: %q", got.Command)
	}
	if !reflect.DeepEqual(got.Args, []string{"-y", "@modelcontextprotocol/server-foo"}) {
		t.Errorf("Args: %v", got.Args)
	}
	if got.Env["TOKEN"] != "x" {
		t.Errorf("Env: %v", got.Env)
	}
	if got.AuthorizationURL != "https://auth.example/authorize" ||
		got.TokenURL != "https://auth.example/token" ||
		got.RegistrationURL != "https://auth.example/register" ||
		got.OAuth2Flow != "authorization_code" {
		t.Errorf("oauth fields: %+v", got)
	}
	if got.AvailableOnPublicInternet == nil || *got.AvailableOnPublicInternet != false {
		t.Errorf("AvailableOnPublicInternet: want *false (tri-state), got %v", got.AvailableOnPublicInternet)
	}
	if got.OAuthPassthrough == nil || *got.OAuthPassthrough != true {
		t.Errorf("OAuthPassthrough: want *true (tri-state), got %v", got.OAuthPassthrough)
	}
}

func TestExtractMCPParams_ExtraHeadersMapShape(t *testing.T) {
	in := map[string]any{
		"extra_headers": map[string]any{"x-foo": "bar"}, // MAP shape — must also survive
	}
	got := extractMCPParams(in)
	m, ok := got.ExtraHeaders.(map[string]any)
	if !ok {
		t.Fatalf("ExtraHeaders not preserved as map: %#v", got.ExtraHeaders)
	}
	if m["x-foo"] != "bar" {
		t.Errorf("map content: %v", m)
	}
}

func TestExtractMCPParams_AccessGroupsAlias(t *testing.T) {
	// Only access_groups present (config.yaml ergonomics, LOW-4 alias).
	in := map[string]any{"access_groups": []any{"g1", "g2"}}
	got := extractMCPParams(in)
	if !reflect.DeepEqual(got.MCPAccessGroups, []string{"g1", "g2"}) {
		t.Errorf("alias not applied: %v", got.MCPAccessGroups)
	}
	// Both keys present → mcp_access_groups wins.
	in2 := map[string]any{
		"mcp_access_groups": []any{"primary"},
		"access_groups":     []any{"loser"},
	}
	got2 := extractMCPParams(in2)
	if !reflect.DeepEqual(got2.MCPAccessGroups, []string{"primary"}) {
		t.Errorf("precedence wrong: %v", got2.MCPAccessGroups)
	}
}

// An absent oauth_passthrough must stay nil, not default to false: the
// field is tri-state and `omitempty` on the *bool is what keeps the key
// out of the request body for every server that never opts in.
func TestExtractMCPParams_OAuthPassthroughUnsetStaysNil(t *testing.T) {
	got := extractMCPParams(map[string]any{"auth_type": "none"})
	if got.OAuthPassthrough != nil {
		t.Errorf("OAuthPassthrough: want nil when unset, got %v", *got.OAuthPassthrough)
	}
}
