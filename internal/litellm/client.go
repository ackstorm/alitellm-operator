// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
)

// Client is the operator's *http.Client wrapper for the LiteLLM REST
// API. All per-domain helpers (model.go, team.go, mcp.go, agents.go,
// keyinfo.go) call into Client.makeRequest.
type Client struct {
	endpoint   string
	masterKey  string
	httpClient *http.Client
	log        logr.Logger
	// limiter caps the sustained rate of outbound HTTP requests to
	// LiteLLM (FIX2.txt MEDIUM-10, 2026-05-22). Nil → unlimited.
	limiter *rate.Limiter
	// listCache memoizes ListMCPServers / ListAgents results for the
	// vanish-probe hot paths. Nil → caching disabled (every CachedListXxx
	// call falls through to the bare LIST). See list_cache.go for details.
	listCache *listCacheStore
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

// WithListCacheTTL enables the in-memory LIST cache used by the
// vanish-probe hot paths. ttl <= 0 disables caching (every
// CachedListXxx call falls through to the bare LIST endpoint). See
// list_cache.go and DefaultListCacheTTL.
//
// v0.4.6: introduced to dedupe the per-CR vanish-probe traffic
// (26 MCPServers × 1 LIST per 5m = 5.2 LIST/min collapsed to 1 LIST
// per TTL window). Connection reconciler wires this on every probe
// outcome so the cache shares the lifetime of the *Client (~5m
// between probe rebuilds).
func WithListCacheTTL(ttl time.Duration) ClientOption {
	return func(c *Client) {
		if ttl > 0 {
			c.listCache = &listCacheStore{ttl: ttl}
		} else {
			c.listCache = nil
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
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
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
	req.Header.Set("Authorization", "Bearer "+c.masterKey)
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

	// H4: cap the read with explicit truncation + preserved read error.
	// Error/non-2xx envelopes are small, but success bodies are LIST
	// payloads the operator parses for drift (ListMCPServers, ListAgents,
	// ListGuardrails, ListTeamsByAlias, GetModelInfo) and can legitimately
	// exceed 1 MB. A silent 1 MB truncation produced an invalid-JSON decode
	// error that looped forever. Use a small cap for envelopes, a larger cap
	// for success bodies, and surface ErrResponseTooLarge distinctly.
	const (
		errEnvelopeCap = 1 << 20  // 1 MB — error / non-2xx envelopes
		listBodyCap    = 16 << 20 // 16 MB — success bodies parsed for drift
	)
	readCap := errEnvelopeCap
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		readCap = listBodyCap
	}
	respBody, readErr := readCappedBody(resp.Body, readCap)
	if readErr != nil {
		// Distinct, actionable error — not a silent decode failure.
		return nil, fmt.Errorf("litellm: %s %s: %w", method, path, readErr)
	}

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
		kind, msg, code := processLitellmError(respBody)
		if code == "" {
			code = fmt.Sprintf("%d", resp.StatusCode)
		}
		// v0.7.3: kindUnparsed is processLitellmError's internal sentinel
		// for "envelope did not deserialize" — it is NOT a LiteLLM
		// closed-enum value (auth_error, validation_error, …). Drop it
		// here so RejectedError.Type honors its documented contract and
		// CR status.message never reads `type=unparsed` (operator state
		// masquerading as LiteLLM state).
		if kind == kindUnparsed {
			kind = ""
		}
		return nil, &RejectedError{
			Method:  method,
			Path:    path,
			Status:  resp.StatusCode,
			Code:    code,
			Type:    kind,
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
