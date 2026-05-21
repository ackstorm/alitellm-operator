// SPDX-License-Identifier: Apache-2.0

package normalize

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Pre-compiled regexes for the 5-step pipeline. Compiled once at
// package init so each Normalize call is allocation-light.
var (
	// dashCharsRe is the step-2 character class: /, :, _, space → "-"
	// (spec §6.3 line 745). These are the four characters most
	// commonly seen in provider feeds (Bedrock colons, Anthropic
	// underscores in display-name fallbacks, OpenAI slashes from
	// org-scoped routes).
	dashCharsRe = regexp.MustCompile(`[/:_ ]`)

	// invalidCharsRe is the step-3 character class: anything not in
	// the allowed set [a-z0-9.-] → "-" (spec §6.3 line 746). Dots
	// are allowed because DNS-1123 subdomains contain dots between
	// segments; dashes are allowed as the only intra-segment punct.
	invalidCharsRe = regexp.MustCompile(`[^a-z0-9.-]`)

	// consecutiveDashesRe matches runs of two or more dashes (spec
	// §6.3 line 747). Collapsing keeps the normalized form readable
	// after steps 2 and 3 over-replace.
	consecutiveDashesRe = regexp.MustCompile(`-+`)

	// leadingTrailingNonAlnumRe strips leading and trailing
	// characters that are not [a-z0-9] (spec §6.3 line 748). Dots
	// and dashes at the boundary are stripped — DNS-1123 forbids
	// labels that start or end with a non-alnum character.
	leadingTrailingNonAlnumRe = regexp.MustCompile(`^[^a-z0-9]+|[^a-z0-9]+$`)
)

// Normalize applies the 5-step pipeline (spec §6.3 lines 743-751) to a
// raw provider-side model ID and returns the normalized form suitable
// for use as a DNS-1123 subdomain segment.
//
// The result MAY be empty (e.g. if the input is "----" — all bytes
// become dashes, collapse to one, and the final trim strips it). The
// caller is responsible for passing the result through `DNS1123Subdomain`
// to catch the empty-result case (which surfaces in the reconciler as
// `status.skippedCandidates[reason=InvalidDiscoveredName]`).
//
// Raw IDs are NOT modified by this function — the input string is
// untouched. Callers that need the raw ID for `child.spec.params.model`
// (MDISC-10) MUST keep their original reference; this function returns
// the K8s-name form only.
func Normalize(rawID string) string {
	s := strings.ToLower(rawID)
	s = dashCharsRe.ReplaceAllString(s, "-")
	s = invalidCharsRe.ReplaceAllString(s, "-")
	s = consecutiveDashesRe.ReplaceAllString(s, "-")
	s = leadingTrailingNonAlnumRe.ReplaceAllString(s, "")
	return s
}

// DNS1123Subdomain validates that `name` is a well-formed RFC 1123
// subdomain (lowercase, segments 1.63 chars, total 1.253 chars,
// each segment matching `[a-z0-9]([-a-z0-9]*[a-z0-9])?`).
//
// Returns nil on success. On failure, returns a wrapped error of the
// form `invalid DNS-1123 subdomain <name>: <messages.>` so the
// reconciler can surface the input name (and the apimachinery
// diagnostic) verbatim in `status.skippedCandidates[].message` per
// spec §6.3 line 762 (MDISC-11).
//
// Delegates to k8s.io/apimachinery's `validation.IsDNS1123Subdomain`
// rather than re-implementing the regex — that package is already
// vendored and is the canonical source of truth for K8s naming
// constraints across this codebase (matches the same validation the
// apiserver runs at admission time).
func DNS1123Subdomain(name string) error {
	errs := validation.IsDNS1123Subdomain(name)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid DNS-1123 subdomain %q: %s", name, strings.Join(errs, "; "))
}
