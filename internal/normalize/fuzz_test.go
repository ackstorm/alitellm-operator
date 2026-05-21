// SPDX-License-Identifier: Apache-2.0

package normalize_test

import (
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/normalize"
)

// FuzzNormalize probes the rawID → K8s-name normalization surface for
// (a) panic-class inputs and (b) post-condition violations on the
// output character set. The Normalize contract is documented in
// internal/normalize/normalize.go: pure string → string, output bytes
// MUST be in [a-z0-9.\-] or the output MUST be empty.
//
// 13-SPEC.md HRD-03 acceptance:
//
//	./scripts/dev.sh go test -run='^$' -fuzz=FuzzNormalize -fuzztime=10s ./internal/normalize/...
func FuzzNormalize(f *testing.F) {
	// Seed 1 — Bedrock colon literal (verbatim from BedrockColonExample test).
	f.Add("anthropic.claude-3-sonnet-20240229-v1:0")
	// Seed 2 — Bedrock meta-llama literal (verbatim).
	f.Add("meta.llama3-70b-instruct-v1:0")
	// Seed 3 — multi-rule cascade (slash + colon + underscore + space all hit step-2).
	f.Add("anthropic/claude:3_sonnet 20240229")
	// Seed 4 — dash collapse (step-4 consecutive-dash run-length compression).
	f.Add("claude---3-sonnet")
	// Seed 5 — unicode (multi-byte UTF-8 runes; output must be ASCII-safe).
	f.Add("claude-中文-3")
	// Seed 6 — all punctuation (every char strips; output is empty after trim).
	f.Add("-:-_-")

	f.Fuzz(func(t *testing.T, in string) {
		out := normalize.Normalize(in)
		for _, b := range []byte(out) {
			valid := (b >= 'a' && b <= 'z') ||
				(b >= '0' && b <= '9') ||
				b == '.' || b == '-'
			if !valid {
				t.Errorf("Normalize(%q) = %q contains invalid byte 0x%02x (%q)",
					in, out, b, string(b))
			}
		}
	})
}
