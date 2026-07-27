// SPDX-License-Identifier: Apache-2.0

package litellm

import "encoding/json"

// LiteLLMParams is the pass-through bag the operator marshals into POST
// /model/new and POST /model/update bodies. The OpenAPI schema declares
// `additionalProperties: true` (truly freeform), so a map preserves
// forward-compat with future LiteLLM fields the operator does not need
// to know about.
type LiteLLMParams map[string]any

// ModelInfo is the model_info sub-block carried by POST /model/new and
// the inverse responses.
type ModelInfo struct {
	// CR-16 / D-7.1-16 (2026-05-19): omitempty is load-bearing on CREATE.
	// Without it, the operator's Deployment{ModelInfo: ModelInfo{}} body
	// serializes "model_info":{"id":""}, LiteLLM 1.83.10 stores model_id=""
	// in LiteLLM_ProxyModelTable, and every subsequent reconcile trips the
	// model_id UNIQUE constraint on retry. On responses the field is always
	// present, so omitempty is a no-op for inbound parsing.
	ID                  string         `json:"id,omitempty"`
	DBModel             bool           `json:"db_model,omitempty"`
	UpdatedAt           string         `json:"updated_at,omitempty"`
	UpdatedBy           string         `json:"updated_by,omitempty"`
	CreatedAt           string         `json:"created_at,omitempty"`
	CreatedBy           string         `json:"created_by,omitempty"`
	BaseModel           string         `json:"base_model,omitempty"`
	Tier                string         `json:"tier,omitempty"`
	TeamID              string         `json:"team_id,omitempty"`
	TeamPublicModelName string         `json:"team_public_model_name,omitempty"`
	Extra               map[string]any `json:"-"`
}

// MarshalJSON inlines Extra into the model_info object so spec.info
// pass-through keys reach LiteLLM (D-05). Typed fields take precedence on
// a key collision (operator overlay wins, e.g. created_by/id). Empty Extra
// is a no-op, preserving CR-16 omitempty semantics (empty ModelInfo → {}).
func (m ModelInfo) MarshalJSON() ([]byte, error) {
	type alias ModelInfo // shed the custom marshaler to avoid recursion; Extra stays json:"-"
	base, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		if _, exists := merged[k]; exists {
			continue // typed field already set this key — operator overlay wins
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}

// Deployment is the POST /model/new request body.
type Deployment struct {
	ModelName     string        `json:"model_name"`
	LiteLLMParams LiteLLMParams `json:"litellm_params"`
	ModelInfo     ModelInfo     `json:"model_info"`
}

// updateDeployment is the POST /model/update request body (note the
// lowercase 'u' — matches the OpenAPI operationId / schema name verbatim).
//
// LiteLLM 1.85.1 schema (FIX7 H-1, 2026-05-23):
//
//	{ "model_name": ., "litellm_params": {.}, "model_info": { "id": "<uuid>", . } }
//
// The model id lives INSIDE model_info, NOT at the root. A prior comment
// (D-7.1-13 / Probe 9 retry 2026-05-18) claimed the 1.83.10 schema
// required a TOP-LEVEL id; that observation was likely transient (or the
// probe happened to succeed for unrelated reasons). The 1.85.1 OpenAPI
// /openapi.json authoritatively defines `updateDeployment` with no
// root-level id, and LiteLLM's body parser returns the misleading
// "Authentication Error, model not found" when the operator sends one
// — see FIX7.txt for the prod-failure repro. 1.83.x sites: pin
// operator to v0.3.0 or earlier.
//
//nolint:revive // OpenAPI schema name is lowercase
type updateDeployment struct {
	ModelName     string        `json:"model_name,omitempty"`
	LiteLLMParams LiteLLMParams `json:"litellm_params,omitempty"`
	ModelInfo     ModelInfo     `json:"model_info,omitempty"`
}

// UpdateDeployment is the exported alias callers in later phases use to
// construct the body. The lowercase name above stays for OpenAPI parity
// in internal code review.
type UpdateDeployment = updateDeployment

// ModelInfoResponse is one entry in a GET /model/info response Data
// array. The OpenAPI doc is sparse here ({}); shape inferred from spike
// Probe 2 + bbdsoftware/litellm-operator's known mapping.
type ModelInfoResponse struct {
	ModelID       string         `json:"model_id"`
	ModelName     string         `json:"model_name"`
	LiteLLMParams LiteLLMParams  `json:"litellm_params"`
	ModelInfo     ModelInfo      `json:"model_info"`
	Extra         map[string]any `json:"-"`
}

// ModelListResponse is the envelope returned by GET /model/info.
type ModelListResponse struct {
	Data []ModelInfoResponse `json:"data"`
}

// ModelDeleteRequest is the POST /model/delete request body.
type ModelDeleteRequest struct {
	ID string `json:"id"`
}

// DeleteTeamRequest is the POST /team/delete request body.
type DeleteTeamRequest struct {
	TeamIDs []string `json:"team_ids"`
}

// TeamListEntry is one row of a GET /v2/team/list response. Shape
// loosely typed — only fields the operator uses are explicit.
type TeamListEntry struct {
	TeamID         string          `json:"team_id"`
	TeamAlias      string          `json:"team_alias"`
	OrganizationID string          `json:"organization_id"`
	Models         []string        `json:"models,omitempty"`
	Blocked        *bool           `json:"blocked,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

// TeamListResponse is the GET /v2/team/list envelope.
type TeamListResponse struct {
	Teams      []TeamListEntry `json:"teams"`
	Total      int             `json:"total"`
	Page       int             `json:"page"`
	PageSize   int             `json:"page_size"`
	TotalPages int             `json:"total_pages"`
}

// MCPServerRequest is the POST /v1/mcp/server request body. Mirrors the
// OpenAPI schema's optional fields.
type MCPServerRequest struct {
	ServerID                  string         `json:"server_id,omitempty"`
	ServerName                string         `json:"server_name,omitempty"`
	Alias                     string         `json:"alias,omitempty"`
	Description               string         `json:"description,omitempty"`
	Transport                 string         `json:"transport,omitempty"`
	AuthType                  string         `json:"auth_type,omitempty"`
	Credentials               map[string]any `json:"credentials,omitempty"`
	URL                       string         `json:"url,omitempty"`
	SpecPath                  string         `json:"spec_path,omitempty"`
	MCPInfo                   map[string]any `json:"mcp_info,omitempty"`
	MCPAccessGroups           []string       `json:"mcp_access_groups,omitempty"`
	AllowedTools              []string       `json:"allowed_tools,omitempty"`
	ToolNameToDisplayName     map[string]any `json:"tool_name_to_display_name,omitempty"`
	ToolNameToDescription     map[string]any `json:"tool_name_to_description,omitempty"`
	ExtraHeaders              any            `json:"extra_headers,omitempty"`
	StaticHeaders             map[string]any `json:"static_headers,omitempty"`
	Command                   string         `json:"command,omitempty"`
	Args                      []string       `json:"args,omitempty"`
	Env                       map[string]any `json:"env,omitempty"`
	AuthorizationURL          string         `json:"authorization_url,omitempty"`
	TokenURL                  string         `json:"token_url,omitempty"`
	RegistrationURL           string         `json:"registration_url,omitempty"`
	OAuth2Flow                string         `json:"oauth2_flow,omitempty"`
	AllowAllKeys              *bool          `json:"allow_all_keys,omitempty"`
	AvailableOnPublicInternet *bool          `json:"available_on_public_internet,omitempty"`
	Extra                     map[string]any `json:"-"`
}

// MCPServerUpdateRequest is the PUT /v1/mcp/server request body. The
// OpenAPI schema marks server_id as required.
type MCPServerUpdateRequest struct {
	ServerID                  string         `json:"server_id"`
	ServerName                string         `json:"server_name,omitempty"`
	Alias                     string         `json:"alias,omitempty"`
	Description               string         `json:"description,omitempty"`
	Transport                 string         `json:"transport,omitempty"`
	AuthType                  string         `json:"auth_type,omitempty"`
	Credentials               map[string]any `json:"credentials,omitempty"`
	URL                       string         `json:"url,omitempty"`
	SpecPath                  string         `json:"spec_path,omitempty"`
	MCPInfo                   map[string]any `json:"mcp_info,omitempty"`
	MCPAccessGroups           []string       `json:"mcp_access_groups,omitempty"`
	AllowedTools              []string       `json:"allowed_tools,omitempty"`
	ToolNameToDisplayName     map[string]any `json:"tool_name_to_display_name,omitempty"`
	ToolNameToDescription     map[string]any `json:"tool_name_to_description,omitempty"`
	ExtraHeaders              any            `json:"extra_headers,omitempty"`
	StaticHeaders             map[string]any `json:"static_headers,omitempty"`
	Command                   string         `json:"command,omitempty"`
	Args                      []string       `json:"args,omitempty"`
	Env                       map[string]any `json:"env,omitempty"`
	AuthorizationURL          string         `json:"authorization_url,omitempty"`
	TokenURL                  string         `json:"token_url,omitempty"`
	RegistrationURL           string         `json:"registration_url,omitempty"`
	OAuth2Flow                string         `json:"oauth2_flow,omitempty"`
	AllowAllKeys              *bool          `json:"allow_all_keys,omitempty"`
	AvailableOnPublicInternet *bool          `json:"available_on_public_internet,omitempty"`
	IsBYOK                    *bool          `json:"is_byok,omitempty"`
	Extra                     map[string]any `json:"-"`
}

// MCPServerEntry is one row of GET /v1/mcp/server (bare array; the
// operator wraps it in MCPServerListResponse for length-check uniformity
// with the model and agent helpers — see REL-05).
type MCPServerEntry struct {
	ServerID       string          `json:"server_id"`
	ServerName     string          `json:"server_name,omitempty"`
	Alias          string          `json:"alias,omitempty"`
	Description    string          `json:"description,omitempty"`
	URL            string          `json:"url,omitempty"`
	SpecPath       string          `json:"spec_path,omitempty"`
	Transport      string          `json:"transport"`
	AuthType       string          `json:"auth_type,omitempty"`
	Status         string          `json:"status,omitempty"`
	ApprovalStatus string          `json:"approval_status,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

// MCPServerListResponse wraps the bare-array GET /v1/mcp/server response
// in a Data envelope so the per-domain length-check pattern is uniform.
type MCPServerListResponse struct {
	Data []MCPServerEntry `json:"data"`
}

// ── MCP Toolset wire types (LiteLLM 1.93.0) ──────────────────────────────
//
// A toolset is a named collection of specific tools drawn from one or more
// MCP servers — a curated subset instead of a whole-server grant.
//
//	POST   /v1/mcp/toolset          body MCPToolsetRequest        → 201
//	GET    /v1/mcp/toolset          → bare array of MCPToolsetEntry
//	PUT    /v1/mcp/toolset          body MCPToolsetUpdateRequest  (id in BODY)
//	GET    /v1/mcp/toolset/{id}
//	DELETE /v1/mcp/toolset/{id}     → 202
//
// Verified on LiteLLM 1.93.0 (2026-07-27):
//   - `toolset_id` is NOT pinnable — a supplied value is ignored and the
//     server mints a UUID (same as A2A `agent_id`, unlike team_id/server_id).
//   - `toolset_name` IS unique — a duplicate returns 409.
//   - There is ZERO referential validation: a nonexistent server_id or
//     tool_name is accepted with 201 and degrades to granting nothing.

// MCPToolsetTool is one {server, tool} pair inside a toolset. Both fields are
// required by the LiteLLM schema. LiteLLM does NOT validate that either value
// refers to anything real.
type MCPToolsetTool struct {
	ServerID string `json:"server_id"`
	ToolName string `json:"tool_name"`
}

// MCPToolsetRequest is the POST /v1/mcp/toolset request body.
//
// ALWAYS-EMIT contract on Tools: the field carries NO omitempty, and callers
// MUST pass a non-nil slice, so an emptied toolset serializes as `[]` (an
// explicit clear) rather than being omitted or sent as `null`. Mirrors the
// team object_permission ALWAYS-EMIT rule — LiteLLM replaces a present field
// and keeps the stale value for an absent one.
type MCPToolsetRequest struct {
	ToolsetName string           `json:"toolset_name"`
	Description string           `json:"description,omitempty"`
	Tools       []MCPToolsetTool `json:"tools"`
}

// MCPToolsetUpdateRequest is the PUT /v1/mcp/toolset request body. NOTE the
// id travels in the BODY, not the path — diverges from every other LiteLLM
// update endpoint the operator calls.
type MCPToolsetUpdateRequest struct {
	ToolsetID   string           `json:"toolset_id"`
	ToolsetName string           `json:"toolset_name,omitempty"`
	Description string           `json:"description,omitempty"`
	Tools       []MCPToolsetTool `json:"tools"`
}

// MCPToolsetEntry is one row of GET /v1/mcp/toolset (bare array; wrapped in
// MCPToolsetListResponse for length-check uniformity per REL-05).
type MCPToolsetEntry struct {
	ToolsetID   string           `json:"toolset_id"`
	ToolsetName string           `json:"toolset_name"`
	Description string           `json:"description,omitempty"`
	Tools       []MCPToolsetTool `json:"tools,omitempty"`
	CreatedAt   string           `json:"created_at,omitempty"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
	Raw         json.RawMessage  `json:"-"`
}

// MCPToolsetListResponse wraps the bare-array GET /v1/mcp/toolset response in
// a Data envelope (uniform length-check pattern).
type MCPToolsetListResponse struct {
	Data []MCPToolsetEntry `json:"data"`
}

// AgentConfig is the POST /v1/agents and PUT /v1/agents/{id} request
// body. agent_name and agent_card_params are required by the OpenAPI
// schema; LiteLLMParams + ObjectPermission etc. are optional.
type AgentConfig struct {
	AgentName        string         `json:"agent_name"`
	AgentCardParams  map[string]any `json:"agent_card_params"`
	LiteLLMParams    LiteLLMParams  `json:"litellm_params,omitempty"`
	ObjectPermission map[string]any `json:"object_permission,omitempty"`
	TPMLimit         *int           `json:"tpm_limit,omitempty"`
	RPMLimit         *int           `json:"rpm_limit,omitempty"`
	SessionTPMLimit  *int           `json:"session_tpm_limit,omitempty"`
	SessionRPMLimit  *int           `json:"session_rpm_limit,omitempty"`
	StaticHeaders    map[string]any `json:"static_headers,omitempty"`
	ExtraHeaders     map[string]any `json:"extra_headers,omitempty"`
	Extra            map[string]any `json:"-"`
}

// AgentEntry is one row of GET /v1/agents (bare-array response wrapped
// in AgentListResponse).
type AgentEntry struct {
	AgentID         string          `json:"agent_id"`
	AgentName       string          `json:"agent_name"`
	AgentCardParams map[string]any  `json:"agent_card_params,omitempty"`
	LiteLLMParams   LiteLLMParams   `json:"litellm_params,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// AgentListResponse wraps the bare-array GET /v1/agents response in a
// Data envelope (uniform length-check pattern).
type AgentListResponse struct {
	Data []AgentEntry `json:"data"`
}

// ── Guardrail wire types ─────────────────────────────────
//
// LiteLLM 2026-05 surface (litellm/types/guardrails.py and
// litellm/proxy/guardrails/guardrail_endpoints.py):
//
//	POST  /guardrails                 body {"guardrail": Guardrail}
//	PUT   /guardrails/{guardrail_id}  body {"guardrail": Guardrail}
//	DELETE /guardrails/{guardrail_id}
//	GET   /v2/guardrails/list         response ListGuardrailsResponse
//
// `Guardrail` (TypedDict in upstream) carries guardrail_name (required),
// litellm_params (required — provider, mode, default_on, + provider-specific
// keys as a free-form map; Pydantic model is extra="allow"), guardrail_info
// (optional), policy_template (optional), and server-set created_at /
// updated_at timestamps. The response shape adds a guardrail_id UUID and a
// guardrail_definition_location enum ("db" | "config").
//
// LiteLLMGuardrailParams is the litellm_params bag as a map[string]any so the
// operator can preserve forward-compat with new provider sub-models without a
// types.go change.

// LiteLLMGuardrailParams is the `litellm_params` pass-through map carried in
// POST /guardrails and PUT /guardrails/{guardrail_id} bodies. Required keys
// per BaseLitellmParams: "guardrail" (provider name), "mode" (string or
// []string of GuardrailEventHooks values). Optional keys: "default_on",
// "api_base", "api_key", and 30+ provider-specific knobs (see
// docs.litellm.ai/docs/guardrail_providers + BaseLitellmParams in
// litellm/types/guardrails.py). The map is forwarded verbatim.
type LiteLLMGuardrailParams map[string]any

// GuardrailBody is the inner `guardrail` object carried by POST /guardrails
// and PUT /guardrails/{guardrail_id} request bodies (the outer wrapper is
// CreateGuardrailRequest).
//
// Field shape mirrors upstream `Guardrail` TypedDict:
//
//   - GuardrailID  — empty on CREATE (server assigns); echoed on UPDATE.
//   - GuardrailName — required; the LB pool key + user-facing identifier.
//   - LitellmParams — the pass-through bag; see LiteLLMGuardrailParams.
//   - GuardrailInfo — optional pass-through (description + dynamic-request
//     param schema surfaced via GET /v2/guardrails/list).
//   - PolicyTemplate — optional reusable rule bundle name; merged with
//     LitellmParams at evaluation time.
//
// Server-assigned timestamps (created_at, updated_at) are not part of the
// outbound body shape.
type GuardrailBody struct {
	GuardrailID    string                 `json:"guardrail_id,omitempty"`
	GuardrailName  string                 `json:"guardrail_name"`
	LitellmParams  LiteLLMGuardrailParams `json:"litellm_params"`
	GuardrailInfo  map[string]any         `json:"guardrail_info,omitempty"`
	PolicyTemplate string                 `json:"policy_template,omitempty"`
}

// CreateGuardrailRequest wraps GuardrailBody under the upstream
// CreateGuardrailRequest pydantic shape (one key: "guardrail").
type CreateGuardrailRequest struct {
	Guardrail *GuardrailBody `json:"guardrail"`
}

// Guardrail definition location values returned by GET /v2/guardrails/list.
// Operator addresses only DB-persisted rows via POST/PUT/DELETE; CONFIG rows
// are read-only and surface as Ready=False, reason=ConflictsWithConfigGuardrail.
const (
	GuardrailDefinitionLocationDB     = "db"
	GuardrailDefinitionLocationConfig = "config"
)

// GuardrailEntry is one record of a GET /v2/guardrails/list response.
// Mirrors GuardrailInfoResponse (litellm/types/guardrails.py): the server
// MASKS sensitive litellm_params values (api_key, etc.) before returning, so
// the operator MUST NOT compare litellm_params for drift via this endpoint;
// drift detection uses the operator-side hash in status.lastRendered.hash.
type GuardrailEntry struct {
	GuardrailID                 string                 `json:"guardrail_id,omitempty"`
	GuardrailName               string                 `json:"guardrail_name"`
	LitellmParams               LiteLLMGuardrailParams `json:"litellm_params,omitempty"`
	GuardrailInfo               map[string]any         `json:"guardrail_info,omitempty"`
	PolicyTemplate              string                 `json:"policy_template,omitempty"`
	CreatedAt                   string                 `json:"created_at,omitempty"`
	UpdatedAt                   string                 `json:"updated_at,omitempty"`
	GuardrailDefinitionLocation string                 `json:"guardrail_definition_location,omitempty"`
	Raw                         json.RawMessage        `json:"-"`
}

// GuardrailListResponse is the GET /v2/guardrails/list envelope.
type GuardrailListResponse struct {
	Guardrails []GuardrailEntry `json:"guardrails"`
}

// RouterSettings is the operator's typed view of LiteLLM router_settings
// for the purpose of editing model_group_alias. All non-alias keys are
// preserved opaquely under Extra so a read-merge-write does NOT clobber
// settings the operator does not understand.
type RouterSettings struct {
	// ModelGroupAlias is the typed view of router_settings.model_group_alias.
	ModelGroupAlias map[string]string `json:"-"`

	// Extra carries every other key in router_settings verbatim. The
	// operator preserves these on read-merge-write so unrelated router
	// configuration is never accidentally cleared.
	Extra map[string]any `json:"-"`
}

// ConfigCallbacksResponse is the GET /get/config/callbacks envelope, scoped
// to the fields the operator cares about. Unknown top-level keys are
// preserved by the JSON decoder via the use of a generic map at the call
// site.
type ConfigCallbacksResponse struct {
	Status         string         `json:"status"`
	RouterSettings map[string]any `json:"router_settings"`
}
