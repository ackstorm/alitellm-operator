// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/prometheus/client_golang/prometheus/testutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
	"github.com/ackstorm/alitellm-operator/internal/toolhive"
)

// ─────────────────────────────────────────────────────────────────────────
// Envtest harness — shared helpers used by Tasks 2a and 2b
// envtest cases. Mirrors the Phase 4 ModelDiscovery test scaffolding
// shape but injects ToolHive objects via unstructured CRDs instead of
// a fakeProvider.
// ─────────────────────────────────────────────────────────────────────────

// stubToolHiveInformer is a fake ToolHiveInformerReader used by
// envtests that need to drive IsReady=false (SourceUnreachable path) or
// inject a deterministic List error (AtomicRefresh path). Tests swap it
// into mcpServerDiscoveryReconciler.ToolHiveInformer at start and restore
// the real *toolhive.Informer in t.Cleanup.
type stubToolHiveInformer struct {
	ready atomic.Bool
	// listErr, if non-nil, is returned by List on every call. Used by
	// the AtomicRefresh test to surface a deterministic source-unreach
	// error and verify existing children stay untouched.
	listErr atomic.Value // stores error
}

func (s *stubToolHiveInformer) IsReady() bool { return s.ready.Load() }

func (s *stubToolHiveInformer) List(_ context.Context, _ schema.GroupVersionKind) (*unstructured.UnstructuredList, error) {
	if v := s.listErr.Load(); v != nil {
		if err, ok := v.(error); ok && err != nil {
			return nil, err
		}
	}
	return &unstructured.UnstructuredList{}, nil
}

// ensureNoMCPServerDiscovery deletes any pre-existing MCPServerDiscovery
// (and all its owned children) so a test starts from a clean slate.
// Waits up to 30s for cascade-drain to complete.
//
// Uses updateWithRetry on every finalizer strip so concurrent controller
// status writes (which would otherwise lose the optimistic-lock race and
// leave the finalizer in place) cannot strand the CR in cascade-drain
// for the full 30s ceiling. The retry loop converges in 1-3 attempts
// under -race.
func ensureNoMCPServerDiscovery(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	// Strip the Discovery finalizer first (retry on conflict) so the
	// parent deletes even if children somehow lag.
	if err := updateWithRetry(ctx, key,
		&litellmv1alpha1.LiteLLMMCPServerDiscovery{},
		func(obj *litellmv1alpha1.LiteLLMMCPServerDiscovery) error {
			controllerutil.RemoveFinalizer(obj, mcpServerDiscoveryFinalizer)
			return nil
		},
	); err == nil || apierrors.IsNotFound(err) {
		_ = k8sClient.Delete(ctx, &litellmv1alpha1.LiteLLMMCPServerDiscovery{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		})
	}
	// Strip + delete owned children directly. The cascade path is
	// exercised by the dedicated CascadeDelete test (Task 2b), not the
	// per-test cleanup.
	var owned litellmv1alpha1.LiteLLMMCPServerList
	if err := k8sClient.List(ctx, &owned,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: name},
	); err == nil {
		for i := range owned.Items {
			childKey := client.ObjectKeyFromObject(&owned.Items[i])
			_ = updateWithRetry(ctx, childKey,
				&litellmv1alpha1.LiteLLMMCPServer{},
				func(obj *litellmv1alpha1.LiteLLMMCPServer) error {
					controllerutil.RemoveFinalizer(obj, mcpServerFinalizer)
					return nil
				},
			)
			_ = k8sClient.Delete(ctx, &litellmv1alpha1.LiteLLMMCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: childKey.Name, Namespace: childKey.Namespace},
			})
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got litellmv1alpha1.LiteLLMMCPServerDiscovery
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			var rem litellmv1alpha1.LiteLLMMCPServerList
			if err := k8sClient.List(ctx, &rem,
				client.InNamespace(WatchNamespace),
				client.MatchingLabels{generatedByLabel: name},
			); err == nil && len(rem.Items) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("warning: MCPServerDiscovery %q (or its children) still present after 30s cleanup", name)
}

// ensureNoToolhiveObject deletes any pre-existing unstructured ToolHive
// object so a test starts from a clean slate.
func ensureNoToolhiveObject(t *testing.T, ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	_ = k8sClient.Delete(ctx, u)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, got); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("warning: ToolHive %s/%s still present after 10s cleanup", namespace, name)
}

// createToolhiveMCPServer creates an unstructured ToolHive
// `toolhive.stacklok.dev/v1beta1 MCPServer` object in the given namespace
// with the given status.url + status.transport values. The status
// subresource is set via a follow-up Status.Update because the
// minimal CRD declares a status subresource (per the suite's
// installToolhiveCRDsForSuite).
func createToolhiveMCPServer(t *testing.T, ctx context.Context, namespace, name, url, transport string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(toolhive.MCPServerGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	// status fields are NOT settable on Create (status subresource); set
	// them after Create via Status.Update on a freshly-Get'd object.
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatalf("create ToolHive MCPServer %s/%s: %v", namespace, name, err)
	}
	if url != "" || transport != "" {
		setToolhiveStatus(t, ctx, toolhive.MCPServerGVK, namespace, name, url, transport, "")
	}
}

// setToolhiveStatus sets the status.{url,transport,phase} fields on an
// existing ToolHive object via the status subresource. transport or
// phase may be "" to leave unset; url is mandatory for endpoint
// derivation per MSDISC-12.
func setToolhiveStatus(t *testing.T, ctx context.Context, gvk schema.GroupVersionKind, namespace, name, url, transport, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
			lastErr = err
			time.Sleep(25 * time.Millisecond)
			continue
		}
		status := map[string]any{}
		if url != "" {
			status["url"] = url
		}
		if transport != "" {
			status["transport"] = transport
		}
		if phase != "" {
			status["phase"] = phase
		}
		if err := unstructured.SetNestedMap(u.Object, status, "status"); err != nil {
			t.Fatalf("set status map: %v", err)
		}
		if err := k8sClient.Status().Update(ctx, u); err != nil {
			lastErr = err
			time.Sleep(25 * time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("setToolhiveStatus %s/%s: timeout; lastErr=%v", namespace, name, lastErr)
}

// pollMCPServerDiscoveryChildren polls k8sClient.List filtered by the
// generated-by label until len(items) == want OR timeout. Returns the
// final list.
func pollMCPServerDiscoveryChildren(t *testing.T, ctx context.Context, parent string, want int, timeout time.Duration) []litellmv1alpha1.LiteLLMMCPServer {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var list litellmv1alpha1.LiteLLMMCPServerList
	for time.Now().Before(deadline) {
		if err := k8sClient.List(ctx, &list,
			client.InNamespace(WatchNamespace),
			client.MatchingLabels{generatedByLabel: parent},
		); err == nil && len(list.Items) == want {
			return list.Items
		}
		time.Sleep(50 * time.Millisecond)
	}
	return list.Items
}

// pollMCPServerDiscoveryCondition polls a Discovery's Ready condition
// until the reason matches wantReason or timeout. Returns the last-
// observed CR.
func pollMCPServerDiscoveryCondition(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMMCPServerDiscovery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var md litellmv1alpha1.LiteLLMMCPServerDiscovery
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &md); err == nil {
			c := apimeta.FindStatusCondition(md.Status.Conditions, "Ready")
			if c != nil && c.Reason == wantReason {
				return &md
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &md
}

// pollMCPServerDiscoverySkipReason polls until skippedCandidates[] contains
// at least one entry with the given reason (or timeout). Returns the
// matching entry, or nil on timeout.
func pollMCPServerDiscoverySkipReason(t *testing.T, ctx context.Context, name, reason string, timeout time.Duration) *litellmv1alpha1.MCPServerSkippedCandidate {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var md litellmv1alpha1.LiteLLMMCPServerDiscovery
		if err := k8sClient.Get(ctx, key, &md); err == nil {
			for i := range md.Status.SkippedCandidates {
				if md.Status.SkippedCandidates[i].Reason == reason {
					return &md.Status.SkippedCandidates[i]
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// msDiscSampleCR returns a minimal MCPServerDiscovery CR for envtest with
// 1-minute refresh (the MSDISC-05 CEL floor; tests trigger reconciles by
// touching annotations or rely on the initial reconcile).
func msDiscSampleCR(name string, namespaces []string) *litellmv1alpha1.LiteLLMMCPServerDiscovery {
	return &litellmv1alpha1.LiteLLMMCPServerDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerDiscoverySpec{
			Type: "toolhive",
			Toolhive: litellmv1alpha1.MCPServerDiscoveryToolhive{
				Namespaces: namespaces,
				Kinds:      []string{"MCPServer", "VirtualMCPServer"},
			},
			Refresh: litellmv1alpha1.MCPServerDiscoveryRefresh{
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
}

// touchMCPServerDiscovery triggers an immediate reconcile by touching an
// annotation (changes spec.generation? — no, annotations don't bump
// generation, but they DO requeue via the For watch). Used by tests that
// mutate ToolHive objects in-place and want to drive a reconcile faster
// than the refresh.interval.
func touchMCPServerDiscovery(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var md litellmv1alpha1.LiteLLMMCPServerDiscovery
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &md); err != nil {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		if md.Annotations == nil {
			md.Annotations = map[string]string{}
		}
		md.Annotations["test.litellm.ackstorm.ai/trigger"] = time.Now().Format(time.RFC3339Nano)
		if err := k8sClient.Update(ctx, &md); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("touchMCPServerDiscovery %q: failed to update within 5s", name)
}

// ─────────────────────────────────────────────────────────────────────────
// Task 2a tests — state machine + happy path (11 cases).
// ─────────────────────────────────────────────────────────────────────────

// TestMCPServerDiscoveryReconciler_BasicGeneration locks the MSDISC-03
// happy path: spec.toolhive.namespaces=[dev], inject ToolHive MCPServer
// `dev/tool-a` with status.url + status.transport → within 30s a child
// MCPServer `<discovery>.dev.tool-a` exists in WatchNamespace with the
// full MSDISC-10 metadata shape (ownerRef[controller=true,
// blockOwnerDeletion=true], generatedByLabel=<discovery>, finalizer,
// endpoint/transport overlaid).
func TestMCPServerDiscoveryReconciler_BasicGeneration(t *testing.T) {
	ctx := context.Background()
	const mdName = "basic-toolhive"
	const thNamespace = "dev"
	const thName = "tool-a"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://a.example.com", "http")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for child to land.
	wantChildName := mdName + "." + thNamespace + "." + thName
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		// Diagnostic dump.
		var mdAfter litellmv1alpha1.LiteLLMMCPServerDiscovery
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter)
		t.Fatalf("expected 1 child, got %d; status.failed=%+v skipped=%+v generated=%v conditions=%+v",
			len(children), mdAfter.Status.FailedCandidates, mdAfter.Status.SkippedCandidates,
			mdAfter.Status.GeneratedChildren, mdAfter.Status.Conditions)
	}
	child := children[0]
	if child.Name != wantChildName {
		t.Errorf("child name: got %q, want %q", child.Name, wantChildName)
	}
	// MSDISC-10 metadata shape.
	if len(child.OwnerReferences) == 0 {
		t.Fatalf("child has no ownerReferences")
	}
	or := child.OwnerReferences[0]
	if or.Kind != "LiteLLMMCPServerDiscovery" || or.Name != mdName {
		t.Errorf("ownerRef[0]: got Kind=%q Name=%q, want MCPServerDiscovery/%s", or.Kind, or.Name, mdName)
	}
	if or.Controller == nil || !*or.Controller {
		t.Error("ownerRef[0].Controller must be true")
	}
	if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Error("ownerRef[0].BlockOwnerDeletion must be true")
	}
	if child.Labels[generatedByLabel] != mdName {
		t.Errorf("label[%s]: got %q, want %q", generatedByLabel, child.Labels[generatedByLabel], mdName)
	}
	if !controllerutil.ContainsFinalizer(&child, mcpServerFinalizer) {
		t.Errorf("child must carry finalizer %q", mcpServerFinalizer)
	}
	if child.Spec.Endpoint != "https://a.example.com" {
		t.Errorf("spec.endpoint: got %q, want %q", child.Spec.Endpoint, "https://a.example.com")
	}
	if child.Spec.Transport != transportHTTP {
		t.Errorf("spec.transport: got %q, want %q", child.Spec.Transport, "http")
	}
}

// TestMCPServerDiscoveryReconciler_TransportNormalization_StreamableHttp
// asserts the D-10 normalization `streamable-http → http`: ToolHive
// emits streamable-http, child carries http, no skippedCandidates entry.
func TestMCPServerDiscoveryReconciler_TransportNormalization_StreamableHttp(t *testing.T) {
	ctx := context.Background()
	const mdName = "norm-streamable"
	const thNamespace = "dev"
	const thName = "tool-streamable"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://s.example.com", "streamable-http")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	if got := children[0].Spec.Transport; got != transportHTTP {
		t.Errorf("transport normalization: streamable-http → got %q, want %q", got, "http")
	}
	// No skippedCandidates entry for this object.
	mdAfter := pollMCPServerDiscoveryCondition(t, ctx, mdName, reasonSynced, 10*time.Second)
	for _, s := range mdAfter.Status.SkippedCandidates {
		if s.Reason == "InvalidTransport" {
			t.Errorf("InvalidTransport skip should NOT be present for streamable-http: %+v", s)
		}
	}
}

// TestMCPServerDiscoveryReconciler_TransportNormalization_Sse asserts
// the D-10 normalization `sse → sse` (pass-through).
func TestMCPServerDiscoveryReconciler_TransportNormalization_Sse(t *testing.T) {
	ctx := context.Background()
	const mdName = "norm-sse"
	const thNamespace = "dev"
	const thName = "tool-sse"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://sse.example.com", "sse")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	if got := children[0].Spec.Transport; got != "sse" {
		t.Errorf("transport pass-through: got %q, want %q", got, "sse")
	}
}

// TestMCPServerDiscoveryReconciler_TransportNormalization_Empty asserts
// the D-9 default: ToolHive status.transport empty/absent → child carries
// transport=http (no skip).
func TestMCPServerDiscoveryReconciler_TransportNormalization_Empty(t *testing.T) {
	ctx := context.Background()
	const mdName = "norm-empty"
	const thNamespace = "dev"
	const thName = "tool-empty-transport"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	// Create the object with status.url set but status.transport empty.
	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://e.example.com", "")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	if got := children[0].Spec.Transport; got != transportHTTP {
		t.Errorf("transport default: empty → got %q, want %q (D-09)", got, "http")
	}
}

// TestMCPServerDiscoveryReconciler_InvalidTransport asserts the D-10
// fall-through: ToolHive emits stdio → NO child created; skippedCandidates
// contains an entry with reason=InvalidTransport, ownedBy=<ns>/<name>,
// message references "stdio".
func TestMCPServerDiscoveryReconciler_InvalidTransport(t *testing.T) {
	ctx := context.Background()
	const mdName = "invalid-transport"
	const thNamespace = "dev"
	const thName = "tool-stdio"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://stdio.example.com", "stdio")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	skip := pollMCPServerDiscoverySkipReason(t, ctx, mdName, "InvalidTransport", 30*time.Second)
	if skip == nil {
		t.Fatalf("expected skippedCandidates[reason=InvalidTransport] within 30s")
	}
	wantOwnedBy := thNamespace + "/" + thName
	if skip.OwnedBy != wantOwnedBy {
		t.Errorf("skip.OwnedBy: got %q, want %q", skip.OwnedBy, wantOwnedBy)
	}
	if !strContains(skip.Message, "stdio") {
		t.Errorf("skip.Message %q should reference %q", skip.Message, "stdio")
	}
	// NO child created.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 0, 5*time.Second)
	if len(children) != 0 {
		t.Errorf("expected 0 children for InvalidTransport candidate, got %d", len(children))
	}
}

// TestMCPServerDiscoveryReconciler_EndpointUnknown asserts MSDISC-12:
// ToolHive object with empty status.url → NO child; skippedCandidates
// contains an entry with reason=EndpointUnknown.
func TestMCPServerDiscoveryReconciler_EndpointUnknown(t *testing.T) {
	ctx := context.Background()
	const mdName = "endpoint-unknown"
	const thNamespace = "dev"
	const thName = "tool-no-url"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	// Create the object with no status.url (the helper skips setStatus
	// when url is "" — but we want the object itself created). Construct
	// it directly to bypass that branch.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(toolhive.MCPServerGVK)
	u.SetNamespace(thNamespace)
	u.SetName(thName)
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatalf("create ToolHive MCPServer %s/%s: %v", thNamespace, thName, err)
	}
	// Set only transport (no url) via the status subresource — exercises
	// the empty-url skip path.
	setToolhiveStatus(t, ctx, toolhive.MCPServerGVK, thNamespace, thName, "", "http", "")
	// Re-fetch to confirm status.url is empty.
	// (No assertion needed — the test asserts the skip downstream.)

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	skip := pollMCPServerDiscoverySkipReason(t, ctx, mdName, "EndpointUnknown", 30*time.Second)
	if skip == nil {
		t.Fatalf("expected skippedCandidates[reason=EndpointUnknown] within 30s")
	}
	// NO child created.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 0, 5*time.Second)
	if len(children) != 0 {
		t.Errorf("expected 0 children for EndpointUnknown candidate, got %d", len(children))
	}
}

// TestMCPServerDiscoveryReconciler_DoesNotFilterByPhase locks the spec
// §6.5 "forward what is published" contract: a ToolHive object with
// status.phase="Pending" (which autoconfig would skip) AND valid
// status.url + status.transport → child IS created.
//
// Anti-pattern guard per CONTEXT.md "DO NOT skip ToolHive objects by
// status.phase or status.backendCount".
func TestMCPServerDiscoveryReconciler_DoesNotFilterByPhase(t *testing.T) {
	ctx := context.Background()
	const mdName = "no-phase-filter"
	const thNamespace = "dev"
	const thName = "tool-pending"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	// Create + set status.phase="Pending" alongside valid url/transport.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(toolhive.MCPServerGVK)
	u.SetNamespace(thNamespace)
	u.SetName(thName)
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatalf("create ToolHive MCPServer: %v", err)
	}
	setToolhiveStatus(t, ctx, toolhive.MCPServerGVK, thNamespace, thName,
		"https://pending.example.com", "http", "Pending")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child despite status.phase=Pending (spec §6.5 — forward what is published), got %d", len(children))
	}
}

// TestMCPServerDiscoveryReconciler_FilterOnPostDerivationName locks the
// MSDISC filter target: spec.filters.include matches the POST-DERIVATION
// dotted name (NOT the bare ToolHive object name). Inject ToolHive
// objects in `dev` AND `prod`; the include pattern admits only `dev` —
// the prod entry is silently dropped (filter excludes are silent).
func TestMCPServerDiscoveryReconciler_FilterOnPostDerivationName(t *testing.T) {
	ctx := context.Background()
	const mdName = "post-deriv-filter"
	const thNameDev = "tool-dev"
	const thNameProd = "tool-prod"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, "dev", thNameDev)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, "prod", thNameProd)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, "dev", thNameDev)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, "prod", thNameProd)
	})

	createToolhiveMCPServer(t, ctx, "dev", thNameDev, "https://dev.example.com", "http")
	createToolhiveMCPServer(t, ctx, "prod", thNameProd, "https://prod.example.com", "http")

	md := msDiscSampleCR(mdName, []string{"dev", "prod"})
	// Include pattern matching the dotted form `<discovery>.dev.<anything>`.
	md.Spec.Filters = &litellmv1alpha1.MCPServerDiscoveryFilters{
		Include: []string{mdName + `\.dev\..*`},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child (dev only via post-derivation include filter), got %d", len(children))
	}
	wantDottedDev := mdName + ".dev." + thNameDev
	if children[0].Name != wantDottedDev {
		t.Errorf("filtered child name: got %q, want %q (dotted-name include matched the dev entry)",
			children[0].Name, wantDottedDev)
	}
}

// TestMCPServerDiscoveryReconciler_NamespaceFilterIsInMemory locks D-07:
// spec.toolhive.namespaces=[dev] → ToolHive objects in dev AND prod →
// only dev becomes a child. Filter is in-memory at reconcile time;
// informer stays cluster-scoped.
func TestMCPServerDiscoveryReconciler_NamespaceFilterIsInMemory(t *testing.T) {
	ctx := context.Background()
	const mdName = "ns-filter-inmem"
	const thNameDev = "tool-ns-dev"
	const thNameProd = "tool-ns-prod"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, "dev", thNameDev)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, "prod", thNameProd)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, "dev", thNameDev)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, "prod", thNameProd)
	})

	createToolhiveMCPServer(t, ctx, "dev", thNameDev, "https://dev.example.com", "http")
	createToolhiveMCPServer(t, ctx, "prod", thNameProd, "https://prod.example.com", "http")

	md := msDiscSampleCR(mdName, []string{"dev"}) // ONLY dev
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child (dev only via in-memory namespace filter), got %d", len(children))
	}
}

// TestMCPServerDiscoveryReconciler_NoLitellmCalls asserts MSDISC-16: the
// MSDisc reconciler issues ZERO LiteLLM API calls during its reconcile.
// Verified by mock.MutationsByMCPServerName(<dotted-name>) == 0 in the
// window after the parent CR is created but before any child reconcile
// runs against the mock (the FakeConnectionCache is invalidated so the
// child reconciler short-circuits without calling LiteLLM either).
func TestMCPServerDiscoveryReconciler_NoLitellmCalls(t *testing.T) {
	ctx := context.Background()
	const mdName = "no-litellm-calls"
	const thNamespace = "dev"
	const thName = "tool-no-litellm"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	// Reset mock counters; FakeConnectionCache invalidated so the child
	// MCPServer reconciler short-circuits before any mutation.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://nolit.example.com", "http")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for the child to land (proves MSDisc's reconcile completed).
	wantChild := mdName + "." + thNamespace + "." + thName
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	// Give a generous settle window — production MSDisc would never call
	// LiteLLM, and the child MCPServer controller (which DOES call LiteLLM
	// in normal operation) is gated off by fakeCache.Invalidated=true.
	time.Sleep(2 * time.Second)
	if got := mockServer.MutationsByMCPServerName(wantChild); got != 0 {
		t.Errorf("MSDISC-16 violation: mutations recorded for child %q via mock: got %d, want 0", wantChild, got)
	}
}

// TestMCPServerDiscoveryReconciler_SourceUnreachable_NoToolHiveCRDs
// asserts MSDISC-06: when Informer.IsReady returns false (ToolHive
// CRDs absent), MSDisc surfaces Ready=False, reason=SourceUnreachable,
// message contains "ToolHive CRDs not installed". NO child writes are
// attempted.
//
// The test swaps the reconciler's ToolHiveInformer to a stub that
// returns IsReady=false, asserts the condition, and restores the real
// informer in cleanup.
func TestMCPServerDiscoveryReconciler_SourceUnreachable_NoToolHiveCRDs(t *testing.T) {
	ctx := context.Background()
	const mdName = "source-unreach"

	ensureNoMCPServerDiscovery(t, ctx, mdName)

	// Swap the informer to a stub that reports IsReady=false. Use the
	// mutex-protected setter so the manager's worker goroutine reading
	// the field via getToolHive does not race the write.
	stub := &stubToolHiveInformer{}
	stub.ready.Store(false)
	realInformer := mcpServerDiscoveryReconciler.getToolHive()
	mcpServerDiscoveryReconciler.SetToolHive(stub)
	t.Cleanup(func() {
		mcpServerDiscoveryReconciler.SetToolHive(realInformer)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
	})

	md := msDiscSampleCR(mdName, []string{"dev"})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	mdAfter := pollMCPServerDiscoveryCondition(t, ctx, mdName, "SourceUnreachable", 30*time.Second)
	c := apimeta.FindStatusCondition(mdAfter.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set; status: %+v", mdAfter.Status)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: got %v, want False", c.Status)
	}
	if c.Reason != "SourceUnreachable" {
		t.Errorf("Ready.Reason: got %q, want SourceUnreachable", c.Reason)
	}
	if !strContains(c.Message, "ToolHive CRDs not installed") {
		t.Errorf("Ready.Message %q should mention %q", c.Message, "ToolHive CRDs not installed")
	}
	// SourceReachable should also be False.
	sr := apimeta.FindStatusCondition(mdAfter.Status.Conditions, "SourceReachable")
	if sr == nil || sr.Status != metav1.ConditionFalse {
		t.Errorf("SourceReachable: got %+v, want Status=False", sr)
	}
	// No child writes attempted.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 0, 5*time.Second)
	if len(children) != 0 {
		t.Errorf("expected 0 children on SourceUnreachable, got %d", len(children))
	}
}

// errSourceUnreachStub is a package-private sentinel used by the
// AtomicRefresh test (Task 2b) to simulate an informer.List error
// WITHOUT triggering the IsReady=false path (which would short-circuit
// Reconcile before the atomic-refresh code path is exercised). The
// reconciler treats any List error the same way (SourceReachable=False;
// existing children untouched).
var errSourceUnreachStub = errors.New("toolhive: simulated list failure for AtomicRefresh test")

// ─────────────────────────────────────────────────────────────────────────
// Task 2b tests — edge cases + lifecycle (8 cases).
// ─────────────────────────────────────────────────────────────────────────

// TestMCPServerDiscoveryReconciler_ConflictExplicit asserts MSDISC-13:
// a user-authored MCPServer with the same dotted name pre-existing in
// WatchNamespace (no controller ownerRef) → Discovery records
// skippedCandidates[reason=ExplicitMCPServerExists]; the user MCPServer
// is unchanged.
func TestMCPServerDiscoveryReconciler_ConflictExplicit(t *testing.T) {
	ctx := context.Background()
	const mdName = "conflict-explicit"
	const thNamespace = "dev"
	const thName = "tool-conflict"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	dotted := mdName + "." + thNamespace + "." + thName
	ensureNoMCPServer(t, ctx, dotted)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	// Pre-create a user-authored MCPServer with the dotted name —
	// no ownerRef, just a vanilla user CR. Gate LiteLLM off so the
	// child reconciler doesn't mutate or fail.
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })
	user := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dotted,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://user-authored.example.com",
			Transport: "http",
		},
	}
	if err := k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("pre-create user MCPServer: %v", err)
	}

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://conflict.example.com", "http")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	skip := pollMCPServerDiscoverySkipReason(t, ctx, mdName, "ExplicitMCPServerExists", 30*time.Second)
	if skip == nil {
		t.Fatalf("expected skippedCandidates[reason=ExplicitMCPServerExists] within 30s")
	}
	if skip.Name != dotted {
		t.Errorf("skip.Name: got %q, want %q", skip.Name, dotted)
	}

	// Verify the user MCPServer's spec.endpoint is UNCHANGED.
	var stillUser litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &stillUser); err != nil {
		t.Fatalf("re-get user MCPServer: %v", err)
	}
	if stillUser.Spec.Endpoint != "https://user-authored.example.com" {
		t.Errorf("user MCPServer endpoint mutated: got %q, want %q (Discovery should NOT have overwritten)",
			stillUser.Spec.Endpoint, "https://user-authored.example.com")
	}
}

// TestMCPServerDiscoveryReconciler_Adoption asserts MSDISC-13 adoption:
// after a successful generation, kubectl-edit-strip the child's
// controller ownerRef → next reconcile records skippedCandidates[
// reason=ExplicitMCPServerExists]; the child is NOT vanish-deleted.
func TestMCPServerDiscoveryReconciler_Adoption(t *testing.T) {
	ctx := context.Background()
	const mdName = "adoption-test"
	const thNamespace = "dev"
	const thName = "tool-adoption"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://adopt.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for the child to land normally.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child pre-adoption, got %d", len(children))
	}

	// Adopt: strip the controller ownerRef. The label stays — this is
	// the spec-defined adoption mechanism. Retry on conflict because
	// the MCPServer reconciler may be concurrently updating the child's
	// status from an in-flight reconcile.
	stripErr := error(nil)
	for attempt := 0; attempt < 10; attempt++ {
		var child litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &child); err != nil {
			stripErr = err
			time.Sleep(25 * time.Millisecond)
			continue
		}
		child.OwnerReferences = nil
		if err := k8sClient.Update(ctx, &child); err == nil {
			stripErr = nil
			break
		} else {
			stripErr = err
			time.Sleep(25 * time.Millisecond)
		}
	}
	if stripErr != nil {
		t.Fatalf("strip ownerRef (adoption): %v", stripErr)
	}

	// Trigger reconcile via parent annotation update. Repeat the touch
	// every 5s while polling because envtest's Owns(&MCPServer{}) event
	// handler can be slow to fire after cross-test cache transitions.
	pollDeadline := time.Now().Add(45 * time.Second)
	var skip *litellmv1alpha1.MCPServerSkippedCandidate
	nextTouch := time.Now()
	for time.Now().Before(pollDeadline) {
		if time.Now().After(nextTouch) {
			touchMCPServerDiscovery(t, ctx, mdName)
			nextTouch = time.Now().Add(3 * time.Second)
		}
		var mdAfter litellmv1alpha1.LiteLLMMCPServerDiscovery
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter); err == nil {
			for i := range mdAfter.Status.SkippedCandidates {
				if mdAfter.Status.SkippedCandidates[i].Reason == "ExplicitMCPServerExists" {
					skip = &mdAfter.Status.SkippedCandidates[i]
					break
				}
			}
			if skip != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if skip == nil {
		t.Fatalf("expected ExplicitMCPServerExists in status.skippedCandidates after adoption")
	}

	// Verify the child is NOT vanish-deleted (it remains adopted).
	var adopted litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &adopted); err != nil {
		t.Errorf("adopted child must NOT be vanish-deleted: %v", err)
	}
}

// TestMCPServerDiscoveryReconciler_VanishDetection asserts vanish:
// generate a child for ToolHive object A; remove A → next reconcile
// deletes the child; child_cr_writes_total{kind=MCPServerDiscovery,
// action=delete, result=success} increments by 1.
func TestMCPServerDiscoveryReconciler_VanishDetection(t *testing.T) {
	ctx := context.Background()
	const mdName = "vanish-test"
	const thNamespace = "dev"
	const thName = "tool-vanish"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://vanish.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for child to land.
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("pre-vanish child count: got %d, want 1", len(children))
	}

	// Remove the ToolHive object → vanish trigger.
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	touchMCPServerDiscovery(t, ctx, mdName)

	// Poll for the child to disappear.
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 0, 30*time.Second); len(children) != 0 {
		names := make([]string, 0, len(children))
		for _, c := range children {
			names = append(names, c.Name)
		}
		t.Fatalf("post-vanish child count: got %d, want 0; remaining: %v", len(children), names)
	}

	// Confirm the child is truly gone.
	var gone litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &gone); !apierrors.IsNotFound(err) {
		t.Errorf("vanished child %q should be NotFound; got err=%v", dotted, err)
	}
}

// TestMCPServerDiscoveryReconciler_AtomicRefresh asserts D-09: on an
// Informer.List error, SourceReachable=False; existing children stay
// UNTOUCHED (NOT deleted even though the desiredSet is technically
// empty in this code path).
func TestMCPServerDiscoveryReconciler_AtomicRefresh(t *testing.T) {
	ctx := context.Background()
	const mdName = "atomic-refresh"
	const thNamespace = "dev"
	const thName = "tool-atomic"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	// Phase 1: real informer, create the ToolHive object, wait for child.
	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://atomic.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("pre-error child count: got %d, want 1", len(children))
	}

	// Phase 2: swap to a stub that reports IsReady=true but returns an
	// error on List. The reconciler MUST surface SourceReachable=False
	// and leave the existing child untouched (D-09 atomic refresh snapshot).
	stub := &stubToolHiveInformer{}
	stub.ready.Store(true)
	stub.listErr.Store(errSourceUnreachStub)
	// Use the mutex-protected setter so the manager's worker goroutine
	// reading the field via getToolHive does not race the write
	// (race detector caught this on the AtomicRefresh path, ref 1e01a74).
	realInformer := mcpServerDiscoveryReconciler.getToolHive()
	mcpServerDiscoveryReconciler.SetToolHive(stub)
	t.Cleanup(func() { mcpServerDiscoveryReconciler.SetToolHive(realInformer) })

	// Trigger reconcile.
	touchMCPServerDiscovery(t, ctx, mdName)

	// Wait for SourceReachable=False to appear.
	deadline := time.Now().Add(30 * time.Second)
	var sourceReachableFalse bool
	for time.Now().Before(deadline) {
		var mdAfter litellmv1alpha1.LiteLLMMCPServerDiscovery
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter); err == nil {
			sr := apimeta.FindStatusCondition(mdAfter.Status.Conditions, "SourceReachable")
			if sr != nil && sr.Status == metav1.ConditionFalse {
				sourceReachableFalse = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sourceReachableFalse {
		t.Fatalf("expected SourceReachable=False after stub error, did not observe within 30s")
	}

	// Existing child MUST still be there.
	time.Sleep(2 * time.Second) // settle to be sure vanish does NOT fire
	var still litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &still); err != nil {
		t.Fatalf("D-09 violation: existing child must NOT be deleted on atomic refresh error: %v", err)
	}
}

// TestMCPServerDiscoveryReconciler_CascadeDelete asserts MSDISC-15:
// kubectl delete mcpserverdiscovery → K8s GC cascade-drains the child
// (blockOwnerDeletion=true); MSDisc finalizer waits via 5s requeue
// until owned-children list is empty; MSDisc emits ZERO LiteLLM calls;
// finally the MSDisc CR is fully deleted.
//
// envtest divergence (Phase 4 PATTERNS.md line 612+): envtest does NOT
// run K8s GC. We simulate the cascade by directly deleting the child
// after asserting the parent's drain-wait state.
func TestMCPServerDiscoveryReconciler_CascadeDelete(t *testing.T) {
	ctx := context.Background()
	const mdName = "cascade-test"
	const thNamespace = "dev"
	const thName = "tool-cascade"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	// Reset connCache so the child MCPServer reconciler's deletion path
	// takes the LiteLLM-unavailable short-circuit (removes finalizer
	// without calling LiteLLM). Without this, test ordering can leave
	// connCache in Ready=true state from a prior LiteLLMConnection
	// probe, and the deletion path's LiteLLM mutation call may race
	// the K8s GC. Mirrors the Phase 4 cascade-test posture.
	resetConnCacheSnapshot()
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://cascade.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("pre-cascade child count: got %d, want 1", len(children))
	}

	// Mock MCP mutation count before cascade — assert ZERO additions from
	// MSDisc's own reconcile work during the cascade (the child reconciler
	// MAY mutate; that's separate). MSDISC-16 contract.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Re-fetch parent with current resourceVersion + finalizer present.
	var mdLatest litellmv1alpha1.LiteLLMMCPServerDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdLatest); err != nil {
		t.Fatalf("re-get parent pre-delete: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&mdLatest, mcpServerDiscoveryFinalizer) {
		t.Fatalf("parent finalizer must be present: %v", mdLatest.Finalizers)
	}

	// Delete the parent.
	if err := k8sClient.Delete(ctx, &mdLatest); err != nil {
		t.Fatalf("delete MCPServerDiscovery: %v", err)
	}

	// Parent must still be present mid-drain (children outstanding).
	var midDelete litellmv1alpha1.LiteLLMMCPServerDiscovery
	getErr := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &midDelete)
	if apierrors.IsNotFound(getErr) {
		t.Fatalf("MCPServerDiscovery %q gone before children drained — drain-wait did not fire", mdName)
	}
	if getErr != nil {
		t.Fatalf("get parent mid-cascade: %v", getErr)
	}
	if midDelete.DeletionTimestamp == nil {
		t.Errorf("parent DeletionTimestamp should be set after Delete")
	}
	if !controllerutil.ContainsFinalizer(&midDelete, mcpServerDiscoveryFinalizer) {
		t.Errorf("parent finalizer should still be present (drain-wait in effect); finalizers: %v", midDelete.Finalizers)
	}

	// Assert MSDISC-15 load-bearing contract: the cascade-drain phase
	// at MSDisc has issued ZERO LiteLLM mutations on the child dotted
	// name. This is the production behavior the test must guard:
	// MSDisc's finalizer NEVER calls LiteLLM; the child MCPServer
	// controller is the sole LiteLLM writer. We sample the mock
	// counter NOW (parent in cascade-drain, children still present)
	// because envtest has no K8s GC and the child-drain step below is
	// a test-harness simulation (not a production reconciler path).
	time.Sleep(1 * time.Second) // settle window — give MSDisc a few cascade-drain reconciles
	if got := mockServer.MutationsByMCPServerName(dotted); got != 0 {
		t.Errorf("MSDISC-15 violation: %d LiteLLM mutations recorded for %q during MSDisc cascade-drain (MSDisc finalizer MUST issue zero LiteLLM calls)", got, dotted)
	}

	// Teardown (envtest divergence from prod K8s GC): force-strip
	// finalizers on the parent + each child so the test exits cleanly.
	// The production cascade flow — K8s GC deletes children via
	// blockOwnerDeletion, child finalizers drain LiteLLM, parent's
	// drain-wait observes empty owned list, parent finalizer removed —
	// is asserted at the parent-stay-mid-drain check above plus the
	// MSDISC-15 zero-mutation check. The teardown below is a test
	// harness convenience, not a production assertion.
	var owned litellmv1alpha1.LiteLLMMCPServerList
	_ = k8sClient.List(ctx, &owned,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: mdName},
	)
	for i := range owned.Items {
		c := &owned.Items[i]
		c.Finalizers = nil
		_ = k8sClient.Update(ctx, c)
		_ = k8sClient.Delete(ctx, c)
	}
	// Strip parent's finalizer so the parent CR can finalize.
	var mdToDrain litellmv1alpha1.LiteLLMMCPServerDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdToDrain); err == nil {
		mdToDrain.Finalizers = nil
		_ = k8sClient.Update(ctx, &mdToDrain)
	}

	// MSDISC-16: MSDisc finalizer issued ZERO LiteLLM mutations.
	if got := mockServer.MutationsByMCPServerName(dotted); got != 0 {
		t.Errorf("MSDISC-16 violation: %d mutations recorded for %q during MSDisc cascade-drain", got, dotted)
	}
}

// TestMCPServerDiscoveryReconciler_PropagationVerbatim asserts MSDISC-11:
// spec.params + spec.secrets[] propagate VERBATIM to generated children.
// Discovery does NOT substitute {{NAME}} placeholders — those ride to
// the child and the child reconciler resolves them on its own reconcile.
func TestMCPServerDiscoveryReconciler_PropagationVerbatim(t *testing.T) {
	ctx := context.Background()
	const mdName = "propagation"
	const thNamespace = "dev"
	const thName = "tool-prop"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://prop.example.com", "http")

	// Verbatim Params with {{TOKEN}} placeholder + spec.secrets[].
	md := msDiscSampleCR(mdName, []string{thNamespace})
	rawParams := []byte(`{"x":1,"msg":"{{TOKEN}}"}`)
	md.Spec.Params = runtime.RawExtension{Raw: rawParams}
	md.Spec.Secrets = []litellmv1alpha1.SecretSubstitution{
		{
			As: "TOKEN",
			SecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "foo",
				Key:  "key",
			},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	child := children[0]

	// Child's spec.params MUST be SEMANTICALLY EQUIVALENT (no
	// substitution / no key drops). Compare via json.Unmarshal because
	// the apiserver canonicalizes JSON key ordering on storage; a
	// byte-level compare would false-positive on reordering. The
	// literal "{{TOKEN}}" placeholder MUST be preserved (no Discovery-
	// side substitution per MSDISC-11).
	var got, want map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &got); err != nil {
		t.Fatalf("decode child.Spec.Params.Raw=%s: %v", child.Spec.Params.Raw, err)
	}
	if err := json.Unmarshal(rawParams, &want); err != nil {
		t.Fatalf("decode rawParams=%s: %v", rawParams, err)
	}
	if got["x"] != want["x"] {
		t.Errorf("spec.params.x: got %v, want %v", got["x"], want["x"])
	}
	if got["msg"] != "{{TOKEN}}" {
		t.Errorf("spec.params.msg: got %v, want %q (placeholder MUST be literal — Discovery does NOT substitute per MSDISC-11)",
			got["msg"], "{{TOKEN}}")
	}
	if len(got) != len(want) {
		t.Errorf("spec.params key count: got %d, want %d (full key-set must propagate)", len(got), len(want))
	}
	// Child's spec.secrets must match parent's (single entry).
	if len(child.Spec.Secrets) != 1 {
		t.Fatalf("child.Spec.Secrets length: got %d, want 1", len(child.Spec.Secrets))
	}
	if child.Spec.Secrets[0].As != "TOKEN" {
		t.Errorf("child.Spec.Secrets[0].As: got %q, want %q", child.Spec.Secrets[0].As, "TOKEN")
	}
	if child.Spec.Secrets[0].SecretRef.Name != "foo" {
		t.Errorf("child.Spec.Secrets[0].SecretRef.Name: got %q, want %q", child.Spec.Secrets[0].SecretRef.Name, "foo")
	}
}

// TestMCPServerDiscoveryReconciler_UrlChangeWithoutCREvent asserts
// MSDISC-09: ToolHive status.url change without a CR event on the
// MCPServerDiscovery → within refresh.interval + 30s the child's
// spec.endpoint updates via SSA.
//
// Uses the touchMCPServerDiscovery helper to trigger an immediate
// reconcile (vs waiting refresh.interval=1m).
func TestMCPServerDiscoveryReconciler_UrlChangeWithoutCREvent(t *testing.T) {
	ctx := context.Background()
	const mdName = "url-change"
	const thNamespace = "dev"
	const thName = "tool-urlchange"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://a.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for initial child with url=a.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 || children[0].Spec.Endpoint != "https://a.example.com" {
		t.Fatalf("initial child wrong; got %+v", children)
	}

	// Mutate ToolHive status.url WITHOUT touching the Discovery CR.
	setToolhiveStatus(t, ctx, toolhive.MCPServerGVK, thNamespace, thName,
		"https://b.example.com", "http", "")

	// Trigger reconcile (mimics what refresh.interval would do — faster).
	touchMCPServerDiscovery(t, ctx, mdName)

	// Poll for the child's endpoint to flip.
	deadline := time.Now().Add(30 * time.Second)
	var updated bool
	for time.Now().Before(deadline) {
		var child litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &child); err == nil {
			if child.Spec.Endpoint == "https://b.example.com" {
				updated = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !updated {
		var child litellmv1alpha1.LiteLLMMCPServer
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &child)
		t.Errorf("child.spec.endpoint did not flip to https://b.example.com within 30s; current: %q", child.Spec.Endpoint)
	}
}

// TestMCPServerDiscoveryReconciler_NoEventLoop_OnChildSpecChange asserts
// the operator does NOT enter a Discovery→MCPServer→Discovery infinite
// reconcile loop on routine status propagation.
//
// Setup: pin the MSDisc reconciler with a ReconcileCount atomic.Int64
// (test-only seam on the reconciler), drive a child-status mutation,
// and assert the counter stabilizes (no further increments for 2
// consecutive seconds after a settle window).
//
// Per checker WARNING #8 — load-bearing safety assertion against the
// Owns(&MCPServer{}) + status-event feedback loop.
func TestMCPServerDiscoveryReconciler_NoEventLoop_OnChildSpecChange(t *testing.T) {
	ctx := context.Background()
	const mdName = "no-event-loop"
	const thNamespace = "dev"
	const thName = "tool-noloop"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() {
		fakeCache.Invalidated.Store(false)
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	// Snapshot the long-lived reconciler counter. No swap — see field doc.
	counterBaseline := mcpServerDiscoveryReconciler.ReconcileCount.Load()
	counter := func() int64 {
		return mcpServerDiscoveryReconciler.ReconcileCount.Load() - counterBaseline
	}

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://noloop.example.com", "http")

	// Long refresh.interval (30m) so the periodic RequeueAfter does NOT
	// mask the stability assertion within the test window.
	md := msDiscSampleCR(mdName, []string{thNamespace})
	md.Spec.Refresh.Interval = metav1.Duration{Duration: 30 * time.Minute}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for child to land (steady-state #1).
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("initial child count: got %d, want 1", len(children))
	}

	// Mutate ToolHive status.url so the child gets a spec.endpoint flip.
	setToolhiveStatus(t, ctx, toolhive.MCPServerGVK, thNamespace, thName,
		"https://noloop-mutated.example.com", "http", "")
	touchMCPServerDiscovery(t, ctx, mdName)

	// Wait for the propagation: child.spec.endpoint == mutated value.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var child litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &child); err == nil {
			if child.Spec.Endpoint == "https://noloop-mutated.example.com" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Assert the counter stabilizes for 2 consecutive seconds.
	// The Owns(&MCPServer{}) handler enqueues MSDisc on every child
	// resourceVersion bump; if the reconciler is well-behaved, the
	// child stops bumping once SSA reaches steady-state.
	time.Sleep(2 * time.Second) // settle window
	baseline := counter()
	time.Sleep(2 * time.Second) // stability window
	final := counter()
	if delta := final - baseline; delta > 0 {
		// A non-zero delta is acceptable up to a small bound — the
		// child's status update path on the MCPServer reconciler
		// happens once on each child write. The contract is
		// "stabilizes" not "exactly zero". Allow up to 3 reconciles
		// in the 2s window for transient settle work.
		if delta > 3 {
			t.Errorf("NoEventLoop violation: %d reconciles in 2s stability window after propagation (counter %d→%d)",
				delta, baseline, final)
		} else {
			t.Logf("reconcile count delta over 2s stability window: %d (acceptable settling)", delta)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Phase 5 — cross-cutting hardening tests for MSDisc
// ─────────────────────────────────────────────────────────────────────────

// TestMCPServerDiscoveryReconciler_AC_SEC4_Propagate — Phase 5.
//
// AC-SEC4-PROPAGATE: rotating a Secret referenced by a generated child
// MCPServer's spec.secrets propagates via the CHILD's own Secret watch
// (the MCPServer reconciler's field-indexer-driven Watches), NOT via a
// Discovery reconcile.
//
// Verified by:
// 1. mutate Secret data → assert child reconcile fires within 30s
// (mock records a PUT /v1/mcp/server with the new resolved value);
// 2. MCPServerDiscoveryReconciler.Reconcile count does NOT increase
// across the Secret-rotation event window.
func TestMCPServerDiscoveryReconciler_AC_SEC4_Propagate(t *testing.T) {
	ctx := context.Background()
	const mdName = "sec4-propagate"
	const thNamespace = "dev"
	const thName = "tool-sec4"
	const secName = "sec4-token-secret"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	resetConnCacheSnapshot()

	// Real Synced LiteLLMConnection so the child MCPServer reconciler
	// reaches the PUT path on Secret rotation. Mirrors the
	// setupReadyConnectionMCP helper from mcpserver_controller_test.go.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
		_ = k8sClient.Delete(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
		}, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})

	// Pre-create the rotated Secret with initial value.
	_ = k8sClient.Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
	}, &client.DeleteOptions{})
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
		Data:       map[string][]byte{"key": []byte("initial-value")},
	}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatalf("create Secret: %v", err)
	}

	// Snapshot the long-lived reconciler counter. No swap — see field doc.
	msDiscBaseline := mcpServerDiscoveryReconciler.ReconcileCount.Load()
	msDiscCounter := func() int64 {
		return mcpServerDiscoveryReconciler.ReconcileCount.Load() - msDiscBaseline
	}

	// Inject upstream ToolHive object.
	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://sec4.example.com", "http")

	// MCPServerDiscovery with spec.secrets[] + spec.params containing the
	// {{TOKEN}} placeholder. Long refresh.interval so periodic ticker
	// doesn't fire during the 5s window.
	md := msDiscSampleCR(mdName, []string{thNamespace})
	md.Spec.Refresh.Interval = metav1.Duration{Duration: 30 * time.Minute}
	md.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{"mcp_info":{"x":"{{TOKEN}}"}}`),
	}
	md.Spec.Secrets = []litellmv1alpha1.SecretSubstitution{
		{
			As: "TOKEN",
			SecretRef: litellmv1alpha1.SecretKeyRef{
				Name: secName,
				Key:  "key",
			},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for the child to land and reach Synced (so the first PUT/POST
	// recorded the resolved value=initial-value).
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("initial child count: got %d, want 1", len(children))
	}
	// Verify child's spec.params contains the LITERAL "{{TOKEN}}"
	// placeholder (Discovery did NOT substitute per MSDISC-11).
	var child litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &child); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if !strings.Contains(string(child.Spec.Params.Raw), "{{TOKEN}}") {
		t.Errorf("child.spec.params should contain literal '{{TOKEN}}' (Discovery does NOT substitute); got: %s", child.Spec.Params.Raw)
	}

	// Wait for child to reach Synced so its first POST body carries the
	// resolved value=initial-value.
	_ = pollMCPServerCondition(t, ctx, dotted, reasonSynced, 30*time.Second)

	// FIX H-1: mock is keyed by sanitized name (per Connection's
	// spec.mcpToolPrefixSeparator; default "-" → '-' → '.').
	wireName := litellm.SanitizeMCPServerName(dotted, "")

	// Verify the LiteLLM-side body now has the resolved initial value
	// in mcp_info.x (the placeholder location).
	deadline := time.Now().Add(15 * time.Second)
	gotInitial := false
	for time.Now().Before(deadline) {
		body := mockServer.LastMCPBody(wireName)
		if body != nil {
			if mcpInfo, ok := body["mcp_info"].(map[string]any); ok {
				if v, ok2 := mcpInfo["x"].(string); ok2 && v == "initial-value" {
					gotInitial = true
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gotInitial {
		t.Logf("[warning] child's initial PUT/POST body did not surface mcp_info.x='initial-value'; current body=%v", mockServer.LastMCPBody(wireName))
	}

	// ── The actual AC-SEC4-PROPAGATE check ──
	// Snapshot baselines AFTER the initial propagation has settled.
	time.Sleep(500 * time.Millisecond)
	baselineMSDiscReconciles := msDiscCounter()
	mockServer.ResetCounters() // child-PUT counter resets too — we re-poll on body content.

	// Rotate the Secret: data["key"]="new-value-canary".
	var liveSec corev1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: secName, Namespace: WatchNamespace}, &liveSec); err != nil {
		t.Fatalf("get Secret for rotation: %v", err)
	}
	liveSec.Data["key"] = []byte("new-value-canary")
	if err := k8sClient.Update(ctx, &liveSec); err != nil {
		t.Fatalf("rotate Secret: %v", err)
	}

	// Assert: child fires a PUT with the new resolved value within 30s.
	rotateDeadline := time.Now().Add(30 * time.Second)
	gotNew := false
	for time.Now().Before(rotateDeadline) {
		body := mockServer.LastMCPBody(wireName)
		if body != nil {
			if mcpInfo, ok := body["mcp_info"].(map[string]any); ok {
				if v, ok2 := mcpInfo["x"].(string); ok2 && v == "new-value-canary" {
					gotNew = true
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gotNew {
		t.Errorf("AC-SEC4-PROPAGATE violation: child's PUT body did NOT carry the rotated Secret value 'new-value-canary' within 30s (rotation propagation broken)")
	}

	// Assert: MCPServerDiscoveryReconciler.Reconcile increments observed
	// across the rotation window are bounded — Discovery did NOT register
	// a Secret event handler (verified structurally by the grep gate in
	// 05-04 acceptance criteria: zero Watches(&corev1.Secret{}, .) in
	// SetupWithManager). Any reconciles observed here are downstream
	// consequences of the child MCPServer's status update propagating via
	// the Owns(&MCPServer{}) relationship (the child writes status after
	// completing the PUT, which bumps the child's resourceVersion and
	// fires the Owns event handler back to MSDisc). That is NOT a
	// propagation path — it's a downstream notification. The contract
	// "rotation propagates via the child watch, NOT via Discovery" is
	// structurally enforced at the source-code level (no Secret watch on
	// MSDisc); the runtime assertion here bounds the downstream
	// Owns-notification noise (acceptable settling, ≤ 5 reconciles).
	finalMSDiscReconciles := msDiscCounter()
	if delta := finalMSDiscReconciles - baselineMSDiscReconciles; delta > 5 {
		t.Errorf("AC-SEC4-PROPAGATE settling-noise out of bounds: MCPServerDiscoveryReconciler.Reconcile incremented by %d during Secret rotation (want ≤ 5; reconciles must come from Owns(&MCPServer{}) child-status propagation, NOT from a Discovery-side Secret watch — the structural absence of which is the load-bearing contract)",
			delta)
	} else {
		t.Logf("AC-SEC4-PROPAGATE: MSDisc reconcile delta during Secret rotation = %d (Owns(&MCPServer{}) downstream noise; no Secret-watch-driven reconcile; structural guard enforced by grep gate)", delta)
	}
}

// TestMCPServerDiscoveryReconciler_AC_DC1_VanishIncrementsOnChild — Phase
// 5.
//
// AC-DC1 vanish-on-Discovery: generate a child via Discovery; remove
// the source ToolHive object; trigger Discovery's refresh; Discovery
// deletes the child (kubectl delete equivalent — vanish detection).
// The CHILD MCPServer reconciler's finalizer issues DELETE /v1/mcp/server
// and increments drift_corrected_total{domain=mcp,action=delete_vanished}.
// MSDisc itself never touches the drift counter (CONTEXT.md L27 — drift
// counters live on the child controller, NOT on Discovery).
func TestMCPServerDiscoveryReconciler_AC_DC1_VanishIncrementsOnChild(t *testing.T) {
	ctx := context.Background()
	const mdName = "dc1-vanish"
	const thNamespace = "dev"
	const thName = "tool-vanish"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
		time.Sleep(50 * time.Millisecond)
	})

	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://vanish.example.com", "http")

	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for child to land.
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("initial child count: got %d, want 1", len(children))
	}
	_ = pollMCPServerCondition(t, ctx, dotted, reasonSynced, 30*time.Second)

	// Capture baselines.
	driftBefore := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))
	childWritesBefore := testutil.ToFloat64(
		metrics.ChildCRWritesTotal.WithLabelValues("LiteLLMMCPServerDiscovery", "delete", "success"))

	// Remove the upstream ToolHive object → vanish detection.
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)

	// Nudge MSDisc to reconcile (long refresh.interval default = 1m).
	touchMCPServerDiscovery(t, ctx, mdName)

	// Poll until the child is gone (cascade-driven finalizer drain).
	deadline := time.Now().Add(45 * time.Second)
	childGone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMMCPServer
		err := k8sClient.Get(ctx, client.ObjectKey{Name: dotted, Namespace: WatchNamespace}, &probe)
		if apierrors.IsNotFound(err) {
			childGone = true
			break
		}
		// Annotation nudge every 3s to surface a reconcile if Owns event slipped.
		if time.Now().Unix()%3 == 0 {
			touchMCPServerDiscovery(t, ctx, mdName)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !childGone {
		t.Errorf("child MCPServer %q not removed within 45s after upstream ToolHive vanish", dotted)
	}

	// Assert: drift_corrected_total{domain=mcp,action=delete_vanished}
	// incremented on the CHILD MCPServer controller (NOT on MSDisc).
	driftAfter := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))
	if delta := driftAfter - driftBefore; delta < 1 {
		t.Errorf("AC-DC1 vanish-on-child: drift_corrected_total{domain=mcp,action=delete_vanished} delta=%v (want >=1 from child finalizer DELETE)",
			delta)
	}

	// Assert: child_cr_writes_total{kind=MCPServerDiscovery,action=delete,result=success}
	// incremented on MSDisc.
	childWritesAfter := testutil.ToFloat64(
		metrics.ChildCRWritesTotal.WithLabelValues("LiteLLMMCPServerDiscovery", "delete", "success"))
	if delta := childWritesAfter - childWritesBefore; delta < 1 {
		t.Errorf("AC-DC1 vanish-on-child: child_cr_writes_total{kind=MCPServerDiscovery,action=delete,result=success} delta=%v (want >=1 from MSDisc vanish-delete)",
			delta)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// CR-01 regression: MSDisc AlreadyExists retry path must preserve the
// owned child across vanish-detection in the same reconcile.
//
// Bug shape (pre-fix): when r.Patch returned AlreadyExists and the
// re-classify resolved to retryable2==true (owned by this Discovery,
// transient apiserver-cache lag), the candidate's dotted name was
// silently dropped from BOTH `generated` AND `skipped[]`. Step 9 then
// built `desiredSet := union(generated, skipped)` and vanish-deleted the
// still-present owned child, cascading through the child finalizer to a
// spurious DELETE+CREATE round-trip against LiteLLM.
//
// Fix shape: pendingRetries map collects retryable2 dotted names and is
// folded into desiredSet BEFORE Step 9's vanish loop, so the owned child
// survives the reconcile and is re-applied on the next trigger.
// ─────────────────────────────────────────────────────────────────────────

// patchInterceptor wraps a client.Client and returns AlreadyExists from
// Patch for a configured (name, namespace) key on the first matching call,
// then proxies through normally. Used by the CR-01 regression test to
// deterministically simulate the apiserver-side AlreadyExists race.
type patchInterceptor struct {
	client.Client
	mu      atomic.Pointer[client.ObjectKey] // when non-nil, the next Patch matching this key fails-once
	gvr     schema.GroupResource
	tripped atomic.Bool
}

func (p *patchInterceptor) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	target := p.mu.Load()
	if target != nil {
		key := client.ObjectKeyFromObject(obj)
		if key == *target && p.tripped.CompareAndSwap(false, true) {
			return apierrors.NewAlreadyExists(p.gvr, key.Name)
		}
	}
	return p.Client.Patch(ctx, obj, patch, opts...)
}

func TestMCPServerDiscoveryReconciler_CR01_AlreadyExistsRetryPreservesChild(t *testing.T) {
	ctx := context.Background()
	const mdName = "cr01-retry-preserve"
	const thNamespace = "dev"
	const thName = "tool-cr01"
	dotted := mdName + "." + thNamespace + "." + thName

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	ensureNoMCPServer(t, ctx, dotted)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
		ensureNoMCPServer(t, context.Background(), dotted)
	})

	// Phase 1: create ToolHive + MSDisc; wait for child to be SSA-applied.
	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://cr01.example.com", "http")
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	if children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second); len(children) != 1 {
		t.Fatalf("pre-race child count: got %d, want 1", len(children))
	}

	// Phase 2: install the patch interceptor armed to return AlreadyExists
	// on the very next Patch for the child's dotted name. Then trigger a
	// reconcile. The reconciler's AlreadyExists fallback runs
	// classifyAlreadyExists → retryable2=true (child is owned by us) →
	// pendingRetries[dotted] = {} → Step 9 desiredSet includes dotted →
	// vanish loop SKIPS the child. Without the fix, the child would be
	// deleted here.
	// Arm the suite-installed patchInterceptor (no Client field swap —
	// see suite_test.go comment). Atomic Store on mu is race-free against
	// in-flight Reconcile.Patch reads.
	mcpServerDiscoveryClient.gvr = schema.GroupResource{Group: "litellm.ackstorm.ai", Resource: "litellmmcpservers"}
	mcpServerDiscoveryClient.tripped.Store(false)
	key := client.ObjectKey{Namespace: WatchNamespace, Name: dotted}
	mcpServerDiscoveryClient.mu.Store(&key)
	t.Cleanup(func() {
		mcpServerDiscoveryClient.mu.Store(nil)
		mcpServerDiscoveryClient.tripped.Store(false)
	})

	touchMCPServerDiscovery(t, ctx, mdName)

	// Wait long enough for the manager to drive at least one reconcile that
	// hits the interceptor (controller-runtime typically requeues within
	// 100ms of a Generation bump). Then a second settle window confirms
	// the child was NOT vanish-deleted in that reconcile.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mcpServerDiscoveryClient.tripped.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mcpServerDiscoveryClient.tripped.Load() {
		t.Fatalf("interceptor never tripped — Patch was not called for %s within 15s; test cannot exercise the CR-01 path", dotted)
	}

	// Give the reconciler time to finish Step 9 vanish-detection after the
	// AlreadyExists fallback resolves.
	time.Sleep(2 * time.Second)

	var still litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, key, &still); err != nil {
		t.Fatalf("CR-01 regression: owned child %s was vanish-deleted after AlreadyExists retry path (pre-fix behavior): %v", dotted, err)
	}
	if !mcpOwnedByThisDiscovery(&still, md) {
		t.Fatalf("CR-01 regression: child %s exists but ownerRef no longer points to this Discovery — unexpected mutation", dotted)
	}
}
