// SPDX-License-Identifier: Apache-2.0

// Package relist hosts the safety-relist drift-recovery envtest(s) that
// depend on the background SafetyRelistRunnable actually firing.
//
// They live in their own package so each runs in an isolated `go test`
// process (its own envtest apiserver + manager) with no neighbor-test
// workqueue / apiserver contention. This eliminates the #74
// -race -shuffle release-gate flake (TestGuardRail_SafetyRelist_CreateMissing):
// in the shared controller suite the recovery POST competed with the
// 100ms full-LIST relist flood and ~290 other tests hitting the SAME
// apiserver, so recovery slipped past even a 90s budget. Here the only
// CR present is the test's own, so recovery lands in ~2-3s deterministically.
package relist
