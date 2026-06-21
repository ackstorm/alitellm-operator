// SPDX-License-Identifier: Apache-2.0

package relist

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
	"github.com/ackstorm/alitellm-operator/internal/controller"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"

	// Blank-import metrics so its §10 set is registered against the
	// controller-runtime registry before the manager starts (the test
	// reads DriftCorrectedTotal via testutil).
	_ "github.com/ackstorm/alitellm-operator/internal/metrics"
)

// WatchNamespace is the namespace this isolated manager watches. Mirrors
// the parent suite's constant; redeclared here because the parent's is
// package-private.
const WatchNamespace = "default"

// conditionTypeReady / guardrailContentFilterProvider mirror the
// parent-suite package-private constants (writestatus_helpers.go:13,
// litellmguardrail_admission_test.go:33).
const (
	conditionTypeReady             = "Ready"
	guardrailContentFilterProvider = "litellm_content_filter"
)

// Test-global state populated by TestMain and read by the relist tests.
var (
	testEnv    *envtest.Environment
	cfg        *rest.Config
	k8sClient  client.Client
	mockServer *mock.MockServer
	connCache  *connection.Cache
	mgrCtx     context.Context
	mgrCancel  context.CancelFunc
)

// TestMain bootstraps a DEDICATED envtest + manager wiring ONLY the
// connection + guardrail reconcilers and the guardrail SafetyRelistRunnable
// (always-on — no Gate). Because this package runs in its own `go test`
// process (scripts/run-envtest-packages.sh starts one process per
// package), there is no neighbor-test contention: the relist over a single
// guardrail CR recovers deterministically.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if found := findEnvtestAssets(); found != "" {
			_ = os.Setenv("KUBEBUILDER_ASSETS", found)
		}
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))
	if code := setupAndRun(m); code != 0 {
		os.Exit(code)
	}
}

//nolint:gocyclo // envtest bootstrap is a single linear setup script.
func setupAndRun(m *testing.M) int {
	// CRDs live at <repo>/config/crd/bases; this file is at
	// internal/controller/relist/ → three parents up to the repo root.
	_, thisFile, _, _ := runtime.Caller(0)
	crdDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "config", "crd", "bases")
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

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(litellmv1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s client: %v\n", err)
		return 1
	}

	ctx := context.Background()
	if err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: WatchNamespace},
	}); err != nil && !ignoreAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "ensure %s: %v\n", WatchNamespace, err)
		return 1
	}

	mockServer = mock.NewServer(nil)
	defer mockServer.Close()
	suiteLLMClient := litellm.NewClient(mockServer.URL(), "sk-test-master-key", logr.Discard())
	_ = suiteLLMClient // kept symmetric with the parent suite; reconcilers use connCache.

	// Metrics server DISABLED (BindAddress "0") — this package runs
	// concurrently with the parent controller package, which binds a
	// fixed metrics port; the relist test reads counters via testutil,
	// not HTTP, so no server is needed.
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{WatchNamespace: {}},
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager.New: %v\n", err)
		return 1
	}

	// Connection cache + reconciler so the connection gate flips Ready.
	connCache = connection.NewCache(logr.Discard())
	if err := mgr.Add(connCache); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(connCache): %v\n", err)
		return 1
	}
	connReconciler := &controller.LiteLLMConnectionReconciler{
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

	// GuardRail field indexer + safety-relist runnable (always-on) +
	// reconciler. 100ms tick so out-of-band DELETE recovery is observable.
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&litellmv1alpha1.LiteLLMGuardRail{},
		controller.GuardrailSecretRefIndexField,
		controller.IndexGuardrailSecretRefs,
	); err != nil {
		fmt.Fprintf(os.Stderr, "IndexField(GuardRail secrets): %v\n", err)
		return 1
	}
	guardrailSafetyRelistCh := make(chan reconcile.Request, 256)
	guardrailSafetyRelist := &controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    WatchNamespace,
		Interval:     100 * time.Millisecond,
		Log:          logr.Discard(),
		RequeueCh:    guardrailSafetyRelistCh,
		ListRequests: controller.ListGuardRailRequests,
		LogLabel:     "guardrails",
		// Gate nil → always active. Safe here: only the relist tests run in
		// this package, with a single guardrail CR, so there is no flood.
	}
	if err := mgr.Add(guardrailSafetyRelist); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(guardrail SafetyRelistRunnable): %v\n", err)
		return 1
	}
	guardrailReconciler := &controller.GuardRailReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Cache:             connCache,
		Recorder:          mgr.GetEventRecorderFor("guardrail-controller"),
		Namespace:         WatchNamespace,
		Log:               logr.Discard(),
		ConnectionRebuilt: connCache.Subscribe(),
	}
	if err := guardrailReconciler.SetupWithManager(mgr, guardrailSafetyRelistCh); err != nil {
		fmt.Fprintf(os.Stderr, "SetupWithManager(GuardRail): %v\n", err)
		return 1
	}

	// Master-key Secret so the connection reconciler can probe the mock.
	if err := k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "litellm-master-key", Namespace: WatchNamespace},
		Data:       map[string][]byte{"masterKey": []byte("sk-test-master-key")},
	}); err != nil && !ignoreAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "ensure master-key secret: %v\n", err)
		return 1
	}

	mgrCtx, mgrCancel = context.WithCancel(ctx)
	defer mgrCancel()
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()

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
		fmt.Fprintf(os.Stderr, "WaitForCacheSync: did not sync within %s\n", startupBudget)
		return 1
	}
	startupCancel()

	rc := m.Run()

	mgrCancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
	}
	return rc
}

// findEnvtestAssets probes the standard devtools-image paths for envtest
// binaries. Mirrors the parent suite's helper.
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
	for _, glob := range []string{"/workspace/.gocache/envtest/k8s/*", "/opt/envtest/k8s/*"} {
		if matches, err := filepath.Glob(glob); err == nil {
			for _, m := range matches {
				if isExecutable(filepath.Join(m, "kube-apiserver")) {
					return m
				}
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
	msg := err.Error()
	return contains(msg, "already exists") || contains(msg, "AlreadyExists")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
