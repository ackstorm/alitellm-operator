// SPDX-License-Identifier: Apache-2.0

// Wire-shape tests for the LiteLLM Guardrail HTTP client. Asserts:
//   - method + path match the upstream surface
//     (POST /guardrails, PUT /guardrails/{id}, DELETE /guardrails/{id},
//     GET /v2/guardrails/list)
//   - request body is wrapped under the "guardrail" key (matches
//     CreateGuardrailRequest in BerriAI/litellm)
//   - JSON snapshot of the rendered body for a representative
//     content-filter Guardrail
//   - response decoder accepts both wrapped {"guardrail": ...} and flat
//     {...} response shapes (route-version robustness)
//
// No live LiteLLM dependency — httptest.Server captureMock pattern
// reused from model_test.go.

package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guardrailHappyResponse returns a minimal `Guardrail` record matching
// the upstream wire shape. Used by every test that does not exercise
// error branches.
func guardrailHappyResponse(name, id string) string {
	return `{
		"guardrail_id":` + jq(id) + `,
		"guardrail_name":` + jq(name) + `,
		"litellm_params":{"guardrail":"litellm_content_filter","mode":"pre_call"},
		"guardrail_definition_location":"db",
		"created_at":"2026-05-23T10:00:00Z",
		"updated_at":"2026-05-23T10:00:00Z"
	}`
}

func jq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestCreateGuardrail_WirePath asserts POST /guardrails with the body
// wrapped under {"guardrail": ...}.
func TestCreateGuardrail_WirePath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(guardrailHappyResponse("credential-filter", "gr-001")))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &GuardrailBody{
		GuardrailName: "credential-filter",
		LitellmParams: LiteLLMGuardrailParams{
			"guardrail":  "litellm_content_filter",
			"mode":       "pre_call",
			"default_on": true,
			"patterns": []any{
				map[string]any{
					"pattern_type": "prebuilt",
					"pattern_name": "aws_access_key",
					"action":       "BLOCK",
				},
			},
		},
	}

	out, err := c.CreateGuardrail(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateGuardrail: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d, want 1", len(captured))
	}
	if captured[0].Method != http.MethodPost {
		t.Errorf("method: got %q want POST", captured[0].Method)
	}
	if captured[0].Path != "/guardrails" {
		t.Errorf("path: got %q want /guardrails", captured[0].Path)
	}

	// Body MUST be wrapped under "guardrail".
	var body map[string]any
	if err := json.Unmarshal(captured[0].Body, &body); err != nil {
		t.Fatalf("body unmarshal: %v\nraw: %s", err, captured[0].Body)
	}
	gr, ok := body["guardrail"].(map[string]any)
	if !ok {
		t.Fatalf("body missing 'guardrail' wrapper; got: %#v", body)
	}
	if gr["guardrail_name"] != "credential-filter" {
		t.Errorf("guardrail.guardrail_name: got %v", gr["guardrail_name"])
	}
	params, ok := gr["litellm_params"].(map[string]any)
	if !ok {
		t.Fatalf("body missing guardrail.litellm_params; got: %#v", gr)
	}
	if params["guardrail"] != "litellm_content_filter" {
		t.Errorf("litellm_params.guardrail: got %v", params["guardrail"])
	}
	if params["mode"] != "pre_call" {
		t.Errorf("litellm_params.mode: got %v", params["mode"])
	}
	if params["default_on"] != true {
		t.Errorf("litellm_params.default_on: got %v", params["default_on"])
	}

	// Nested patterns array must survive verbatim.
	patterns, ok := params["patterns"].([]any)
	if !ok || len(patterns) != 1 {
		t.Fatalf("litellm_params.patterns: got %#v", params["patterns"])
	}
	p0 := patterns[0].(map[string]any)
	if p0["pattern_name"] != "aws_access_key" || p0["action"] != "BLOCK" {
		t.Errorf("patterns[0]: got %#v", p0)
	}

	// Response decoding.
	if out.GuardrailID != "gr-001" {
		t.Errorf("response GuardrailID: got %q", out.GuardrailID)
	}
	if out.GuardrailDefinitionLocation != GuardrailDefinitionLocationDB {
		t.Errorf("response DefinitionLocation: got %q want db", out.GuardrailDefinitionLocation)
	}
}

// TestUpdateGuardrail_WirePath asserts PUT /guardrails/{id} with the
// body wrapped under {"guardrail": ...} and the id segment URL-encoded
// (UUIDs contain only ASCII alphanumerics + hyphen, but exercise the
// path regardless).
func TestUpdateGuardrail_WirePath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(guardrailHappyResponse("credential-filter", "gr-001")))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &GuardrailBody{
		GuardrailID:   "gr-001",
		GuardrailName: "credential-filter",
		LitellmParams: LiteLLMGuardrailParams{
			"guardrail": "litellm_content_filter",
			"mode":      []any{"pre_call", "post_call"},
		},
	}

	if _, err := c.UpdateGuardrail(context.Background(), "gr-001", req); err != nil {
		t.Fatalf("UpdateGuardrail: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d, want 1", len(captured))
	}
	if captured[0].Method != http.MethodPut {
		t.Errorf("method: got %q want PUT", captured[0].Method)
	}
	if captured[0].Path != "/guardrails/gr-001" {
		t.Errorf("path: got %q want /guardrails/gr-001", captured[0].Path)
	}

	// Mode as []string must serialize as a JSON array.
	if !strings.Contains(string(captured[0].Body), `"mode":["pre_call","post_call"]`) {
		t.Errorf("mode array not serialized; body: %s", captured[0].Body)
	}
}

// TestUpdateGuardrail_EmptyID rejects an empty guardrail_id locally —
// constructing the URL with an empty segment would silently POST to the
// list endpoint instead.
func TestUpdateGuardrail_EmptyID(t *testing.T) {
	c := newTestClient(t, "http://unreachable.invalid")
	_, err := c.UpdateGuardrail(context.Background(), "", &GuardrailBody{GuardrailName: "x"})
	if err == nil {
		t.Fatal("expected error on empty guardrail_id; got nil")
	}
	if !strings.Contains(err.Error(), "empty guardrail_id") {
		t.Errorf("error message: %q does not mention empty guardrail_id", err.Error())
	}
}

// TestDeleteGuardrail_WirePath asserts DELETE /guardrails/{id}.
func TestDeleteGuardrail_WirePath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteGuardrail(context.Background(), "gr-001"); err != nil {
		t.Fatalf("DeleteGuardrail: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d, want 1", len(captured))
	}
	if captured[0].Method != http.MethodDelete {
		t.Errorf("method: got %q want DELETE", captured[0].Method)
	}
	if captured[0].Path != "/guardrails/gr-001" {
		t.Errorf("path: got %q want /guardrails/gr-001", captured[0].Path)
	}
	if len(captured[0].Body) != 0 {
		t.Errorf("expected empty body on DELETE; got %q", captured[0].Body)
	}
}

// TestDeleteGuardrail_EmptyID — symmetry with the update path.
func TestDeleteGuardrail_EmptyID(t *testing.T) {
	c := newTestClient(t, "http://unreachable.invalid")
	err := c.DeleteGuardrail(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on empty guardrail_id; got nil")
	}
	if !strings.Contains(err.Error(), "empty guardrail_id") {
		t.Errorf("error message: %q does not mention empty guardrail_id", err.Error())
	}
}

// TestListGuardrails_WirePath asserts GET /v2/guardrails/list (NOT the
// legacy /guardrails/list).
func TestListGuardrails_WirePath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"guardrails":[
				{"guardrail_id":"gr-001","guardrail_name":"credential-filter","guardrail_definition_location":"db","litellm_params":{"guardrail":"litellm_content_filter","mode":"pre_call"}},
				{"guardrail_id":"gr-002","guardrail_name":"presidio-redact","guardrail_definition_location":"config","litellm_params":{"guardrail":"presidio","mode":"pre_call"}}
			]
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	out, err := c.ListGuardrails(context.Background())
	if err != nil {
		t.Fatalf("ListGuardrails: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d, want 1", len(captured))
	}
	if captured[0].Method != http.MethodGet {
		t.Errorf("method: got %q want GET", captured[0].Method)
	}
	if captured[0].Path != "/v2/guardrails/list" {
		t.Errorf("path: got %q want /v2/guardrails/list", captured[0].Path)
	}

	if len(out.Guardrails) != 2 {
		t.Fatalf("Guardrails len: got %d want 2", len(out.Guardrails))
	}
	if out.Guardrails[0].GuardrailDefinitionLocation != GuardrailDefinitionLocationDB {
		t.Errorf("Guardrails[0] location: got %q", out.Guardrails[0].GuardrailDefinitionLocation)
	}
	if out.Guardrails[1].GuardrailDefinitionLocation != GuardrailDefinitionLocationConfig {
		t.Errorf("Guardrails[1] location: got %q", out.Guardrails[1].GuardrailDefinitionLocation)
	}
}

// TestGetGuardrailByName returns the matching entry or (nil, nil).
func TestGetGuardrailByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"guardrails":[
				{"guardrail_id":"gr-a","guardrail_name":"alpha","guardrail_definition_location":"db"},
				{"guardrail_id":"gr-b","guardrail_name":"beta","guardrail_definition_location":"db"}
			]
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetGuardrailByName(context.Background(), "beta")
	if err != nil {
		t.Fatalf("GetGuardrailByName: %v", err)
	}
	if got == nil || got.GuardrailID != "gr-b" {
		t.Errorf("got %#v want gr-b", got)
	}

	miss, err := c.GetGuardrailByName(context.Background(), "gamma")
	if err != nil {
		t.Fatalf("GetGuardrailByName(miss): %v", err)
	}
	if miss != nil {
		t.Errorf("expected nil for missing name; got %#v", miss)
	}
}

// TestCreateGuardrail_JSONBodySnapshot locks the exact wire shape for a
// representative content-filter Guardrail. Any future schema drift on the
// body will trip this snapshot before it ships.
//
// The snapshot is intentionally pinned with sorted keys + no whitespace —
// the JSON encoder emits sorted map keys for nested maps in Go 1.12+ so
// this is stable.
func TestCreateGuardrail_JSONBodySnapshot(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(guardrailHappyResponse("credential-filter", "gr-001")))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &GuardrailBody{
		GuardrailName: "credential-filter",
		LitellmParams: LiteLLMGuardrailParams{
			"guardrail":  "litellm_content_filter",
			"mode":       "pre_call",
			"default_on": true,
		},
		GuardrailInfo: map[string]any{
			"description": "credential filter",
		},
	}
	if _, err := c.CreateGuardrail(context.Background(), req); err != nil {
		t.Fatalf("CreateGuardrail: %v", err)
	}

	// Re-marshal to canonical form (sorted keys) so this test does not
	// rely on Go's map-iteration order. Compare against the canonical
	// reference body byte-for-byte.
	var body map[string]any
	if err := json.Unmarshal(captured[0].Body, &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	want := `{"guardrail":{"guardrail_info":{"description":"credential filter"},"guardrail_name":"credential-filter","litellm_params":{"default_on":true,"guardrail":"litellm_content_filter","mode":"pre_call"}}}`
	if string(got) != want {
		t.Errorf("body snapshot drift:\nwant: %s\ngot:  %s", want, string(got))
	}
}

// TestDecodeGuardrailResponse_AcceptsBothShapes exercises the
// flat-vs-wrapped response decoder.
func TestDecodeGuardrailResponse_AcceptsBothShapes(t *testing.T) {
	flat := []byte(`{"guardrail_id":"gr-001","guardrail_name":"x","guardrail_definition_location":"db"}`)
	wrapped := []byte(`{"guardrail":{"guardrail_id":"gr-002","guardrail_name":"y","guardrail_definition_location":"db"}}`)

	a, err := decodeGuardrailResponse(flat, "test flat")
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if a.GuardrailID != "gr-001" {
		t.Errorf("flat: got %q want gr-001", a.GuardrailID)
	}

	b, err := decodeGuardrailResponse(wrapped, "test wrapped")
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if b.GuardrailID != "gr-002" {
		t.Errorf("wrapped: got %q want gr-002", b.GuardrailID)
	}
}

// TestNormalizeGuardrailMode lowercases for hash stability vs LiteLLM's
// server-side field_validator(normalize_lowercase).
func TestNormalizeGuardrailMode(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"pre_call":     "pre_call",
		"PRE_CALL":     "pre_call",
		"Pre_Call":     "pre_call",
		"DURING_CALL":  "during_call",
		"logging_only": "logging_only",
	}
	for in, want := range cases {
		if got := NormalizeGuardrailMode(in); got != want {
			t.Errorf("NormalizeGuardrailMode(%q) = %q, want %q", in, got, want)
		}
	}
}
