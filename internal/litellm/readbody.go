// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge signals that an upstream response exceeded the read
// cap. Surfaced as a distinct error (not a JSON decode failure) so a
// truncated LIST body produces an actionable "response too large" message
// instead of a misleading "decode" error that loops forever.
var ErrResponseTooLarge = errors.New("litellm: response exceeded read cap")

// readCappedBody reads up to capBytes from r. If the body is larger than
// capBytes (i.e. a capBytes+1th byte exists), it returns ErrResponseTooLarge
// instead of silently truncating. The underlying read error is preserved
// (never discarded), so a mid-body network reset is not misclassified as a
// decode failure.
func readCappedBody(r io.Reader, capBytes int) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(capBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("litellm: read response: %w", err)
	}
	if len(buf) > capBytes {
		return nil, ErrResponseTooLarge
	}
	return buf, nil
}
