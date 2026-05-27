// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsNotFound_* asserts the contract documented in errors.go: a single
// helper distinguishes "entry already absent in LiteLLM" from "other 4xx /
// 5xx / network" on both the list-style helpers (ErrNotFound sentinel) and
// the makeRequest path (*RejectedError with Status == 404).
//
// Post-2026-05-26 review finding F4 (model finalizer strand on 404).

func TestIsNotFound_ErrNotFound(t *testing.T) {
	if !IsNotFound(ErrNotFound) {
		t.Fatalf("IsNotFound(ErrNotFound) = false, want true")
	}
}

func TestIsNotFound_RejectedError404(t *testing.T) {
	rej := &RejectedError{Method: "GET", Path: "/model/info", Status: 404, Code: "404"}
	if !IsNotFound(rej) {
		t.Fatalf("IsNotFound(RejectedError{Status:404}) = false, want true")
	}
}

func TestIsNotFound_RejectedError400IsFalse(t *testing.T) {
	rej := &RejectedError{Method: "GET", Path: "/model/info", Status: 400, Code: "400"}
	if IsNotFound(rej) {
		t.Fatalf("IsNotFound(RejectedError{Status:400}) = true, want false")
	}
}

func TestIsNotFound_WrappedRejectedError404(t *testing.T) {
	rej := &RejectedError{Method: "GET", Path: "/model/info", Status: 404, Code: "404"}
	wrapped := fmt.Errorf("upstream: %w", rej)
	if !IsNotFound(wrapped) {
		t.Fatalf("IsNotFound(wrapped 404) = false, want true (errors.As must unwrap)")
	}
}

func TestIsNotFound_NilAndOther(t *testing.T) {
	if IsNotFound(nil) {
		t.Fatalf("IsNotFound(nil) = true, want false")
	}
	if IsNotFound(errors.New("dial tcp")) {
		t.Fatalf("IsNotFound(non-litellm error) = true, want false")
	}
}
