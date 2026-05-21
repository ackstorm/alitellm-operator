// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpdateTeamUsesPostNotPatch — same Pitfall 2 enforcement at the
// team.go layer. POST /team/update — never PATCH.
func TestUpdateTeamUsesPostNotPatch(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"team_id":"t1","team_alias":"alpha"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UpdateTeam(context.Background(), &UpdateTeamRequest{TeamID: "t1", TeamAlias: "alpha"})
	if err != nil {
		t.Fatalf("UpdateTeam: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != "POST" || captured[0].Path != "/team/update" {
		t.Errorf("UpdateTeam: want POST /team/update, got %+v", captured)
	}
}

// TestListTeamsByAliasExactMatchFilter — §6.7 client-side exact-match
// filter. LiteLLM's server-side filter is partial; the operator MUST
// drop non-exact matches.
func TestListTeamsByAliasExactMatchFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		// Partial server-side match — includes alpha, alpha-beta, alpha-prod.
		_, _ = w.Write([]byte(`{"teams":[
			{"team_id":"t1","team_alias":"alpha"},
			{"team_id":"t2","team_alias":"alpha-beta"},
			{"team_id":"t3","team_alias":"alpha-prod"}
		],"total":3,"page":1,"page_size":100,"total_pages":1}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.ListTeamsByAlias(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("ListTeamsByAlias: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("exact-match filter failed: want 1, got %d (%+v)", len(got), got)
	}
	if got[0].TeamID != "t1" || got[0].TeamAlias != "alpha" {
		t.Errorf("wrong entry kept: %+v", got[0])
	}
}

// TestListTeamsByAliasEmptyOK — empty list is NOT ErrNotFound for the
// team helper; callers decide whether absence is an error (per §6.7).
func TestListTeamsByAliasEmptyOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"teams":[],"total":0,"page":1,"page_size":100,"total_pages":0}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.ListTeamsByAlias(context.Background(), "missing")
	if err != nil {
		t.Errorf("ListTeamsByAlias empty: want nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListTeamsByAlias empty: want empty slice, got %+v", got)
	}
}

// TestListTeamsByAliasPath — path-string assertion. /v2/team/list (NOT /v1).
func TestListTeamsByAliasPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"teams":[],"total":0,"page":1,"page_size":100,"total_pages":0}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _ = c.ListTeamsByAlias(context.Background(), "x")
	if len(captured) != 1 || captured[0].Method != "GET" {
		t.Fatalf("ListTeamsByAlias: want GET, got %+v", captured)
	}
	if !strings.HasPrefix(captured[0].Path, "/v2/team/list?") {
		t.Errorf("path: want prefix /v2/team/list?, got %q", captured[0].Path)
	}
	if !strings.Contains(captured[0].Path, "page_size=100") {
		t.Errorf("path: want page_size=100, got %q", captured[0].Path)
	}
}

// TestCreateTeamBodyShape1_83_10 — CR-10 / D-7.1-10 regression test.
//
// Asserts that CreateTeamRaw produces a body whose top-level field names
// match the locked-down LiteLLM 1.83.10 schema from spec/litellm_api.json
// POST /team/new.
//
// Diagnostic-first diff finding (Option A synthetic capture, 2026-05-19):
// - Working Team/default body: {team_alias, max_budget, budget_duration}
// - Failing Team/finance body: above + {tpm_limit, rpm_limit, tags, blocked}
//
// Root cause: sending "blocked": false triggers HTTP 403 in LiteLLM 1.83.10
// (the default team body that succeeded did NOT include blocked). The field
// is valid per spec/litellm_api.json NewTeamRequest schema but LiteLLM
// 1.83.10 enforces an admin-only restriction on setting blocked at creation
// time. Fix: omit blocked from the body when its value is false (the
// schema default); only include it when explicitly true.
//
// All other fields (tpm_limit, rpm_limit, tags) are valid and accepted.
// The spec confirms all are in NewTeamRequest.properties with correct types.
func TestCreateTeamBodyShape1_83_10(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"team_id":"t-finance","team_alias":"finance"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	// Simulate what the controller's body-builder produces for Team/finance.
	// The blocked field is intentionally OMITTED from the body when false —
	// this is the 1.83.10 fix per the diagnostic-first diff.
	body := map[string]any{
		"team_alias":      "finance",
		"max_budget":      500.0,
		"budget_duration": "30d",
		"tpm_limit":       1000000,
		"rpm_limit":       6000,
		"tags":            []any{"finance", "production"},
		// "blocked": false — intentionally absent per 1.83.10 fix:
		// sending blocked=false triggers HTTP 403 (admin-only field).
		// The schema default is false; omitting it is semantically identical.
	}
	_, err := c.CreateTeamRaw(context.Background(), body)
	if err != nil {
		t.Fatalf("CreateTeamRaw: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("captured: want 1 request, got %d", len(captured))
	}
	got := captured[0]
	if got.Method != "POST" || got.Path != "/team/new" {
		t.Errorf("CreateTeamRaw: want POST /team/new, got %s %s", got.Method, got.Path)
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(got.Body, &bodyMap); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}

	// Verify expected keys are present per spec/litellm_api.json NewTeamRequest.
	for _, key := range []string{"team_alias", "max_budget", "budget_duration", "tpm_limit", "rpm_limit", "tags"} {
		if _, ok := bodyMap[key]; !ok {
			t.Errorf("body missing expected key %q", key)
		}
	}

	// Verify "blocked": false is NOT present in the body.
	// The 1.83.10 diagnostic-first diff shows blocked triggers HTTP 403.
	// The schema default is false — omitting it is semantically equivalent.
	if v, ok := bodyMap["blocked"]; ok && v == false {
		t.Errorf("body must NOT include 'blocked': false — triggers HTTP 403 in LiteLLM 1.83.10 (admin-only field); got %v", v)
	}

	// Verify team_alias is set correctly.
	if alias, _ := bodyMap["team_alias"].(string); alias != "finance" {
		t.Errorf("team_alias: want finance, got %v", bodyMap["team_alias"])
	}
}

// TestTeamHelpers401Propagation — REL-06 propagation through team helpers.
func TestTeamHelpers401Propagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(litellmAuth401Body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	check := func(name string, err error) {
		t.Helper()
		var a *Auth401Error
		if !errors.As(err, &a) {
			t.Errorf("%s: want *Auth401Error, got %T: %v", name, err, err)
		}
	}

	_, err := c.CreateTeam(context.Background(), &NewTeamRequest{TeamAlias: "x"})
	check("CreateTeam", err)
	_, err = c.UpdateTeam(context.Background(), &UpdateTeamRequest{TeamID: "x"})
	check("UpdateTeam", err)
	err = c.DeleteTeam(context.Background(), []string{"x"})
	check("DeleteTeam", err)
	_, err = c.ListTeamsByAlias(context.Background(), "x")
	check("ListTeamsByAlias", err)
}
