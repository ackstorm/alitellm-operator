// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// ErrorKind classifies an HTTP response status for reconcile-level
// retry / fast-path decisions.
type ErrorKind int

const (
	// KindTransient — caller should requeue (network errors, 5xx).
	KindTransient ErrorKind = iota
	// KindPermanent — caller should NOT requeue without operator action
	// (4xx other than 401, 422 validation, etc.).
	KindPermanent
	// KindAuth401 — caller should take the §7.7 fast-path.
	KindAuth401
)

// classify maps an HTTP status code to an ErrorKind. Network errors
// (the resp == nil case) are mapped by the caller to KindTransient.
func classify(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return KindAuth401
	case status >= 500:
		return KindTransient
	case status >= 400:
		return KindPermanent
	default:
		return KindPermanent
	}
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

// processLitellmError parses the {error: {message, type, param, code}}
// envelope LiteLLM returns on every non-2xx response. On unmarshal
// failure it returns the raw body capped at 512 bytes and kind="unparsed"
// so the caller still has something to log without spraying possibly
// large response bodies into error strings.
//
// Derivative work from bbdsoftware/litellm-operator (Apache-2.0; NOTICE).
func processLitellmError(body []byte) (kind, message, code string) {
	var env litellmErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" && env.Error.Message == "" {
		cap := body
		if len(cap) > 512 {
			cap = cap[:512]
		}
		return "unparsed", string(cap), ""
	}
	return env.Error.Type, env.Error.Message, env.Error.Code
}
