// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned by list-style helpers when the LiteLLM response
// envelope is non-nil but its Data slice is empty. Callers use
// errors.Is(err, ErrNotFound) to distinguish "list endpoint returned 200
// with empty data" from "HTTP error".
var ErrNotFound = errors.New("litellm: not found")

// Auth401Error is the typed error returned by Client.makeRequest when
// LiteLLM responds with HTTP 401. The reconciler's §7.7 fast-path uses
// errors.As(err, &auth401) to detect it and trigger cache-invalidate +
// re-probe enqueue + Ready=False, reason=LiteLLMUnavailable.
//
// The Body field carries the raw response body for diagnostic logging
// (only emitted when LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=true).
// Default code paths never log Body.
type Auth401Error struct {
	Path string
	Body []byte
}

// Error implements the error interface. The message intentionally omits
// the response body to honor §9.1: no body content in error strings,
// because controller-runtime emits errors as Events / status condition
// messages where they would be persisted in cluster-readable state.
func (e *Auth401Error) Error() string {
	return fmt.Sprintf("litellm: 401 unauthorized on %s", e.Path)
}

// litellmErrorEnvelope mirrors the LiteLLM 1.83.10 error response shape
// (uniform across all 14 authenticated endpoints — verified by spike
// Probe 8//
//
//	{"error":{"message":".","type":".","param":null|".","code":"401"}}
type litellmErrorEnvelope struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Param   json.RawMessage `json:"param"`
		Code    string          `json:"code"`
	} `json:"error"`
}

// RejectedError is returned by Client.makeRequest for any non-2xx,
// non-401 response. It carries the parsed envelope's message + code so
// reconcilers can surface a non-generic LiteLLMRejected condition
// without re-parsing the body or losing the upstream signal in a
// hand-rolled "litellm: 400 on <path>" prefix.
//
// FIX2.txt MEDIUM-5 (2026-05-22): pre-fix every 4xx wrote the same
// boilerplate string to condition.Message, hiding actionable detail
// (e.g. "Server name cannot contain '.'.") in the operator log.
type RejectedError struct {
	Method string
	Path   string
	Status int
	// Code is the envelope's error.code field when present; otherwise
	// the stringified HTTP status.
	Code string
	// Type is the envelope's error.type field when present (e.g.
	// "auth_error", "validation_error", "not_found_error"). Empty
	// when the body was unparseable OR when LiteLLM omitted error.type
	// — processLitellmError's "unparsed" internal sentinel is
	// filtered at the construction site (client.makeRequest, v0.7.3)
	// so this field strictly carries LiteLLM closed-enum values.
	// LiteLLM treats this field as a closed enum, NOT a free-form
	// echo of inbound payload, so it is safe to surface in CR
	// condition.Message without enabling
	// LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY (UAT LOW-02).
	Type string
	// Message is the envelope's error.message (already truncated to
	// 512 bytes by processLitellmError on parse). Empty when the body
	// was unparseable, in which case Error() falls back to the
	// status+path shape.
	Message string
}

// Error implements the error interface. §9.1: NEVER includes the
// parsed envelope body — Error() output flows into logs and the
// error-string prefix matchers (is4xxNon401Status etc.) which both
// must stay body-free. Reconcilers that want the body call .Message
// directly via errors.As after FIX2.txt M-5 (2026-05-22).
//
// Shape preserved bit-for-bit from the pre-FIX2 fmt.Errorf path so
// existing prefix matchers and tests keep passing.
func (e *RejectedError) Error() string {
	return fmt.Sprintf("litellm: %d on %s %s (code=%s)",
		e.Status, e.Method, e.Path, e.Code)
}

// IsNotFound returns true when err represents a "not found" response
// from LiteLLM, regardless of which surface produced it:
//
//   - errors.Is(err, ErrNotFound): list-style helpers (GetModelInfo,
//     GetMCPServer, GetAgent, ...) returned an empty Data array.
//   - errors.As(err, *RejectedError) with Status == 404: a non-DELETE
//     request received an HTTP 404 from LiteLLM (e.g., name-resolve
//     fallback on the deletion path of a Model CR with empty
//     status.lastRendered.modelID).
//
// Use IsNotFound on the deletion path of controllers that need to
// distinguish "already absent" from "other 4xx" (the latter should
// surface as LiteLLMRejected and NOT remove the finalizer).
//
// Post-2026-05-26 review finding F4.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var rej *RejectedError
	if errors.As(err, &rej) && rej.Status == 404 {
		return true
	}
	return false
}

// kindUnparsed is the sentinel `kind` value processLitellmError
// returns when the envelope cannot be deserialized OR carries empty
// code+message. Callers that surface kind into LiteLLM-facing fields
// (e.g. RejectedError.Type, which is documented as a LiteLLM closed
// enum) MUST filter this sentinel — see client.makeRequest (v0.7.3).
const kindUnparsed = "unparsed"

// processLitellmError parses the {error: {message, type, param, code}}
// envelope LiteLLM returns on every non-2xx response. On unmarshal
// failure it returns the raw body capped at 512 bytes and
// kind=kindUnparsed so the caller still has something to log without
// spraying possibly large response bodies into error strings.
//
// Derivative work from bbdsoftware/litellm-operator (Apache-2.0; NOTICE).
func processLitellmError(body []byte) (kind, message, code string) {
	var env litellmErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil ||
		(env.Error.Type == "" && env.Error.Code == "" && env.Error.Message == "") {
		cap := body
		if len(cap) > 512 {
			cap = cap[:512]
		}
		return kindUnparsed, string(cap), ""
	}
	return env.Error.Type, env.Error.Message, env.Error.Code
}
