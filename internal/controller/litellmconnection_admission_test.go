// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestLiteLLMConnection_Admission_EndpointRejections exercises the CRD
// Pattern + 3 CEL XValidation rules on spec.endpoint. Each subtest
// expects apiserver-side admission rejection BEFORE the reconciler
// sees the object.
func TestLiteLLMConnection_Admission_EndpointRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ensureNoConnectionDefault(t, ctx)
	ensureMasterKeySecret(t, ctx)

	cases := []struct {
		name     string
		endpoint string
		want     string // substring expected in apiserver rejection
	}{
		{"missing scheme", "litellm:4000", "should match"},
		{"ftp scheme", "ftp://litellm:4000", "http"},
		{"userinfo", "http://u:p@litellm:4000", "userinfo"},
		{"whitespace", "http://litellm:4000 ", "whitespace"},
		{"query", "http://litellm:4000?a=1", "should match"},
		{"fragment", "http://litellm:4000#f", "should match"},
		{"too long", "http://" + strings.Repeat("a", 2100), "Too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &litellmv1alpha1.LiteLLMConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
				Spec: litellmv1alpha1.LiteLLMConnectionSpec{
					Endpoint:           tc.endpoint,
					MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{Name: "litellm-master-key", Key: "masterKey"},
				},
			}
			err := k8sClient.Create(ctx, conn)
			if err == nil {
				_ = k8sClient.Delete(ctx, conn)
				t.Fatalf("expected admission rejection for %q, got nil", tc.endpoint)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestLiteLLMConnection_Admission_EndpointAccepts confirms the new
// Pattern + CEL rules do not regress the canonical accepted shapes.
// Tests Create + Delete with no reconciler interaction expectations.
func TestLiteLLMConnection_Admission_EndpointAccepts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ensureMasterKeySecret(t, ctx)

	cases := []string{
		"http://litellm.default.svc.cluster.local:4000",
		"https://litellm.example.com",
		"https://gw.example.com/litellm",
		"https://gw.example.com/litellm/v1",
		"https://[::1]:4000",
	}
	for _, ep := range cases {
		t.Run(ep, func(t *testing.T) {
			ensureNoConnectionDefault(t, ctx)
			conn := &litellmv1alpha1.LiteLLMConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
				Spec: litellmv1alpha1.LiteLLMConnectionSpec{
					Endpoint:           ep,
					MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{Name: "litellm-master-key", Key: "masterKey"},
				},
			}
			if err := k8sClient.Create(ctx, conn); err != nil {
				t.Fatalf("expected accept for %q, got %v", ep, err)
			}
			_ = k8sClient.Delete(ctx, conn)
		})
	}
}
