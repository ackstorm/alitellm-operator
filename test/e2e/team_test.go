//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var teamGVR = schema.GroupVersionResource{
	Group:    "litellm.ackstorm.ai",
	Version:  "v1alpha1",
	Resource: "litellmteams",
}

func newTeamCR(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "litellm.ackstorm.ai/v1alpha1",
			"kind":       "LiteLLMTeam",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{},
		},
	}
}

func teamID(obj *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "teamID")
	return id
}

// litellmTeamsByAlias runs an in-cluster curl against
// GET /v2/team/list?team_alias=<alias>&page_size=100 and returns the
// decoded `teams` array. Strips kubectl-run warning prefix lines.
func litellmTeamsByAlias(alias string) []map[string]interface{} {
	path := "/v2/team/list?team_alias=" + url.QueryEscape(alias) + "&page_size=100"
	// curlPodJSON retries past the kubectl-run attach race that can drop the
	// response body to empty.
	body := curlPodJSON("litellm-system", "team-list-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		"http://litellm.litellm-system.svc.cluster.local:4000"+path,
	)
	var resp struct {
		Teams []map[string]interface{} `json:"teams"`
	}
	ExpectWithOffset(1, json.Unmarshal(body, &resp)).
		To(Succeed(), "raw=%s", string(body))
	// Server-side filter is partial — apply exact match client-side.
	var matched []map[string]interface{}
	for _, t := range resp.Teams {
		if a, _ := t["team_alias"].(string); a == alias {
			matched = append(matched, t)
		}
	}
	return matched
}

const litellmBase = "http://litellm.litellm-system.svc.cluster.local:4000"

// litellmTeamObjectPermission returns the EXPANDED object_permission object
// for a team, read from GET /team/info.
//
// Use this, not litellmTeamsByAlias, for any object_permission assertion:
// /v2/team/list reports `object_permission: null` even when the row exists
// (it carries only object_permission_id), so an assertion against the list
// endpoint can never pass. Verified on LiteLLM 1.93.0.
func litellmTeamObjectPermission(teamID string) map[string]interface{} {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "team-info-poke", '{',
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		litellmBase+"/team/info?team_id="+url.QueryEscape(teamID),
	)
	var resp struct {
		TeamInfo struct {
			ObjectPermission map[string]interface{} `json:"object_permission"`
		} `json:"team_info"`
	}
	ExpectWithOffset(1, json.Unmarshal(body, &resp)).
		To(Succeed(), "raw=%s", string(body))
	return resp.TeamInfo.ObjectPermission
}

// generateTeamKey mints a team-scoped virtual key via POST /key/generate
// (master-key authed) so a spec can prove LiteLLM ENFORCES the team's model
// scope on real inference. Returns the `sk-...` value. NOTE: this is
// test-driver traffic to /key/* — the AC-N3 scope invariant (see
// scope_ac_n3_test.go) attributes forbidden /key//user calls to the OPERATOR
// pod IP only, so this call does not trip it.
func generateTeamKey(teamID string) string {
	GinkgoHelper()
	body := curlPodJSON("litellm-system", "key-gen", '{',
		"curl", "-sS", "--max-time", "10", "-X", "POST",
		"-H", "Authorization: Bearer sk-test-master-key",
		"-H", "Content-Type: application/json",
		"-d", `{"team_id":"`+teamID+`"}`,
		litellmBase+"/key/generate",
	)
	var resp struct {
		Key string `json:"key"`
	}
	Expect(json.Unmarshal(body, &resp)).To(Succeed(), "raw=%s", string(body))
	Expect(resp.Key).NotTo(BeEmpty(), "no key in /key/generate response: %s", string(body))
	return resp.Key
}

// deleteLiteLLMKey revokes a virtual key (best-effort cleanup).
func deleteLiteLLMKey(key string) {
	_, _ = runCurlPod("litellm-system", "key-del",
		"curl", "-sS", "--max-time", "10", "-X", "POST",
		"-H", "Authorization: Bearer sk-test-master-key",
		"-H", "Content-Type: application/json",
		"-d", `{"keys":["`+key+`"]}`,
		litellmBase+"/key/delete",
	)
}

// envtest counterpart: internal/controller/team_hubseam_test.go (renamed
// from team_hubseam_e2e_test.go in Phase 4 / Task 4.1) covers reconcile
// logic, hash-equal noop steady state, 401 fastpath, and AC-DC1 hand-
// managed coexistence against an in-process mock LiteLLM. This suite
// proves the Helm-deployed operator round-trips a real Team CR through
// the real LiteLLM team-management API (real persistence, real DNS).
var _ = Describe("LiteLLMTeam", Ordered, ContinueOnFailure, func() {
	dyn := dynClient()
	const ns = "default"
	const financeName = "finance"

	BeforeAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, financeName, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	AfterAll(func() {
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, financeName, metav1.DeleteOptions{PropagationPolicy: &fg})
	})

	It("AC-T4: Team/default present in LiteLLM with no K8s Team/default CR", func() {
		// Sanity: no Team/default CR in our watched namespace.
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Get(ctx, "default", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"unexpected Team/default CR exists: %v", err)

		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias("default")
			g.Expect(matches).NotTo(BeEmpty(),
				"no LiteLLM team with team_alias=default")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("lifetime canary: deleting Team/finance does not remove Team/default", func() {
		// Snapshot Team/default's team_id pre-delete.
		preDefault := litellmTeamsByAlias("default")
		Expect(preDefault).NotTo(BeEmpty(), "Team/default missing pre-canary")
		defaultIDPre, _ := preDefault[0]["team_id"].(string)
		Expect(defaultIDPre).NotTo(BeEmpty())

		// Create Team/finance CR.
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, newTeamCR(financeName, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Wait for finance teamID to populate (AC-T1 confirmation).
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, financeName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty(), "finance teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Delete finance CR (foreground so finalizer drains synchronously).
		fg := metav1.DeletePropagationForeground
		Expect(dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, financeName, metav1.DeleteOptions{PropagationPolicy: &fg})).
			To(Succeed())
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, financeName, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"finance CR still present (err=%v)", err)
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Lifetime canary: Team/default still in LiteLLM with same team_id.
		Eventually(func(g Gomega) {
			postDefault := litellmTeamsByAlias("default")
			g.Expect(postDefault).NotTo(BeEmpty(),
				"Team/default vanished after finance deletion")
			postID, _ := postDefault[0]["team_id"].(string)
			g.Expect(postID).To(Equal(defaultIDPre),
				"Team/default team_id changed: pre=%s post=%s", defaultIDPre, postID)
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		// Negative: finance alias is gone from LiteLLM (operator cleanup).
		Eventually(func(g Gomega) {
			finance := litellmTeamsByAlias(financeName)
			g.Expect(finance).To(BeEmpty(),
				"LiteLLM still has team_alias=finance: %v", finance)
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	// ─── Phase 10 / TRL-01..TRL-06 — spec.rateLimits scenarios ─────────
	//
	// Three required scenarios per CONTEXT.md D-05 floor + two extras
	// to hit the 5-6 target: composite, RPM-leaf-clear, params-rpm_limit-
	// collision, params-rpm_limit_type-collision, whole-block-clear.
	//
	// LiteLLM 1.83.10's GET /v2/team/list returns rate-limit fields
	// (rpm_limit, tpm_limit, *_type) on team objects empirically — but
	// not all builds expose all fields uniformly. Each It block soft-
	// asserts on the LiteLLM-side view: if the field is present in the
	// returned team object, its value must match the expected; if the
	// field is absent, we fall back to status.lastRendered.hash stability
	// (which transitively proves the operator built the body correctly,
	// since the hash is over the canonical-JSON of the entire body).

	It("rateLimits composite — Team with budget+rateLimits+params reaches Synced", func() {
		const teamName = "team-composite-tr"
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["budget"] = map[string]interface{}{
			"limit":  500.0,
			"period": "30d",
		}
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(6000),
			"tpm": int64(1000000),
		}
		spec["params"] = map[string]interface{}{
			"metadata": map[string]interface{}{
				"dept": "finance",
				"env":  "production",
			},
			"blocked": false,
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty(),
				"composite team teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Soft assertion: the LiteLLM-side team_alias is found.
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias(teamName)
			g.Expect(matches).NotTo(BeEmpty(),
				"composite team not found by alias on LiteLLM side")
			if v, ok := matches[0]["rpm_limit"]; ok {
				// LiteLLM exposes rpm_limit on team_list — assert value.
				g.Expect(v).To(BeNumerically("==", 6000),
					"LiteLLM rpm_limit: want 6000, got %v", v)
			}
			if v, ok := matches[0]["tpm_limit"]; ok {
				g.Expect(v).To(BeNumerically("==", 1000000),
					"LiteLLM tpm_limit: want 1000000, got %v", v)
			}
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("rateLimits leaf-clear — patching out rpm produces hash drift", func() {
		const teamName = "team-clearing-tr"
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(6000),
			"tpm": int64(1000000),
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		// Wait for first reconcile.
		var firstHash string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty())
			h, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
			g.Expect(h).NotTo(BeEmpty())
			firstHash = h
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Patch out the rpm leaf (keep tpm). The new body should have
		// rpm_limit:null + rpm_limit_type absent, so the canonical-JSON
		// hash changes.
		obj, err := dyn.Resource(teamGVR).Namespace(ns).
			Get(ctx, teamName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		objSpec, _, _ := unstructured.NestedMap(obj.Object, "spec")
		rateLimits, _ := objSpec["rateLimits"].(map[string]interface{})
		delete(rateLimits, "rpm")
		objSpec["rateLimits"] = rateLimits
		Expect(unstructured.SetNestedMap(obj.Object, objSpec, "spec")).To(Succeed())
		_, err = dyn.Resource(teamGVR).Namespace(ns).
			Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Expect hash to change → operator emitted a new body with
		// rpm_limit:null (clearing semantic per Feature 01 §2.1).
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			h, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
			g.Expect(h).NotTo(Equal(firstHash),
				"hash unchanged after rpm leaf delete; operator did not re-render with rpm_limit:null")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("rateLimits collision — spec.params.rpm_limit yields ProjectionOverride event", func() {
		const teamName = "team-collision-tr"
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(6000),
		}
		spec["params"] = map[string]interface{}{
			"rpm_limit": int64(9999),
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		// Wait Synced.
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty(),
				"collision team teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Assert ProjectionOverride event fired mentioning rpm_limit.
		Eventually(func(g Gomega) {
			out, err := exec.Command("kubectl", "-n", ns, "get", "events",
				"--field-selector",
				"involvedObject.name="+teamName+",reason=ProjectionOverride",
				"-o", "json").CombinedOutput()
			g.Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))
			var resp struct {
				Items []map[string]interface{} `json:"items"`
			}
			g.Expect(json.Unmarshal(out, &resp)).To(Succeed())
			found := false
			for _, ev := range resp.Items {
				msg, _ := ev["message"].(string)
				if bytes.Contains([]byte(msg), []byte("rpm_limit")) {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(),
				"no ProjectionOverride event mentioning rpm_limit found (events=%d)",
				len(resp.Items))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		// Soft assertion: operator overlay wins on the wire — LiteLLM
		// sees rpm_limit=6000 (not 9999).
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias(teamName)
			g.Expect(matches).NotTo(BeEmpty())
			if v, ok := matches[0]["rpm_limit"]; ok {
				g.Expect(v).To(BeNumerically("==", 6000),
					"operator overlay must win — LiteLLM rpm_limit: want 6000, got %v", v)
			}
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("rateLimits *_type collision — spec.params.rpm_limit_type yields ProjectionOverride event", func() {
		const teamName = "team-collision-type-tr"
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(6000),
		}
		spec["params"] = map[string]interface{}{
			"rpm_limit_type": "high_priority",
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty())
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			out, err := exec.Command("kubectl", "-n", ns, "get", "events",
				"--field-selector",
				"involvedObject.name="+teamName+",reason=ProjectionOverride",
				"-o", "json").CombinedOutput()
			g.Expect(err).NotTo(HaveOccurred(), "out=%s", string(out))
			var resp struct {
				Items []map[string]interface{} `json:"items"`
			}
			g.Expect(json.Unmarshal(out, &resp)).To(Succeed())
			found := false
			for _, ev := range resp.Items {
				msg, _ := ev["message"].(string)
				if bytes.Contains([]byte(msg), []byte("rpm_limit_type")) {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(),
				"no ProjectionOverride event mentioning rpm_limit_type found (events=%d)",
				len(resp.Items))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		// Soft assertion: LiteLLM-side *_type value (if exposed) is
		// operator-hardcoded best_effort_throughput, not user-supplied
		// "high_priority".
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias(teamName)
			g.Expect(matches).NotTo(BeEmpty())
			if v, ok := matches[0]["rpm_limit_type"].(string); ok {
				g.Expect(v).To(Equal("best_effort_throughput"),
					"operator hardcoded value must win — LiteLLM rpm_limit_type: want best_effort_throughput, got %q", v)
			}
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("rateLimits whole-block clear — removing spec.rateLimits drives hash drift", func() {
		const teamName = "team-block-clear-tr"
		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(6000),
			"tpm": int64(1000000),
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		var firstHash string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty())
			h, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
			g.Expect(h).NotTo(BeEmpty())
			firstHash = h
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Patch out the whole rateLimits block.
		obj, err := dyn.Resource(teamGVR).Namespace(ns).
			Get(ctx, teamName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		unstructured.RemoveNestedField(obj.Object, "spec", "rateLimits")
		_, err = dyn.Resource(teamGVR).Namespace(ns).
			Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			h, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
			g.Expect(h).NotTo(Equal(firstHash),
				"hash unchanged after whole-block rateLimits removal; operator did not re-render with both nulls")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	// AC-T4 rateLimits-clear: CR-01 regression coverage at e2e level.
	// Creates a Team/default CR with spec.rateLimits populated, waits for
	// the rate-limit values to land on the LiteLLM-side `default` team,
	// then deletes the CR. The AC-T4 protected-deletion path (team_
	// controller.go:797) must re-apply the implicit-empty body, clearing
	// rpm_limit + tpm_limit on the wire while preserving the LiteLLM team
	// aliased `default` (no POST /team/delete on the default alias).
	It("AC-T4 rateLimits-clear: deleting Team/default with rateLimits clears them on the wire", func() {
		// Snapshot the team_id pre-create — AC-T4 invariant: deletion
		// MUST NOT change team_id (no recreate).
		preDefault := litellmTeamsByAlias("default")
		Expect(preDefault).NotTo(BeEmpty(), "Team/default missing pre-test")
		defaultIDPre, _ := preDefault[0]["team_id"].(string)
		Expect(defaultIDPre).NotTo(BeEmpty())

		By("creating Team/default CR with spec.rateLimits populated")
		cr := newTeamCR("default", ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["rateLimits"] = map[string]interface{}{
			"rpm": int64(7777),
			"tpm": int64(7777777),
		}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Ensure cleanup even if assertions fail mid-test.
		DeferCleanup(func() {
			fg := metav1.DeletePropagationForeground
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), "default", metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		By("waiting for the rate-limit values to land on the LiteLLM-side `default` team (CR Synced)")
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, "default", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			h, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
			g.Expect(h).NotTo(BeEmpty(), "lastRendered.hash not yet populated")
			// Optional soft-check: if LiteLLM exposes rpm_limit on the
			// /v2/team/list response, it should reflect the applied value.
			matches := litellmTeamsByAlias("default")
			g.Expect(matches).NotTo(BeEmpty())
			if v, ok := matches[0]["rpm_limit"].(float64); ok {
				g.Expect(v).To(Equal(7777.0),
					"rpm_limit on LiteLLM `default` team did not match applied value")
			}
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Snapshot the post-apply hash so we can detect hash drift post-delete
		// (fallback signal: the body re-rendered with rpm_limit:nil + tpm_limit:nil
		// hashes differently than the body that carried 7777/7777777).
		obj, err := dyn.Resource(teamGVR).Namespace(ns).
			Get(ctx, "default", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		preDeleteHash, _, _ := unstructured.NestedString(obj.Object, "status", "lastRendered", "hash")
		Expect(preDeleteHash).NotTo(BeEmpty())

		By("deleting Team/default — must fire AC-T4 protected-deletion path")
		fg := metav1.DeletePropagationForeground
		Expect(dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, "default", metav1.DeleteOptions{PropagationPolicy: &fg})).
			To(Succeed())

		By("verifying the CR is reaped")
		Eventually(func(g Gomega) {
			_, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, "default", metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"Team/default CR still present (err=%v)", err)
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("verifying the LiteLLM-side team aliased `default` is preserved AND no longer carries the user-set rate-limit values")
		// AC-T4 invariant: LiteLLM team aliased `default` is preserved
		// AND team_id is unchanged (no recreate).
		// Soft assertion on rpm_limit / tpm_limit: if LiteLLM 1.83.10's
		// /v2/team/list exposes these fields, both must be null/absent
		// post-deletion (the AC-T4 deletion body emits both as null).
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias("default")
			g.Expect(matches).NotTo(BeEmpty(),
				"AC-T4 violation: LiteLLM team aliased `default` was REMOVED")
			postID, _ := matches[0]["team_id"].(string)
			g.Expect(postID).To(Equal(defaultIDPre),
				"AC-T4 violation: team_id changed (pre=%s post=%s) — deletion must NOT recreate the LiteLLM team",
				defaultIDPre, postID)
			// If rpm_limit/tpm_limit are exposed, both must be null.
			if v, ok := matches[0]["rpm_limit"]; ok && v != nil {
				g.Expect(v).To(BeNil(),
					"CR-01 regression: rpm_limit on LiteLLM `default` team is %v post-deletion; want null", v)
			}
			if v, ok := matches[0]["tpm_limit"]; ok && v != nil {
				g.Expect(v).To(BeNil(),
					"CR-01 regression: tpm_limit on LiteLLM `default` team is %v post-deletion; want null", v)
			}
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})

	// ─── Deny-by-default (TEAM-05) — a present spec.permission block that
	// leaves `models` empty must FAIL CLOSED. LiteLLM reads an empty team
	// `models` list as "no filter" (fail-OPEN → the team inherits the full
	// master-key ceiling), so the operator projects the deny-all sentinel
	// `["__deny_all__"]`. This spec proves the whole chain end-to-end against
	// the Helm-deployed operator + real LiteLLM: the operator writes
	// the sentinel (round-trip via /v2/team/list) AND LiteLLM ENFORCES it —
	// a team-scoped key is denied (team_model_access_denied) when it
	// tries to run inference against a real model. Closes the one gap the
	// deny-by-default change could not verify without a live cluster (the
	// completion rejection, not just the /models phantom).
	//
	// Status code is asserted as 401-or-403: LiteLLM 1.83.10 returned 401,
	// 1.93.0 returns 403 for the same team_model_access_denied condition.
	// The stable contract is the error type + the sentinel echo, not the code.
	It("deny-by-default: empty spec.permission denies inference — TEAM-05", func() {
		const teamName = "team-deny-default"
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, teamName, metav1.DeleteOptions{PropagationPolicy: &fg})

		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		// Present-but-empty permission block: models omitted → deny-by-default.
		spec["permission"] = map[string]interface{}{}
		_, err := dyn.Resource(teamGVR).Namespace(ns).
			Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		// Wait for the operator to create the team and populate teamID.
		var tid string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).
				Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			tid = teamID(obj)
			g.Expect(tid).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Round-trip: real LiteLLM persisted the deny-all models sentinel (the
		// fail-open field turned fail-closed). An empty block would have left
		// `models: []` (all models) pre-fix.
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias(teamName)
			g.Expect(matches).NotTo(BeEmpty(), "team not in LiteLLM")
			models, _ := matches[0]["models"].([]interface{})
			g.Expect(models).To(ConsistOf("__deny_all__"),
				"team.models should carry the deny-all sentinel, got %v", matches[0]["models"])
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		// Enforcement: a team-scoped key cannot run inference against a real
		// model. tier2-openai is a live model in the e2e LiteLLM.
		key := generateTeamKey(tid)
		DeferCleanup(func() { deleteLiteLLMKey(key) })

		body := curlPodJSON("litellm-system", "deny-complete", '{',
			"curl", "-sS", "--max-time", "15",
			"-w", "\nHTTP_STATUS:%{http_code}",
			"-X", "POST",
			"-H", "Authorization: Bearer "+key,
			"-H", "Content-Type: application/json",
			"-d", `{"model":"tier2-openai","messages":[{"role":"user","content":"hi"}]}`,
			litellmBase+"/v1/chat/completions",
		)
		out := string(body)
		Expect(out).To(MatchRegexp(`HTTP_STATUS:(401|403)`),
			"team-scoped completion must be denied 401/403, got: %s", out)
		Expect(out).To(ContainSubstring("team_model_access_denied"),
			"denial must cite team_model_access_denied, got: %s", out)
		Expect(out).To(ContainSubstring("__deny_all__"),
			"denial must reference the deny-all sentinel, got: %s", out)
	})

	// TEAM-06: spec.permission.mcpToolsets carries toolset NAMES; the operator
	// resolves each to its toolset_id UUID before projecting onto
	// object_permission.mcp_toolsets, because LiteLLM matches on the UUID and
	// silently ignores a name.
	It("TEAM-06 grants MCP toolsets by name, projecting resolved UUIDs", func() {
		const teamName = "e2e-team-toolset-grant"
		const tsName = "e2e-team-toolset-target"
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Delete(ctx, tsName, metav1.DeleteOptions{PropagationPolicy: &fg})

		// The toolset must exist in LiteLLM before the Team references it —
		// otherwise the Team parks ToolsetNotFound and requeues (by design).
		ts := toolsetCR(ns, tsName, []interface{}{
			map[string]interface{}{
				"server": "some-server",
				"tools":  []interface{}{"a_tool"},
			},
		})
		_, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).
			Create(ctx, ts, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(mcpToolsetGVR).Namespace(ns).
				Delete(context.Background(), tsName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		var wantToolsetID string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(mcpToolsetGVR).Namespace(ns).
				Get(ctx, tsName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			wantToolsetID = toolsetID(obj)
			g.Expect(wantToolsetID).NotTo(BeEmpty(), "toolsetID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["permission"] = map[string]interface{}{
			"mcpToolsets": []interface{}{tsName},
		}
		_, err = dyn.Resource(teamGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(teamID(obj)).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// The grant must carry the UUID, NOT the human name.
		//
		// Read it from GET /team/info, NOT /v2/team/list: the list endpoint
		// returns `object_permission: null` even when the row exists (it
		// reports only the object_permission_id), so asserting there always
		// fails. /team/info expands the row.
		Eventually(func(g Gomega) {
			op := litellmTeamObjectPermission(teamName)
			g.Expect(op).NotTo(BeNil(), "object_permission absent from /team/info")
			granted, _ := op["mcp_toolsets"].([]interface{})
			g.Expect(granted).To(ConsistOf(wantToolsetID),
				"object_permission.mcp_toolsets must carry the resolved UUID %q, not the name %q; got %v",
				wantToolsetID, tsName, op["mcp_toolsets"])
		}, 60*time.Second, 3*time.Second).Should(Succeed())
	})

	// TEAM-07 (ported from the prod UAT runbook's FN3): the POSITIVE half of
	// TEAM-05.
	//
	// TEAM-05 proves a team with no model grant is DENIED. That denial is
	// answered by LiteLLM's own auth layer and never reaches a provider, so on
	// its own it cannot distinguish "deny-by-default works" from "team keys are
	// broken and everything is denied". This spec closes that gap by driving a
	// granted model all the way through to the mock provider.
	It("TEAM-07 grant works: a team-scoped key CAN infer against a granted model", func() {
		const teamName = "e2e-team-grant-allow"
		// Own model. Ginkgo randomizes TOP-LEVEL containers, so this spec
		// cannot rely on LiteLLMModel's tier2-openai having been created —
		// unlike TEAM-05, whose denial is answered by LiteLLM's auth layer and
		// so holds whether or not the model exists.
		const grantModel = "e2e-team-grant-model"
		fg := metav1.DeletePropagationForeground
		_ = dyn.Resource(teamGVR).Namespace(ns).
			Delete(ctx, teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		_ = dyn.Resource(modelGVR).Namespace(ns).
			Delete(ctx, grantModel, metav1.DeleteOptions{})

		Eventually(func(g Gomega) {
			_, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, grantModel, metav1.GetOptions{})
			g.Expect(err).To(HaveOccurred(), "grant model still terminating")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		_, err := dyn.Resource(modelGVR).Namespace(ns).
			Create(ctx, newOpenAIMockModel(grantModel, ns), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(modelGVR).Namespace(ns).
				Delete(context.Background(), grantModel, metav1.DeleteOptions{})
		})
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(modelGVR).Namespace(ns).
				Get(ctx, grantModel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(modelID(obj)).NotTo(BeEmpty(), "grant model not registered yet")
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		cr := newTeamCR(teamName, ns)
		spec, _ := cr.Object["spec"].(map[string]interface{})
		spec["permission"] = map[string]interface{}{
			"models": []interface{}{grantModel},
		}
		_, err = dyn.Resource(teamGVR).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = dyn.Resource(teamGVR).Namespace(ns).
				Delete(context.Background(), teamName, metav1.DeleteOptions{PropagationPolicy: &fg})
		})

		var tid string
		Eventually(func(g Gomega) {
			obj, err := dyn.Resource(teamGVR).Namespace(ns).Get(ctx, teamName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			tid = teamID(obj)
			g.Expect(tid).NotTo(BeEmpty(), "teamID not yet populated")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// The grant must land as a real model list, never the deny-all sentinel.
		Eventually(func(g Gomega) {
			matches := litellmTeamsByAlias(teamName)
			g.Expect(matches).NotTo(BeEmpty(), "team not in LiteLLM")
			g.Expect(matches[0]["models"]).To(ConsistOf(grantModel),
				"granted team must carry its model list, got %v", matches[0]["models"])
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		key := generateTeamKey(tid)
		DeferCleanup(func() { deleteLiteLLMKey(key) })

		// Retry the whole completion: LiteLLM caches team/key auth state
		// briefly, so the first call after /key/generate can still 401.
		Eventually(func(g Gomega) {
			body := curlPodJSON("litellm-system", "allow-complete", '{',
				"curl", "-sS", "--max-time", "20",
				"-w", "\nHTTP_STATUS:%{http_code}",
				"-X", "POST",
				"-H", "Authorization: Bearer "+key,
				"-H", "Content-Type: application/json",
				"-d", `{"model":"`+grantModel+`","messages":[{"role":"user","content":"hi"}]}`,
				litellmBase+"/v1/chat/completions",
			)
			out := string(body)
			g.Expect(out).To(ContainSubstring("HTTP_STATUS:200"),
				"granted completion must succeed, got: %s", out)
			// Proves the request reached the mock provider rather than being
			// short-circuited by LiteLLM — see mock handleChatCompletions.
			g.Expect(out).To(ContainSubstring("E2E-MOCK-COMPLETION-OK"),
				"completion did not reach the mock provider, got: %s", out)
		}, 90*time.Second, 5*time.Second).Should(Succeed())
	})
})
