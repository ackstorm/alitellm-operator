// SPDX-License-Identifier: Apache-2.0

package controller

// extractedMCPParams holds the typed view of a LiteLLMMCPServer spec.params
// map that the controller forwards to LiteLLM on CREATE/UPDATE. Every field
// here MUST correspond 1:1 to a field already modeled in
// litellm.MCPServerRequest / MCPServerUpdateRequest. New top-level params
// added in a future release land here first, then in the controller
// CREATE/UPDATE constructors.
type extractedMCPParams struct {
	Description               string
	AuthType                  string
	Credentials               map[string]any
	MCPAccessGroups           []string
	AllowedTools              []string
	ToolNameToDisplayName     map[string]any
	ToolNameToDescription     map[string]any
	ExtraHeaders              any // map (inject) or []string (forward-from-client); verbatim
	StaticHeaders             map[string]any
	MCPInfo                   map[string]any
	Command                   string
	Args                      []string
	Env                       map[string]any
	AuthorizationURL          string
	TokenURL                  string
	RegistrationURL           string
	OAuth2Flow                string
	AllowAllKeys              *bool
	AvailableOnPublicInternet *bool
}

// reservedMCPParamKeys are structural keys the operator owns. If a user
// puts any of these in spec.params they are ignored at extraction time —
// the controller stamps them from the CR (server_name/alias from sanitized
// metadata.name, url from spec.endpoint, transport from spec.transport,
// server_id from status.lastRendered, spec_path is unsupported in v0.3.1).
var reservedMCPParamKeys = map[string]struct{}{
	"server_id":   {},
	"server_name": {},
	"alias":       {},
	"url":         {},
	"transport":   {},
	"spec_path":   {},
}

// extractMCPParams pulls every modeled top-level field out of paramsMap.
// Reserved keys (see reservedMCPParamKeys) are dropped. Unknown keys are
// silently ignored in v0.3.1 (LOW-5 deferred).
func extractMCPParams(p map[string]any) extractedMCPParams {
	out := extractedMCPParams{}
	if p == nil {
		return out
	}
	out.AuthType, _ = p["auth_type"].(string)
	out.AllowAllKeys = boolPtrFromParams(p, "allow_all_keys")
	out.MCPAccessGroups = stringSliceFromParams(p, "mcp_access_groups")
	if len(out.MCPAccessGroups) == 0 {
		out.MCPAccessGroups = stringSliceFromParams(p, "access_groups")
	}
	return out
}

// boolPtrFromParams returns a *bool for a JSON-decoded bool value, or nil
// when the key is missing or the type does not match. Distinguishes
// "explicitly false" from "unset" — required by LiteLLM's tri-state
// AllowAllKeys / AvailableOnPublicInternet semantics.
func boolPtrFromParams(p map[string]any, key string) *bool {
	v, ok := p[key]
	if !ok {
		return nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

// stringSliceFromParams returns a []string from a JSON-decoded []any of
// strings. Non-string elements are dropped silently. Returns nil when the
// key is missing or the value is not a slice.
func stringSliceFromParams(p map[string]any, key string) []string {
	v, ok := p[key]
	if !ok {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, el := range raw {
		if s, ok := el.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
