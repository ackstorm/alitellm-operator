// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
)

// authHeaderKind selects which HTTP header carries the master key.
// empirically verified + Probe 8, both
// `Authorization: Bearer` AND `x-litellm-api-key` are honored on
// LiteLLM 1.83.10 across all 14 authenticated endpoints. The operator
// defaults to `Authorization: Bearer` (spec §6.1-aligned).
type authHeaderKind int

const (
	// AuthBearer sends `Authorization: Bearer <master-key>` — DEFAULT.
	AuthBearer authHeaderKind = iota
	// AuthXLiteLLMAPIKey sends `x-litellm-api-key: <master-key>`.
	// Switch by setting LITELLM_OPERATOR_AUTH_HEADER=x-litellm-api-key
	// at pod startup. Retained as an escape hatch in case LiteLLM 1.83.11+
	// changes behavior.
	AuthXLiteLLMAPIKey
)

// EnvAuthHeader is the operator-pod env var that overrides the default
// auth header at startup. Accepted values:
// - unset / empty / "authorization" / "bearer" → AuthBearer (default)
// - "x-litellm-api-key" → AuthXLiteLLMAPIKey
const EnvAuthHeader = "LITELLM_OPERATOR_AUTH_HEADER"

// defaultAuthHeader is AuthBearer per spec §6.1 (both Bearer and
// x-litellm-api-key forms work on LiteLLM 1.83.10; Bearer wins).
const defaultAuthHeader = AuthBearer

// Client is the operator's *http.Client wrapper for the LiteLLM REST
// API. All per-domain helpers (model.go, team.go, mcp.go, agents.go,
// keyinfo.go) call into Client.makeRequest.
type Client struct {
	endpoint   string
	masterKey  string
	httpClient *http.Client
	log        logr.Logger
	authHeader authHeaderKind
	// limiter caps the sustained rate of outbound HTTP requests to
	// LiteLLM (FIX2.txt MEDIUM-10, 2026-05-22). Nil → unlimited.
	limiter *rate.Limiter
}

// ClientOption configures optional Client behavior. Apply at construction
// time via NewClient(..., opts...). Stable order: WithRateLimit is
// idempotent when called multiple times; the LAST WithRateLimit wins.
type ClientOption func(*Client)

// WithRateLimit attaches a token-bucket limiter that caps sustained
// outbound HTTP requests at rps with allowed burst. rps <= 0 disables
// the limiter (no token wait at all). FIX2.txt M-10 (2026-05-22):
// prevents a boot-time thundering herd of ~30 writes/s from pushing a
// modestly-stressed LiteLLM proxy into 5xx territory and triggering the
// operator's own backoff loop.
func WithRateLimit(rps float64, burst int) ClientOption {
	return func(c *Client) {
		if rps > 0 {
			if burst < 1 {
				burst = 1
			}
			c.limiter = rate.NewLimiter(rate.Limit(rps), burst)
		} else {
			c.limiter = nil
		}
	}
}

// NewClient constructs a *Client wired to the redacting RoundTripper.
// endpoint is the base URL (e.g. "http://litellm.default.svc.cluster.local:4000");
// masterKey is the LiteLLM master key (sk-.). The logger is wrapped in
// the redacting RoundTripper — callers may safely log at any verbosity.
//
// Optional opts apply after the defaults. Currently:
// - WithRateLimit(rps, burst) — see its godoc.
func NewClient(endpoint, masterKey string, log logr.Logger, opts ...ClientOption) *Client {
	c := &Client{
		endpoint:   strings.TrimRight(endpoint, "/"),
		masterKey:  masterKey,
		httpClient: newHTTPClient(log),
		log:        log,
		authHeader: defaultAuthHeader,
	}
	switch strings.ToLower(os.Getenv(EnvAuthHeader)) {
	case "x-litellm-api-key":
		c.authHeader = AuthXLiteLLMAPIKey
	case "", "authorization", "bearer":
		c.authHeader = AuthBearer
	default:
		c.authHeader = AuthBearer
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// setAuth attaches the master key to the request according to the
// configured authHeader kind. Never logs the key (§9.1).
func (c *Client) setAuth(req *http.Request) {
	switch c.authHeader {
	case AuthXLiteLLMAPIKey:
		req.Header.Set("x-litellm-api-key", c.masterKey)
	default:
		req.Header.Set("Authorization", "Bearer "+c.masterKey)
	}
}

// makeRequest is the central request path. Every per-domain helper
// funnels through here so REL-04 (drain+close), REL-06 (typed 401), and
// §9.1 (no body in error strings) are enforced once.
//
// On success: returns the response body bytes and nil.
// On 401: returns nil, *Auth401Error (errors.As resolvable).
// On 404 + DELETE: returns the body and nil (treated as success per §7.7).
// On other 4xx: returns nil with a fmt.Errorf describing status + parsed
//
//	error code (NEVER raw body — §9.1).
//
// On 5xx / network: returns nil with a transient fmt.Errorf.
func (c *Client) makeRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("litellm: marshal %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("litellm: build %s %s: %w", method, path, err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// FIX2.txt M-10: token-wait BEFORE Do so the limiter governs the
	// outbound request rate. ctx cancellation during the wait surfaces
	// as a plain error (not a transport error) and short-circuits the
	// rest of the path.
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("litellm: %s %s: rate limiter wait: %w", method, path, err)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network error: resp is nil — nothing to drain. Wrap as
		// transient. Note we do NOT include err.Error unredacted; the
		// underlying URL/error wrapper from net/http may contain
		// credential fragments. Keep the wrapper's %w semantics so
		// errors.As / errors.Is still works without leaking text via
		// the Error string.
		return nil, fmt.Errorf("litellm: %s %s: transport error: %w", method, path, err)
	}
	defer drainAndClose(resp.Body) // REL-04: every code path.

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return respBody, nil
	case resp.StatusCode == http.StatusUnauthorized:
		// REL-06: typed error for the §7.7 fast-path.
		return nil, &Auth401Error{Path: path, Body: respBody}
	case resp.StatusCode == http.StatusNotFound && strings.EqualFold(method, http.MethodDelete):
		// §7.7: DELETE 404 is treated as success (idempotent delete).
		return respBody, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// FIX2.txt M-5 (2026-05-22): return a typed *RejectedError that
		// carries the envelope message so reconcilers can surface the
		// actionable detail in condition.Message instead of the generic
		// "litellm: 400 on <path>" string. Error() shape preserved for
		// existing prefix matchers (is4xxNon401Status).
		_, msg, code := processLitellmError(respBody)
		if code == "" {
			code = fmt.Sprintf("%d", resp.StatusCode)
		}
		return nil, &RejectedError{
			Method:  method,
			Path:    path,
			Status:  resp.StatusCode,
			Code:    code,
			Message: msg,
		}
	default:
		// 5xx and anything else — transient. processLitellmError used
		// only for the code field (NEVER the message — §9.1).
		_, _, code := processLitellmError(respBody)
		if code == "" {
			code = fmt.Sprintf("%d", resp.StatusCode)
		}
		return nil, fmt.Errorf("litellm: %d on %s %s (code=%s, transient)", resp.StatusCode, method, path, code)
	}
}

// IsAuth401 is a typed-error convenience for callers who don't want to
// import errors.As at every call site. Returns the *Auth401Error and
// true if err is one; otherwise nil, false.
func IsAuth401(err error) (*Auth401Error, bool) {
	var a *Auth401Error
	if errors.As(err, &a) {
		return a, true
	}
	return nil, false
}
