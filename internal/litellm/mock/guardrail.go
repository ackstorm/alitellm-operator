// SPDX-License-Identifier: Apache-2.0

// Guardrail handlers + state for the in-memory MockServer. Kept in a
// separate file from mock.go to keep that file's per-domain growth
// bounded; the shared MockServer struct uses methods defined here.
//
// State model mirrors the other resources (model/team/agent/mcp): an
// `entries` map keyed by the server-assigned ID, plus a per-name index
// for O(1) lookups. Unlike the other resources, guardrails permit
// multiple entries to share guardrail_name (load-balancing pool), so the
// name index points to a slice of IDs rather than a single ID.

package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// guardrailEntry is a stateful in-memory record of a guardrail created
// via POST /guardrails or registered via the team-submission flow.
// LastBody captures the most-recent POST/PUT body for envtest assertions
// (drift correction, pass-through preservation, secret resolution).
type guardrailEntry struct {
	GuardrailID                 string
	GuardrailName               string
	LitellmParams               map[string]any
	GuardrailInfo               map[string]any
	PolicyTemplate              string
	GuardrailDefinitionLocation string // "db" (operator-addressable) | "config" (read-only)
	LastBody                    map[string]any
}

// guardrailState lives on MockServer (lazy-initialised via
// ensureGuardrailState). Holding it in a sub-struct keeps the existing
// MockServer field list stable — no breaking change to other tests.
type guardrailState struct {
	mu               sync.Mutex
	entries          map[string]*guardrailEntry // keyed by GuardrailID
	byName           map[string][]string        // GuardrailName → []GuardrailID (LB pool)
	seq              int64
	perNameMutations map[string]int64
}

// ensureGuardrailState lazily initialises the per-MockServer guardrail
// state. Safe to call concurrently (double-checked initialisation).
func (m *MockServer) ensureGuardrailState() *guardrailState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guardrails == nil {
		m.guardrails = &guardrailState{
			entries:          make(map[string]*guardrailEntry),
			byName:           make(map[string][]string),
			perNameMutations: make(map[string]int64),
		}
	}
	return m.guardrails
}

// ResetGuardrails clears the in-memory guardrail store. Call between
// tests that need a clean slate for GET /v2/guardrails/list responses.
func (m *MockServer) ResetGuardrails() {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	gs.entries = make(map[string]*guardrailEntry)
	gs.byName = make(map[string][]string)
	gs.perNameMutations = make(map[string]int64)
	gs.seq = 0
	gs.mu.Unlock()
}

// HasGuardrail returns true if the mock contains at least one
// guardrail entry with the given name. Useful for envtest assertions
// after a CR-driven CREATE.
func (m *MockServer) HasGuardrail(name string) bool {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return len(gs.byName[name]) > 0
}

// GetGuardrailID returns the FIRST guardrail_id assigned to a name.
// Returns "" if no entry exists.
func (m *MockServer) GetGuardrailID(name string) string {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	ids := gs.byName[name]
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// GuardrailPoolSize returns the number of entries sharing a name (LB
// pool size). Returns 0 when no entries exist.
func (m *MockServer) GuardrailPoolSize(name string) int {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return len(gs.byName[name])
}

// MutationsByGuardrailName returns the count of POST/PUT/DELETE calls
// against guardrails sharing the given name. Symmetric with the other
// per-name mutation counters.
func (m *MockServer) MutationsByGuardrailName(name string) int64 {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return gs.perNameMutations[name]
}

// LastGuardrailBody returns the most-recent POST/PUT body received for
// any entry with the given name (the FIRST entry in the LB pool if
// there are multiple). Returns nil when no entry exists.
func (m *MockServer) LastGuardrailBody(name string) map[string]any {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	ids := gs.byName[name]
	if len(ids) == 0 {
		return nil
	}
	e, ok := gs.entries[ids[0]]
	if !ok || e.LastBody == nil {
		return nil
	}
	cp := make(map[string]any, len(e.LastBody))
	for k, v := range e.LastBody {
		cp[k] = v
	}
	return cp
}

// DeleteGuardrailOutOfBand removes a guardrail from the mock store
// WITHOUT going through the HTTP handler. Simulates an out-of-band
// DELETE in LiteLLM (e.g. a human admin removes the entry directly via
// the LiteLLM Admin UI or curl). Used by the safety-re-list envtest to
// verify the operator's existence probe detects the missing row and
// fires a CREATE on the next reconcile + bumps the
// alitellm_operator_drift_corrected_total{action=create_missing} counter.
func (m *MockServer) DeleteGuardrailOutOfBand(guardrailID string) {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	entry, ok := gs.entries[guardrailID]
	if !ok {
		return
	}
	removeIDFromSlice(gs.byName, entry.GuardrailName, guardrailID)
	delete(gs.entries, guardrailID)
}

// AddHandManagedConfigGuardrail inserts a guardrail into the mock as if
// it had been loaded from the LiteLLM config file (definition_location =
// "config"). Such entries are NOT addressable via POST/PUT/DELETE — the
// operator must surface ConflictsWithConfigGuardrail when it observes a
// name collision with a CONFIG row. Used by envtests that exercise the
// CONFIG-conflict reconcile branch.
func (m *MockServer) AddHandManagedConfigGuardrail(name string) string {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	gs.seq++
	id := fmt.Sprintf("mock-config-guardrail-id-%d", gs.seq)
	gs.entries[id] = &guardrailEntry{
		GuardrailID:                 id,
		GuardrailName:               name,
		GuardrailDefinitionLocation: "config",
		LitellmParams:               map[string]any{},
	}
	gs.byName[name] = append(gs.byName[name], id)
	return id
}

// extractGuardrailFromBody pulls the inner GuardrailBody out of the
// wrapped {"guardrail": {...}} request shape used by POST/PUT.
func extractGuardrailFromBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var wrapped map[string]any
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	gr, ok := wrapped["guardrail"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mock: POST/PUT /guardrails: body missing 'guardrail' wrapper")
	}
	return gr, nil
}

// handleGuardrailCreate handles POST /guardrails. Mints a guardrail_id,
// stores the body, and returns a Guardrail record with definition_location
// = "db". On a collision with an existing CONFIG row sharing the same
// name, the mock still permits the create (the upstream LiteLLM server
// also permits this — operator-side ConflictsWithConfigGuardrail handles
// the conflict before the POST fires).
func (m *MockServer) handleGuardrailCreate(r *http.Request) []byte {
	gr, err := extractGuardrailFromBody(r)
	if err != nil {
		return []byte(`{"error":{"message":"` + err.Error() + `","type":"invalid_request_error","code":"400"}}`)
	}
	name, _ := gr["guardrail_name"].(string)
	params, _ := gr["litellm_params"].(map[string]any)
	info, _ := gr["guardrail_info"].(map[string]any)
	policyTemplate, _ := gr["policy_template"].(string)

	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	gs.seq++
	id := fmt.Sprintf("mock-guardrail-id-%d", gs.seq)
	gs.entries[id] = &guardrailEntry{
		GuardrailID:                 id,
		GuardrailName:               name,
		LitellmParams:               params,
		GuardrailInfo:               info,
		PolicyTemplate:              policyTemplate,
		GuardrailDefinitionLocation: "db",
		LastBody:                    gr,
	}
	gs.byName[name] = append(gs.byName[name], id)
	gs.perNameMutations[name]++
	gs.mu.Unlock()

	resp := map[string]any{
		"guardrail_id":                  id,
		"guardrail_name":                name,
		"litellm_params":                params,
		"guardrail_definition_location": "db",
	}
	if info != nil {
		resp["guardrail_info"] = info
	}
	if policyTemplate != "" {
		resp["policy_template"] = policyTemplate
	}
	out, _ := json.Marshal(resp)
	return out
}

// handleGuardrailUpdate handles PUT /guardrails/{guardrail_id}.
// Wholesale-replace: every field except guardrail_id is overwritten by
// the body. Returns the updated record. Re-keys byName if the name
// changed.
func (m *MockServer) handleGuardrailUpdate(r *http.Request, guardrailID string) []byte {
	gr, err := extractGuardrailFromBody(r)
	if err != nil {
		return []byte(`{"error":{"message":"` + err.Error() + `","type":"invalid_request_error","code":"400"}}`)
	}
	name, _ := gr["guardrail_name"].(string)
	params, _ := gr["litellm_params"].(map[string]any)
	info, _ := gr["guardrail_info"].(map[string]any)
	policyTemplate, _ := gr["policy_template"].(string)

	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	entry, ok := gs.entries[guardrailID]
	if !ok {
		return []byte(`{"error":{"message":"guardrail not found","type":"not_found_error","code":"404"}}`)
	}
	if entry.GuardrailName != name && entry.GuardrailName != "" {
		// Re-key the byName index.
		removeIDFromSlice(gs.byName, entry.GuardrailName, guardrailID)
		gs.byName[name] = append(gs.byName[name], guardrailID)
	}
	entry.GuardrailName = name
	entry.LitellmParams = params
	entry.GuardrailInfo = info
	entry.PolicyTemplate = policyTemplate
	entry.LastBody = gr
	gs.perNameMutations[name]++

	resp := map[string]any{
		"guardrail_id":                  guardrailID,
		"guardrail_name":                name,
		"litellm_params":                params,
		"guardrail_definition_location": entry.GuardrailDefinitionLocation,
	}
	if info != nil {
		resp["guardrail_info"] = info
	}
	if policyTemplate != "" {
		resp["policy_template"] = policyTemplate
	}
	out, _ := json.Marshal(resp)
	return out
}

// handleGuardrailDelete handles DELETE /guardrails/{guardrail_id}.
// Returns 200 + {} on success, 404 + LiteLLM-shaped error envelope on
// unknown id. The operator's finalizer-time DELETE treats 404 as
// success (entry already gone).
func (m *MockServer) handleGuardrailDelete(guardrailID string) []byte {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	entry, ok := gs.entries[guardrailID]
	if !ok {
		return []byte(`{"error":{"message":"guardrail not found","type":"not_found_error","code":"404"}}`)
	}
	removeIDFromSlice(gs.byName, entry.GuardrailName, guardrailID)
	delete(gs.entries, guardrailID)
	gs.perNameMutations[entry.GuardrailName]++
	return []byte(`{}`)
}

// handleGuardrailList handles GET /v2/guardrails/list. Returns every
// known entry — both DB and CONFIG rows — in the
// ListGuardrailsResponse envelope. Sensitive litellm_params values
// (api_key etc.) are NOT masked here; production LiteLLM masks them, but
// the mock-side mask would interfere with body-shape assertions and the
// operator never relies on the response body for drift detection.
func (m *MockServer) handleGuardrailList() []byte {
	gs := m.ensureGuardrailState()
	gs.mu.Lock()
	defer gs.mu.Unlock()
	entries := make([]map[string]any, 0, len(gs.entries))
	for _, e := range gs.entries {
		row := map[string]any{
			"guardrail_id":                  e.GuardrailID,
			"guardrail_name":                e.GuardrailName,
			"guardrail_definition_location": e.GuardrailDefinitionLocation,
		}
		if e.LitellmParams != nil {
			row["litellm_params"] = e.LitellmParams
		}
		if e.GuardrailInfo != nil {
			row["guardrail_info"] = e.GuardrailInfo
		}
		if e.PolicyTemplate != "" {
			row["policy_template"] = e.PolicyTemplate
		}
		entries = append(entries, row)
	}
	out, _ := json.Marshal(map[string]any{"guardrails": entries})
	return out
}

// removeIDFromSlice removes a single ID from byName[name]. Empty after
// removal → delete the key entirely so HasGuardrail returns false.
func removeIDFromSlice(byName map[string][]string, name, id string) {
	ids := byName[name]
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		delete(byName, name)
	} else {
		byName[name] = out
	}
}
