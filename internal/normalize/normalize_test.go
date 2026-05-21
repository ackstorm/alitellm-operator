// SPDX-License-Identifier: Apache-2.0

package normalize_test

import (
	"strings"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/normalize"
)

// ─── Normalize — table-driven coverage of the 5-step pipeline (spec §6.3 lines 743-751) ──

// TestNormalize_Step1_Lowercase asserts that uppercase characters are
// down-cased before any character substitution runs.
func TestNormalize_Step1_Lowercase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"MixedCase", "Claude-3-Sonnet", "claude-3-sonnet"},
		{"AllUpper", "ANTHROPIC", "anthropic"},
		{"DigitPunctUpper", "GPT-4.1-Turbo", "gpt-4.1-turbo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_Step2_SlashColonUnderscoreSpace exercises the step-2
// character class {/, :, _, space} → "-" replacement.
func TestNormalize_Step2_SlashColonUnderscoreSpace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"Slash", "anthropic/claude-3", "anthropic-claude-3"},
		{"Colon", "anthropic:claude-3", "anthropic-claude-3"},
		{"Underscore", "anthropic_claude_3", "anthropic-claude-3"},
		{"Space", "claude 3 sonnet", "claude-3-sonnet"},
		{"AllFour", "anthropic/claude:3_sonnet 20240229", "anthropic-claude-3-sonnet-20240229"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_Step3_OtherInvalidChars covers characters outside the
// allowed set [a-z0-9.-] that are NOT in step 2's class — they must be
// replaced with "-" in step 3.
func TestNormalize_Step3_OtherInvalidChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"PlusAt", "claude+3@sonnet", "claude-3-sonnet"},
		{"BangHash", "claude!3#sonnet", "claude-3-sonnet"},
		{"ParensBrackets", "claude(3)[sonnet]", "claude-3-sonnet"},
		{"AmpQuestion", "claude&3?sonnet", "claude-3-sonnet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_Step4_CollapseDashes asserts that runs of consecutive
// dashes (produced by earlier replacement steps) collapse to a single
// dash before trimming.
func TestNormalize_Step4_CollapseDashes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ThreeDashes", "claude---3-sonnet", "claude-3-sonnet"},
		{"FiveDashes", "claude-----3-sonnet", "claude-3-sonnet"},
		{"InternalRunsOnly", "claude---3-----sonnet", "claude-3-sonnet"},
		{"DashesFromStepReplacement", "a/b/c_d e:f", "a-b-c-d-e-f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_Step5_TrimLeadingTrailingNonAlnum asserts that the
// leading and trailing non-[a-z0-9] characters are stripped (dots
// included — DNS-1123 forbids labels that start with a dot).
func TestNormalize_Step5_TrimLeadingTrailingNonAlnum(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"LeadingDash", "-claude-3-sonnet", "claude-3-sonnet"},
		{"TrailingDash", "claude-3-sonnet-", "claude-3-sonnet"},
		{"BothDashes", "-claude-3-sonnet-", "claude-3-sonnet"},
		{"LeadingDots", "...claude.3.sonnet", "claude.3.sonnet"},
		{"TrailingDots", "claude.3.sonnet...", "claude.3.sonnet"},
		{"BothDots", "...claude.3.sonnet...", "claude.3.sonnet"},
		{"MixedLeading", "-.-claude", "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_BedrockColonExample_Verbatim locks the spec §6.3 line 756
// example: the colon between `-v1` and `0` MUST normalize to a dash,
// yielding a DNS-1123-valid name.
func TestNormalize_BedrockColonExample_Verbatim(t *testing.T) {
	const in = "anthropic.claude-3-sonnet-20240229-v1:0"
	const want = "anthropic.claude-3-sonnet-20240229-v1-0"
	if got := normalize.Normalize(in); got != want {
		t.Errorf("Normalize(%q) = %q; want %q (spec §6.3 line 756)", in, got, want)
	}
}

// TestNormalize_BedrockMetaLlamaExample_Verbatim locks the spec §6.3
// line 757 example.
func TestNormalize_BedrockMetaLlamaExample_Verbatim(t *testing.T) {
	const in = "meta.llama3-70b-instruct-v1:0"
	const want = "meta.llama3-70b-instruct-v1-0"
	if got := normalize.Normalize(in); got != want {
		t.Errorf("Normalize(%q) = %q; want %q (spec §6.3 line 757)", in, got, want)
	}
}

// TestNormalize_EmptyAfterTrim verifies the contract: when every input
// byte becomes a dash and the trim wipes it all, Normalize returns "".
// The caller (reconciler) is responsible for catching this via the
// DNS-1123 validator (empty string is not a valid subdomain).
func TestNormalize_EmptyAfterTrim(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"AllPunct", "-:-_-"},
		{"OnlyDashes", "----"},
		{"OnlyColons", "::::"},
		{"OnlyDots", "...."}, // dots are valid bytes but get trimmed (non-alnum)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize.Normalize(tc.in)
			if got != "" {
				t.Errorf("Normalize(%q) = %q; want empty (caller catches via DNS-1123)", tc.in, got)
			}
		})
	}
}

// TestNormalize_UnicodeReplacement documents Go regexp's byte-level
// handling of multi-byte UTF-8 chars inside the step-3 invalid-chars
// regex. Each non-ASCII byte is replaced with a dash; consecutive
// dashes then collapse. v1alpha1 providers all return ASCII IDs, so
// this case exists to lock the observable behavior in case it ever
// fires on a pathological feed.
func TestNormalize_UnicodeReplacement(t *testing.T) {
	in := "claude-中文-3"
	// `中` and `文` are 3-byte UTF-8 sequences. Step 3 replaces each
	// byte with a dash → step 4 collapses runs → "claude-3".
	const want = "claude-3"
	if got := normalize.Normalize(in); got != want {
		t.Errorf("Normalize(%q) = %q; want %q (byte-level Unicode behavior is documented)", in, got, want)
	}
}

// TestNormalize_EmptyInput verifies Normalize("") returns "".
func TestNormalize_EmptyInput(t *testing.T) {
	if got := normalize.Normalize(""); got != "" {
		t.Errorf("Normalize(\"\") = %q; want empty", got)
	}
}

// ─── DNS1123Subdomain — validation wrapper around k8s.io/apimachinery ──

// TestDNS1123_Valid_BedrockExample asserts the post-normalization
// Bedrock name passes DNS-1123 subdomain validation.
func TestDNS1123_Valid_BedrockExample(t *testing.T) {
	const name = "bedrock.anthropic.claude-3-sonnet-20240229-v1-0"
	if err := normalize.DNS1123Subdomain(name); err != nil {
		t.Errorf("DNS1123Subdomain(%q): unexpected error: %v", name, err)
	}
}

// TestDNS1123_Valid_TypicalNames asserts a handful of representative
// post-normalization names from each provider type validate cleanly.
func TestDNS1123_Valid_TypicalNames(t *testing.T) {
	names := []string{
		"anthropic.claude-3-5-sonnet-20241022",
		"openai.gpt-4-turbo-2024-04-09",
		"gemini.gemini-1.5-pro-002",
		"kubeai.llama-3.1-70b-instruct",
		"bedrock.amazon.titan-text-express-v1",
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			if err := normalize.DNS1123Subdomain(n); err != nil {
				t.Errorf("DNS1123Subdomain(%q) failed: %v", n, err)
			}
		})
	}
}

// TestDNS1123_TooLong254Chars asserts a 254-byte input fails validation
// with an error message mentioning the violated length cap (apimachinery's
// canonical message contains "253 characters" or "must be no more than 253").
func TestDNS1123_TooLong254Chars(t *testing.T) {
	long := strings.Repeat("a", 254)
	err := normalize.DNS1123Subdomain(long)
	if err == nil {
		t.Fatalf("DNS1123Subdomain(<254-char>): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "253") {
		t.Errorf("DNS1123Subdomain error message %q should mention the 253 limit", err.Error())
	}
}

// TestDNS1123_InvalidUppercase asserts uppercase characters are
// rejected (DNS-1123 mandates lowercase).
func TestDNS1123_InvalidUppercase(t *testing.T) {
	if err := normalize.DNS1123Subdomain("BedRock.Claude"); err == nil {
		t.Errorf("DNS1123Subdomain(\"BedRock.Claude\"): expected error, got nil")
	}
}

// TestDNS1123_EmptyString asserts the empty string is rejected.
// Normalize may return "" on pathological input; the validator
// catches that downstream.
func TestDNS1123_EmptyString(t *testing.T) {
	if err := normalize.DNS1123Subdomain(""); err == nil {
		t.Errorf("DNS1123Subdomain(\"\"): expected error, got nil")
	}
}

// TestDNS1123_LeadingDot asserts a name starting with `.` is rejected
// (the segment before the first dot is empty).
func TestDNS1123_LeadingDot(t *testing.T) {
	if err := normalize.DNS1123Subdomain(".claude-3"); err == nil {
		t.Errorf("DNS1123Subdomain(\".claude-3\"): expected error, got nil")
	}
}

// TestDNS1123_TrailingDot asserts a name ending with `.` is rejected.
func TestDNS1123_TrailingDot(t *testing.T) {
	if err := normalize.DNS1123Subdomain("claude-3."); err == nil {
		t.Errorf("DNS1123Subdomain(\"claude-3.\"): expected error, got nil")
	}
}

// TestDNS1123_LeadingDash asserts a name starting with `-` is rejected
// (per DNS-1123, each label must start with [a-z0-9]).
func TestDNS1123_LeadingDash(t *testing.T) {
	if err := normalize.DNS1123Subdomain("-claude-3"); err == nil {
		t.Errorf("DNS1123Subdomain(\"-claude-3\"): expected error, got nil")
	}
}

// TestDNS1123_ErrorMessageIncludesInput verifies the wrapped error
// references the input name so the reconciler can surface it in the
// status.skippedCandidates[].message field per spec §6.3 line 762.
func TestDNS1123_ErrorMessageIncludesInput(t *testing.T) {
	const in = "INVALID_NAME"
	err := normalize.DNS1123Subdomain(in)
	if err == nil {
		t.Fatalf("DNS1123Subdomain(%q): expected error, got nil", in)
	}
	if !strings.Contains(err.Error(), in) {
		t.Errorf("DNS1123Subdomain error %q should contain input %q so the reconciler can surface it in skippedCandidates[].message",
			err.Error(), in)
	}
}
