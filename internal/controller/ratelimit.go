// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// transientBackoffBaseDelay is the controller-runtime exponential
// rate-limiter base. Same value the controller-runtime default rate
// limiter uses.
const transientBackoffBaseDelay = 200 * time.Millisecond

// transientBackoffMaxDelay caps the per-item exponential backoff. FIX.txt
// MEDIUM-3 (2026-05-22): without a cap, a brief upstream-LiteLLM blip
// could push the next retry 5–10 min out under the default unbounded
// backoff, stranding CRs in the backoff queue even after the upstream
// recovered. 30s keeps recovery within a single user-perceptible window
// while leaving room for genuine transient-storm spacing.
const transientBackoffMaxDelay = 30 * time.Second

// transientBackoffOptions returns controller.Options with an exponential
// per-item rate limiter capped at transientBackoffMaxDelay. Used by every
// controller that talks to LiteLLM (mcpserver, model, a2aagent, team,
// mcpserverdiscovery, modeldiscovery).
//
// Non-transient errors (LiteLLMRejected/4xx, SecretNotFound, InvalidConfig)
// short-circuit upstream of the rate limiter via `return ctrl.Result{},
// nil`, so they never feed the exponential backoff path and the cap does
// not change their deterministic behavior.
func transientBackoffOptions() controller.Options {
	return controller.Options{
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
			transientBackoffBaseDelay, transientBackoffMaxDelay,
		),
	}
}
