// SPDX-License-Identifier: Apache-2.0

package substitution

import (
	"reflect"
	"sort"
	"testing"
)

// sortedStrings returns a sorted copy of the given slice (nil-safe).
// Used to normalize result slices before comparison without relying on
// implementation-specific ordering.
func sortedStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

// TestSubstitute_SEC01_HappyPath verifies the basic substitution case:
// a single {{NAME}} placeholder is replaced by the matching secrets value;
// referencedAs contains the matched name; missingPlaceholders is empty.
func TestSubstitute_SEC01_HappyPath(t *testing.T) {
	body := map[string]any{
		"api_key": "{{ANTHROPIC_API_KEY}}",
	}
	secrets := map[string]string{
		"ANTHROPIC_API_KEY": "sk-real",
	}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]any{"api_key": "sk-real"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"ANTHROPIC_API_KEY"}) {
		t.Errorf("referencedAs = %v, want [ANTHROPIC_API_KEY]", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_SEC02_StrictRegex covers three negative-match cases per §5.2:
// whitespace inside braces, lowercase NAME, and a digit-leading NAME.
func TestSubstitute_SEC02_StrictRegex(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		secrets map[string]string
		wantKey string
		wantVal string
	}{
		{
			name:    "whitespace not matched",
			body:    map[string]any{"x": "{{ A }}"},
			secrets: map[string]string{"A": "v"},
			wantKey: "x",
			wantVal: "{{ A }}",
		},
		{
			name:    "lowercase not matched",
			body:    map[string]any{"x": "{{api_key}}"},
			secrets: map[string]string{"api_key": "v"},
			wantKey: "x",
			wantVal: "{{api_key}}",
		},
		{
			name:    "digit-start not matched",
			body:    map[string]any{"x": "{{1ABC}}"},
			secrets: map[string]string{"1ABC": "v"},
			wantKey: "x",
			wantVal: "{{1ABC}}",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, miss, err := Substitute(c.body, c.secrets)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := c.body[c.wantKey]; got != c.wantVal {
				t.Errorf("body[%q] = %q, want %q", c.wantKey, got, c.wantVal)
			}
			if len(ref) != 0 {
				t.Errorf("referencedAs = %v, want []", ref)
			}
			if len(miss) != 0 {
				t.Errorf("missingPlaceholders = %v, want []", miss)
			}
		})
	}
}

// TestSubstitute_SEC04_MultiPlaceholderOrdering verifies that multiple
// distinct placeholders in a single string are all substituted. The
// returned referencedAs SET (asserted via map equality) must contain all
// matched names.
func TestSubstitute_SEC04_MultiPlaceholderOrdering(t *testing.T) {
	body := map[string]any{
		"url": "https://{{HOST}}/path?key={{TOKEN}}",
	}
	secrets := map[string]string{
		"HOST":  "example.com",
		"TOKEN": "abc",
	}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantBody := map[string]any{"url": "https://example.com/path?key=abc"}
	if !reflect.DeepEqual(body, wantBody) {
		t.Errorf("body = %v, want %v", body, wantBody)
	}

	// Assert set equality (order-independent).
	refSet := make(map[string]bool, len(ref))
	for _, n := range ref {
		refSet[n] = true
	}
	if !refSet["HOST"] || !refSet["TOKEN"] || len(refSet) != 2 {
		t.Errorf("referencedAs set = %v, want {HOST, TOKEN}", refSet)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_SEC05_MissingPlaceholderRecorded verifies that a
// placeholder absent from secrets is left unchanged in the body and
// reported in missingPlaceholders; it must NOT appear in referencedAs.
func TestSubstitute_SEC05_MissingPlaceholderRecorded(t *testing.T) {
	body := map[string]any{
		"x": "{{MISSING_NAME}}",
	}
	secrets := map[string]string{}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Placeholder string must be left intact.
	if body["x"] != "{{MISSING_NAME}}" {
		t.Errorf("body[x] = %q, want {{MISSING_NAME}} (placeholder intact)", body["x"])
	}
	if len(ref) != 0 {
		t.Errorf("referencedAs = %v, want [] (missing placeholder must NOT appear)", ref)
	}
	if !reflect.DeepEqual(sortedStrings(miss), []string{"MISSING_NAME"}) {
		t.Errorf("missingPlaceholders = %v, want [MISSING_NAME]", miss)
	}
}

// TestSubstitute_SEC07_UnusedSecretNotInReferencedAs verifies that secrets
// present in the map but not referenced in the body do NOT appear in
// referencedAs. The caller is responsible for diffing referencedAs against
// spec.secrets[].as to detect UnusedSecretRef (SEC-07).
func TestSubstitute_SEC07_UnusedSecretNotInReferencedAs(t *testing.T) {
	body := map[string]any{
		"x": "{{USED}}",
	}
	secrets := map[string]string{
		"USED":   "v",
		"UNUSED": "w",
	}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if body["x"] != "v" {
		t.Errorf("body[x] = %q, want v", body["x"])
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"USED"}) {
		t.Errorf("referencedAs = %v, want [USED] (UNUSED must not appear)", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_SEC10_OsEnvironPassthrough confirms that os.environ/<VAR>
// literal strings are passed through unchanged — the regex never matches
// them because they have no {{.}} delimiters (SEC-10).
func TestSubstitute_SEC10_OsEnvironPassthrough(t *testing.T) {
	body := map[string]any{
		"api_key": "os.environ/MY_KEY",
	}
	secrets := map[string]string{}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if body["api_key"] != "os.environ/MY_KEY" {
		t.Errorf("body[api_key] = %q, want os.environ/MY_KEY (pass-through unchanged)", body["api_key"])
	}
	if len(ref) != 0 {
		t.Errorf("referencedAs = %v, want []", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_NestedMap verifies that substitution walks into nested
// map[string]any values recursively.
func TestSubstitute_NestedMap(t *testing.T) {
	body := map[string]any{
		"outer": map[string]any{
			"inner": "{{NAME}}",
		},
	}
	secrets := map[string]string{"NAME": "v"}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]any{
		"outer": map[string]any{
			"inner": "v",
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"NAME"}) {
		t.Errorf("referencedAs = %v, want [NAME]", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_NestedList verifies that substitution walks into []any
// slices recursively.
func TestSubstitute_NestedList(t *testing.T) {
	body := map[string]any{
		"list": []any{"a", "{{NAME}}", "c"},
	}
	secrets := map[string]string{"NAME": "b"}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]any{
		"list": []any{"a", "b", "c"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"NAME"}) {
		t.Errorf("referencedAs = %v, want [NAME]", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_MixedTypesPassthrough confirms that non-string leaf values
// (int, bool, float64, nil) pass through unchanged (SEC-02 non-string
// leaves not scanned).
func TestSubstitute_MixedTypesPassthrough(t *testing.T) {
	body := map[string]any{
		"n":   42,
		"b":   true,
		"f":   3.14,
		"nil": nil,
	}
	secrets := map[string]string{}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]any{
		"n":   42,
		"b":   true,
		"f":   3.14,
		"nil": nil,
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if len(ref) != 0 {
		t.Errorf("referencedAs = %v, want []", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_DuplicatePlaceholdersDeduped verifies that the same
// placeholder appearing multiple times produces exactly one entry in
// referencedAs and the body is fully substituted everywhere.
func TestSubstitute_DuplicatePlaceholdersDeduped(t *testing.T) {
	body := map[string]any{
		"a": "{{X}}",
		"b": "{{X}}",
		"c": "{{X}}",
	}
	secrets := map[string]string{"X": "v"}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]any{"a": "v", "b": "v", "c": "v"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"X"}) {
		t.Errorf("referencedAs = %v, want [X] (de-duplicated)", ref)
	}
	if len(miss) != 0 {
		t.Errorf("missingPlaceholders = %v, want []", miss)
	}
}

// TestSubstitute_DuplicateMissingDeduped verifies that the same missing
// placeholder appearing in multiple leaves produces exactly one entry in
// missingPlaceholders.
func TestSubstitute_DuplicateMissingDeduped(t *testing.T) {
	body := map[string]any{
		"a": "{{Y}}",
		"b": "{{Y}}",
	}
	secrets := map[string]string{}

	ref, miss, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both leaves remain unchanged.
	if body["a"] != "{{Y}}" || body["b"] != "{{Y}}" {
		t.Errorf("body = %v, want both leaves {{Y}}", body)
	}
	if len(ref) != 0 {
		t.Errorf("referencedAs = %v, want []", ref)
	}
	if !reflect.DeepEqual(sortedStrings(miss), []string{"Y"}) {
		t.Errorf("missingPlaceholders = %v, want [Y] (de-duplicated)", miss)
	}
}

// TestSubstitute_EmptyInputs verifies nil body and empty body both return
// empty slices and nil error.
func TestSubstitute_EmptyInputs(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		ref, miss, err := Substitute(nil, map[string]string{"X": "v"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ref) != 0 {
			t.Errorf("referencedAs = %v, want []", ref)
		}
		if len(miss) != 0 {
			t.Errorf("missingPlaceholders = %v, want []", miss)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		ref, miss, err := Substitute(map[string]any{}, map[string]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ref) != 0 {
			t.Errorf("referencedAs = %v, want []", ref)
		}
		if len(miss) != 0 {
			t.Errorf("missingPlaceholders = %v, want []", miss)
		}
	})
}

// TestSubstitute_InPlaceMutation verifies that the caller's input map is
// mutated directly — the returned body reference reflects the changes.
func TestSubstitute_InPlaceMutation(t *testing.T) {
	body := map[string]any{
		"key": "{{SECRET}}",
	}
	secrets := map[string]string{"SECRET": "resolved"}

	ref, _, err := Substitute(body, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The input map must be mutated in place.
	if body["key"] != "resolved" {
		t.Errorf("body[key] = %q after Substitute, want resolved (in-place mutation)", body["key"])
	}
	if !reflect.DeepEqual(sortedStrings(ref), []string{"SECRET"}) {
		t.Errorf("referencedAs = %v, want [SECRET]", ref)
	}
}

// TestSubstitute — table-driven catch-all covering additional edge cases
// and the full SEC matrix concisely.
func TestSubstitute(t *testing.T) {
	cases := []struct {
		name        string
		body        map[string]any
		secrets     map[string]string
		wantBody    map[string]any
		wantRef     []string // sorted expected
		wantMissing []string // sorted expected
	}{
		{
			name:        "empty secrets map with placeholder",
			body:        map[string]any{"x": "{{ALPHA}}"},
			secrets:     map[string]string{},
			wantBody:    map[string]any{"x": "{{ALPHA}}"},
			wantRef:     []string{},
			wantMissing: []string{"ALPHA"},
		},
		{
			name:        "underscore-led NAME is valid",
			body:        map[string]any{"x": "{{_MY_VAR}}"},
			secrets:     map[string]string{"_MY_VAR": "ok"},
			wantBody:    map[string]any{"x": "ok"},
			wantRef:     []string{"_MY_VAR"},
			wantMissing: []string{},
		},
		{
			name: "mixed hit and miss in same string",
			body: map[string]any{
				"x": "{{FOUND}}/{{ABSENT}}",
			},
			secrets:     map[string]string{"FOUND": "yes"},
			wantBody:    map[string]any{"x": "yes/{{ABSENT}}"},
			wantRef:     []string{"FOUND"},
			wantMissing: []string{"ABSENT"},
		},
		{
			name: "deeply nested map",
			body: map[string]any{
				"l1": map[string]any{
					"l2": map[string]any{
						"v": "{{DEEP}}",
					},
				},
			},
			secrets:     map[string]string{"DEEP": "d"},
			wantBody:    map[string]any{"l1": map[string]any{"l2": map[string]any{"v": "d"}}},
			wantRef:     []string{"DEEP"},
			wantMissing: []string{},
		},
		{
			name: "integer in list not scanned",
			body: map[string]any{
				"list": []any{1, "{{A}}", true},
			},
			secrets:     map[string]string{"A": "found"},
			wantBody:    map[string]any{"list": []any{1, "found", true}},
			wantRef:     []string{"A"},
			wantMissing: []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, miss, err := Substitute(c.body, c.secrets)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(c.body, c.wantBody) {
				t.Errorf("body = %v, want %v", c.body, c.wantBody)
			}
			if !reflect.DeepEqual(sortedStrings(ref), c.wantRef) {
				t.Errorf("referencedAs = %v, want %v", sortedStrings(ref), c.wantRef)
			}
			if !reflect.DeepEqual(sortedStrings(miss), c.wantMissing) {
				t.Errorf("missingPlaceholders = %v, want %v", sortedStrings(miss), c.wantMissing)
			}
		})
	}
}
