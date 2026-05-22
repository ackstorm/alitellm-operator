//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// DumpForensicsOnFail writes operator + LiteLLM logs, CRs YAML, and events to
// /tmp/tier2-<timestamp>-<spec>-*.log when the current spec failed.
// Call from a DeferCleanup or AfterEach block.
func DumpForensicsOnFail() {
	rep := CurrentSpecReport()
	if !rep.Failed() {
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	prefix := filepath.Join("/tmp", "tier2-"+stamp+"-"+sanitize(rep.LeafNodeText))

	targets := []struct {
		name string
		args []string
	}{
		{"operator.log", []string{"-n", "default", "logs", "deploy/alitellm-operator", "--tail=400"}},
		{"litellm.log", []string{"-n", "litellm-system", "logs", "deploy/litellm", "--tail=400"}},
		{"crs.yaml", []string{"-n", "default", "get",
			"litellmconnections,models,modeldiscoveries,mcpservers,mcpserverdiscoveries,a2aagents,teams",
			"-o", "yaml"}},
		{"events.txt", []string{"-n", "default", "get", "events", "--sort-by=.lastTimestamp"}},
	}
	for _, t := range targets {
		out, _ := exec.Command("kubectl", t.args...).CombinedOutput()
		_ = os.WriteFile(prefix+"-"+t.name, out, 0o644)
	}
	AddReportEntry("forensics-prefix", prefix)
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "anon"
	}
	s = sanitizeRe.ReplaceAllString(s, "_")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
