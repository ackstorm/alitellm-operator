// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestReadCappedBody_TruncationIsDistinctError(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 2048)
	_, err := readCappedBody(io.NopCloser(bytes.NewReader(big)), 1024)
	if err == nil {
		t.Fatal("expected truncation error when body exceeds cap")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestReadCappedBody_PreservesReadError(t *testing.T) {
	want := errors.New("boom")
	_, err := readCappedBody(io.NopCloser(errReader{want}), 1024)
	if !errors.Is(err, want) {
		t.Fatalf("read error not preserved: %v", err)
	}
}

func TestReadCappedBody_PassesUnderCap(t *testing.T) {
	body, err := readCappedBody(io.NopCloser(strings.NewReader("hello")), 1024)
	if err != nil || string(body) != "hello" {
		t.Fatalf("got %q, %v", body, err)
	}
}

func TestReadCappedBody_ExactlyAtCap(t *testing.T) {
	exact := bytes.Repeat([]byte("y"), 1024)
	body, err := readCappedBody(io.NopCloser(bytes.NewReader(exact)), 1024)
	if err != nil {
		t.Fatalf("body exactly at cap must not error: %v", err)
	}
	if len(body) != 1024 {
		t.Fatalf("want 1024 bytes, got %d", len(body))
	}
}
