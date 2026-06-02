// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// maxRejectedMessageBytes caps the LiteLLMRejected condition.Message
// length so `kubectl get -o wide` (which truncates around 80 chars) and
// status writers (whose updates are bounded by the apiserver's 1MiB
// resource limit) stay healthy regardless of how chatty an upstream
// error envelope is.
const maxRejectedMessageBytes = 200

// dangerouslyIncludeRejectedBodyEnv is the opt-in escape hatch that
// restores the parsed LiteLLM error envelope Message into the persisted
// CR condition. OFF by default because the envelope can echo inbound
// request payload fields (param echo, JSON-decode error citing a value)
// and LiteLLM is the source of truth for whether a given field is
// secret-bearing — operator cannot know in general. The opt-in path
// still runs `sanitizeSecretShapedTokens` so well-known token prefixes
// (sk-, xai-, claude-, gsk_, AKIA..., AIzaSy..., hf_, Bearer ...) are
// redacted as a last line of defense.
//
// Kept unexported: single consumer (rejectedMessage in this package);
// no cross-package callers and no contract for one. Promote to
// `EnvDangerouslyIncludeRejectedBody` only if/when a second consumer
// appears, mirroring `litellm.EnvDangerouslyLogBodies`.
const dangerouslyIncludeRejectedBodyEnv = "LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY"

// secretShapedTokenRE matches common secret-key prefixes and Bearer/
// Authorization headers across LiteLLM-supported providers. Currently
// covers: OpenAI/generic (sk-), xAI (xai-), Anthropic (claude-),
// Groq (gsk_), AWS Bedrock access keys (AKIA + 16 chars), Google AI
// Studio (AIzaSy + 33 chars), HuggingFace (hf_), plus Bearer /
// Authorization header forms. Non-exhaustive by design — the default
// path already strips the entire envelope; this is defense in depth
// for the opt-in path. Updated as new provider prefixes are observed.
var secretShapedTokenRE = regexp.MustCompile(
	`(?i)\b(?:sk-[A-Za-z0-9_\-]{8,}|xai-[A-Za-z0-9_\-]{8,}|claude-[A-Za-z0-9_\-]{8,}|gsk_[A-Za-z0-9_\-]{8,}|AKIA[0-9A-Z]{16}|AIzaSy[0-9A-Za-z_\-]{33}|hf_[A-Za-z0-9]{30,}|Bearer\s+[A-Za-z0-9_\-\.]{8,}|Authorization:\s*[A-Za-z0-9_\-\.]{8,})`,
)

// rejectedMessage formats a LiteLLMRejected condition message.
//
// DEFAULT PATH (no opt-in): emits a generic shape carrying status code +
// LiteLLM error.code only — the envelope Message is NOT persisted to CR
// status, regardless of content. Logs continue to carry rej.Message via
// the caller's error path (transport-layer redaction applies there).
//
// OPT-IN PATH: when LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY=true,
// the envelope Message is appended AFTER secret-shaped-token sanitization.
// Even with the opt-in, never restores anything matching
// secretShapedTokenRE — those substrings are replaced with [REDACTED].
//
// Non-*RejectedError errors fall back to the pre-existing
// "LiteLLM rejected <op>: <errStr>" shape (errStr is already redacted by
// the transport layer for HTTP errors, or carries plain network text for
// dial/timeout cases — both safe).
//
// Post-2026-05-26 status-leak fix.
func rejectedMessage(opDesc string, err error, errStr string) string {
	var rej *litellm.RejectedError
	if !errors.As(err, &rej) {
		return clipMessage(fmt.Sprintf("LiteLLM rejected %s: %s", opDesc, errStr))
	}

	// #55 (P0 security): rej.Code and rej.Type are interpolated into CR
	// status.conditions[].message — a cluster-readable surface. The
	// construction site (client.makeRequest) only filters the "unparsed"
	// sentinel; it does NOT enforce a closed enum, so a proxy in front of
	// LiteLLM (or a non-LiteLLM upstream) can place arbitrary, possibly
	// secret-shaped text in either field. Route BOTH through the same
	// secret-shaped-token redactor the opt-in Message path uses, as
	// defense in depth on this no-opt-in path.
	code := sanitizeSecretShapedTokens(rej.Code)
	base := fmt.Sprintf(
		"LiteLLM rejected %s: %d (code=%s)",
		opDesc, rej.Status, code,
	)
	if rej.Type != "" {
		// UAT LOW-02: error.type is documented as a LiteLLM closed enum
		// (auth_error, validation_error, …); surfaced without the opt-in,
		// but sanitized first (#55) because the enum contract is not
		// enforced upstream of the operator.
		base = fmt.Sprintf(
			"LiteLLM rejected %s: %d (code=%s, type=%s)",
			opDesc, rej.Status, code, sanitizeSecretShapedTokens(rej.Type),
		)
	}

	if strings.EqualFold(os.Getenv(dangerouslyIncludeRejectedBodyEnv), "true") && rej.Message != "" {
		sanitized := sanitizeSecretShapedTokens(rej.Message)
		base = base + ": " + sanitized
	}

	return clipMessage(base)
}

// sanitizeSecretShapedTokens replaces well-known secret prefixes with
// [REDACTED]. Defense-in-depth on both paths: the opt-in
// dangerously-include-body path (rej.Message) and the default path's
// rej.Code / rej.Type interpolation (#55).
func sanitizeSecretShapedTokens(s string) string {
	return secretShapedTokenRE.ReplaceAllString(s, "[REDACTED]")
}

// clipMessage truncates s to maxRejectedMessageBytes; oversized strings
// get an ellipsis suffix so the truncation is visible. The cut walks
// back to a UTF-8 rune boundary so a multi-byte rune is never split
// (which would otherwise yield invalid UTF-8 in CR status — apiserver
// would reject the update). UTF-8 max rune size is 4 bytes, so walking
// back at most 3 bytes is sufficient.
func clipMessage(s string) string {
	if len(s) <= maxRejectedMessageBytes {
		return s
	}
	cut := maxRejectedMessageBytes - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
