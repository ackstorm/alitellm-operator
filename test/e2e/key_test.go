//go:build e2e

// SPDX-License-Identifier: MIT

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	keyGVR    = schema.GroupVersionResource{Group: "litellm.ackstorm.ai", Version: "v1alpha1", Resource: "litellmkeys"}
	secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// litellmKeyInfoBody reads GET /key/info by hash, as the operator does, so
// the raw key never appears in a URL. Returns the raw body: a deleted key is
// 404 or 200 with status "deleted" depending on the LiteLLM version.
func litellmKeyInfoBody(value string) string {
	GinkgoHelper()
	sum := sha256.Sum256([]byte(value))
	return string(curlPodJSON("litellm-system", "key-info-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/key/info?key="+hex.EncodeToString(sum[:]),
	))
}

func keyCompletion(key, model string) string {
	GinkgoHelper()
	return string(curlPodJSON("litellm-system", "key-complete", '{',
		"curl", "-sS", "--max-time", "20",
		"-w", "\nHTTP_STATUS:%{http_code}",
		"-X", "POST",
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/json",
		"-d", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`,
		litellmBase+"/v1/chat/completions",
	))
}

// keyGet issues GET path with key and returns the body followed by an
// HTTP_STATUS line.
func keyGet(key, path string, extra ...string) string {
	GinkgoHelper()
	args := append([]string{"curl", "-sS", "--max-time", "20",
		"-w", "\nHTTP_STATUS:%{http_code}",
		"-H", "Authorization: Bearer " + key}, extra...)
	return string(curlPodBody("litellm-system", "key-get",
		func(b []byte) bool { return bytes.Contains(b, []byte("HTTP_STATUS:")) },
		append(args, litellmBase+path)...))
}

// keyStatusLine returns the HTTP_STATUS line of a keyGet or keyCompletion
// result plus the first 160 bytes of the body, for the spec report.
func keyStatusLine(out string) string {
	body, status, _ := strings.Cut(out, "\nHTTP_STATUS:")
	if len(body) > 160 {
		body = body[:160]
	}
	return "HTTP " + strings.TrimSpace(status) + " " + strings.ReplaceAll(body, "\n", " ")
}

// envtest counterpart: internal/controller/key_controller_test.go covers the
// reconcile state machine against the mock. This spec proves what the mock
// cannot: the real LiteLLM accepts an operator-supplied key value, enforces
// the key's models, clears a budget on an explicit null, and the operator's
// per-namespace Secret RBAC from the Helm chart is enough.
var _ = Describe("LiteLLMKey", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const (
		ns        = "default"
		secretNS  = "e2e-keys"
		crName    = "e2e-key"
		secret    = "e2e-key-secret"
		modelName = "e2e-key-model"
	)

	// syncedKey waits for the LiteLLMKey to be Synced and returns the value
	// the operator wrote to its Secret.
	syncedKey := func(ctx context.Context, crName, secret string) string {
		GinkgoHelper()
		var key string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(keyGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			reason := ""
			for _, c := range conds {
				if m, ok := c.(map[string]interface{}); ok && m["type"] == "Ready" {
					reason, _ = m["reason"].(string)
				}
			}
			g.Expect(reason).To(Equal("Synced"), "conditions=%v", conds)
			s, err := dyn.Resource(secretGVR).Namespace(secretNS).Get(ctx, secret, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			enc, _, _ := unstructured.NestedString(s.Object, "data", "api-key")
			raw, err := base64.StdEncoding.DecodeString(enc)
			g.Expect(err).NotTo(HaveOccurred())
			key = string(raw)
			g.Expect(strings.HasPrefix(key, "sk-")).To(BeTrue(), "Secret does not hold an sk- key")
		}, 90*time.Second, 3*time.Second).Should(Succeed())
		return key
	}

	It("KEY-01 mints a key the service can use, scoped to its models", func() {
		ctx := context.Background()
		_ = dyn.Resource(keyGVR).Namespace(ns).Delete(ctx, crName, metav1.DeleteOptions{})
		_ = dyn.Resource(modelGVR).Namespace(ns).Delete(ctx, modelName, metav1.DeleteOptions{})
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(keyGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "previous key still terminating")
			_, err = dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "previous model still terminating")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(modelName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(modelGVR).Namespace(ns).Delete(context.Background(), modelName, metav1.DeleteOptions{})
		})
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "model not registered yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMKey",
			"metadata":   map[string]interface{}{"name": crName, "namespace": ns},
			"spec": map[string]interface{}{
				"models":         []interface{}{modelName},
				"budget":         map[string]interface{}{"limit": int64(5), "period": "30d"},
				"secretRef":      map[string]interface{}{"namespace": secretNS, "name": secret, "key": "api-key"},
				"deletionPolicy": "Delete",
			},
		}}
		_, err = dyn.Resource(keyGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(keyGVR).Namespace(ns).Delete(context.Background(), crName, metav1.DeleteOptions{})
		})

		key := syncedKey(ctx, crName, secret)

		// Retry: LiteLLM caches key auth state briefly after /key/generate.
		Eventually(func(g Gomega) {
			out := keyCompletion(key, modelName)
			g.Expect(out).To(ContainSubstring("HTTP_STATUS:200"), "allowed model must answer, got: %s", out)
			g.Expect(out).To(ContainSubstring("E2E-MOCK-COMPLETION-OK"), "call did not reach the mock provider: %s", out)
		}, 90*time.Second, 5*time.Second).Should(Succeed())

		Expect(keyCompletion(key, "e2e-key-not-granted")).To(ContainSubstring("key_model_access_denied"),
			"a model outside spec.models must be denied by the key")

		plain := keyGet(key, "/metrics/")
		AddReportEntry("case 3: key without allowedRoutes, GET /metrics/", keyStatusLine(plain))
		Expect(plain).To(ContainSubstring("HTTP_STATUS:401"), "a key without allowedRoutes must be refused on /metrics/")

		var info struct {
			Info struct {
				MaxBudget      *float64 `json:"max_budget"`
				BudgetDuration *string  `json:"budget_duration"`
			} `json:"info"`
		}
		Expect(json.Unmarshal([]byte(litellmKeyInfoBody(key)), &info)).To(Succeed())
		Expect(info.Info.MaxBudget).NotTo(BeNil())
		Expect(*info.Info.MaxBudget).To(Equal(5.0))

		// Removing the budget must clear it in LiteLLM (explicit null).
		Eventually(func() error {
			obj, err := dyn.Resource(keyGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			unstructured.RemoveNestedField(obj.Object, "spec", "budget")
			_, err = dyn.Resource(keyGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			info.Info.MaxBudget, info.Info.BudgetDuration = nil, nil
			g.Expect(json.Unmarshal([]byte(litellmKeyInfoBody(key)), &info)).To(Succeed())
			g.Expect(info.Info.MaxBudget).To(BeNil(), "max_budget not cleared")
			g.Expect(info.Info.BudgetDuration).To(BeNil(), "budget_duration not cleared")
		}, 60*time.Second, 3*time.Second).Should(Succeed())

		Expect(dyn.Resource(keyGVR).Namespace(ns).Delete(ctx, crName, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(secretGVR).Namespace(secretNS).Get(ctx, secret, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Secret must be deleted with the LiteLLMKey")
			body := litellmKeyInfoBody(key)
			g.Expect(strings.Contains(body, "not found") || strings.Contains(body, `"deleted"`)).To(BeTrue(),
				"key must be gone from LiteLLM, got: %s", body)
		}, 90*time.Second, 3*time.Second).Should(Succeed())
	})

	// Needs litellm_settings.require_auth_for_metrics_endpoint on the proxy
	// (01-deps/litellm.values.yaml): with it off /metrics answers anyone.
	It("KEY-02 confines a key to allowedRoutes and clears them", func() {
		ctx := context.Background()
		const (
			crName    = "e2e-key-metrics"
			secret    = "e2e-key-metrics-secret"
			modelName = "e2e-key-metrics-model"
		)
		_ = dyn.Resource(keyGVR).Namespace(ns).Delete(ctx, crName, metav1.DeleteOptions{})
		_ = dyn.Resource(modelGVR).Namespace(ns).Delete(ctx, modelName, metav1.DeleteOptions{})
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(keyGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "previous key still terminating")
			_, err = dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "previous model still terminating")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		Expect(keyGet("sk-e2e-not-a-key", "/metrics/")).To(ContainSubstring("HTTP_STATUS:401"),
			"the proxy must require auth on /metrics, or this spec proves nothing")

		// A real model, so a denial below is the key's and not a missing model.
		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(modelName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(modelGVR).Namespace(ns).Delete(context.Background(), modelName, metav1.DeleteOptions{})
		})
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).Get(ctx, modelName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "model not registered yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMKey",
			"metadata":   map[string]interface{}{"name": crName, "namespace": ns},
			"spec": map[string]interface{}{
				"allowedRoutes":  []interface{}{"/metrics"},
				"secretRef":      map[string]interface{}{"namespace": secretNS, "name": secret, "key": "api-key"},
				"deletionPolicy": "Delete",
			},
		}}
		_, err = dyn.Resource(keyGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(keyGVR).Namespace(ns).Delete(context.Background(), crName, metav1.DeleteOptions{})
		})
		key := syncedKey(ctx, crName, secret)

		// Retry: LiteLLM caches key auth state briefly after a key write.
		var out string
		Eventually(func(g Gomega) {
			out = keyGet(key, "/metrics/")
			g.Expect(out).To(ContainSubstring("HTTP_STATUS:200"), "got: %s", keyStatusLine(out))
			g.Expect(out).To(ContainSubstring("# HELP"), "not a Prometheus exposition: %s", keyStatusLine(out))
		}, 90*time.Second, 5*time.Second).Should(Succeed())
		AddReportEntry("case 1: allowedRoutes [/metrics], GET /metrics/", keyStatusLine(out))

		// The bare path redirects to the slash form.
		out = keyGet(key, "/metrics", "-L")
		AddReportEntry("case 1: allowedRoutes [/metrics], GET /metrics (following redirects)", keyStatusLine(out))
		Expect(out).To(ContainSubstring("HTTP_STATUS:200"))

		out = keyCompletion(key, modelName)
		AddReportEntry("case 2: allowedRoutes [/metrics], POST /v1/chat/completions", keyStatusLine(out))
		Expect(out).To(ContainSubstring("HTTP_STATUS:403"))
		Expect(out).To(ContainSubstring("not allowed to call this route"))

		Eventually(func() error {
			obj, err := dyn.Resource(keyGVR).Namespace(ns).Get(ctx, crName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			unstructured.RemoveNestedField(obj.Object, "spec", "allowedRoutes")
			_, err = dyn.Resource(keyGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
			return err
		}, 30*time.Second, 2*time.Second).Should(Succeed())
		var info struct {
			Info struct {
				Models        []string `json:"models"`
				AllowedRoutes []string `json:"allowed_routes"`
			} `json:"info"`
		}
		Eventually(func(g Gomega) {
			info.Info.Models, info.Info.AllowedRoutes = nil, nil
			g.Expect(json.Unmarshal([]byte(litellmKeyInfoBody(key)), &info)).To(Succeed())
			g.Expect(info.Info.AllowedRoutes).To(BeEmpty(), "allowed_routes not cleared")
		}, 60*time.Second, 3*time.Second).Should(Succeed())
		AddReportEntry("case 4: allowedRoutes removed from the CR, GET /key/info",
			fmt.Sprintf("allowed_routes=%v models=%v", info.Info.AllowedRoutes, info.Info.Models))
		Expect(info.Info.Models).To(ConsistOf("no-default-models"))

		Eventually(func(g Gomega) {
			out = keyGet(key, "/metrics/")
			g.Expect(out).To(ContainSubstring("HTTP_STATUS:401"), "got: %s", keyStatusLine(out))
		}, 90*time.Second, 5*time.Second).Should(Succeed())
		AddReportEntry("case 4: same key after the clear, GET /metrics/", keyStatusLine(out))

		// No routes and no models: the chat route is open again and the key
		// still reaches no model.
		Eventually(func(g Gomega) {
			out = keyCompletion(key, modelName)
			g.Expect(out).To(ContainSubstring("key_model_access_denied"), "got: %s", keyStatusLine(out))
		}, 90*time.Second, 5*time.Second).Should(Succeed())
		AddReportEntry("models rule: no models, no allowedRoutes, POST /v1/chat/completions", keyStatusLine(out))
	})
})
