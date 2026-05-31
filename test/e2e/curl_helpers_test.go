//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// curl_helpers centralizes the throwaway curlimages/curl pod used to probe
// cluster-internal HTTP endpoints from the test runner.
//
// `kubectl run --rm -i` is racy: the interactive attach performs an HTTP
// connection upgrade that can lose the short-lived curl container
// ("unable to upgrade connection: container <p> not found in pod <p>"),
// after which the log-streaming fallback may capture nothing — yielding
// EMPTY stdout with exit code 0. The curl process still ran (so POST side
// effects land), but the response body is lost. Any caller that PARSES the
// body must therefore retry until the expected payload appears; the helpers
// below do exactly that, absorbing the race instead of flaking the suite.
const (
	curlImage      = "curlimages/curl:8.10.1"
	curlPodTimeout = 90 * time.Second
	curlPodPoll    = 3 * time.Second
)

// runCurlPod runs one throwaway curl pod in ns and returns its combined
// output. podPrefix is suffixed with a nanosecond stamp for uniqueness.
func runCurlPod(ns, podPrefix string, curlArgs ...string) ([]byte, error) {
	podName := fmt.Sprintf("%s-%d", podPrefix, time.Now().UnixNano())
	runArgs := append([]string{
		"-n", ns, "run", podName,
		"--rm", "-i", "--restart=Never", "--quiet",
		"--image=" + curlImage, "--",
	}, curlArgs...)
	return exec.Command("kubectl", runArgs...).CombinedOutput()
}

// curlPodJSON probes an endpoint and retries past the kubectl-run attach
// race until the combined output contains marker ('{' for a JSON object,
// '[' for an array), returning the output sliced from that byte. Fails the
// spec only if no marker appears within the retry budget.
func curlPodJSON(ns, podPrefix string, marker byte, curlArgs ...string) []byte {
	GinkgoHelper()
	var body, lastOut []byte
	var lastErr error
	Eventually(func() bool {
		out, err := runCurlPod(ns, podPrefix, curlArgs...)
		lastOut, lastErr = out, err
		if err != nil {
			return false
		}
		idx := bytes.IndexByte(out, marker)
		if idx < 0 {
			return false // attach race: empty/markerless body — retry
		}
		body = out[idx:]
		return true
	}, curlPodTimeout, curlPodPoll).Should(BeTrue(),
		"curl %s/%s never returned a %q JSON marker (kubectl-run attach race); lastErr=%v out=%s",
		ns, podPrefix, string(marker), lastErr, string(lastOut))
	return body
}

// curlPodBody probes an endpoint and retries until accept is satisfied with
// the combined output, returning it raw. Use for non-JSON payloads (e.g.
// Prometheus text exposition).
func curlPodBody(ns, podPrefix string, accept func([]byte) bool, curlArgs ...string) []byte {
	GinkgoHelper()
	var out []byte
	var lastErr error
	Eventually(func() bool {
		var err error
		out, err = runCurlPod(ns, podPrefix, curlArgs...)
		lastErr = err
		if err != nil {
			return false
		}
		return accept(out)
	}, curlPodTimeout, curlPodPoll).Should(BeTrue(),
		"curl %s/%s body never satisfied predicate (kubectl-run attach race); lastErr=%v out=%s",
		ns, podPrefix, lastErr, string(out))
	return out
}
