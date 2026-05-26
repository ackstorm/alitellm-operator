// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// TestWarnIfDangerouslyLogBodies_FiresOnTrue asserts that setting the
// env var to the literal string "true" emits an Error-level banner
// whose msg field names the env var verbatim.
//
// Cross-ref: Issue #26 acceptance criteria.
func TestWarnIfDangerouslyLogBodies_FiresOnTrue(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "true")

	cap := &bytes.Buffer{}
	logger := logr.New(&bufferSink{buf: cap})

	fired := WarnIfDangerouslyLogBodies(logger)
	if !fired {
		t.Fatalf("WarnIfDangerouslyLogBodies returned false, want true")
	}

	out := cap.String()
	if !strings.Contains(out, EnvDangerouslyLogBodies) {
		t.Errorf("captured log does not contain env var name %q; got:\n%s",
			EnvDangerouslyLogBodies, out)
	}
	if !strings.Contains(out, "DANGER") {
		t.Errorf("captured log does not contain DANGER marker; got:\n%s", out)
	}
	// bufferSink.Error appends " err=<errStr(err)>" — with nil err this
	// becomes the stable " err=" suffix on the msg portion. Its presence
	// is the proxy assertion for "this line came through the Error path,
	// not Info."
	if !strings.Contains(out, " err=") {
		t.Errorf("captured log lacks the Error-level marker ' err='; "+
			"banner may have been emitted at Info level. Got:\n%s", out)
	}
}
