// SPDX-License-Identifier: Apache-2.0

package filters_test

import (
	"errors"
	"reflect"
	"testing"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/filters"
)

// wantErr asserts the type-of-error contract via errors.As (NOT
// string compare). wantType is one of:
// - "" → err must be nil
// - "*filters.UpstreamInvalidError" → errors.As to that type
// - "*filters.InvalidConfigError" → errors.As to that type
func wantErr(t *testing.T, got error, wantType string) {
	t.Helper()
	switch wantType {
	case "":
		if got != nil {
			t.Fatalf("want no error, got %T: %v", got, got)
		}
	case "*filters.UpstreamInvalidError":
		var target *filters.UpstreamInvalidError
		if !errors.As(got, &target) {
			t.Fatalf("want *filters.UpstreamInvalidError, got %T: %v", got, got)
		}
	case "*filters.InvalidConfigError":
		var target *filters.InvalidConfigError
		if !errors.As(got, &target) {
			t.Fatalf("want *filters.InvalidConfigError, got %T: %v", got, got)
		}
	default:
		t.Fatalf("wantErr: unknown wantType %q", wantType)
	}
}

// equalSliceLoose treats nil and []string{} as equal at the boundary
// (the Apply implementation may return either when nothing is kept).
func equalSliceLoose(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// ---- TestApply_NilFilters_PassesAll ------------------------------------

// Spec line 817: "No `filters` block → no filtering, all upstream names surfaced."
func TestApply_NilFilters_PassesAll(t *testing.T) {
	in := []string{"claude-3", "gpt-4"}
	got, err := filters.Apply(in, nil)
	wantErr(t, err, "")
	if !equalSliceLoose(got, in) {
		t.Fatalf("want %v, got %v", in, got)
	}
}

// ---- TestApply_EmptyIncludeEqualsAbsent --------------------------------

// Spec line 818: "`include` absent OR `include: []` → no include filter applied".
func TestApply_EmptyIncludeEqualsAbsent(t *testing.T) {
	in := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{Include: []string{}})
	wantErr(t, err, "")
	if !equalSliceLoose(got, in) {
		t.Fatalf("want %v, got %v", in, got)
	}
}

// ---- TestApply_EmptyExcludeEqualsAbsent --------------------------------

// Spec line 819: "`exclude` absent OR `exclude: []` → no exclusions."
func TestApply_EmptyExcludeEqualsAbsent(t *testing.T) {
	in := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{Exclude: []string{}})
	wantErr(t, err, "")
	if !equalSliceLoose(got, in) {
		t.Fatalf("want %v, got %v", in, got)
	}
}

// ---- TestApply_AnchoredFromStart ---------------------------------------

// Spec line 812: "implementation prepends `^` to each pattern".
//
// Filter `Include: ["claude"]` matches "claude-3" (starts with "claude")
// but NOT "models/claude-3" (starts with "m"). Substring match would
// keep both — the implicit anchor must drop "models/claude-3".
func TestApply_AnchoredFromStart(t *testing.T) {
	in := []string{"claude-3", "models/claude-3"}
	want := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"claude"},
	})
	wantErr(t, err, "")
	if !equalSliceLoose(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// ---- TestApply_IncludeStrict_ZeroMatchReturnsUpstreamInvalid -----------

// MDISC-07 / spec line 822: include matching zero IDs → UpstreamInvalid.
//
// The error payload must name every offending pattern (index + verbatim
// source string) so the reconciler can format the message per spec
// §6.3 line 822.
func TestApply_IncludeStrict_ZeroMatchReturnsUpstreamInvalid(t *testing.T) {
	in := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"o1-.*"},
	})
	wantErr(t, err, "*filters.UpstreamInvalidError")
	if len(got) != 0 {
		t.Fatalf("want nil/empty kept on UpstreamInvalid, got %v", got)
	}
	// Drill into the payload to verify Index + Pattern are reported.
	var uie *filters.UpstreamInvalidError
	if !errors.As(err, &uie) {
		t.Fatalf("errors.As(*UpstreamInvalidError) failed: %v", err)
	}
	if len(uie.UnmatchedPatterns) != 1 {
		t.Fatalf("want 1 unmatched pattern, got %d (%+v)", len(uie.UnmatchedPatterns), uie.UnmatchedPatterns)
	}
	if uie.UnmatchedPatterns[0].Index != 0 || uie.UnmatchedPatterns[0].Pattern != "o1-.*" {
		t.Fatalf("want {Index:0, Pattern:\"o1-.*\"}, got %+v", uie.UnmatchedPatterns[0])
	}
}

// ---- TestApply_ExcludeLenient_ZeroMatchIsSilent ------------------------

// MDISC-08 / spec line 823: exclude matching nothing is a silent no-op.
func TestApply_ExcludeLenient_ZeroMatchIsSilent(t *testing.T) {
	in := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Exclude: []string{"o1-.*"},
	})
	wantErr(t, err, "")
	if !equalSliceLoose(got, in) {
		t.Fatalf("want %v, got %v", in, got)
	}
}

// ---- TestApply_InvalidRegex_ReturnsInvalidConfigError ------------------

// MDISC-09 / spec line 824: RE2 compile error → InvalidConfigError with
// Location ∈ {"include", "exclude"} + Index + Pattern + wrapped Err.
func TestApply_InvalidRegex_ReturnsInvalidConfigError(t *testing.T) {
	in := []string{"claude-3"}

	// Include variant.
	_, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"[invalid"},
	})
	wantErr(t, err, "*filters.InvalidConfigError")
	var ice *filters.InvalidConfigError
	if !errors.As(err, &ice) {
		t.Fatalf("errors.As(*InvalidConfigError) failed for include: %v", err)
	}
	if ice.Location != "include" {
		t.Fatalf("want Location=\"include\", got %q", ice.Location)
	}
	if ice.Index != 0 {
		t.Fatalf("want Index=0, got %d", ice.Index)
	}
	if ice.Pattern != "[invalid" {
		t.Fatalf("want Pattern=\"[invalid\", got %q", ice.Pattern)
	}
	if ice.Err == nil {
		t.Fatalf("want non-nil wrapped Err, got nil")
	}
	// errors.Unwrap must surface the underlying regexp compile error
	// so the reconciler can format spec §6.3 line 824's message verbatim.
	if unwrapped := errors.Unwrap(ice); unwrapped == nil {
		t.Fatalf("want non-nil errors.Unwrap, got nil")
	}

	// Exclude variant — same bad pattern under Exclude must report
	// Location="exclude". This proves the Location field actually
	// reflects which side the offender came from (not hard-coded).
	_, err = filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Exclude: []string{"[invalid"},
	})
	wantErr(t, err, "*filters.InvalidConfigError")
	if !errors.As(err, &ice) {
		t.Fatalf("errors.As(*InvalidConfigError) failed for exclude: %v", err)
	}
	if ice.Location != "exclude" {
		t.Fatalf("want Location=\"exclude\", got %q", ice.Location)
	}
}

// ---- TestApply_OrderDivergenceFromAutoconfig ---------------------------

// THE LOAD-BEARING TEST.
//
// Locks the spec's include-FIRST-then-exclude order against
// autoconfig's reverse order at
// /home/jcm/Projects/mcp/litellm-autoconfig/src/generator.py:324.
//
// The chosen inputs/filters intentionally make the autoconfig order
// (exclude FIRST, then include) produce a DIFFERENT outcome than the
// spec order (include FIRST, then exclude):
//
//	Inputs: ["claude-3-haiku-20240307"]
//	Filters.Include: ["claude-3-haiku-20240307"]
//	Filters.Exclude: ["claude-3-haiku-.*"]
//
//	Spec order (include FIRST):
//	 - include "claude-3-haiku-20240307" matches the one candidate
//	 (include hit count = 1, NOT zero → no UpstreamInvalid).
//	 - exclude "claude-3-haiku-.*" then removes it.
//	 - FINAL kept = []. err = nil.
//
//	Autoconfig order (exclude FIRST):
//	 - exclude "claude-3-haiku-.*" removes the one candidate first.
//	 - include "claude-3-haiku-20240307" then matches against [].
//	 - include hit count = 0 → would return UpstreamInvalid.
//
// The two orders diverge on the error-vs-no-error axis, which is the
// most observable divergence (the reconciler maps UpstreamInvalid to
// Ready=False; spec order leaves Ready=True with an empty child set).
//
// If a future change to filters.Apply silently swaps to autoconfig's
// order, this test fails on the err != nil branch (UpstreamInvalid
// instead of nil). The autoconfig reference is named in the comment
// above so the next reader knows what they're protecting.
func TestApply_OrderDivergenceFromAutoconfig(t *testing.T) {
	in := []string{"claude-3-haiku-20240307"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"claude-3-haiku-20240307"},
		Exclude: []string{"claude-3-haiku-.*"},
	})
	wantErr(t, err, "")
	if !equalSliceLoose(got, []string{}) {
		t.Fatalf("want empty kept (spec order: include hits then exclude removes), got %v", got)
	}
}

// ---- TestApply_MultipleIncludePatterns_UnionSemantics ------------------

// Spec line 814: matches_any returns true if AT LEAST ONE pattern matches.
//
// Include union: a candidate that matches ANY one of the include
// patterns is kept.
func TestApply_MultipleIncludePatterns_UnionSemantics(t *testing.T) {
	in := []string{"claude-3", "gpt-4", "gemini-pro"}
	want := []string{"claude-3", "gpt-4"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"claude.*", "gpt.*"},
	})
	wantErr(t, err, "")
	if !equalSliceLoose(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// ---- TestApply_MultipleExcludePatterns_UnionSemantics ------------------

// Symmetric to include union: a candidate that matches ANY one of the
// exclude patterns is dropped.
func TestApply_MultipleExcludePatterns_UnionSemantics(t *testing.T) {
	in := []string{"claude-3", "claude-2", "claude-instant"}
	want := []string{"claude-3"}
	got, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Exclude: []string{"claude-2.*", "claude-instant"},
	})
	wantErr(t, err, "")
	if !equalSliceLoose(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// ---- TestApply_DeterministicOrder --------------------------------------

// "Filter function is set-stable" (must_haves): applying the same
// filter twice to the same input yields IDENTICAL output. Guards
// against map-iteration ordering leaks in the implementation (e.g. if
// the implementer uses a `map[string]bool` set during matching).
func TestApply_DeterministicOrder(t *testing.T) {
	in := []string{"claude-3", "gpt-4", "gemini-pro", "claude-2", "gpt-3.5"}
	f := &litellmv1alpha1.ModelDiscoveryFilters{Include: []string{"c.*", "g.*"}}

	got1, err := filters.Apply(in, f)
	wantErr(t, err, "")
	got2, err := filters.Apply(in, f)
	wantErr(t, err, "")
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("non-deterministic Apply: run1=%v run2=%v", got1, got2)
	}
	// Order MUST preserve input order — input "claude-3" before
	// "gpt-4" before "gemini-pro" → kept order matches.
	want := []string{"claude-3", "gpt-4", "gemini-pro", "claude-2", "gpt-3.5"}
	if !equalSliceLoose(got1, want) {
		t.Fatalf("want input-order-preserving kept %v, got %v", want, got1)
	}
}

// ---- TestApply_IncludeStrict_MultipleUnmatchedPatternsReported ---------
//
// Bonus coverage: spec line 822 says "lists every offending pattern".
// When two of three include patterns match zero candidates, the
// UpstreamInvalidError payload must enumerate BOTH (in source order).
func TestApply_IncludeStrict_MultipleUnmatchedPatternsReported(t *testing.T) {
	in := []string{"claude-3"}
	_, err := filters.Apply(in, &litellmv1alpha1.ModelDiscoveryFilters{
		Include: []string{"claude.*", "o1-.*", "gemini-.*"},
	})
	wantErr(t, err, "*filters.UpstreamInvalidError")
	var uie *filters.UpstreamInvalidError
	if !errors.As(err, &uie) {
		t.Fatalf("errors.As(*UpstreamInvalidError) failed: %v", err)
	}
	if len(uie.UnmatchedPatterns) != 2 {
		t.Fatalf("want 2 unmatched patterns, got %d (%+v)", len(uie.UnmatchedPatterns), uie.UnmatchedPatterns)
	}
	if uie.UnmatchedPatterns[0].Index != 1 || uie.UnmatchedPatterns[0].Pattern != "o1-.*" {
		t.Fatalf("want first unmatched at Index=1 Pattern=\"o1-.*\", got %+v", uie.UnmatchedPatterns[0])
	}
	if uie.UnmatchedPatterns[1].Index != 2 || uie.UnmatchedPatterns[1].Pattern != "gemini-.*" {
		t.Fatalf("want second unmatched at Index=2 Pattern=\"gemini-.*\", got %+v", uie.UnmatchedPatterns[1])
	}
}
