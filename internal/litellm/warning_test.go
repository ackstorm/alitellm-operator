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

// TestWarnIfDangerouslyLogBodies_SilentWhenUnset asserts that an unset
// env var produces zero log output (the helper returns false and does
// not call log.Error).
func TestWarnIfDangerouslyLogBodies_SilentWhenUnset(t *testing.T) {
	t.Setenv(EnvDangerouslyLogBodies, "")

	cap := &bytes.Buffer{}
	logger := logr.New(&bufferSink{buf: cap})

	fired := WarnIfDangerouslyLogBodies(logger)
	if fired {
		t.Fatalf("WarnIfDangerouslyLogBodies returned true with unset env, want false")
	}
	if out := cap.String(); out != "" {
		t.Errorf("captured log non-empty with unset env; got:\n%s", out)
	}
}

// TestWarnIfDangerouslyLogBodies_SilentOnNonTrueValues asserts the
// banner predicate matches newHTTPClient byte-for-byte: only the exact
// string "true" trips it. Other spellings that ParseBool would accept
// ("1", "yes", "TRUE", "True") MUST NOT trip the banner, because they
// also do NOT enable body logging in newHTTPClient.
//
// This is the invariant under test: banner-fires-iff-bodies-logged.
func TestWarnIfDangerouslyLogBodies_SilentOnNonTrueValues(t *testing.T) {
	cases := []string{"false", "0", "1", "yes", "TRUE", "True", "no", "junk"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvDangerouslyLogBodies, v)

			cap := &bytes.Buffer{}
			logger := logr.New(&bufferSink{buf: cap})

			fired := WarnIfDangerouslyLogBodies(logger)
			if fired {
				t.Fatalf("WarnIfDangerouslyLogBodies returned true for value %q, want false", v)
			}
			if out := cap.String(); out != "" {
				t.Errorf("captured log non-empty for value %q; got:\n%s", v, out)
			}
		})
	}
}
