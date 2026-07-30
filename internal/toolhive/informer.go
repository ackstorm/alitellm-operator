// SPDX-License-Identifier: Apache-2.0

// Package toolhive — see types.go for the package-level doc comment.
//
// This file implements Informer and helpers.
//
// # Registration
//
// The Informer registers 3 dynamic informers, one per kind (MCPServer,
// VirtualMCPServer, MCPRemoteProxy) at v1beta1. Registration is attempted
// per-GVK — a missing CRD for one kind does not prevent the others from
// registering — but the Informer only reports Ready once all three succeed,
// because upstream ships all three in the same toolhive-operator-crds chart.
package toolhive

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// DefaultRetryInterval is the default cadence at which the Informer
// retries registration when ToolHive CRDs are absent. Per Phase 5 D-08:
// "Retry registration on a 1-minute background ticker."
const DefaultRetryInterval = 1 * time.Minute

// ErrNotReady is returned by Informer.List when the Informer has not
// yet successfully registered its dynamic informers. Callers
// (MCPServerDiscoveryReconciler) MUST translate this into a
// `Ready=False, reason=SourceUnreachable` condition on the affected
// MCPServerDiscovery CR (per spec §6.5).
//
// The message names v1beta1 deliberately. Since v1alpha1 support was
// dropped, "CRDs absent" is no longer the only cause — a cluster on a
// pre-0.41.0 toolhive-operator-crds chart has the CRDs Established but
// serves v1alpha1 ONLY, so an admin who reads "CRDs may be absent" runs
// `kubectl get crd mcpservers.toolhive.stacklok.dev`, sees it present, and
// has nowhere to go next.
var ErrNotReady = errors.New(
	"toolhive: no informer registered for toolhive.stacklok.dev/v1beta1 " +
		"(CRDs absent, or installed at v1alpha1 only — requires toolhive-operator-crds >= 0.41.0)")

// Informer is the cluster-scoped dynamic-informer wrapper for ToolHive
// CRs. It satisfies controller-runtime's manager.Runnable interface so
// it can be added via mgr.Add and started alongside the manager.
//
// Lifecycle (Phase 5 D-08):
//
// 1. cmd/main.go (or suite_test.go) constructs an Informer pointing
// at the manager.
// 2. mgr.Add(informer) is called BEFORE mgr.Start.
// 3. When mgr.Start runs, it invokes Informer.Start(ctx). Start
// attempts an initial registration synchronously; on failure it
// spawns a goroutine that retries every RetryInterval (default 1m)
// until either success or ctx cancellation. Start ALWAYS returns nil
// (non-blocking) so manager.Setup does not crash on absent CRDs.
// 4. After successful registration (each kind ready), IsReady
// transitions to true. Downstream consumers gate calls to List on
// IsReady.
type Informer struct {
	// Manager is the controller-runtime Manager whose cache the
	// Informer registers against. Required.
	Manager manager.Manager

	// Log is the logger used for registration-attempt diagnostics. If
	// zero-value, a discarded logger is substituted in Start.
	Log logr.Logger

	// RetryInterval overrides DefaultRetryInterval. Tests inject a
	// shorter value (e.g. 200ms) to keep total test wall time bounded.
	// Production wiring leaves this zero; Start substitutes
	// DefaultRetryInterval.
	RetryInterval time.Duration

	// ready is set to true once tryRegister reports that every kind is
	// registered. Atomic for lock-free reads from List + IsReady.
	ready atomic.Bool

	// readyMu protects per-kind ready tracking.
	readyMu sync.Mutex
	// kindReady tracks whether a kind's informer is registered.
	// Keys: "MCPServer", "VirtualMCPServer", "MCPRemoteProxy".
	kindReady map[string]bool

	// registered records the GVKs successfully passed through
	// GetCache().GetInformer in tryRegister. Read via RegisteredGVKs()
	// under readyMu (shared with kindReady). FIX2.txt LOW-11 diagnostic
	// surface (2026-05-22): lets tests + the startup summary log assert
	// exactly which GVKs were registered.
	registered []schema.GroupVersionKind
}

// gvkStrings renders a slice of GVKs as "<group>/<version>/<kind>" strings
// for structured-log emission. Stable order = caller's order.
func gvkStrings(gvks []schema.GroupVersionKind) []string {
	out := make([]string, 0, len(gvks))
	for _, g := range gvks {
		out = append(out, g.String())
	}
	return out
}

// RegisteredGVKs returns a snapshot of GVKs the Informer has registered
// dynamic informers for. Read-only; safe for concurrent use.
// FIX2.txt LOW-11 (2026-05-22).
func (i *Informer) RegisteredGVKs() []schema.GroupVersionKind {
	i.readyMu.Lock()
	defer i.readyMu.Unlock()
	out := make([]schema.GroupVersionKind, len(i.registered))
	copy(out, i.registered)
	return out
}

// Start satisfies controller-runtime's manager.Runnable interface.
//
// Behavior: Start attempts registration once synchronously. On success
// (all kinds ready), it stores ready=true and returns nil. On failure,
// it spawns a retry goroutine that loops until either tryRegister
// succeeds or ctx is cancelled, then returns nil immediately.
//
// Start NEVER blocks the manager's Setup phase. Per Phase 5 D-08, an
// absent ToolHive CRD-set MUST NOT crash manager startup.
func (i *Informer) Start(ctx context.Context) error {
	if i.RetryInterval == 0 {
		i.RetryInterval = DefaultRetryInterval
	}
	if i.Log == (logr.Logger{}) {
		i.Log = logr.Discard()
	}

	// Reset both together: tryRegister now skips via kindReady, so leaving a
	// stale `registered` behind would let a second Start append duplicates and
	// make RegisteredGVKs()/the startup log report 6 GVKs for 3 kinds.
	i.kindReady = make(map[string]bool)
	i.registered = nil

	// Try once synchronously. The first attempt's outcome is
	// immediately visible to IsReady, so downstream consumers can
	// short-circuit if registration succeeded at startup.
	if i.tryRegister(ctx) {
		i.ready.Store(true)
		i.Log.Info("toolhive informers registered at startup",
			"gvks", gvkStrings(i.RegisteredGVKs()),
			"count", len(i.RegisteredGVKs()))
		return nil
	}

	// Same rationale as ErrNotReady: name the version, since a v1alpha1-only
	// cluster has the CRDs present but unusable. Emitted once at startup; the
	// per-GVK detail stays at V(1) so the 1-minute retry loop cannot spam.
	i.Log.Info("no toolhive.stacklok.dev/v1beta1 informer registered at startup "+
		"(CRDs absent, or installed at v1alpha1 only — requires toolhive-operator-crds >= 0.41.0) "+
		"— retrying in background",
		"retryInterval", i.RetryInterval.String())

	// Spawn a goroutine that retries until success or ctx cancellation.
	go i.retryLoop(ctx)
	return nil
}

// retryLoop runs in a goroutine spawned by Start. It blocks on a ticker
// until either tryRegister succeeds (Ready flips to true and the loop
// exits) or ctx is cancelled (clean shutdown).
func (i *Informer) retryLoop(ctx context.Context) {
	ticker := time.NewTicker(i.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if i.tryRegister(ctx) {
				i.ready.Store(true)
				i.Log.Info("toolhive informers registered after retry",
					"gvks", gvkStrings(i.RegisteredGVKs()),
					"count", len(i.RegisteredGVKs()))
				return
			}
			i.Log.V(1).Info("toolhive informers still unregistered — will retry")
		}
	}
}

// tryRegister attempts to register a dynamic informer per discoverable
// kind. Each GVK is tried independently; a failure for one does not
// prevent the others from being registered. Returns true once all three
// kinds are registered.
//
// The pattern is controller-runtime's recommended dynamic-informer approach:
// construct an *unstructured.Unstructured with the GVK set, call
// mgr.GetCache().GetInformer(ctx, u). The handle is discarded here because
// the reconciler reads via mgr.GetClient().List on an UnstructuredList
// (the cache is shared between the informer and client).
//
// Error classes returned by GetInformer when CRDs are absent:
//   - "no kind \"MCPServer\" is registered for version
//     \"toolhive.stacklok.dev/v1beta1\" in scheme ."
//   - "no matches for kind \"MCPServer\" in version
//     \"toolhive.stacklok.dev/v1beta1\""
//   - apiextensions discovery errors during informer warm-up.
//
// All such errors mean "try again later". Per Phase 5 D-08, the Informer
// does NOT inspect apiextensions to determine whether CRDs exist.
func (i *Informer) tryRegister(ctx context.Context) bool {
	i.readyMu.Lock()
	defer i.readyMu.Unlock()

	for _, gvk := range discoverableGVKs {
		if i.kindReady[gvk.Kind] {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if _, err := i.Manager.GetCache().GetInformer(ctx, u); err != nil {
			i.Log.V(1).Info("toolhive informer registration failed",
				"version", gvk.Version,
				"kind", gvk.Kind,
				"err", err.Error())
			continue
		}
		i.Log.V(1).Info("toolhive informer registered", "gvk", gvk.String())
		i.kindReady[gvk.Kind] = true
		i.registered = append(i.registered, gvk)
	}

	// All three kinds ship in the same upstream toolhive-operator-crds
	// chart, so "ToolHive is installed" means all three CRDs exist.
	// MCPRemoteProxy gets no special case.
	return i.kindReady[MCPServerGVK.Kind] &&
		i.kindReady[VirtualMCPServerGVK.Kind] &&
		i.kindReady[MCPRemoteProxyGVK.Kind]
}

// IsReady reports whether the Informer has successfully registered an
// informer for every ToolHive kind. Atomic, lock-free read.
// MCPServerDiscoveryReconciler gates List calls on this — when false,
// the reconciler surfaces Ready=False, reason=SourceUnreachable on the
// affected MCPServerDiscovery CRs (MSDISC-06).
func (i *Informer) IsReady() bool {
	return i.ready.Load()
}

// List returns the cached objects for the given GVK, backed by the
// manager's shared cache.
//
// On success, returns an UnstructuredList of every object the informer has
// observed (cluster-scoped). The caller filters by
// `spec.toolhive.namespaces[]` in memory.
//
// Errors:
//   - ErrNotReady: the dynamic informers are not yet registered (e.g.
//     ToolHive CRDs are absent). The caller MUST translate this into a
//     `Ready=False, reason=SourceUnreachable` condition.
//   - Any error returned by mgr.GetClient().List (cache miss, list
//     decode error, etc.): the caller MUST translate this into a
//     transient `SourceReachable=False` condition and return
//     (ctrl.Result{}, err) for controller-runtime backoff. Surfacing an
//     empty list instead would let callers mistake a transient API outage
//     for "no objects exist" and prune all child CRs.
//
// Per Phase 5 D-07: the List is cluster-scoped (no namespace filter
// applied here); namespace filtering happens in-memory at the reconciler.
func (i *Informer) List(ctx context.Context, gvk schema.GroupVersionKind) (*unstructured.UnstructuredList, error) {
	if !i.IsReady() {
		return nil, ErrNotReady
	}

	listGVK := gvk
	listGVK.Kind = gvk.Kind + "List"

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	if err := i.Manager.GetClient().List(ctx, list); err != nil {
		return nil, err
	}
	return list, nil
}
