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

// TestRejectedError_TypeFieldPropagates — LOW-02. The Type field on
// RejectedError must round-trip the envelope's error.type so the
// controller layer can surface it in condition.Message without
// re-parsing the body or enabling the dangerously-include-body opt-in.
func TestRejectedError_TypeFieldPropagates(t *testing.T) {
	body := []byte(`{"error":{"message":"bad","type":"validation_error","param":"model","code":"422"}}`)
	kind, msg, code := processLitellmError(body)
	if kind != "validation_error" {
		t.Fatalf("kind: want validation_error, got %q", kind)
	}
	if msg != "bad" || code != "422" {
		t.Fatalf("unexpected msg/code: %q/%q", msg, code)
	}

	rej := &RejectedError{
		Method:  "POST",
		Path:    "/model/new",
		Status:  422,
		Code:    code,
		Type:    kind,
		Message: msg,
	}
	if rej.Type != "validation_error" {
		t.Fatalf("RejectedError.Type: want validation_error, got %q", rej.Type)
	}
	// Error() string MUST stay unchanged — existing prefix matchers
	// (is4xxNon401Status) depend on the exact format.
	wantErr := "litellm: 422 on POST /model/new (code=422)"
	if got := rej.Error(); got != wantErr {
		t.Fatalf("Error() shape regressed:\n  want: %q\n  got:  %q", got, wantErr)
	}
}

func TestProcessLitellmError_TypeOnlyEnvelope_KeepsType(t *testing.T) {
	body := []byte(`{"error":{"type":"not_found_error","message":"","code":""}}`)
	kind, _, _ := processLitellmError(body)
	if kind != "not_found_error" {
		t.Errorf("type-only envelope: want kind=not_found_error, got %q", kind)
	}
}

func TestProcessLitellmError_UnparseableStaysUnparsed(t *testing.T) {
	// An unparseable body must yield the kindUnparsed sentinel — NOT a
	// spurious real type. (makeRequest drops kindUnparsed → "" before it
	// reaches CR status; see client.go.)
	kind, _, _ := processLitellmError([]byte(`<html>500</html>`))
	if kind != kindUnparsed {
		t.Errorf("unparseable body: want kindUnparsed (%q), got %q", kindUnparsed, kind)
	}
}
