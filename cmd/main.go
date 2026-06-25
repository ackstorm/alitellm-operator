// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller"
	"github.com/ackstorm/alitellm-operator/internal/identity"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/toolhive"

	// Blank-import the metrics package so its init registers the §10
	// metric set against controller-runtime's global metrics.Registry.
	// metrics.CRStatusAgeTracker (OBS-03 custom Collector) is also
	// registered automatically via the metrics package's init.
	_ "github.com/ackstorm/alitellm-operator/internal/metrics"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(litellmv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// envOr returns os.Getenv(key) if non-empty, else fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validateWatchNamespace rejects a WATCH_NAMESPACE that is not a single
// valid Kubernetes namespace. The operator watches exactly ONE namespace
// (cache.Options.DefaultNamespaces carries a single key); a comma- or
// space-separated list is NOT supported and would otherwise be treated as
// one literal namespace that matches nothing — silently watching no CRs.
// Enforcing a DNS-1123 label fails fast on a list-looking value.
func validateWatchNamespace(ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("WATCH_NAMESPACE %q is not a single valid namespace "+
			"(the operator watches exactly one namespace, not a list): %s",
			ns, strings.Join(errs, "; "))
	}
	return nil
}

// envBool parses os.Getenv(key) as a bool ("1"/"true"/"yes" → true).
// Empty / unset / anything else returns fallback.
func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	// Env-var-first configuration with CLI flag overrides. Env vars are
	// the primary surface (Deployment env: blocks); flags are kept for
	// local `go run` / `make run` ergonomics.
	flag.StringVar(&metricsAddr, "metrics-bind-address",
		envOr("METRICS_BIND_ADDRESS", ":8080"),
		"The address the metrics endpoint binds to (plain HTTP per spec §10).")
	flag.StringVar(&probeAddr, "health-probe-bind-address",
		envOr("HEALTH_PROBE_BIND_ADDRESS", ":8081"),
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect",
		envBool("ENABLE_LEADER_ELECTION", false),
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Issue #26: emit a loud Error-level startup banner if
	// LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=true so a misconfigured
	// deploy that ships with body logging on cannot exfiltrate
	// substituted provider API keys silently. The helper is a no-op
	// when the env var is unset or any value other than "true".
	litellm.WarnIfDangerouslyLogBodies(setupLog)

	// SCOPE-04: WATCH_NAMESPACE enforcement via cache.Options.DefaultNamespaces.
	// The manager's informer cache is filtered at construction so a CR
	// created in any other namespace is never observed by the operator
	// (defense-in-depth above RBAC). Default "default" per spec §4.
	watchNS := envOr("WATCH_NAMESPACE", "default")
	if err := validateWatchNamespace(watchNS); err != nil {
		setupLog.Error(err, "invalid WATCH_NAMESPACE; aborting")
		os.Exit(1)
	}
	setupLog.Info("watch namespace configured", "namespace", watchNS)

	// H5: parse LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL EXACTLY ONCE here and
	// thread the resolved value to every consumer — the four reconciler
	// package vars (via SetSafetyRelistIntervals) AND the three relist
	// Runnables (Model, Team, GuardRail) below. Default 10m
	// (DefaultSafetyRelistInterval); 5s floor; invalid input aborts startup.
	// Accepts any time.ParseDuration string. Reasoning + floor justification
	// in internal/controller/safety_relist.go.
	relistInterval, err := controller.ResolvedSafetyRelistInterval(os.Getenv(controller.EnvSafetyRelistInterval))
	if err != nil {
		setupLog.Error(err, "invalid safety-relist interval override; aborting")
		os.Exit(1)
	}
	controller.SetSafetyRelistIntervals(relistInterval)
	setupLog.Info("safety-relist interval resolved",
		"env", controller.EnvSafetyRelistInterval, "interval", relistInterval)

	// Plain HTTP :8080/metrics per spec §10 and Open Question #2.
	// Kubebuilder v4 defaults to :8443 HTTPS+authn with
	// `WithAuthenticationAndAuthorization` — explicitly overridden here.
	// In v1alpha1 the deployment assumption is that Prometheus scrapes
	// from inside the cluster on a non-routable network; HTTPS+authn is
	// deferred to v1beta1 (Phase 7+). Re-evaluate then.
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: false,
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNS: {},
			},
		},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "alitellm-operator-leader",
		LeaderElectionNamespace: watchNS, // §7.8 — lease lives in WATCH_NAMESPACE
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Phase 2 — connection cache (D-01/D-02/D-03: atomic.Pointer snapshot,
	// fresh *litellm.Client per rebuild) + LiteLLMConnectionReconciler
	// (D-05/D-06/D-07: periodic 5min probe via §7.6 RequeueAfter, transient
	// err path, Connecting-on-entry write) + 401 fast-path Channel
	// (D-09/D-10: Source.Channel fed by the cache, CAS storm gate
	// collapses N-CR 401 storms to one event per cycle).
	//
	// The reconciler constructs a fresh *litellm.Client per probe from
	// the LiteLLMConnection CR's spec.endpoint + spec.masterKeySecretRef
	// (D-03 / CONN-01). LITELLM_URL / LITELLM_MASTER_KEY env vars are no
	// longer consumed by the manager — the CR + Secret are the source of
	// truth.
	//
	// NoOpReconciler is retired from cmd/main.go production wire-up.
	// It remains in internal/controller/ as a test-only helper for the
	// four Phase 1 envtests; controller-runtime's manager registry is
	// independent of test-only types in the same package.
	connCache := connection.NewCache(ctrl.Log.WithName("connection-cache"))
	if err := mgr.Add(connCache); err != nil {
		setupLog.Error(err, "unable to add connection cache to manager")
		os.Exit(1)
	}
	if err := (&controller.LiteLLMConnectionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Namespace: watchNS,
		Log:       ctrl.Log.WithName("controller").WithName("LiteLLMConnection"),
	}).SetupWithManager(mgr, connCache.Channel()); err != nil {
		setupLog.Error(err, "unable to set up LiteLLMConnection reconciler")
		os.Exit(1)
	}

	// Phase 3 — D-06: Secret-to-Model reverse index for SEC-09 rotation propagation.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMModel{},
		controller.SecretRefIndexField,
		controller.IndexModelSecretRefs,
	); err != nil {
		setupLog.Error(err, "unable to register Model secrets field indexer")
		os.Exit(1)
	}

	// Phase 3 — §7.6 safety re-list runnable. Lists all Model CRs every 30min
	// and enqueues them for reconciliation so out-of-band DELETEs are recovered
	// (D-03 existence-only scope — no UPDATE on this path, only CREATE when missing).
	// REL-02 compliance: uses time.Ticker inside a Runnable, NOT RequeueAfter.
	safetyRelistCh := make(chan reconcile.Request, 256)
	// H5: use the single resolved interval (parsed once above). Previously a
	// second independent time.ParseDuration here defaulted to 30m and
	// silently fell back to 30m on invalid input — disagreeing with both the
	// reconciler package vars and the floor enforced for them.
	safetyRelist := &controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    watchNS,
		Interval:     relistInterval,
		Log:          ctrl.Log.WithName("model-safety-relist"),
		RequeueCh:    safetyRelistCh,
		ListRequests: controller.ListModelRequests,
		LogLabel:     "models",
	}
	if err := mgr.Add(safetyRelist); err != nil {
		setupLog.Error(err, "unable to add model SafetyRelistRunnable to manager")
		os.Exit(1)
	}

	// FIX2.txt H-2: BootSweeper one-shot Runnable enqueues every project
	// CR whose observedGeneration matches metadata.generation but
	// Ready != True, after a 2s cache hydration delay. Each per-kind
	// channel is plumbed into the corresponding reconciler's BootEvents
	// field; SetupWithManager subscribes via source.TypedFunc so a
	// sweep enqueue fires a reconcile on the right controller.
	bootSweep := controller.NewBootSweeper(mgr.GetClient())
	if err := mgr.Add(bootSweep); err != nil {
		setupLog.Error(err, "unable to add BootSweeper to manager")
		os.Exit(1)
	}

	if err := (&controller.ModelReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Cache:              connCache, // typed as connection.ConnectionCache per D-12
		Recorder:           mgr.GetEventRecorderFor("model-controller"),
		Namespace:          watchNS,
		Log:                ctrl.Log.WithName("controller").WithName("Model"),
		BootEvents:         bootSweep.ModelEvents,
		ConnectionRebuilt:  connCache.Subscribe(),
		RecreateLimit:      controller.ResolveRecreateLimitPerMin(os.Getenv(controller.EnvRecreateLimitPerMin)),
		DefaultAccessGroup: controller.ResolveDefaultAccessGroup(os.Getenv(controller.EnvDefaultAccessGroup)),
	}).SetupWithManager(mgr, safetyRelistCh); err != nil {
		setupLog.Error(err, "unable to set up Model reconciler")
		os.Exit(1)
	}

	// Phase 4 — ModelDiscoveryReconciler (Pipeline B): periodic provider
	// discovery → filter → SSA-rendered child Models with field owner
	// "litellm-modeldiscovery". MDISC-21 Secret-rotation propagation
	// requires a field indexer on spec.credentialsSecretRef.name so the
	// secretToModelDiscoveries watch handler can list affected CRs in O(1).
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMModelDiscovery{},
		controller.CredentialsSecretRefField,
		controller.IndexModelDiscoveryCredentialsSecretRef,
	); err != nil {
		setupLog.Error(err, "unable to register ModelDiscovery credentialsSecretRef field indexer")
		os.Exit(1)
	}

	// Shared HTTP client for HTTP providers (anthropic, gemini, openai,
	// kubeai). Bedrock uses aws-sdk-go-v2 and ignores this client.
	// Conservative timeouts: 10s total request, 30s idle conn timeout,
	// 4 max idle conns per host (5 providers × few CRs each).
	discoveryHTTPClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	if err := (&controller.ModelDiscoveryReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		HTTPClient: discoveryHTTPClient,
		Recorder:   mgr.GetEventRecorderFor("modeldiscovery-controller"),
		Namespace:  watchNS,
		Log:        ctrl.Log.WithName("controller").WithName("ModelDiscovery"),
		BootEvents: bootSweep.ModelDiscoveryEvents,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up ModelDiscovery reconciler")
		os.Exit(1)
	}

	// Phase 5 — MCPServerReconciler (Pipeline A): per-CR
	// resolve/diff/apply against /v1/mcp/server. Mirrors the Phase 3
	// Model wiring shape. The Secret reverse-index supports SEC-09
	// rotation propagation within 30s (Phase 3 D-06 pattern).
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMMCPServer{},
		controller.MCPServerSecretRefIndexField,
		controller.IndexMCPServerSecretRefs,
	); err != nil {
		setupLog.Error(err, "unable to register MCPServer secrets field indexer")
		os.Exit(1)
	}
	if err := (&controller.MCPServerReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache, // typed as connection.ConnectionCache per D-12
		Recorder:          mgr.GetEventRecorderFor("mcpserver-controller"),
		Namespace:         watchNS,
		Log:               ctrl.Log.WithName("controller").WithName("MCPServer"),
		BootEvents:        bootSweep.MCPServerEvents,
		ConnectionRebuilt: connCache.Subscribe(),
		RecreateLimit:     controller.ResolveRecreateLimitPerMin(os.Getenv(controller.EnvRecreateLimitPerMin)),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up MCPServer reconciler")
		os.Exit(1)
	}

	// Phase 5 — A2AAgentReconciler (Pipeline A): per-CR
	// resolve/diff/apply against /v1/agents. Mirrors the Phase 5
	// MCPServer wiring shape with A2A-specific divergences (two-pass
	// substitution per D-04, four-collision ProjectionOverride Events per
	// D-05). The Secret reverse-index supports SEC-09 rotation propagation
	// within 30s across BOTH spec.params and spec.agentCard bags (Phase 3
	// D-06 pattern). Events RBAC marker is INHERITED from the MCPServer
	// reconciler (package-wide grant per 05-01-SUMMARY.md Task 0 audit) —
	// the A2AAgent reconciler does NOT duplicate the marker.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMA2AAgent{},
		controller.A2AAgentSecretRefIndexField,
		controller.IndexA2AAgentSecretRefs,
	); err != nil {
		setupLog.Error(err, "unable to register A2AAgent secrets field indexer")
		os.Exit(1)
	}
	if err := (&controller.A2AAgentReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache, // typed as connection.ConnectionCache per D-12
		Recorder:          mgr.GetEventRecorderFor("a2aagent-controller"),
		Namespace:         watchNS,
		Log:               ctrl.Log.WithName("controller").WithName("A2AAgent"),
		BootEvents:        bootSweep.A2AAgentEvents,
		ConnectionRebuilt: connCache.Subscribe(),
		RecreateLimit:     controller.ResolveRecreateLimitPerMin(os.Getenv(controller.EnvRecreateLimitPerMin)),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up A2AAgent reconciler")
		os.Exit(1)
	}

	// Phase 6 — TeamReconciler (Pipeline A): per-CR
	// resolve/diff/apply against /team/new + /team/update via the alias-
	// list resolution from spec §7.4 + the smallest-team_id duplicate
	// rule from spec §7.1. Mirrors the Phase 5 MCPServer
	// wiring shape with Team-specific divergences:
	// (a) name-resolve via /v2/team/list?team_alias=. + the
	// client-side exact-match filter in
	// internal/litellm/team.go::ListTeamsByAlias.
	// (b) absent-budget → JSON null wire form (body built as
	// map[string]any → CreateTeamRaw/UpdateTeamRaw bypass of the
	// typed NewTeamRequest struct, which would drop nil pointers
	// via ,omitempty and violate spec §6.7 line 1194's clearing-
	// budget contract).
	// (c) three ProjectionOverride collision keys (team_alias,
	// max_budget, budget_duration) — NOT four like A2A's D-05.
	// Events RBAC marker INHERITED from the MCPServer reconciler
	// package-wide grant (05-01-SUMMARY.md Task 0 audit).
	//
	// NOTE: This block wires the per-CR reconciler only. The implicit-
	// default synthetic reconcile (TEAM-07 / AC-T2) is wired separately.
	// The finalizer-add and deletion-path code lives in the per-CR
	// reconciler.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMTeam{},
		controller.TeamSecretRefIndexField,
		controller.IndexTeamSecretRefs,
	); err != nil {
		setupLog.Error(err, "unable to register Team secrets field indexer")
		os.Exit(1)
	}
	// Phase 6 — TeamDefaultRunnable: spec §7.4 line 1313
	// mandates a synthetic Team/default reconcile on manager start (after
	// LiteLLMConnection/default first reaches Ready=True) + every 30-min
	// safety re-list. The runnable satisfies the external identity system first-SSO
	// contract (spec §8 line 1466). manager.Add treats Runnables as
	// leader-only by default — only the elected leader runs the bootstrap
	// + ticker loop (T-06-03-05 mitigation).
	teamDefaultRequeueCh := make(chan reconcile.Request, 256)
	teamDefaultRunnable := &controller.TeamDefaultRunnable{
		Cache:             connCache,
		Namespace:         watchNS,
		Interval:          relistInterval, // H5: single resolved cadence (was hardcoded 30m)
		ReadyPollInterval: 5 * time.Second,
		Log:               ctrl.Log.WithName("runnable").WithName("TeamDefault"),
		RequeueCh:         teamDefaultRequeueCh,
	}
	if err := mgr.Add(teamDefaultRunnable); err != nil {
		setupLog.Error(err, "unable to add TeamDefaultRunnable to manager")
		os.Exit(1)
	}

	if err := (&controller.TeamReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache, // typed as connection.ConnectionCache per D-12
		Recorder:          mgr.GetEventRecorderFor("team-controller"),
		Namespace:         watchNS,
		Log:               ctrl.Log.WithName("controller").WithName("Team"),
		BootEvents:        bootSweep.TeamEvents,
		ConnectionRebuilt: connCache.Subscribe(),
		RecreateLimit:     controller.ResolveRecreateLimitPerMin(os.Getenv(controller.EnvRecreateLimitPerMin)),
	}).SetupWithManager(mgr, teamDefaultRequeueCh); err != nil {
		setupLog.Error(err, "unable to set up Team reconciler")
		os.Exit(1)
	}

	// Phase 5 — ToolHive lazy dynamic informer (D-08).
	// Registers cluster-scoped unstructured informers for
	// toolhive.stacklok.dev/v1beta1 MCPServer and VirtualMCPServer. The
	// Informer satisfies manager.Runnable: Start tries an initial
	// registration synchronously, then (on failure) spawns a 1-minute
	// retry goroutine so manager.Setup does NOT crash when ToolHive
	// CRDs are absent at deploy time (MSDISC-06).
	//
	// Wires this to MCPServerDiscoveryReconciler. Until 05-04
	// lands, the informer exists as a runnable with no consumer; that
	// is harmless — it costs one goroutine + (when CRDs present) two
	// informer subscriptions on the manager cache.
	toolHiveInformer := &toolhive.Informer{
		Manager: mgr,
		Log:     ctrl.Log.WithName("toolhive-informer"),
	}
	if err := mgr.Add(toolHiveInformer); err != nil {
		setupLog.Error(err, "unable to add toolhive informer")
		os.Exit(1)
	}

	// Phase 5 — MCPServerDiscoveryReconciler (Pipeline B):
	// reads ToolHive `MCPServer` + `VirtualMCPServer` snapshots via the
	// lazy informer above and SSA-renders K8s MCPServer child CRs in
	// WATCH_NAMESPACE with FieldOwner `litellm-mcpserverdiscovery`. The
	// reconciler NEVER calls LiteLLM (MSDISC-16); the child MCPServer
	// reconciler is the sole LiteLLM writer for the discovered set.
	//
	// Per AC-SEC4-PROPAGATE: NO field indexer registered here — MSDisc
	// has no credentialsSecretRef (MSDISC-04) and SetupWithManager does
	// NOT watch Secrets. Secret-rotation propagation for the discovered
	// set rides the child MCPServer controller's own Secret indexer
	// (Phase 3 D-06 pattern).
	if err := (&controller.MCPServerDiscoveryReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		ToolHiveInformer: toolHiveInformer,
		Recorder:         mgr.GetEventRecorderFor("mcpserverdiscovery-controller"),
		Namespace:        watchNS,
		Log:              ctrl.Log.WithName("controller").WithName("MCPServerDiscovery"),
		BootEvents:       bootSweep.MCPServerDiscoveryEvents,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up MCPServerDiscovery reconciler")
		os.Exit(1)
	}

	// GuardRailReconciler — POST/PUT/DELETE against the LiteLLM
	// /guardrails HTTP surface. SecretRefIndexField mirrors the Model /
	// Team / MCPServer / A2A pattern so the Secret-rotation fan-in
	// reconciles every guardrail that referenced the rotated Secret.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&litellmv1alpha1.LiteLLMGuardRail{},
		controller.GuardrailSecretRefIndexField,
		controller.IndexGuardrailSecretRefs,
	); err != nil {
		setupLog.Error(err, "unable to register guardrail secret-ref field indexer")
		os.Exit(1)
	}
	// GuardRail safety-re-list runnable. 30m tick in production matches
	// the Model safety-re-list interval. Channel is read inside the
	// reconciler's SetupWithManager via source.TypedFunc.
	guardrailSafetyRelistCh := make(chan reconcile.Request, 256)
	if err := mgr.Add(&controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    watchNS,
		Interval:     relistInterval, // H5: single resolved cadence (was hardcoded 30m)
		Log:          ctrl.Log.WithName("guardrail-safety-relist"),
		RequeueCh:    guardrailSafetyRelistCh,
		ListRequests: controller.ListGuardRailRequests,
		LogLabel:     "guardrails",
	}); err != nil {
		setupLog.Error(err, "unable to add guardrail SafetyRelistRunnable")
		os.Exit(1)
	}

	if err := (&controller.GuardRailReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache,
		Recorder:          mgr.GetEventRecorderFor("guardrail-controller"),
		Namespace:         watchNS,
		Log:               ctrl.Log.WithName("controller").WithName("GuardRail"),
		BootEvents:        bootSweep.GuardRailEvents,
		ConnectionRebuilt: connCache.Subscribe(),
	}).SetupWithManager(mgr, guardrailSafetyRelistCh); err != nil {
		setupLog.Error(err, "unable to set up GuardRail reconciler")
		os.Exit(1)
	}

	// MALIAS — ModelAliasReconciler aggregates ALL LiteLLMModelAlias CRs
	// cluster-wide into one router_settings.model_group_alias map and writes
	// it via /config/update. The reconciler coalesces all CR events onto the
	// sentinel key controller.ModelAliasSingletonKey so concurrent edits
	// produce ONE HTTP write per debounce window. No field indexer and no
	// safety-relist Runnable — periodic resync is handled inside Reconcile
	// via the RequeueAfter return.
	if err := (&controller.ModelAliasReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache,
		Recorder:          mgr.GetEventRecorderFor("modelalias-controller"),
		Namespace:         watchNS,
		Log:               ctrl.Log.WithName("controller").WithName("ModelAlias"),
		ConnectionRebuilt: connCache.Subscribe(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up ModelAlias reconciler")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// FIX4.txt H-1: log the audit identity literal the binary will stamp
	// into LiteLLM /model/new, /model/update, /team/*, /v1/mcp/server,
	// /a2a/agent payloads. Confirms ldflags Version injection without an
	// external probe (prior FIX4 evidence had the field landing as null
	// in the UI; this surfaces the actual value at boot).
	setupLog.Info("operator identity", "value", identity.Operator())

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
