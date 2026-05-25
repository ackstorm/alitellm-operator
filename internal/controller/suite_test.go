// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/toolhive"

	// Blank-import metrics so init runs and the §10 set is registered
	// against controller-runtime's metrics.Registry before TestMain spins
	// up the metrics server.
	_ "github.com/ackstorm/alitellm-operator/internal/metrics"
)

// WatchNamespace is the namespace the test manager watches. AC-N4 tests
// verify that CRs created elsewhere (e.g. "default") are never reconciled.
const WatchNamespace = "default"

// MetricsAddr is the bind address used by the test manager's metrics
// endpoint. We pick a non-privileged port unlikely to collide with the
// operator's production :8080 default — controller-runtime's metrics
// server does not expose its bound listener (issue: #1571 upstream), so
// we bind a known port and the scrape test connects there.
const MetricsAddr = "127.0.0.1:18080"

// MetricsURL is the URL the scrape test connects to.
const MetricsURL = "http://127.0.0.1:18080/metrics"

// Test-global state shared across all suite test files (watchnamespace_test.go,
// idempotency_test.go, fastpath_test.go, metrics_scrape_test.go). TestMain
// populates these; individual Test* functions read them.
var (
	testEnv          *envtest.Environment
	cfg              *rest.Config
	k8sClient        client.Client
	mockServer       *mock.MockServer
	reconciler       *NoOpReconciler
	fakeCache        *FakeConnectionCache
	reconcileCalls   *atomic.Int64
	mgrCtx           context.Context
	mgrCancel        context.CancelFunc
	metricsActualURL string

	// Phase 2 — real connection cache + LiteLLMConnection
	// reconciler. Wired alongside the Phase 1 NoOpReconciler so the 4
	// Phase 1 envtests keep passing while the 4 Phase 2 envtests
	// (litellmconnection_controller_test.go) exercise the real probe loop.
	connCache      *connection.Cache
	connReconciler *LiteLLMConnectionReconciler

	// Phase 3 — Model reconciler. Shares connCache with the
	// Phase 2 reconciler. Model envtests run against the same envtest cluster
	// as Phase 1 + Phase 2 tests.
	modelReconciler *ModelReconciler

	// Phase 3 — ModelSafetyRelistRunnable + its request channel.
	modelSafetyRelist   *ModelSafetyRelistRunnable
	modelSafetyRelistCh chan reconcile.Request

	// Phase 4 — ModelDiscoveryReconciler. Shares the
	// manager+envtest infrastructure with Phase 1/2/3 reconcilers.
	// 04-05 envtests exercise: NORM1 (Bedrock colon → dash), MDISC-11
	// (DNS-1123 reject), AC-DC3 (vanish detection), AC-MD-CASCADE
	// (cascade-delete drain), AtomicRefresh_NoDelete (D-09 gate).
	mdReconciler *ModelDiscoveryReconciler

	// Phase 5 — MCPServerReconciler. Shares connCache with
	// the Phase 2/3 reconcilers. Tests exercise MCP-01.04 + AC-MS1
	// (ownerRef tolerance, drift counters, finalizer, 401 fast-path,
	// connection-gate, secret rotation, CEL admission).
	mcpServerReconciler *MCPServerReconciler

	// Phase 5 — A2AAgentReconciler. Shares connCache with
	// the Phase 2/3/5 reconcilers. Tests exercise A2A-01.06 +
	// two-pass substitution (D-04), four-collision ProjectionOverride
	// Events (D-05), CreateOnFirstReconcile, UpdateOnDrift,
	// DeleteViaFinalizer, ConnectionGate, 401FastPath, SecretRotation.
	a2aAgentReconciler *A2AAgentReconciler

	// Phase 6 — TeamReconciler. Shares connCache with the
	// Phase 2/3/5 reconcilers. Tests exercise TEAM-01.06 + AC-T1
	// (budget projection), AC-T6 (params pass-through + ProjectionOverride
	// for three structural-overlay collision keys), AC-DC1 team slice
	// (hand-managed coexistence), AC-S1 (redaction canary), 401 fast-path,
	// ConnectionGate, DriftMetricsFirstReconcileSuppressed.
	teamReconciler *TeamReconciler

	// Phase 6 — TeamDefaultRunnable + its request channel.
	// 100ms tick + 50ms Ready-poll for fast envtests (production uses
	// 30m + 5s). The runnable gates on connCache.Snapshot.Ready and
	// then synthetically enqueues reconcile.Request{
	// NamespacedName:{WatchNamespace,"default"}} so the TeamReconciler's
	// NotFound-on-default branch (reconcileImplicitDefault) drives the
	// implicit-default bootstrap per spec §7.4 line 1313 + AC-T4.
	teamDefaultRunnable  *TeamDefaultRunnable
	teamDefaultRequeueCh chan reconcile.Request

	// Phase 5 — ToolHive lazy dynamic informer. Populated by
	// TestMain wiring (installs ToolHive CRDs + Adds the
	// real *toolhive.Informer to the manager with a short retry interval
	// so the 19 MCPServerDiscoveryReconciler envtests run against a real
	// cache). Tests that need the absent-CRD path inject a stub through
	// the reconciler's ToolHiveInformer field; the package's informer-
	// proper envtests live in internal/toolhive/informer_test.go.
	toolhiveInformer *toolhive.Informer

	// Phase 5 — MCPServerDiscoveryReconciler. Pipeline B
	// reconciler that reads ToolHive `MCPServer` + `VirtualMCPServer`
	// snapshots via toolhiveInformer above and SSA-renders K8s
	// MCPServer child CRs in WatchNamespace. Tests exercise the full
	// 19-case behavior surface across Tasks 2a (state machine + happy
	// path) and 2b (edge cases + lifecycle).
	mcpServerDiscoveryReconciler *MCPServerDiscoveryReconciler

	// mcpServerDiscoveryClient is the suite-installed patchInterceptor
	// wrapping the manager client for MSDisc. Tests use ArmAlreadyExists
	// on it to inject a one-shot AlreadyExists from Patch, without
	// swapping the reconciler's Client field at runtime.
	mcpServerDiscoveryClient *patchInterceptor

	// GuardRailReconciler. Shares connCache with the
	// Phase 2/3/5/6 reconcilers. Tests exercise the GR-01..n surface
	// (POST/PUT/DELETE wire semantics, {{NAME}} substitution, drift
	// correction, CONFIG conflict, PoolProviderMismatch, safety re-list
	// create_missing recovery).
	guardrailReconciler *GuardRailReconciler

	// GuardRail safety-re-list runnable + its request channel. 100ms
	// tick in envtest so out-of-band DELETE recovery is observable
	// inside a 5s poll window.
	guardrailSafetyRelist   *GuardRailSafetyRelistRunnable
	guardrailSafetyRelistCh chan reconcile.Request
)

// TestMain is the envtest bootstrap. It starts a real etcd+kube-apiserver
// pair (via envtest), constructs a manager.Manager pointed at it, wires
// the MockServer, registers the NoOpReconciler, and launches the manager
// in a goroutine.
//
// Per-test setup (CR creation, mode flips) lives in the individual
// Test* functions. Cleanup (testEnv.Stop, mock.Close, manager cancel)
// happens via defer/cleanup chains rooted in TestMain.
func TestMain(m *testing.M) {
	// Resolve envtest binaries. KUBEBUILDER_ASSETS may already be set
	// (Makefile target). Otherwise probe the standard paths the devtools
	// image provides.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if found := findEnvtestAssets(); found != "" {
			_ = os.Setenv("KUBEBUILDER_ASSETS", found)
		}
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	// Bootstrap.
	if code := setupAndRun(m); code != 0 {
		os.Exit(code)
	}
}

// setupAndRun is split out so deferred cleanup happens cleanly before
// os.Exit (deferred funcs do NOT run after os.Exit).
//
//nolint:gocyclo // envtest bootstrap is a single linear setup script; splitting would require persisting many test-scoped globals across helper boundaries.
func setupAndRun(m *testing.M) int {
	// Build envtest.Environment pointed at the operator's CRDs.
	_, thisFile, _, _ := runtime.Caller(0)
	crdDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd", "bases")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\n", err)
		return 1
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
		}
	}()

	// Scheme: clientgoscheme + our types + apiextensions (for 	// programmatic install of ToolHive CRDs).
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(litellmv1alpha1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	// Direct client for test fixtures (CR create/delete bypassing the
	// manager cache).
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s client: %v\n", err)
		return 1
	}

	// Ensure WatchNamespace exists for tests that create CRs in it.
	ctx := context.Background()
	if err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: WatchNamespace},
	}); err != nil && !ignoreAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "ensure %s: %v\n", WatchNamespace, err)
		return 1
	}
	// `default` namespace is created by envtest automatically, but
	// some envtest versions skip it — ensure idempotently.
	if err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}); err != nil && !ignoreAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "ensure default: %v\n", err)
		return 1
	}

	// Phase 5: install ToolHive CRDs programmatically before
	// the manager starts. Mirrors internal/toolhive/informer_test.go's
	// installToolhiveCRDs helper — minimal CRD shape with
	// x-kubernetes-preserve-unknown-fields: true at the object root so
	// the MSDisc envtests can create unstructured ToolHive MCPServer /
	// VirtualMCPServer objects with status.url + status.transport fields
	// (used by reconcile flow). The
	// dev / prod namespaces (referenced by spec.toolhive.namespaces[]
	// in MSDisc CRs) are also created here so child rendering can
	// proceed without a manual namespace-create.
	if err := installToolhiveCRDsForSuite(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "install ToolHive CRDs for suite: %v\n", err)
		return 1
	}
	for _, nsName := range []string{"dev", "prod", "ac-n3-ns"} {
		if err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: nsName},
		}); err != nil && !ignoreAlreadyExists(err) {
			fmt.Fprintf(os.Stderr, "ensure namespace %q: %v\n", nsName, err)
			return 1
		}
	}

	// Mock LiteLLM (in-process httptest.Server). NewServer accepts nil
	// when called outside a *testing.T context (TestMain has no t).
	mockServer = mock.NewServer(nil)
	defer mockServer.Close()

	// *litellm.Client wired against the mock.
	llm := litellm.NewClient(mockServer.URL(), "sk-test-master-key", logr.Discard())

	// Manager with WATCH_NAMESPACE-scoped cache (SCOPE-04).
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				WatchNamespace: {},
			},
		},
		Metrics: metricsserver.Options{
			BindAddress:   MetricsAddr,
			SecureServing: false,
		},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager.New: %v\n", err)
		return 1
	}

	// Wire NoOpReconciler.
	fakeCache = NewFakeConnectionCache()
	reconciler = &NoOpReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		LiteLLM:              llm,
		Cache:                fakeCache,
		SafetyRelistInterval: 1 * time.Second, // Accelerated for AC-R1 smoke
		Log:                  logr.Discard(),
	}
	reconcileCalls = &reconciler.ReconcileCalls
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager: %v\n", err)
		return 1
	}

	// Phase 2 wiring: real connection.Cache +
	// LiteLLMConnectionReconciler. Runs SIDE-BY-SIDE with the Phase 1
	// NoOpReconciler — the 4 Phase 1 tests target Model CRs and the 4
	// Phase 2 tests target LiteLLMConnection CRs, no conflict.
	connCache = connection.NewCache(logr.Discard())
	if err := mgr.Add(connCache); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(connCache): %v\n", err)
		return 1
	}
	connReconciler = &LiteLLMConnectionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := connReconciler.SetupWithManager(mgr, connCache.Channel()); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(LiteLLMConnection): %v\n", err)
		return 1
	}

	// Phase 3: register the Model field indexer + ModelReconciler.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMModel{},
		SecretRefIndexField,
		IndexModelSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(Model secrets): %v\n", err)
		return 1
	}

	// Phase 3: wire ModelSafetyRelistRunnable with 100ms interval
	// for fast test execution (production uses 30min).
	modelSafetyRelistCh = make(chan reconcile.Request, 256)
	modelSafetyRelist = &ModelSafetyRelistRunnable{
		Client:    mgr.GetClient(),
		Namespace: WatchNamespace,
		Interval:  100 * time.Millisecond,
		Log:       logr.Discard(),
		RequeueCh: modelSafetyRelistCh,
	}
	if err := mgr.Add(modelSafetyRelist); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(ModelSafetyRelistRunnable): %v\n", err)
		return 1
	}

	modelReconciler = &ModelReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("model-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := modelReconciler.SetupWithManager(mgr, modelSafetyRelistCh); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(Model): %v\n", err)
		return 1
	}

	// Phase 4: register the ModelDiscovery field indexer +
	// ModelDiscoveryReconciler. The shared *http.Client mirrors cmd/main.go's
	// 30s-timeout client (D-02) — production-equivalent for envtests.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMModelDiscovery{},
		CredentialsSecretRefField,
		IndexModelDiscoveryCredentialsSecretRef,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(ModelDiscovery credentialsSecretRef): %v\n", err)
		return 1
	}
	mdReconciler = &ModelDiscoveryReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Recorder:   mgr.GetEventRecorderFor("modeldiscovery-controller"),
		Namespace:  WatchNamespace,
		Log:        logr.Discard(),
	}
	if err := mdReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(ModelDiscovery): %v\n", err)
		return 1
	}

	// Phase 5: register the MCPServer field indexer +
	// MCPServerReconciler. Mirrors the Phase 3 Model wiring block.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMMCPServer{},
		MCPServerSecretRefIndexField,
		IndexMCPServerSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(MCPServer secrets): %v\n", err)
		return 1
	}
	mcpServerReconciler = &MCPServerReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("mcpserver-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := mcpServerReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(MCPServer): %v\n", err)
		return 1
	}

	// Phase 5: register the A2AAgent field indexer +
	// A2AAgentReconciler. Mirrors the Phase 3 Model + Phase 5 	// MCPServer wiring blocks.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMA2AAgent{},
		A2AAgentSecretRefIndexField,
		IndexA2AAgentSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(A2AAgent secrets): %v\n", err)
		return 1
	}
	a2aAgentReconciler = &A2AAgentReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("a2aagent-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := a2aAgentReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(A2AAgent): %v\n", err)
		return 1
	}

	// Phase 6: register the Team field indexer +
	// TeamReconciler. Mirrors the Phase 3 Model + Phase 5 	// MCPServer + A2AAgent wiring blocks.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMTeam{},
		TeamSecretRefIndexField,
		IndexTeamSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(Team secrets): %v\n", err)
		return 1
	}
	teamReconciler = &TeamReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("team-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}

	// Phase 6: wire TeamDefaultRunnable with 100ms tick and
	// 50ms ready-poll for fast envtest execution (production uses 30m +
	// 5s). The channel feeds the TeamReconciler's source.TypedFunc
	// raw-source registered below via SetupWithManager.
	teamDefaultRequeueCh = make(chan reconcile.Request, 16)
	teamDefaultRunnable = &TeamDefaultRunnable{
		Cache:             connCache,
		Namespace:         WatchNamespace,
		Interval:          100 * time.Millisecond,
		ReadyPollInterval: 50 * time.Millisecond,
		Log:               logr.Discard(),
		RequeueCh:         teamDefaultRequeueCh,
	}
	if err := mgr.Add(teamDefaultRunnable); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(TeamDefaultRunnable): %v\n", err)
		return 1
	}

	if err := teamReconciler.SetupWithManager(mgr, teamDefaultRequeueCh); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(Team): %v\n", err)
		return 1
	}

	// Phase 5: wire the real ToolHive lazy informer + MSDisc
	// reconciler. The informer's retry interval is shortened to 250ms
	// (vs the production 1m) so the envtest absent-CRD path stays fast.
	// The CRDs were installed above in installToolhiveCRDsForSuite, so
	// the informer's first registration attempt succeeds synchronously
	// and IsReady flips to true within the manager startup window.
	toolhiveInformer = &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 250 * time.Millisecond,
	}
	if err := mgr.Add(toolhiveInformer); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(toolhiveInformer): %v\n", err)
		return 1
	}
	// Install the patchInterceptor once at suite setup. Tests arm/disarm
	// via the interceptor's atomic.Pointer (atomic write) instead of
	// swapping the reconciler's Client field — swapping a field on a
	// long-lived reconciler races with in-flight Reconcile goroutines
	// (race detector flagged the previous design at suite-wide runs).
	mcpServerDiscoveryClient = &patchInterceptor{Client: mgr.GetClient()}
	mcpServerDiscoveryReconciler = &MCPServerDiscoveryReconciler{
		Client:           mcpServerDiscoveryClient,
		Scheme:           mgr.GetScheme(),
		ToolHiveInformer: toolhiveInformer,
		Recorder:         mgr.GetEventRecorderFor("mcpserverdiscovery-controller"),
		Namespace:        WatchNamespace,
		Log:              logr.Discard(),
	}
	if err := mcpServerDiscoveryReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(MCPServerDiscovery): %v\n", err)
		return 1
	}

	// GuardRail field indexer + reconciler. Mirrors the Phase 3
	// Model + Phase 5 MCPServer + A2AAgent wiring blocks.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMGuardRail{},
		GuardrailSecretRefIndexField,
		IndexGuardrailSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(GuardRail secrets): %v\n", err)
		return 1
	}
	// GuardRail safety-re-list runnable — 100ms in envtest (vs 30m prod)
	// so create_missing drift correction is observable inside a 5s poll
	// window. Mirrors ModelSafetyRelistRunnable.
	guardrailSafetyRelistCh = make(chan reconcile.Request, 256)
	guardrailSafetyRelist = &GuardRailSafetyRelistRunnable{
		Client:    mgr.GetClient(),
		Namespace: WatchNamespace,
		Interval:  100 * time.Millisecond,
		Log:       logr.Discard(),
		RequeueCh: guardrailSafetyRelistCh,
	}
	if err := mgr.Add(guardrailSafetyRelist); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(GuardRailSafetyRelistRunnable): %v\n", err)
		return 1
	}
	guardrailReconciler = &GuardRailReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("guardrail-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := guardrailReconciler.SetupWithManager(mgr, guardrailSafetyRelistCh); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(GuardRail): %v\n", err)
		return 1
	}

	// MALIAS — ModelAlias reconciler. Shares connCache with the rest.
	// All CR events coalesce onto sentinel work-key ModelAliasSingletonKey,
	// so the envtest suite sees one HTTP write per reconcile pass regardless
	// of how many CRs change in the window.
	modelAliasReconciler := &ModelAliasReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Cache:     connCache,
		Recorder:  mgr.GetEventRecorderFor("modelalias-controller"),
		Namespace: WatchNamespace,
		Log:       logr.Discard(),
	}
	if err := modelAliasReconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(ModelAlias): %v\n", err)
		return 1
	}

	// Task 3: master-key Secret 'litellm-master-key' in
	// WatchNamespace so AC-C1 envtests can observe Ready=Synced.
	// k8sClient is the direct client (bypasses the manager cache) so
	// this works before mgr.Start. Idempotent — re-runs are safe.
	if err := k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "litellm-master-key", Namespace: WatchNamespace},
		Data:       map[string][]byte{"masterKey": []byte("sk-test-master-key")},
	}); err != nil && !ignoreAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "ensure master-key secret: %v\n", err)
		return 1
	}

	// Start the manager in a goroutine.
	mgrCtx, mgrCancel = context.WithCancel(ctx)
	defer mgrCancel()
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()

	// Deterministic readiness gate: mgr.Elected() closes once the
	// manager has finished leader-election and started its runnables
	// (cache + controllers). Only after Elected can WaitForCacheSync
	// be invoked safely — calling it before the cache runnable starts
	// returns true immediately on an empty set, racing the first
	// Reconcile dispatch. The combined gate replaces the prior fixed
	// 2s settle sleep, saving ~1.8s per suite run while remaining
	// race-free against mgr.Start internals.
	const startupBudget = 30 * time.Second
	startupCtx, startupCancel := context.WithTimeout(mgrCtx, startupBudget)
	select {
	case <-mgr.Elected():
	case err := <-mgrDone:
		startupCancel()
		fmt.Fprintf(os.Stderr, "mgr.Start returned before Elected: %v\n", err)
		return 1
	case <-startupCtx.Done():
		startupCancel()
		fmt.Fprintf(os.Stderr, "mgr.Elected: not signaled within %s\n", startupBudget)
		return 1
	}
	if !mgr.GetCache().WaitForCacheSync(startupCtx) {
		startupCancel()
		fmt.Fprintf(os.Stderr, "mgr.GetCache().WaitForCacheSync: did not sync within %s\n", startupBudget)
		return 1
	}
	startupCancel()

	// Best-effort metrics URL discovery — controller-runtime does not
	// expose the bound metrics listener publicly. We use the well-known
	// pattern: bind ":<port>" via the manager Options, then test files
	// HTTP-GET via the registered controller-runtime metrics.Registry.
	// For Phase 1's metrics_scrape_test.go we scrape via the same
	// registry-Gather path the metrics server uses internally, so the
	// test does not depend on knowing the bound port.
	metricsActualURL = ""

	// Verify the metrics handler is reachable on the actual port (for
	// tests that want a real HTTP scrape rather than registry.Gather).
	// Set an env var hint so the metrics scrape test can pick a path.
	// (See metrics_scrape_test.go for the Gather-based assertion.)

	// Run tests.
	rc := m.Run()

	// Cancel manager before the deferred testEnv.Stop runs.
	mgrCancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
	}

	return rc
}

// updateWithRetry wraps a test-side k8sClient.Update read-modify-write
// inside retry.RetryOnConflict. The mutate callback receives the freshly-
// fetched object; if it returns an error other than 409, the retry loop
// aborts. Use this for test-driven CR mutations (annotation bumps,
// spec edits) where the operator may be concurrently writing status —
// under -race the conflict window widens and a plain Update routinely
// loses the optimistic-lock race.
//
// Generic on the concrete client.Object type so callers don't lose
// typed-field access in the mutate closure.
func updateWithRetry[T client.Object](ctx context.Context, key client.ObjectKey, obj T, mutate func(T) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, obj); err != nil {
			return err
		}
		if err := mutate(obj); err != nil {
			return err
		}
		return k8sClient.Update(ctx, obj)
	})
}

// findEnvtestAssets probes the standard paths for envtest binaries
// pre-baked into the devtools image (per Dockerfile.devtools). Returns
// "" if none found.
func findEnvtestAssets() string {
	candidates := []string{
		"/opt/envtest/k8s/1.31.0-linux-amd64",
		"/opt/envtest/k8s/1.31.0-linux-arm64",
		"/opt/envtest/k8s",
		"/workspace/.gocache/envtest/k8s/1.31.0-linux-amd64",
		"/workspace/.gocache/envtest/k8s/1.31.0-linux-arm64",
	}
	for _, c := range candidates {
		if isExecutable(filepath.Join(c, "kube-apiserver")) {
			return c
		}
	}
	// Glob fallback under /workspace/.gocache/envtest/k8s/*
	if matches, err := filepath.Glob("/workspace/.gocache/envtest/k8s/*"); err == nil {
		for _, m := range matches {
			if isExecutable(filepath.Join(m, "kube-apiserver")) {
				return m
			}
		}
	}
	if matches, err := filepath.Glob("/opt/envtest/k8s/*"); err == nil {
		for _, m := range matches {
			if isExecutable(filepath.Join(m, "kube-apiserver")) {
				return m
			}
		}
	}
	return ""
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !st.IsDir() && st.Mode()&0o111 != 0
}

func ignoreAlreadyExists(err error) bool {
	if err == nil {
		return true
	}
	// Cheap stringer rather than pulling apierrors — envtest already
	// pulls it transitively but this keeps the file's import surface
	// minimal.
	msg := err.Error()
	for _, s := range []string{"already exists", "AlreadyExists"} {
		if contains(msg, s) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Re-export http for sibling test files so they don't need a separate
// net/http import to access stdlib constants.
var _ = http.StatusOK

// installToolhiveCRDsForSuite installs minimal toolhive.stacklok.dev
// MCPServer + VirtualMCPServer CRDs serving BOTH v1alpha1 and v1beta1
// into the shared envtest API server so MCPServerDiscoveryReconciler
// envtests can create unstructured ToolHive objects under either version.
// Mirrors internal/toolhive/informer_test.go's installToolhiveCRDs helper
// but lives here so the controller suite owns its own CRD installation
// lifecycle.
//
// Version setup:
//   - v1alpha1: served=true, storage=true (canonical, matches
//     toolhive.MCPServerGVK / VirtualMCPServerGVK constants).
//   - v1beta1:  served=true, storage=false (so the informer's dual-version
//     registration path is exercised; objects created under v1alpha1 are
//     visible under v1beta1 list calls via apiserver auto-conversion).
//
// Idempotent: calling InstallCRDs over an existing CRD upserts.
func installToolhiveCRDsForSuite(ctx context.Context, cfg *rest.Config) error {
	preserve := true
	mkVersion := func(name string, storage bool) apiextensionsv1.CustomResourceDefinitionVersion {
		return apiextensionsv1.CustomResourceDefinitionVersion{
			Name:    name,
			Served:  true,
			Storage: storage,
			Schema: &apiextensionsv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:                   "object",
					XPreserveUnknownFields: &preserve,
				},
			},
			Subresources: &apiextensionsv1.CustomResourceSubresources{
				Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
			},
		}
	}
	mkCRD := func(kind, listKind, plural, singular string) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: plural + ".toolhive.stacklok.dev",
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "toolhive.stacklok.dev",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind:     kind,
					ListKind: listKind,
					Plural:   plural,
					Singular: singular,
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					mkVersion("v1alpha1", true),
					mkVersion("v1beta1", false),
				},
			},
		}
	}
	_ = ctx // reserved for future cancellation; InstallCRDs uses its own context
	_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			mkCRD("MCPServer", "MCPServerList", "mcpservers", "mcpserver"),
			mkCRD("VirtualMCPServer", "VirtualMCPServerList", "virtualmcpservers", "virtualmcpserver"),
		},
		MaxTime: 30 * time.Second,
	})
	return err
}
