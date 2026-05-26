// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// §10 metric set. All 11 metrics are declared as package-level vars so
// Phase 3+ reconcilers can increment them directly (e.g.
// metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing").Inc).

// ReconcileTotal — spec §10: counter labeled by kind, result.
// kinds ∈ {LiteLLMConnection, Model, ModelDiscovery, MCPServer,
// MCPServerDiscovery, A2AAgent, Team}; result ∈ {success, error, requeued}.
var ReconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "reconcile_total",
		Help: "Reconciles by kind and outcome (§10).",
	},
	[]string{"kind", "result"},
)

// LitellmOperatorReconcileTotal — FIX2.txt LOW-6 (2026-05-22). Counter
// labeled by kind, namespace, result with a richer result enum than
// ReconcileTotal's {success, error, requeued}. Lets dashboards slice
// provider rejection patterns (e.g. "Bedrock IAM scope errors on
// namespace=ackstorm") without a kubectl-describe sweep — the per-CR
// rejection reason is encoded in the result label.
//
//	result ∈ {
//	  "synced",         // Ready=True written
//	  "rejected",       // LiteLLMRejected (deterministic upstream 4xx)
//	  "transient_error",// LiteLLMUnavailable (5xx, transport, 401)
//	  "secret_missing", // SecretNotFound
//	  "unreachable",    // Connection cache not Ready
//	  "invalid_config", // CR-side InvalidConfig (CEL miss, JSON parse, etc.)
//	}
var LitellmOperatorReconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "litellm_operator",
		Name:      "reconcile_total",
		Help:      "Reconciles by kind/namespace/result with reason-derived result enum (FIX2.txt LOW-6).",
	},
	[]string{"kind", "namespace", "result"},
)

// ReasonToReconcileResult maps a condition Reason string to the result
// label used on LitellmOperatorReconcileTotal. Unknown reasons map to
// "synced" (so a Ready=True with no Reason populated still increments
// the success bucket). Centralized so all reconcilers stamp consistent
// label values.
func ReasonToReconcileResult(reason string) string {
	switch reason {
	case "LiteLLMRejected":
		return "rejected"
	case "LiteLLMUnavailable", "BadMasterKey", "Connecting", "Unreachable":
		return "transient_error"
	case "SecretNotFound":
		return "secret_missing"
	case "InvalidConfig":
		return "invalid_config"
	case "":
		return "synced"
	default:
		return "synced"
	}
}

// ReconcileDurationSeconds — spec §10: histogram labeled by kind.
var ReconcileDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "reconcile_duration_seconds",
		Help:    "Reconcile duration by kind (§10).",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"kind"},
)

// LiteLLMAPIRequestDurationSeconds — spec §10: histogram labeled by
// operation and status (2xx/4xx/5xx/error).
var LiteLLMAPIRequestDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "litellm_api_request_duration_seconds",
		Help:    "LiteLLM REST request duration by operation and status (§10).",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"operation", "status"},
)

// LiteLLMAPIErrorsTotal — spec §10: counter labeled by operation and
// status (4xx/5xx/error).
var LiteLLMAPIErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "litellm_api_errors_total",
		Help: "LiteLLM REST errors by operation and status (§10).",
	},
	[]string{"operation", "status"},
)

// DiscoveryRefreshTotal — spec §10: counter labeled by kind, source, result.
// kind ∈ {ModelDiscovery, MCPServerDiscovery, A2AAgentDiscovery} (subset
// of the overall kind enum that participates in discovery);
// source ∈ {anthropic, bedrock, gemini, kubeai, openai, toolhive};
// result ∈ {success, error}.
var DiscoveryRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "discovery_refresh_total",
		Help: "Discovery refresh attempts by kind, source, and outcome (§10).",
	},
	[]string{"kind", "source", "result"},
)

// DiscoveryGeneratedCount — spec §10 / _FINALv3 (D-10): gauge labeled by
// kind, source. Renamed from DiscoveryRegisteredCount per Phase 4 CONTEXT.md
// D-10: Discovery (Pipeline B) emits K8s child Model CRs and NEVER calls
// LiteLLM — the metric name now reflects "child CRs generated" rather than
// the retired "names registered in LiteLLM" semantic that belonged to the
// pre-_FINALv3 single-pipeline model.
var DiscoveryGeneratedCount = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "discovery_generated_count",
		Help: "Count of K8s child Model CRs generated per discovery kind+source (§10).",
	},
	[]string{"kind", "source"},
)

// DiscoverySkippedTotal — spec §10: counter labeled by kind, reason.
// reasons ∈ {ExplicitModelExists, DuplicateDiscovery, InvalidDiscoveredName,
// EndpointUnknown, ExplicitMCPServerExists}.
var DiscoverySkippedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "discovery_skipped_total",
		Help: "Discovery items skipped, by kind and reason (§10).",
	},
	[]string{"kind", "reason"},
)

// DiscoveryFailedTotal — spec §10 / _FINALv3 (D-10): counter labeled by
// kind, reason. reasons ∈ {ChildCRWriteFailed} — narrowed in _FINALv3 from
// the pre-_FINALv3 {LiteLLMRejected, LiteLLMUnavailable} pair because
// Discovery never calls LiteLLM (MDISC-27); apiserver-side child SSA write
// failure is now the sole Discovery-level failure reason.
var DiscoveryFailedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "discovery_failed_total",
		Help: "Discovery items that failed to register, by kind and reason (§10).",
	},
	[]string{"kind", "reason"},
)

// ChildCRWritesTotal — Phase 4 OBS-04: counter labeled by parent
// Discovery kind, SSA action (create|update|delete), and outcome
// (success|error). Incremented on every K8s child CR write the Discovery
// reconciler performs (Pipeline B). Label combos pre-touched in init so
// /metrics enumerates the full surface on first scrape (Assumption A5 /
// AC-O1). Inherited by MCPServerDiscovery in Phase 5.
var ChildCRWritesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "child_cr_writes_total",
		Help: "K8s child CR writes by parent Discovery kind, action, and outcome (§10).",
	},
	[]string{"kind", "action", "result"},
)

// DriftCorrectedTotal — spec §10: counter labeled by domain, action.
// domain ∈ {model, mcp, a2a, team};
// action ∈ {create_missing, update_drifted, delete_vanished}.
var DriftCorrectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "drift_corrected_total",
		Help: "LiteLLM writes issued to correct drift, by domain and action (§10).",
	},
	[]string{"domain", "action"},
)

// DeletionOrphanedTotal — Issue #23: counter incremented on every code
// path where the operator removed a finalizer without confirming the
// LiteLLM-side delete (deletionPolicy=Orphan + ack-missing). Labeled
// by kind so operators can alert on a specific CRD's orphan rate.
var DeletionOrphanedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "litellm_operator_deletion_orphaned_total",
		Help: "Count of CRs whose finalizer was removed without LiteLLM-side delete ack (deletionPolicy=Orphan).",
	},
	[]string{"kind"},
)

// ConnectionReady — spec §10: gauge labeled by reason.
// reasons ∈ {Synced, Connecting, Absent, Unreachable, BadMasterKey,
// SecretNotFound}. Exactly one reason should be 1 at any time.
var ConnectionReady = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "connection_ready",
		Help: "LiteLLMConnection readiness gauge (one-hot across reason labels, §10).",
	},
	[]string{"reason"},
)

// CRStatusAgeTracker — spec §10 OBS-03: custom prometheus.Collector that
// emits one cr_status_age_seconds{kind,name} sample per tracked CR on every
// scrape. The sample value is time.Since(last_successful_status_write).Seconds.
//
// Replaces the prior prometheus.GaugeVec(.Set(0)) placeholder (Phase 1).
// RecordSuccess(kind, name) is called on every successful status write.
// Forget(kind, name) is called in every finalizer immediately before
// controllerutil.RemoveFinalizer to prevent monotonic /metrics cardinality.
//
// See internal/metrics/cr_status_age_collector.go for implementation.
var CRStatusAgeTracker = NewCRStatusAgeTracker()

// Label-value enums lifted verbatim from spec §10's sub-table.
var (
	// All 7 operator-owned kinds.
	allKinds = []string{
		"LiteLLMConnection", "Model", "ModelDiscovery",
		"MCPServer", "MCPServerDiscovery", "A2AAgent", "Team",
	}

	// Discovery sources (§10 source label).
	allSources = []string{
		"anthropic", "bedrock", "gemini", "kubeai", "openai", "toolhive",
	}

	// LiteLLM REST operations (§10 operation label).
	allOperations = []string{
		"model_list", "model_new", "model_update", "model_delete",
		"team_list", "team_new", "team_update", "team_delete",
		"mcp_list", "mcp_new", "mcp_update", "mcp_delete",
		"a2a_list", "a2a_new", "a2a_update", "a2a_delete",
		"probe",
	}

	// drift_corrected_total's domain × action — 5 × 3 = 15 combos
	// (model, mcp, a2a, team, guardrail).
	allDomains = []string{"model", "mcp", "a2a", "team", "guardrail"}
	allActions = []string{"create_missing", "update_drifted", "delete_vanished"}

	// reconcile_total{result}.
	reconcileResults = []string{"success", "error", "requeued"}

	// litellm_api_request_duration_seconds{status}.
	apiRequestStatuses = []string{"2xx", "4xx", "5xx", "error"}

	// litellm_api_errors_total{status}.
	apiErrorStatuses = []string{"4xx", "5xx", "error"}

	// discovery_refresh_total{result}.
	discoveryRefreshResults = []string{"success", "error"}

	// discovery_skipped_total{reason}.
	discoverySkippedReasons = []string{
		"ExplicitModelExists", "DuplicateDiscovery", "InvalidDiscoveredName",
		"EndpointUnknown", "ExplicitMCPServerExists",
	}

	// discovery_failed_total{reason} — _FINALv3 (D-10) narrowing: this
	// enum was {LiteLLMRejected, LiteLLMUnavailable} before _FINALv3.
	// Phase 4 retires both reasons because Discovery never calls LiteLLM
	// (MDISC-27). The single remaining reason is ChildCRWriteFailed —
	// apiserver-side child SSA write failure (server timeout, rate-limit,
	// service unavailable, SSA field conflict).
	discoveryFailedReasons = []string{"ChildCRWriteFailed"}

	// child_cr_writes_total{kind, action, result} — Phase 4 OBS-04.
	// 2 (Discovery kinds) × 3 (actions) × 2 (results) = 12 combos.
	// MCPServerDiscovery is enumerated alongside ModelDiscovery so the
	// surface stays stable across Phase 4 → Phase 5.
	discoveryParentKinds = []string{"ModelDiscovery", "MCPServerDiscovery"}
	childCRWriteActions  = []string{"create", "update", "delete"}
	childCRWriteResults  = []string{"success", "error"}

	// litellm_operator_deletion_orphaned_total{kind} — Issue #23.
	// Uses full kind names ("LiteLLM*") to match the per-controller
	// `*Kind` constants used at increment sites (modelKind="LiteLLMModel"
	// etc.). Intentionally diverges from `allKinds` (which uses bare
	// names like "Model") because pre-touch label values must match
	// what controllers actually emit.
	deletionPolicyKinds = []string{
		"LiteLLMModel", "LiteLLMTeam", "LiteLLMMCPServer",
		"LiteLLMA2AAgent", "LiteLLMGuardRail",
	}

	// connection_ready{reason}.
	connectionReadyReasons = []string{
		"Synced", "Connecting", "Absent", "Unreachable", "BadMasterKey", "SecretNotFound",
	}
)

func init() {
	// Register all 11 metrics against controller-runtime's global
	// metrics.Registry — controller-runtime's metricsserver scrapes from
	// this registry on the /metrics endpoint.
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal,
		ReconcileDurationSeconds,
		LiteLLMAPIRequestDurationSeconds,
		LiteLLMAPIErrorsTotal,
		DiscoveryRefreshTotal,
		DiscoveryGeneratedCount,
		DiscoverySkippedTotal,
		DiscoveryFailedTotal,
		DriftCorrectedTotal,
		ConnectionReady,
		CRStatusAgeTracker, // OBS-03: custom Collector — replaces CRStatusAgeSeconds GaugeVec
		ChildCRWritesTotal,
		LitellmOperatorReconcileTotal,
		DeletionOrphanedTotal,
		DeletionBlocked, // Issue #23: custom Collector — emits one sample per blocked CR
	)

	// Pre-touch every enumerated label combination from spec §10's
	// Label-value enums sub-table so /metrics enumerates them on the
	// first scrape (Assumption A5; required for AC-O1).
	//
	// .WithLabelValues(.) at zero is a noop for Counters and Gauges —
	// it registers the label tuple in the collector's state without
	// adding to its value.

	// reconcile_total{kind, result} — 7 × 3 = 21 combos.
	for _, k := range allKinds {
		for _, r := range reconcileResults {
			ReconcileTotal.WithLabelValues(k, r)
		}
	}

	// reconcile_duration_seconds{kind} — 7 combos.
	for _, k := range allKinds {
		ReconcileDurationSeconds.WithLabelValues(k)
	}

	// litellm_api_request_duration_seconds{operation, status}.
	for _, op := range allOperations {
		for _, st := range apiRequestStatuses {
			LiteLLMAPIRequestDurationSeconds.WithLabelValues(op, st)
		}
	}

	// litellm_api_errors_total{operation, status}.
	for _, op := range allOperations {
		for _, st := range apiErrorStatuses {
			LiteLLMAPIErrorsTotal.WithLabelValues(op, st)
		}
	}

	// discovery_refresh_total{kind, source, result}.
	for _, k := range allKinds {
		for _, s := range allSources {
			for _, r := range discoveryRefreshResults {
				DiscoveryRefreshTotal.WithLabelValues(k, s, r)
			}
		}
	}

	// discovery_generated_count{kind, source} — renamed in _FINALv3 (D-10)
	// from the pre-Phase-4 LiteLLM-registration-counter name. Same shape;
	// the rename reflects Pipeline B emitting K8s child Model CRs rather
	// than registering names directly with LiteLLM.
	for _, k := range allKinds {
		for _, s := range allSources {
			DiscoveryGeneratedCount.WithLabelValues(k, s)
		}
	}

	// discovery_skipped_total{kind, reason}.
	for _, k := range allKinds {
		for _, r := range discoverySkippedReasons {
			DiscoverySkippedTotal.WithLabelValues(k, r)
		}
	}

	// discovery_failed_total{kind, reason}.
	for _, k := range allKinds {
		for _, r := range discoveryFailedReasons {
			DiscoveryFailedTotal.WithLabelValues(k, r)
		}
	}

	// drift_corrected_total{domain, action} — 4 × 3 = 12 combos.
	// This is the canonical Cartesian enumeration the AC-O1 scrape test
	// asserts on.
	for _, d := range allDomains {
		for _, a := range allActions {
			DriftCorrectedTotal.WithLabelValues(d, a)
		}
	}

	// connection_ready{reason} — 6 combos.
	for _, r := range connectionReadyReasons {
		ConnectionReady.WithLabelValues(r)
	}

	// cr_status_age_seconds — now emitted by CRStatusAgeTracker (OBS-03
	// custom Collector; replaces CRStatusAgeSeconds GaugeVec pre-touch).
	// The Collector emits nothing until the first RecordSuccess call —
	// this is correct: there are no tracked CRs until reconciliation starts.
	// The old name="" sentinel pre-touch is no longer needed because the
	// custom Collector reports live ages, not zero sentinels.

	// litellm_operator_deletion_orphaned_total{kind} — Issue #23.
	// Pre-touch every kind affected by spec.deletionPolicy so /metrics
	// enumerates the full surface on first scrape (Assumption A5 /
	// AC-O1). Connection is excluded (no LiteLLM-side delete);
	// Discovery kinds are excluded (their finalizers run on the
	// Discovery parent, not on the child — children are Orphan-forced
	// and counted under their kind here).
	for _, k := range deletionPolicyKinds {
		DeletionOrphanedTotal.WithLabelValues(k)
	}

	// child_cr_writes_total{kind, action, result} — Phase 4 OBS-04 / D-10.
	// 2 Discovery kinds × 3 actions × 2 results = 12 combos. Pre-touched
	// at init time so /metrics enumerates the full surface on first scrape
	// (Assumption A5 / AC-O1) — Phase 4 introduces this metric and Phase 5
	// inherits it for MCPServerDiscovery (the second kind is enumerated
	// here so the surface stays stable across phases).
	for _, k := range discoveryParentKinds {
		for _, a := range childCRWriteActions {
			for _, r := range childCRWriteResults {
				ChildCRWritesTotal.WithLabelValues(k, a, r)
			}
		}
	}
}
