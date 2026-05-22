// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/providers"
)

// TestBuildChildModel_EmptyParams locks the empty-safe overlay contract
// for `buildChildModel`. A Discovery with `spec.params` absent
// (Raw == nil) must produce a child whose `spec.params` is the typed-field
// overlay map only (`{"model": "<litellm-provider>/<rawID>"}`), with no
// empty-object oddity and no decode error.
//
// Task 2 acceptance criterion: "TestBuildChildModel_EmptyParams"
// is a WHITE-BOX call to buildChildModel — does NOT require envtest.
func TestBuildChildModel_EmptyParams(t *testing.T) {
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic-md",
			Namespace: "default",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type: "anthropic",
			// Params intentionally omitted — Raw will be nil.
		},
	}

	child, err := buildChildModel(md, "anthropic.claude-3-5-sonnet-20241022",
		"claude-3-5-sonnet-20241022", "anthropic", "default")
	if err != nil {
		t.Fatalf("buildChildModel(empty params): unexpected error: %v", err)
	}

	if got := string(child.Spec.Params.Raw); got == "" {
		t.Fatalf("child.Spec.Params.Raw is empty — buildChildModel should produce {\"model\":\"...\"} not empty")
	}

	var decoded map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &decoded); err != nil {
		t.Fatalf("decode child.Spec.Params: %v (raw=%s)", err, string(child.Spec.Params.Raw))
	}
	if got, want := decoded["model"], "anthropic/claude-3-5-sonnet-20241022"; got != want {
		t.Errorf("child.Spec.Params.model: got %v, want %s", got, want)
	}
	// No stray empty-bag artifacts: exactly one key (`model`) on the empty-params path.
	if got, want := len(decoded), 1; got != want {
		t.Errorf("child.Spec.Params: got %d keys, want %d (no empty-object oddity)", got, want)
	}

	// MDISC-24 essentials.
	if child.OwnerReferences[0].UID != md.UID {
		t.Errorf("ownerReferences[0].UID: got %s, want %s", child.OwnerReferences[0].UID, md.UID)
	}
	if child.OwnerReferences[0].Controller == nil || !*child.OwnerReferences[0].Controller {
		t.Error("ownerReferences[0].Controller must be true")
	}
	if child.OwnerReferences[0].BlockOwnerDeletion == nil || !*child.OwnerReferences[0].BlockOwnerDeletion {
		t.Error("ownerReferences[0].BlockOwnerDeletion must be true")
	}
	if got, want := child.Labels[generatedByLabel], "anthropic-md"; got != want {
		t.Errorf("labels[%s]: got %q, want %q", generatedByLabel, got, want)
	}
	if !controllerutil.ContainsFinalizer(child, modelFinalizer) {
		t.Errorf("child.Finalizers must contain %s", modelFinalizer)
	}
}

// TestBuildChildModel_KubeAIAPIBaseOverlay asserts that kubeai Discovery
// children carry spec.params.api_base = discovery.spec.baseUrl, parallel
// to the bedrock spec.region → aws_region_name overlay. Regression for
// FIX.txt HIGH-2 (KubeAI children registered but had no api_base →
// inference requests failed at LiteLLM with no route for
// hosted_vllm/<id>).
func TestBuildChildModel_KubeAIAPIBaseOverlay(t *testing.T) {
	const baseURL = "http://kubeai.kubeai.svc/openai/v1"
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeai-md", Namespace: "default", UID: "abcd"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "kubeai",
			BaseURL: baseURL,
			Params:  k8sruntime.RawExtension{Raw: []byte(`{"rpm":25,"timeout":300}`)},
		},
	}
	child, err := buildChildModel(md, "kubeai-md-qwen3-4b", "qwen3-4b", "hosted_vllm", "default")
	if err != nil {
		t.Fatalf("buildChildModel: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := decoded["model"], "hosted_vllm/qwen3-4b"; got != want {
		t.Errorf("model overlay: got %v, want %s", got, want)
	}
	if got, want := decoded["api_base"], baseURL; got != want {
		t.Errorf("api_base overlay: got %v, want %s", got, want)
	}
	if got, want := decoded["rpm"], float64(25); got != want {
		t.Errorf("verbatim params.rpm: got %v, want %v", got, want)
	}
	if got, want := decoded["timeout"], float64(300); got != want {
		t.Errorf("verbatim params.timeout: got %v, want %v", got, want)
	}
}

// TestBuildChildModel_KubeAIUserAPIBaseWins asserts that a user-supplied
// discovery.spec.params.api_base takes precedence over the auto-overlay.
// Diverges from bedrock's overlay-wins precedence on purpose — kubeai's
// api_base is a routing endpoint the user may legitimately override
// (e.g. test pointing at a sidecar) while preserving the typed Discovery
// pattern, whereas bedrock's region is identity-bearing for the AWS API.
func TestBuildChildModel_KubeAIUserAPIBaseWins(t *testing.T) {
	const baseURL = "http://kubeai.kubeai.svc/openai/v1"
	const userOverride = "http://user-override.example/v1"
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeai-md", Namespace: "default", UID: "abcd"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "kubeai",
			BaseURL: baseURL,
			Params:  k8sruntime.RawExtension{Raw: []byte(`{"api_base":"` + userOverride + `"}`)},
		},
	}
	child, err := buildChildModel(md, "kubeai-md-qwen3-4b", "qwen3-4b", "hosted_vllm", "default")
	if err != nil {
		t.Fatalf("buildChildModel: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := decoded["api_base"], userOverride; got != want {
		t.Errorf("user-supplied api_base must win: got %v, want %s", got, want)
	}
}

// TestBuildChildModel_BedrockOverlay ensures the Bedrock-only
// `aws_region_name` typed-field overlay is merged on top of the user's
// pass-through bag (verbatim from spec.params).
func TestBuildChildModel_BedrockOverlay(t *testing.T) {
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "bedrock-md", Namespace: "default", UID: "abcd"},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:   "bedrock",
			Region: "us-east-1",
			Params: k8sruntime.RawExtension{Raw: []byte(`{"rpm":50,"timeout":30}`)},
		},
	}
	child, err := buildChildModel(md, "bedrock.anthropic-claude-3-sonnet",
		"anthropic.claude-3-sonnet-20240229-v1:0", "bedrock", "default")
	if err != nil {
		t.Fatalf("buildChildModel: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := decoded["model"], "bedrock/anthropic.claude-3-sonnet-20240229-v1:0"; got != want {
		t.Errorf("model overlay: got %v, want %s", got, want)
	}
	if got, want := decoded["aws_region_name"], "us-east-1"; got != want { //nolint:goconst // AWS region literal in fixture / overlay-roundtrip assertion; const would obscure the wire value being tested

		t.Errorf("aws_region_name overlay: got %v, want %s", got, want)
	}
	if got, want := decoded["rpm"], float64(50); got != want {
		t.Errorf("verbatim params.rpm: got %v, want %v", got, want)
	}
	if got, want := decoded["timeout"], float64(30); got != want {
		t.Errorf("verbatim params.timeout: got %v, want %v", got, want)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Envtest scenarios — NORM1 / MDISC-11 / AC-DC3 / CASCADE / AtomicRefresh.
// ═══════════════════════════════════════════════════════════════════════════

// fakeProvider is a deterministic test-only Provider implementation used
// by the 04-05 envtest scenarios. It returns whatever candidates are
// stored in `list` on each call (callers can swap `list` between
// reconciles to exercise vanish detection). If `err` is non-nil,
// List returns it verbatim (exercises the atomic-refresh D-09 path).
//
// Thread-safe via atomic pointer swaps so a test can mutate the
// returned candidate set mid-reconcile without a race detector flag.
type fakeProvider struct {
	typeLabel string
	listPtr   atomic.Pointer[[]providers.Candidate]
	errPtr    atomic.Pointer[error]
}

func newFakeProvider(typeLabel string, candidates []providers.Candidate) *fakeProvider {
	fp := &fakeProvider{typeLabel: typeLabel}
	fp.setList(candidates)
	return fp
}

func (f *fakeProvider) Type() string { return f.typeLabel }

func (f *fakeProvider) List(_ context.Context) ([]providers.Candidate, error) {
	if errPtr := f.errPtr.Load(); errPtr != nil {
		return nil, *errPtr
	}
	if listPtr := f.listPtr.Load(); listPtr != nil {
		// Copy the slice so callers can mutate without races.
		out := make([]providers.Candidate, len(*listPtr))
		copy(out, *listPtr)
		return out, nil
	}
	return nil, nil
}

func (f *fakeProvider) setList(c []providers.Candidate) {
	copied := append([]providers.Candidate(nil), c...)
	f.listPtr.Store(&copied)
	f.errPtr.Store(nil) // clear any prior error
}

func (f *fakeProvider) setError(err error) {
	f.errPtr.Store(&err)
}

// modeldiscoverySampleCR returns a minimal ModelDiscovery CR for envtest.
// Caller can override fields after construction. Default refresh interval
// is 1 minute (the MDISC-05 CEL floor); tests that need tighter cadence
// either trigger reconciles via touching the CR or use the bedrock test
// path where the cadence doesn't matter for first-reconcile assertions.
func modeldiscoverySampleCR(name, providerType string) *litellmv1alpha1.LiteLLMModelDiscovery {
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type: providerType,
			Refresh: litellmv1alpha1.ModelDiscoveryRefresh{
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	// Per-type required fields per spec §6.3 CEL matrix.
	switch providerType {
	case "anthropic", "gemini", "openai":
		md.Spec.CredentialsSecretRef = &litellmv1alpha1.SecretObjectRef{
			Name: name + "-creds",
		}
	case "bedrock":
		md.Spec.Region = "us-east-1"
		md.Spec.CredentialsSecretRef = &litellmv1alpha1.SecretObjectRef{
			Name: name + "-creds",
		}
	case "kubeai":
		md.Spec.BaseURL = "http://kubeai.test.svc/openai/v1"
	}
	return md
}

// ensureNoModelDiscovery deletes any pre-existing ModelDiscovery (and
// all its owned children) so a test starts from a clean slate. Waits
// up to 30s for cascade-delete to drain.
//
// Uses updateWithRetry on every finalizer strip so concurrent controller
// status writes (which would otherwise lose the optimistic-lock race and
// leave the finalizer in place) cannot strand the CR in cascade-drain
// for the full 30s ceiling.
func ensureNoModelDiscovery(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	// Strip the Discovery finalizer first (retry on conflict) so the
	// parent deletes even if the controller is unable to drain children.
	if err := updateWithRetry(ctx, key,
		&litellmv1alpha1.LiteLLMModelDiscovery{},
		func(obj *litellmv1alpha1.LiteLLMModelDiscovery) error {
			controllerutil.RemoveFinalizer(obj, modelDiscoveryFinalizer)
			return nil
		},
	); err == nil || apierrors.IsNotFound(err) {
		_ = k8sClient.Delete(ctx, &litellmv1alpha1.LiteLLMModelDiscovery{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		})
	}
	// Also remove any owned children directly. The cascade-delete drain
	// in the reconciler is exercised by AC-MD-CASCADE; cleanup should
	// NOT depend on that path.
	var owned litellmv1alpha1.LiteLLMModelList
	if err := k8sClient.List(ctx, &owned,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: name},
	); err == nil {
		for i := range owned.Items {
			childKey := client.ObjectKeyFromObject(&owned.Items[i])
			_ = updateWithRetry(ctx, childKey,
				&litellmv1alpha1.LiteLLMModel{},
				func(obj *litellmv1alpha1.LiteLLMModel) error {
					controllerutil.RemoveFinalizer(obj, modelFinalizer)
					return nil
				},
			)
			_ = k8sClient.Delete(ctx, &litellmv1alpha1.LiteLLMModel{
				ObjectMeta: metav1.ObjectMeta{Name: childKey.Name, Namespace: childKey.Namespace},
			})
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got litellmv1alpha1.LiteLLMModelDiscovery
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			// Parent gone — also wait for orphan children to clear.
			var rem litellmv1alpha1.LiteLLMModelList
			if err := k8sClient.List(ctx, &rem,
				client.InNamespace(WatchNamespace),
				client.MatchingLabels{generatedByLabel: name},
			); err == nil && len(rem.Items) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("warning: ModelDiscovery %q (or its children) still present after 30s cleanup", name)
}

// ensureCredentialSecret creates a synthetic provider credentials Secret
// in WatchNamespace. Idempotent — re-runs against an existing Secret are
// a no-op.
func ensureCredentialSecret(t *testing.T, ctx context.Context, name, providerType string) {
	t.Helper()
	data := map[string][]byte{}
	switch providerType {
	case "anthropic":
		data["ANTHROPIC_API_KEY"] = []byte("sk-test-anthropic")
	case "gemini":
		data["GEMINI_API_KEY"] = []byte("test-gemini")
	case "openai":
		data["OPENAI_API_KEY"] = []byte("sk-test-openai")
	case "bedrock":
		data["AWS_ACCESS_KEY_ID"] = []byte("AKIATESTCANARY12345")
		data["AWS_SECRET_ACCESS_KEY"] = []byte("test-secret")
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Data:       data,
	}
	if err := k8sClient.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create credentials Secret %q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), sec)
	})
}

// pollChildrenCount polls k8sClient.List filtered by the generated-by
// label until len(items) == want or timeout. Returns the final list
// (may have len != want on timeout — caller asserts).
func pollChildrenCount(t *testing.T, ctx context.Context, parent string, want int, timeout time.Duration) []litellmv1alpha1.LiteLLMModel {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var list litellmv1alpha1.LiteLLMModelList
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

// pollDiscoveryStatusReady polls a ModelDiscovery's Ready condition
// reason until it matches wantReason or timeout expires. Returns the
// last-observed CR.
func pollDiscoveryStatusReady(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMModelDiscovery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var md litellmv1alpha1.LiteLLMModelDiscovery
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

// TestModelDiscovery_AC_MD_NORM1_BedrockColonNormalization locks the spec
// §6.3 line 756 contract: the Bedrock raw ID
// "anthropic.claude-3-sonnet-20240229-v1:0" normalizes to
// "anthropic.claude-3-sonnet-20240229-v1-0" (the `:` between `v1` and
// `0` collapses to a `-`). The resulting child Model's name is
// "bedrock.anthropic.claude-3-sonnet-20240229-v1-0", and its
// spec.params.model preserves the RAW ID verbatim ("bedrock/anthropic.
// claude-3-sonnet-20240229-v1:0") — normalization is for the K8s name
// ONLY (MDISC-10).
//
// Uses providers.RegisterTestProvider to inject a fakeProvider into the
// registry — bypasses aws-sdk-go-v2's middleware stack (which is
// package-private to the providers package and cross-package
// inaccessible from the controller tests).
func TestModelDiscovery_AC_MD_NORM1_BedrockColonNormalization(t *testing.T) {
	ctx := context.Background()
	const mdName = "norm1-bedrock"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Inject a fake Bedrock provider that returns the spec §6.3 line 756
	// example verbatim.
	fake := newFakeProvider("bedrock", []providers.Candidate{
		{ID: "anthropic.claude-3-sonnet-20240229-v1:0", DisplayName: "Claude 3 Sonnet"},
	})
	providers.RegisterTestProvider(t, "bedrock", fake)

	// Bedrock requires credentialsSecretRef + region (spec §6.3 CEL).
	ensureCredentialSecret(t, ctx, mdName+"-creds", "bedrock")

	md := modeldiscoverySampleCR(mdName, "bedrock")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// The expected child name is the post-normalization full form.
	const wantChildName = "bedrock.anthropic.claude-3-sonnet-20240229-v1-0"

	// Poll for the child Model to appear.
	deadline := time.Now().Add(30 * time.Second)
	var child litellmv1alpha1.LiteLLMModel
	key := client.ObjectKey{Name: wantChildName, Namespace: WatchNamespace}
	found := false
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &child); err == nil {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		// Diagnostic: list current children + dump parent status
		var list litellmv1alpha1.LiteLLMModelList
		_ = k8sClient.List(ctx, &list,
			client.InNamespace(WatchNamespace),
			client.MatchingLabels{generatedByLabel: mdName},
		)
		names := make([]string, 0, len(list.Items))
		for _, c := range list.Items {
			names = append(names, c.Name)
		}
		var mdAfter litellmv1alpha1.LiteLLMModelDiscovery
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter)
		t.Fatalf("child Model %q not created within 30s; existing owned: %v; status.failed: %+v; status.skipped: %+v",
			wantChildName, names, mdAfter.Status.FailedCandidates, mdAfter.Status.SkippedCandidates)
	}

	// Verify spec.params.model preserves the RAW ID verbatim (MDISC-10).
	var params map[string]any
	if err := json.Unmarshal(child.Spec.Params.Raw, &params); err != nil {
		t.Fatalf("decode child.Spec.Params: %v", err)
	}
	const wantModel = "bedrock/anthropic.claude-3-sonnet-20240229-v1:0"
	if got := params["model"]; got != wantModel {
		t.Errorf("child.Spec.Params.model: got %v, want %q (raw ID MUST be preserved verbatim per MDISC-10)",
			got, wantModel)
	}
	if got, want := params["aws_region_name"], "us-east-1"; got != want {
		t.Errorf("child.Spec.Params.aws_region_name: got %v, want %s (Bedrock overlay per D-06)", got, want)
	}
}

// TestModelDiscovery_MDISC11_InvalidDiscoveredName locks the contract
// that a candidate whose normalized name fails DNS-1123 validation
// MUST land in status.skippedCandidates[reason=InvalidDiscoveredName]
// with message `<original-id> -> <full-name>` per spec §6.3 line 762,
// AND MUST NOT trigger a child Model CR write.
//
// Uses an injected fakeProvider that returns a pathological raw ID
// whose 5-step normalization yields an empty string (`":-:"` → all
// dashes → collapse to single dash → trim → empty). The full name
// becomes `<prefix>.` which has an empty trailing segment — invalid
// per DNS-1123.
func TestModelDiscovery_MDISC11_InvalidDiscoveredName(t *testing.T) {
	ctx := context.Background()
	const mdName = "mdisc11-anthropic"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Inject a fake provider returning one pathological raw ID whose
	// normalized form is empty (the trim wipes the only-punct input).
	// A second healthy candidate is included to assert that the skip
	// of one doesn't poison the other (defensive — the spec invariant
	// `discoveredCount == generated + skipped + failed` must hold).
	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: ":-:", DisplayName: "Pathological"},
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)

	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Poll for the status to reflect: 1 generated (the healthy
	// candidate), 1 skipped (the pathological one), 0 failed.
	deadline := time.Now().Add(30 * time.Second)
	var mdAfter litellmv1alpha1.LiteLLMModelDiscovery
	key := client.ObjectKey{Name: mdName, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &mdAfter); err == nil {
			if mdAfter.Status.GeneratedCount == 1 && len(mdAfter.Status.SkippedCandidates) == 1 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got, want := mdAfter.Status.DiscoveredCount, int32(2); got != want {
		t.Errorf("DiscoveredCount: got %d, want %d (post-filter set size)", got, want)
	}
	if got, want := mdAfter.Status.GeneratedCount, int32(1); got != want {
		t.Errorf("GeneratedCount: got %d, want %d (healthy candidate only)", got, want)
	}
	if got, want := len(mdAfter.Status.SkippedCandidates), 1; got != want {
		t.Fatalf("len(SkippedCandidates): got %d, want %d. Full status: %+v",
			got, want, mdAfter.Status)
	}
	skip := mdAfter.Status.SkippedCandidates[0]
	if skip.Reason != "InvalidDiscoveredName" {
		t.Errorf("skipped.Reason: got %q, want %q", skip.Reason, "InvalidDiscoveredName")
	}
	// spec §6.3 line 762: message is `<original-id> -> <full-name>`.
	// The pathological ID `":-:"` normalizes to empty, so the full
	// name is `anthropic.` (prefix + "." + empty). The message MUST
	// contain BOTH ":-:" and "anthropic.".
	if !strContains(skip.Message, ":-:") {
		t.Errorf("skipped.Message %q must contain the original raw ID %q (spec §6.3 line 762)",
			skip.Message, ":-:")
	}
	if !strContains(skip.Message, " -> ") {
		t.Errorf("skipped.Message %q must contain the ` -> ` separator (spec §6.3 line 762)",
			skip.Message)
	}

	// Assert NO child Model CR was created for the pathological candidate.
	var owned litellmv1alpha1.LiteLLMModelList
	if err := k8sClient.List(ctx, &owned,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: mdName},
	); err != nil {
		t.Fatalf("list owned children: %v", err)
	}
	if len(owned.Items) != 1 {
		t.Errorf("len(owned children): got %d, want 1 (the healthy candidate only)", len(owned.Items))
	}
	// Invariant: discoveredCount == generated + skipped + failed.
	got := int(mdAfter.Status.GeneratedCount) + len(mdAfter.Status.SkippedCandidates) + len(mdAfter.Status.FailedCandidates)
	if got != int(mdAfter.Status.DiscoveredCount) {
		t.Errorf("invariant violation: generated(%d) + skipped(%d) + failed(%d) = %d; DiscoveredCount = %d",
			mdAfter.Status.GeneratedCount, len(mdAfter.Status.SkippedCandidates),
			len(mdAfter.Status.FailedCandidates), got, mdAfter.Status.DiscoveredCount)
	}
}

// strContains is a tiny helper to avoid the `strings` import re-add
// (which was deleted as unused above).
func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestModelDiscovery_FIX_L5_PruneOnFilterMutation asserts that mutating
// spec.filters.exclude on a Discovery drives a prune pass within one
// reconcile (sub-second envtest latency), NOT only on the refresh-interval
// tick. Regression for FIX.txt LOW-5 (2026-05-22 prod: openai discovery
// filter mutation took ~6 min to drain 117 → 49 children even though
// status.generatedCount flipped to 49 immediately).
//
// Hypothesis check: if this test PASSES against current code, the prune
// path already runs on generation change and the prod symptom is a
// kubectl/metric-only artifact. If it FAILS, the vanish-detection block
// is gated on the refresh tick and Task 9 follow-up forces unconditional
// prune.
func TestModelDiscovery_FIX_L5_PruneOnFilterMutation(t *testing.T) {
	ctx := context.Background()
	const mdName = "fix-l5-prune"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "alpha"},
		{ID: "beta"},
		{ID: "gamma"},
		{ID: "delta"},
		{ID: "epsilon"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	// Long refresh — proves the prune is NOT refresh-driven.
	md.Spec.Refresh.Interval = metav1.Duration{Duration: 30 * time.Minute}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// All 5 candidates → 5 children.
	if initial := pollChildrenCount(t, ctx, mdName, 5, 30*time.Second); len(initial) != 5 {
		t.Fatalf("initial child count: got %d, want 5", len(initial))
	}

	// Reset connection cache so child finalizers short-circuit on delete
	// (mirrors the DC3 pattern — keeps the test deterministic at <30s).
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	// Mutate spec.filters.exclude to drop 3 of the 5 (keep alpha + beta).
	if err := updateWithRetry(ctx,
		client.ObjectKey{Name: mdName, Namespace: WatchNamespace},
		&litellmv1alpha1.LiteLLMModelDiscovery{},
		func(obj *litellmv1alpha1.LiteLLMModelDiscovery) error {
			if obj.Spec.Filters == nil {
				obj.Spec.Filters = &litellmv1alpha1.ModelDiscoveryFilters{}
			}
			obj.Spec.Filters.Exclude = []string{"gamma", "delta", "epsilon"}
			return nil
		},
	); err != nil {
		t.Fatalf("mutate filters.exclude: %v", err)
	}

	// Filter mutation must trigger sub-refresh prune. Allow generous slack
	// for envtest scheduling but well under the 30m refresh interval. If
	// this fails, prune is gated on refresh tick (FIX.txt LOW-5 root cause).
	const pruneDeadline = 30 * time.Second
	if post := pollChildrenCount(t, ctx, mdName, 2, pruneDeadline); len(post) != 2 {
		names := make([]string, 0, len(post))
		for _, c := range post {
			names = append(names, c.Name)
		}
		t.Fatalf("filter mutation did NOT drive prune within %s (refresh=30m, FIX.txt L-5): got %d children %v, want 2",
			pruneDeadline, len(post), names)
	}
}

// TestModelDiscovery_AC_DC3_VanishDetection locks the vanish-detection
// contract (MDISC-20): when a candidate disappears from the upstream
// feed between reconciles, the corresponding child Model CR is deleted
// on the next reconcile. The deleted child's own finalizer issues
// POST /model/delete (Phase 3 model_controller.go:147-217); in envtest
// the FakeConnectionCache may not be Synced, so the child's finalizer
// short-circuits at model_controller.go:205-208 ('LiteLLM unavailable
// on deletion; finalizer removed; entry MAY persist').
//
// Test flow:
// 1. fakeProvider returns 2 candidates → 2 children land.
// 2. Mutate fakeProvider to return only 1 candidate.
// 3. Trigger reconcile (touch the parent's spec.prefix indirectly via
// an annotation flip) and wait for vanish detection to fire.
// 4. Assert exactly 1 child remains and the vanished one is gone.
func TestModelDiscovery_AC_DC3_VanishDetection(t *testing.T) {
	ctx := context.Background()
	const mdName = "dc3-anthropic"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
		{ID: "claude-3-haiku-20240307", DisplayName: "Claude 3 Haiku"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Wait for both children to land.
	initial := pollChildrenCount(t, ctx, mdName, 2, 30*time.Second)
	if len(initial) != 2 {
		t.Fatalf("initial child count: got %d, want 2", len(initial))
	}

	// Snapshot the names so we can assert which one survives.
	initialNames := map[string]struct{}{}
	for _, c := range initial {
		initialNames[c.Name] = struct{}{}
	}
	const wantSurvivor = "anthropic.claude-3-5-sonnet-20241022"
	const wantVanished = "anthropic.claude-3-haiku-20240307"
	if _, ok := initialNames[wantSurvivor]; !ok {
		t.Fatalf("expected initial child %q; got %v", wantSurvivor, initialNames)
	}
	if _, ok := initialNames[wantVanished]; !ok {
		t.Fatalf("expected initial child %q; got %v", wantVanished, initialNames)
	}

	// Mutate the provider: only the survivor stays.
	fake.setList([]providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
	})

	// Trigger a reconcile by touching an annotation (changes
	// metadata.generation isn't strictly needed — metadata.resourceVersion
	// changes will requeue via the owned-watch). A spec-bag flip is safer.
	var mdLatest litellmv1alpha1.LiteLLMModelDiscovery
	if err := updateWithRetry(ctx,
		client.ObjectKey{Name: mdName, Namespace: WatchNamespace},
		&mdLatest,
		func(md *litellmv1alpha1.LiteLLMModelDiscovery) error {
			if md.Annotations == nil {
				md.Annotations = map[string]string{}
			}
			md.Annotations["test.litellm.ackstorm.ai/trigger"] = "1"
			return nil
		},
	); err != nil {
		t.Fatalf("annotate to trigger reconcile: %v", err)
	}

	// Poll for vanish: the vanished child should disappear, and the
	// child count should drop to 1. We use a tight deadline because
	// the test child has its own finalizer (modelFinalizer) — when
	// fakeCache.Ready=false, the finalizer short-circuits at
	// model_controller.go:205-208 ('LiteLLM unavailable on deletion;
	// finalizer removed; entry MAY persist'), so the K8s deletion
	// completes promptly. Reset fakeCache to ensure Ready=false (no
	// blocking on a probe loop).
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	post := pollChildrenCount(t, ctx, mdName, 1, 60*time.Second)
	if len(post) != 1 {
		names := make([]string, 0, len(post))
		for _, c := range post {
			names = append(names, c.Name)
		}
		t.Fatalf("post-vanish child count: got %d, want 1; remaining names: %v",
			len(post), names)
	}
	if post[0].Name != wantSurvivor {
		t.Errorf("post-vanish survivor: got %q, want %q", post[0].Name, wantSurvivor)
	}

	// Verify the vanished child is GONE.
	var gone litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: wantVanished, Namespace: WatchNamespace}, &gone); !apierrors.IsNotFound(err) {
		t.Errorf("vanished child %q should be NotFound; got err=%v", wantVanished, err)
	}
}

// TestModelDiscovery_AC_MD_CASCADE locks the MDISC-28 cascade-delete
// contract: deleting a ModelDiscovery CR triggers cascade-delete of
// all owned children via blockOwnerDeletion=true K8s GC. Discovery's
// own finalizer is removed AFTER all children drain; Discovery itself
// issues NO LiteLLM call.
//
// Phase 3 cascade-trigger dependency: this test relies on the child
// Model finalizer's short-circuit at model_controller.go:205-208
// ('LiteLLM unavailable on deletion → remove finalizer; entry MAY
// persist'). Capture the FakeConnectionCache state EXPLICITLY before
// invoking parent Delete so the test fails LOUDLY if that code path
// is ever removed or its branch condition mutated.
func TestModelDiscovery_AC_MD_CASCADE(t *testing.T) {
	ctx := context.Background()
	const mdName = "cascade-anthropic"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
		{ID: "claude-3-haiku-20240307", DisplayName: "Claude 3 Haiku"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Wait for both children to land.
	initial := pollChildrenCount(t, ctx, mdName, 2, 30*time.Second)
	if len(initial) != 2 {
		t.Fatalf("initial child count: got %d, want 2", len(initial))
	}

	// Phase 3 cascade-trigger dependency: this test relies on the
	// model-finalizer short-circuit at model_controller.go:205-208.
	// Capture cache state explicitly so the test fails loudly if that
	// code path is ever removed.
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })
	snap := fakeCache.Snapshot()
	if snap.Ready {
		t.Fatalf("Phase 3 cascade short-circuit precondition: FakeConnectionCache must report Ready=false; if you removed the model_controller.go:205-208 short-circuit ('LiteLLM unavailable on deletion; finalizer removed'), this test (and Phase 4 cascade-delete) must be re-architected to provide an alternative drain mechanism. Got snap.Ready=%v, reason=%q", snap.Ready, snap.Reason)
	}

	// Re-fetch with current resourceVersion (the parent's finalizer
	// was added on the AddFinalizer reconcile after Create).
	var mdLatest litellmv1alpha1.LiteLLMModelDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdLatest); err != nil {
		t.Fatalf("re-get ModelDiscovery before delete: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&mdLatest, modelDiscoveryFinalizer) {
		t.Fatalf("Discovery finalizer must be present before cascade-delete: %v", mdLatest.Finalizers)
	}

	// Trigger cascade-delete on the parent.
	if err := k8sClient.Delete(ctx, &mdLatest); err != nil {
		t.Fatalf("delete ModelDiscovery: %v", err)
	}

	// envtest divergence (04-PATTERNS.md line 612+): envtest does NOT
	// run the K8s garbage collector. ownerReferences are stored but no
	// GC propagates DeletionTimestamp from parent to children. The
	// production K8s control plane would issue Delete on every child
	// with `ownerReferences[blockOwnerDeletion=true]` when the parent
	// gets DeletionTimestamp. Here we simulate that by directly
	// Delete'ing the children — which exercises the SAME drain code
	// path on the parent's next reconcile (Step 2a-extension lists
	// owned children; when len(owned) hits 0 it removes the finalizer).
	//
	// First, verify the parent is STILL present (drain-wait in effect)
	// — the parent should NOT have disappeared yet because children
	// remain.
	var midDelete litellmv1alpha1.LiteLLMModelDiscovery
	getErr := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &midDelete)
	if apierrors.IsNotFound(getErr) {
		t.Fatalf("ModelDiscovery %q unexpectedly already gone before children were drained — drain-wait did not fire (the Discovery's finalizer should still be present blocking parent GC)", mdName)
	}
	if getErr != nil {
		t.Fatalf("get parent mid-cascade: %v", getErr)
	}
	if midDelete.DeletionTimestamp == nil {
		t.Errorf("parent DeletionTimestamp should be set after Delete; was: %v", midDelete.DeletionTimestamp)
	}
	if !controllerutil.ContainsFinalizer(&midDelete, modelDiscoveryFinalizer) {
		t.Errorf("parent finalizer should still be present (drain-wait in effect); finalizers: %v", midDelete.Finalizers)
	}

	// Simulate K8s GC: delete each owned child. The child's own
	// finalizer short-circuits on Ready=false (model_controller.go:
	// 205-208) and removes itself, allowing the child to disappear.
	var owned litellmv1alpha1.LiteLLMModelList
	if err := k8sClient.List(ctx, &owned,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: mdName},
	); err != nil {
		t.Fatalf("list owned children to simulate GC: %v", err)
	}
	for i := range owned.Items {
		c := &owned.Items[i]
		if err := k8sClient.Delete(ctx, c); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("simulate-GC delete of child %q: %v", c.Name, err)
		}
	}

	// Now poll until Discovery is gone AND all children are gone.
	deadline := time.Now().Add(60 * time.Second)
	parentGone := false
	childrenGone := false
	for time.Now().Before(deadline) {
		var mdAfter litellmv1alpha1.LiteLLMModelDiscovery
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter); apierrors.IsNotFound(err) {
			parentGone = true
		}
		var remaining litellmv1alpha1.LiteLLMModelList
		if err := k8sClient.List(ctx, &remaining,
			client.InNamespace(WatchNamespace),
			client.MatchingLabels{generatedByLabel: mdName},
		); err == nil && len(remaining.Items) == 0 {
			childrenGone = true
		}
		if parentGone && childrenGone {
			return // success — drain completed
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !parentGone {
		t.Errorf("ModelDiscovery %q still present after 60s — drain-wait did not advance after children were drained", mdName)
	}
	if !childrenGone {
		var remaining litellmv1alpha1.LiteLLMModelList
		_ = k8sClient.List(ctx, &remaining,
			client.InNamespace(WatchNamespace),
			client.MatchingLabels{generatedByLabel: mdName},
		)
		names := make([]string, 0, len(remaining.Items))
		for _, c := range remaining.Items {
			names = append(names, c.Name)
		}
		t.Errorf("%d children %v still present after 60s", len(remaining.Items), names)
	}
}

// TestModelDiscovery_AtomicRefresh_ProviderError_NoDelete locks the
// D-09 atomic-refresh contract (CONTEXT.md line 94-101): when the
// provider returns an error AFTER children have already been
// generated, existing children stay UNTOUCHED — vanish detection
// does NOT run, no Delete is issued.
//
// Test flow:
// 1. fakeProvider returns 2 candidates → 2 children land.
// 2. Snapshot the current child list.
// 3. Mutate fakeProvider to return an error on subsequent calls
// (simulates 5xx or auth failure mid-stream).
// 4. Trigger requeue by touching the parent's annotations.
// 5. Wait ≥ 10 seconds (multiple reconciles fire with errors).
// 6. Assert: BOTH children are STILL there. The D-09 gate is
// enforced — Discovery does NOT delete children just because
// the provider became unreachable.
func TestModelDiscovery_AtomicRefresh_ProviderError_NoDelete(t *testing.T) {
	ctx := context.Background()
	const mdName = "atomicrefresh-anthropic"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
		{ID: "claude-3-haiku-20240307", DisplayName: "Claude 3 Haiku"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Wait for both children to land via the happy-path.
	initial := pollChildrenCount(t, ctx, mdName, 2, 30*time.Second)
	if len(initial) != 2 {
		t.Fatalf("initial child count: got %d, want 2", len(initial))
	}
	wantNames := map[string]struct{}{}
	for _, c := range initial {
		wantNames[c.Name] = struct{}{}
	}

	// Flip the provider into error mode. Any non-nil error works —
	// we use a synthetic "503 Service Unavailable" to mimic a
	// realistic upstream failure. The reconciler's Step 6 error path
	// writes status + returns err (no enumeration/deletion).
	fake.setError(&simulatedProviderError{msg: "simulated upstream 503"})

	// Touch the parent annotation to trigger a requeue immediately
	// (don't wait for the 1-minute refresh interval).
	var mdLatest litellmv1alpha1.LiteLLMModelDiscovery
	if err := updateWithRetry(ctx,
		client.ObjectKey{Name: mdName, Namespace: WatchNamespace},
		&mdLatest,
		func(md *litellmv1alpha1.LiteLLMModelDiscovery) error {
			if md.Annotations == nil {
				md.Annotations = map[string]string{}
			}
			md.Annotations["test.litellm.ackstorm.ai/trigger"] = "1"
			return nil
		},
	); err != nil {
		t.Fatalf("annotate to trigger reconcile: %v", err)
	}

	// Wait 4 seconds during which the reconciler will retry with
	// exponential backoff and observe the provider error multiple
	// times. The D-09 gate guarantees no child is deleted.
	time.Sleep(4 * time.Second)

	// Verify ALL initial children are STILL there.
	var post litellmv1alpha1.LiteLLMModelList
	if err := k8sClient.List(ctx, &post,
		client.InNamespace(WatchNamespace),
		client.MatchingLabels{generatedByLabel: mdName},
	); err != nil {
		t.Fatalf("list owned children after provider error: %v", err)
	}
	if len(post.Items) != 2 {
		names := make([]string, 0, len(post.Items))
		for _, c := range post.Items {
			names = append(names, c.Name)
		}
		t.Errorf("D-09 violation: children should be UNTOUCHED on provider error; got %d (expected 2): %v",
			len(post.Items), names)
	}
	for _, c := range post.Items {
		if _, ok := wantNames[c.Name]; !ok {
			t.Errorf("D-09 violation: unexpected child %q after provider error (was a delete-and-recreate observed?)", c.Name)
		}
	}

	// Verify the Discovery's condition reflects the failure.
	var mdAfter litellmv1alpha1.LiteLLMModelDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &mdAfter); err != nil {
		t.Fatalf("re-get ModelDiscovery after errors: %v", err)
	}
	cond := apimeta.FindStatusCondition(mdAfter.Status.Conditions, "SourceReachable")
	if cond == nil {
		t.Errorf("SourceReachable condition should be present after provider error")
	} else if cond.Status != metav1.ConditionFalse {
		t.Errorf("SourceReachable should be ConditionFalse after provider error; got %v (reason=%q, msg=%q)",
			cond.Status, cond.Reason, cond.Message)
	}
}

// simulatedProviderError is a thin error wrapper used by the
// AtomicRefresh test to make fakeProvider return a non-nil error on
// List. The error string is just informational.
type simulatedProviderError struct {
	msg string
}

func (e *simulatedProviderError) Error() string { return e.msg }

// TestModelDiscovery_Samples_CELAccept locks the Phase 4 sample-manifest
// contract: each of the five per-provider sample
// manifests in config/samples/modeldiscovery-*.yaml MUST survive the
// CRD's CEL admission rules (api/litellm/v1alpha1/modeldiscovery_types.go
// XValidation markers). Failure here means a user copying the sample
// straight into kubectl apply would hit an admission rejection — which
// would break the DEPLOY-02 dogfood starter-roster.
//
// The test loads each sample as bytes, unmarshals via sigs.k8s.io/yaml
// (the same JSON-bridged YAML decoder kubectl uses), rewrites the name
// to avoid colliding with other tests' Discoveries, drops any provider-
// resolution paths by NOT creating the referenced Secrets (the CR
// admission CEL fires BEFORE the reconciler attempts a List), then
// k8sClient.Create — which exercises the CRD's CEL rules.
//
// Asserts: every sample is accepted (no admission error). On rejection,
// the test dumps the rejection message so the sample author can see
// which rule fired.
//
// Cleanup: each Created CR is deleted; finalizers stripped first. We
// deliberately do NOT wait for child Models to settle — the reconciler
// would try to call the upstream provider and the test would race the
// real-network failure. Setting the spec.type to a registered fake (via
// providers.RegisterTestProvider) and then immediately deleting blocks
// any reconcile from progressing past credential resolution.
func TestModelDiscovery_Samples_CELAccept(t *testing.T) {
	ctx := context.Background()

	// Locate config/samples/ — runtime.Caller gives the absolute path of
	// THIS test file; samples are at <repo>/config/samples/.
	_, thisFile, _, _ := runtime.Caller(0)
	samplesDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "samples")

	type sampleCase struct {
		file string
		// CEL would also reject ahead of credentialsSecretRef resolution,
		// but if the test happens to trigger a reconcile, the missing
		// Secret would surface as Ready=False rather than blocking
		// admission. We don't assert on that — only on CEL admission.
	}
	cases := []sampleCase{
		{file: "modeldiscovery-anthropic.yaml"},
		{file: "modeldiscovery-bedrock.yaml"},
		{file: "modeldiscovery-gemini.yaml"},
		{file: "modeldiscovery-kubeai.yaml"},
		{file: "modeldiscovery-openai.yaml"},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(samplesDir, tc.file)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read sample %q: %v", path, err)
			}

			var md litellmv1alpha1.LiteLLMModelDiscovery
			if err := yaml.Unmarshal(raw, &md); err != nil {
				t.Fatalf("unmarshal sample %q: %v", path, err)
			}

			// Rewrite to a test-isolated name + namespace so the Create
			// doesn't collide with other tests. Drop status, resourceVersion,
			// UID — Create only honors spec + metadata.{name,namespace,labels,annotations}.
			origName := md.Name
			md.Name = "sample-cel-" + sanitizeForK8sName(tc.file)
			md.Namespace = WatchNamespace
			md.ResourceVersion = ""
			md.UID = ""
			md.Status = litellmv1alpha1.ModelDiscoveryStatus{}
			md.Finalizers = nil
			md.OwnerReferences = nil

			t.Cleanup(func() {
				bg := context.Background()
				var existing litellmv1alpha1.LiteLLMModelDiscovery
				key := client.ObjectKey{Name: md.Name, Namespace: md.Namespace}
				if err := k8sClient.Get(bg, key, &existing); err == nil {
					controllerutil.RemoveFinalizer(&existing, modelDiscoveryFinalizer)
					_ = k8sClient.Update(bg, &existing)
					_ = k8sClient.Delete(bg, &existing)
				}
				// Also drain any child Models the reconciler may have
				// SSA-applied before the test deletes the parent.
				var owned litellmv1alpha1.LiteLLMModelList
				if err := k8sClient.List(bg, &owned,
					client.InNamespace(md.Namespace),
					client.MatchingLabels{generatedByLabel: md.Name},
				); err == nil {
					for i := range owned.Items {
						c := &owned.Items[i]
						controllerutil.RemoveFinalizer(c, modelFinalizer)
						_ = k8sClient.Update(bg, c)
						_ = k8sClient.Delete(bg, c)
					}
				}
			})

			// The Create call exercises CEL admission rules from the CRD.
			// If a CEL rule rejects the spec, the error message will name
			// the failing rule (e.g. "anthropic requires spec.credentials
			// SecretRef and forbids spec.region/spec.baseUrl").
			if err := k8sClient.Create(ctx, &md); err != nil {
				t.Fatalf("CEL admission rejected sample %q (origName=%q):\n  %v",
					tc.file, origName, err)
			}
		})
	}
}

// sanitizeForK8sName turns a filename into a DNS-1123 segment for use
// as a Discovery name. Example: "modeldiscovery-anthropic.yaml" →
// "modeldiscovery-anthropic". Drops ".yaml" and replaces any non-[a-z0-9-]
// with '-' (the file basenames are already DNS-friendly so this is
// defensive).
func sanitizeForK8sName(filename string) string {
	out := filename
	// Strip .yaml suffix.
	if len(out) > 5 && out[len(out)-5:] == ".yaml" {
		out = out[:len(out)-5]
	}
	// Replace dots and underscores with '-' (RFC-1123 compatibility).
	b := make([]byte, 0, len(out))
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}

// ═══════════════════════════════════════════════════════════════════════════
// Envtest scenarios — AC-CF2 / AC-CF2b / AC-CF3.
// K8s-native conflict resolution + adoption recognition.
// ═══════════════════════════════════════════════════════════════════════════

// pollSkippedReason polls a ModelDiscovery's status.skippedCandidates
// until at least one entry with the given reason is present, or timeout.
// Returns the matching SkippedCandidate (zero-value on timeout) plus
// the final ModelDiscovery CR (for diagnostics on failure).
func pollSkippedReason(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) (litellmv1alpha1.SkippedCandidate, *litellmv1alpha1.LiteLLMModelDiscovery) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var md litellmv1alpha1.LiteLLMModelDiscovery
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &md); err == nil {
			for _, s := range md.Status.SkippedCandidates {
				if s.Reason == wantReason {
					return s, &md
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return litellmv1alpha1.SkippedCandidate{}, &md
}

// TestModelDiscovery_AC_CF2_ExplicitModelExists locks spec §6.3 line 800
// (MDISC-14): a user-authored Model with the same name a Discovery would
// generate is RECOGNIZED via K8s-native conflict resolution (SSA
// AlreadyExists → Get → no controller ownerRef → ExplicitModelExists).
// The user's Model is NOT mutated and NO controller ownerRef is added
// — Discovery only OBSERVES the conflict (spec §6.3 line 799).
//
// Flow:
// 1. Pre-create user-authored Model "anthropic.claude-3-5-sonnet-20241022"
// with distinctive spec.params and NO ownerReferences.
// 2. Setup mock provider returning ONE matching candidate.
// 3. Create ModelDiscovery → reconciler hits AlreadyExists on the SSA
// Patch → Get returns the user's Model → no controller ownerRef →
// ExplicitModelExists skip recorded.
// 4. Assert: status.skippedCandidates[ExplicitModelExists, ownedBy=name],
// status.generatedCount==0, user-authored Model unchanged.
func TestModelDiscovery_AC_CF2_ExplicitModelExists(t *testing.T) {
	ctx := context.Background()
	const mdName = "cf2-anthropic"
	const collidingName = "anthropic.claude-3-5-sonnet-20241022"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Cleanup the user-authored Model on test exit. Defined first so the
	// defer-style cleanup runs before the Discovery teardown.
	t.Cleanup(func() {
		bg := context.Background()
		var stale litellmv1alpha1.LiteLLMModel
		k := client.ObjectKey{Name: collidingName, Namespace: WatchNamespace}
		if err := k8sClient.Get(bg, k, &stale); err == nil {
			controllerutil.RemoveFinalizer(&stale, modelFinalizer)
			_ = k8sClient.Update(bg, &stale)
			_ = k8sClient.Delete(bg, &stale)
		}
	})

	// Phase 3 cascade short-circuit precondition (so the user model's
	// finalizer doesn't block deletion at cleanup).
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	// Pre-create user-authored Model with distinctive spec.params:
	// { "model": "user-authored/different", "rpm": 50 }
	// Distinct from what Discovery would produce
	// ({ "model": "anthropic/claude-3-5-sonnet-20241022", . }) so the
	// test can assert non-mutation by comparing the post-reconcile bag.
	userModel := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       collidingName,
			Namespace:  WatchNamespace,
			Finalizers: []string{modelFinalizer},
			// CRITICAL: no ownerReferences, no generated-by label.
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: k8sruntime.RawExtension{Raw: []byte(`{"model":"user-authored/different","rpm":50}`)},
		},
	}
	if err := k8sClient.Create(ctx, userModel); err != nil {
		t.Fatalf("create user-authored Model %q: %v", collidingName, err)
	}

	// Setup mock provider that returns one candidate matching the
	// user-authored Model's name. The Discovery WILL try to SSA-apply
	// a child with the same name → AlreadyExists → classify path.
	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Poll for the ExplicitModelExists skip to land.
	skip, mdAfter := pollSkippedReason(t, ctx, mdName, "ExplicitModelExists", 30*time.Second)
	if skip.Reason != "ExplicitModelExists" {
		t.Fatalf("expected status.skippedCandidates[reason=ExplicitModelExists]; got %+v\nFull status: %+v",
			mdAfter.Status.SkippedCandidates, mdAfter.Status)
	}
	if skip.Name != collidingName {
		t.Errorf("skipped.Name: got %q, want %q", skip.Name, collidingName)
	}
	if skip.OwnedBy != collidingName {
		t.Errorf("skipped.OwnedBy: got %q, want %q (user-authored Model is its own owner per MDISC-14)",
			skip.OwnedBy, collidingName)
	}
	if mdAfter.Status.GeneratedCount != 0 {
		t.Errorf("GeneratedCount: got %d, want 0 (no SSA write should succeed when colliding with user Model)",
			mdAfter.Status.GeneratedCount)
	}
	if got := len(mdAfter.Status.GeneratedChildren); got != 0 {
		t.Errorf("len(GeneratedChildren): got %d, want 0", got)
	}

	// Re-read user-authored Model: spec.params MUST be unchanged.
	var userAfter litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: collidingName, Namespace: WatchNamespace}, &userAfter); err != nil {
		t.Fatalf("re-get user-authored Model: %v", err)
	}
	var paramsAfter map[string]any
	if err := json.Unmarshal(userAfter.Spec.Params.Raw, &paramsAfter); err != nil {
		t.Fatalf("decode user-authored params after reconcile: %v (raw=%s)",
			err, string(userAfter.Spec.Params.Raw))
	}
	if got, want := paramsAfter["model"], "user-authored/different"; got != want {
		t.Errorf("user-authored Model spec.params.model MUTATED by Discovery: got %v, want %v\n"+
			"Spec §6.3 line 799: 'Discovery NEVER mutates a child whose controller ownerRef points elsewhere'",
			got, want)
	}
	if got, want := paramsAfter["rpm"], float64(50); got != want {
		t.Errorf("user-authored Model spec.params.rpm MUTATED: got %v, want %v", got, want)
	}
	// MUST NOT have a controller ownerRef added.
	for i := range userAfter.OwnerReferences {
		ref := &userAfter.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			t.Errorf("user-authored Model got a controller ownerRef ADDED by Discovery: %+v\n"+
				"Spec §6.3 line 799 violation — Discovery only OBSERVES, never mutates.",
				ref)
		}
	}
	// Invariant per spec §6.3 line 875.
	got := int(mdAfter.Status.GeneratedCount) + len(mdAfter.Status.SkippedCandidates) + len(mdAfter.Status.FailedCandidates)
	if got != int(mdAfter.Status.DiscoveredCount) {
		t.Errorf("invariant violation: generated(%d) + skipped(%d) + failed(%d) = %d; DiscoveredCount = %d",
			mdAfter.Status.GeneratedCount, len(mdAfter.Status.SkippedCandidates),
			len(mdAfter.Status.FailedCandidates), got, mdAfter.Status.DiscoveredCount)
	}
}

// TestModelDiscovery_AC_CF2b_AdoptionRecognition locks spec §6.3 line 801
// (MDISC-25): a user stripping the controller ownerRef from a generated
// child is the spec-defined adoption mechanism. The very next Discovery
// reconcile MUST recognize the strip, record the adopted name in
// status.skippedCandidates[reason=ExplicitModelExists], and STOP managing
// the child (no SSA re-apply, no vanish-delete).
//
// Flow:
// 1. Setup mock + Discovery happy-path → 1 child lands.
// 2. Strip ownerReferences via Update.
// 3. Trigger Discovery reconcile (annotation flip).
// 4. Assert: status.skippedCandidates[ExplicitModelExists] for the
// adopted child, child STILL EXISTS in K8s (not vanish-deleted),
// generated-by label STILL present (user PATCH didn't touch labels).
func TestModelDiscovery_AC_CF2b_AdoptionRecognition(t *testing.T) {
	ctx := context.Background()
	const mdName = "cf2b-anthropic"
	const childName = "anthropic.claude-3-5-sonnet-20241022"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Phase 3 cascade short-circuit precondition (so the post-test
	// cleanup can drain the adopted child's finalizer).
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)
	ensureCredentialSecret(t, ctx, mdName+"-creds", "anthropic")

	md := modeldiscoverySampleCR(mdName, "anthropic")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Wait for the child to land via happy-path.
	initial := pollChildrenCount(t, ctx, mdName, 1, 30*time.Second)
	if len(initial) != 1 {
		t.Fatalf("initial child count: got %d, want 1", len(initial))
	}
	if initial[0].Name != childName {
		t.Fatalf("initial child name: got %q, want %q", initial[0].Name, childName)
	}
	if len(initial[0].OwnerReferences) == 0 {
		t.Fatalf("initial child has no ownerReferences; expected Discovery's controller ownerRef")
	}

	// Strip the ownerReferences entry (the adoption mechanism).
	// kubectl JSON-patch equivalent:
	// [{"op":"remove","path":"/metadata/ownerReferences"}]
	//
	// The Discovery reconciler may be re-reconciling concurrently (e.g.
	// updating status as we Get), so retry on optimistic-concurrency
	// conflicts up to 5 times — mirrors `kubectl apply --force`'s
	// resourceVersion-retry loop.
	adoptedKey := client.ObjectKey{Name: childName, Namespace: WatchNamespace}
	var stripErr error
	for attempt := 0; attempt < 5; attempt++ {
		var adopted litellmv1alpha1.LiteLLMModel
		if err := k8sClient.Get(ctx, adoptedKey, &adopted); err != nil {
			stripErr = err
			break
		}
		adopted.OwnerReferences = nil
		stripErr = k8sClient.Update(ctx, &adopted)
		if stripErr == nil {
			break
		}
		if !apierrors.IsConflict(stripErr) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if stripErr != nil {
		t.Fatalf("strip ownerReferences (adoption) after retries: %v", stripErr)
	}

	// Trigger a Discovery reconcile via annotation flip. Retry on
	// optimistic-concurrency conflicts (the reconciler is actively
	// updating status concurrently).
	mdKey := client.ObjectKey{Name: mdName, Namespace: WatchNamespace}
	var triggerErr error
	for attempt := 0; attempt < 5; attempt++ {
		var mdLatest litellmv1alpha1.LiteLLMModelDiscovery
		if err := k8sClient.Get(ctx, mdKey, &mdLatest); err != nil {
			triggerErr = err
			break
		}
		if mdLatest.Annotations == nil {
			mdLatest.Annotations = map[string]string{}
		}
		mdLatest.Annotations["test.litellm.ackstorm.ai/adopt-trigger"] = "1"
		triggerErr = k8sClient.Update(ctx, &mdLatest)
		if triggerErr == nil {
			break
		}
		if !apierrors.IsConflict(triggerErr) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if triggerErr != nil {
		t.Fatalf("annotate to trigger reconcile after retries: %v", triggerErr)
	}

	// Poll for the ExplicitModelExists skip on the adopted child.
	// 120s rather than 30s: GitHub-hosted CI runners are noticeably slower
	// than the devtools image on a developer workstation, and the
	// reconciler chain (annotate → reconcile → status update → cache
	// re-list) can exceed 60s end-to-end under load — observed in the
	// v0.0.3 release pipeline where the test failed at exactly the 60s
	// boundary. Doubled to 120s with margin.
	skip, mdAfter := pollSkippedReason(t, ctx, mdName, "ExplicitModelExists", 120*time.Second)
	if skip.Reason != "ExplicitModelExists" {
		t.Fatalf("expected status.skippedCandidates[reason=ExplicitModelExists] after adoption; got %+v\nFull status: %+v",
			mdAfter.Status.SkippedCandidates, mdAfter.Status)
	}
	if skip.Name != childName {
		t.Errorf("adopted-skip.Name: got %q, want %q", skip.Name, childName)
	}
	if !strContains(skip.Message, "adopted") {
		t.Errorf("adopted-skip.Message %q should mention adoption (containing \"adopted\")", skip.Message)
	}

	// Adopted child MUST STILL EXIST (not vanish-deleted).
	var stillThere litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, adoptedKey, &stillThere); err != nil {
		if apierrors.IsNotFound(err) {
			t.Fatalf("adopted child %q was VANISH-DELETED — Discovery did NOT honor adoption!\n"+
				"Spec §6.3 line 801 violation — adopted children MUST be preserved.",
				childName)
		}
		t.Fatalf("re-get adopted child: %v", err)
	}
	// Generated-by label MUST persist (user PATCH of /metadata/ownerReferences
	// does NOT touch /metadata/labels).
	if got := stillThere.Labels[generatedByLabel]; got != mdName {
		t.Errorf("adopted child labels[%s]: got %q, want %q (label MUST persist across ownerRef strip)",
			generatedByLabel, got, mdName)
	}
	// Ownership-strip must have stuck (no ownerReferences re-added by
	// Discovery — that would be the spec-violation we're testing for).
	for i := range stillThere.OwnerReferences {
		ref := &stillThere.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller &&
			ref.Kind == "LiteLLMModelDiscovery" && ref.UID == mdAfter.UID {
			t.Errorf("Discovery RE-CLAIMED ownership of adopted child %q (ownerRef added back): %+v\n"+
				"Spec §6.3 line 801 violation — Discovery MUST NOT manage adopted children.",
				childName, ref)
		}
	}
	// Generated children should NOT include the adopted one anymore.
	for _, n := range mdAfter.Status.GeneratedChildren {
		if n == childName {
			t.Errorf("status.generatedChildren still contains the adopted child %q; it must be excluded after adoption", childName)
		}
	}
}

// TestModelDiscovery_AC_CF3_DuplicateDiscovery locks spec §6.3 line 808
// (MDISC-13): two ModelDiscovery CRs that would produce the same child
// name race; the loser records status.skippedCandidates[
// reason=DuplicateDiscovery, ownedBy=<winner-info>]. The winner's
// children are intact.
//
// Flow:
// 1. ONE mock provider returns ONE candidate.
// 2. Discovery disc-a is created first; we WAIT until its child lands
// to make the order deterministic. (No need to rely on race ordering
// — disc-a is established before disc-b enters the picture.)
// 3. Discovery disc-b is created with the SAME prefix (defaulted to
// "anthropic") so it derives the same child name.
// 4. disc-b's SSA Patch returns AlreadyExists → classify → the existing
// child has a controller ownerRef pointing at disc-a (DIFFERENT UID
// from disc-b) → DuplicateDiscovery skip with ownedBy containing
// "ModelDiscovery/disc-a/<UID>".
// 5. Assert: disc-a still owns the child; disc-b records the skip.
func TestModelDiscovery_AC_CF3_DuplicateDiscovery(t *testing.T) {
	ctx := context.Background()
	const aName = "cf3-disc-a"
	const bName = "cf3-disc-b"
	const sharedChild = "anthropic.claude-3-5-sonnet-20241022"

	ensureNoModelDiscovery(t, ctx, aName)
	ensureNoModelDiscovery(t, ctx, bName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), aName) })
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), bName) })

	// Phase 3 cascade short-circuit (for child finalizer drainage on cleanup).
	fakeCache.Invalidated.Store(true)
	t.Cleanup(func() { fakeCache.Invalidated.Store(false) })

	// ONE provider returning ONE candidate. Both Discoveries' reconcilers
	// will dispatch through the same registered "anthropic" entry — the
	// SECOND constructor call replaces the FIRST, but the fake instance
	// is idempotent (stateless wrt the caller), so this works.
	fake := newFakeProvider("anthropic", []providers.Candidate{
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet"},
	})
	providers.RegisterTestProvider(t, "anthropic", fake)

	// Both Discoveries get their own Secret (same data; just per-CR refs).
	ensureCredentialSecret(t, ctx, aName+"-creds", "anthropic")
	ensureCredentialSecret(t, ctx, bName+"-creds", "anthropic")

	// Create disc-a FIRST and wait until its child lands.
	mdA := modeldiscoverySampleCR(aName, "anthropic")
	if err := k8sClient.Create(ctx, mdA); err != nil {
		t.Fatalf("create disc-a: %v", err)
	}
	initialA := pollChildrenCount(t, ctx, aName, 1, 30*time.Second)
	if len(initialA) != 1 {
		t.Fatalf("disc-a initial child count: got %d, want 1", len(initialA))
	}
	if initialA[0].Name != sharedChild {
		t.Fatalf("disc-a child name: got %q, want %q", initialA[0].Name, sharedChild)
	}
	// Record disc-a's UID — the loser's DuplicateDiscovery skip MUST
	// reference this UID as the winner's identity.
	var mdARefreshed litellmv1alpha1.LiteLLMModelDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: aName, Namespace: WatchNamespace}, &mdARefreshed); err != nil {
		t.Fatalf("get disc-a after child creation: %v", err)
	}
	discAUID := string(mdARefreshed.UID)

	// Create disc-b — its SSA will hit AlreadyExists.
	mdB := modeldiscoverySampleCR(bName, "anthropic")
	if err := k8sClient.Create(ctx, mdB); err != nil {
		t.Fatalf("create disc-b: %v", err)
	}

	// Poll for disc-b's DuplicateDiscovery skip.
	skip, mdBAfter := pollSkippedReason(t, ctx, bName, "DuplicateDiscovery", 30*time.Second)
	if skip.Reason != "DuplicateDiscovery" {
		t.Fatalf("disc-b expected status.skippedCandidates[reason=DuplicateDiscovery]; got %+v\nFull status: %+v",
			mdBAfter.Status.SkippedCandidates, mdBAfter.Status)
	}
	if skip.Name != sharedChild {
		t.Errorf("disc-b skip.Name: got %q, want %q", skip.Name, sharedChild)
	}
	// OwnedBy format is "<Kind>/<Name>/<UID>" — must include disc-a's
	// identity. Substring match for robustness.
	if !strContains(skip.OwnedBy, "LiteLLMModelDiscovery") {
		t.Errorf("disc-b skip.OwnedBy %q should contain \"LiteLLMModelDiscovery\"", skip.OwnedBy)
	}
	if !strContains(skip.OwnedBy, aName) {
		t.Errorf("disc-b skip.OwnedBy %q should contain disc-a's name %q", skip.OwnedBy, aName)
	}
	if !strContains(skip.OwnedBy, discAUID) {
		t.Errorf("disc-b skip.OwnedBy %q should contain disc-a's UID %q (the spec-mandated winner identity)",
			skip.OwnedBy, discAUID)
	}

	// disc-a's child must still be present and still owned by disc-a.
	var aChild litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: sharedChild, Namespace: WatchNamespace}, &aChild); err != nil {
		t.Fatalf("disc-a child %q vanished during disc-b reconcile: %v", sharedChild, err)
	}
	if got := aChild.Labels[generatedByLabel]; got != aName {
		t.Errorf("shared child labels[%s]: got %q, want %q (disc-a's ownership must persist)",
			generatedByLabel, got, aName)
	}
	var foundOwner bool
	for i := range aChild.OwnerReferences {
		ref := &aChild.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller &&
			ref.Kind == "LiteLLMModelDiscovery" && string(ref.UID) == discAUID {
			foundOwner = true
			break
		}
	}
	if !foundOwner {
		t.Errorf("shared child ownerReferences MUST still contain disc-a's controller ownerRef; got %+v",
			aChild.OwnerReferences)
	}

	// disc-a's status should still list the child as generated.
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: aName, Namespace: WatchNamespace}, &mdARefreshed); err != nil {
		t.Fatalf("re-get disc-a for status check: %v", err)
	}
	var aStillOwns bool
	for _, n := range mdARefreshed.Status.GeneratedChildren {
		if n == sharedChild {
			aStillOwns = true
			break
		}
	}
	if !aStillOwns {
		t.Errorf("disc-a status.generatedChildren should still contain %q; got %v",
			sharedChild, mdARefreshed.Status.GeneratedChildren)
	}

	// disc-b should NOT have any generated children.
	if mdBAfter.Status.GeneratedCount != 0 {
		t.Errorf("disc-b GeneratedCount: got %d, want 0 (loser of cross-Discovery race must not own the child)",
			mdBAfter.Status.GeneratedCount)
	}
}
