// SPDX-License-Identifier: Apache-2.0

// Package toolhive — see types.go for the package-level doc comment.
//
// This file implements Informer, dedupStore, and helpers.
//
// # Dual-version registration
//
// The Informer registers 6 dynamic informers: v1alpha1 and v1beta1 for each
// of MCPServer, VirtualMCPServer and MCPRemoteProxy. Registration is attempted
// per-GVK — a missing CRD for one version does not prevent the other from
// registering. Each kind is considered "ready" once at least one of its two
// version informers has successfully registered.
//
// # Dedup store
//
// The dedupStore aggregates discovered objects from all 6 informers keyed by
// {kind, namespace, name}. On collision (same key from both v1alpha1 and
// v1beta1), v1alpha1 wins and the v1beta1 entry is logged at info level with
// dedup_reason=alpha_wins. This ensures identical List output for deployments
// with only v1alpha1 CRDs installed.
//
// # Error messages
//
// When a per-version informer fails to register, the error message includes
// the version that failed (e.g. "v1beta1 MCPServer informer: <err>").
package toolhive

import (
	"context"
	"errors"
	"fmt"
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
// yet successfully registered its dynamic informers (e.g. ToolHive
// CRDs are absent from the cluster). Callers (MCPServerDiscoveryReconciler)
// MUST translate this into a `Ready=False, reason=SourceUnreachable`
// condition on the affected MCPServerDiscovery CR with a message like
// "ToolHive CRDs not installed" (per spec §6.5).
var ErrNotReady = errors.New("toolhive: informers not yet registered (CRDs may be absent)")

// dedupKey uniquely identifies a ToolHive object across both versions.
// Group is intentionally omitted — both v1alpha1 and v1beta1 share the
// same group ("toolhive.stacklok.dev").
type dedupKey struct {
	Kind      string // "MCPServer", "VirtualMCPServer" or "MCPRemoteProxy"
	Namespace string
	Name      string
}

// versionedObj pairs an unstructured object with the API version it was
// discovered from ("v1alpha1" or "v1beta1").
type versionedObj struct {
	Obj     *unstructured.Unstructured
	Version string
}

// dedupLogWindow is the minimum interval between dedup INFO log emissions
// per (kind, namespace, name) tuple. FIX.txt LOW-7 (2026-05-22): pre-fix
// the dedup log fired once per reconcile per MCPServer (22 servers × poll
// interval = noisy). After the fix, emission at V(0) is capped at once
// per window; finer-grained traces remain available at V(2).
const dedupLogWindow = 60 * time.Second

// dedupSummaryWindow caps the coalesced summary INFO line at one emit
// per window. FIX2.txt M-4 (2026-05-22): even with the per-tuple
// throttle, 22 unique MCPServers produce 22 INFO lines/min — the
// documented contract, but operationally noisy. Demote per-tuple lines
// to V(2) and emit one coalesced summary line at V(0) per window.
const dedupSummaryWindow = 5 * time.Minute

// dedupLogThrottle records the last time the v1alpha1-wins INFO line was
// emitted for each (kind, namespace, name) tuple. Lives on the Informer
// (NOT on the per-List dedupStore) so the throttle persists across List
// calls — without persistence, every reconcile would re-emit because each
// List builds a fresh dedupStore.
type dedupLogThrottle struct {
	mu           sync.Mutex
	lastLoggedAt map[dedupKey]time.Time
}

func newDedupLogThrottle() *dedupLogThrottle {
	return &dedupLogThrottle{lastLoggedAt: make(map[dedupKey]time.Time)}
}

// shouldLog returns true iff at least `window` has elapsed since the last
// emission for `key`. On true, the call also records "now" as the new
// last-logged timestamp.
func (t *dedupLogThrottle) shouldLog(key dedupKey, window time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if last, ok := t.lastLoggedAt[key]; ok && now.Sub(last) < window {
		return false
	}
	t.lastLoggedAt[key] = now
	return true
}

// dedupStore is a thread-safe aggregating store for ToolHive objects
// discovered from multiple API versions. It implements the v1alpha1-wins
// dedup rule: when the same {kind, namespace, name} is observed from both
// v1alpha1 and v1beta1, the v1alpha1 instance is kept. The store records
// which keys collided in `collisions` so the caller can log at the
// appropriate verbosity / throttle (the store itself is verbosity-free).
type dedupStore struct {
	mu         sync.RWMutex
	items      map[dedupKey]versionedObj
	collisions []dedupKey // keys where v1alpha1 won over an incoming v1beta1
}

// newDedupStore constructs an empty dedupStore.
func newDedupStore() *dedupStore {
	return &dedupStore{
		items: make(map[dedupKey]versionedObj),
	}
}

// Upsert adds or updates obj in the store, applying the v1alpha1-wins
// dedup rule on collision.
func (s *dedupStore) Upsert(obj *unstructured.Unstructured) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := dedupKey{
		Kind:      obj.GetKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
	version := obj.GroupVersionKind().Version

	existing, found := s.items[key]
	if found && existing.Version != version {
		// Cross-version collision. v1alpha1 wins REGARDLESS of arrival order
		// (M-B6): the previous code only handled existing=v1alpha1 +
		// incoming=v1beta1, so when v1beta1 was stored first an incoming
		// v1alpha1 overwrote it WITHOUT recording the collision. Record the
		// collision in both directions; keep v1alpha1.
		s.collisions = append(s.collisions, key)
		if existing.Version == "v1alpha1" {
			// incoming (v1beta1) loses; keep the stored v1alpha1.
			return
		}
		// existing is v1beta1, incoming is v1alpha1 → v1alpha1 wins (store).
	}
	s.items[key] = versionedObj{Obj: obj, Version: version}
}

// List returns all stored objects matching the given kind (and optionally
// namespace — pass "" to return all namespaces). The returned slice is a
// snapshot; callers must not mutate the elements.
func (s *dedupStore) List(kind, namespace string) []*unstructured.Unstructured {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*unstructured.Unstructured, 0, len(s.items))
	for k, v := range s.items {
		if k.Kind != kind {
			continue
		}
		if namespace != "" && k.Namespace != namespace {
			continue
		}
		out = append(out, v.Obj)
	}
	return out
}

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
//
// The Informer registers FOUR unstructured informers (Phase 9, Task 09-07):
// one for each combination of {v1alpha1, v1beta1} × {MCPServer,
// VirtualMCPServer}. A kind is ready once at least one of its two version
// informers succeeds. Objects from both versions are aggregated via
// dedupStore with the v1alpha1-wins rule.
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

	// ready is set to true once tryRegister reports that each kind
	// (MCPServer + VirtualMCPServer) has at least one version ready.
	// Atomic for lock-free reads from List + IsReady.
	ready atomic.Bool

	// readyMu protects per-kind ready tracking.
	readyMu sync.Mutex
	// kindReady tracks whether at least one version per kind is registered.
	// Keys: "MCPServer", "VirtualMCPServer".
	kindReady map[string]bool

	// dedupThrottle persists "last logged at" per (kind, ns, name) tuple
	// across List() calls so the V(2) per-tuple line emits at most once
	// per dedupLogWindow even though every List builds a fresh
	// dedupStore. Lazily initialized on first List call (sync.Once).
	dedupThrottleOnce sync.Once
	dedupThrottle     *dedupLogThrottle

	// dedupSummaryMu + dedupSummaryLastLogged drive the V(0) coalesced
	// "dedup summary" line — emitted at most once per dedupSummaryWindow
	// regardless of how many tuples collided. FIX2.txt M-4.
	dedupSummaryMu         sync.Mutex
	dedupSummaryLastLogged time.Time

	// registered records the GVKs successfully passed through
	// GetCache().GetInformer in tryRegister. Read via RegisteredGVKs()
	// under readyMu (shared with kindReady). FIX2.txt LOW-11 diagnostic
	// surface (2026-05-22): lets tests + the startup summary log assert
	// exactly which GVKs were eagerly registered (vs. lazy-registered
	// via Client.List on first dedup pass).
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

// shouldLogDedupSummary returns true iff dedupSummaryWindow has elapsed
// since the last summary emission. Records "now" on true.
func (i *Informer) shouldLogDedupSummary() bool {
	i.dedupSummaryMu.Lock()
	defer i.dedupSummaryMu.Unlock()
	now := time.Now()
	if !i.dedupSummaryLastLogged.IsZero() && now.Sub(i.dedupSummaryLastLogged) < dedupSummaryWindow {
		return false
	}
	i.dedupSummaryLastLogged = now
	return true
}

// dedupCollisionExamples returns up to n unique "kind/ns/name" strings
// drawn from collisions, in the order encountered. Used to give an
// operator a fingerprint of which tuples are colliding without dumping
// a potentially long list.
func dedupCollisionExamples(collisions []dedupKey, n int) []string {
	if n <= 0 {
		return nil
	}
	seen := make(map[string]struct{}, n)
	out := make([]string, 0, n)
	for _, ck := range collisions {
		key := ck.Kind + "/" + ck.Namespace + "/" + ck.Name
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
		if len(out) >= n {
			break
		}
	}
	return out
}

// RegisteredGVKs returns a snapshot of GVKs the Informer has eagerly
// registered dynamic informers for. Read-only; safe for concurrent use.
// FIX2.txt LOW-11 (2026-05-22).
//
// Note: v1beta1 GVKs may not appear here even when v1beta1 objects
// are reachable via List — controller-runtime's cache lazily registers
// informers on first Client.List(unstructured) for a previously-unseen
// GVK. This accessor is the eager set only; for the full reachable
// set, exercise List for each candidate GVK.
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

	i.kindReady = make(map[string]bool)

	// Try once synchronously. The first attempt's outcome is
	// immediately visible to IsReady, so downstream consumers can
	// short-circuit if registration succeeded at startup.
	if i.tryRegister(ctx) {
		i.ready.Store(true)
		// FIX2.txt L-11 (2026-05-22): list every eagerly-registered GVK
		// honestly instead of just the canonical (alpha-wins) names.
		i.Log.Info("toolhive informers registered at startup",
			"gvks", gvkStrings(i.RegisteredGVKs()),
			"count", len(i.RegisteredGVKs()))
		return nil
	}

	i.Log.Info("toolhive CRDs absent at startup — retrying in background",
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

// tryRegister attempts to register all 6 dynamic informers. Each GVK is
// tried independently; a failure for one version does not prevent other
// versions from being registered. Returns true if each kind (MCPServer,
// VirtualMCPServer and MCPRemoteProxy) has at least one version successfully
// registered.
//
// The pattern is controller-runtime's recommended dynamic-informer approach:
// construct an *unstructured.Unstructured with the GVK set, call
// mgr.GetCache().GetInformer(ctx, u). The handle is discarded here because
// the reconciler reads via mgr.GetClient().List on an UnstructuredList
// (the cache is shared between the informer and client).
//
// Error classes returned by GetInformer when CRDs are absent:
//   - "no kind \"MCPServer\" is registered for version
//     \"toolhive.stacklok.dev/v1alpha1\" in scheme ."
//   - "no matches for kind \"MCPServer\" in version
//     \"toolhive.stacklok.dev/v1alpha1\""
//   - apiextensions discovery errors during informer warm-up.
//
// All such errors mean "try again later". Per Phase 5 D-08, the Informer
// does NOT inspect apiextensions to determine whether CRDs exist.
func (i *Informer) tryRegister(ctx context.Context) bool {
	// All 6 GVKs we want to register.
	gvks := []schema.GroupVersionKind{
		MCPServerGVKv1alpha1,
		MCPServerGVKv1beta1,
		VirtualMCPServerGVKv1alpha1,
		VirtualMCPServerGVKv1beta1,
		MCPRemoteProxyGVKv1alpha1,
		MCPRemoteProxyGVKv1beta1,
	}

	i.readyMu.Lock()
	defer i.readyMu.Unlock()

	// FIX2.txt L-11 (2026-05-22): attempt ALL 4 GVKs eagerly — no
	// short-circuit after the first success per kind. Previously the
	// loop did `if i.kindReady[kind] continue`, so once v1alpha1 was
	// registered v1beta1 stayed off the eager set. v1beta1 still worked
	// at runtime because controller-runtime lazy-registers an informer
	// on first Client.List(unstructured) for an unseen GVK, but the
	// startup audit log only saw v1alpha1. Eager registration makes
	// the audit log honest.
	already := make(map[schema.GroupVersionKind]bool, len(i.registered))
	for _, g := range i.registered {
		already[g] = true
	}
	for _, gvk := range gvks {
		if already[gvk] {
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
	return i.kindReady["MCPServer"] && i.kindReady["VirtualMCPServer"] && i.kindReady["MCPRemoteProxy"]
}

// IsReady reports whether the Informer has successfully registered at
// least one informer per ToolHive kind. Atomic, lock-free read.
// MCPServerDiscoveryReconciler gates List calls on this — when false,
// the reconciler surfaces Ready=False, reason=SourceUnreachable on the
// affected MCPServerDiscovery CRs (MSDISC-06).
func (i *Informer) IsReady() bool {
	return i.ready.Load()
}

// List returns the cached objects for the given GVK, backed by the
// manager's shared cache. Objects from both v1alpha1 and v1beta1 are
// aggregated; when the same {namespace, name} exists in both versions,
// the v1alpha1 instance is returned (v1alpha1-wins rule).
//
// The GVK parameter specifies the kind to query. Passing MCPServerGVK or
// MCPServerGVKv1alpha1 queries MCPServer objects from both versions.
// Passing VirtualMCPServerGVK or VirtualMCPServerGVKv1alpha1 queries
// VirtualMCPServer objects from both versions. Passing MCPRemoteProxyGVK
// or MCPRemoteProxyGVKv1alpha1 queries MCPRemoteProxy objects from both
// versions.
//
// On success, returns an UnstructuredList of every deduped object the
// informer has observed (cluster-scoped). The caller filters by
// `spec.toolhive.namespaces[]` in memory.
//
// Errors:
//   - ErrNotReady: the dynamic informers are not yet registered (e.g.
//     ToolHive CRDs are absent). The caller MUST translate this into a
//     `Ready=False, reason=SourceUnreachable` condition.
//   - Any error returned by mgr.GetClient().List (cache miss, list
//     decode error, etc.): the caller MUST translate this into a
//     transient `SourceReachable=False` condition and return
//     (ctrl.Result{}, err) for controller-runtime backoff.
//
// Per Phase 5 D-07: the List is cluster-scoped (no namespace filter
// applied here); namespace filtering happens in-memory at the reconciler.
func (i *Informer) List(ctx context.Context, gvk schema.GroupVersionKind) (*unstructured.UnstructuredList, error) {
	if !i.IsReady() {
		return nil, ErrNotReady
	}

	// Determine the canonical kind name from the GVK.
	kind := gvk.Kind // "MCPServer", "VirtualMCPServer" or "MCPRemoteProxy"

	// Determine the two list GVKs (one per version) for this kind.
	var listGVKs []schema.GroupVersionKind
	switch kind {
	case "MCPServer":
		listGVKs = []schema.GroupVersionKind{MCPServerListGVKv1alpha1, MCPServerListGVKv1beta1}
	case "VirtualMCPServer":
		listGVKs = []schema.GroupVersionKind{VirtualMCPServerListGVKv1alpha1, VirtualMCPServerListGVKv1beta1}
	case "MCPRemoteProxy":
		listGVKs = []schema.GroupVersionKind{MCPRemoteProxyListGVKv1alpha1, MCPRemoteProxyListGVKv1beta1}
	default:
		// Fallback: single query with the list GVK derived from the input.
		fallback := gvk
		fallback.Kind = gvk.Kind + "List"
		listGVKs = []schema.GroupVersionKind{fallback}
	}

	// Query both versions and feed into the dedup store.
	store := newDedupStore()
	var lastErr error
	failures := 0
	for _, listGVK := range listGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := i.Manager.GetClient().List(ctx, list); err != nil {
			// Log at V(1) — a missing CRD for one version is not fatal
			// because the other version may serve the objects.
			i.Log.V(1).Info("toolhive List failed for version",
				"listGVK", listGVK.String(),
				"err", err.Error())
			lastErr = err
			failures++
			continue
		}
		for idx := range list.Items {
			store.Upsert(&list.Items[idx])
		}
	}
	// If EVERY version query failed, the empty result is not authoritative.
	// Surfacing nil here would let callers mistake a transient API outage
	// for "no objects exist" and prune all child CRs. Propagate the error
	// so the caller can return (ctrl.Result{}, err) for backoff.
	if failures == len(listGVKs) && failures > 0 {
		return nil, fmt.Errorf("toolhive List failed for all %d version(s): %w", failures, lastErr)
	}

	// FIX.txt LOW-7 (2026-05-22): each per-tuple dedup observation logs
	// at V(2), throttled to once per dedupLogWindow per (kind, ns, name).
	// FIX2.txt M-4 (2026-05-22): at default verbosity, emit a single
	// coalesced "dedup summary" INFO line at most once per
	// dedupSummaryWindow — listing tuple count + a small example slice.
	// Operational noise drops from "22 lines/min" to "1 line per 5m" at
	// V(0) without losing per-tuple traceability at V(2).
	i.dedupThrottleOnce.Do(func() { i.dedupThrottle = newDedupLogThrottle() })
	for _, ck := range store.collisions {
		if i.dedupThrottle.shouldLog(ck, dedupLogWindow) {
			i.Log.V(2).Info("toolhive dedup: v1alpha1 wins",
				"kind", ck.Kind,
				"namespace", ck.Namespace,
				"name", ck.Name,
				"dedup_reason", "alpha_wins",
			)
		}
	}
	if len(store.collisions) > 0 && i.shouldLogDedupSummary() {
		examples := dedupCollisionExamples(store.collisions, 10)
		i.Log.Info("toolhive dedup summary: v1alpha1 wins",
			"tuple_count", len(store.collisions),
			"examples", examples,
			"dedup_reason", "alpha_wins",
		)
	}

	// Collect deduped results.
	deduped := store.List(kind, "")

	result := &unstructured.UnstructuredList{}
	// Preserve the GVK on the list object for callers that inspect it.
	// Use the v1alpha1 list GVK as canonical (alpha-wins rule).
	switch kind {
	case "MCPServer":
		result.SetGroupVersionKind(MCPServerListGVKv1alpha1)
	case "VirtualMCPServer":
		result.SetGroupVersionKind(VirtualMCPServerListGVKv1alpha1)
	default:
		result.SetGroupVersionKind(listGVKs[0])
	}

	result.Items = make([]unstructured.Unstructured, 0, len(deduped))
	for _, obj := range deduped {
		result.Items = append(result.Items, *obj)
	}
	return result, nil
}
