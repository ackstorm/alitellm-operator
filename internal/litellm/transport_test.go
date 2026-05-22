// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// bufferSink is a tiny logr.LogSink that writes every Info / Error call
// (plus its key=value fields) into an in-memory buffer. Used by the
// redaction-canary tests to assert that NO body / header / credential
// material ever reaches a log line under default settings.
type bufferSink struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (b *bufferSink) Init(info logr.RuntimeInfo)             {}
func (b *bufferSink) Enabled(level int) bool                 { return true }
func (b *bufferSink) WithValues(kv ...any) logr.LogSink      { return b }
func (b *bufferSink) WithName(name string) logr.LogSink      { return b }
func (b *bufferSink) Info(level int, msg string, kv ...any)  { b.write(msg, kv) }
func (b *bufferSink) Error(err error, msg string, kv ...any) { b.write(msg+" err="+errStr(err), kv) }

func (b *bufferSink) write(msg string, kv []any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(b.buf, "%s", msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(b.buf, " %v=%v", kv[i], kv[i+1])
	}
	b.buf.WriteByte('\n')
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func (b *bufferSink) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// canaryMasterKey is the synthetic master-key string the redaction
// tests register. If this string EVER appears in captured log output
// under default settings, §9.1 is violated and the test fails.
const canaryMasterKey = "sk-canary-XYZ-12345-FAKE"

// nineResponseShapes returns an http.HandlerFunc that emits the 9
// response shapes the §9.1 canary exercises: 200, 200-empty, 400, 401,
// 404, 422, 500, 5xx-with-junk-body, and connection-reset (handled by
// the test via a separate hijacking handler).
func nineResponseShapes(t *testing.T) http.HandlerFunc {
	t.Helper()
	var n int32
	statuses := []int{200, 200, 400, 401, 404, 422, 500, 502}
	return func(w http.ResponseWriter, r *http.Request) {
		idx := int(atomicInc(&n)) - 1
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		s := statuses[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		switch s {
		case 200:
			if idx == 1 {
				// empty body
				return
			}
			_, _ = w.Write([]byte(`{"hello":"` + canaryMasterKey + `-echoed"}`))
		case 401:
			_, _ = w.Write([]byte(`{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = ` + canaryMasterKey + `","type":"token_not_found_in_db","param":"key","code":"401"}}`))
		default:
			_, _ = w.Write([]byte(`{"error":{"message":"junk","type":"x","param":null,"code":"x"}}`))
		}
	}
}

// atomicInc avoids the sync/atomic import by serializing via a mutex
// (the canary test fires requests sequentially, so contention is zero).
var atomicMu sync.Mutex

func atomicInc(p *int32) int32 {
	atomicMu.Lock()
	defer atomicMu.Unlock()
	*p++
	return *p
}

// TestNoCredentialLeak — §9.1 canary. With the DANGEROUSLY env var
// UNSET, no master-key string, request body, response body, or header
// value may appear in captured log output across 9 response shapes.
func TestNoCredentialLeak(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "") // explicit: redaction ON

	srv := httptest.NewServer(nineResponseShapes(t))
	defer srv.Close()

	cap := &bytes.Buffer{}
	logger := logr.New(&bufferSink{buf: cap})
	client := newHTTPClient(logger)

	// 8 normal-status requests…
	for i := 0; i < 8; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/test", strings.NewReader(`{"req":"`+canaryMasterKey+`"}`))
		req.Header.Set("Authorization", "Bearer "+canaryMasterKey)
		resp, err := client.Do(req)
		if err == nil {
			drainAndClose(resp.Body)
		}
	}

	// …plus one "connection reset" request via a hijacking handler.
	resetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("hijack not supported")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer resetSrv.Close()
	req, _ := http.NewRequest("GET", resetSrv.URL+"/reset", nil)
	req.Header.Set("Authorization", "Bearer "+canaryMasterKey)
	resp, err := client.Do(req)
	if err == nil && resp != nil {
		drainAndClose(resp.Body)
	}

	got := cap.String()
	if strings.Contains(got, canaryMasterKey) {
		t.Fatalf("§9.1 canary FAILED: master-key string leaked to logs.\nLogs:\n%s", got)
	}
	for _, k := range []string{"method=", "path=", "status="} {
		if !strings.Contains(got, k) {
			t.Errorf("expected log key %q in captured output (got: %q)", k, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Phase 5 — AC-S1 redaction canary extension to MCP + A2A.
//
// Mirrors TestNoCredentialLeak (above) but exercises the MCP and A2A
// route surfaces (POST/PUT/DELETE /v1/mcp/server and POST/PUT/DELETE
// /v1/agents). Each sub-test:
// 1. Spins up an httptest.Server returning a route-appropriate body
// (success or 401 / 4xx / 5xx) with the canary master key echoed
// into the response so a buggy log path would surface it.
// 2. Invokes the relevant client method on a buffered-log client.
// 3. Asserts the captured log buffer + any returned error string
// contain ZERO occurrences of canaryMasterKey AND ZERO occurrences
// of the "Bearer sk-" prefix (defense in depth).
// ─────────────────────────────────────────────────────────────────────────

// mcpRouteHandler returns an http.Handler that serves
// /v1/mcp/server{,/<id>} routes with a configurable status code. The
// success-path body echoes canaryMasterKey into a credential-shaped key
// (so any default LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=false reader
// would surface it). The 401 body shape mirrors the §7.7 / Probe 8
// literal (path-independent — same shape as model / agents endpoints).
func mcpRouteHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch status {
		case 200:
			// Echo the master key into a header-shaped field that a
			// debug logger might inadvertently include.
			_, _ = w.Write([]byte(`{"server_id":"mock-id-1","server_name":"redaction-canary","url":"https://example.com","transport":"http","extra_headers":{"Authorization":"Bearer ` + canaryMasterKey + `"}}`))
		case 401:
			_, _ = w.Write([]byte(`{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = ` + canaryMasterKey + `","type":"token_not_found_in_db","param":"key","code":"401"}}`))
		default:
			_, _ = w.Write([]byte(`{"error":{"message":"junk","type":"x","param":null,"code":"x"}}`))
		}
	}
}

// agentRouteHandler is the same as mcpRouteHandler but for the
// /v1/agents{,/<id>} route shape. The success body echoes the canary
// into agent_card_params.authentication.credentials_value (a plausible
// field name on the OpenAPI shape).
func agentRouteHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch status {
		case 200:
			_, _ = w.Write([]byte(`{"agent_id":"mock-agent-id-1","agent_name":"redaction-canary","agent_card_params":{"authentication":{"credentials_value":"` + canaryMasterKey + `"}}}`))
		case 401:
			_, _ = w.Write([]byte(`{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = ` + canaryMasterKey + `","type":"token_not_found_in_db","param":"key","code":"401"}}`))
		default:
			_, _ = w.Write([]byte(`{"error":{"message":"junk","type":"x","param":null,"code":"x"}}`))
		}
	}
}

// canaryAssertNoLeak fails the test if canaryMasterKey OR the
// "Bearer sk-" defense-in-depth substring appears in capturedLogs or
// any returned error's Error string.
func canaryAssertNoLeak(t *testing.T, name string, capturedLogs string, returnedErrs ...error) {
	t.Helper()
	if strings.Contains(capturedLogs, canaryMasterKey) {
		t.Errorf("[%s] §9.1 FAIL: canary master key %q leaked to logs", name, canaryMasterKey)
	}
	if strings.Contains(capturedLogs, "Bearer sk-") {
		t.Errorf("[%s] §9.1 FAIL: 'Bearer sk-' literal leaked to logs (defense-in-depth check)", name)
	}
	for i, err := range returnedErrs {
		if err == nil {
			continue
		}
		s := err.Error()
		if strings.Contains(s, canaryMasterKey) {
			t.Errorf("[%s] §9.1 FAIL: canary master key leaked to error[%d] string: %q", name, i, s)
		}
		if strings.Contains(s, "Bearer sk-") {
			t.Errorf("[%s] §9.1 FAIL: 'Bearer sk-' literal leaked to error[%d] string: %q", name, i, s)
		}
	}
}

// TestNoCredentialLeak_MCP — §9.1 / AC-S1 extension to MCP route surface.
// Exercises CreateMCPServer (POST /v1/mcp/server), UpdateMCPServer (PUT
// /v1/mcp/server), DeleteMCPServer (DELETE /v1/mcp/server/<id>), and
// ListMCPServers (GET /v1/mcp/server) across success / 401 / 4xx / 5xx
// status codes. After each invocation the captured log buffer and any
// returned error string are scanned for canaryMasterKey occurrences;
// the assertion is zero.
func TestNoCredentialLeak_MCP(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "") // explicit: redaction ON

	cases := []struct {
		name   string
		status int
	}{
		{"Success", 200},
		{"401", 401},
		{"4xx", 400},
		{"5xx", 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(mcpRouteHandler(tc.status))
			defer srv.Close()

			capBuf := &bytes.Buffer{}
			logger := logr.New(&bufferSink{buf: capBuf})
			client := NewClient(srv.URL, canaryMasterKey, logger)

			// Exercise each MCP route on the same status.
			_, err1 := client.CreateMCPServer(context.Background(), &MCPServerRequest{
				ServerName: "redaction-canary",
				URL:        "https://example.com",
				Transport:  "http",
			})
			_, err2 := client.UpdateMCPServer(context.Background(), &MCPServerUpdateRequest{
				ServerID:   "mock-id-1",
				ServerName: "redaction-canary",
				URL:        "https://example.com",
				Transport:  "http",
			})
			err3 := client.DeleteMCPServer(context.Background(), "mock-id-1")
			_, err4 := client.ListMCPServers(context.Background())

			canaryAssertNoLeak(t, "MCP/"+tc.name, capBuf.String(), err1, err2, err3, err4)
		})
	}

	// ConnectionReset path: hijacker handler closes the connection.
	t.Run("ConnectionReset", func(t *testing.T) {
		resetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("hijack not supported")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
		defer resetSrv.Close()
		capBuf := &bytes.Buffer{}
		logger := logr.New(&bufferSink{buf: capBuf})
		client := NewClient(resetSrv.URL, canaryMasterKey, logger)
		_, err := client.CreateMCPServer(context.Background(), &MCPServerRequest{
			ServerName: "redaction-canary",
			URL:        "https://example.com",
			Transport:  "http",
		})
		canaryAssertNoLeak(t, "MCP/ConnectionReset", capBuf.String(), err)
	})
}

// TestNoCredentialLeak_A2A — §9.1 / AC-S1 extension to A2A route surface.
// Exercises CreateAgent (POST /v1/agents), UpdateAgent (PUT
// /v1/agents/<id>), DeleteAgent (DELETE /v1/agents/<id>), and ListAgents
// (GET /v1/agents) across success / 401 / 4xx / 5xx status codes. Same
// shape as TestNoCredentialLeak_MCP above; differing routes only.
func TestNoCredentialLeak_A2A(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "") // explicit: redaction ON

	cases := []struct {
		name   string
		status int
	}{
		{"Success", 200},
		{"401", 401},
		{"4xx", 400},
		{"5xx", 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(agentRouteHandler(tc.status))
			defer srv.Close()

			capBuf := &bytes.Buffer{}
			logger := logr.New(&bufferSink{buf: capBuf})
			client := NewClient(srv.URL, canaryMasterKey, logger)

			_, err1 := client.CreateAgent(context.Background(), &AgentConfig{
				AgentName:       "redaction-canary",
				AgentCardParams: map[string]any{"name": "redaction-canary"},
			})
			_, err2 := client.UpdateAgent(context.Background(), "mock-agent-id-1", &AgentConfig{
				AgentName:       "redaction-canary",
				AgentCardParams: map[string]any{"name": "redaction-canary"},
			})
			err3 := client.DeleteAgent(context.Background(), "mock-agent-id-1")
			_, err4 := client.ListAgents(context.Background())

			canaryAssertNoLeak(t, "A2A/"+tc.name, capBuf.String(), err1, err2, err3, err4)
		})
	}

	t.Run("ConnectionReset", func(t *testing.T) {
		resetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("hijack not supported")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
		defer resetSrv.Close()
		capBuf := &bytes.Buffer{}
		logger := logr.New(&bufferSink{buf: capBuf})
		client := NewClient(resetSrv.URL, canaryMasterKey, logger)
		_, err := client.CreateAgent(context.Background(), &AgentConfig{
			AgentName:       "redaction-canary",
			AgentCardParams: map[string]any{"name": "redaction-canary"},
		})
		canaryAssertNoLeak(t, "A2A/ConnectionReset", capBuf.String(), err)
	})
}

// TestDangerouslyEnvFlipsRedaction — env var DOES flip redaction.
// With LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=true, the canary string
// SHOULD appear (because the response body contains it). Proves the
// opt-out works in both directions.
func TestDangerouslyEnvFlipsRedaction(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "true")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"echo":"` + canaryMasterKey + `"}`))
	}))
	defer srv.Close()

	cap := &bytes.Buffer{}
	logger := logr.New(&bufferSink{buf: cap})
	client := newHTTPClient(logger)

	req, _ := http.NewRequest("GET", srv.URL+"/probe", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	// Read body to assert byte-perfect restore semantics.
	body, _ := io.ReadAll(resp.Body)
	drainAndClose(resp.Body)
	if !strings.Contains(string(body), canaryMasterKey) {
		t.Fatalf("expected body restore to deliver canary; got: %s", body)
	}

	got := cap.String()
	if !strings.Contains(got, canaryMasterKey) {
		t.Fatalf("expected canary in logs with DANGEROUSLY=true; got: %s", got)
	}
}

// TestDrainAndClose — REL-04 reinforcement. Run 200 sequential
// requests against a server returning a 64 KiB body, defer drainAndClose
// every iteration, then assert the goroutine delta < 5. Failing this
// test means drain+close is leaking somewhere — equivalent to FD-stable
// over many requests. Stress kept modest so the test stays well under
// the per-package timeout on small CI runners with -race enabled.
func TestDrainAndClose(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "")

	payload := bytes.Repeat([]byte("A"), 64<<10) // 64 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	logger := logr.Discard()
	client := newHTTPClient(logger)

	// Settle goroutines from server startup.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		drainAndClose(resp.Body)
	}

	// Allow keepalive goroutines to settle / time out.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	delta := after - before
	if delta > 5 {
		t.Fatalf("goroutine leak: before=%d after=%d delta=%d (>5)", before, after, delta)
	}
}

// TestProcessLitellmError — feeds the literal 401 body shape recorded
// // extracts the code + a non-empty message.
func TestProcessLitellmError(t *testing.T) {
	literal := `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...-key","type":"token_not_found_in_db","param":"key","code":"401"}}`
	kind, msg, code := processLitellmError([]byte(literal))
	if code != "401" {
		t.Errorf("code: want 401, got %q", code)
	}
	if msg == "" {
		t.Errorf("message: want non-empty, got empty")
	}
	if kind == "" {
		t.Errorf("kind: want non-empty, got empty")
	}

	// Unparsable body returns kind="unparsed" + raw (capped) body.
	junk := []byte("not json at all")
	kind2, msg2, code2 := processLitellmError(junk)
	if kind2 != "unparsed" {
		t.Errorf("kind: want unparsed for junk, got %q", kind2)
	}
	if code2 != "" {
		t.Errorf("code: want empty for junk, got %q", code2)
	}
	if msg2 != "not json at all" {
		t.Errorf("message: want raw body for junk, got %q", msg2)
	}
}

// TestClassifyKindMatrix exercises classify across the status-code
// space to lock in the REL-06 fast-path mapping.
func TestClassifyKindMatrix(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{401, KindAuth401},
		{500, KindTransient},
		{502, KindTransient},
		{503, KindTransient},
		{400, KindPermanent},
		{404, KindPermanent},
		{422, KindPermanent},
		{200, KindPermanent}, // classify is only called on non-2xx, but the default arm holds.
	}
	for _, c := range cases {
		if got := classify(c.status); got != c.want {
			t.Errorf("classify(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}
