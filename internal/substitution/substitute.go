// SPDX-License-Identifier: Apache-2.0

package substitution

import (
	"regexp"
	"sort"
)

// placeholderRe is the §5.2 strict regex (CONTEXT.md D-05).
//
// Pattern: \{\{[A-Z_][A-Z0-9_]*\}\}
// - "{{" and "}}" are the literal delimiters.
// - NAME must start with [A-Z_] and consist only of [A-Z0-9_].
// - Whitespace inside braces does NOT match (SEC-02).
// - Lowercase-led and digit-led names do NOT match (SEC-02).
//
// NAME is extracted in substituteString by slicing the match string
// (match[2:len-2]) — no second regex pass required per match.
var placeholderRe = regexp.MustCompile(`\{\{[A-Z_][A-Z0-9_]*\}\}`)

// Substitute walks string-typed leaves of body recursively and replaces
// literal {{NAME}} placeholders from the secrets map. Returns the set of
// "as" names that were actually referenced by at least one placeholder
// match — caller diffs this against spec.secrets[].as to detect
// UnusedSecretRef (SEC-07).
//
// missingPlaceholders contains every NAME found in the body that was absent
// from secrets. The caller maps each missing name to a Ready=False /
// reason=SecretNotFound condition (SEC-05 / SEC-06).
//
// Contract (CONTEXT.md D-05 — non-negotiable):
// - body is walked in-place; the caller must own the body map.
// - Non-string leaves (int, bool, float64, nil) pass through unchanged.
// - err is always nil in this implementation; the signature reserves the
// return slot for future JSON-decode error propagation.
func Substitute(body map[string]any, secrets map[string]string) (referencedAs []string, missingPlaceholders []string, err error) {
	refSet := make(map[string]struct{})
	missSet := make(map[string]struct{})

	walkMap(body, secrets, refSet, missSet)

	return toSortedSlice(refSet), toSortedSlice(missSet), nil
}

// walkMap recurses into a map[string]any, replacing string leaf values in
// place and delegating nested maps and slices to their respective helpers.
func walkMap(m map[string]any, secrets map[string]string, refSet, missSet map[string]struct{}) {
	for k, v := range m {
		m[k] = walk(v, secrets, refSet, missSet)
	}
}

// walk dispatches on the dynamic type of v. String leaves are substituted;
// maps and slices are recursed into; all other types pass through unchanged.
// The returned value is the (possibly substituted) replacement for the caller
// to store back in its parent container.
func walk(v any, secrets map[string]string, refSet, missSet map[string]struct{}) any {
	switch typed := v.(type) {
	case map[string]any:
		walkMap(typed, secrets, refSet, missSet)
		return typed

	case []any:
		for i, elem := range typed {
			typed[i] = walk(elem, secrets, refSet, missSet)
		}
		return typed

	case string:
		return substituteString(typed, secrets, refSet, missSet)

	default:
		// int, float64, bool, nil — non-string leaves are not scanned (SEC-02).
		return v
	}
}

// substituteString applies placeholderRe to a single string leaf, looking
// up each matched NAME in secrets. On a hit the replacement value is
// returned and the NAME is added to refSet. On a miss the original
// placeholder literal "{{NAME}}" is left in the output and the NAME is
// added to missSet.
//
// NAME extraction is done by slicing the match string directly — the regex
// guarantees the structure is always "{{NAME}}", so NAME = match[2:len-2].
// This avoids a second regex pass per match (zero extra allocation on the
// non-string code path per the implementation contract).
func substituteString(s string, secrets map[string]string, refSet, missSet map[string]struct{}) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(match string) string {
		// match is always "{{NAME}}" — peel the 2-char "{{" and "}}" delimiters.
		name := match[2 : len(match)-2]
		if val, ok := secrets[name]; ok {
			refSet[name] = struct{}{}
			return val
		}
		missSet[name] = struct{}{}
		return match // leave placeholder intact so callers can report the failure
	})
}

// toSortedSlice converts a set (map[string]struct{}) into a deterministically
// ordered []string. Sorted order makes test assertions and caller diffs
// (UnusedSecretRef, SecretNotFound) stable without additional caller-side
// sorting.
func toSortedSlice(s map[string]struct{}) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
