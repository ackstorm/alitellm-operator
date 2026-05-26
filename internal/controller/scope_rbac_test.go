// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRBAC_ManagerRoleIsNamespaced — issue #21. The kustomize bundle
// at deploy/kustomize/manager-rbac.yaml MUST contain a namespace-
// scoped Role (not a ClusterRole) for the operator's runtime
// permissions, plus a RoleBinding (not a ClusterRoleBinding) under
// the new names. ClusterRoles for metrics-auth and toolhive-reader
// remain.
//
// This is a structural assertion against the generated bundle. It
// runs in `make unit` (no apiserver needed) and gates regressions if
// `make deploy-kustomize-sync` or the config/rbac/ source files are
// ever reverted to ClusterRole.
func TestRBAC_ManagerRoleIsNamespaced(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// internal/controller -> repo root is ../..
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	bundle := filepath.Join(root, "deploy", "kustomize", "manager-rbac.yaml")

	body, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("read %s: %v", bundle, err)
	}
	s := string(body)

	// Positive assertions: the new shape must be present.
	mustContain := []string{
		"kind: Role",
		"name: alitellm-operator-role",
		"kind: RoleBinding",
		"name: alitellm-operator-rolebinding",
	}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Errorf("manager-rbac.yaml missing required substring %q", want)
		}
	}

	// Negative assertions: the old shape must NOT be present.
	mustNotContain := []string{
		"name: alitellm-operator-manager-role",
		"name: alitellm-operator-manager-rolebinding",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(s, bad) {
			t.Errorf("manager-rbac.yaml still contains legacy substring %q", bad)
		}
	}

	// The manager Role binding must NOT be a ClusterRoleBinding. Look
	// back from "name: alitellm-operator-rolebinding" to confirm the
	// nearest preceding "kind:" line is RoleBinding, not ClusterRoleBinding.
	if idx := strings.Index(s, "name: alitellm-operator-rolebinding"); idx > 0 {
		start := idx - 200
		if start < 0 {
			start = 0
		}
		window := s[start:idx]
		if strings.Contains(window, "kind: ClusterRoleBinding") {
			t.Errorf("alitellm-operator-rolebinding is declared as ClusterRoleBinding, want RoleBinding")
		}
	}

	// metrics-auth and toolhive-reader must REMAIN ClusterRole/Binding.
	clusterMust := []string{
		"name: alitellm-operator-metrics-auth-role",
		"name: alitellm-operator-toolhive-reader",
	}
	for _, want := range clusterMust {
		if !strings.Contains(s, want) {
			t.Errorf("manager-rbac.yaml missing retained ClusterRole %q", want)
		}
	}
}
