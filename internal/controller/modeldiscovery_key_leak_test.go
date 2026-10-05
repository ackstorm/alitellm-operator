// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/providers"
)

// H1 regression guard (Task 1.2). The end-to-end leak path is:
//
//	gemini.List transport error → listErr.Error() → writeBothConditions
//	→ status.conditions[].message (persisted to etcd) + V(1) log.
//
// Task 1.1 moved the Gemini key into the x-goog-api-key header so the
// transport error (a *url.Error) no longer echoes it. This envtest drives
// the REAL gemini provider (no fakeProvider override) against a closed
// server to force that transport error with a canary key in the
// credentials Secret, then asserts neither the Ready nor SourceReachable
// condition message carries the canary.
//
// There is no providers fuzz target, so this envtest plus the two
// providers-package unit tests (TestGemini_KeyInHeaderNotQuery,
// TestGemini_TransportError_NoKeyLeak) are the regression record for H1.
func TestModelDiscovery_GeminiListError_NoKeyLeakIntoStatus(t *testing.T) {
	const canary = "AIza-CANARY-controller-leak-FAKE"
	ctx := context.Background()
	mdName := "gemini-key-leak-guard"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Credentials Secret holding the canary key (overrides the helper's
	// non-canary default; CR refs <name>-creds per modeldiscoverySampleCR).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mdName + "-creds", Namespace: WatchNamespace},
		Data:       map[string][]byte{"GEMINI_API_KEY": []byte(canary)},
	}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatalf("create canary credentials Secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })

	// Point the REAL gemini provider at a closed server → connection-refused
	// transport error on List. No RegisterTestProvider override, so
	// providers.Lookup("gemini") returns the production constructor.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	providers.SetTestBaseURL(t, "gemini", srv.URL)
	srv.Close()

	md := modeldiscoverySampleCR(mdName, "gemini")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// The transport-error path writes Ready=False reason="SourceUnreachable".
	got := pollDiscoveryStatusReady(t, ctx, mdName, "SourceUnreachable", 30*time.Second)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if ready == nil || ready.Reason != "SourceUnreachable" {
		t.Fatalf("expected Ready=SourceUnreachable; got %+v", got.Status.Conditions)
	}
	for _, c := range got.Status.Conditions {
		if strings.Contains(c.Message, canary) {
			t.Fatalf("condition %q message leaked canary key: %q", c.Type, c.Message)
		}
	}
}

// TestModelDiscovery_Vertex_KeyNeverReachesChild drives the REAL vertex
// provider (fake token + API servers) with a service-account key carrying a
// canary, and asserts the generated child carries only the routing params
// (vertex_location / vertex_project) — never key material (MDISC-15).
func TestModelDiscovery_Vertex_KeyNeverReachesChild(t *testing.T) {
	const canary = "VERTEX-KEY-CANARY-FAKE"
	ctx := context.Background()
	mdName := "vertex-key-leak-guard"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	key, _ := json.Marshal(map[string]string{
		"project_id":     "alt06-gemini",
		"private_key_id": canary,
		"client_email":   "sa@alt06-gemini.iam.gserviceaccount.com",
		"private_key":    pemKey,
	})
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mdName + "-creds", Namespace: WatchNamespace},
		Data:       map[string][]byte{"VERTEX_CREDENTIALS": key},
	}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatalf("create credentials Secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"publisherModels":[{"name":"publishers/google/models/gemini-3.8-flash"}]}`))
	}))
	defer apiSrv.Close()
	providers.SetTestBaseURL(t, "vertex-token", tokenSrv.URL) // providers.vertexTokenURLKey
	providers.SetTestBaseURL(t, "vertex", apiSrv.URL)

	md := modeldiscoverySampleCR(mdName, "vertex")
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	child := pollChildModel(t, ctx, "vertex.gemini-3.8-flash", 30*time.Second)
	raw := string(child.Spec.Params.Raw)
	for _, leak := range []string{canary, "PRIVATE KEY", "private_key"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("child params leaked %q: %s", leak, raw)
		}
	}
	var parent litellmv1alpha1.LiteLLMModelDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &parent); err == nil {
		for _, c := range parent.Status.Conditions {
			if strings.Contains(c.Message, canary) {
				t.Fatalf("condition %q leaked canary: %q", c.Type, c.Message)
			}
		}
	}
}
