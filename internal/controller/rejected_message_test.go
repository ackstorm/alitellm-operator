// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// Tests for rejectedMessage. The contract (post-2026-05-26 status-leak fix):
//   - Default path: emits "LiteLLM rejected <op>: <status> (code=<code>)" —
//     no envelope body, no provider secret can leak via param-echo.
//   - Opt-in path: LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY=true
//     restores the envelope Message AFTER the secret-shaped-token sanitizer.
//   - Non-RejectedError errors fall back to the generic shape (unchanged).

func TestRejectedMessage_DefaultStripsEnvelopeBody(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "")
	rej := &litellm.RejectedError{
		Method:  "POST",
		Path:    "/model/new",
		Status:  400,
		Code:    "400",
		Message: "Invalid api_key sk-leaked-key-1234567890abcdef",
	}
	got := rejectedMessage("model create", rej, rej.Error())
	if strings.Contains(got, "sk-leaked-key") {
		t.Fatalf("envelope body leaked into condition message: %q", got)
	}
	if !strings.Contains(got, "LiteLLM rejected model create") {
		t.Fatalf("expected generic prefix, got %q", got)
	}
	if !strings.Contains(got, "400") {
		t.Fatalf("expected status code in message, got %q", got)
	}
}

func TestRejectedMessage_OptInRestoresBodyButSanitizesSecrets(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	rej := &litellm.RejectedError{
		Method:  "POST",
		Path:    "/model/new",
		Status:  400,
		Code:    "400",
		Message: "Invalid api_key sk-leaked-key-1234567890abcdef on call",
	}
	got := rejectedMessage("model create", rej, rej.Error())
	if strings.Contains(got, "sk-leaked-key") {
		t.Fatalf("secret-shaped token survived sanitizer: %q", got)
	}
	if !strings.Contains(got, "Invalid api_key") {
		t.Fatalf("expected envelope body (post-sanitizer) in opt-in mode, got %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected redaction marker, got %q", got)
	}
}

func TestRejectedMessage_OptInWithoutSecretsPassesThrough(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	rej := &litellm.RejectedError{
		Method:  "POST",
		Path:    "/team/new",
		Status:  400,
		Code:    "400",
		Message: "Server name cannot contain '.'",
	}
	got := rejectedMessage("team create", rej, rej.Error())
	if !strings.Contains(got, "Server name cannot contain") {
		t.Fatalf("expected actionable detail in opt-in mode, got %q", got)
	}
	if strings.Contains(got, "[REDACTED]") {
		t.Fatalf("regex over-matched benign text: %q", got)
	}
}

func TestRejectedMessage_NonRejectedErrorFallback(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "")
	err := errors.New("dial tcp: connection refused")
	got := rejectedMessage("model create", err, err.Error())
	if !strings.Contains(got, "dial tcp") {
		t.Fatalf("expected fallback to errStr for non-RejectedError, got %q", got)
	}
}

func TestRejectedMessage_RespectsMaxLength(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	long := strings.Repeat("x", 1000)
	rej := &litellm.RejectedError{
		Method:  "POST",
		Path:    "/p",
		Status:  400,
		Code:    "400",
		Message: long,
	}
	got := rejectedMessage("op", rej, rej.Error())
	if len(got) > maxRejectedMessageBytes {
		t.Fatalf("clip not respected: got %d bytes, want <= %d", len(got), maxRejectedMessageBytes)
	}
}

// Empty Message must not append the ": " separator — covers the
// `rej.Message != ""` guard in rejectedMessage.
func TestRejectedMessage_OptInEmptyMessageDoesNotAppendColon(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	rej := &litellm.RejectedError{Method: "POST", Path: "/p", Status: 400, Code: "400", Message: ""}
	got := rejectedMessage("op", rej, rej.Error())
	if strings.HasSuffix(got, ": ") {
		t.Fatalf("trailing colon-space leaked despite empty Message: %q", got)
	}
	want := "LiteLLM rejected op: 400 (code=400)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Extended provider coverage: AWS Bedrock access keys, Google AI Studio
// API keys, and HuggingFace tokens must be redacted by the
// sanitizeSecretShapedTokens path when the opt-in is enabled.
func TestRejectedMessage_OptInRedactsExtendedProviderKeys(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	cases := []struct {
		name    string
		message string
	}{
		{"aws", "Invalid AKIAIOSFODNN7EXAMPLE access key"},
		{"google_ai_studio", "Invalid AIzaSyFAKEFixtureNotARealKeyForTestingA api key"},
		{"huggingface", "Invalid hf_abcdefghijklmnopqrstuvwxyz1234567890 token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rej := &litellm.RejectedError{Method: "POST", Path: "/p", Status: 400, Code: "400", Message: tc.message}
			got := rejectedMessage("op", rej, rej.Error())
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("%s key not redacted: %q", tc.name, got)
			}
		})
	}
}

// clipMessage must walk back to a UTF-8 rune boundary so a multi-byte
// rune is never split — otherwise the apiserver would reject the
// status update for containing invalid UTF-8.
func TestRejectedMessage_ClipPreservesRuneBoundary(t *testing.T) {
	t.Setenv("LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY", "true")
	// 195 bytes of 'a' (under the 200-byte clip), then multi-byte runes
	// that straddle the maxRejectedMessageBytes-3 cut point. Each CJK
	// rune below is 3 bytes in UTF-8.
	msg := strings.Repeat("a", 195) + "你好世界"
	rej := &litellm.RejectedError{Method: "POST", Path: "/p", Status: 400, Code: "400", Message: msg}
	got := rejectedMessage("op", rej, rej.Error())
	if !utf8.ValidString(got) {
		t.Fatalf("clipMessage produced invalid UTF-8: %q", got)
	}
}
