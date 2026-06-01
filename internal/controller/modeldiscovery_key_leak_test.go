// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
