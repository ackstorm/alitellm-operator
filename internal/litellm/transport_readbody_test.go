// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
)

type fakeBaseRoundTripper struct{ resp *http.Response }

func (f fakeBaseRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return f.resp, nil }

type erroringBody struct {
	readErr error
	closed  bool
}

func (e *erroringBody) Read([]byte) (int, error) { return 0, e.readErr }
func (e *erroringBody) Close() error             { e.closed = true; return nil }

// TestRoundTrip_LogBodies_ReadError_BodyStillReadable is the #53 regression:
// in log-bodies mode the response Body is Closed unconditionally, but before
// the fix the NopCloser substitution only ran on read success — so a
// mid-stream read error returned a response whose Body was already closed,
// turning into a silent downstream read failure. The Body must remain
// readable even when the diagnostic read errors.
func TestRoundTrip_LogBodies_ReadError_BodyStillReadable(t *testing.T) {
	eb := &erroringBody{readErr: errors.New("mid-stream reset")}
	resp := &http.Response{
		StatusCode: 200,
		Body:       eb,
		Header:     make(http.Header),
	}
	rt := &redactingRoundTripper{
		base:      fakeBaseRoundTripper{resp: resp},
		log:       logr.Discard(),
		logBodies: true,
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/path", nil)

	got, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !eb.closed {
		t.Error("original response Body should have been closed")
	}
	// The returned Body must be a fresh, readable ReadCloser — NOT the
	// already-closed erroring body.
	if _, rerr := io.ReadAll(got.Body); rerr != nil {
		t.Fatalf("returned Body must be readable after a logBodies read error; got %v", rerr)
	}
}
