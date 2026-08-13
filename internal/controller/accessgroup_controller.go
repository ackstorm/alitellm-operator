// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// accessGroupFinalizer is the finalizer managed by the LiteLLMAccessGroup
// reconciler. DELETE /v1/access_group/<id> removes the group before the CR
// leaves etcd.
const accessGroupFinalizer = "accessgroups.litellm.ackstorm.ai/finalizer"

// accessGroupKind is the metric label for LiteLLMAccessGroup CRs.
const accessGroupKind = "LiteLLMAccessGroup"

// renderedAccessGroup is the resolved, order-normalized projection of an
// AccessGroupSpec. All three slices are guaranteed NON-NIL: they serialize as
// `[]` (an explicit LiteLLM clear), never `null`/absent, which upstream reads
// as "keep the stale value".
type renderedAccessGroup struct {
	Models       []string `json:"access_model_names"`
	MCPServerIDs []string `json:"access_mcp_server_ids"`
	AgentIDs     []string `json:"access_agent_ids"`
}

// missingAccessGroupRefs collects spec names with no upstream id.
type missingAccessGroupRefs struct {
	MCPServers []string
	Agents     []string
}

// renderAccessGroup resolves a spec into the LiteLLM projection.
//
// Models pass through VERBATIM — LiteLLM matches access_model_names on
// model_name, so no resolution step exists for them. MCP servers and agents
// are resolved name→id because those two dimensions match on ids and SILENTLY
// IGNORE names (same trap as team object_permission.agents). An unresolved
// name is reported, never dropped: dropping it would silently narrow a
// permission object with no signal.
//
// Every slice is sorted so the hash is order-independent and a reordered spec
// does not read as drift. (Sorting is why "verbatim" means "no id resolution",
// not "declaration order preserved".)
func renderAccessGroup(
	spec litellmv1alpha1.AccessGroupSpec,
	serverIDs, agentIDs map[string]string,
) (renderedAccessGroup, missingAccessGroupRefs) {
	var missing missingAccessGroupRefs

	models := append([]string{}, spec.Models...)
	sort.Strings(models)

	servers, missingServers := resolveNames(spec.MCPServers, serverIDs)
	missing.MCPServers = missingServers
	sort.Strings(servers)

	agents, missingAgents := resolveNames(spec.Agents, agentIDs)
	missing.Agents = missingAgents
	sort.Strings(agents)

	return renderedAccessGroup{
		Models:       models,
		MCPServerIDs: servers,
		AgentIDs:     agents,
	}, missing
}

// accessGroupHash is the SHA-256 hex of the rendered projection. Feeds
// status.lastRendered.hash and the steady-state short-circuit.
//
// spec.description is deliberately OUT of the hash: it is not part of the
// authorization surface, and a description-only edit still reaches the UPDATE
// branch because it bumps metadata.generation (the steady state also requires
// observedGeneration == generation).
func accessGroupHash(r renderedAccessGroup) string {
	blob, err := canonicalJSON(r)
	if err != nil {
		// Marshaling three []string cannot fail; fall back to a value that
		// never compares equal so a bug forces a re-render rather than a
		// spurious steady state.
		return fmt.Sprintf("unmarshalable-%v", err)
	}
	sum := sha256.Sum256(blob)
	return fmt.Sprintf("%x", sum)
}

// Events RBAC marker inheritance: the package-wide
// `+kubebuilder:rbac:groups="",resources=events,verbs=create;patch` marker
// lives on internal/controller/mcpserver_controller.go. kubebuilder marker
// scope is per-package — this reconciler INHERITS the events grant and MUST
// NOT duplicate it.

// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmaccessgroups/finalizers,verbs=update

// AccessGroupReconciler reconciles LiteLLMAccessGroup CRs against LiteLLM's
// /v1/access_group endpoints. Closest sibling: MCPToolsetReconciler
// (server-minted id, name-based adoption, vanish probe, churn breaker, no
// secrets, no free-form params).
//
// State machine (per-reconcile):
//
//   - Step 1:  Fetch the CR (NotFound → return nil).
//   - Step 2a: DeletionTimestamp set → DELETE /v1/access_group/<id> (with
//     name-resolve fallback when AccessGroupID is empty) → RemoveFinalizer.
//   - Step 2b: Finalizer absent → AddFinalizer → return.
//   - Step 3:  Connection-gating on snap.Usable() (issue #74 — NOT snap.Ready;
//     a Ready-with-nil-Client snapshot is reachable and nil-derefs).
//   - Step 4:  Resolve spec.mcpServers → server_id (GET /v1/mcp/server) and
//     spec.agents → agent_id (GET /v1/agents).
//   - Step 5:  renderAccessGroup; a non-empty `missing` parks the CR
//     Ready=False/MCPServerNotFound|AgentNotFound.
//   - Step 6:  accessGroupHash over the render.
//   - Step 7:  Existence probe (vanish-detection) against GET /v1/access_group.
//   - Step 8:  Hash-equal steady-state short-circuit, INCLUDING the
//     Ready-condition heal (issue #102 — mandatory in every
//     steady-state short-circuit).
//   - Step 9:  Branch CREATE (POST, id read from the RESPONSE, 409 → adopt by
//     name) vs UPDATE (PUT with the id in the PATH).
//   - Step 10: Classify mutation errors per §7.7.
//   - Step 11: Update status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided:
//   - NO RequeueAfter for periodic drift (the SafetyRelistRunnable owns the
//     tick — issue #102). That includes the Step 5 parking path: an ordering
//     dependency self-heals on the next relist tick.
//   - NEVER writes assigned_team_ids / assigned_key_ids. Team attachment is
//     written from the team side; a second writer would drag in delta repair.
type AccessGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMAccessGroup object. No
	// Event is emitted on the happy path today, but the deletion-policy
	// newAckMissingFn requires a non-nil recorder.
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
	// ConnectionRebuilt — issue #44 cache-population race close. nil-safe.
	ConnectionRebuilt <-chan event.GenericEvent
	// Churn trips the RecreateThrottled breaker on a created-but-not-listed
	// group. SetupWithManager wires newChurnGuard(); tests may leave it nil.
	Churn *churnGuard
	// RecreateLimit is the per-CR recreates-per-minute ceiling. <= 0 →
	// DefaultRecreateLimitPerMin.
	RecreateLimit int
}

// Reconcile implements the LiteLLMAccessGroup state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the step mapping.
func (r *AccessGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("accessgroup", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var ag litellmv1alpha1.LiteLLMAccessGroup
	if err := r.Get(ctx, req.NamespacedName, &ag); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !ag.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ag, accessGroupFinalizer) {
			policy := deletionpolicy.Resolve(&ag, ag.Spec.DeletionPolicy)
			onAckMissing := newAckMissingFn(r.Recorder, &ag, accessGroupKind, ag.Namespace, ag.Name, policy)

			snap := r.Cache.Snapshot()
			if snap.Usable() {
				groupID := ag.Status.LastRendered.AccessGroupID
				if groupID == "" {
					// Stale-status fallback: re-resolve by name via
					// GET /v1/access_group.
					if resolved := r.resolveAccessGroupIDByName(ctx, snap.Client, ag.Name, logger); resolved != "" {
						groupID = resolved
					}
				}
				if groupID != "" {
					// DELETE 404 is already folded into success by the client
					// (§7.7 idempotent delete) — unlike /v1/mcp/toolset, this
					// endpoint answers a clean 404 on an absent row, so a
					// confirmed-absent entry drains here regardless of policy.
					if err := snap.Client.DeleteAccessGroup(ctx, groupID); err != nil {
						var auth401 *litellm.Auth401Error
						switch {
						case errors.As(err, &auth401):
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							if gerr := onAckMissing("401 on DeleteAccessGroup"); gerr != nil {
								return ctrl.Result{}, gerr
							}
						case is4xxStatus(err):
							logger.Info("deletion: deterministic 4xx on DeleteAccessGroup; ack-missing", "error", err.Error())
							if gerr := onAckMissing("4xx on DeleteAccessGroup: " + err.Error()); gerr != nil {
								return ctrl.Result{}, gerr
							}
						default:
							// Transient error — return for backoff. Finalizer stays.
							return ctrl.Result{}, err
						}
					} else {
						metrics.DriftCorrectedTotal.WithLabelValues("accessgroup", "delete_vanished").Inc()
						metrics.DeletionBlocked.Forget(accessGroupKind, ag.Namespace, ag.Name)
						logger.Info("finalizer removed; LiteLLM access group deleted", "accessGroupID", groupID)
					}
				} else {
					metrics.DeletionBlocked.Forget(accessGroupKind, ag.Namespace, ag.Name)
					logger.Info("finalizer removed; LiteLLM entry already absent (no pinned ID, name-resolve returned empty)", "name", ag.Name)
				}
			} else {
				if err := onAckMissing("LiteLLM unavailable"); err != nil {
					return ctrl.Result{}, err
				}
			}

			metrics.CRStatusAgeTracker.Forget(accessGroupKind, ag.Name)
			metrics.DeletionBlocked.Forget(accessGroupKind, ag.Namespace, ag.Name)
			controllerutil.RemoveFinalizer(&ag, accessGroupFinalizer)
			if err := r.Update(ctx, &ag); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&ag, accessGroupFinalizer) {
		controllerutil.AddFinalizer(&ag, accessGroupFinalizer)
		if err := r.Update(ctx, &ag); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connection-gating ─────────────────────────────────────────
	//
	// Usable(), NOT Ready (issue #74): Cache.Rebuild does not enforce the
	// Ready=true ⇒ Client!=nil invariant, so a Ready snapshot with a nil
	// Client is reachable and would nil-deref below.
	snap := r.Cache.Snapshot()
	if !snap.Usable() {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &ag, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(accessGroupKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 4: Resolve MCP server + agent NAMES to ids ───────────────────
	//
	// Only listed when the corresponding spec list is non-empty — a group of
	// pure model names costs zero extra LiteLLM calls.
	//
	// DELIBERATE deviation from the Team reconciler, which uses the UNCACHED
	// ListMCPServers / ListA2AAgents for the same job: the 30s cache means a
	// server or agent registered moments ago can still read as missing, so the
	// CR parks Ready=False reason=MCPServerNotFound for up to one TTL longer
	// than a fresh list would. That is acceptable here because parking is
	// self-healing (the SafetyRelistRunnable re-drives it) and the operator
	// invalidates the cache on its OWN mutations, so the stale window only
	// covers out-of-band registrations.
	var serverIDs map[string]string
	if len(ag.Spec.MCPServers) > 0 {
		entries, lerr := snap.Client.CachedListMCPServers(ctx)
		if lerr != nil && !litellm.IsNotFound(lerr) {
			return r.classifyMutationError(ctx, &ag, logger, lerr, "GET /v1/mcp/server")
		}
		serverIDs = make(map[string]string, len(entries))
		for _, e := range entries {
			serverIDs[e.ServerName] = e.ServerID
		}
	}
	var agentIDs map[string]string
	if len(ag.Spec.Agents) > 0 {
		entries, lerr := snap.Client.CachedListAgents(ctx)
		if lerr != nil && !litellm.IsNotFound(lerr) {
			return r.classifyMutationError(ctx, &ag, logger, lerr, "GET /v1/agents")
		}
		agentIDs = make(map[string]string, len(entries))
		for _, e := range entries {
			agentIDs[e.AgentName] = e.AgentID
		}
	}

	// ─── Step 5: Render, park on unresolved names ──────────────────────────
	//
	// An unresolved name is an ordering dependency with the LiteLLMMCPServer /
	// LiteLLMA2AAgent CR that registers it. Park rather than under-grant
	// silently, and return WITHOUT a RequeueAfter — the SafetyRelistRunnable
	// owns the periodic tick (#102).
	rendered, missing := renderAccessGroup(ag.Spec, serverIDs, agentIDs)
	if len(missing.MCPServers) > 0 {
		msg := fmt.Sprintf("spec.mcpServers not yet registered in LiteLLM: %s",
			strings.Join(missing.MCPServers, ", "))
		if werr := r.writeStatus(ctx, &ag, metav1.ConditionFalse, reasonMCPServerNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonMCPServerNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(accessGroupKind, "success").Inc()
		return ctrl.Result{}, nil
	}
	if len(missing.Agents) > 0 {
		msg := fmt.Sprintf("spec.agents not yet registered in LiteLLM: %s",
			strings.Join(missing.Agents, ", "))
		if werr := r.writeStatus(ctx, &ag, metav1.ConditionFalse, reasonAgentNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonAgentNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(accessGroupKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 6: Compute currentRenderedHash ───────────────────────────────
	currentRenderedHash := accessGroupHash(rendered)

	// ─── Step 7: Existence probe (vanish-detection) ────────────────────────
	// Verify AccessGroupID still exists in LiteLLM. On not-found OR id-drift,
	// clear it so the Step 9 CREATE arm fires. Without this probe the operator
	// silently misses out-of-band deletes.
	//
	// Skipped on first reconcile (lastRendered.hash empty).
	if ag.Status.LastRendered.AccessGroupID != "" && ag.Status.LastRendered.Hash != "" {
		clear, probeErr := probeVanishedResourceID(ctx,
			ag.Status.LastRendered.AccessGroupID,
			func(c context.Context) (string, error) {
				// ErrNotFound is classified as "vanished" by
				// probeVanishedResourceID itself — do not pre-translate it.
				entry, gerr := snap.Client.GetAccessGroupByName(c, ag.Name)
				if gerr != nil {
					return "", gerr
				}
				if entry == nil {
					return "", nil
				}
				return entry.AccessGroupID, nil
			},
			r.Cache.InvalidateOn401, logger, "accessgroup")
		if probeErr != nil {
			return ctrl.Result{}, probeErr
		}
		if clear {
			ag.Status.LastRendered.AccessGroupID = ""
		}
	}

	// ─── Step 8: Hash-equal steady state ───────────────────────────────────
	if ag.Status.LastRendered.Hash == currentRenderedHash &&
		ag.Status.LastRendered.AccessGroupID != "" &&
		ag.Status.ObservedGeneration == ag.Generation {
		// Stale-status heal (issue #102): a Step 3 connection-gate write
		// stamps observedGeneration alongside Ready=False and leaves
		// lastRendered intact, so once the connection recovers all three
		// predicates hold and the reconciler would short-circuit forever on a
		// stale Ready=False. Every steady-state short-circuit MUST heal.
		if ready := apimeta.FindStatusCondition(ag.Status.Conditions, conditionTypeReady); ready == nil ||
			ready.Status != metav1.ConditionTrue || ready.Reason != reasonSynced {
			if err := r.writeStatus(ctx, &ag, metav1.ConditionTrue, reasonSynced, "access group registered"); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
		}
		metrics.CRStatusAgeTracker.RecordSuccess(accessGroupKind, ag.Name)
		r.Churn.Forget(req.NamespacedName)
		// No RequeueAfter: the SafetyRelistRunnable owns the periodic tick.
		return ctrl.Result{}, nil
	}

	// ─── Step 9: Branch CREATE vs UPDATE ───────────────────────────────────
	//
	// First-reconcile sentinel: on observedGeneration == 0 OR
	// lastRendered.hash == "", drift counters are suppressed.
	firstReconcile := ag.Status.ObservedGeneration == 0 || ag.Status.LastRendered.Hash == ""

	var newGroupID string
	if ag.Status.LastRendered.AccessGroupID == "" {
		// CREATE path. LiteLLM MINTS the access_group_id — a supplied one is
		// ignored on 1.93.0 — so it must be read from the response, never
		// derived from metadata.name (unlike team_id / MCP server_id).
		//
		// Recreate circuit breaker: a recreate (not first reconcile) means the
		// vanish probe cleared a populated AccessGroupID. If that repeats
		// faster than RecreateLimit/min the entry is created-but-not-listed;
		// park the CR instead of storming LiteLLM.
		if !firstReconcile {
			limit := r.RecreateLimit
			if limit <= 0 {
				limit = DefaultRecreateLimitPerMin
			}
			if n := r.Churn.Count(req.NamespacedName); n >= limit {
				msg := fmt.Sprintf("recreate throttled: %d recreates within %s (limit %d); "+
					"LiteLLM accepts POST /v1/access_group but the entry never appears on the existence probe "+
					"(created-but-not-listed); parked to avoid a reconcile storm. Retrying after %s.",
					n, churnWindow, limit, recreateThrottleBackoff)
				if werr := r.writeStatus(ctx, &ag, metav1.ConditionFalse, reasonRecreateThrottled, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonRecreateThrottled)
				}
				metrics.ReconcileTotal.WithLabelValues(accessGroupKind, "success").Inc()
				return ctrl.Result{RequeueAfter: recreateThrottleBackoff}, nil
			}
		}

		created, cerr := snap.Client.CreateAccessGroup(ctx, &litellm.AccessGroupCreateRequest{
			AccessGroupName:    ag.Name,
			Description:        ag.Spec.Description,
			AccessModelNames:   rendered.Models,
			AccessMCPServerIDs: rendered.MCPServerIDs,
			AccessAgentIDs:     rendered.AgentIDs,
		})
		switch {
		case cerr == nil:
			newGroupID = created.AccessGroupID
			if !firstReconcile && ag.Status.ObservedGeneration > 0 {
				metrics.DriftCorrectedTotal.WithLabelValues("accessgroup", "create_missing").Inc()
				r.Churn.Record(req.NamespacedName)
			}
			logger.V(1).Info("access group created in LiteLLM", "accessGroupID", newGroupID)

		case rejectedStatus(cerr) == http.StatusConflict:
			// access_group_name is unique server-side. A 409 means a group
			// with this name already exists (operator restart, or an
			// out-of-band create). Adopt it by name and push our rendered
			// state onto it rather than parking the CR forever.
			adopted := r.resolveAccessGroupIDByName(ctx, snap.Client, ag.Name, logger)
			if adopted == "" {
				// 409 but the name is not listed — cannot adopt; surface the
				// original rejection.
				return r.classifyMutationError(ctx, &ag, logger, cerr, "POST /v1/access_group")
			}
			if _, uerr := snap.Client.UpdateAccessGroup(ctx, adopted, accessGroupUpdateBody(&ag, rendered)); uerr != nil {
				return r.classifyMutationError(ctx, &ag, logger, uerr, "PUT /v1/access_group (adopt)")
			}
			newGroupID = adopted
			logger.V(1).Info("adopted pre-existing LiteLLM access group by name", "accessGroupID", adopted)

		default:
			return r.classifyMutationError(ctx, &ag, logger, cerr, "POST /v1/access_group")
		}
	} else {
		// UPDATE path — PUT /v1/access_group/<id> with the id in the PATH.
		id := ag.Status.LastRendered.AccessGroupID
		if _, err := snap.Client.UpdateAccessGroup(ctx, id, accessGroupUpdateBody(&ag, rendered)); err != nil {
			return r.classifyMutationError(ctx, &ag, logger, err, "PUT /v1/access_group")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("accessgroup", "update_drifted").Inc()
		}
		newGroupID = id
		logger.V(1).Info("access group updated in LiteLLM", "accessGroupID", newGroupID)
	}

	// ─── Step 11: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	ag.Status.LastRendered = litellmv1alpha1.AccessGroupLastRenderedStatus{
		Hash:          currentRenderedHash,
		AccessGroupID: newGroupID,
		At:            &now,
	}
	if err := r.writeStatus(ctx, &ag, metav1.ConditionTrue, reasonSynced, "access group registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(accessGroupKind, ag.Name)
	metrics.ReconcileTotal.WithLabelValues(accessGroupKind, "success").Inc()
	logger.V(1).Info("access group reconciled", "accessGroupID", newGroupID, "hash", currentRenderedHash)

	// No RequeueAfter: the SafetyRelistRunnable owns the periodic tick.
	return ctrl.Result{}, nil
}

// accessGroupUpdateBody builds the PUT body. The three managed lists are
// carried through verbatim so an emptied one is sent as an explicit `[]` clear
// (ALWAYS-EMIT — an omitted field KEEPS the stale grant upstream). Name and
// description are pointers so "" is a real clear, not an omission.
//
// assigned_team_ids / assigned_key_ids are absent from the request struct BY
// DESIGN: omission means KEEP, which preserves human-managed assignments.
func accessGroupUpdateBody(ag *litellmv1alpha1.LiteLLMAccessGroup, rendered renderedAccessGroup) *litellm.AccessGroupUpdateRequest {
	name := ag.Name
	desc := ag.Spec.Description
	return &litellm.AccessGroupUpdateRequest{
		AccessGroupName:    &name,
		Description:        &desc,
		AccessModelNames:   rendered.Models,
		AccessMCPServerIDs: rendered.MCPServerIDs,
		AccessAgentIDs:     rendered.AgentIDs,
	}
}

// resolveAccessGroupIDByName re-resolves an access group's LiteLLM
// access_group_id from a metadata.name lookup. Used by the finalizer path when
// status.lastRendered.AccessGroupID is empty, and by the CREATE arm's 409
// adoption branch. Returns "" if the entry is absent or the LIST call fails
// non-fatally.
func (r *AccessGroupReconciler) resolveAccessGroupIDByName(ctx context.Context, llm *litellm.Client, name string, logger logr.Logger) string {
	entry, err := llm.GetAccessGroupByName(ctx, name)
	if err != nil {
		if errors.Is(err, litellm.ErrNotFound) {
			return ""
		}
		var auth401 *litellm.Auth401Error
		if errors.As(err, &auth401) {
			r.Cache.InvalidateOn401()
			logger.Info("name-resolve: 401 fast-path", "path", auth401.Path)
			return ""
		}
		logger.V(1).Info("name-resolve: GET /v1/access_group failed; treating as absent", "error", err)
		return ""
	}
	if entry == nil {
		return ""
	}
	return entry.AccessGroupID
}

// classifyMutationError handles §7.7 error classification for LiteLLM access
// group calls. See the shared classifyMutationError helper.
func (r *AccessGroupReconciler) classifyMutationError(ctx context.Context, ag *litellmv1alpha1.LiteLLMAccessGroup, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	snap := r.Cache.Snapshot()
	return classifyMutationError(ctx, logger, err, opDesc, accessGroupKind,
		func(c context.Context, s metav1.ConditionStatus, reason, msg string) error {
			return r.writeStatus(c, ag, s, reason, msg)
		},
		r.Cache.InvalidateOn401, snap.NormalizedRequeueOnRejectedAfter)
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact.
func (r *AccessGroupReconciler) writeStatus(
	ctx context.Context,
	ag *litellmv1alpha1.LiteLLMAccessGroup,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	cond := buildReadyCondition(ag.Generation, status, reason, message)
	desiredLastRendered := ag.Status.LastRendered
	desiredObservedGen := ag.Generation

	var fresh litellmv1alpha1.LiteLLMAccessGroup
	err := writeStatusWithRetry(ctx, r.Client, ag, &fresh, func(f *litellmv1alpha1.LiteLLMAccessGroup) {
		apimeta.SetStatusCondition(&f.Status.Conditions, cond)
		f.Status.ObservedGeneration = desiredObservedGen
		f.Status.LastRendered = desiredLastRendered
	})
	if err == nil {
		ag.Status = fresh.Status
		ag.ResourceVersion = fresh.ResourceVersion
	}
	recordReconcileMetric(accessGroupKind, ag.Namespace, reason)
	return err
}

// SetupWithManager registers the AccessGroupReconciler with controller-runtime.
//
// Watches:
//   - For(&LiteLLMAccessGroup{}) — primary watch.
//   - Watches(&LiteLLMConnection{}) — connection fan-in.
//
// No Secret watch — this CRD has no spec.secrets. No MCPServer/A2AAgent watch
// either: a spec.mcpServers / spec.agents name that is not yet registered
// parks the CR, and the SafetyRelistRunnable re-drives it.
func (r *AccessGroupReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error {
	if r.Churn == nil {
		r.Churn = newChurnGuard()
	}
	if r.RecreateLimit <= 0 {
		r.RecreateLimit = DefaultRecreateLimitPerMin
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMAccessGroup{}, builder.WithPredicates()).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToAccessGroups),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("accessgroup")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	if src := ConnectionRebuiltSource(r.ConnectionRebuilt, r.connectionToAccessGroups); src != nil {
		b = b.WatchesRawSource(src)
	}

	if len(safetyRelistCh) > 0 && safetyRelistCh[0] != nil {
		ch := safetyRelistCh[0]
		b = b.WatchesRawSource(source.TypedFunc[reconcile.Request](
			func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case req, ok := <-ch:
							if !ok {
								return
							}
							q.Add(req)
						}
					}
				}()
				return nil
			},
		))
	}

	return b.Complete(r)
}

// ListAccessGroupRequests lists every LiteLLMAccessGroup in namespace and
// returns their reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests —
// see ListModelRequests for the shared contract.
func ListAccessGroupRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMAccessGroupList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return reqs, nil
}
