// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
)

// EnvDangerouslyLogBodies is the operator-pod env var that flips
// redaction off. When set to the literal string "true", the transport
// reads and re-substitutes both request and response bodies and emits
// them in the log line. This is a §9.1 escape hatch for live debugging;
// the default is "redaction on".
const EnvDangerouslyLogBodies = "LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES"

// redactingRoundTripper wraps an http.RoundTripper to enforce the §9.1
// hygiene contract: by default it logs ONLY {method, path, status,
// latency_ms} — never headers, never bodies, never auth credentials.
//
// When logBodies is true, the round-tripper reads both bodies into
// memory and substitutes io.NopCloser(bytes.NewReader(.)) so the
// caller still receives identical bytes. This path is intended for
// short-lived live debugging only.
type redactingRoundTripper struct {
	base      http.RoundTripper
	log       logr.Logger
	logBodies bool
}

// RoundTrip implements http.RoundTripper.
func (r *redactingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Capture request body BEFORE the round-trip if logBodies is on.
	// We must read+restore so r.base sees identical bytes on the wire.
	var reqBodyForLog []byte
	if r.logBodies && req.Body != nil {
		buf, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			reqBodyForLog = buf
			req.Body = io.NopCloser(bytes.NewReader(buf))
		}
	}

	resp, err := r.base.RoundTrip(req)
	latency := time.Since(start)

	if err != nil {
		// Network / transport error — log ONLY status="error" without
		// the err.Error string itself (it can contain the URL with
		// embedded credentials or master-key fragments leaked via DNS).
		r.log.Info("litellm request",
			"method", req.Method,
			"path", req.URL.Path,
			"status", "error",
			"latency_ms", latency.Milliseconds(),
		)
		return resp, err
	}

	fields := []any{
		"method", req.Method,
		"path", req.URL.Path,
		"status", resp.StatusCode,
		"latency_ms", latency.Milliseconds(),
	}

	if r.logBodies {
		// Diagnostic-only path. Read the response body, log it, then
		// substitute a fresh ReadCloser so the caller can read identical
		// bytes downstream.
		respBodyForLog, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr == nil {
			resp.Body = io.NopCloser(bytes.NewReader(respBodyForLog))
			fields = append(fields,
				"request_body", string(reqBodyForLog),
				"response_body", string(respBodyForLog),
			)
		}
	}

	r.log.Info("litellm request", fields...)
	return resp, nil
}

// newHTTPClient constructs the operator's *http.Client. Reads
// LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES once at construction time; the
// env var is NOT re-read per request (deliberate — flipping bodies
// mid-flight would be confusing).
func newHTTPClient(log logr.Logger) *http.Client {
	return &http.Client{
		Transport: &redactingRoundTripper{
			base:      http.DefaultTransport,
			log:       log,
			logBodies: os.Getenv(EnvDangerouslyLogBodies) == "true",
		},
		Timeout: 30 * time.Second,
	}
}

// drainAndClose is the REL-04 helper: every code path that holds an
// *http.Response MUST defer this immediately after http.Client.Do
// returns success. Draining the body before close enables HTTP keepalive
// reuse and prevents FD/goroutine leaks (bbdsoftware/litellm-operator's
// load-bearing bug).
//
// Both Copy and Close errors are intentionally ignored: drain is
// best-effort (a slow server should never block the caller), and a
// double-close on Close is a no-op on net/http's response body.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
