// SPDX-License-Identifier: Apache-2.0

// mcpserverdiscovery_controller.go implements the Pipeline B reconciler for
// `LiteLLMMCPServerDiscovery` CRs (spec §6.5). The reconciler reads cluster-scoped
// ToolHive `MCPServer` and `VirtualMCPServer` objects via the lazy dynamic
// informer, filters them per spec.toolhive.{namespaces,kinds}, derives a
// hyphen-separated two-part child name (`<spec.prefix>-<source-name>` —
// FIX4.txt H-2 v0.3.0 breaking change; pre-v0.3.0 was the dotted
// three-part `<discovery>.<source-ns>.<source-name>`), normalizes the
// transport per Phase 5 D-10, applies the RE2 filter pipeline against
// the post-derivation child name, and SSA-renders one child
// `LiteLLMMCPServer` per kept candidate via Server-Side Apply with
// FieldOwner `litellm-mcpserverdiscovery`. The reconciler returns
// `ctrl.Result{RequeueAfter: spec.refresh.interval}` (Phase 4 D-08 +
// Phase 5 D-08 inherited).
//
// FIX4.txt H-2 collision policy (v0.3.0): cross-discovery collisions
// are the user's responsibility via `spec.prefix`. Intra-discovery
// collisions (two upstream ToolHive objects with the same source name
// but different source namespaces) are loud-fail: the first occurrence
// wins, later occurrences are dropped into status.skippedCandidates
// with Reason=NameCollision AND a parent-level
// NameCollision=True/Reason=NameCollision status condition. The user
// must rename one upstream or split the discovery into prefix-distinct
// ones to resolve.
//
// Pipeline B contract (MSDISC-16): the reconciler does NOT import
// `internal/litellm`, `internal/connection`, or `internal/substitution`.
// It NEVER calls LiteLLM and is not gated on `LiteLLMConnection/default`.
// `spec.params` and `spec.secrets[]` propagate VERBATIM into every
// generated child (MSDISC-11 / AC-SEC4-PROPAGATE) — the child MCPServer
// reconciler substitutes on its own reconcile.
//
// Three divergences from Phase 4 LiteLLMModelDiscovery (per 05-CONTEXT.md):
//
// - Source: ToolHive informer (not HTTP provider). The reconciler reads
// `mcpservers.toolhive.stacklok.dev` + `virtualmcpservers.toolhive.
// stacklok.dev` snapshots via the cluster-scoped informer cache.
// ToolHive CRDs absent at boot → MSDISC-06 Ready=False, reason=
// SourceUnreachable.
// - Naming: hyphen-separated two-part name `<spec.prefix>-<source-name>`
// (FIX4.txt H-2 v0.3.0 — matches LiteLLMModelDiscovery's
// `<prefix>-<source-name>` convention exactly; pre-v0.3.0 was the
// dotted three-part form which had a redundant source-namespace
// component).
// - Transport normalization (D-10): `streamable-http → http`, `sse →
// sse`, empty/absent → `http`, anything else → SKIP with
// status.skippedCandidates[reason=InvalidTransport].
//
// Atomic refresh snapshot (D-09 inherited): on any informer.List error,
// set SourceReachable=False, do NOT enumerate owned children, do NOT
// diff, do NOT delete; existing children stay untouched.
//
// AC-SEC4-PROPAGATE structural guard: SetupWithManager does NOT register
// a Secret event-handler. MSDisc has no credentialsSecretRef (MSDISC-04)
// and does not watch Secrets — the child MCPServer reconciler carries
// Secret-rotation propagation per Phase 3 D-06.

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/filters"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/toolhive"
)

// Discovery-side identifiers — mirror Phase 4 LiteLLMModelDiscovery with MSDisc-
// specific renames and the D-06 SSA field-owner.
const (
	// mcpServerDiscoveryFinalizer is the Discovery finalizer name. Per
	// MSDISC-15 + spec §7.5, this finalizer issues NO LiteLLM call — its
	// only work is waiting for owned children to drain via
	// `blockOwnerDeletion=true` cascade, then removing itself. Each
	// child LiteLLMMCPServer's own finalizer issues the upstream DELETE against
	// LiteLLM.
	mcpServerDiscoveryFinalizer = "mcpserverdiscoveries.litellm.ackstorm.ai/finalizer"

	// mcpServerDiscoveryKind is the metric label for LiteLLMMCPServerDiscovery CRs.
	mcpServerDiscoveryKind = "LiteLLMMCPServerDiscovery"

	// MCPServer transport-enum values (spec §6.4 CEL set {http, sse}).
	// transportHTTP also covers the streamable-http normalization target.
	transportHTTP = "http"
	transportSSE  = "sse"

	// msDiscFieldOwner is the SSA field manager identity used by MSDisc on
	// every child LiteLLMMCPServer write (Phase 5 D-06). Distinct from Phase 4's
	// `litellm-modeldiscovery` so the operator owns its projection
	// without fighting prior SSA writers.
	msDiscFieldOwner = "litellm-mcpserverdiscovery"
)

// ToolHiveInformerReader is the minimal surface this reconciler requires
// from the ToolHive lazy dynamic informer. The production
// type `*toolhive.Informer` satisfies this interface; envtests inject a
// stub when they need to exercise the absent-CRD / SourceUnreachable
// path without touching the shared envtest environment's CRD set.
//
// Defined here (rather than in internal/toolhive) so public
// API stays minimal — the consumer's interface contract lives next to
// the consumer.
type ToolHiveInformerReader interface {
	IsReady() bool
	List(ctx context.Context, gvk schema.GroupVersionKind) (*unstructured.UnstructuredList, error)
}

// Compile-time guarantee that *toolhive.Informer satisfies our interface.
var _ ToolHiveInformerReader = (*toolhive.Informer)(nil)

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpserverdiscoveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpserverdiscoveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpserverdiscoveries/finalizers,verbs=update
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers,verbs=get;list;watch;create;update;patch;delete

// MCPServerDiscoveryReconciler reconciles LiteLLMMCPServerDiscovery CRs per
// spec §6.5 + Phase 5 CONTEXT.md D-06.D-10. The reconciler is periodic
// (`RequeueAfter: spec.refresh.interval`, MSDISC-05 floor 1m enforced
// at admission) and event-driven via `Owns(&LiteLLMMCPServer{})` so cascade-
// delete + adoption hooks drive sub-interval reconciles.
//
// Pipeline B contract (MSDISC-16): no `internal/litellm` import; no
// `connection.Cache` field; no Secret event-handler.
type MCPServerDiscoveryReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// toolHiveMu serializes reads/writes of ToolHiveInformer. The field
	// is mutated by tests (stub injection in mcpserverdiscovery_controller_test.go)
	// while the manager's worker goroutine is concurrently reading it
	// from Reconcile — a race that the Go race detector flagged during
	// CI (see commit 1e01a74). Production code writes once (struct
	// literal init from cmd/main.go) and reads many; the mutex is
	// uncontended in production and only relevant under envtest.
	toolHiveMu       sync.RWMutex
	ToolHiveInformer ToolHiveInformerReader

	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent

	// ReconcileCount is a test-only seam: every Reconcile invocation
	// increments it atomically at the entry point (before any
	// short-circuit). Tests snapshot baseline/final deltas — they do NOT
	// swap the counter. A single, long-lived atomic.Int64 owned by the
	// reconciler eliminates the pointer-swap race that the previous
	// atomic.Pointer[atomic.Int64] design exhibited: an in-flight
	// Reconcile holding the loaded *atomic.Int64 from a prior test could
	// race against the next test's local atomic.Int64 once the address
	// was GC-reused.
	ReconcileCount atomic.Int64

	// cascadeDrainLog tracks per-CR drain progress so the
	// "cascade-delete: waiting for children to drain" line is emitted
	// at INFO only when the remaining count changes or the wait
	// exceeds cascadeDrainDeadline. All other reconciles log at V(2).
	// FIX3.txt LOW-3 — prevents one line every 5s during a hung delete.
	cascadeDrainLogMu sync.Mutex
	cascadeDrainLog   map[string]cascadeDrainState
}

// cascadeDrainState carries per-CR throttle state for the
// "waiting for children to drain" log line. See cascadeDrainLog.
type cascadeDrainState struct {
	lastRemaining int
	startedAt     time.Time
	lastWarnAt    time.Time
}

// cascadeDrainDeadline is the elapsed-time threshold after which the
// drain-wait log line is escalated to WARN with a hint to check
// finalizer state on the children.
const cascadeDrainDeadline = 5 * time.Minute

// candidate is the post-derivation tuple for one ToolHive object that
// passed the namespace + kind filter. childName is the K8s metadata.name
// of the to-be-rendered child LiteLLMMCPServer in the new
// `<spec.prefix>-<source-name>` form (FIX4.txt H-2 v0.3.0 breaking
// change — pre-v0.3.0 was `<discovery>.<source-ns>.<source-name>`).
// url + transport are sourced from ToolHive `status.url` / normalized
// `status.transport`.
type candidate struct {
	childName       string
	url             string
	transport       string
	sourceNamespace string
	sourceName      string
}

// getToolHive returns the current ToolHiveInformer under the read lock.
// Reconcile and tests must use this accessor (not r.ToolHiveInformer
// directly) to keep the race detector happy when fixture code swaps the
// field while a worker goroutine is reading it.
func (r *MCPServerDiscoveryReconciler) getToolHive() ToolHiveInformerReader {
	r.toolHiveMu.RLock()
	defer r.toolHiveMu.RUnlock()
	return r.ToolHiveInformer
}

// SetToolHive replaces the ToolHiveInformer under the write lock. Test
// fixtures use this to inject stubs; production code uses the struct-literal
// initialization in cmd/main.go (single-writer, never concurrent with a
// running reconcile so the lock is uncontended).
func (r *MCPServerDiscoveryReconciler) SetToolHive(i ToolHiveInformerReader) {
	r.toolHiveMu.Lock()
	defer r.toolHiveMu.Unlock()
	r.ToolHiveInformer = i
}

// logCascadeDrain emits the "cascade-delete: waiting for children to
// drain" line with FIX3.txt LOW-3 throttling: INFO only when the
// remaining count changes from the last observation OR when the wait
// has exceeded cascadeDrainDeadline (then WARN with a hint). All other
// reconciles log at V(2). Per-CR state lives in r.cascadeDrainLog.
// Returns overdue=true (at most once per deadline window) when the drain has
// exceeded the deadline, so the caller can emit a Warning event + metric
// (M-B9) in addition to the WARN log.
func (r *MCPServerDiscoveryReconciler) logCascadeDrain(_ context.Context, logger logr.Logger, name string, remaining int) (overdue bool) {
	r.cascadeDrainLogMu.Lock()
	defer r.cascadeDrainLogMu.Unlock()
	if r.cascadeDrainLog == nil {
		r.cascadeDrainLog = map[string]cascadeDrainState{}
	}
	now := time.Now()
	prev, ok := r.cascadeDrainLog[name]
	if !ok {
		prev = cascadeDrainState{lastRemaining: -1, startedAt: now}
	}
	changed := prev.lastRemaining != remaining
	overdue = now.Sub(prev.startedAt) >= cascadeDrainDeadline &&
		now.Sub(prev.lastWarnAt) >= cascadeDrainDeadline
	switch {
	case overdue:
		logger.Info("cascade-delete: still draining past deadline; check finalizer state on children",
			"remaining", remaining,
			"elapsed", now.Sub(prev.startedAt).Round(time.Second).String())
		prev.lastWarnAt = now
		metrics.CascadeDrainOverdueTotal.WithLabelValues(mcpServerDiscoveryKind).Inc()
	case changed:
		logger.Info("cascade-delete: waiting for children to drain", "remaining", remaining)
	default:
		logger.V(2).Info("cascade-delete: waiting for children to drain", "remaining", remaining)
	}
	prev.lastRemaining = remaining
	r.cascadeDrainLog[name] = prev
	return overdue
}

// forgetCascadeDrain clears the per-CR drain-log throttle state after
// the parent's finalizer is removed (drain complete). Prevents
// monotonic growth of r.cascadeDrainLog over the operator's lifetime.
func (r *MCPServerDiscoveryReconciler) forgetCascadeDrain(name string) {
	r.cascadeDrainLogMu.Lock()
	defer r.cascadeDrainLogMu.Unlock()
	delete(r.cascadeDrainLog, name)
}

// Reconcile implements the MCPServerDiscovery state machine. See package
// doc above.
//
//nolint:gocyclo // Linear state machine — splitting obscures the §6.5 mapping.
func (r *MCPServerDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.ReconcileCount.Add(1)
	logger := log.FromContext(ctx).WithValues("mcpserverdiscovery", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var md litellmv1alpha1.LiteLLMMCPServerDiscovery
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path — cascade-delete drain wait (MSDISC-15) ────
	// MSDisc's finalizer issues NO LiteLLM call (MSDISC-16 anti-pattern).
	// The K8s garbage collector cascade-deletes owned children via
	// blockOwnerDeletion=true; each child LiteLLMMCPServer's own finalizer
	// issues the upstream DELETE against LiteLLM.
	// The reconciler MUST wait for all owned children to drain before
	// removing its own finalizer.
	if !md.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&md, mcpServerDiscoveryFinalizer) {
			var owned litellmv1alpha1.LiteLLMMCPServerList
			if err := r.List(ctx, &owned,
				client.InNamespace(r.Namespace),
				client.MatchingLabels{generatedByLabel: md.Name},
			); err != nil {
				return ctrl.Result{}, err
			}
			if len(owned.Items) > 0 {
				// FIX3.txt HIGH-1: K8s GC cannot propagate the parent
				// delete to children while the parent's finalizer is
				// pending (deadlock: GC waits for finalizer, finalizer
				// waits for GC). The reconciler MUST issue an explicit
				// Delete against every child that has not yet entered
				// its own deletion path. Children already being deleted
				// (DeletionTimestamp set) are skipped — their own
				// finalizer drives the LiteLLM DELETE.
				for i := range owned.Items {
					child := &owned.Items[i]
					if !child.DeletionTimestamp.IsZero() {
						continue
					}
					if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
						return ctrl.Result{}, err
					}
				}
				if r.logCascadeDrain(ctx, logger, md.Name, len(owned.Items)) && r.Recorder != nil {
					r.Recorder.Eventf(&md, corev1.EventTypeWarning, "CascadeDrainOverdue",
						"cascade-delete still draining %d child MCPServer(s) past deadline; check finalizer state on the children",
						len(owned.Items))
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// All children drained. MSDisc finalizer issues NO LiteLLM
			// call — just remove the finalizer.
			// OBS-03: drop the cr_status_age_seconds label before the CR is gone (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(mcpServerDiscoveryKind, md.Name)
			// FIX3.txt LOW-3: drop drain-log throttle state.
			r.forgetCascadeDrain(md.Name)
			controllerutil.RemoveFinalizer(&md, mcpServerDiscoveryFinalizer)
			if err := r.Update(ctx, &md); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&md, mcpServerDiscoveryFinalizer) {
		controllerutil.AddFinalizer(&md, mcpServerDiscoveryFinalizer)
		if err := r.Update(ctx, &md); err != nil {
			return ctrl.Result{}, err
		}
		// FIX10 (v0.4.4): explicit requeue. The previous bare-nil return
		// relied on the For watch firing on the finalizer-add Update event
		// to re-enter Reconcile. With the discoverySpecChanged predicate
		// filtering Updates that don't bump generation, metadata-only
		// finalizer adds are filtered → reconciler never returns to
		// generate children. Explicit Requeue:true bypasses the predicate.
		return ctrl.Result{Requeue: true}, nil
	}

	// ─── Step 3: Source-reachable gate (MSDISC-06 / D-08) ──────────────────
	// ToolHive CRDs absent at startup → Informer.IsReady == false.
	// MSDisc surfaces Ready=False, reason=SourceUnreachable and requeues
	// on the informer's retry cadence (1m by default).
	informer := r.getToolHive()
	if informer == nil || !informer.IsReady() {
		r.writeBothConditions(ctx, &md,
			metav1.ConditionFalse, "SourceUnreachable", "ToolHive CRDs not installed",
			metav1.ConditionFalse, "Unreachable", "ToolHive CRDs not installed")
		metrics.ReconcileTotal.WithLabelValues(mcpServerDiscoveryKind, "success").Inc()
		// Use the informer's retry cadence (D-08) — 1 minute. Faster
		// requeue is unnecessary; the informer wakes us when CRDs land.
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// ─── Step 4: Compute the candidate set via the Informer ────────────────
	// Atomic refresh snapshot (D-09 inherited): on ANY List error, set
	// SourceReachable=False, DO NOT enumerate owned children, DO NOT
	// diff, DO NOT delete; return for backoff. Existing children stay
	// untouched.
	kinds := md.Spec.Toolhive.Kinds
	if len(kinds) == 0 {
		// Default per CRD: both kinds.
		kinds = []string{"MCPServer", "VirtualMCPServer"}
	}
	kindSet := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		kindSet[k] = struct{}{}
	}
	nsSet := make(map[string]struct{}, len(md.Spec.Toolhive.Namespaces))
	for _, ns := range md.Spec.Toolhive.Namespaces {
		nsSet[ns] = struct{}{}
	}

	// Gather raw unstructured objects from both GVKs, filtered by the
	// configured kinds.
	var raw []*unstructured.Unstructured
	if _, want := kindSet["MCPServer"]; want {
		list, err := informer.List(ctx, toolhive.MCPServerGVK)
		if err != nil {
			r.writeBothConditions(ctx, &md,
				metav1.ConditionFalse, "SourceUnreachable", sanitizeError(err),
				metav1.ConditionFalse, "Unreachable", sanitizeError(err))
			logger.V(1).Info("ToolHive List(MCPServer) failed; D-09 atomic refresh — children untouched",
				"error", err)
			return ctrl.Result{}, err
		}
		for i := range list.Items {
			raw = append(raw, &list.Items[i])
		}
	}
	if _, want := kindSet["VirtualMCPServer"]; want {
		list, err := informer.List(ctx, toolhive.VirtualMCPServerGVK)
		if err != nil {
			r.writeBothConditions(ctx, &md,
				metav1.ConditionFalse, "SourceUnreachable", sanitizeError(err),
				metav1.ConditionFalse, "Unreachable", sanitizeError(err))
			logger.V(1).Info("ToolHive List(VirtualMCPServer) failed; D-09 atomic refresh — children untouched",
				"error", err)
			return ctrl.Result{}, err
		}
		for i := range list.Items {
			raw = append(raw, &list.Items[i])
		}
	}

	// ─── Step 5: Namespace filter (in-memory per D-07) + derivation ────────
	// For each candidate ToolHive object: filter by namespace, derive the
	// child name, read status.url (skip with EndpointUnknown if absent),
	// normalize status.transport per D-10 (skip with InvalidTransport
	// for unmappable values).
	candidates := make([]candidate, 0, len(raw))
	skipped := make([]litellmv1alpha1.MCPServerSkippedCandidate, 0, len(raw))
	var failed []litellmv1alpha1.MCPServerFailedCandidate
	// Sort raw objects DESC by (namespace, name) so the FIRST entry the
	// first-seen-wins dedup loop encounters for each sourceName is the
	// alpha-LAST entry under ASC `(namespace, name)` ordering — which
	// is the survivor under the project-wide alpha-last-wins conflict
	// rule (docs/concepts/conflict-resolution.md, ADR-0001).
	//
	// Note: status output stability is preserved by the post-loop
	// sort.Slice on `skipped` (line ~699) which sorts by candidate
	// Name ASC — the rendering layer is order-independent of the
	// dedup-time iteration direction.
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].GetNamespace() != raw[j].GetNamespace() {
			return raw[i].GetNamespace() > raw[j].GetNamespace()
		}
		return raw[i].GetName() > raw[j].GetName()
	})

	// FIX4.txt H-2 (v0.3.0): cross-namespace name-collision tracker.
	// When two ToolHive objects from different namespaces share the same
	// metadata.name within a single discovery (e.g. ns=mcp object=foo +
	// ns=tools object=foo), the new naming scheme `<prefix>-<source-name>`
	// would produce identical child names and the second SSA write would
	// either silently overwrite the first or AlreadyExists-classify it
	// as a cross-Discovery duplicate. Loud-fail instead: skip later
	// occurrences and surface a NameCollision=True status condition on
	// the parent so the user can rename one upstream or split the
	// discovery into two prefix-distinct ones.
	//
	// Survivor policy (alpha-last-wins, project-wide per ADR-0001):
	// with `raw` sorted DESC above, the FIRST occurrence the loop sees
	// for each sourceName is the alpha-LAST under ASC ordering — that
	// entry wins. Subsequent occurrences (alpha-earlier) are skipped.
	seenSourceName := map[string]string{} // sourceName → winning sourceNamespace (alpha-LAST under ASC)
	nameCollisions := []string{}
	for _, obj := range raw {
		// Namespace filter (in-memory; cluster-scoped informer per D-07).
		if _, want := nsSet[obj.GetNamespace()]; !want {
			continue
		}

		sourceName := obj.GetName()
		childName := md.Spec.Prefix + "-" + sourceName
		ownedBy := obj.GetNamespace() + "/" + sourceName

		// M-B7: <prefix>-<source-name> can exceed the 63-char DNS-1123 label
		// budget (prefix MaxLength=30 + an up-to-63-char source name), so the
		// "MaxLength=30 leaves room" assumption does not hold. A child Model
		// with an over-long name is rejected at K8s admission downstream;
		// loud-fail here with InvalidDiscoveredName so the user sees why
		// rather than hitting an opaque create error.
		if len(childName) > 63 {
			skipped = append(skipped, litellmv1alpha1.MCPServerSkippedCandidate{
				Name:    childName,
				Reason:  "InvalidDiscoveredName",
				OwnedBy: ownedBy,
				Message: fmt.Sprintf("child name is %d chars, exceeds the 63-char DNS-1123 label limit (prefix %q + source %q)", len(childName), md.Spec.Prefix, sourceName),
			})
			continue
		}

		// FIX4.txt H-2: intra-discovery name collision. Alpha-last-wins
		// (ADR-0001): with `raw` sorted DESC, the FIRST occurrence the
		// loop sees for each sourceName is the alpha-LAST under ASC
		// ordering — that entry wins. Subsequent occurrences (alpha-
		// earlier under ASC) are skipped with a NameCollision
		// skippedCandidate entry AND a parent-level NameCollision
		// status condition (set after the candidate loop).
		if winnerNs, dup := seenSourceName[sourceName]; dup {
			skipped = append(skipped, litellmv1alpha1.MCPServerSkippedCandidate{
				Name:    childName,
				Reason:  "NameCollision",
				OwnedBy: ownedBy,
				Message: fmt.Sprintf("source name %q superseded by namespace %q within this discovery (alpha-last-wins; FIX4 H-2)", sourceName, winnerNs),
			})
			nameCollisions = append(nameCollisions,
				fmt.Sprintf("%q (ns %q superseded by ns %q)", sourceName, obj.GetNamespace(), winnerNs))
			continue
		}
		seenSourceName[sourceName] = obj.GetNamespace()

		// Read status.url (MSDISC-12 — EndpointUnknown if absent).
		url, _, _ := unstructured.NestedString(obj.Object, "status", "url")
		if url == "" {
			skipped = append(skipped, litellmv1alpha1.MCPServerSkippedCandidate{
				Name:    childName,
				Reason:  "EndpointUnknown",
				OwnedBy: ownedBy,
				Message: "status.url empty",
			})
			continue
		}

		// Read status.transport (default "http" per D-09; normalize per D-10).
		transportRaw, _, _ := unstructured.NestedString(obj.Object, "status", "transport")
		transport, ok := normalizeTransport(transportRaw)
		if !ok {
			skipped = append(skipped, litellmv1alpha1.MCPServerSkippedCandidate{
				Name:    childName,
				Reason:  "InvalidTransport",
				OwnedBy: ownedBy,
				Message: fmt.Sprintf("transport=%q not in {http, sse, streamable-http→http}", transportRaw),
			})
			continue
		}

		candidates = append(candidates, candidate{
			childName:       childName,
			url:             url,
			transport:       transport,
			sourceNamespace: obj.GetNamespace(),
			sourceName:      sourceName,
		})
	}

	// FIX4.txt H-2: surface the NameCollision condition on the parent
	// when intra-discovery clashes were detected. Idempotent
	// (apimeta.SetStatusCondition on Status=False when no collisions).
	r.setNameCollisionCondition(&md, nameCollisions)

	// ─── Step 6: RE2 filter pipeline on childName (post-derivation) ───────
	// Per <specifics> line 314: the filter target is the POST-DERIVATION
	// child name (`<spec.prefix>-<source-name>`), NOT the bare ToolHive
	// object name (pre-v0.3.0 was the dotted three-part form). Reuse
	// Phase 4's filters.Apply by mapping the MSDisc-typed filter into the
	// LiteLLMModelDiscovery filter shape (the underlying RE2 contract is
	// identical — same Include-strict / Exclude-lenient semantics).
	preFilterCount := len(candidates)
	if md.Spec.Filters != nil {
		childNames := make([]string, len(candidates))
		for i, c := range candidates {
			childNames[i] = c.childName
		}
		// Adapt MCPServerDiscoveryFilters → ModelDiscoveryFilters (same
		// shape, same semantics — filter package owns the regex pipeline).
		adapted := &litellmv1alpha1.ModelDiscoveryFilters{
			Include: md.Spec.Filters.Include,
			Exclude: md.Spec.Filters.Exclude,
		}
		kept, err := filters.Apply(childNames, adapted)
		if err != nil {
			// InvalidConfigError (bad regex) → Ready=False reason=InvalidConfig.
			// UpstreamInvalidError (include matched zero) → Ready=False
			// reason=UpstreamInvalid. Mirrors Phase 4 LiteLLMModelDiscovery.
			switch err.(type) {
			case *filters.InvalidConfigError:
				r.writeBothConditions(ctx, &md,
					metav1.ConditionFalse, "InvalidConfig", err.Error(),
					metav1.ConditionTrue, "Ok", "")
			case *filters.UpstreamInvalidError:
				r.writeBothConditions(ctx, &md,
					metav1.ConditionFalse, "UpstreamInvalid", err.Error(),
					metav1.ConditionTrue, "Ok", "")
			default:
				return ctrl.Result{}, err
			}
			metrics.ReconcileTotal.WithLabelValues(mcpServerDiscoveryKind, "success").Inc()
			return ctrl.Result{RequeueAfter: md.Spec.Refresh.Interval.Duration}, nil
		}
		// Re-build candidates slice keeping only kept child names.
		keptSet := make(map[string]struct{}, len(kept))
		for _, k := range kept {
			keptSet[k] = struct{}{}
		}
		filtered := make([]candidate, 0, len(kept))
		for _, c := range candidates {
			if _, ok := keptSet[c.childName]; ok {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}
	_ = preFilterCount // documented seam — RE2-filtered drops are silent (no skippedCandidate per Phase 4 contract).

	// ─── Step 7: Enumerate existing owned children + adoption recognition ──
	// Single label-selector list reused for BOTH adoption recognition AND
	// vanish detection. The label persists across ownerRef strips (the
	// user PATCHing the ownerRef field does not touch labels) so this is
	// the correct enumeration key for "children this Discovery ever
	// wrote, alive or adopted".
	var existingChildren litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &existingChildren,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{generatedByLabel: md.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list label-matched children for adoption + vanish: %w", err)
	}

	adoptedNames := make(map[string]struct{})
	for i := range existingChildren.Items {
		child := &existingChildren.Items[i]
		if mcpOwnedByThisDiscovery(child, &md) {
			continue
		}
		// Ownership stripped by user (or never set). Record as
		// ExplicitMCPServerExists per spec §6.5 + MSDISC-13 and EXCLUDE
		// from both SSA-write and vanish-delete.
		adoptedNames[child.Name] = struct{}{}
		skipped = append(skipped, litellmv1alpha1.MCPServerSkippedCandidate{
			Name:    child.Name,
			Reason:  "ExplicitMCPServerExists",
			OwnedBy: child.Name,
			Message: "ownerRef stripped; child adopted by user",
		})
		logger.V(1).Info("adoption recognized: child no longer owned by this Discovery",
			"child", child.Name)
	}

	// ─── Step 8: SSA-apply each kept candidate ─────────────────────────────
	generated := make([]string, 0, len(candidates))
	// CR-01 (Phase 5 code review): candidates whose Patch returned
	// AlreadyExists with owned-by-this-Discovery (transient apiserver-cache
	// lag) must NOT be allowed to fall out of desiredSet in Step 9, otherwise
	// the still-present owned child gets spuriously vanish-deleted in the
	// same reconcile, which cascades through the child finalizer to a
	// DELETE+CREATE round-trip against LiteLLM.
	pendingRetries := make(map[string]struct{})
	for _, c := range candidates {
		// Adoption short-circuit: if this candidate's child name matches a
		// child whose ownerRef the user already stripped, the candidate is
		// recorded in skipped[] above and MUST NOT trigger a fresh SSA write.
		if _, adopted := adoptedNames[c.childName]; adopted {
			continue
		}

		// K8s-native conflict pre-check (Phase 4 04-06 pattern). Get the
		// would-be child BEFORE Patch so we can classify cross-Discovery
		// or user-authored collisions deterministically. The Get is cheap
		// (single named lookup, cache-served after first reconcile).
		classifiedSkip, retryable, classifyErr := r.classifyAlreadyExists(ctx, c.childName, &md)
		if classifyErr != nil {
			metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "error").Inc()
			failed = append(failed, litellmv1alpha1.MCPServerFailedCandidate{
				Name:    c.childName,
				Reason:  "ChildCRWriteFailed",
				Message: sanitizeError(classifyErr),
			})
			continue
		}
		if classifiedSkip != nil {
			metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "conflict").Inc()
			skipped = append(skipped, *classifiedSkip)
			logger.V(1).Info("pre-Patch K8s-native conflict classified",
				"child", c.childName, "reason", classifiedSkip.Reason, "ownedBy", classifiedSkip.OwnedBy)
			continue
		}
		_ = retryable // retryable==true means: NotFound (proceed to Patch) OR owned-by-this-Discovery (idempotent re-apply).

		child := buildChildMCPServer(&md, c, r.Namespace)
		applyErr := r.Patch(ctx, child, client.Apply,
			client.FieldOwner(msDiscFieldOwner),
			client.ForceOwnership)
		if applyErr != nil {
			// AlreadyExists fallback path — re-classify (Patch's
			// AlreadyExists can fire when the existing child's controller
			// ownerRef belongs to a DIFFERENT controller; SSA+ForceOwnership
			// is non-overrideable across controllers).
			if apierrors.IsAlreadyExists(applyErr) {
				classifiedSkip2, retryable2, classifyErr2 := r.classifyAlreadyExists(ctx, c.childName, &md)
				if classifyErr2 != nil {
					metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "error").Inc()
					failed = append(failed, litellmv1alpha1.MCPServerFailedCandidate{
						Name:    c.childName,
						Reason:  "ChildCRWriteFailed",
						Message: sanitizeError(classifyErr2),
					})
					continue
				}
				if retryable2 {
					metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "conflict").Inc()
					// CR-01: register in pendingRetries so Step 9 keeps the
					// owned child in desiredSet and does not vanish-delete it.
					pendingRetries[c.childName] = struct{}{}
					logger.V(1).Info("AlreadyExists retry-soon (owned-by-this-Discovery; protected from vanish-delete)",
						"child", c.childName)
					continue
				}
				if classifiedSkip2 != nil {
					metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "conflict").Inc()
					skipped = append(skipped, *classifiedSkip2)
					continue
				}
				// Defensive fall-through — surface as ChildCRWriteFailed.
			}
			metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "error").Inc()
			failed = append(failed, litellmv1alpha1.MCPServerFailedCandidate{
				Name:    c.childName,
				Reason:  "ChildCRWriteFailed",
				Message: sanitizeError(applyErr),
			})
			continue
		}
		metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "create_or_update", "success").Inc()
		generated = append(generated, c.childName)
	}

	sort.Strings(generated)
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Name < skipped[j].Name })

	// ─── Step 9: Label-selector vanish detection (D-09 atomic refresh) ─────
	// Per CONTEXT.md D-09: vanish runs ONLY on the post-source-success path.
	// Any earlier informer.List error returned above with (ctrl.Result{},
	// err) so existing children stay UNTOUCHED on transient failures by
	// construction.
	//
	// Vanish-detection contract (MSDISC-09 inherited shape):
	// - Enumerate owned children via label selector (Step 7 result).
	// - Children NOT in desiredSet (generated + skipped names) AND
	// NOT in adoptedNames get Delete'd.
	// - The deleted child's own finalizer issues the upstream DELETE
	// against LiteLLM. MSDisc NEVER touches LiteLLM.
	desiredSet := make(map[string]struct{}, len(generated)+len(skipped)+len(pendingRetries))
	for _, n := range generated {
		desiredSet[n] = struct{}{}
	}
	for _, s := range skipped {
		desiredSet[s.Name] = struct{}{}
	}
	// CR-01: fold pendingRetries into desiredSet BEFORE vanish-detection so
	// the owned child whose SSA Patch returned AlreadyExists (transient cache
	// race) is preserved across this reconcile and re-tried on the next one.
	for n := range pendingRetries {
		desiredSet[n] = struct{}{}
	}
	for i := range existingChildren.Items {
		child := &existingChildren.Items[i]
		if _, keep := desiredSet[child.Name]; keep {
			continue
		}
		if _, adopted := adoptedNames[child.Name]; adopted {
			// Don't vanish-delete an adopted child — the user owns it now.
			continue
		}
		if !mcpOwnedByThisDiscovery(child, &md) {
			logger.V(1).Info("skipping vanish-delete: child no longer owned by this Discovery (ownerRef stripped)",
				"child", child.Name)
			continue
		}
		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "delete", "error").Inc()
			logger.Error(err, "delete vanished child", "child", child.Name)
			continue
		}
		metrics.ChildCRWritesTotal.WithLabelValues(mcpServerDiscoveryKind, "delete", "success").Inc()
		logger.V(1).Info("vanish-deleted child", "child", child.Name)
	}

	// ─── Step 10: Update status ────────────────────────────────────────────
	now := metav1.NewTime(time.Now())
	md.Status.GeneratedChildren = generated
	// int32 casts are safe — candidate counts are bounded by the user's
	// ToolHive cluster size; v1alpha1 has no explicit cap (per CONTEXT.md
	// T-05-04-05 accept disposition).
	md.Status.GeneratedCount = int32(len(generated))   //nolint:gosec // bounded by ToolHive cluster size
	md.Status.DiscoveredCount = int32(len(candidates)) //nolint:gosec // bounded by ToolHive cluster size
	md.Status.SkippedCandidates = skipped
	md.Status.FailedCandidates = failed
	md.Status.LastRefreshAt = &now
	md.Status.ObservedGeneration = md.Generation

	readyStatus := metav1.ConditionTrue
	readyReason := reasonSynced
	readyMsg := fmt.Sprintf("%d/%d children generated", len(generated), len(candidates))
	if len(failed) > 0 {
		readyStatus = metav1.ConditionFalse
		readyReason = "ChildCRWriteFailed"
		readyMsg = fmt.Sprintf("%d/%d children failed to write to apiserver", len(failed), len(candidates))
	}
	if err := r.writeBothConditionsObj(ctx, &md,
		readyStatus, readyReason, readyMsg,
		metav1.ConditionTrue, "Ok", ""); err != nil {
		logStatusUpdateErr(logger, err)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state; suppress controller-runtime's
			// ERROR "Reconciler error" log + backoff for this error class.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	metrics.ReconcileTotal.WithLabelValues(mcpServerDiscoveryKind, "success").Inc()
	metrics.CRStatusAgeTracker.RecordSuccess(mcpServerDiscoveryKind, md.Name)

	logger.V(1).Info("reconciled",
		"discovered", md.Status.DiscoveredCount,
		"generated", md.Status.GeneratedCount,
		"skipped", len(skipped),
		"failed", len(failed),
		"requeueAfter", md.Spec.Refresh.Interval.Duration)

	// ─── Step 11: Return RequeueAfter (Phase 4 D-08 inherited) ─────────────
	return ctrl.Result{RequeueAfter: md.Spec.Refresh.Interval.Duration}, nil
}

// normalizeTransport implements the Phase 5 D-10 transport normalization
// table:
//
//	streamable-http → http
//	sse → sse
//	"" → http (D-09 default)
//	http → http (verbatim)
//	anything else → InvalidTransport skip (ok=false)
//
// The normalization happens at the Discovery boundary so the child
// LiteLLMMCPServer's spec.transport always falls within the CRD CEL enum
// `{http, sse}` (spec §6.4). The MCPServer reconciler does NOT
// translate (per 05-CONTEXT.md anti-pattern line 303).
func normalizeTransport(raw string) (string, bool) {
	switch raw {
	case "streamable-http":
		return transportHTTP, true
	case transportSSE:
		return transportSSE, true
	case "", transportHTTP:
		return transportHTTP, true
	default:
		return "", false
	}
}

// buildChildMCPServer constructs the desired child LiteLLMMCPServer object that
// will be applied via SSA. Mirrors Phase 4's buildChildModel with MCP-
// side renames:
//
// - TypeMeta required for SSA — the apiserver rejects SSA patches
// without apiVersion+kind.
// - ObjectMeta.{Name, Namespace, Labels, OwnerReferences, Finalizers}
// per MSDISC-10:
// - Name = <spec.prefix>-<source-name>.
// - Labels[generatedByLabel] = parent.Name (the vanish-detection key).
// - OwnerReferences[controller=true, blockOwnerDeletion=true] →
// parent for cascade-delete + adoption recognition.
// - Finalizers = [mcpservers.litellm.ackstorm.ai/finalizer] so the
// child reconciler can drain LiteLLM on cascade.
// - Spec.{Endpoint, Transport} from the candidate (typed-field overlays
// sourced from ToolHive status.url + normalized status.transport).
// - Spec.{Params, Secrets} propagated VERBATIM from parent (MSDISC-11 /
// AC-SEC4-PROPAGATE — MSDisc does NOT substitute {{NAME}}; the child
// reconciler substitutes on its own reconcile).
func buildChildMCPServer(
	md *litellmv1alpha1.LiteLLMMCPServerDiscovery,
	c candidate,
	namespace string,
) *litellmv1alpha1.LiteLLMMCPServer {
	yes := true

	// Empty-safe Params propagation (mirrors Phase 4 buildChildModel's
	// infoRaw handling at modeldiscovery_controller.go:858-862). When
	// md.Spec.Params is absent or empty, the deep-copy yields a zero
	// RawExtension which serializes as `null` — the LiteLLMMCPServer CRD
	// rejects that with "spec.params in body must be of type object".
	// Substitute an empty JSON object `{}` so SSA admission succeeds;
	// the child reconciler treats empty params identically to absent.
	paramsRaw := *md.Spec.Params.DeepCopy()
	if len(paramsRaw.Raw) == 0 {
		paramsRaw = runtime.RawExtension{Raw: []byte(`{}`)}
	}

	return &litellmv1alpha1.LiteLLMMCPServer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: litellmv1alpha1.GroupVersion.String(),
			Kind:       "LiteLLMMCPServer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.childName,
			Namespace: namespace,
			Labels: map[string]string{
				generatedByLabel: md.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         litellmv1alpha1.GroupVersion.String(),
					Kind:               mcpServerDiscoveryKind,
					Name:               md.Name,
					UID:                md.UID,
					Controller:         &yes,
					BlockOwnerDeletion: &yes,
				},
			},
			Finalizers: []string{mcpServerFinalizer},
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  c.url,
			Transport: c.transport,
			// Params + Secrets propagate VERBATIM (MSDISC-11). The empty-
			// safe `{}` substitution above ensures SSA admission succeeds
			// when md.Spec.Params is absent (CRD requires `params` to be a
			// JSON object, not `null`). copy-slice for Secrets so future
			// mutations of the parent's Secrets don't leak through.
			Params:  paramsRaw,
			Secrets: append([]litellmv1alpha1.SecretSubstitution(nil), md.Spec.Secrets...),
		},
	}
}

// classifyAlreadyExists handles K8s-native conflict resolution per
// MSDISC-13. Get the colliding object; classify per ownerRef state:
//
//	(skip=ExplicitMCPServerExists, retryable=false, err=nil)
//	 No controller ownerRef → user-authored LiteLLMMCPServer.
//
//	(skip=Conflict, retryable=false, err=nil)
//	 Controller ownerRef points at a DIFFERENT controller (foreign
//	 Kind, or another Discovery that wins alpha-last-wins). The
//	 OwnedBy field names the winner.
//
//	(skip=nil, retryable=true, err=nil)
//	 Either NotFound (raced delete) OR ownerRef points at THIS
//	 Discovery (transient apiserver/cache race — retry next
//	 reconcile) OR another MCPServerDiscovery owns the child but this
//	 Discovery sorts alpha-last by <namespace>/<name> (ADR-0001 alpha-
//	 last-wins between Discoveries — next SSA Patch with
//	 ForceOwnership wins the child's controller ownerRef).
//
//	(skip=nil, retryable=false, err=<get-err>)
//	 Non-NotFound apiserver error — surface as ChildCRWriteFailed.
//
// Mirrors Phase 4 modeldiscovery_controller.go:1090-1139.
func (r *MCPServerDiscoveryReconciler) classifyAlreadyExists(
	ctx context.Context, childName string, parent *litellmv1alpha1.LiteLLMMCPServerDiscovery,
) (*litellmv1alpha1.MCPServerSkippedCandidate, bool, error) {
	var existing litellmv1alpha1.LiteLLMMCPServer
	if err := r.Get(ctx, client.ObjectKey{Name: childName, Namespace: r.Namespace}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			// Either no collision yet (happy path will Patch as CREATE) OR
			// the AlreadyExists raced with a Delete. Either way: retry.
			return nil, true, nil
		}
		return nil, false, err
	}
	var ctrlRef *metav1.OwnerReference
	for i := range existing.OwnerReferences {
		ref := &existing.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			ctrlRef = ref
			break
		}
	}
	if ctrlRef == nil {
		// No controller ownerRef → user-authored LiteLLMMCPServer.
		return &litellmv1alpha1.MCPServerSkippedCandidate{
			Name:    childName,
			Reason:  "ExplicitMCPServerExists",
			OwnedBy: existing.Name,
		}, false, nil
	}
	if ctrlRef.Kind == mcpServerDiscoveryKind && ctrlRef.UID == parent.UID {
		// Should not happen — SSA+ForceOwnership should have won. Treat as
		// transient.
		return nil, true, nil
	}
	// Different controller (different Discovery UID, or different Kind).
	// Per ADR-0001 this is a `Conflict` skip. Alpha-last-wins ownership
	// transfer between Discoveries is intentionally NOT applied here —
	// it requires a get-then-update path to replace metadata.ownerReferences
	// across field managers and is deferred to a follow-up PR.
	return &litellmv1alpha1.MCPServerSkippedCandidate{
		Name:    childName,
		Reason:  "Conflict",
		OwnedBy: ctrlRef.Kind + "/" + ctrlRef.Name + "/" + string(ctrlRef.UID),
	}, false, nil
}

// mcpOwnedByThisDiscovery reports whether `child`'s controller ownerRef
// (if any) points at `parent` by Kind=LiteLLMMCPServerDiscovery AND UID match.
// UID is forgery-resistant (apiserver-assigned, immutable). Mirrors
// Phase 4 ownedByThisDiscovery.
//
// Used for adoption recognition: a child with the generated-by label but
// WITHOUT this Discovery's controller ownerRef has been adopted by the
// user and MUST NOT be re-claimed by Discovery.
func mcpOwnedByThisDiscovery(child *litellmv1alpha1.LiteLLMMCPServer, parent *litellmv1alpha1.LiteLLMMCPServerDiscovery) bool {
	for i := range child.OwnerReferences {
		ref := &child.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller &&
			ref.Kind == mcpServerDiscoveryKind && ref.UID == parent.UID {
			return true
		}
	}
	return false
}

// ConditionTypeNameCollision is the status condition type fired by the
// FIX4.txt H-2 v0.3.0 NameCollision detector when two upstream ToolHive
// objects from different namespaces produce the same
// `<spec.prefix>-<source-name>` child name within a single discovery.
// Status=True with Reason="NameCollision" lists the offending source
// names; Status=False with Reason="NoCollisions" otherwise (idempotent
// transitions visible to status watchers).
const ConditionTypeNameCollision = "NameCollision"

// setNameCollisionCondition stamps the NameCollision condition on the
// parent in-memory; the final Status().Update at the end of the success
// path picks it up. FIX4.txt H-2.
func (r *MCPServerDiscoveryReconciler) setNameCollisionCondition(
	md *litellmv1alpha1.LiteLLMMCPServerDiscovery, collisions []string,
) {
	if len(collisions) == 0 {
		apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeNameCollision,
			Status:             metav1.ConditionFalse,
			Reason:             "NoCollisions",
			Message:            "",
			ObservedGeneration: md.Generation,
			LastTransitionTime: metav1.Now(),
		})
		return
	}
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeNameCollision,
		Status:             metav1.ConditionTrue,
		Reason:             "NameCollision",
		Message:            "intra-discovery source-name collision(s) detected: " + strings.Join(collisions, "; ") + " — rename one upstream or split the discovery (FIX4 H-2)",
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

// writeBothConditions sets both Ready and SourceReachable conditions and
// writes status as best-effort. Used by error paths that need to set
// conditions and still return an error for backoff.
//
//nolint:unparam // readyStatus is always ConditionFalse today but the helper signature mirrors writeReady for symmetry; planned future paths (Synced gate inversion) will pass True.
func (r *MCPServerDiscoveryReconciler) writeBothConditions(
	ctx context.Context, md *litellmv1alpha1.LiteLLMMCPServerDiscovery,
	readyStatus metav1.ConditionStatus, readyReason, readyMessage string,
	sourceStatus metav1.ConditionStatus, sourceReason, sourceMessage string,
) {
	// Uses Update (not Patch + MergeFrom): callers mutate counters and
	// child lists on md.Status before this call. A MergeFrom orig captured
	// here would already include the mutation, so the patch body would
	// omit DiscoveredCount/GeneratedCount/GeneratedChildren and the server
	// state would diverge from the in-memory reconciled state.
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               "SourceReachable",
		Status:             sourceStatus,
		Reason:             sourceReason,
		Message:            sourceMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	md.Status.ObservedGeneration = md.Generation
	if err := r.Status().Update(ctx, md); err != nil {
		log.FromContext(ctx).V(1).Info("status update failed (best-effort path)", "error", err)
	}
}

// writeBothConditionsObj is the same as writeBothConditions but returns
// the Status.Update error so the success-path caller can surface it.
func (r *MCPServerDiscoveryReconciler) writeBothConditionsObj(
	ctx context.Context, md *litellmv1alpha1.LiteLLMMCPServerDiscovery,
	readyStatus metav1.ConditionStatus, readyReason, readyMessage string,
	sourceStatus metav1.ConditionStatus, sourceReason, sourceMessage string,
) error {
	// Uses Update (not Patch + MergeFrom): same rationale as writeBothConditions
	// — callers mutate counters and child lists on md.Status before this call.
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               "SourceReachable",
		Status:             sourceStatus,
		Reason:             sourceReason,
		Message:            sourceMessage,
		ObservedGeneration: md.Generation,
		LastTransitionTime: metav1.Now(),
	})
	md.Status.ObservedGeneration = md.Generation
	return r.Status().Update(ctx, md)
}

// SetupWithManager registers the MCPServerDiscoveryReconciler with
// controller-runtime.
//
// Watches:
// - For(&LiteLLMMCPServerDiscovery{}) — primary watch.
// - Owns(&LiteLLMMCPServer{}) — child LiteLLMMCPServer events drive sub-interval
// reconciles (cascade-delete + adoption hooks).
//
// NO Secret event-handler — MSDisc has no credentialsSecretRef (MSDISC-04)
// and does not watch Secrets. Secret-rotation propagation
// for the discovered set is the child MCPServer reconciler's job
// (Phase 3 D-06 pattern; AC-SEC4-PROPAGATE structural guard).
//
// Named("mcpserverdiscovery") — controller registry name.
func (r *MCPServerDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMMCPServerDiscovery{},
			builder.WithPredicates(discoverySpecChanged())).
		Owns(&litellmv1alpha1.LiteLLMMCPServer{}, builder.WithPredicates(ownedChildSpecChanged())).
		WithOptions(transientBackoffOptions()).
		Named("mcpserverdiscovery")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}
