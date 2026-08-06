// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Mode constants — set via MockServer.SetMode.
const (
	// ModeHappy returns 200 with minimal valid bodies for every path.
	ModeHappy = "happy"
	// Mode401 returns 401 with the literal LiteLLM 1.83.10 auth_error
	// body shape.
	Mode401 = "401"
	// ModeTransient5xx returns 503 with Retry-After.
	ModeTransient5xx = "transient_5xx"
	// ModeSlow sleeps 5s before responding 200 (forces client timeouts).
	ModeSlow = "slow"
	// Mode422 returns 422 Unprocessable Entity for POST /model/new.
	// Used by TestModel_LiteLLMRejected_OnHTTP422 to exercise the
	// LiteLLMRejected ready condition path (MODEL-06).
	Mode422 = "422"

	// ModeDelete422 returns HTTP 422 for any delete-shaped request
	// (POST .../delete or HTTP DELETE) and serves all other paths happily.
	// Used to exercise the deterministic-4xx finalizer-delete path.
	ModeDelete422 = "delete422"

	// ─── Phase 6 route-targeted modes ──────────────────────────
	// These three modes flip only the specified Team route to a non-2xx
	// response; every other path is served as happy. Used by the AC-T3
	// envtest suite to exercise the non-default Team deletion path:
	//
	// * `not-found-list-teams` → GET /v2/team/list returns 404 (LIST is
	// spec §7.7 line 1432 LiteLLMRejected — NOT success).
	// * `not-found-delete-team` → POST /team/delete returns 404 (DELETE
	// 404 is spec §7.5 line 1332 success — operator's drift-correction
	// succeeded; the team is gone).
	// * `401-delete-team` → POST /team/delete returns 401 (REL-06
	// anti-storm: cache invalidated, finalizer removed anyway).
	//
	// Modes are mutually exclusive — the most recent SetMode wins. Other
	// routes (POST /team/new, GET /v2/team/list when mode is not-found-
	// delete-team, etc.) are served by the happy-path statefulBody.
	ModeNotFoundListTeams  = "not-found-list-teams"
	ModeNotFoundDeleteTeam = "not-found-delete-team"
	Mode401DeleteTeam      = "401-delete-team"
)

// LiteLLMAuth401Body is the byte-perfect 401 response body LiteLLM 1.83.10
// emits across all 14 authenticated endpoints (Probe 8). Recorded in
// 01-01-SUMMARY.md and reused here so the envtest harness drives the same
// parser path that *Auth401Error parser was tested against.
//
// The literal contains:
// - error.message: human-readable diagnostic (contains "Invalid proxy
// server token" and a key-hash fragment — verbatim from live LiteLLM)
// - error.type: "token_not_found_in_db" (uniform across all 14 endpoints)
// - error.param: "key"
// - error.code: "401"
const LiteLLMAuth401Body = `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...-key, Key Hash (Token) =61def7928d739903cc1d300521e6ac878bf50e70720607e03ff077cd6c5cb57d. Unable to find token in cache or ` + "`LiteLLM_VerificationTokenTable`" + `","type":"token_not_found_in_db","param":"key","code":"401"}}`

// pathTeamDelete is the spec §7.5 wire path matched in three mode
// branches; extracted as a const so goconst stays quiet.
const pathTeamDelete = "/team/delete"

// pathMCPToolset is the MCP toolset COLLECTION route. Note that LiteLLM's
// update verb targets this same collection path (the toolset_id travels in
// the body), unlike every other update endpoint the operator calls.
const pathMCPToolset = "/v1/mcp/toolset"

// pathAccessGroup is the access-group COLLECTION route. Item operations
// (PUT/DELETE) append /{access_group_id}. LiteLLM answers POST with 201 and
// GET with a BARE array.
const pathAccessGroup = "/v1/access_group"

// modelEntry is a stateful in-memory record of a model created via
// POST /model/new. The mock tracks these to serve realistic GET /model/info
// responses that include the model_info.id UUID needed for update/delete tests.
type modelEntry struct {
	ModelID   string
	ModelName string
}

// mcpEntry is a stateful in-memory record of an MCP server created via
// POST /v1/mcp/server. The mock tracks these to serve realistic
// GET /v1/mcp/server bare-array responses and to honor PUT /v1/mcp/server
// wholesale-replace semantics (Probe 10c ✓ on 1.83.10-stable). Phase 5 plan
// 05-01 envtests assert on the per-server mutation counter +
// HasMCPServer/GetMCPServerID helpers.
type mcpEntry struct {
	ServerID   string
	ServerName string
	URL        string
	Transport  string
	// LastBody captures the most-recent POST/PUT request body for the
	// server. Used by Phase 5 AC-SEC4-PROPAGATE envtest to
	// verify the resolved secret value on a child-PUT after a Secret
	// rotation propagates from MSDisc → MCPServer.
	LastBody map[string]any
}

// teamEntry is a stateful in-memory record of a LiteLLM team created via
// POST /team/new. The mock tracks these to serve realistic
// GET /v2/team/list responses and to honor the spec §6.7 wire contract
// (max_budget + budget_duration are explicit JSON null when absent on the
// CR). Phase 6 envtests assert on the per-team mutation counter
// + HasTeam/MutationsByTeamAlias/LastTeamBody helpers.
//
// MaxBudget is a pointer so the mock can distinguish "absent" (nil → JSON
// null on the response wire) from "explicit zero". BudgetDuration uses
// the empty-string sentinel for "absent" (matches the LiteLLM 1.83.10
// response shape: the field is either a duration string OR null OR
// omitted).
type teamEntry struct {
	TeamID         string
	TeamAlias      string
	MaxBudget      *float64
	BudgetDuration string
	// LastBody captures the most-recent POST /team/new or POST
	// /team/update request body for this team. Used by 	// envtests to verify operator structural overlays (team_alias,
	// max_budget, budget_duration) WIN on the wire over identically-
	// keyed entries in spec.params, and to assert the null-preservation
	// (max_budget == nil, budget_duration == "" → JSON null on the wire)
	// for the absent-budget happy path.
	LastBody map[string]any
}

// agentEntry is a stateful in-memory record of an A2A agent created via
// POST /v1/agents. The mock tracks these to serve realistic GET /v1/agents
// bare-array responses and to honor PUT /v1/agents/<id> wholesale-replace
// semantics (Phase 1 Probe 7 ✓ on 1.82.6). Phase 5 envtests
// assert on the per-agent mutation counter + HasAgent/GetAgentID helpers
// and on LastAgentBody (most-recent POST/PUT body capture used to verify
// the four ProjectionOverride collision-key projections).
type agentEntry struct {
	AgentID         string
	AgentName       string
	AgentCardParams map[string]any
	LastBody        map[string]any
}

// toolsetEntry is a stateful in-memory record of an MCP toolset created via
// POST /v1/mcp/toolset. Mirrors agentEntry: LiteLLM 1.93.0 MINTS the
// toolset_id (a caller-supplied one is ignored) and PUT is a wholesale
// replace. LastBody captures the most recent POST/PUT body so envtests can
// assert the rendered `tools` array — notably that an emptied list arrives as
// a present-but-empty `[]` rather than being omitted.
type toolsetEntry struct {
	ToolsetID   string
	ToolsetName string
	Description string
	Tools       []any
	LastBody    map[string]any
}

// accessGroupEntry is the mock's in-memory access group. Field names and
// JSON tags mirror LiteLLM 1.93.0's wire shape so the operator's client can
// decode this verbatim. Deliberately mock-local: the mock package is
// stdlib-only and must not import internal/litellm.
type accessGroupEntry struct {
	AccessGroupID      string   `json:"access_group_id"`
	AccessGroupName    string   `json:"access_group_name"`
	Description        string   `json:"description"`
	AccessModelNames   []string `json:"access_model_names"`
	AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
	AccessAgentIDs     []string `json:"access_agent_ids"`
	AssignedTeamIDs    []string `json:"assigned_team_ids"`
	AssignedKeyIDs     []string `json:"assigned_key_ids"`
}

// MockServer wraps a httptest.Server with per-method counters and a
// programmable response mode. It exposes the same shape every test
// expects: URL for *litellm.Client construction, Mutations/Reads
// for AC-R1 assertions, SetMode for fast-path / transient-5xx flipping.
type MockServer struct {
	srv *httptest.Server

	// mutations counts POST/PUT/DELETE/PATCH calls. Mutations are the
	// only requests AC-R1 forbids during steady state — read calls
	// (GET) are unrestricted.
	mutations atomic.Int64
	// reads counts GET/HEAD calls.
	reads atomic.Int64

	// mode is the current response mode (ModeHappy, Mode401, …). Stored
	// in an atomic.Value to support racy reads from the HTTP handler
	// without holding a mutex on every request.
	mode atomic.Value // string

	// loggingCallbacksNull, when true, makes POST /key/health return
	// {"key":"healthy","logging_callbacks":null}, mirroring a real
	// LiteLLM proxy with no callbacks configured. Used by UAT LOW-01
	// regression tests. Default false (full callback list returned).
	loggingCallbacksNull atomic.Bool

	// recorder captures every request for tests that need to assert on
	// path / method / body. Append-only; consumers should hold the mutex
	// while iterating.
	// models is the in-memory model store — populated by POST /model/new,
	// updated by POST /model/update, cleared by POST /model/delete. Lets
	// GET /model/info return realistic model_info.id values so the Model
	// reconciler can persist and re-use the UUID across reconciles.
	mu                sync.Mutex
	recorded          []RecordedCall
	models            map[string]*modelEntry // keyed by model_name (the operator-owned "primary" row)
	modelSeq          atomic.Int64           // monotonically increasing UUID suffix
	perModelMutations map[string]int64       // tracks mutation count per model_name

	// dupModelRows holds EXTRA deployment rows injected for a model_name
	// beyond the single primary entry in `models`, keyed by model_name →
	// list of extra model_ids. LiteLLM allows multiple deployment rows per
	// model_name (POST /model/new is NOT idempotent); the `models` map only
	// models the one-row-per-name happy path, so this parallel store lets
	// duplicate-row tests reproduce the id-drift churn condition. The
	// GET /model/info?model_name= handler appends these rows; POST
	// /model/delete removes a matching id from here too. Test-only.
	dupModelRows map[string][]string

	// lastModelInfo records the model_info sub-block of the most recent
	// POST /model/new or /model/update for each model_name, so tests can
	// assert spec.info forwarding. Guarded by m.mu.
	lastModelInfo map[string]map[string]any

	// MCP server state. Two indices for O(1) lookups:
	// mcpServers — keyed by ServerID (the LiteLLM-assigned UUID, surfaced
	// in POST/PUT response bodies + DELETE path param)
	// mcpByName — keyed by ServerName → ServerID, mirrors the in-memory
	// filter the operator does after GET /v1/mcp/server
	// perMCPMutations tracks mutation count per ServerName so envtests can
	// assert AC-MS1-style "exactly N mutations for server X" properties.
	mcpServers      map[string]*mcpEntry // keyed by ServerID
	mcpByName       map[string]string    // ServerName → ServerID
	mcpSeq          atomic.Int64
	perMCPMutations map[string]int64

	// A2A agent state. Two indices for O(1) lookups:
	// agents — keyed by AgentID (the LiteLLM-assigned UUID, surfaced
	// in POST/PUT response bodies + DELETE path param)
	// agentByName — keyed by AgentName → AgentID, mirrors the in-memory
	// filter the operator does after GET /v1/agents
	// perAgentMutations tracks mutation count per AgentName so envtests can
	// assert AC-A1-style "exactly N mutations for agent X" properties.
	agents            map[string]*agentEntry // keyed by AgentID
	agentByName       map[string]string      // AgentName → AgentID
	agentSeq          atomic.Int64
	perAgentMutations map[string]int64

	// MCP toolset state. Mirrors the A2A shape because the LiteLLM
	// semantics match: the server MINTS toolset_id (a caller-supplied one
	// is ignored) and adoption goes through the unique toolset_name.
	// toolsets — keyed by ToolsetID
	// toolsetByName — keyed by ToolsetName → ToolsetID; toolset_name is
	// UNIQUE server-side, so a duplicate POST is a 409.
	toolsets            map[string]*toolsetEntry
	toolsetByName       map[string]string
	toolsetSeq          atomic.Int64
	perToolsetMutations map[string]int64

	// Access group state. Mirrors the toolset shape: the server MINTS
	// access_group_id (a caller-supplied one is ignored) and adoption goes
	// through the unique access_group_name.
	accessGroups      map[string]*accessGroupEntry // keyed by AccessGroupID
	accessGroupByName map[string]string            // AccessGroupName → AccessGroupID
	accessGroupSeq    atomic.Int64

	// Team state. Two indices for O(1) lookups:
	// teams — keyed by TeamID (the LiteLLM-assigned UUID,
	// surfaced in POST/UPDATE response bodies + DELETE
	// request body's team_ids[] array)
	// teamByAlias — keyed by TeamAlias → TeamID, mirrors the in-memory
	// exact-match filter the operator applies after
	// GET /v2/team/list?team_alias=. (spec §6.7
	// partial-match server-side → exact-match client-side).
	// perTeamMutations tracks mutation count per TeamAlias so envtests can
	// assert AC-T1 / AC-T6 / AC-DC1 team-slice invariants ("exactly N
	// mutations against the operator-declared alias", "zero mutations
	// against hand-managed aliases").
	teams            map[string]*teamEntry // keyed by TeamID
	teamByAlias      map[string]string     // TeamAlias → TeamID
	teamSeq          atomic.Int64
	perTeamMutations map[string]int64

	// deleteTeamCalls captures every team_id that the operator passed to
	// POST /team/delete via the request body's team_ids[] array. Phase 6
	// AC-T4 envtests assert this list is EMPTY across the
	// entire Team/default CRUD-and-delete cycle (the protected-default
	// deletion path re-applies the implicit empty body via POST
	// /team/update — the team-delete endpoint is never touched against
	// the default-aliased team_id).
	deleteTeamCalls []string

	// pathCalls counts every HTTP request the mock has handled, keyed by
	// the normalized URL path (parameterized IDs collapsed — e.g.
	// /v1/mcp/server/<id> → /v1/mcp/server/*). Incremented under m.mu in
	// the central handle entry. PathCallCount(prefix) sums all entries
	// whose key starts with `prefix`. Phase 6 Hub seam E2E
	// uses PathCallCount for the user and key path prefixes as the
	// load-bearing TEAM-09 + SCOPE-03 negative assertion.
	pathCalls map[string]int64

	// guardrails carries the in-memory LiteLLM guardrail store.
	// Lazy-initialised the first time a guardrail handler / helper
	// fires so existing tests that never touch guardrails do not pay
	// any setup cost. See mock/guardrail.go for the per-route logic.
	guardrails *guardrailState

	// routerSettings is the in-memory mirror of LiteLLM router_settings
	// driven by POST /config/update and read by GET /get/config/callbacks.
	// Used by ModelAlias envtests to assert the operator's read-merge-write
	// against router_settings.model_group_alias.
	routerSettings map[string]any
}

// RecordedCall is one entry in the mock's audit log.
type RecordedCall struct {
	Method string
	Path   string
	When   time.Time
}

// NewServer constructs a MockServer in ModeHappy and starts the underlying
// httptest.Server. The *testing.T is optional — pass nil from TestMain or
// other non-Test* code paths; pass the real t from a Test* function so
// t.Helper works and t.Cleanup auto-closes the server.
func NewServer(t *testing.T) *MockServer {
	if t != nil {
		t.Helper()
	}
	m := &MockServer{
		models:            make(map[string]*modelEntry),
		perModelMutations: make(map[string]int64),
		lastModelInfo:     make(map[string]map[string]any),
		dupModelRows:      make(map[string][]string),
		mcpServers:        make(map[string]*mcpEntry),
		mcpByName:         make(map[string]string),
		perMCPMutations:   make(map[string]int64),
		agents:            make(map[string]*agentEntry),
		agentByName:       make(map[string]string),
		perAgentMutations: make(map[string]int64),

		toolsets:            make(map[string]*toolsetEntry),
		toolsetByName:       make(map[string]string),
		perToolsetMutations: make(map[string]int64),

		accessGroups:      make(map[string]*accessGroupEntry),
		accessGroupByName: make(map[string]string),

		teams:            make(map[string]*teamEntry),
		teamByAlias:      make(map[string]string),
		perTeamMutations: make(map[string]int64),
		pathCalls:        make(map[string]int64),
		routerSettings:   map[string]any{"model_group_alias": map[string]any{}},
	}
	m.mode.Store(ModeHappy)
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	if t != nil {
		t.Cleanup(m.Close)
	}
	return m
}

// URL returns the mock server's base URL — pass this to litellm.NewClient.
func (m *MockServer) URL() string { return m.srv.URL }

// Close shuts the httptest.Server down.
func (m *MockServer) Close() {
	if m.srv != nil {
		m.srv.Close()
	}
}

// Mutations returns the count of POST/PUT/DELETE/PATCH calls observed.
func (m *MockServer) Mutations() int64 { return m.mutations.Load() }

// Reads returns the count of GET/HEAD calls observed.
func (m *MockServer) Reads() int64 { return m.reads.Load() }

// ResetCounters zeroes the mutation + read counters. Useful between
// test phases (e.g. settle the first reconcile, then start counting).
func (m *MockServer) ResetCounters() {
	m.mutations.Store(0)
	m.reads.Store(0)
}

// ResetRecorded clears the audit log of captured requests. Call before a
// test that wants to assert on the exact set of calls issued.
func (m *MockServer) ResetRecorded() {
	m.mu.Lock()
	m.recorded = nil
	m.mu.Unlock()
}

// ResetModels clears the in-memory model store. Call between tests that
// need a clean slate for GET /model/info responses.
func (m *MockServer) ResetModels() {
	m.mu.Lock()
	m.models = make(map[string]*modelEntry)
	m.perModelMutations = make(map[string]int64)
	m.lastModelInfo = make(map[string]map[string]any)
	m.dupModelRows = make(map[string][]string)
	m.mu.Unlock()
}

// ResetMCPServers clears the in-memory MCP server store. Call between
// tests that need a clean slate for GET /v1/mcp/server responses.
func (m *MockServer) ResetMCPServers() {
	m.mu.Lock()
	m.mcpServers = make(map[string]*mcpEntry)
	m.mcpByName = make(map[string]string)
	m.perMCPMutations = make(map[string]int64)
	m.mu.Unlock()
}

// AddHandManagedMCPServer inserts an MCP server into the mock's internal
// store as if it was created out-of-band (NOT via POST /v1/mcp/server
// through the operator). Returns the minted server_id UUID. Used by
// Phase 5 envtests that need a pre-existing entry visible to
// ListMCPServers — e.g. when simulating an MCPServer CR re-reconcile
// against a server already known to LiteLLM.
func (m *MockServer) AddHandManagedMCPServer(name, url, transport string) string {
	seq := m.mcpSeq.Add(1)
	serverID := fmt.Sprintf("mock-mcp-server-id-%d", seq)
	m.mu.Lock()
	m.mcpServers[serverID] = &mcpEntry{
		ServerID:   serverID,
		ServerName: name,
		URL:        url,
		Transport:  transport,
	}
	m.mcpByName[name] = serverID
	m.mu.Unlock()
	return serverID
}

// DeleteMCPServerOutOfBand removes an MCP server from the mock's internal
// store WITHOUT going through the HTTP handler. Simulates an out-of-band
// DELETE in LiteLLM.
func (m *MockServer) DeleteMCPServerOutOfBand(serverID string) {
	m.mu.Lock()
	if e, ok := m.mcpServers[serverID]; ok {
		delete(m.mcpByName, e.ServerName)
	}
	delete(m.mcpServers, serverID)
	m.mu.Unlock()
}

// HasMCPServer returns true if the mock's internal store contains an MCP
// server with the given name.
func (m *MockServer) HasMCPServer(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mcpByName[name]
	return ok
}

// GetMCPServerID returns the server_id the mock assigned to an MCP
// server with the given name. Returns "" if no entry with that name
// exists.
func (m *MockServer) GetMCPServerID(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.mcpByName[name]; ok {
		return id
	}
	return ""
}

// MutationsByMCPServerName returns the number of MCP mutation calls
// (POST/PUT/DELETE /v1/mcp/server*) that touched the given server name.
// Used by Phase 5 envtests to assert AC-MS1-style invariants.
func (m *MockServer) MutationsByMCPServerName(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perMCPMutations[name]
}

// LastMCPBody returns the most-recent POST/PUT body received for the
// given MCP server name. Returns nil if no body has been captured. Used
// by Phase 5 AC-SEC4-PROPAGATE envtest to verify the resolved
// secret value on a child-PUT after a Secret rotation propagates from
// MSDisc → MCPServer reconciler.
func (m *MockServer) LastMCPBody(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.mcpByName[name]; ok {
		if e, ok2 := m.mcpServers[id]; ok2 && e.LastBody != nil {
			// Return a shallow copy so callers can read without holding the mutex.
			cp := make(map[string]any, len(e.LastBody))
			for k, v := range e.LastBody {
				cp[k] = v
			}
			return cp
		}
	}
	return nil
}

// ── A2A agent helpers ────────────────────────────────

// ResetAgents clears the in-memory A2A agent store. Call between tests
// that need a clean slate for GET /v1/agents responses.
func (m *MockServer) ResetAgents() {
	m.mu.Lock()
	m.agents = make(map[string]*agentEntry)
	m.agentByName = make(map[string]string)
	m.perAgentMutations = make(map[string]int64)
	m.mu.Unlock()
}

// AddHandManagedAgent inserts an A2A agent into the mock's internal store
// as if it was created out-of-band (NOT via POST /v1/agents through the
// operator). Returns the minted agent_id UUID. Used by Phase 5 envtests
// that need a pre-existing entry visible to ListAgents — e.g. when
// simulating an A2AAgent CR re-reconcile against an agent already known
// to LiteLLM.
func (m *MockServer) AddHandManagedAgent(name string) string {
	seq := m.agentSeq.Add(1)
	agentID := fmt.Sprintf("mock-agent-id-%d", seq)
	m.mu.Lock()
	m.agents[agentID] = &agentEntry{
		AgentID:   agentID,
		AgentName: name,
	}
	m.agentByName[name] = agentID
	m.mu.Unlock()
	return agentID
}

// ── MCP toolset helpers ──────────────────────────────

// ResetToolsets clears the in-memory MCP toolset store. Call between tests
// that need a clean slate for GET /v1/mcp/toolset responses.
func (m *MockServer) ResetToolsets() {
	m.mu.Lock()
	m.toolsets = make(map[string]*toolsetEntry)
	m.toolsetByName = make(map[string]string)
	m.perToolsetMutations = make(map[string]int64)
	m.mu.Unlock()
}

// SeedToolset inserts an MCP toolset into the mock's store as if it were
// created out-of-band (NOT via POST /v1/mcp/toolset through the operator).
// Returns the minted toolset_id. Used by envtests that need a pre-existing
// entry visible to ListMCPToolsets — notably the 409-adoption path, where a
// toolset with the CR's name already exists in LiteLLM.
func (m *MockServer) SeedToolset(name string) string {
	seq := m.toolsetSeq.Add(1)
	toolsetID := fmt.Sprintf("mock-toolset-id-%d", seq)
	m.mu.Lock()
	m.toolsets[toolsetID] = &toolsetEntry{
		ToolsetID:   toolsetID,
		ToolsetName: name,
		Tools:       []any{},
	}
	m.toolsetByName[name] = toolsetID
	m.mu.Unlock()
	return toolsetID
}

// DeleteToolsetOutOfBand removes an MCP toolset from the mock's store WITHOUT
// going through the HTTP handler. Simulates an out-of-band DELETE in LiteLLM,
// which the operator's vanish probe should detect.
func (m *MockServer) DeleteToolsetOutOfBand(toolsetID string) {
	m.mu.Lock()
	if e, ok := m.toolsets[toolsetID]; ok {
		delete(m.toolsetByName, e.ToolsetName)
	}
	delete(m.toolsets, toolsetID)
	m.mu.Unlock()
}

// HasToolset reports whether the mock's store contains a toolset with the
// given name.
func (m *MockServer) HasToolset(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.toolsetByName[name]
	return ok
}

// GetToolsetID returns the toolset_id the mock minted for the given name, or
// "" if no entry with that name exists.
func (m *MockServer) GetToolsetID(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.toolsetByName[name]; ok {
		return id
	}
	return ""
}

// MutationsByToolsetName returns the number of toolset mutation calls
// (POST/PUT/DELETE /v1/mcp/toolset*) that touched the given toolset name.
func (m *MockServer) MutationsByToolsetName(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perToolsetMutations[name]
}

// LastToolsetBody returns the most-recent POST/PUT body received for the
// given toolset name, or nil if none was captured. Envtests use it to assert
// the rendered `tools` array — in particular that an emptied list arrives as
// a PRESENT empty array (ALWAYS-EMIT) rather than being omitted.
func (m *MockServer) LastToolsetBody(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.toolsetByName[name]
	if !ok {
		return nil
	}
	e, ok := m.toolsets[id]
	if !ok || e.LastBody == nil {
		return nil
	}
	cp := make(map[string]any, len(e.LastBody))
	for k, v := range e.LastBody {
		cp[k] = v
	}
	return cp
}

// ── Access group helpers ──────────────────────────────

// AccessGroupSnapshot is a read-only copy of one stored access group, handed
// to tests. Exported (unlike accessGroupEntry) because envtests assert on it.
type AccessGroupSnapshot struct {
	AccessGroupID      string
	AccessGroupName    string
	Description        string
	AccessModelNames   []string
	AccessMCPServerIDs []string
	AccessAgentIDs     []string
	AssignedTeamIDs    []string
}

// ResetAccessGroups clears the in-memory access-group store. Call between
// tests that need a clean slate for GET /v1/access_group responses.
func (m *MockServer) ResetAccessGroups() {
	m.mu.Lock()
	m.accessGroups = make(map[string]*accessGroupEntry)
	m.accessGroupByName = make(map[string]string)
	m.mu.Unlock()
}

// AccessGroups returns a name-sorted snapshot of the stored access groups.
func (m *MockServer) AccessGroups() []AccessGroupSnapshot {
	m.mu.Lock()
	out := make([]AccessGroupSnapshot, 0, len(m.accessGroups))
	for _, e := range m.accessGroups {
		out = append(out, AccessGroupSnapshot{
			AccessGroupID:      e.AccessGroupID,
			AccessGroupName:    e.AccessGroupName,
			Description:        e.Description,
			AccessModelNames:   append([]string{}, e.AccessModelNames...),
			AccessMCPServerIDs: append([]string{}, e.AccessMCPServerIDs...),
			AccessAgentIDs:     append([]string{}, e.AccessAgentIDs...),
			AssignedTeamIDs:    append([]string{}, e.AssignedTeamIDs...),
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].AccessGroupName < out[j].AccessGroupName
	})
	return out
}

// SeedAccessGroup inserts an access group into the mock's store as if it were
// created out-of-band (NOT via POST /v1/access_group through the operator).
// Returns the minted access_group_id. Used by the envtest that exercises the
// 409-adoption path, where a group with the CR's name already exists.
func (m *MockServer) SeedAccessGroup(name string) string {
	id := fmt.Sprintf("mock-access-group-id-%d", m.accessGroupSeq.Add(1))
	m.mu.Lock()
	m.accessGroups[id] = &accessGroupEntry{
		AccessGroupID:      id,
		AccessGroupName:    name,
		AccessModelNames:   []string{},
		AccessMCPServerIDs: []string{},
		AccessAgentIDs:     []string{},
		AssignedTeamIDs:    []string{},
		AssignedKeyIDs:     []string{},
	}
	m.accessGroupByName[name] = id
	m.mu.Unlock()
	return id
}

// DeleteAccessGroupOutOfBand removes an access group from the mock's store
// WITHOUT going through the HTTP handler. Simulates an out-of-band DELETE in
// LiteLLM, which the operator's vanish probe should detect.
func (m *MockServer) DeleteAccessGroupOutOfBand(id string) {
	m.mu.Lock()
	if e, ok := m.accessGroups[id]; ok {
		delete(m.accessGroupByName, e.AccessGroupName)
	}
	delete(m.accessGroups, id)
	m.mu.Unlock()
}

// DeleteAgentOutOfBand removes an A2A agent from the mock's internal
// store WITHOUT going through the HTTP handler. Simulates an out-of-band
// DELETE in LiteLLM.
func (m *MockServer) DeleteAgentOutOfBand(agentID string) {
	m.mu.Lock()
	if e, ok := m.agents[agentID]; ok {
		delete(m.agentByName, e.AgentName)
	}
	delete(m.agents, agentID)
	m.mu.Unlock()
}

// HasAgent returns true if the mock's internal store contains an A2A
// agent with the given name.
func (m *MockServer) HasAgent(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.agentByName[name]
	return ok
}

// GetAgentID returns the agent_id the mock assigned to an A2A agent
// with the given name. Returns "" if no entry with that name exists.
func (m *MockServer) GetAgentID(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.agentByName[name]; ok {
		return id
	}
	return ""
}

// MutationsByAgentName returns the number of A2A mutation calls
// (POST/PUT/DELETE /v1/agents*) that touched the given agent name.
// Used by Phase 5 envtests to assert AC-A1-style invariants.
func (m *MockServer) MutationsByAgentName(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perAgentMutations[name]
}

// LastAgentBody returns the most-recent POST/PUT body received for the
// given agent name. Returns nil if no body has been captured. Used by
// Phase 5 envtests to verify the four ProjectionOverride
// collision-key projections (agent_name, agent_card_params,
// agent_card_params.url, model_info) and the resolved value of
// substituted placeholders in spec.agentCard.skills[].description.
func (m *MockServer) LastAgentBody(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.agentByName[name]; ok {
		if e, ok2 := m.agents[id]; ok2 && e.LastBody != nil {
			// Return a shallow copy so callers can read without holding the mutex.
			cp := make(map[string]any, len(e.LastBody))
			for k, v := range e.LastBody {
				cp[k] = v
			}
			return cp
		}
	}
	return nil
}

// ── Team helpers ─────────────────────────────────────

// ResetTeams clears the in-memory Team store. Call between tests that
// need a clean slate for GET /v2/team/list responses.
func (m *MockServer) ResetTeams() {
	m.mu.Lock()
	m.teams = make(map[string]*teamEntry)
	m.teamByAlias = make(map[string]string)
	m.perTeamMutations = make(map[string]int64)
	m.deleteTeamCalls = nil
	m.mu.Unlock()
}

// DeleteTeamCalls returns the list of team_id values the operator
// passed to POST /team/delete since the last ResetTeams. Phase 6
// AC-T4 envtests assert this list is EMPTY across the
// entire Team/default CRUD-and-delete cycle — the protected-default
// deletion path re-applies the implicit empty body via POST
// /team/update and never invokes the team-delete endpoint against
// the default-aliased team_id.
func (m *MockServer) DeleteTeamCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.deleteTeamCalls))
	copy(out, m.deleteTeamCalls)
	return out
}

// AddHandManagedTeam inserts a Team into the mock's internal store as if
// it was created out-of-band (NOT via POST /team/new through the
// operator). Returns the minted team_id UUID. Used by Phase 6 envtests
// (AC-DC1 team slice) that need a pre-existing entry visible to
// ListTeamsByAlias — e.g. when asserting that operator reconciliation of
// a DIFFERENT alias never touches the hand-managed entry.
func (m *MockServer) AddHandManagedTeam(alias string) string {
	seq := m.teamSeq.Add(1)
	teamID := fmt.Sprintf("mock-team-id-%d", seq)
	m.mu.Lock()
	m.teams[teamID] = &teamEntry{
		TeamID:    teamID,
		TeamAlias: alias,
	}
	m.teamByAlias[alias] = teamID
	m.mu.Unlock()
	return teamID
}

// AddHandManagedTeamWithID inserts a Team into the mock's internal store
// with a caller-chosen team_id (instead of a minted UUID). Used by the
// adopt envtest to seed a name-id team — team_id == team_alias == name —
// that simulates a team the operator previously created with the
// name-as-id scheme and must re-adopt by alias after a status loss.
func (m *MockServer) AddHandManagedTeamWithID(alias, teamID string) {
	m.mu.Lock()
	m.teams[teamID] = &teamEntry{
		TeamID:    teamID,
		TeamAlias: alias,
	}
	m.teamByAlias[alias] = teamID
	m.mu.Unlock()
}

// DeleteTeamOutOfBand removes a Team from the mock's internal store
// WITHOUT going through the HTTP handler. Simulates an out-of-band
// DELETE in LiteLLM (e.g. a hand admin removes the entry directly).
func (m *MockServer) DeleteTeamOutOfBand(teamID string) {
	m.mu.Lock()
	if e, ok := m.teams[teamID]; ok {
		delete(m.teamByAlias, e.TeamAlias)
	}
	delete(m.teams, teamID)
	m.mu.Unlock()
}

// HasTeam returns true if the mock's internal store contains a Team
// with the given alias.
func (m *MockServer) HasTeam(alias string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.teamByAlias[alias]
	return ok
}

// GetTeamID returns the team_id the mock assigned to a Team with the
// given alias. Returns "" if no entry with that alias exists.
func (m *MockServer) GetTeamID(alias string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.teamByAlias[alias]; ok {
		return id
	}
	return ""
}

// MutationsByTeamAlias returns the number of Team mutation calls
// (POST /team/new + POST /team/update + POST /team/delete) that touched
// the given alias. Used by Phase 6 envtests to assert AC-T1 / AC-T6 /
// AC-DC1 team-slice invariants ("exactly N mutations against the
// operator-declared alias", "zero mutations against hand-managed
// aliases").
func (m *MockServer) MutationsByTeamAlias(alias string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perTeamMutations[alias]
}

// LastTeamBody returns the most-recent POST /team/new or POST
// /team/update body received for the given Team alias. Returns nil if
// no body has been captured. Used by Phase 6 envtests to
// verify: (a) operator structural overlays (team_alias, max_budget,
// budget_duration) WIN on the wire over identically-keyed entries in
// spec.params; (b) absent-budget → JSON null preservation (max_budget /
// budget_duration entries present in body with nil values per spec §6.7
// line 1194).
func (m *MockServer) LastTeamBody(alias string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.teamByAlias[alias]; ok {
		if e, ok2 := m.teams[id]; ok2 && e.LastBody != nil {
			// Return a shallow copy so callers can read without holding the mutex.
			cp := make(map[string]any, len(e.LastBody))
			for k, v := range e.LastBody {
				cp[k] = v
			}
			return cp
		}
	}
	return nil
}

// AddHandManagedModel inserts a model into the mock's internal store as if
// it was created out-of-band (i.e. NOT via POST /model/new through the
// operator). This simulates pre-existing LiteLLM entries that the operator
// does NOT own. Used by OWN-06 hand-managed entry tests.
func (m *MockServer) AddHandManagedModel(name, id string) {
	m.mu.Lock()
	m.models[name] = &modelEntry{ModelID: id, ModelName: name}
	m.mu.Unlock()
}

// AddDuplicateModelRow injects an EXTRA deployment row for an existing
// model_name with a distinct model_id, simulating the LiteLLM duplicate-rows
// state that drives the id-drift churn (POST /model/new is NOT idempotent, so
// real LiteLLM accumulates multiple rows under one name). After this call,
// GET /model/info?model_name=<name> returns the primary `models` entry PLUS
// every injected duplicate row, and POST /model/delete {"id":<dupID>} removes
// exactly that duplicate row. Test-only.
func (m *MockServer) AddDuplicateModelRow(modelName, modelID string) {
	m.mu.Lock()
	m.dupModelRows[modelName] = append(m.dupModelRows[modelName], modelID)
	m.mu.Unlock()
}

// DeleteModelOutOfBand removes a model from the mock's internal store
// WITHOUT going through the HTTP handler. Simulates an out-of-band DELETE
// of a model entry in LiteLLM (e.g. a human admin deletes it directly).
// Used by TestModel_DriftCounter_CreateMissing_SafetyRelist.
func (m *MockServer) DeleteModelOutOfBand(name string) {
	m.mu.Lock()
	delete(m.models, name)
	m.mu.Unlock()
}

// MutationsByModelName returns the number of mutation calls (POST /model/new,
// /model/update, /model/delete) that referenced the given model name.
// Used by OWN-06 tests to assert that hand-managed entries were not touched.
func (m *MockServer) MutationsByModelName(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perModelMutations[name]
}

// HasModel returns true if the mock's internal store contains a model with
// the given name. Used by OWN-06 tests to assert hand-managed entry presence.
func (m *MockServer) HasModel(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.models[name]
	return ok
}

// GetModelEntry returns the model entry for the given name, or nil if absent.
func (m *MockServer) GetModelEntry(name string) *modelEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.models[name]; ok {
		cp := *e
		return &cp
	}
	return nil
}

// ModelEntry holds the model name and ID returned by GetModelEntry.
// Re-exported as a public alias for test assertions.
type ModelEntry = modelEntry

// GetModelID returns the model_info.id UUID the mock assigned to a model
// created with the given name. Returns "" if no model with that name exists.
func (m *MockServer) GetModelID(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.models[name]; ok {
		return e.ModelID
	}
	return ""
}

// LastModelInfoBody returns the model_info sub-block of the most recent
// POST /model/new or /model/update for the given model_name (nil if none).
func (m *MockServer) LastModelInfoBody(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastModelInfo[name]
}

// SetLoggingCallbacksNull controls whether POST /key/health returns
// logging_callbacks: null (true) or the default healthy-callback list
// (false). Used by UAT LOW-01 regression tests to assert the operator
// surfaces a NoCallbacksReported reason instead of an empty Unknown.
// Safe to flip concurrently with handler reads.
func (m *MockServer) SetLoggingCallbacksNull(v bool) {
	m.loggingCallbacksNull.Store(v)
}

// SetMode switches the mock's response mode. Valid values are ModeHappy,
// Mode401, ModeTransient5xx, ModeSlow, Mode422, ModeNotFoundListTeams,
// ModeNotFoundDeleteTeam, Mode401DeleteTeam. Unknown values fall back to
// ModeHappy so a fat-fingered test doesn't silently hang.
func (m *MockServer) SetMode(mode string) {
	switch mode {
	case ModeHappy, Mode401, ModeTransient5xx, ModeSlow, Mode422,
		ModeNotFoundListTeams, ModeNotFoundDeleteTeam, Mode401DeleteTeam:
		m.mode.Store(mode)
	default:
		m.mode.Store(ModeHappy)
	}
}

// PathCallCount returns the count of every HTTP request whose URL path
// starts with `prefix`. Used by Phase 6 Hub seam E2E to assert
// the TEAM-09 + SCOPE-03 negative invariant — zero operator-issued calls
// to /user/* or /key/* across a full reconcile cycle (the operator owns
// Team alias + budget; external identity system owns User/VirtualKey/team-membership).
//
// The match is path-prefix only (no method filter); reads + mutations
// both count. Callers that need method discrimination can use Recorded
// instead.
//
// Counts persist across SetMode flips; ResetCounters does NOT clear
// pathCalls (the counter is structural — once a request happens, the
// fact that it happened is permanent for the test's lifetime). Tests
// that need a clean slate should construct a fresh MockServer.
func (m *MockServer) PathCallCount(prefix string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for path, n := range m.pathCalls {
		if strings.HasPrefix(path, prefix) {
			total += n
		}
	}
	return total
}

// Mode returns the current response mode.
func (m *MockServer) Mode() string {
	v, _ := m.mode.Load().(string)
	if v == "" {
		return ModeHappy
	}
	return v
}

// Recorded returns a copy of all captured requests. Snapshot semantics —
// the caller may iterate without holding the mock's lock.
func (m *MockServer) Recorded() []RecordedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedCall, len(m.recorded))
	copy(out, m.recorded)
	return out
}

// writeDuplicateToolsetConflict answers POST /v1/mcp/toolset with a 409 when
// the requested toolset_name is already taken, and reports whether it wrote
// the response.
//
// toolset_name is UNIQUE in LiteLLM 1.93.0 (verified: a duplicate create
// returns `{"detail":{"error":"A toolset named 'X' already exists."}}`), and
// the operator's CREATE arm adopts the existing toolset on that 409. This
// lives outside statefulBody because the happy path writes StatusOK before
// statefulBody runs, so a body-only route cannot set a status code. Gated on
// happy mode so the fault modes still win.
func (m *MockServer) writeDuplicateToolsetConflict(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodPost || r.URL.Path != pathMCPToolset {
		return false
	}
	body, _ := io.ReadAll(r.Body)
	// Restore the body for statefulBody when this is NOT a duplicate.
	r.Body = io.NopCloser(bytes.NewReader(body))
	var reqBody map[string]any
	_ = json.Unmarshal(body, &reqBody)
	name, _ := reqBody["toolset_name"].(string)

	m.mu.Lock()
	_, dup := m.toolsetByName[name]
	m.mu.Unlock()
	if !dup {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(fmt.Sprintf(
		`{"detail":{"error":"A toolset named '%s' already exists."}}`, name)))
	return true
}

// writeAbsentToolsetDelete500 reproduces a REAL LiteLLM 1.93.0 defect:
// DELETE /v1/mcp/toolset/<id> for a toolset that no longer exists returns
// HTTP 500, not 404. Upstream:
//
//	toolset_db.py:110  delete_mcp_toolset
//	    row = await ...delete(...)      # None when the row is absent
//	    return _toolset_from_row(row)
//	toolset_db.py:16   _toolset_from_row
//	    AttributeError: 'NoneType' object has no attribute 'model_dump'
//
// This matters because a naive reconciler classifies 500 as transient and
// retries the finalizer forever, stranding the CR in Terminating (observed
// in e2e TOOLSET-05). The mock reproduces it so the operator's
// confirmed-absent drain stays regression-locked.
//
// Returns whether it wrote the response.
func (m *MockServer) writeAbsentToolsetDelete500(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodDelete ||
		!strings.HasPrefix(r.URL.Path, pathMCPToolset+"/") {
		return false
	}
	toolsetID := strings.TrimPrefix(r.URL.Path, pathMCPToolset+"/")
	m.mu.Lock()
	_, present := m.toolsets[toolsetID]
	m.mu.Unlock()
	if present {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(
		`{"error":{"message":"AttributeError: 'NoneType' object has no attribute 'model_dump'",` +
			`"type":"internal_server_error","param":null,"code":"500"}}`))
	return true
}

// writeAccessGroupCreate serves POST /v1/access_group. It lives here rather
// than in statefulBody because the happy path writes StatusOK before
// statefulBody runs, and this endpoint answers 201 — VERIFIED against stock
// LiteLLM 1.93.0 on 2026-08-06. A duplicate access_group_name answers 409,
// which is how the operator's CREATE arm learns to adopt by name.
//
// Gated on happy mode so the fault modes (401 / transient 5xx / slow) still
// win, exactly like writeDuplicateToolsetConflict.
//
// Returns whether it wrote the response.
func (m *MockServer) writeAccessGroupCreate(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodPost || r.URL.Path != pathAccessGroup {
		return false
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		AccessGroupName    string   `json:"access_group_name"`
		Description        string   `json:"description"`
		AccessModelNames   []string `json:"access_model_names"`
		AccessMCPServerIDs []string `json:"access_mcp_server_ids"`
		AccessAgentIDs     []string `json:"access_agent_ids"`
	}
	_ = json.Unmarshal(body, &req)

	m.mu.Lock()
	if _, dup := m.accessGroupByName[req.AccessGroupName]; dup {
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		body, _ := json.Marshal(map[string]any{"detail": map[string]any{
			"error": fmt.Sprintf("An access group named '%s' already exists.", req.AccessGroupName),
		}})
		_, _ = w.Write(body)
		return true
	}

	// The server MINTS the id; a caller-supplied access_group_id is ignored.
	id := fmt.Sprintf("mock-access-group-id-%d", m.accessGroupSeq.Add(1))
	e := &accessGroupEntry{
		AccessGroupID:      id,
		AccessGroupName:    req.AccessGroupName,
		Description:        req.Description,
		AccessModelNames:   nonNilStrings(req.AccessModelNames),
		AccessMCPServerIDs: nonNilStrings(req.AccessMCPServerIDs),
		AccessAgentIDs:     nonNilStrings(req.AccessAgentIDs),
		AssignedTeamIDs:    []string{},
		AssignedKeyIDs:     []string{},
	}
	m.accessGroups[id] = e
	m.accessGroupByName[req.AccessGroupName] = id
	out, _ := json.Marshal(e)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out)
	return true
}

// writeAccessGroupDelete404 answers DELETE /v1/access_group/<id> with 404 when
// the group is already gone. Unlike the MCP toolset endpoint (which 500s on an
// absent row — see writeAbsentToolsetDelete500), the access-group endpoint
// behaves correctly, so the operator's confirmed-absent drain sees a clean 404.
//
// Returns whether it wrote the response.
func (m *MockServer) writeAccessGroupDelete404(w http.ResponseWriter, r *http.Request, mode string) bool {
	if mode != ModeHappy || r.Method != http.MethodDelete ||
		!strings.HasPrefix(r.URL.Path, pathAccessGroup+"/") {
		return false
	}
	id := strings.TrimPrefix(r.URL.Path, pathAccessGroup+"/")
	m.mu.Lock()
	_, present := m.accessGroups[id]
	m.mu.Unlock()
	if present {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(fmt.Sprintf(
		`{"detail":{"error":"Access group %s not found."}}`, id)))
	return true
}

// writeAccessGroupResponse dispatches to the two access-group status-code
// writers. Folded into a single call site (rather than two separate `if`s in
// handle()) to keep handle()'s cyclomatic complexity under the gocyclo gate —
// the two conditions are mutually exclusive (POST vs DELETE), so combining
// them changes no behavior.
//
// Returns whether one of them wrote the response.
func (m *MockServer) writeAccessGroupResponse(w http.ResponseWriter, r *http.Request, mode string) bool {
	if m.writeAccessGroupCreate(w, r, mode) {
		return true
	}
	return m.writeAccessGroupDelete404(w, r, mode)
}

// nonNilStrings coerces a nil slice to empty so the mock's JSON renders [],
// never null — matching what LiteLLM returns for these fields.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// handle is the single HTTP handler routed to every request. It records
// the call, increments the appropriate counter, and emits a mode-specific
// response.
func (m *MockServer) handle(w http.ResponseWriter, r *http.Request) {
	// Per-method bucketing — GET/HEAD are reads, everything else is a
	// mutation. PATCH is included for forward-compat (LiteLLM 1.83.10
	// does not currently use PATCH for the verbs the operator issues,
	// but the upstream OpenAPI does mention PATCH /model/{id}/update —
	// recording it lets us catch any future regression).
	//
	// Special case: POST /key/health is the connection probe (formerly
	// GET /models). Even though the HTTP verb is POST, semantically the
	// probe is a read-only health check owned by the LiteLLMConnection
	// reconciler, not a domain mutation. Counting it under reads
	// preserves the invariant that every test asserting "Mutations() == 0
	// when the reconciler-under-test does not call domain mutation paths"
	// continues to hold across the probe swap.
	switch {
	case strings.ToUpper(r.Method) == http.MethodPost && r.URL.Path == "/key/health":
		m.reads.Add(1)
	case strings.ToUpper(r.Method) == http.MethodGet,
		strings.ToUpper(r.Method) == http.MethodHead:
		m.reads.Add(1)
	default:
		m.mutations.Add(1)
	}

	m.mu.Lock()
	m.recorded = append(m.recorded, RecordedCall{
		Method: r.Method,
		Path:   r.URL.Path,
		When:   time.Now(),
	})
	// pathCalls: increment under the same mutex so PathCallCount sees a
	// consistent snapshot. Use the verbatim path (no parameter
	// normalization at the per-request site — PathCallCount does prefix
	// matching at read time, which is sufficient for the /user/ and /key/
	// negative assertion that Hub seam needs).
	m.pathCalls[r.URL.Path]++
	m.mu.Unlock()

	mode := m.Mode()

	// ─── Phase 6 route-targeted modes ──────────────────────────
	// These three modes flip a single Team route to a non-2xx response;
	// every other path is served as happy. Mutually exclusive with the
	// per-everything modes above.
	if mode == ModeNotFoundListTeams && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/team/list") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found","type":"not_found_error","param":null,"code":"404"}}`))
		return
	}
	if mode == ModeNotFoundDeleteTeam && r.Method == http.MethodPost && r.URL.Path == pathTeamDelete {
		// Per spec §7.5 line 1332, the operator treats POST /team/delete
		// 404 as success. The mock still records the team_id passed in
		// the request body so the test can assert that the operator
		// DID make the call.
		var reqBody struct {
			TeamIDs []string `json:"team_ids"`
		}
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		m.mu.Lock()
		m.deleteTeamCalls = append(m.deleteTeamCalls, reqBody.TeamIDs...)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found","type":"not_found_error","param":null,"code":"404"}}`))
		return
	}
	if mode == Mode401DeleteTeam && r.Method == http.MethodPost && r.URL.Path == pathTeamDelete {
		// Per REL-06: 401 on /team/delete triggers the operator's
		// anti-storm finalizer-removal path. Body matches the byte-perfect
		// LiteLLM 1.83.10 shape so the typed parser at internal/litellm/
		// errors.go produces a *Auth401Error. The mock still records the
		// team_id in deleteTeamCalls so the test can assert "operator
		// DID make the call (and got 401)" alongside the anti-storm path.
		var reqBody struct {
			TeamIDs []string `json:"team_ids"`
		}
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		m.mu.Lock()
		m.deleteTeamCalls = append(m.deleteTeamCalls, reqBody.TeamIDs...)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(LiteLLMAuth401Body))
		return
	}

	if m.writeDuplicateToolsetConflict(w, r, mode) {
		return
	}
	if m.writeAbsentToolsetDelete500(w, r, mode) {
		return
	}
	if m.writeAccessGroupResponse(w, r, mode) {
		return
	}

	switch mode {
	case Mode401:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(LiteLLMAuth401Body))
		return
	case ModeTransient5xx:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable","type":"upstream","param":null,"code":"503"}}`))
		return
	case Mode422:
		// Return 422 for POST /model/new only; other paths are served normally.
		if r.Method == http.MethodPost && r.URL.Path == "/model/new" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"message":"Unprocessable Entity","type":"invalid_request_error","param":null,"code":"422"}}`))
			return
		}
		// Other paths (GET /models, etc.) are served as happy.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.statefulBody(r))
		return
	case ModeDelete422:
		isDelete := (r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/delete")) ||
			r.Method == http.MethodDelete
		if isDelete {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"message":"Unprocessable Entity","type":"invalid_request_error","param":null,"code":"422"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.statefulBody(r))
		return
	case ModeSlow:
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			return
		}
		fallthrough
	default:
		// Happy mode — emit a minimal valid body per route, tracking model state.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.statefulBody(r))
	}
}

// statefulBody returns a response body for the given request, tracking
// model CRUD state so GET /model/info returns realistic model_info.id
// values after POST /model/new. This enables the Model reconciler tests
// to assert that status.lastRendered.modelID is populated and
// persisted correctly across reconciles (D-04).
//
//nolint:gocyclo // Single-router for the full mock CRUD surface (POST/PUT/DELETE/GET across model/team/agent/mcp paths); splitting per-route would multiply boilerplate and obscure the spec mapping.
func (m *MockServer) statefulBody(r *http.Request) []byte {
	method := r.Method
	path := r.URL.Path

	// POST /key/health — connection probe (LiteLLM's auth-gated
	// key+logging health endpoint, swapped in from GET /models). Empty
	// POST body authenticates via Authorization header alone; the
	// response carries the master-key liveness AND the configured
	// logging-callback health, which the connection reconciler surfaces
	// as the secondary LoggingHealthy condition.
	if method == http.MethodPost && path == "/key/health" {
		if m.loggingCallbacksNull.Load() {
			// UAT LOW-01: mirror real LiteLLM proxies that return
			// logging_callbacks: null when no callbacks are configured.
			return []byte(`{"key":"healthy","logging_callbacks":null}`)
		}
		return []byte(`{"key":"healthy","logging_callbacks":{"callbacks":[],"status":"healthy","details":"No logger exceptions triggered, system is healthy."}}`)
	}

	// GET /models — legacy probe path. Kept around so any direct test
	// callers (and the connection_proxy_test invariant) still get a
	// 200 response; not used by the operator's probe anymore.
	if method == http.MethodGet && path == "/models" {
		return []byte(`{"data":[]}`)
	}

	// POST /model/new — create model, track state, return UUID.
	if method == http.MethodPost && path == "/model/new" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		modelName, _ := reqBody["model_name"].(string)
		seq := m.modelSeq.Add(1)
		modelID := fmt.Sprintf("mock-model-id-%d", seq)

		// Mirror real LiteLLM: router pseudo-models (litellm_params.model
		// "auto_router/…") are accepted (200 + a fresh id) but live in the
		// in-memory router, NOT the DB model table — so GET /model/info never
		// lists them. Returning the id without storing reproduces the
		// created-but-not-listed trait the operator must tolerate (router
		// models skip the existence probe; non-routers trip the breaker).
		var routerCreate bool
		if lp, ok := reqBody["litellm_params"].(map[string]any); ok {
			if mdl, ok := lp["model"].(string); ok && strings.HasPrefix(mdl, "auto_router/") {
				routerCreate = true
			}
		}

		m.mu.Lock()
		if !routerCreate {
			m.models[modelName] = &modelEntry{ModelID: modelID, ModelName: modelName}
		}
		if mi, ok := reqBody["model_info"].(map[string]any); ok {
			m.lastModelInfo[modelName] = mi
		}
		m.perModelMutations[modelName]++
		m.mu.Unlock()

		return []byte(fmt.Sprintf(
			`{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}`,
			modelID, modelName, modelID,
		))
	}

	// POST /model/update — update existing model, keep same UUID.
	if method == http.MethodPost && path == "/model/update" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		modelName, _ := reqBody["model_name"].(string)
		m.mu.Lock()
		entry, exists := m.models[modelName]
		if mi, ok := reqBody["model_info"].(map[string]any); ok {
			m.lastModelInfo[modelName] = mi
		}
		m.perModelMutations[modelName]++
		m.mu.Unlock()
		if exists {
			return []byte(fmt.Sprintf(
				`{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}`,
				entry.ModelID, entry.ModelName, entry.ModelID,
			))
		}
		return []byte(`{"model_id":"","model_name":"","litellm_params":{},"model_info":{"id":""}}`)
	}

	// POST /model/delete — remove model from state.
	if method == http.MethodPost && path == "/model/delete" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		delID, _ := reqBody["id"].(string)
		m.mu.Lock()
		// Primary rows: delete the named entry whose id matches.
		deleted := false
		for name, e := range m.models {
			if e.ModelID == delID {
				delete(m.models, name)
				m.perModelMutations[name]++
				deleted = true
				break
			}
		}
		// Duplicate rows: a prune targets these extra ids. Remove exactly the
		// matching id from whichever name's dup list holds it (duplicate-row
		// convergence — Fix B). Counts as a mutation against that name.
		if !deleted {
			for name, dups := range m.dupModelRows {
				for i, dupID := range dups {
					if dupID == delID {
						m.dupModelRows[name] = append(dups[:i], dups[i+1:]...)
						if len(m.dupModelRows[name]) == 0 {
							delete(m.dupModelRows, name)
						}
						m.perModelMutations[name]++
						deleted = true
						break
					}
				}
				if deleted {
					break
				}
			}
		}
		m.mu.Unlock()
		return []byte(`{}`)
	}

	// GET /model/info — return list of known models. Supports both
	// ?litellm_model_id=<id> and ?model_name=<name> query params.
	if method == http.MethodGet && path == "/model/info" {
		q := r.URL.Query()
		modelID := q.Get("litellm_model_id")
		modelName := q.Get("model_name")

		m.mu.Lock()
		defer m.mu.Unlock()

		if modelID != "" {
			// Filter by ID.
			for _, e := range m.models {
				if e.ModelID == modelID {
					return []byte(fmt.Sprintf(
						`{"data":[{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}]}`,
						e.ModelID, e.ModelName, e.ModelID,
					))
				}
			}
			return []byte(`{"data":[]}`)
		}
		if modelName != "" {
			// Filter by name. Real LiteLLM returns EVERY deployment row for a
			// name (duplicates allowed); the mock mirrors that by emitting
			// every injected duplicate row FIRST, then the primary `models`
			// entry. Ordering the duplicates ahead of the tracked-id row
			// reproduces the live id-drift trigger: a first-row read sees a
			// dup id != the tracked id and (pre-fix) treats it as drift. The
			// set-membership fix is order-independent, so this ordering is
			// the adversarial case for the regression test.
			rows := make([]string, 0, 1+len(m.dupModelRows[modelName]))
			for _, dupID := range m.dupModelRows[modelName] {
				rows = append(rows, fmt.Sprintf(
					`{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}`,
					dupID, modelName, dupID,
				))
			}
			if e, ok := m.models[modelName]; ok {
				rows = append(rows, fmt.Sprintf(
					`{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}`,
					e.ModelID, e.ModelName, e.ModelID,
				))
			}
			return []byte(fmt.Sprintf(`{"data":[%s]}`, strings.Join(rows, ",")))
		}
		// No filter — return all models.
		var entries []string
		for _, e := range m.models {
			entries = append(entries, fmt.Sprintf(
				`{"model_id":%q,"model_name":%q,"litellm_params":{},"model_info":{"id":%q}}`,
				e.ModelID, e.ModelName, e.ModelID,
			))
		}
		return []byte(fmt.Sprintf(`{"data":[%s]}`, strings.Join(entries, ",")))
	}

	// ── Team routes ───────────────────────────────
	// The mock honors the LiteLLM 1.83.10 wire contract for the Team
	// reconciler's "clearing budget" semantics (spec §6.7 line 1194):
	// POST /team/new and POST /team/update accept JSON null for
	// max_budget and budget_duration; the operator builds these bodies
	// as map[string]any (avoiding a typed struct's `,omitempty` drop)
	// and the mock preserves the explicit nil values
	// in m.teams[id].LastBody for envtest assertions.
	//
	// POST /team/delete — body {"team_ids": [.]} per OpenAPI.
	if method == http.MethodPost && path == pathTeamDelete {
		var reqBody struct {
			TeamIDs []string `json:"team_ids"`
		}
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		m.mu.Lock()
		for _, id := range reqBody.TeamIDs {
			// Track every team_id passed to POST /team/delete — AC-T4
			// asserts this list is EMPTY across the Team/default
			// deletion cycle (the protected-default path never calls
			// the team-delete endpoint).
			m.deleteTeamCalls = append(m.deleteTeamCalls, id)
			if e, ok := m.teams[id]; ok {
				m.perTeamMutations[e.TeamAlias]++
				delete(m.teamByAlias, e.TeamAlias)
				delete(m.teams, id)
			}
		}
		m.mu.Unlock()
		return []byte(`{}`)
	}
	// POST /team/new — create, mint team_id, capture body for envtest
	// assertions on operator structural overlays + null preservation.
	if method == http.MethodPost && path == "/team/new" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		alias, _ := reqBody["team_alias"].(string)
		// max_budget may be present as float64 OR as nil (JSON null).
		// The mock preserves whichever the operator sent.
		var maxBudget *float64
		if v, ok := reqBody["max_budget"]; ok && v != nil {
			if f, ok2 := v.(float64); ok2 {
				maxBudget = &f
			}
		}
		budgetDuration, _ := reqBody["budget_duration"].(string)
		// Honor a caller-supplied team_id (mirrors LiteLLM 1.83.10, which
		// accepts a client-chosen team_id on POST /team/new — verified in
		// prod: team "platform" has team_id "team-platform"). Fall back to
		// a minted UUID only when the body omits team_id, preserving the
		// legacy server-assigned-id behavior for older tests.
		teamID, _ := reqBody["team_id"].(string)
		if teamID == "" {
			seq := m.teamSeq.Add(1)
			teamID = fmt.Sprintf("mock-team-id-%d", seq)
		}

		m.mu.Lock()
		m.teams[teamID] = &teamEntry{
			TeamID:         teamID,
			TeamAlias:      alias,
			MaxBudget:      maxBudget,
			BudgetDuration: budgetDuration,
			LastBody:       reqBody,
		}
		m.teamByAlias[alias] = teamID
		m.perTeamMutations[alias]++
		m.mu.Unlock()

		// Build a response mirroring LiteLLM 1.83.10's POST /team/new
		// response shape (team_id + team_alias + max_budget +
		// budget_duration). Use the typed-encoder so absent-budget
		// keys are written as JSON null verbatim.
		respBody := map[string]any{
			"team_id":    teamID,
			"team_alias": alias,
		}
		// Preserve the wire shape the operator sent for budget keys —
		// the operator's status round-trip uses these.
		if v, ok := reqBody["max_budget"]; ok {
			respBody["max_budget"] = v
		}
		if v, ok := reqBody["budget_duration"]; ok {
			respBody["budget_duration"] = v
		}
		out, _ := json.Marshal(respBody)
		return out
	}
	// POST /team/update — wholesale-replace per spec §5.1 Q10. Look up
	// by team_id (REQUIRED per OpenAPI), overwrite max_budget +
	// budget_duration + LastBody. Response is empty object — the
	// operator persists team_id from status.lastRendered already.
	if method == http.MethodPost && path == "/team/update" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		teamID, _ := reqBody["team_id"].(string)
		var maxBudget *float64
		if v, ok := reqBody["max_budget"]; ok && v != nil {
			if f, ok2 := v.(float64); ok2 {
				maxBudget = &f
			}
		}
		budgetDuration, _ := reqBody["budget_duration"].(string)

		m.mu.Lock()
		if e, ok := m.teams[teamID]; ok {
			// Wholesale-replace: also re-key teamByAlias if the alias
			// changed in the update body (defensive — operator sends
			// the same alias every time, but mirror the live LiteLLM
			// behavior of trusting the body).
			if newAlias, ok2 := reqBody["team_alias"].(string); ok2 && newAlias != "" && newAlias != e.TeamAlias {
				delete(m.teamByAlias, e.TeamAlias)
				e.TeamAlias = newAlias
				m.teamByAlias[newAlias] = teamID
			}
			e.MaxBudget = maxBudget
			e.BudgetDuration = budgetDuration
			e.LastBody = reqBody
			m.perTeamMutations[e.TeamAlias]++
		}
		m.mu.Unlock()

		return []byte(`{}`)
	}
	// GET /v2/team/list — return TeamListResponse envelope with teams
	// filtered by team_alias substring (LiteLLM's server-side partial
	// match; operator does exact-match client-side per spec §6.7).
	if method == http.MethodGet && strings.HasPrefix(path, "/v2/team/list") {
		// Honor the team_alias query parameter as a substring filter
		// (LiteLLM 1.83.10 partial-match behavior; operator does the
		// exact-match client-side per spec §6.7).
		alias := r.URL.Query().Get("team_alias")
		m.mu.Lock()
		var matched []*teamEntry
		for _, e := range m.teams {
			if alias == "" || strings.Contains(e.TeamAlias, alias) {
				matched = append(matched, e)
			}
		}
		// Build envelope entries with team_id + team_alias (operator
		// uses these two fields only; other entry fields are
		// forward-compat noise).
		entries := make([]map[string]any, 0, len(matched))
		for _, e := range matched {
			entries = append(entries, map[string]any{
				"team_id":    e.TeamID,
				"team_alias": e.TeamAlias,
			})
		}
		m.mu.Unlock()
		env := map[string]any{
			"teams":       entries,
			"total":       len(entries),
			"page":        1,
			"page_size":   100,
			"total_pages": 1,
		}
		out, _ := json.Marshal(env)
		return out
	}

	// ── MCP server routes ──────────────────────────
	// The mock honors the LiteLLM 1.83.10-stable semantics empirically
	// verified by Probe 10c: PUT /v1/mcp/server IS wholesale-replace
	// (Phase 5 D-01 ✓ branch). The mock's PUT route therefore performs a
	// full-record replace (same as the live LiteLLM container).
	//
	// DELETE /v1/mcp/server/<server_id> — extract id from path.
	if method == http.MethodDelete && strings.HasPrefix(path, "/v1/mcp/server/") {
		serverID := strings.TrimPrefix(path, "/v1/mcp/server/")
		m.mu.Lock()
		if e, ok := m.mcpServers[serverID]; ok {
			delete(m.mcpByName, e.ServerName)
			m.perMCPMutations[e.ServerName]++
		}
		delete(m.mcpServers, serverID)
		m.mu.Unlock()
		return []byte(`{}`)
	}
	// POST /v1/mcp/server — create, mint server_id, return body.
	if method == http.MethodPost && path == "/v1/mcp/server" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		serverName, _ := reqBody["server_name"].(string)
		url, _ := reqBody["url"].(string)
		transport, _ := reqBody["transport"].(string)
		// Honor a caller-supplied server_id — LiteLLM 1.83.10 pins it on
		// POST /v1/mcp/server (verified live, 201 echoes the value). Fall
		// back to a minted UUID when absent, preserving behavior for
		// hand-managed / legacy callers that send no server_id.
		serverID, _ := reqBody["server_id"].(string)
		if serverID == "" {
			seq := m.mcpSeq.Add(1)
			serverID = fmt.Sprintf("mock-mcp-server-id-%d", seq)
		}

		m.mu.Lock()
		m.mcpServers[serverID] = &mcpEntry{
			ServerID:   serverID,
			ServerName: serverName,
			URL:        url,
			Transport:  transport,
			LastBody:   reqBody,
		}
		m.mcpByName[serverName] = serverID
		m.perMCPMutations[serverName]++
		m.mu.Unlock()

		return []byte(fmt.Sprintf(
			`{"server_id":%q,"server_name":%q,"url":%q,"transport":%q}`,
			serverID, serverName, url, transport,
		))
	}
	// PUT /v1/mcp/server — wholesale-replace (Phase 5 D-01 ✓ branch /
	// Probe 10c). The mock keeps the same server_id but overwrites all
	// other fields with the PUT body's values.
	if method == http.MethodPut && path == "/v1/mcp/server" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		serverID, _ := reqBody["server_id"].(string)
		serverName, _ := reqBody["server_name"].(string)
		url, _ := reqBody["url"].(string)
		transport, _ := reqBody["transport"].(string)

		m.mu.Lock()
		if e, ok := m.mcpServers[serverID]; ok {
			e.ServerName = serverName
			e.URL = url
			e.Transport = transport
			e.LastBody = reqBody
			m.mcpByName[serverName] = serverID
			m.perMCPMutations[serverName]++
		}
		m.mu.Unlock()

		return []byte(fmt.Sprintf(
			`{"server_id":%q,"server_name":%q,"url":%q,"transport":%q}`,
			serverID, serverName, url, transport,
		))
	}
	// GET /v1/mcp/server — bare-array list of all MCP servers.
	if method == http.MethodGet && strings.HasPrefix(path, "/v1/mcp/server") {
		m.mu.Lock()
		entries := make([]string, 0, len(m.mcpServers))
		for _, e := range m.mcpServers {
			entries = append(entries, fmt.Sprintf(
				`{"server_id":%q,"server_name":%q,"url":%q,"transport":%q}`,
				e.ServerID, e.ServerName, e.URL, e.Transport,
			))
		}
		m.mu.Unlock()
		return []byte(fmt.Sprintf(`[%s]`, strings.Join(entries, ",")))
	}

	// ── A2A agent routes ───────────────────────────
	// The mock honors the LiteLLM 1.82.6 semantics empirically verified by
	// Phase 1 Probe 7: PUT /v1/agents/<id> IS wholesale-replace. The mock's
	// PUT route therefore performs a full-record replace (same as the live
	// LiteLLM container). Captures the most-recent POST/PUT body for
	// envtest projection-override assertions (see LastAgentBody helper).
	//
	// DELETE /v1/agents/<agent_id> — extract id from path.
	if method == http.MethodDelete && strings.HasPrefix(path, "/v1/agents/") {
		agentID := strings.TrimPrefix(path, "/v1/agents/")
		m.mu.Lock()
		if e, ok := m.agents[agentID]; ok {
			delete(m.agentByName, e.AgentName)
			m.perAgentMutations[e.AgentName]++
		}
		delete(m.agents, agentID)
		m.mu.Unlock()
		return []byte(`{}`)
	}
	// PUT /v1/agents/<agent_id> — wholesale-replace (Probe 7 ✓). The mock
	// keeps the same agent_id but overwrites all other fields with the
	// PUT body's values.
	if method == http.MethodPut && strings.HasPrefix(path, "/v1/agents/") {
		agentID := strings.TrimPrefix(path, "/v1/agents/")
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		agentName, _ := reqBody["agent_name"].(string)
		agentCardParams, _ := reqBody["agent_card_params"].(map[string]any)

		m.mu.Lock()
		if e, ok := m.agents[agentID]; ok {
			e.AgentName = agentName
			e.AgentCardParams = agentCardParams
			e.LastBody = reqBody
			m.agentByName[agentName] = agentID
			m.perAgentMutations[agentName]++
		}
		m.mu.Unlock()

		return []byte(fmt.Sprintf(
			`{"agent_id":%q,"agent_name":%q}`,
			agentID, agentName,
		))
	}
	// POST /v1/agents — create, mint agent_id, return body.
	if method == http.MethodPost && path == "/v1/agents" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		agentName, _ := reqBody["agent_name"].(string)
		agentCardParams, _ := reqBody["agent_card_params"].(map[string]any)
		seq := m.agentSeq.Add(1)
		agentID := fmt.Sprintf("mock-agent-id-%d", seq)

		m.mu.Lock()
		m.agents[agentID] = &agentEntry{
			AgentID:         agentID,
			AgentName:       agentName,
			AgentCardParams: agentCardParams,
			LastBody:        reqBody,
		}
		m.agentByName[agentName] = agentID
		m.perAgentMutations[agentName]++
		m.mu.Unlock()

		return []byte(fmt.Sprintf(
			`{"agent_id":%q,"agent_name":%q}`,
			agentID, agentName,
		))
	}
	// GET /v1/agents — bare-array list of all A2A agents (honors
	// ?health_check=false by ignoring the query param).
	if method == http.MethodGet && strings.HasPrefix(path, "/v1/agents") {
		m.mu.Lock()
		entries := make([]string, 0, len(m.agents))
		for _, e := range m.agents {
			entries = append(entries, fmt.Sprintf(
				`{"agent_id":%q,"agent_name":%q}`,
				e.AgentID, e.AgentName,
			))
		}
		m.mu.Unlock()
		return []byte(fmt.Sprintf(`[%s]`, strings.Join(entries, ",")))
	}

	// ── MCP toolset routes ─────────────────────────
	// LiteLLM 1.93.0 semantics, verified live:
	//   - POST mints toolset_id; a caller-supplied one is IGNORED.
	//   - PUT carries toolset_id in the BODY (not the path) and is a
	//     wholesale replace.
	//   - GET returns a BARE ARRAY.
	// The duplicate-name 409 is handled earlier in handle() because this
	// function cannot set a status code.
	//
	// DELETE /v1/mcp/toolset/<toolset_id> — id from the path.
	if method == http.MethodDelete && strings.HasPrefix(path, pathMCPToolset+"/") {
		toolsetID := strings.TrimPrefix(path, pathMCPToolset+"/")
		m.mu.Lock()
		if e, ok := m.toolsets[toolsetID]; ok {
			delete(m.toolsetByName, e.ToolsetName)
			m.perToolsetMutations[e.ToolsetName]++
		}
		delete(m.toolsets, toolsetID)
		m.mu.Unlock()
		return []byte(`{}`)
	}
	// PUT /v1/mcp/toolset — wholesale-replace; toolset_id is read from the
	// BODY, which is what makes this endpoint diverge from every other
	// LiteLLM update the operator issues.
	if method == http.MethodPut && path == pathMCPToolset {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		toolsetID, _ := reqBody["toolset_id"].(string)
		name, _ := reqBody["toolset_name"].(string)
		desc, _ := reqBody["description"].(string)
		tools, _ := reqBody["tools"].([]any)

		m.mu.Lock()
		if e, ok := m.toolsets[toolsetID]; ok {
			delete(m.toolsetByName, e.ToolsetName)
			e.ToolsetName = name
			e.Description = desc
			e.Tools = tools
			e.LastBody = reqBody
			m.toolsetByName[name] = toolsetID
			m.perToolsetMutations[name]++
		}
		m.mu.Unlock()

		return []byte(fmt.Sprintf(`{"toolset_id":%q,"toolset_name":%q}`, toolsetID, name))
	}
	// POST /v1/mcp/toolset — create, MINT toolset_id (a supplied one is
	// deliberately ignored, mirroring real 1.93.0).
	if method == http.MethodPost && path == pathMCPToolset {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		name, _ := reqBody["toolset_name"].(string)
		desc, _ := reqBody["description"].(string)
		tools, _ := reqBody["tools"].([]any)
		seq := m.toolsetSeq.Add(1)
		toolsetID := fmt.Sprintf("mock-toolset-id-%d", seq)

		m.mu.Lock()
		m.toolsets[toolsetID] = &toolsetEntry{
			ToolsetID:   toolsetID,
			ToolsetName: name,
			Description: desc,
			Tools:       tools,
			LastBody:    reqBody,
		}
		m.toolsetByName[name] = toolsetID
		m.perToolsetMutations[name]++
		m.mu.Unlock()

		return []byte(fmt.Sprintf(`{"toolset_id":%q,"toolset_name":%q}`, toolsetID, name))
	}
	// GET /v1/mcp/toolset — bare-array list of all toolsets.
	if method == http.MethodGet && strings.HasPrefix(path, pathMCPToolset) {
		m.mu.Lock()
		entries := make([]string, 0, len(m.toolsets))
		for _, e := range m.toolsets {
			toolsJSON, err := json.Marshal(e.Tools)
			if err != nil || e.Tools == nil {
				toolsJSON = []byte(`[]`)
			}
			entries = append(entries, fmt.Sprintf(
				`{"toolset_id":%q,"toolset_name":%q,"tools":%s}`,
				e.ToolsetID, e.ToolsetName, toolsJSON,
			))
		}
		m.mu.Unlock()
		return []byte(fmt.Sprintf(`[%s]`, strings.Join(entries, ",")))
	}

	// ── /v1/access_group ────────────────────────────────────────────────
	// Semantics MEASURED against stock LiteLLM 1.93.0 on 2026-08-06:
	//   - PUT  → an OMITTED field KEEPS the stored value, a sent list
	//            REPLACES wholesale, and [] (or null) CLEARS.
	//   - GET  → a BARE array, not {"access_groups": [...]}.
	// POST (201) and DELETE-of-an-absent-id (404) are handled by the
	// write* helpers in handle(), which can set a status code.
	//
	// assigned_team_ids / assigned_key_ids are stored but NEVER written by
	// these routes: the operator does not manage that face (a team-side
	// write does not propagate here), so they must survive a PUT untouched.

	// PUT /v1/access_group/<id> — id from the path.
	if method == http.MethodPut && strings.HasPrefix(path, pathAccessGroup+"/") {
		id := strings.TrimPrefix(path, pathAccessGroup+"/")
		// Decode into a raw map first so "field absent" (KEEP) is
		// distinguishable from "field sent as []" (CLEAR). That distinction
		// IS this endpoint's contract and is what the operator's struct tags
		// depend on — an omitempty on a managed list would silently turn a
		// clear into a keep.
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)

		m.mu.Lock()
		e, ok := m.accessGroups[id]
		if !ok {
			m.mu.Unlock()
			return []byte(`{"detail":{"error":"Access group not found."}}`)
		}
		assign := func(key string, dst *[]string) {
			blob, present := raw[key]
			if !present {
				return // absent = KEEP
			}
			var v []string
			if err := json.Unmarshal(blob, &v); err != nil {
				return
			}
			*dst = nonNilStrings(v) // sent = REPLACE; [] and null both CLEAR
		}
		assign("access_model_names", &e.AccessModelNames)
		assign("access_mcp_server_ids", &e.AccessMCPServerIDs)
		assign("access_agent_ids", &e.AccessAgentIDs)
		if blob, present := raw["description"]; present {
			var d string
			if json.Unmarshal(blob, &d) == nil {
				e.Description = d
			}
		}
		if blob, present := raw["access_group_name"]; present {
			var name string
			if json.Unmarshal(blob, &name) == nil && name != "" && name != e.AccessGroupName {
				delete(m.accessGroupByName, e.AccessGroupName)
				e.AccessGroupName = name
				m.accessGroupByName[name] = id
			}
		}
		out, _ := json.Marshal(e)
		m.mu.Unlock()
		return out
	}

	// DELETE /v1/access_group/<id> — only reached when the group EXISTS
	// (writeAccessGroupDelete404 intercepts the absent case).
	if method == http.MethodDelete && strings.HasPrefix(path, pathAccessGroup+"/") {
		id := strings.TrimPrefix(path, pathAccessGroup+"/")
		m.mu.Lock()
		if e, ok := m.accessGroups[id]; ok {
			delete(m.accessGroupByName, e.AccessGroupName)
		}
		delete(m.accessGroups, id)
		m.mu.Unlock()
		return []byte(fmt.Sprintf(`{"access_group_id":%q,"deleted":true}`, id))
	}

	// GET /v1/access_group — BARE array of all groups.
	if method == http.MethodGet && strings.HasPrefix(path, pathAccessGroup) {
		m.mu.Lock()
		out := make([]*accessGroupEntry, 0, len(m.accessGroups))
		for _, e := range m.accessGroups {
			out = append(out, e)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].AccessGroupName < out[j].AccessGroupName
		})
		blob, err := json.Marshal(out)
		m.mu.Unlock()
		if err != nil {
			return []byte(`[]`)
		}
		return blob
	}

	// ── Guardrail routes ───────────────────────────
	// LiteLLM 2026-05 surface (BerriAI/litellm proxy/guardrails/
	// guardrail_endpoints.py):
	//
	//   POST  /guardrails                 body {"guardrail": Guardrail}
	//   PUT   /guardrails/{guardrail_id}  body {"guardrail": Guardrail}
	//   DELETE /guardrails/{guardrail_id}
	//   GET   /v2/guardrails/list         envelope ListGuardrailsResponse
	//
	// The mock honors PUT-as-wholesale-replace (same pattern as MCP/agent).
	// LB pool support: multiple entries may share guardrail_name; the mock
	// keys by guardrail_id and tracks a name→IDs slice for sibling counts.
	if method == http.MethodPost && path == "/guardrails" {
		return m.handleGuardrailCreate(r)
	}
	if method == http.MethodPut && strings.HasPrefix(path, "/guardrails/") {
		guardrailID := strings.TrimPrefix(path, "/guardrails/")
		return m.handleGuardrailUpdate(r, guardrailID)
	}
	if method == http.MethodDelete && strings.HasPrefix(path, "/guardrails/") {
		guardrailID := strings.TrimPrefix(path, "/guardrails/")
		return m.handleGuardrailDelete(guardrailID)
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/v2/guardrails/list") {
		return m.handleGuardrailList()
	}

	// LiteLLM router_settings round-trip — used by the ModelAlias
	// reconciler's read-merge-write against router_settings.model_group_alias.
	if method == http.MethodGet && path == "/get/config/callbacks" {
		m.mu.Lock()
		body, _ := json.Marshal(map[string]any{
			"status":          "success",
			"router_settings": m.routerSettings,
		})
		m.mu.Unlock()
		return body
	}
	if method == http.MethodPost && path == "/config/update" {
		var reqBody map[string]any
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &reqBody)
		}
		if rs, ok := reqBody["router_settings"].(map[string]any); ok {
			m.mu.Lock()
			m.routerSettings = rs
			m.mu.Unlock()
		}
		return []byte(`{"status":"success"}`)
	}

	// POST/PUT mutations — return an empty success body.
	return []byte(`{}`)
}

// ModelGroupAlias returns a snapshot of the current
// router_settings.model_group_alias map, for use by ModelAlias envtests.
// Returns an empty (non-nil) map if no aliases have been written.
func (m *MockServer) ModelGroupAlias() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	if m.routerSettings == nil {
		return out
	}
	raw, ok := m.routerSettings["model_group_alias"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if sv, ok := v.(string); ok {
			out[k] = sv
		}
	}
	return out
}
