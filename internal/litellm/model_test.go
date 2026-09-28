// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// testModelName is the model_name reused across the GetModelInfoByName /
// GetModelIDsByName tests. Extracted as a const so goconst stays quiet
// (the literal appears in many comparison/argument positions).
const testModelName = "my-model"

// capturedRequest records the wire-level shape of a request the mock
// server saw. Used by the path-string-verification tests to enforce
// Pitfall 2 (POST /model/update — partial-update path-templated verb forbidden).
type capturedRequest struct {
	Method string
	Path   string
	Body   []byte
}

// captureMock returns an http.HandlerFunc that records every request
// into the provided slice (mutex-protected) and responds with the given
// status + body. Caller passes a function that produces the response
// for the i-th request, enabling status-sequence per call.
func captureMock(t *testing.T, captured *[]capturedRequest, respond func(i int, w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*captured = append(*captured, capturedRequest{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Body:   body,
		})
		idx := len(*captured) - 1
		mu.Unlock()
		respond(idx, w)
	}
}

// TestUpdateModelUsesPostNotPatch — Pitfall 2 + FIX7 H-1 enforcement
// at the model.go layer. Asserts:
//   - the captured method is POST (NOT PATCH)
//   - the captured path is exactly /model/update (NOT /model/<id>/update)
//   - the request body does NOT carry a top-level "id" (LiteLLM 1.85.1
//     rejects this with a misleading 400 "Authentication Error,
//     model not found")
//   - the request body does NOT carry a top-level "model_id" (the
//     1.82.6-era field name)
//   - the request body places the model id at model_info.id per the
//     1.85.1 OpenAPI updateDeployment schema
//
// This is the single most important test in the package — it locks in
// the bbdsoftware/litellm-operator bug fix that motivated this rewrite
// AND the 1.85.1 body-shape fix per FIX7 H-1.
func TestUpdateModelUsesPostNotPatch(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model_id":"abc","model_name":"foo","litellm_params":{},"model_info":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &updateDeployment{
		ModelInfo:     ModelInfo{ID: "abc-123"},
		LiteLLMParams: LiteLLMParams{"timeout": 30},
	}
	if _, err := c.UpdateModel(context.Background(), req); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("captured: want 1 request, got %d", len(captured))
	}
	got := captured[0]
	if got.Method != http.MethodPost {
		t.Errorf("method: want POST, got %q", got.Method)
	}
	if got.Path != "/model/update" {
		t.Errorf("path: want exactly /model/update (NOT /model/<id>/update — Pitfall 2), got %q", got.Path)
	}

	// Body: per LiteLLM 1.85.1 (FIX7 H-1) the model id lives INSIDE
	// model_info, NOT at the top level. A top-level `id` is rejected
	// by LiteLLM's body parser with the misleading 400
	// "Authentication Error, model not found". `model_id` must also
	// be absent (the 1.82.6-era key name is not accepted).
	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if _, present := body["id"]; present {
		t.Errorf("body.id (top-level): must be absent on 1.85.1, got %v", body["id"])
	}
	if _, present := body["model_id"]; present {
		t.Errorf("body.model_id: must be absent on 1.85.1, got %v", body["model_id"])
	}
	mi, ok := body["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("body.model_info missing: %s", got.Body)
	}
	if id, _ := mi["id"].(string); id != "abc-123" {
		t.Errorf("body.model_info.id: want abc-123, got %v", mi["id"])
	}
}

// TestGetModelInfoLengthCheck — REL-05. Three malformed empty shapes
// must all return ErrNotFound (NOT panic, NOT generic error).
func TestGetModelInfoLengthCheck(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_data_array", `{"data":[]}`},
		{"null_data", `{"data":null}`},
		{"missing_data_key", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			got, err := c.GetModelInfo(context.Background(), "anything")
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err: want ErrNotFound, got %v", err)
			}
			if got != nil {
				t.Errorf("result: want nil on empty, got %+v", got)
			}
		})
	}
}

// TestGetModelInfoReturnsFirstEntry — happy path: well-formed Data → first entry.
func TestGetModelInfoReturnsFirstEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[{"model_id":"m1","model_name":"foo","litellm_params":{},"model_info":{"id":"m1"}}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetModelInfo(context.Background(), "m1")
	if err != nil {
		t.Fatalf("GetModelInfo: %v", err)
	}
	if got == nil || got.ModelID != "m1" {
		t.Errorf("result: want ModelID=m1, got %+v", got)
	}
}

// TestModelHelpers401Propagation — REL-06 propagation through every
// model helper that issues HTTP.
func TestModelHelpers401Propagation(t *testing.T) {
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

	_, err := c.CreateModel(context.Background(), &Deployment{ModelName: "x", LiteLLMParams: LiteLLMParams{}, ModelInfo: ModelInfo{ID: "x"}})
	check("CreateModel", err)

	_, err = c.UpdateModel(context.Background(), &updateDeployment{ModelInfo: ModelInfo{ID: "x"}})
	check("UpdateModel", err)

	err = c.DeleteModel(context.Background(), "x")
	check("DeleteModel", err)

	_, err = c.GetModelInfo(context.Background(), "x")
	check("GetModelInfo", err)
}

// TestCreateModelPath — POST /model/new path-string assertion.
func TestCreateModelPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model_id":"new"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _ = c.CreateModel(context.Background(), &Deployment{ModelName: "x", LiteLLMParams: LiteLLMParams{}, ModelInfo: ModelInfo{ID: "x"}})

	if len(captured) != 1 || captured[0].Method != http.MethodPost || captured[0].Path != "/model/new" {
		t.Errorf("CreateModel: want POST /model/new, got %+v", captured)
	}
	// Also assert the body is well-formed JSON with model_name+litellm_params+model_info.
	if !strings.Contains(string(captured[0].Body), `"model_name":"x"`) {
		t.Errorf("CreateModel body missing model_name=x: %s", captured[0].Body)
	}
}

// TestGetModelInfoByName_HappyPath — GetModelInfoByName with a
// matching entry in data[]. The helper must return the entry (not nil).
func TestGetModelInfoByName_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the query param is correct.
		if r.URL.Query().Get("model_name") != testModelName {
			t.Errorf("expected model_name=my-model, got %q", r.URL.Query().Get("model_name"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[
			{"model_id":"uuid-1","model_name":"my-model","litellm_params":{},"model_info":{"id":"uuid-1"}},
			{"model_id":"uuid-2","model_name":"other-model","litellm_params":{},"model_info":{"id":"uuid-2"}}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetModelInfoByName(context.Background(), testModelName)
	if err != nil {
		t.Fatalf("GetModelInfoByName: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("GetModelInfoByName: want non-nil result, got nil")
	}
	if got.ModelName != testModelName {
		t.Errorf("ModelName: want my-model, got %q", got.ModelName)
	}
	if got.ModelInfo.ID != "uuid-1" {
		t.Errorf("ModelInfo.ID: want uuid-1, got %q", got.ModelInfo.ID)
	}
}

// TestGetModelInfoByName_NotFound — GetModelInfoByName with empty data[]
// or 200 with no matching name must return (nil, nil) — NOT an error.
func TestGetModelInfoByName_NotFound(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_data", `{"data":[]}`},
		{"no_name_match", `{"data":[{"model_id":"other","model_name":"other-model","litellm_params":{},"model_info":{"id":"other"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			got, err := c.GetModelInfoByName(context.Background(), testModelName)
			if err != nil {
				t.Fatalf("GetModelInfoByName: unexpected error (want nil,nil for not-found): %v", err)
			}
			if got != nil {
				t.Errorf("GetModelInfoByName: want nil result on not-found, got %+v", got)
			}
		})
	}
}

// TestGetModelInfoByName_401 — GetModelInfoByName must return *Auth401Error on 401.
func TestGetModelInfoByName_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(litellmAuth401Body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetModelInfoByName(context.Background(), testModelName)
	if got != nil {
		t.Errorf("GetModelInfoByName: want nil result on 401, got %+v", got)
	}
	var a *Auth401Error
	if !errors.As(err, &a) {
		t.Errorf("GetModelInfoByName: want *Auth401Error on 401, got %T: %v", err, err)
	}
}

// TestGetModelInfoByName_5xx — GetModelInfoByName returns non-nil error on 5xx.
func TestGetModelInfoByName_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream","type":"upstream","param":null,"code":"503"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetModelInfoByName(context.Background(), testModelName)
	if err == nil {
		t.Fatal("GetModelInfoByName: want error on 5xx, got nil")
	}
	if got != nil {
		t.Errorf("GetModelInfoByName: want nil result on error, got %+v", got)
	}
	// Must NOT be an Auth401Error.
	var a *Auth401Error
	if errors.As(err, &a) {
		t.Errorf("GetModelInfoByName: 5xx should not be *Auth401Error")
	}
}

// TestGetModelInfoByName_404ReturnsNilNil asserts the contract documented
// in the function godoc: HTTP 404 from /model/info?model_name=<name>
// resolves to (nil, nil), NOT (nil, *RejectedError{Status:404}).
//
// Post-2026-05-26 review finding F4 (finalizer strand on 404). Pre-fix
// the helper returned the *RejectedError unchanged, leaving the Model
// controller's deletion-path fallback stuck in controller-runtime
// exponential backoff because it only special-cased Auth401Error.
func TestGetModelInfoByName_404ReturnsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"not_found","param":null,"code":"404"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.GetModelInfoByName(context.Background(), testModelName)
	if err != nil {
		t.Fatalf("GetModelInfoByName: want (nil, nil) on 404, got error: %v", err)
	}
	if got != nil {
		t.Errorf("GetModelInfoByName: want nil result on 404, got %+v", got)
	}
}

// TestGetModelIDsByName_ReturnsAllDuplicates — duplicate-row fix (Fix A).
//
// LiteLLM allows multiple deployment rows per model_name (POST /model/new is
// NOT idempotent). The safety-relist must reason about the WHOLE set of ids
// LiteLLM lists under a name — "is our tracked id still present?" — rather
// than the first row only (which drives the id-drift duplicate-amplifier
// churn). This asserts every exact-name match is collected, in response
// order, and that rows for OTHER names are skipped.
func TestGetModelIDsByName_ReturnsAllDuplicates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model_name") != testModelName {
			t.Errorf("expected model_name=my-model, got %q", r.URL.Query().Get("model_name"))
		}
		w.WriteHeader(200)
		// Three rows for "my-model" interleaved with an "other-model" row;
		// the function must return [a b c] (skip the other-model row).
		_, _ = w.Write([]byte(`{"data":[
			{"model_id":"a","model_name":"my-model","litellm_params":{},"model_info":{"id":"a"}},
			{"model_id":"b","model_name":"my-model","litellm_params":{},"model_info":{"id":"b"}},
			{"model_id":"other","model_name":"other-model","litellm_params":{},"model_info":{"id":"other"}},
			{"model_id":"c","model_name":"my-model","litellm_params":{},"model_info":{"id":"c"}}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ids, err := c.GetModelIDsByName(context.Background(), testModelName)
	if err != nil {
		t.Fatalf("GetModelIDsByName: unexpected error: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("GetModelIDsByName: want [a b c], got %v", ids)
	}
}

// TestGetModelIDsByName_NotFound — empty data[] / no name match / 404 must
// all resolve to (nil, nil) — NOT an error (mirrors GetModelInfoByName).
func TestGetModelIDsByName_NotFound(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"empty_data", 200, `{"data":[]}`},
		{"no_name_match", 200, `{"data":[{"model_id":"other","model_name":"other-model","litellm_params":{},"model_info":{"id":"other"}}]}`},
		{"http_404", 404, `{"error":{"message":"model not found","type":"not_found","param":null,"code":"404"}}`},
		{"empty_ids_skipped", 200, `{"data":[{"model_id":"","model_name":"my-model","litellm_params":{},"model_info":{"id":""}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			ids, err := c.GetModelIDsByName(context.Background(), testModelName)
			if err != nil {
				t.Fatalf("GetModelIDsByName: want (nil, nil) for %s, got error: %v", tc.name, err)
			}
			if ids != nil {
				t.Errorf("GetModelIDsByName: want nil ids for %s, got %v", tc.name, ids)
			}
		})
	}
}

// TestGetModelIDsByName_401 — typed *Auth401Error propagated on 401.
func TestGetModelIDsByName_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(litellmAuth401Body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ids, err := c.GetModelIDsByName(context.Background(), testModelName)
	if ids != nil {
		t.Errorf("GetModelIDsByName: want nil ids on 401, got %v", ids)
	}
	var a *Auth401Error
	if !errors.As(err, &a) {
		t.Errorf("GetModelIDsByName: want *Auth401Error on 401, got %T: %v", err, err)
	}
}

// TestDeleteModelPath — POST /model/delete with {"id":.} body.
func TestDeleteModelPath(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DeleteModel(context.Background(), "to-delete"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPost || captured[0].Path != "/model/delete" {
		t.Errorf("DeleteModel: want POST /model/delete, got %+v", captured)
	}
	if !strings.Contains(string(captured[0].Body), `"id":"to-delete"`) {
		t.Errorf("DeleteModel body missing id=to-delete: %s", captured[0].Body)
	}
}

// TestCreateModelBodyStampsCreatedBy — FIX4.txt H-1 regression guard.
// Asserts that when a caller stamps model_info.created_by + updated_by
// on the request, the values reach the wire body verbatim. The
// production code path in model_controller.go stamps identity.Operator()
// on every CREATE; this guards against a future regression where the
// model_info bag is dropped or zeroed before serialization.
func TestCreateModelBodyStampsCreatedBy(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model_id":"abc","model_info":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &Deployment{
		ModelName:     "test-model",
		LiteLLMParams: LiteLLMParams{},
		ModelInfo: ModelInfo{
			CreatedBy: "alitellm-operator/v0.2.1-test",
			UpdatedBy: "alitellm-operator/v0.2.1-test",
		},
	}
	if _, err := c.CreateModel(context.Background(), req); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured: want 1, got %d", len(captured))
	}
	var body map[string]any
	if err := json.Unmarshal(captured[0].Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	mi, ok := body["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info missing from /model/new body: %s", captured[0].Body)
	}
	if cb, _ := mi["created_by"].(string); cb == "" {
		t.Errorf("model_info.created_by empty in /model/new body: %s", captured[0].Body)
	}
	if ub, _ := mi["updated_by"].(string); ub == "" {
		t.Errorf("model_info.updated_by empty in /model/new body: %s", captured[0].Body)
	}
}

// TestUpdateModelBodyStampsUpdatedBy — FIX4.txt H-1 regression guard.
// CreatedBy is intentionally not asserted on UPDATE — LiteLLM 1.83.10
// keeps the original creator across updates, and the operator's
// model_controller.go UPDATE branch stamps UpdatedBy only.
func TestUpdateModelBodyStampsUpdatedBy(t *testing.T) {
	var captured []capturedRequest
	srv := httptest.NewServer(captureMock(t, &captured, func(i int, w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model_id":"abc","model_info":{"id":"abc"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := &updateDeployment{
		ModelName:     "test-model",
		LiteLLMParams: LiteLLMParams{},
		// FIX7 H-1: id lives inside model_info on 1.85.1.
		ModelInfo: ModelInfo{ID: "abc", UpdatedBy: "alitellm-operator/v0.2.1-test"},
	}
	if _, err := c.UpdateModel(context.Background(), req); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured: want 1, got %d", len(captured))
	}
	var body map[string]any
	if err := json.Unmarshal(captured[0].Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	mi, ok := body["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info missing from /model/update body: %s", captured[0].Body)
	}
	if ub, _ := mi["updated_by"].(string); ub == "" {
		t.Errorf("model_info.updated_by empty in /model/update body: %s", captured[0].Body)
	}
	if id, _ := mi["id"].(string); id != "abc" {
		t.Errorf("model_info.id: want abc, got %v", mi["id"])
	}
}

// TestUpdateDeploymentJSONShape_1_85_1 — FIX7 H-1 contract lock.
// Marshal a fully populated updateDeployment and assert the wire body
// (a) has NO top-level id key, (b) places id inside model_info, and
// (c) keeps the canonical 3-key top-level shape verbatim. If LiteLLM
// regresses the schema (or a refactor accidentally re-introduces a
// root-level ID field on the struct), this test fails first — well
// before a prod /model/update returns 400 "Authentication Error,
// model not found".
func TestUpdateDeploymentJSONShape_1_85_1(t *testing.T) {
	req := &updateDeployment{
		ModelName:     "anthropic.claude-opus-4-7",
		LiteLLMParams: LiteLLMParams{"model": "anthropic/claude-opus-4-7"},
		ModelInfo: ModelInfo{
			ID:        "02c8b202-aaaa-bbbb-cccc-deadbeef0000",
			UpdatedBy: "alitellm-operator/0.3.1",
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}

	if _, present := top["id"]; present {
		t.Errorf("top-level id MUST be absent on 1.85.1, got %v\nfull body: %s", top["id"], b)
	}
	if _, present := top["model_id"]; present {
		t.Errorf("top-level model_id MUST be absent on 1.85.1, got %v", top["model_id"])
	}

	wantTopKeys := map[string]struct{}{"model_name": {}, "litellm_params": {}, "model_info": {}}
	for k := range top {
		if _, ok := wantTopKeys[k]; !ok {
			t.Errorf("unexpected top-level key %q in /model/update body: %s", k, b)
		}
	}

	mi, ok := top["model_info"].(map[string]any)
	if !ok {
		t.Fatalf("model_info missing or wrong type: %s", b)
	}
	if id, _ := mi["id"].(string); id != "02c8b202-aaaa-bbbb-cccc-deadbeef0000" {
		t.Errorf("model_info.id: want UUID, got %v", mi["id"])
	}
	if ub, _ := mi["updated_by"].(string); ub != "alitellm-operator/0.3.1" {
		t.Errorf("model_info.updated_by: want alitellm-operator/0.3.1, got %v", mi["updated_by"])
	}
}

func TestDeleteModel_EmptyIDGuard(t *testing.T) {
	c := newTestClient(t, "http://unreachable.invalid")
	err := c.DeleteModel(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on empty model_id; no request should be issued")
	}
	if !strings.Contains(err.Error(), "empty model_id") {
		t.Errorf("error %q does not mention empty model_id", err.Error())
	}
}
