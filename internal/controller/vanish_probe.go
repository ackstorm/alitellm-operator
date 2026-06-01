// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// probeVanishedResourceID checks whether an externally-tracked LiteLLM
// resource (Model, MCPServer, Team, A2AAgent) still exists in LiteLLM
// at the ID a CR's status pins. Used by every safety-relist sweep to
// detect out-of-band deletes / id-drift that the operator-side hash
// cache would otherwise mask.
//
// `lookup` is the caller-supplied closure that performs the LiteLLM
// LIST + name filter (or GET-by-name) and returns the CURRENT ID under
// that name. Contract:
//
//   - ("", nil)                  → resource vanished from LiteLLM
//   - (id, nil) where id==lastID → resource present with same ID (no clear)
//   - (id, nil) where id!=lastID → id drift (clear; CREATE/UPDATE re-targets)
//   - (_, Auth401Error)          → invalidate() + leave lastID intact
//   - (_, ErrNotFound)           → treat as vanished
//   - (_, other err)             → propagate for controller-runtime backoff
//
// Returns (clear=true) when the caller should clear its stored lastID
// so the downstream branch falls through to CREATE / UPDATE-re-target.
//
// v0.4.5: introduced to deduplicate the per-controller inline blocks
// that previously implemented this pattern in MCPServer, Team, and
// A2AAgent reconcilers. Model carries its own pre-existing Step 7b
// (GetModelInfoByName) and is intentionally not migrated to keep the
// v0.4.5 surface area minimal.
func probeVanishedResourceID(
	ctx context.Context,
	lastID string,
	lookup func(ctx context.Context) (string, error),
	invalidate func(),
	logger logr.Logger,
	resourceKind string,
) (clear bool, err error) {
	resolvedID, probeErr := lookup(ctx)
	if probeErr != nil {
		var auth401 *litellm.Auth401Error
		if errors.As(probeErr, &auth401) {
			if invalidate != nil {
				invalidate()
			}
			logger.Info("vanish-probe: 401 fast-path; leaving ID intact",
				"kind", resourceKind, "path", auth401.Path)
			return false, nil
		}
		// #56: use litellm.IsNotFound so a typed *RejectedError{Status:404}
		// (e.g. a fronting proxy or LiteLLM upgrade returning HTTP 404 on the
		// list/get endpoint) is recognized as "vanished" — not just the
		// sentinel ErrNotFound from the 200-empty-body path.
		if litellm.IsNotFound(probeErr) {
			logger.Info("vanish-probe: LiteLLM reported not-found; clearing ID",
				"kind", resourceKind, "lastID", lastID)
			return true, nil
		}
		return false, fmt.Errorf("vanish-probe %s: %w", resourceKind, probeErr)
	}
	if resolvedID == "" {
		logger.Info("safety re-list detected out-of-band delete; clearing ID",
			"kind", resourceKind, "lastID", lastID)
		return true, nil
	}
	if resolvedID != lastID {
		logger.Info("safety re-list detected id drift; clearing ID",
			"kind", resourceKind, "lastID", lastID, "currentID", resolvedID)
		return true, nil
	}
	return false, nil
}
