// SPDX-License-Identifier: Apache-2.0

package toolhive_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ackstorm/alitellm-operator/internal/toolhive"
)

// testEnv holds the shared envtest harness for this package's tests.
// Each TestInformer_* sub-test gets its own *manager.Manager built
// against this env; they share the underlying API server.
type testEnv struct {
	env       *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    *k8sruntime.Scheme
}

// setupEnvtest brings up a single envtest API server WITHOUT ToolHive
// CRDs preinstalled. Tests that need the CRDs installed call
// installToolhiveCRDs (or pass them via env.CRDs).
//
// Lifecycle: caller defers env.env.Stop (the testing.T.Cleanup chain
// in each top-level test).
func setupEnvtest(t *testing.T) *testEnv {
	t.Helper()

	// Discover envtest binaries — same pattern as
	// internal/controller/suite_test.go.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if found := findEnvtestAssets(); found != "" {
			_ = os.Setenv("KUBEBUILDER_ASSETS", found)
		}
	}

	env := &envtest.Environment{
		ErrorIfCRDPathMissing: false,
		// Intentionally NO CRDDirectoryPaths — TestInformer_StartsWithoutToolhiveCRDs
		// exercises the absent-CRD path. Tests that need CRDs install them
		// programmatically via installToolhiveCRDs.
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	})

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	return &testEnv{
		env:       env,
		cfg:       cfg,
		k8sClient: c,
		scheme:    scheme,
	}
}

// findEnvtestAssets mirrors internal/controller/suite_test.go's path
// probe.
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

// newManager constructs a fresh manager.Manager against the shared
// envtest API server. Each test gets its own manager so informer
// lifecycle is isolated.
func newManager(t *testing.T, te *testEnv) manager.Manager {
	t.Helper()
	mgr, err := manager.New(te.cfg, manager.Options{
		Scheme: te.scheme,
		Metrics: metricsserver.Options{
			// BindAddress :0 — never bind a port (we don't scrape in
			// this package's tests).
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	return mgr
}

// toolhiveCRDManifest constructs a minimal v1beta1 CRD definition for a
// single ToolHive kind under toolhive.stacklok.dev. The schema is
// intentionally permissive (x-kubernetes-preserve-unknown-fields: true at
// the object root) — the informer doesn't care about validation, only that
// the CRD exists so GetInformer succeeds.
func toolhiveCRDManifest(kind, listKind, plural, singular string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	crdVersions := []apiextensionsv1.CustomResourceDefinitionVersion{{
		Name:    "v1beta1",
		Served:  true,
		Storage: true,
		Schema: &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type:                   "object",
				XPreserveUnknownFields: &preserve,
			},
		},
		Subresources: &apiextensionsv1.CustomResourceSubresources{
			Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
		},
	}}
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
			Scope:    apiextensionsv1.NamespaceScoped,
			Versions: crdVersions,
		},
	}
}

// installToolhiveCRDs registers the MCPServer, VirtualMCPServer and
// MCPRemoteProxy CRDs at v1beta1 into the envtest API server. Idempotent
// — re-installs over an existing CRD by name are upserts. Used by
// TestInformer_LazyRetry and TestInformer_ListReturnsLiveObjects.
func installToolhiveCRDs(t *testing.T, te *testEnv) {
	t.Helper()
	crds := []*apiextensionsv1.CustomResourceDefinition{
		toolhiveCRDManifest("MCPServer", "MCPServerList", "mcpservers", "mcpserver"),
		toolhiveCRDManifest("VirtualMCPServer", "VirtualMCPServerList", "virtualmcpservers", "virtualmcpserver"),
		toolhiveCRDManifest("MCPRemoteProxy", "MCPRemoteProxyList", "mcpremoteproxies", "mcpremoteproxy"),
	}
	_, err := envtest.InstallCRDs(te.cfg, envtest.CRDInstallOptions{
		CRDs:    crds,
		MaxTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("InstallCRDs: %v", err)
	}
}

// TestInformer_StartsWithoutToolhiveCRDs — D-08 behavior 1.
//
// Setup: envtest with NO toolhive CRDs installed.
// Action: build an Informer, mgr.Add(informer), mgr.Start in goroutine.
// Assertions:
// - Start does not block (manager comes up within 1s)
// - Informer.IsReady returns false
// - Informer.List(MCPServerGVK) returns ErrNotReady
func TestInformer_StartsWithoutToolhiveCRDs(t *testing.T) {
	te := setupEnvtest(t)
	mgr := newManager(t, te)

	inf := &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 250 * time.Millisecond, // fast retry for tests
	}
	if err := mgr.Add(inf); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()

	// Give manager a brief window to call Start on runnables.
	time.Sleep(500 * time.Millisecond)

	if inf.IsReady() {
		t.Fatal("expected IsReady=false when toolhive CRDs are absent")
	}

	_, err := inf.List(ctx, toolhive.MCPServerGVK)
	if !errors.Is(err, toolhive.ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got: %v", err)
	}

	cancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

// TestInformer_LazyRetryRegistersOnCRDInstall — D-08 behavior 2.
//
// Setup: envtest with NO toolhive CRDs at startup.
// Action: launch informer with short retry interval (200ms); wait;
// install ToolHive CRDs mid-flight; wait for ready flip.
// Assertions:
// - IsReady starts false
// - After CRD install, IsReady flips to true within (retry × 5) seconds
// - List(MCPServerGVK) returns an empty UnstructuredList post-ready
// (no objects yet)
func TestInformer_LazyRetryRegistersOnCRDInstall(t *testing.T) {
	te := setupEnvtest(t)
	mgr := newManager(t, te)

	inf := &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 200 * time.Millisecond,
	}
	if err := mgr.Add(inf); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()

	// Confirm initially absent.
	time.Sleep(400 * time.Millisecond)
	if inf.IsReady() {
		t.Fatal("expected IsReady=false before CRD install")
	}

	// Install CRDs mid-flight.
	installToolhiveCRDs(t, te)

	// Poll for Ready flip — generous timeout to absorb informer cache
	// warm-up after CRD discovery refresh.
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(_ context.Context) (bool, error) {
			return inf.IsReady(), nil
		}); err != nil {
		t.Fatalf("informer did not become Ready after CRD install: %v", err)
	}

	// List should return an empty (but non-nil) list.
	list, err := inf.List(ctx, toolhive.MCPServerGVK)
	if err != nil {
		t.Fatalf("List after Ready: %v", err)
	}
	if list == nil {
		t.Fatal("expected non-nil UnstructuredList from List")
	}
	if len(list.Items) != 0 {
		t.Fatalf("expected empty list, got %d items", len(list.Items))
	}

	cancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

// TestInformer_ListReturnsLiveObjects — D-08 behavior 3.
//
// Setup: envtest with ToolHive CRDs preinstalled (via the manager
// options' CRDInstallOptions, simulating a cluster where ToolHive
// already exists at startup).
// Action: create a toolhive.stacklok.dev/v1beta1 MCPServer object via
// the dynamic client; wait for cache to observe it.
// Assertions:
// - List(MCPServerGVK) returns 1 item with the expected
// name/namespace within 10s.
func TestInformer_ListReturnsLiveObjects(t *testing.T) {
	te := setupEnvtest(t)
	installToolhiveCRDs(t, te)

	mgr := newManager(t, te)
	inf := &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 200 * time.Millisecond,
	}
	if err := mgr.Add(inf); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()

	// Wait for Ready (should be near-instant since CRDs are installed).
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(_ context.Context) (bool, error) {
			return inf.IsReady(), nil
		}); err != nil {
		t.Fatalf("informer did not become Ready: %v", err)
	}

	// Ensure a target namespace exists.
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("Namespace"))
	// Namespaces live at core/v1; use the typed client for that.
	// Reusing the unstructured path here would need core/v1 GVK, so
	// drop to the direct k8sClient.
	nsObj := &unstructured.Unstructured{}
	nsObj.SetAPIVersion("v1")
	nsObj.SetKind("Namespace")
	nsObj.SetName("dev")
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		// Namespace may already exist from a sibling test; tolerate.
		if !containsAlreadyExists(err.Error()) {
			t.Fatalf("create namespace dev: %v", err)
		}
	}

	// Create an MCPServer in the dev namespace.
	mcp := &unstructured.Unstructured{}
	mcp.SetGroupVersionKind(toolhive.MCPServerGVK)
	mcp.SetNamespace("dev")
	mcp.SetName("vendor-research")
	// status.url + status.transport so reconciler will be
	// happy — but this test only cares that the object appears in
	// List, not that the fields are read.
	_ = unstructured.SetNestedField(mcp.Object, "https://mcp.example.com", "status", "url")
	_ = unstructured.SetNestedField(mcp.Object, "http", "status", "transport")
	if err := te.k8sClient.Create(ctx, mcp); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	// Poll for the object to appear in the informer's cache via List.
	var seen *unstructured.UnstructuredList
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true,
		func(_ context.Context) (bool, error) {
			list, lerr := inf.List(ctx, toolhive.MCPServerGVK)
			if lerr != nil {
				return false, nil
			}
			if len(list.Items) == 1 {
				seen = list
				return true, nil
			}
			return false, nil
		}); err != nil {
		t.Fatalf("List did not return the created MCPServer within timeout: %v", err)
	}

	if got := seen.Items[0].GetName(); got != "vendor-research" {
		t.Fatalf("List returned wrong object: name=%q", got)
	}
	if got := seen.Items[0].GetNamespace(); got != "dev" {
		t.Fatalf("List returned wrong namespace: ns=%q", got)
	}

	cancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

// TestInformer_StartIsNonBlocking — D-08 behavior 4.
//
// Asserts that Informer.Start returns within 100ms even when CRDs are
// absent. This is the load-bearing contract per Phase 5 D-08: the
// manager's Setup phase must not block on absent ToolHive CRDs.
//
// Tests Start in isolation (not through mgr.Add) so the latency is the
// Informer's own contribution, not the manager's startup overhead.
func TestInformer_StartIsNonBlocking(t *testing.T) {
	te := setupEnvtest(t)
	mgr := newManager(t, te)

	// Start the manager in a goroutine so that mgr.GetCache is
	// initialized (the cache is constructed lazily — without
	// mgr.Start it would block in GetInformer). However, we measure
	// the latency of inf.Start directly, not through mgr.Add.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()

	// Give the manager a moment to initialize.
	time.Sleep(200 * time.Millisecond)

	inf := &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 5 * time.Second, // long retry — we don't want
		// the retry loop to fire during the measurement window
	}

	start := time.Now()
	if err := inf.Start(ctx); err != nil {
		t.Fatalf("inf.Start: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("Start took %v (>100ms); must be non-blocking per D-08", elapsed)
	}

	cancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

// containsAlreadyExists is a cheap substring check for AlreadyExists
// errors. Avoids pulling apierrors transitively.
func containsAlreadyExists(s string) bool {
	for _, sub := range []string{"already exists", "AlreadyExists"} {
		if containsSubstring(s, sub) {
			return true
		}
	}
	return false
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestInformer_FIX2_L11_RegisteredGVKsFull — FIX2.txt LOW-11
// (2026-05-22). RegisteredGVKs returns one v1beta1 GVK per discoverable
// kind (MCPServer, VirtualMCPServer, MCPRemoteProxy). The startup audit
// log lists this set honestly.
func TestInformer_FIX2_L11_RegisteredGVKsFull(t *testing.T) {
	te := setupEnvtest(t)
	installToolhiveCRDs(t, te)

	mgr := newManager(t, te)
	inf := &toolhive.Informer{
		Manager:       mgr,
		Log:           logr.Discard(),
		RetryInterval: 200 * time.Millisecond,
	}
	if err := mgr.Add(inf); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()

	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(_ context.Context) (bool, error) { return inf.IsReady(), nil }); err != nil {
		t.Fatalf("informer did not become Ready: %v", err)
	}

	got := inf.RegisteredGVKs()
	if len(got) != 3 {
		t.Fatalf("RegisteredGVKs(): got %d, want 3 (one per kind). got=%v", len(got), got)
	}
	perKind := map[string]int{}
	for _, gvk := range got {
		if gvk.Version != "v1beta1" {
			t.Fatalf("expected only v1beta1 GVKs, got %v", gvk)
		}
		perKind[gvk.Kind]++
	}
	if perKind["MCPServer"] != 1 || perKind["VirtualMCPServer"] != 1 || perKind["MCPRemoteProxy"] != 1 {
		t.Fatalf("expected 1 GVK per kind; got per-kind counts: %v", perKind)
	}

	cancel()
	select {
	case <-mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

// _ keeps the runtime / fmt imports busy if the test scaffold is
// refactored; harmless no-op.
var _ = fmt.Sprintf
var _ = runtime.Caller
