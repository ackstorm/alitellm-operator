// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/identity"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/substitution"
)

// mcpServerFinalizer is the finalizer name managed by the LiteLLMMCPServer reconciler
// per spec §7.5. Issuing DELETE /v1/mcp/server/<server_id> against LiteLLM
// removes the MCP server entry from LiteLLM before the CR is fully removed
// from etcd.
const mcpServerFinalizer = "mcpservers.litellm.ackstorm.ai/finalizer"

// mcpServerKind is the metric label for LiteLLMMCPServer CRs.
const mcpServerKind = "LiteLLMMCPServer"

// MCPServerSecretRefIndexField is the field indexer path registered in
// cmd/main.go for reverse-mapping Secret names back to MCPServers that
// reference them (Phase 3 D-06 pattern carry-forward for SEC-09 rotation
// propagation).
const MCPServerSecretRefIndexField = ".spec.secrets[*].secretRef.name" // #nosec G101 -- field-selector JSONPath, not a credential

// IndexMCPServerSecretRefs is the field indexer function for
// MCPServerSecretRefIndexField. Mirrors IndexModelSecretRefs verbatim,
// specialized for the LiteLLMMCPServer type.
func IndexMCPServerSecretRefs(o client.Object) []string {
	mcp, ok := o.(*litellmv1alpha1.LiteLLMMCPServer)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(mcp.Spec.Secrets))
	for _, s := range mcp.Spec.Secrets {
		names = append(names, s.SecretRef.Name)
	}
	return names
}

// Task 0 audit (per checker WARNING #6 — read-only events RBAC inheritance
// check): zero existing kubebuilder:rbac:events markers in
// internal/controller/ AND zero events stanzas in config/rbac/role.yaml
// (verified 2026-05-16; audit captured in /tmp/05-01-task0-rbac.out).
// MCPServer reconciler ADDS the events marker below as the package-wide
// grant. Phase 5 plans 05-03 (A2A) and 05-04 (MSDisc) INHERIT this marker
// — they do NOT duplicate it (kubebuilder marker scope is per-package,
// not per-file). Documented in 05-01-SUMMARY.md.

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers/finalizers,verbs=update

// MCPServerReconciler reconciles LiteLLMMCPServer CRs against LiteLLM 1.83.10 per
// spec §6.4 + §7.3 and Phase 5 CONTEXT.md D-01.D-10.
//
// State machine (per-reconcile) — mirrors ModelReconciler shape:
//
// - Step 1: Fetch the CR (NotFound → return nil).
// - Step 2a: DeletionTimestamp set → issue DELETE /v1/mcp/server/<id>
// (with name-resolve fallback via ListMCPServers + in-memory filter
// when ServerID is empty) → RemoveFinalizer → Update.
// - Step 2b: Finalizer absent → AddFinalizer → Update → return.
// - Step 3: Connection-gating per Phase 3 D-08: !snap.Ready → writeStatus
// (LiteLLMUnavailable, echo-reason) → return nil.
// - Step 3.5: SEC-03 uniqueness of spec.secrets[].as values (runtime check
// mirroring Phase 3 LiteLLMModel).
// - Step 4: Resolve spec.secrets[] → secretMap.
// - Step 5: Decode spec.params into paramsMap; apply single-pass
// substitution (Phase 5 D-04 — MCP is single-pass; A2A is two-pass).
// - Step 6: SEC-07 UnusedSecretRef detection → Event per unused as.
// - Step 7: Compute currentRenderedHash (SHA-256 of canonical JSON body
// {server_name, url, transport, params}).
// - Step 8: Hash-equal steady-state short-circuit (no mutation).
// - Step 9: Branch CREATE (status.lastRendered.ServerID == "") vs UPDATE.
// UPDATE arm is the simple PUT /v1/mcp/server per Probe 10c verdict ✓
// on LiteLLM 1.83.10-stable (Phase 5 D-01 "If positive" branch — NO
// delete-and-recreate path is committed; the single-arm-enforcement
// grep gate documented in 05-01-PLAN.md acceptance criteria asserts
// the absence of removedKeys/setDiff/shrinkage signatures).
// - Step 10: Classify mutation errors per §7.7 — 401 → InvalidateOn401 +
// nil return (anti-storm REL-06); 4xx → LiteLLMRejected + nil return;
// 5xx/network → return err for backoff.
// - Step 11: Update status (LastRendered + Ready=Synced) on success.
//
// Anti-patterns avoided (Phase 5 PATTERNS.md L527+):
// - NO RequeueAfter anywhere (REL-02 — MCPServer is event-driven only).
// - NO Owns(.) — child resources are not owned by this controller;
// the MCPServerDiscovery reconciler is the parent for generated children
// (cascade-delete via blockOwnerDeletion=true, see).
// - NO ProjectionOverride Event emission — LiteLLMMCPServer has no structural
// overlay collisions (only A2A does, per Phase 5 D-05).
// - NO comparison against LiteLLM response (Phase 3 D-01 — operator-side
// hash only).
// - NO global LIST-and-prune (Phase 3 OWN-01 — one name per reconcile).
//
// Verdict ✓ per 05-00-SUMMARY.md — UPDATE arm uses simple PUT /v1/mcp/server.
type MCPServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Cache is the interface (per Phase 2 D-12) — NEVER the concrete
	// *connection.Cache. Tests substitute fakes without code change.
	Cache connection.ConnectionCache
	// Recorder emits Kubernetes Events on the LiteLLMMCPServer object —
	// Normal/UnusedSecretRef (SEC-07). Non-nil in production; tests pass
	// mgr.GetEventRecorderFor("mcpserver-controller").
	Recorder  record.EventRecorder
	Namespace string
	Log       logr.Logger
	// BootEvents (FIX2.txt H-2) — optional BootSweeper channel. nil-safe.
	BootEvents <-chan event.GenericEvent
}

// Reconcile implements the LiteLLMMCPServer state machine.
//
//nolint:gocyclo // Linear state machine; splitting obscures the §7.3 mapping.
func (r *MCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("mcpserver", req.NamespacedName)

	// ─── Step 1: Fetch the CR ──────────────────────────────────────────────
	var mcp litellmv1alpha1.LiteLLMMCPServer
	if err := r.Get(ctx, req.NamespacedName, &mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ─── Step 2a: Deletion path ────────────────────────────────────────────
	if !mcp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&mcp, mcpServerFinalizer) {
			snap := r.Cache.Snapshot()
			if snap.Ready {
				serverID := mcp.Status.LastRendered.ServerID
				if serverID == "" {
					// Phase 5 D-02 stale-status fallback: re-resolve by name
					// via ListMCPServers + in-memory filter on metadata.name.
					// FIX2.txt H-9: probe sanitized first, then original
					// metadata.name (orphan-adoption fallback).
					sanitized := litellm.SanitizeMCPServerName(mcp.Name, snap.MCPToolPrefixSeparator)
					if resolved := r.resolveServerIDByName(ctx, snap.Client, sanitized, mcp.Name, logger); resolved != "" {
						serverID = resolved
					}
				}
				if serverID != "" {
					if err := snap.Client.DeleteMCPServer(ctx, serverID); err != nil {
						var auth401 *litellm.Auth401Error
						if errors.As(err, &auth401) {
							r.Cache.InvalidateOn401()
							logger.Info("deletion: 401 fast-path; cache invalidated", "path", auth401.Path)
							// Anti-storm: fall through to remove finalizer.
						} else {
							// Transient error — return for backoff. Finalizer stays.
							return ctrl.Result{}, err
						}
					} else {
						metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished").Inc()
						logger.Info("finalizer removed; LiteLLM MCP server deleted", "serverID", serverID)
					}
				} else {
					logger.Info("finalizer removed; LiteLLM entry already absent (no pinned ID, name-resolve returned empty)", "name", mcp.Name)
				}
			} else {
				logger.Info("LiteLLM unavailable on deletion; finalizer removed; MCP entry MAY persist until next reconcile with valid connection")
			}

			// OBS-03: drop the cr_status_age_seconds label before the CR is gone
			// so /metrics cardinality never grows monotonically (T-07-01-01).
			metrics.CRStatusAgeTracker.Forget(mcpServerKind, mcp.Name)
			controllerutil.RemoveFinalizer(&mcp, mcpServerFinalizer)
			if err := r.Update(ctx, &mcp); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 2b: Finalizer add path ───────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&mcp, mcpServerFinalizer) {
		controllerutil.AddFinalizer(&mcp, mcpServerFinalizer)
		if err := r.Update(ctx, &mcp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ─── Step 3: Connection-gating (Phase 3 D-08) ──────────────────────────
	snap := r.Cache.Snapshot()
	if !snap.Ready {
		reason := snap.Reason
		if reason == "" {
			reason = reasonConnecting
		}
		msg := fmt.Sprintf("LiteLLMConnection/default not Ready (reason: %s)", reason)
		if err := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); err != nil {
			logStatusUpdateErr(logger, err, "reason", reasonLiteLLMUnavailable)
		}
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	// ─── Step 3.5: SEC-03 uniqueness of spec.secrets[].as values ──────────
	{
		seen := make(map[string]struct{}, len(mcp.Spec.Secrets))
		for _, entry := range mcp.Spec.Secrets {
			if _, exists := seen[entry.As]; exists {
				msg := fmt.Sprintf("spec.secrets[]: duplicate as value %q (SEC-03: must be unique within a LiteLLMMCPServer)", entry.As)
				if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
				}
				metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
				return ctrl.Result{}, nil
			}
			seen[entry.As] = struct{}{}
		}
	}

	// ─── Step 4: Resolve Secrets referenced by spec.secrets[] ─────────────
	secretMap := make(map[string]string)
	for _, entry := range mcp.Spec.Secrets {
		var secret corev1.Secret
		secretKey := types.NamespacedName{
			Namespace: mcp.Namespace,
			Name:      entry.SecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				msg := mcp.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
				if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
					logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
				}
				metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
				return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
			}
			return ctrl.Result{}, err
		}
		val, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			msg := mcp.Namespace + "/" + entry.SecretRef.Name + ":" + entry.SecretRef.Key + " not found"
			if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
			}
			metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
			return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
		}
		secretMap[entry.As] = string(val)
	}

	// ─── Step 5: Decode spec.params + single-pass substitution ────────────
	paramsMap := make(map[string]any)
	if len(mcp.Spec.Params.Raw) > 0 {
		if err := json.Unmarshal(mcp.Spec.Params.Raw, &paramsMap); err != nil {
			msg := "spec.params: invalid JSON: " + err.Error()
			if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, "InvalidConfig", msg); werr != nil {
				logStatusUpdateErr(logger, werr, "reason", "InvalidConfig")
			}
			return ctrl.Result{}, nil
		}
	}
	// FIX5 H-1: drop reserved structural keys from paramsMap BEFORE
	// substitution / hash / extraction so downstream state (hash,
	// status.lastRendered.ParamsKeys, the extracted struct) is consistent.
	// Reserved keys (server_id, server_name, alias, url, transport,
	// spec_path) are stamped from the CR — a user-supplied value here
	// would be a hijack vector, so we ignore the bag entries silently.
	for k := range reservedMCPParamKeys {
		delete(paramsMap, k)
	}

	// Phase 5 D-04: MCP is single-pass on spec.params (vs A2A's two-pass
	// across spec.params + spec.agentCard).
	referencedParams, missingParams, _ := substitution.Substitute(paramsMap, secretMap)
	if len(missingParams) > 0 {
		msg := fmt.Sprintf("placeholder {{%s}} has no matching spec.secrets[].as", missingParams[0])
		if werr := r.writeStatus(ctx, &mcp, metav1.ConditionFalse, reasonSecretNotFound, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonSecretNotFound)
		}
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		return ctrl.Result{RequeueAfter: snap.NormalizedRequeueOnRejectedAfter()}, nil
	}

	// ─── Step 6: SEC-07 UnusedSecretRef detection ──────────────────────────
	referencedSet := make(map[string]struct{})
	for _, n := range referencedParams {
		referencedSet[n] = struct{}{}
	}
	for _, entry := range mcp.Spec.Secrets {
		if _, ok := referencedSet[entry.As]; !ok {
			r.Recorder.Eventf(&mcp, corev1.EventTypeNormal, "UnusedSecretRef",
				"spec.secrets[].as %q is declared but unreferenced by any {{NAME}} placeholder in spec.params",
				entry.As)
		}
	}

	// ─── Step 7: Compute currentRenderedHash (Phase 3 D-01) ───────────────
	// Build the canonical body: structural overlays {server_name, url,
	// transport} + the pass-through params bag. The structural overlays
	// participate in the hash so endpoint/transport edits drive UPDATE.
	//
	// FIX.txt H-1 (2026-05-22): LiteLLM rejects its MCP_TOOL_PREFIX_SEPARATOR
	// inside server_name. The reconciler sanitizes the K8s metadata.name
	// per LiteLLMConnection.spec.mcpToolPrefixSeparator (the snapshot
	// carries the value). The hash is computed on the SANITIZED form so
	// drift detection matches what LiteLLM actually stores; K8s
	// metadata.name remains untouched.
	sanitizedName := litellm.SanitizeMCPServerName(mcp.Name, snap.MCPToolPrefixSeparator)
	merged := map[string]any{
		"server_name": sanitizedName,
		"url":         mcp.Spec.Endpoint,
		"transport":   mcp.Spec.Transport,
		"params":      paramsMap,
	}
	canonicalBytes, err := canonicalJSON(merged)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mcpserver controller: canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	currentRenderedHash := fmt.Sprintf("%x", sum)

	// ─── Step 8: Hash-equal steady state ──────────────────────────────────
	if mcp.Status.LastRendered.Hash == currentRenderedHash &&
		mcp.Status.LastRendered.ServerID != "" &&
		mcp.Status.ObservedGeneration == mcp.Generation {
		metrics.CRStatusAgeTracker.RecordSuccess(mcpServerKind, mcp.Name)
		return ctrl.Result{}, nil
	}

	// ─── Step 9: Branch CREATE vs UPDATE (simple PUT — verdict ✓) ─────────
	//
	// Phase 3 OWN-04 first-reconcile sentinel inherited: on observedGeneration
	// == 0 OR lastRendered.hash == "", drift counters are suppressed
	// (the first CREATE is not a "drift correction"; it's the user's
	// initial registration).
	firstReconcile := mcp.Status.ObservedGeneration == 0 || mcp.Status.LastRendered.Hash == ""

	// Construct the body. FIX5 H-1: every top-level key in spec.params that
	// corresponds to a modeled field on litellm.MCPServerRequest /
	// MCPServerUpdateRequest is forwarded to LiteLLM. Reserved structural
	// keys (server_id, server_name, alias, url, transport, spec_path) are
	// stamped from the CR — see reservedMCPParamKeys.
	ext := extractMCPParams(paramsMap)

	var newServerID string
	// FIX2.txt H-9 orphan adoption (2026-05-22): when ServerID is empty,
	// probe LiteLLM for an existing record under EITHER the sanitized name
	// or the original K8s metadata.name BEFORE issuing a CREATE. If found,
	// adopt the existing record and switch to the UPDATE arm — this heals
	// the v0.1.1 → v0.1.3 upgrade-orphan scenario without requiring a
	// kubectl probe-and-edit dance.
	if mcp.Status.LastRendered.ServerID == "" {
		if adopted := r.resolveServerIDByName(ctx, snap.Client, sanitizedName, mcp.Name, logger); adopted != "" {
			mcp.Status.LastRendered.ServerID = adopted
		}
	}
	// Verdict ✓ per 05-00-SUMMARY.md — UPDATE arm uses simple PUT;
	// delete-and-recreate (with paramsKeys shrinkage detection) is NOT
	// present in this codebase.
	if mcp.Status.LastRendered.ServerID == "" {
		// CREATE path — first reconcile or stale status.
		// CR-10 / D-7.1-10: set Alias = ServerName per LiteLLM 1.83.10 requirement.
		// Probe 10c (HTTP 201) sent alias=server_name; the dogfood LiteLLMMCPServer/exa-mcp
		// (HTTP 400) did not include alias. The spec marks alias optional, but
		// 1.83.10 validates it at create time (diagnostic-first diff, 2026-05-19).
		// FIX4.txt H-1: stamp operator identity into the mcp_info bag.
		// LiteLLM 1.83.10 /v1/mcp/server has no native audit field at the
		// top level, but mcp_info is freeform and persisted verbatim.
		// CREATE stamps both created_by + updated_by.
		ext.MCPInfo = stampMCPIdentity(ext.MCPInfo, true)
		createReq := &litellm.MCPServerRequest{
			ServerName:                sanitizedName,
			Alias:                     sanitizedName, // alias = server_name per 1.83.10 (D-7.1-10)
			URL:                       mcp.Spec.Endpoint,
			Transport:                 mcp.Spec.Transport,
			Description:               ext.Description,
			AuthType:                  ext.AuthType,
			Credentials:               ext.Credentials,
			MCPInfo:                   ext.MCPInfo,
			MCPAccessGroups:           ext.MCPAccessGroups,
			AllowedTools:              ext.AllowedTools,
			ToolNameToDisplayName:     ext.ToolNameToDisplayName,
			ToolNameToDescription:     ext.ToolNameToDescription,
			ExtraHeaders:              ext.ExtraHeaders,
			StaticHeaders:             ext.StaticHeaders,
			Command:                   ext.Command,
			Args:                      ext.Args,
			Env:                       ext.Env,
			AuthorizationURL:          ext.AuthorizationURL,
			TokenURL:                  ext.TokenURL,
			RegistrationURL:           ext.RegistrationURL,
			OAuth2Flow:                ext.OAuth2Flow,
			AllowAllKeys:              ext.AllowAllKeys,
			AvailableOnPublicInternet: ext.AvailableOnPublicInternet,
		}
		result, err := snap.Client.CreateMCPServer(ctx, createReq)
		if err != nil {
			return r.classifyMutationError(ctx, &mcp, logger, err, "POST /v1/mcp/server")
		}
		newServerID = result.ServerID
		// Phase 3 OWN-04 + Phase 5 : suppress create_missing on the
		// very first reconcile (ObservedGeneration == 0) — the user's initial
		// POST is not a "drift correction". On subsequent re-creates (after a
		// delete-and-recreate cycle OR external-vanish recovery where
		// ObservedGeneration > 0 already), the counter DOES increment.
		if !firstReconcile && mcp.Status.ObservedGeneration > 0 {
			metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing").Inc()
		}
		logger.V(1).Info("mcp server created in LiteLLM", "serverID", newServerID)
	} else {
		// UPDATE path — simple PUT /v1/mcp/server (verdict ✓ per
		// 05-00-SUMMARY.md; PUT IS wholesale-replace on 1.83.10-stable).
		// FIX4.txt H-1: stamp updated_by only on UPDATE (LiteLLM keeps the
		// original creator).
		ext.MCPInfo = stampMCPIdentity(ext.MCPInfo, false)
		updateReq := &litellm.MCPServerUpdateRequest{
			ServerID:                  mcp.Status.LastRendered.ServerID,
			ServerName:                sanitizedName,
			URL:                       mcp.Spec.Endpoint,
			Transport:                 mcp.Spec.Transport,
			Description:               ext.Description,
			AuthType:                  ext.AuthType,
			Credentials:               ext.Credentials,
			MCPInfo:                   ext.MCPInfo,
			MCPAccessGroups:           ext.MCPAccessGroups,
			AllowedTools:              ext.AllowedTools,
			ToolNameToDisplayName:     ext.ToolNameToDisplayName,
			ToolNameToDescription:     ext.ToolNameToDescription,
			ExtraHeaders:              ext.ExtraHeaders,
			StaticHeaders:             ext.StaticHeaders,
			Command:                   ext.Command,
			Args:                      ext.Args,
			Env:                       ext.Env,
			AuthorizationURL:          ext.AuthorizationURL,
			TokenURL:                  ext.TokenURL,
			RegistrationURL:           ext.RegistrationURL,
			AllowAllKeys:              ext.AllowAllKeys,
			AvailableOnPublicInternet: ext.AvailableOnPublicInternet,
		}
		if _, err := snap.Client.UpdateMCPServer(ctx, updateReq); err != nil {
			return r.classifyMutationError(ctx, &mcp, logger, err, "PUT /v1/mcp/server")
		}
		if !firstReconcile {
			metrics.DriftCorrectedTotal.WithLabelValues("mcp", "update_drifted").Inc()
		}
		// UPDATE keeps the same LiteLLM server_id (verdict ✓ — PUT
		// preserves the row's identity).
		newServerID = mcp.Status.LastRendered.ServerID
		logger.V(1).Info("mcp server updated in LiteLLM (simple PUT)", "serverID", newServerID)
	}

	// ─── Step 11: Update status on success ─────────────────────────────────
	now := metav1.NewTime(time.Now())
	mcp.Status.LastRendered = litellmv1alpha1.MCPServerLastRenderedStatus{
		Hash:       currentRenderedHash,
		ParamsKeys: sortedKeys(paramsMap),
		ServerID:   newServerID,
		At:         &now,
	}
	if err := r.writeStatus(ctx, &mcp, metav1.ConditionTrue, reasonSynced, "mcp server registered"); err != nil {
		logStatusUpdateErr(logger, err, "reason", reasonSynced)
		if apierrors.IsConflict(err) {
			// Conflict (RV bump, CR deleted, UID precondition) — informer
			// re-enqueues with fresh state; suppress controller-runtime's
			// ERROR "Reconciler error" log + backoff for this error class.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.CRStatusAgeTracker.RecordSuccess(mcpServerKind, mcp.Name)
	metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
	logger.V(1).Info("mcp server reconciled", "serverID", newServerID, "hash", currentRenderedHash)

	return ctrl.Result{}, nil
}

// resolveServerIDByName re-resolves a LiteLLMMCPServer's LiteLLM server_id
// from a metadata.name lookup via ListMCPServers + in-memory filter.
//
// Probes `sanitizedName` first. If no match AND `originalName` differs
// from sanitizedName, probes `originalName` as a fallback (FIX2.txt
// HIGH-9 orphan adoption, 2026-05-22): a pre-v0.1.2 LiteLLM record was
// created under the K8s metadata.name without sanitization; the v0.1.2
// sanitizer mangled the name and broke the link. With the v0.1.3
// no-op-on-safe sanitizer, sanitized==original for inputs without the
// forbidden char, so the fallback only triggers for inputs that DID
// contain the forbidden char (rare, but heals the upgrade-orphan
// scenario without operator intervention).
//
// Used by:
//   - finalizer path when status.lastRendered.ServerID is empty (Phase 5
//     D-02 stale-status fallback).
//   - CREATE-vs-UPDATE branch in the main reconcile loop (FIX2.txt H-9):
//     probe before POSTing so an orphaned LiteLLM record gets adopted as
//     an UPDATE instead of triggering a 4xx duplicate-name CREATE.
//
// Returns "" if neither name matches or the LIST call fails non-fatally.
// The caller decides what to do with "" (finalizer drains anyway; the
// CREATE branch proceeds with POST).
func (r *MCPServerReconciler) resolveServerIDByName(ctx context.Context, llm *litellm.Client, sanitizedName, originalName string, logger logr.Logger) string {
	entries, err := llm.ListMCPServers(ctx)
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
		logger.V(1).Info("name-resolve: ListMCPServers failed; treating as absent", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.ServerName == sanitizedName {
			return e.ServerID
		}
	}
	if originalName != "" && originalName != sanitizedName {
		for _, e := range entries {
			if e.ServerName == originalName {
				logger.Info("name-resolve: adopted orphan under pre-sanitize name (FIX2.txt H-9)",
					"sanitizedName", sanitizedName, "originalName", originalName, "serverID", e.ServerID)
				return e.ServerID
			}
		}
	}
	return ""
}

// classifyMutationError handles §7.7 error classification for LiteLLM
// mutation calls (CreateMCPServer / UpdateMCPServer / DeleteMCPServer):
// - Auth401Error → cache invalidation + LiteLLMUnavailable + nil return
// (anti-storm REL-06).
// - 4xx (non-401) → LiteLLMRejected + nil return (deterministic).
// - 5xx / network → return err for controller-runtime exponential backoff.
func (r *MCPServerReconciler) classifyMutationError(ctx context.Context, mcp *litellmv1alpha1.LiteLLMMCPServer, logger logr.Logger, err error, opDesc string) (ctrl.Result, error) {
	var auth401 *litellm.Auth401Error
	if errors.As(err, &auth401) {
		r.Cache.InvalidateOn401()
		msg := "401 from LiteLLM on " + opDesc + "; cache invalidated, re-probe enqueued"
		if werr := r.writeStatus(ctx, mcp, metav1.ConditionFalse, reasonLiteLLMUnavailable, msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", reasonLiteLLMUnavailable)
		}
		logger.Info("401 fast-path: invalidating connection cache", "path", auth401.Path, "op", opDesc)
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		return ctrl.Result{}, nil
	}

	errStr := err.Error()
	is4xx := false
	for code := 400; code < 500; code++ {
		prefix := fmt.Sprintf("litellm: %d on", code)
		if len(errStr) >= len(prefix) && errStr[:len(prefix)] == prefix {
			is4xx = true
			break
		}
	}

	if is4xx {
		msg := rejectedMessage(opDesc, err, errStr)
		if werr := r.writeStatus(ctx, mcp, metav1.ConditionFalse, "LiteLLMRejected", msg); werr != nil {
			logStatusUpdateErr(logger, werr, "reason", "LiteLLMRejected")
		}
		logger.Info("LiteLLM rejected request", "op", opDesc, "error", errStr)
		metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "success").Inc()
		// FIX2.txt H-2: periodically re-reconcile so an upstream fix
		// (or a CR edit landing during the rate-limiter quiet window)
		// heals without external poke.
		return ctrl.Result{RequeueAfter: r.Cache.Snapshot().NormalizedRequeueOnRejectedAfter()}, nil
	}

	// 5xx / network transient — return err for controller-runtime backoff.
	logger.V(1).Info("transient error from LiteLLM; returning for backoff", "op", opDesc, "error", errStr)
	metrics.ReconcileTotal.WithLabelValues(mcpServerKind, "error").Inc()
	return ctrl.Result{}, err
}

// writeStatus sets the Ready condition and updates the status subresource.
// §9.1: the message parameter is the caller's responsibility — this helper
// does not redact. Callers MUST ensure no secret material reaches `message`.
func (r *MCPServerReconciler) writeStatus(
	ctx context.Context,
	mcp *litellmv1alpha1.LiteLLMMCPServer,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Uses Update (not Patch + MergeFrom) because callers mutate
	// mcp.Status.LastRendered before this call; a MergeFrom orig captured
	// here would already carry the mutation and the resulting patch would
	// omit ServerID, leaving the server with an empty value and causing a
	// duplicate POST /mcp/server/add on the next reconcile. 409 conflict
	// noise on this Update path is demoted to V(1) by logStatusUpdateErr
	// at each call site.
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: mcp.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&mcp.Status.Conditions, cond)
	mcp.Status.ObservedGeneration = mcp.Generation
	// FIX2.txt LOW-6: per-CR reconcile-outcome counter labeled by
	// kind/namespace/result. Fired here so every status-write also
	// surfaces on the prometheus dashboard without an extra call site.
	metrics.LitellmOperatorReconcileTotal.WithLabelValues(
		mcpServerKind, mcp.Namespace, metrics.ReasonToReconcileResult(reason),
	).Inc()
	return r.Status().Update(ctx, mcp)
}

// secretToMCPServers maps a Secret update event to the set of LiteLLMMCPServer
// CRs that reference it via spec.secrets[].secretRef.name (Phase 3 D-06
// rotation-propagation pattern). Uses the field indexer registered in
// cmd/main.go.
func (r *MCPServerReconciler) secretToMCPServers(ctx context.Context, obj client.Object) []reconcile.Request {
	var mcpList litellmv1alpha1.LiteLLMMCPServerList
	if err := r.List(ctx, &mcpList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{MCPServerSecretRefIndexField: obj.GetName()},
	); err != nil {
		r.Log.V(1).Info("secretToMCPServers: list failed; skipping", "error", err)
		return nil
	}
	out := make([]reconcile.Request, 0, len(mcpList.Items))
	for i := range mcpList.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&mcpList.Items[i]),
		})
	}
	return out
}

// SetupWithManager registers the MCPServerReconciler with controller-runtime.
//
// Watches:
// - For(&LiteLLMMCPServer{}) — primary watch.
// - Watches(&Secret{}, secretToMCPServers) — SEC-09 rotation propagation.
//
// Named("mcpserver") — controller registry name (Phase 5 PATTERNS.md L506).
// No Owns(.) and no safety re-list channel — Phase 5 may add
// these if cross-CR vanish detection requires them (Phase 7 dogfood gate).
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&litellmv1alpha1.LiteLLMMCPServer{}, builder.WithPredicates()).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToMCPServers),
		).
		Watches(
			&litellmv1alpha1.LiteLLMConnection{},
			handler.EnqueueRequestsFromMapFunc(r.connectionToMCPServers),
			builder.WithPredicates(connectionReadyTransition()),
		).
		WithOptions(transientBackoffOptions()).
		Named("mcpserver")
	if src := BootEventsSource(r.BootEvents); src != nil {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}

// stampMCPIdentity injects identity.Operator() into the mcp_info bag.
// LiteLLM 1.83.10 /v1/mcp/server has no native audit field at the top
// level, but mcp_info is freeform and persisted verbatim. On CREATE
// both created_by + updated_by are stamped; on UPDATE only updated_by
// (LiteLLM-side audit semantics — original creator is immutable).
// FIX4.txt H-1.
func stampMCPIdentity(mcpInfo map[string]any, includeCreatedBy bool) map[string]any {
	if mcpInfo == nil {
		mcpInfo = map[string]any{}
	}
	if includeCreatedBy {
		mcpInfo["created_by"] = identity.Operator()
	}
	mcpInfo["updated_by"] = identity.Operator()
	return mcpInfo
}
