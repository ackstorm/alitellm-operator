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

// NewTeamRequest is the POST /team/new request body. Mirrors the
// OpenAPI schema's optional fields; callers populate only what they need.
//
// *LimitType fields are informational; controller builds body via
// map[string]any to preserve JSON null semantics (see team_controller.go
// Step 7). These fields exist for symmetry with TPMLimit/RPMLimit and for
// forward-compat with any future caller that constructs the wire body via
// the typed struct.
type NewTeamRequest struct {
	TeamAlias      string         `json:"team_alias,omitempty"`
	TeamID         string         `json:"team_id,omitempty"`
	OrganizationID string         `json:"organization_id,omitempty"`
	Admins         []string       `json:"admins,omitempty"`
	Members        []string       `json:"members,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	TPMLimit       *int           `json:"tpm_limit,omitempty"`
	TPMLimitType   *string        `json:"tpm_limit_type,omitempty"`
	RPMLimit       *int           `json:"rpm_limit,omitempty"`
	RPMLimitType   *string        `json:"rpm_limit_type,omitempty"`
	MaxBudget      *float64       `json:"max_budget,omitempty"`
	BudgetDuration string         `json:"budget_duration,omitempty"`
	Models         []string       `json:"models,omitempty"`
	Blocked        *bool          `json:"blocked,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Extra          map[string]any `json:"-"`
}

// UpdateTeamRequest is the POST /team/update request body. team_id is
// required by the OpenAPI schema.
//
// *LimitType fields are informational; controller builds body via
// map[string]any to preserve JSON null semantics (see team_controller.go
// Step 7). These fields exist for symmetry with TPMLimit/RPMLimit and for
// forward-compat with any future caller that constructs the wire body via
// the typed struct.
type UpdateTeamRequest struct {
	TeamID         string         `json:"team_id"`
	TeamAlias      string         `json:"team_alias,omitempty"`
	OrganizationID string         `json:"organization_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	TPMLimit       *int           `json:"tpm_limit,omitempty"`
	TPMLimitType   *string        `json:"tpm_limit_type,omitempty"`
	RPMLimit       *int           `json:"rpm_limit,omitempty"`
	RPMLimitType   *string        `json:"rpm_limit_type,omitempty"`
	MaxBudget      *float64       `json:"max_budget,omitempty"`
	BudgetDuration string         `json:"budget_duration,omitempty"`
	Models         []string       `json:"models,omitempty"`
	Blocked        *bool          `json:"blocked,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Extra          map[string]any `json:"-"`
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
