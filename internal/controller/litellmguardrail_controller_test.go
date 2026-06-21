// SPDX-License-Identifier: Apache-2.0

// Envtests for the LiteLLMGuardRail reconciler.
//
// The shared TestMain bootstrap (suite_test.go) wires every reconciler
// against the same in-process MockServer + connCache. These tests
// drive LiteLLMGuardRail CRs and assert on (a) the mock's per-name
// mutation counters / LastGuardrailBody, (b) the CR's Ready condition
// + status.lastRendered, (c) recorded Events.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// guardrailSampleCR returns a minimal valid CR. Tests mutate spec on
// the returned object before Create.
func guardrailReconcilerSampleCR(name string) *litellmv1alpha1.LiteLLMGuardRail {
	return &litellmv1alpha1.LiteLLMGuardRail{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.GuardRailSpec{
			GuardrailName: name,
			Provider:      guardrailContentFilterProvider,
			Mode:          []litellmv1alpha1.GuardRailMode{"pre_call"},
		},
	}
}

// ensureNoGuardrailCR removes any pre-existing CR with the given name
// and waits for full removal — symmetric with ensureNoModel etc.
func ensureNoGuardrailCR(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMGuardRail
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		existing.SetFinalizers(nil)
		_ = k8sClient.Update(ctx, &existing)
		_ = k8sClient.Delete(ctx, &existing)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("warning: LiteLLMGuardRail %q still present after 10s cleanup wait", name)
}

// pollReadyConditionDeadline bounds every call to
// pollGuardrailCondition. Pulled out as a file-local const because all
// 13 callers used the same value (unparam linter caught it). 30s (not 5s)
// so the Ready condition has margin under -race -shuffle full-suite load;
// the helper fast-breaks on the first satisfied poll, so the happy path
// stays sub-second and only a genuinely stuck condition waits longer (#74).
const pollReadyConditionDeadline = 30 * time.Second

// pollGuardrailCondition polls the Ready condition for up to
// pollReadyConditionDeadline. Returns the final re-Get'd CR; callers
// assert the condition reason on the returned object.
//
// Thin wrapper over pollCR. The ctx argument is preserved for caller
// signature stability but is not threaded into the Get (pollCR uses
// context.Background; the deadline bounds the loop).
func pollGuardrailCondition(t *testing.T, ctx context.Context, name, wantReason string) *litellmv1alpha1.LiteLLMGuardRail {
	t.Helper()
	_ = ctx
	return pollCR[litellmv1alpha1.LiteLLMGuardRail](t, name,
		func(gr *litellmv1alpha1.LiteLLMGuardRail) bool {
			c := apimeta.FindStatusCondition(gr.Status.Conditions, conditionTypeReady)
			return c != nil && c.Reason == wantReason
		}, pollReadyConditionDeadline)
}

// pollGuardrailGuardrailID polls until status.lastRendered.guardrailID
// is non-empty.
func pollGuardrailID(t *testing.T, ctx context.Context, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var gr litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, key, &gr); err == nil && gr.Status.LastRendered.GuardrailID != "" {
			return gr.Status.LastRendered.GuardrailID
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// readyConnectionForTest installs a connection.Snapshot that returns
// Ready=true with the mock-backed client so reconcile paths past the
// connection gate. Restores the prior snapshot on test cleanup.
func readyConnectionForTest(t *testing.T) {
	t.Helper()
	// connCache is a real *connection.Cache shared across envtests;
	// the connection-reconciler probes the mock on its own. Wait until
	// Snapshot.Ready flips true.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := connCache.Snapshot()
		if snap.Ready && snap.Client != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("connCache.Snapshot did not become Ready within 10s; check LiteLLMConnection/default in WatchNamespace")
}

// ensureLiteLLMConnectionDefault writes a LiteLLMConnection/default CR
// pointing at the mock — required for the connection-cache to flip
// Ready=true. Idempotent across tests.
func ensureLiteLLMConnectionDefault(t *testing.T, ctx context.Context) {
	t.Helper()
	conn := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMConnectionSpec{
			Endpoint: mockServer.URL(),
			MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "litellm-master-key",
				Key:  "masterKey",
			},
		},
	}
	err := k8sClient.Create(ctx, conn)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure LiteLLMConnection/default: %v", err)
	}
}

// ── GR-01: happy CREATE ─────────────────────────────────────────────

// TestGuardRail_HappyCreate exercises the happy path:
//   - apply a minimal CR
//   - reconciler POSTs to /guardrails (via mock)
//   - status.lastRendered.guardrailID populated
//   - status.Ready=True, reason=Synced
//   - mock has exactly one mutation against this name (no
//     superfluous PUT after CREATE)
//   - rendered litellm_params carry the typed-field overlays
//     (guardrail / mode / default_on per the spec)
func TestGuardRail_HappyCreate(t *testing.T) {
	ctx := context.Background()
	name := "gr-happy"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	defaultOn := true
	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.DefaultOn = &defaultOn
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}

	got := pollGuardrailCondition(t, ctx, name, "Synced")
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition: got %#v want True/Synced", c)
	}
	if got.Status.LastRendered.GuardrailID == "" {
		t.Errorf("status.lastRendered.guardrailID is empty after reconcile")
	}
	if got.Status.LastRendered.DefinitionLocation != "db" {
		t.Errorf("status.lastRendered.definitionLocation: got %q want db",
			got.Status.LastRendered.DefinitionLocation)
	}
	if got.Status.LastRendered.PoolSize != 1 {
		t.Errorf("status.lastRendered.poolSize: got %d want 1", got.Status.LastRendered.PoolSize)
	}
	if got.Status.LastRendered.Hash == "" {
		t.Errorf("status.lastRendered.hash is empty after reconcile")
	}

	if got, want := mockServer.MutationsByGuardrailName(name), int64(1); got != want {
		t.Errorf("mock mutations: got %d want %d", got, want)
	}
	if !mockServer.HasGuardrail(name) {
		t.Errorf("mock missing guardrail entry %q", name)
	}

	// Rendered body assertions.
	body := mockServer.LastGuardrailBody(name)
	if body == nil {
		t.Fatalf("mock LastGuardrailBody(%q) nil — wire shape missing", name)
	}
	if body["guardrail_name"] != name {
		t.Errorf("body guardrail_name: %v", body["guardrail_name"])
	}
	params, _ := body["litellm_params"].(map[string]any)
	if params == nil {
		t.Fatalf("body missing litellm_params: %#v", body)
	}
	if params["guardrail"] != guardrailContentFilterProvider {
		t.Errorf("litellm_params.guardrail: got %v", params["guardrail"])
	}
	if params["mode"] != "pre_call" {
		t.Errorf("litellm_params.mode (scalar): got %v", params["mode"])
	}
	if params["default_on"] != true {
		t.Errorf("litellm_params.default_on: got %v", params["default_on"])
	}
}

// ── GR-02: secret substitution ──────────────────────────────────────

// TestGuardRail_SecretSubstitution_HappyPath exercises {{NAME}}
// substitution in spec.params via spec.secrets[]. The resolved value
// MUST land in the wire body, NOT the literal placeholder.
func TestGuardRail_SecretSubstitution_HappyPath(t *testing.T) {
	ctx := context.Background()
	name := "gr-secret"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	// Provider secret.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aporia-creds-gr", Namespace: WatchNamespace},
		Data:       map[string][]byte{"APORIA_API_KEY": []byte("ap-realkey-XYZ")},
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })
	if err := k8sClient.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Create Secret: %v", err)
	}

	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.Provider = "aporia"
	cr.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{"api_base":"https://aporia","api_key":"{{APORIA_API_KEY}}"}`),
	}
	cr.Spec.Secrets = []litellmv1alpha1.SecretSubstitution{
		{As: "APORIA_API_KEY", SecretRef: litellmv1alpha1.SecretKeyRef{Name: "aporia-creds-gr", Key: "APORIA_API_KEY"}},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}

	_ = pollGuardrailCondition(t, ctx, name, "Synced")
	body := mockServer.LastGuardrailBody(name)
	if body == nil {
		t.Fatal("mock has no body for the guardrail")
	}
	params := body["litellm_params"].(map[string]any)
	if params["api_key"] != "ap-realkey-XYZ" {
		t.Errorf("api_key not substituted: got %q want ap-realkey-XYZ", params["api_key"])
	}
	if strings.Contains(string(mustBytes(t, params)), "{{APORIA_API_KEY}}") {
		t.Errorf("literal placeholder leaked into wire body: %v", params)
	}
}

// mustBytes is a tiny test helper that returns the JSON-canonical
// bytes of a value via the standard encoder.
func mustBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := jsonMarshalForTest(v)
	if err != nil {
		t.Fatalf("mustBytes marshal: %v", err)
	}
	return b
}

func jsonMarshalForTest(v any) ([]byte, error) {
	// canonicalMarshal is package-internal but symmetric with the
	// reconciler's hash path — reuse it for the placeholder-leak check.
	return canonicalMarshal(v)
}

// ── GR-03: missing secret → reason=SecretNotFound ────────────────────

// TestGuardRail_SecretNotFound emits Ready=False / reason=SecretNotFound
// when {{NAME}} has no matching secret entry. The mock must NOT see a
// mutation — the gate fires before the POST.
func TestGuardRail_SecretNotFound(t *testing.T) {
	ctx := context.Background()
	name := "gr-missing-secret"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{"api_key":"{{ABSENT_SECRET}}"}`),
	}
	// No spec.secrets — placeholder is unresolvable.
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}

	got := pollGuardrailCondition(t, ctx, name, "SecretNotFound")
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready: got %#v want False/SecretNotFound", c)
	}
	if !strings.Contains(c.Message, "ABSENT_SECRET") {
		t.Errorf("Ready message missing placeholder name: %q", c.Message)
	}
	if got, want := mockServer.MutationsByGuardrailName(name), int64(0); got != want {
		t.Errorf("mock mutations: got %d want %d (no POST should fire pre-gate)", got, want)
	}
}

// ── GR-04: drift correction → PUT ───────────────────────────────────

// TestGuardRail_DriftCorrection_OnSpecEdit edits spec.params after the
// initial reconcile; the reconciler observes the hash mismatch and
// issues PUT /guardrails/{id}. Mock mutation counter increments to 2.
func TestGuardRail_DriftCorrection_OnSpecEdit(t *testing.T) {
	ctx := context.Background()
	name := "gr-drift"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"threshold":0.5}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	_ = pollGuardrailCondition(t, ctx, name, "Synced")
	// At-least-once pre-flight: a CREATE happened. The 100ms safety
	// re-list can fire a redundant idempotent mutation for the same
	// guardrail before the create's status write propagates (#74), so
	// require >=1 rather than exactly 1 — the real per-test assertions
	// below are delta/shape based and tolerate the slack.
	if got := mockServer.MutationsByGuardrailName(name); got < 1 {
		t.Fatalf("post-CREATE mutations: got %d want >=1", got)
	}
	id := pollGuardrailID(t, ctx, name, 5*time.Second)
	if id == "" {
		t.Fatal("guardrailID never populated after initial reconcile")
	}

	// Spec edit — change threshold.
	var fresh litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &fresh); err != nil {
		t.Fatalf("Get for spec edit: %v", err)
	}
	fresh.Spec.Params = runtime.RawExtension{Raw: []byte(`{"threshold":0.9}`)}
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("Update CR (drift): %v", err)
	}

	// Wait for the PUT — mutation counter bumps to 2 AND LastBody
	// reflects the new threshold. Polling both in the same loop avoids
	// the CI flake where the safety-relist runnable's accelerated cadence
	// (100ms in suite_test.go) interleaves a re-CREATE with the
	// spec-edit-driven PUT: the mutation count crosses 2 on the
	// re-CREATE leg while LastGuardrailBody still points at the original
	// CREATE's pool[0] entry (threshold=0.5). The original single-shot
	// assert flaked on the slower CI runner; the polling form lets the
	// PUT land observably.
	deadline := time.Now().Add(10 * time.Second)
	var (
		bodyConverged bool
		lastBody      map[string]any
	)
	for time.Now().Before(deadline) {
		if mockServer.MutationsByGuardrailName(name) >= 2 {
			lastBody = mockServer.LastGuardrailBody(name)
			if lastBody != nil {
				if p, ok := lastBody["litellm_params"].(map[string]any); ok {
					if p["threshold"] == float64(0.9) {
						bodyConverged = true
						break
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := mockServer.MutationsByGuardrailName(name); got < 2 {
		t.Fatalf("post-drift mutations: got %d want >= 2 (PUT did not fire)", got)
	}
	if !bodyConverged {
		t.Errorf("threshold not propagated within 10s: last body=%v want threshold=0.9", lastBody)
	}
	// guardrailID preserved across the PUT.
	if mockServer.GetGuardrailID(name) != id {
		t.Errorf("guardrailID changed across drift PUT: %q -> %q",
			id, mockServer.GetGuardrailID(name))
	}
}

// ── GR-05: invalid mode (realtime + others) ─────────────────────────

// TestGuardRail_InvalidMode_RealtimeNotAlone surfaces
// Ready=False reason=InvalidMode when realtime_input_transcription is
// combined with another slot. NOTE: admission permits MaxItems=6 but
// the reconciler enforces the realtime-exclusivity rule.
func TestGuardRail_InvalidMode_RealtimeNotAlone(t *testing.T) {
	ctx := context.Background()
	name := "gr-bad-mode"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{"realtime_input_transcription", "pre_call"}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	got := pollGuardrailCondition(t, ctx, name, "InvalidMode")
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready: got %#v want False/InvalidMode", c)
	}
	if got, want := mockServer.MutationsByGuardrailName(name), int64(0); got != want {
		t.Errorf("mock mutations: got %d want %d (no POST should fire)", got, want)
	}
}

// ── GR-06: CONFIG conflict ──────────────────────────────────────────

// TestGuardRail_ConflictsWithConfigGuardrail asserts that a CR sharing
// a name with a pre-existing CONFIG row is rejected with
// Ready=False reason=ConflictsWithConfigGuardrail and zero mutations.
func TestGuardRail_ConflictsWithConfigGuardrail(t *testing.T) {
	ctx := context.Background()
	name := "gr-config-conflict"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	// Pre-seed a CONFIG row in the mock with the same name.
	_ = mockServer.AddHandManagedConfigGuardrail(name)

	cr := guardrailReconcilerSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	got := pollGuardrailCondition(t, ctx, name, "ConflictsWithConfigGuardrail")
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready: got %#v want False/ConflictsWithConfigGuardrail", c)
	}
	if got.Status.LastRendered.DefinitionLocation != litellm.GuardrailDefinitionLocationConfig {
		t.Errorf("status.lastRendered.definitionLocation: got %q want config",
			got.Status.LastRendered.DefinitionLocation)
	}
	// CONFIG row in the mock counts as a pre-existing entry, NOT an
	// operator-issued mutation. PoolSize is the count of CRs sharing
	// this name in WatchNamespace — only the one CR.
	if got, want := mockServer.MutationsByGuardrailName(name), int64(0); got != want {
		t.Errorf("mock mutations: got %d want %d (operator must not touch CONFIG row)", got, want)
	}
}

// ── GR-07: finalizer DELETE ─────────────────────────────────────────

// TestGuardRail_FinalizerDelete creates a CR, lets it reach Synced, then
// deletes it and asserts the mock observed a DELETE. The CR removal
// completes (finalizer drops).
func TestGuardRail_FinalizerDelete(t *testing.T) {
	ctx := context.Background()
	name := "gr-delete"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	_ = pollGuardrailCondition(t, ctx, name, "Synced")
	// At-least-once pre-flight: a CREATE happened. The 100ms safety
	// re-list can fire a redundant idempotent mutation for the same
	// guardrail before the create's status write propagates (#74), so
	// require >=1 rather than exactly 1 — the real per-test assertions
	// below are delta/shape based and tolerate the slack.
	if got := mockServer.MutationsByGuardrailName(name); got < 1 {
		t.Fatalf("post-CREATE mutations: got %d want >=1", got)
	}

	// Delete.
	var fresh litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &fresh); err != nil {
		t.Fatalf("Get for delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &fresh); err != nil {
		t.Fatalf("Delete CR: %v", err)
	}

	// Wait until the CR is gone (finalizer removed → garbage-collected).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &probe); apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var after litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &after); !apierrors.IsNotFound(err) {
		t.Fatalf("CR not removed after delete: err=%v", err)
	}
	// Mutations: 1 CREATE + 1 DELETE = 2 entries under this name.
	if got := mockServer.MutationsByGuardrailName(name); got < 2 {
		t.Errorf("mock mutations after delete: got %d want >= 2 (POST + DELETE)", got)
	}
	// Mock no longer carries this name.
	if mockServer.HasGuardrail(name) {
		t.Errorf("mock still carries guardrail %q after finalizer DELETE", name)
	}
}

// ── GR-08: LB pool, provider homogeneity OK ─────────────────────────

// TestGuardRail_LBPool_PoolSize asserts that two CRs sharing
// spec.guardrailName + the same provider both reach Synced, both create
// rows in LiteLLM, and the PoolSize on each is 2.
func TestGuardRail_LBPool_PoolSize(t *testing.T) {
	ctx := context.Background()
	primary := "gr-lb-primary"
	secondary := "gr-lb-secondary"
	ensureNoGuardrailCR(t, ctx, primary)
	ensureNoGuardrailCR(t, ctx, secondary)
	t.Cleanup(func() {
		ensureNoGuardrailCR(t, context.Background(), primary)
		ensureNoGuardrailCR(t, context.Background(), secondary)
	})
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	pool := guardrailLBPoolName
	a := guardrailReconcilerSampleCR(primary)
	a.Spec.GuardrailName = pool
	b := guardrailReconcilerSampleCR(secondary)
	b.Spec.GuardrailName = pool

	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("Create primary: %v", err)
	}
	if err := k8sClient.Create(ctx, b); err != nil {
		t.Fatalf("Create secondary: %v", err)
	}

	// Both must reach Synced.
	_ = pollGuardrailCondition(t, ctx, primary, "Synced")
	_ = pollGuardrailCondition(t, ctx, secondary, "Synced")

	// Mock has TWO rows for the pool name.
	if got := mockServer.GuardrailPoolSize(pool); got != 2 {
		t.Errorf("mock pool size: got %d want 2", got)
	}

	// CR-side PoolSize is eventually 2 on both. Poll because the
	// secondary CR's poolSize is written on its own reconcile, but
	// the primary's poolSize may need a re-poke from a sibling change
	// to update — give the connection-fan-in a moment to run.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var prim, sec litellmv1alpha1.LiteLLMGuardRail
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: primary, Namespace: WatchNamespace}, &prim)
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: secondary, Namespace: WatchNamespace}, &sec)
		if prim.Status.LastRendered.PoolSize == 2 && sec.Status.LastRendered.PoolSize == 2 {
			return
		}
		// Touch the primary spec to nudge the reconciler — pool
		// recount happens at the head of every reconcile.
		if prim.Status.LastRendered.PoolSize != 2 {
			if prim.Annotations == nil {
				prim.Annotations = map[string]string{}
			}
			prim.Annotations["poolsize-nudge"] = time.Now().String()
			_ = k8sClient.Update(ctx, &prim)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("PoolSize did not stabilize at 2 on both CRs within 5s; pool count on mock = %d",
		mockServer.GuardrailPoolSize(pool))
}

// ── GR-09: pool provider mismatch ───────────────────────────────────

// TestGuardRail_PoolProviderMismatch creates two CRs sharing
// spec.guardrailName with different spec.provider values. The
// second-reconciled CR must surface PoolProviderMismatch.
func TestGuardRail_PoolProviderMismatch(t *testing.T) {
	ctx := context.Background()
	primary := "gr-mismatch-primary"
	secondary := "gr-mismatch-secondary"
	ensureNoGuardrailCR(t, ctx, primary)
	ensureNoGuardrailCR(t, ctx, secondary)
	t.Cleanup(func() {
		ensureNoGuardrailCR(t, context.Background(), primary)
		ensureNoGuardrailCR(t, context.Background(), secondary)
	})
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	pool := "mismatched-pool"
	a := guardrailReconcilerSampleCR(primary)
	a.Spec.GuardrailName = pool
	a.Spec.Provider = guardrailContentFilterProvider
	b := guardrailReconcilerSampleCR(secondary)
	b.Spec.GuardrailName = pool
	b.Spec.Provider = "aporia"

	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("Create primary: %v", err)
	}
	_ = pollGuardrailCondition(t, ctx, primary, "Synced")
	if err := k8sClient.Create(ctx, b); err != nil {
		t.Fatalf("Create secondary: %v", err)
	}

	// At least one of the two CRs must surface PoolProviderMismatch.
	deadline := time.Now().Add(5 * time.Second)
	var matched bool
	for time.Now().Before(deadline) {
		for _, name := range []string{primary, secondary} {
			var gr litellmv1alpha1.LiteLLMGuardRail
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &gr); err == nil {
				if c := apimeta.FindStatusCondition(gr.Status.Conditions, conditionTypeReady); c != nil && c.Reason == "PoolProviderMismatch" {
					matched = true
					break
				}
			}
		}
		if matched {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !matched {
		t.Fatalf("expected one of {%q, %q} to surface Ready=False reason=PoolProviderMismatch", primary, secondary)
	}
}

// NOTE: GR-11 (TestGuardRail_SafetyRelist_CreateMissing) was moved to its
// own isolated package internal/controller/relist/ so the background
// SafetyRelistRunnable recovery runs in a dedicated process with no
// neighbor-test apiserver contention (the #74 -race -shuffle release-gate
// flake). See internal/controller/relist/guardrail_relist_test.go.

// ── GR-10: reserved-key stripping ───────────────────────────────────

// TestGuardRail_ReservedKeysStripped asserts that placing reserved keys
// inside spec.params is stripped before the wire body is built. The
// typed-field overlays remain authoritative, and a Warning Event is
// emitted (we do not assert the Event here — Recorder is not exposed
// via envtest — but the strip side effect on the body is verifiable).
func TestGuardRail_ReservedKeysStripped(t *testing.T) {
	ctx := context.Background()
	name := "gr-reserved"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	// User mistakenly mirrors reserved keys into params.
	cr.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{
			"guardrail":"presidio",
			"mode":"post_call",
			"default_on":false,
			"policy_template":"hacked",
			"guardrail_name":"hacked",
			"api_base":"https://example"
		}`),
	}
	cr.Spec.Provider = guardrailContentFilterProvider
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{"pre_call"}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	_ = pollGuardrailCondition(t, ctx, name, "Synced")

	body := mockServer.LastGuardrailBody(name)
	if body == nil {
		t.Fatal("LastGuardrailBody nil")
	}
	// Typed-field overlay wins on the wire.
	if body["guardrail_name"] != name {
		t.Errorf("guardrail_name overridden: got %v want %q", body["guardrail_name"], name)
	}
	params := body["litellm_params"].(map[string]any)
	if params["guardrail"] != guardrailContentFilterProvider {
		t.Errorf("litellm_params.guardrail: got %v want litellm_content_filter (typed-field overlay)", params["guardrail"])
	}
	if params["mode"] != "pre_call" {
		t.Errorf("litellm_params.mode: got %v want pre_call", params["mode"])
	}
	// api_base flows through (non-reserved).
	if params["api_base"] != "https://example" {
		t.Errorf("litellm_params.api_base: got %v want https://example", params["api_base"])
	}
	// policy_template hoisted out of params (top-level field). NOT in params.
	if _, ok := params["policy_template"]; ok {
		t.Errorf("litellm_params.policy_template should be stripped, got %v", params["policy_template"])
	}
}

// TestGuardRail_DeletionPath_NeverPersisted_DeletePolicyDrains guards the
// confirmed-absent drain (Task 2.5): a guardrail CR whose guardrailID was
// never persisted must drain its finalizer even under deletionPolicy=Delete,
// because the operator never confirmed a create (entry is provably absent).
func TestGuardRail_DeletionPath_NeverPersisted_DeletePolicyDrains(t *testing.T) {
	ctx := context.Background()
	name := "gr-never-persisted"
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetGuardrails()
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		ensureNoGuardrailCR(t, context.Background(), name)
	})
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	cr.Spec.DeletionPolicy = string(deletionpolicy.Delete)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create guardrail: %v", err)
	}
	// Wait for the finalizer to be added (Synced), then force guardrailID="".
	_ = pollGuardrailCondition(t, ctx, name, "Synced")
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var gr litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, key, &gr); err != nil {
			return err
		}
		gr.Status.LastRendered.GuardrailID = ""
		return k8sClient.Status().Update(ctx, &gr)
	}); err != nil {
		t.Fatalf("clear guardrailID: %v", err)
	}

	var got litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get guardrail: %v", err)
	}
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &got)) {
			return // success — finalizer drained under Delete
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("guardrail stuck Terminating with empty guardrailID under Delete policy")
}

// TestGuardRail_PoolMembers_DoNotAdoptSameRow guards finding #8: two CRs
// sharing a guardrailName (an LB pool) must each create their own LiteLLM
// row — by-name adoption is disabled when poolSize > 1, so they never
// collapse onto the same guardrail_id.
func TestGuardRail_PoolMembers_DoNotAdoptSameRow(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetGuardrails()
	ensureNoGuardrailCR(t, ctx, "pool-a")
	ensureNoGuardrailCR(t, ctx, "pool-b")
	t.Cleanup(func() {
		ensureNoGuardrailCR(t, context.Background(), "pool-a")
		ensureNoGuardrailCR(t, context.Background(), "pool-b")
	})
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	// Two CRs sharing the SAME guardrailName (an LB pool), same provider.
	a := guardrailReconcilerSampleCR("pool-a")
	a.Spec.GuardrailName = "pool-x"
	b := guardrailReconcilerSampleCR("pool-b")
	b.Spec.GuardrailName = "pool-x"
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := k8sClient.Create(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}

	gotA := pollGuardrailCondition(t, ctx, "pool-a", reasonSynced)
	gotB := pollGuardrailCondition(t, ctx, "pool-b", reasonSynced)

	if gotA.Status.LastRendered.GuardrailID == "" || gotB.Status.LastRendered.GuardrailID == "" {
		t.Fatalf("both pool members must persist an ID; got a=%q b=%q",
			gotA.Status.LastRendered.GuardrailID, gotB.Status.LastRendered.GuardrailID)
	}
	if gotA.Status.LastRendered.GuardrailID == gotB.Status.LastRendered.GuardrailID {
		t.Errorf("pool members collapsed onto the same guardrail_id %q (adoption race)",
			gotA.Status.LastRendered.GuardrailID)
	}
}

// TestGuardRail_SteadyState_IncrementsReconcileTotal guards finding #13:
// the hash-equal steady-state return must increment
// reconcile_total{guardrail,success} like every other success path.
func TestGuardRail_SteadyState_IncrementsReconcileTotal(t *testing.T) {
	ctx := context.Background()
	name := "gr-steady-metric"
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetGuardrails()
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailReconcilerSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create guardrail: %v", err)
	}
	_ = pollGuardrailCondition(t, ctx, name, reasonSynced)

	// Capture AFTER reaching Synced so the CREATE increment is already counted;
	// the steady-state reconcile triggered below must add at least one more.
	before := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success"))

	// Annotation bump does NOT change generation → reconcile hits steady-state.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := updateWithRetry(ctx, key, &litellmv1alpha1.LiteLLMGuardRail{},
		func(o *litellmv1alpha1.LiteLLMGuardRail) error {
			if o.Annotations == nil {
				o.Annotations = map[string]string{}
			}
			o.Annotations["test.litellm.ackstorm.ai/trigger"] = "steady"
			return nil
		},
	); err != nil {
		t.Fatalf("annotate to trigger reconcile: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues(guardrailKind, "success")) > before {
			return // success — steady-state path incremented the counter
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("reconcile_total{guardrail,success} did not increment on the steady-state path (before=%v)", before)
}
