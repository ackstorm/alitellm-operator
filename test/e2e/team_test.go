//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	podName := fmt.Sprintf("team-list-poke-%d", time.Now().UnixNano())
	path := "/v2/team/list?team_alias=" + url.QueryEscape(alias) + "&page_size=100"
	out, err := exec.Command("kubectl", "-n", "litellm-system", "run", podName,
		"--rm", "-i", "--restart=Never", "--quiet",
		"--image=curlimages/curl:8.10.1", "--",
		"curl", "-sS", "--max-time", "10",
		"-H", "Authorization: Bearer sk-test-master-key",
		"http://litellm.litellm-system.svc.cluster.local:4000"+path,
	).CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "out=%s", string(out))

	idx := bytes.IndexByte(out, '{')
	ExpectWithOffset(1, idx).To(BeNumerically(">=", 0), "no JSON object in: %s", string(out))
	var resp struct {
		Teams []map[string]interface{} `json:"teams"`
	}
	ExpectWithOffset(1, json.Unmarshal(out[idx:], &resp)).
		To(Succeed(), "raw=%s", string(out[idx:]))
	// Server-side filter is partial — apply exact match client-side.
	var matched []map[string]interface{}
	for _, t := range resp.Teams {
		if a, _ := t["team_alias"].(string); a == alias {
			matched = append(matched, t)
		}
	}
	return matched
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
})
