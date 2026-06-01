// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// TestProbeVanishedResourceID_NotFoundClassification is the #56 regression:
// a typed *RejectedError{Status:404} (e.g. a fronting proxy or LiteLLM
// upgrade returning HTTP 404 on the list/get endpoint) must be recognized
// as "vanished" (clear=true), not fall through to the generic error arm.
// The sentinel ErrNotFound and a generic error are also asserted.
func TestProbeVanishedResourceID_NotFoundClassification(t *testing.T) {
	mk := func(err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return "", err }
	}

	t.Run("typed RejectedError 404 → clear", func(t *testing.T) {
		clear, err := probeVanishedResourceID(context.Background(), "old-id",
			mk(&litellm.RejectedError{Status: 404, Path: "/v1/agents"}), nil, logr.Discard(), "TestKind")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !clear {
			t.Fatal("RejectedError{404} must clear the ID (treated as vanished)")
		}
	})

	t.Run("sentinel ErrNotFound → clear", func(t *testing.T) {
		clear, err := probeVanishedResourceID(context.Background(), "old-id",
			mk(litellm.ErrNotFound), nil, logr.Discard(), "TestKind")
		if err != nil || !clear {
			t.Fatalf("ErrNotFound must clear; got clear=%v err=%v", clear, err)
		}
	})

	t.Run("other error → propagate, no clear", func(t *testing.T) {
		boom := errors.New("boom")
		clear, err := probeVanishedResourceID(context.Background(), "old-id",
			mk(boom), nil, logr.Discard(), "TestKind")
		if clear {
			t.Fatal("a generic error must NOT clear the ID")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("generic error must propagate; got %v", err)
		}
	})
}
