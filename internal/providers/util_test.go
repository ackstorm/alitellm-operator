// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/go-logr/logr"
)

// bufferSink is a tiny logr.LogSink that captures every Info / Error
// call into an in-memory buffer. Provider tests use this to assert
// that canaryAPIKey NEVER leaks into log output under any code path
// (request build, request error, response parse, error wrapping).
//
// Mirrors internal/litellm/transport_test.go:30-63 verbatim minus the
// LiteLLM-specific masterKey constant — the providers package uses a
// per-test canaryAPIKey instead, since each provider's auth surface is
// different (header vs URL query) and the canary value is the API key
// that should never appear in the captured buffer.
type bufferSink struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (b *bufferSink) Init(info logr.RuntimeInfo)             {}
func (b *bufferSink) Enabled(level int) bool                 { return true }
func (b *bufferSink) WithValues(kv ...any) logr.LogSink      { return b }
func (b *bufferSink) WithName(name string) logr.LogSink      { return b }
func (b *bufferSink) Info(level int, msg string, kv ...any)  { b.write(msg, kv) }
func (b *bufferSink) Error(err error, msg string, kv ...any) { b.write(msg+" err="+errStr(err), kv) }

func (b *bufferSink) write(msg string, kv []any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(b.buf, "%s", msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(b.buf, " %v=%v", kv[i], kv[i+1])
	}
	b.buf.WriteByte('\n')
}

func (b *bufferSink) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// newBufferSinkLogger returns a (*bytes.Buffer, logr.Logger) pair so
// canary tests can assert no credential material leaks through any
// log line emitted during a provider's List call. The buffer is the
// observable side; the logger plugs straight into anywhere a
// logr.Logger is accepted.
//
// All five provider tests reuse this — the buffer-as-Sink pattern is
// cheaper than wiring up zap/zerolog and gives byte-exact substring
// assertions for the canary.
func newBufferSinkLogger(t *testing.T) (*bytes.Buffer, logr.Logger) { //nolint:unparam // logr.Logger return reserved for tests that need both the buffer + the sink-as-Logger (some currently only need the buffer)
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &bufferSink{buf: buf}
	return buf, logr.New(sink)
}
