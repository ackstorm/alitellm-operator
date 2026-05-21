// SPDX-License-Identifier: Apache-2.0

package filters

import (
	"fmt"
	"regexp"
	"strings"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// UnmatchedPattern is the index + verbatim source string of an
// include pattern that compiled successfully but matched zero
// candidates in the current refresh. Used inside
// UpstreamInvalidError so the reconciler can format
// `filters.include[<Index>] (<Pattern>) matched no upstream models`
// per spec §6.3 line 822.
type UnmatchedPattern struct {
	Index   int
	Pattern string
}

// UpstreamInvalidError is returned when one or more include patterns
// compile successfully but match zero upstream candidates. The
// reconciler maps this to:
//
//	Ready=False, reason=UpstreamInvalid,
//	message: "filters.include[<idx>] (<pattern>) matched no upstream models"
//	 joined with "; " across all unmatched patterns.
//
// Per spec §6.3 line 822 + MDISC-07.
type UpstreamInvalidError struct {
	UnmatchedPatterns []UnmatchedPattern
}

// Error implements error. Returns one segment per unmatched pattern,
// joined with "; ". Stable across runs (input-order preserved).
func (e *UpstreamInvalidError) Error() string {
	if e == nil || len(e.UnmatchedPatterns) == 0 {
		return "filters: include matched no upstream models"
	}
	parts := make([]string, 0, len(e.UnmatchedPatterns))
	for _, p := range e.UnmatchedPatterns {
		parts = append(parts, fmt.Sprintf("filters.include[%d] (%s) matched no upstream models", p.Index, p.Pattern))
	}
	return strings.Join(parts, "; ")
}

// InvalidConfigError is returned when an include or exclude pattern
// fails Go RE2 compilation. The reconciler maps this to:
//
//	Ready=False, reason=InvalidConfig,
//	message: "filters.<Location>[<Index>]: invalid regex: <Err>"
//
// Per spec §6.3 line 824 + MDISC-09. The wrapped Err is the verbatim
// regexp.Compile failure so the reconciler can surface the Go
// compile diagnostic to the user.
type InvalidConfigError struct {
	Location string // "include" | "exclude"
	Index    int
	Pattern  string
	Err      error
}

// Error implements error using the spec §6.3 line 824 message
// template verbatim.
func (e *InvalidConfigError) Error() string {
	return fmt.Sprintf("filters.%s[%d]: invalid regex: %v", e.Location, e.Index, e.Err)
}

// Unwrap exposes the underlying regexp.Compile error so callers can
// use errors.Is / errors.As against regexp.Error if needed.
func (e *InvalidConfigError) Unwrap() error { return e.Err }

// Apply filters the raw upstream candidate list using the spec's
// filters block. Returns the kept set (in input order, deterministic)
// and an error.
//
// Semantics (spec §6.3 lines 810-825 + MDISC-06.09):
//
// 1. nil filters block → return (candidates, nil) — pass-through.
// 2. Compile each include pattern with implicit "^" anchor
// (anchored-from-start). On compile failure: return
// *InvalidConfigError{Location:"include",.} BEFORE any
// exclude pattern is compiled.
// 3. Compile each exclude pattern. On compile failure: return
// *InvalidConfigError{Location:"exclude",.}.
// 4. Apply include step: kept = {id | matchesAny(id, includeRes)},
// or all candidates if includeRes is empty (empty-equals-absent).
// Track which include patterns matched ≥1 candidate. After the
// iteration, if any include pattern matched zero, return
// *UpstreamInvalidError{UnmatchedPatterns:.} naming every
// offender (in source order).
// 5. Apply exclude step: kept = {id ∈ kept | NOT matchesAny(id,
// excludeRes)}. No empty-match error path (MDISC-08).
// 6. Return (kept, nil).
//
// On error, the caller MUST treat the (nil, err) return as "abort
// this refresh" — no partial work is reported.
//
// Patterns are NOT cached across calls; the reconciler invokes Apply
// once per spec.refresh.interval (≥1m floor) and the per-call
// regexp.Compile cost is well within budget at the expected pattern
// counts (<10 typical). Caching would introduce thread-safety burden
// for no measurable win.
func Apply(candidates []string, f *litellmv1alpha1.ModelDiscoveryFilters) ([]string, error) {
	if f == nil {
		return candidates, nil
	}

	// Step 1: compile includes (anchored-from-start).
	includeRes, err := compileAnchored(f.Include, "include")
	if err != nil {
		return nil, err
	}

	// Apply include step BEFORE compiling excludes. This preserves
	// the spec's ordering precedence — an empty include set is
	// treated as "admit all" without checking exclude regex
	// validity yet (validity check fires on the exclude compile).
	//
	// Compile excludes AFTER include matches so that an invalid
	// exclude regex paired with a passing include still surfaces
	// InvalidConfigError (not UpstreamInvalidError) — both are
	// operator-side errors, and InvalidConfig is the spec's
	// reserved reason for compile failures (line 824).
	excludeRes, err := compileAnchored(f.Exclude, "exclude")
	if err != nil {
		return nil, err
	}

	// Step 4: include narrowing.
	var kept []string
	if len(includeRes) == 0 {
		// Empty include == absent: pass-through.
		kept = append(kept, candidates...)
	} else {
		matchedIncludes := make([]bool, len(includeRes))
		for _, id := range candidates {
			if matchAnyAndTrack(id, includeRes, matchedIncludes) {
				kept = append(kept, id)
			}
		}
		// Strict include: surface every unmatched pattern (in
		// source order) so the reconciler reports the full
		// offender list per spec §6.3 line 822.
		var unmatched []UnmatchedPattern
		for i, hit := range matchedIncludes {
			if !hit {
				unmatched = append(unmatched, UnmatchedPattern{
					Index:   i,
					Pattern: f.Include[i],
				})
			}
		}
		if len(unmatched) > 0 {
			return nil, &UpstreamInvalidError{UnmatchedPatterns: unmatched}
		}
	}

	// Step 5: exclude carving (lenient — no zero-match error).
	if len(excludeRes) == 0 {
		return kept, nil
	}
	out := make([]string, 0, len(kept))
	for _, id := range kept {
		if !matchesAny(id, excludeRes) {
			out = append(out, id)
		}
	}
	return out, nil
}

// compileAnchored compiles each user pattern with an implicit "^"
// prefix via regexp.Compile (NOT MustCompile — these are runtime
// user inputs). Returns *InvalidConfigError on the first failure,
// naming the location (include|exclude), index, and pattern.
func compileAnchored(patterns []string, location string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	res := make([]*regexp.Regexp, 0, len(patterns))
	for i, pat := range patterns {
		re, err := regexp.Compile("^" + pat)
		if err != nil {
			return nil, &InvalidConfigError{
				Location: location,
				Index:    i,
				Pattern:  pat,
				Err:      err,
			}
		}
		res = append(res, re)
	}
	return res, nil
}

// matchesAny is the matches_any predicate from spec §6.3 line 814:
// returns true if s matches AT LEAST ONE compiled pattern (linear
// scan with short-circuit). O(N*M) where N=candidates, M=patterns.
func matchesAny(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// matchAnyAndTrack is matchesAny + per-pattern hit-bookkeeping. Marks
// matched[i]=true for every pattern that matches s. Used during the
// include step so we can later report the UNMATCHED include patterns
// in the UpstreamInvalidError payload. Does NOT short-circuit (we
// need full hit coverage across all patterns).
func matchAnyAndTrack(s string, res []*regexp.Regexp, matched []bool) bool {
	any := false
	for i, re := range res {
		if re.MatchString(s) {
			matched[i] = true
			any = true
		}
	}
	return any
}
