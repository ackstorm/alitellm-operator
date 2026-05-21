// SPDX-License-Identifier: Apache-2.0

package substitution

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseCSVSecrets converts a "K=V,K2=V2" string into the secrets map the
// Substitute helper accepts. Malformed tokens are skipped silently — the
// fuzzer's job is to probe Substitute, not to validate this helper's
// own input shape.
func parseCSVSecrets(s string) map[string]string {
	out := make(map[string]string)
	if s == "" {
		return out
	}
	for _, tok := range strings.Split(s, ",") {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		out[tok[:eq]] = tok[eq+1:]
	}
	return out
}

// FuzzSubstitute probes the placeholder/secret-substitution surface against
// arbitrary (bodyJSON, secretsCSV) inputs. Seed corpus mirrors the 14
// TestSubstitute_SEC* + table-style table tests (HRD-03 audit list); the
// fuzzer's job is to find panic-class inputs that the SEC suite missed.
//
// Invariant: for any (bodyJSON, secretsCSV) pair Substitute MUST NOT panic.
// Malformed JSON is skipped at the iteration boundary so the fuzzer
// explores body-shape space, not JSON parser space.
//
// 13-SPEC.md HRD-03 acceptance:
//
//	./scripts/dev.sh go test -run='^$' -fuzz=FuzzSubstitute -fuzztime=10s ./internal/substitution/...
func FuzzSubstitute(f *testing.F) {
	// Seed 1 — SEC01 happy path.
	f.Add(`{"api_key":"{{ANTHROPIC_API_KEY}}"}`, `ANTHROPIC_API_KEY=sk-real`)
	// Seed 2 — SEC02 strict regex (whitespace inside braces is invalid).
	f.Add(`{"x":"{{ A }}"}`, `A=v`)
	// Seed 3 — SEC02 strict regex (lowercase is invalid per [A-Z_][A-Z0-9_]*).
	f.Add(`{"x":"{{api_key}}"}`, `api_key=v`)
	// Seed 4 — SEC02 strict regex (digit-leading is invalid).
	f.Add(`{"x":"{{1ABC}}"}`, `1ABC=v`)
	// Seed 5 — SEC04 multi-placeholder ordering.
	f.Add(`{"url":"https://{{HOST}}/path?key={{TOKEN}}"}`, `HOST=example.com,TOKEN=abc`)
	// Seed 6 — SEC05 missing placeholder recorded.
	f.Add(`{"x":"{{MISSING_NAME}}"}`, ``)
	// Seed 7 — SEC07 unused secret not in referencedAs.
	f.Add(`{"x":"{{USED}}"}`, `USED=v,UNUSED=w`)
	// Seed 8 — SEC10 os.environ/ passthrough literal.
	f.Add(`{"api_key":"os.environ/MY_KEY"}`, ``)
	// Seed 9 — nested map containing a placeholder.
	f.Add(`{"outer":{"inner":"{{NAME}}"}}`, `NAME=v`)
	// Seed 10 — nested list with placeholder element.
	f.Add(`{"items":["{{NAME}}","literal"]}`, `NAME=v`)
	// Seed 11 — mixed types passthrough (int, bool, float, nil).
	f.Add(`{"x":1,"y":true,"z":1.5,"n":null}`, ``)
	// Seed 12 — duplicate placeholders deduped in referencedAs.
	f.Add(`{"a":"{{X}}","b":"{{X}}"}`, `X=v`)
	// Seed 13 — duplicate missing names deduped in missingPlaceholders.
	f.Add(`{"a":"{{Y}}","b":"{{Y}}"}`, ``)
	// Seed 14 — empty inputs edge case.
	f.Add(`{}`, ``)
	// Shape-diversity extras (5):
	f.Add(`{"deep":{"d1":{"d2":{"d3":"{{NESTED}}"}}}}`, `NESTED=v`)
	f.Add(`{"":"{{EMPTY_KEY_PLACE}}"}`, ``)
	f.Add(`{"x":"{{NAME"}`, `NAME=v`)                                        // partial-brace value
	f.Add(`{"x":"plain","y":"{{A}} and {{B}} and {{A}}"}`, `A=alpha,B=beta`) // mixed plain + interpolation
	f.Add(`{"big":"`+strings.Repeat("{{X}}", 16)+`"}`, `X=v`)                // 16-fold repetition

	f.Fuzz(func(t *testing.T, bodyJSON string, secretsCSV string) {
		var body map[string]any
		if err := json.Unmarshal([]byte(bodyJSON), &body); err != nil {
			return // skip malformed JSON; explore body-shape space
		}
		if body == nil {
			return // nil body is not a valid input shape
		}
		secrets := parseCSVSecrets(secretsCSV)
		_, _, _ = Substitute(body, secrets) // success = no panic
	})
}
