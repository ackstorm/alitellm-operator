// SPDX-License-Identifier: Apache-2.0

// Package conflict resolves cross-CR ownership of a single LiteLLM-side
// natural key. The rule is fixed and applies to every controller that
// has a natural-key collision surface: sort the candidate CRs by
// "<namespace>/<name>" in lexicographic ascending order; the LAST
// element wins; every other candidate is a loser and must short-circuit
// its reconcile with Ready=False, Reason=Conflict.
//
// See docs/concepts/conflict-resolution.md for the user-facing
// contract and rationale.
package conflict
