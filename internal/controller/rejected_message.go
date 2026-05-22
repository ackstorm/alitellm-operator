// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// maxRejectedMessageBytes caps the LiteLLMRejected condition.Message
// length so `kubectl get -o wide` (which truncates around 80 chars) and
// status writers (whose updates are bounded by the apiserver's 1MiB
// resource limit) stay healthy regardless of how chatty an upstream
// error envelope is.
const maxRejectedMessageBytes = 200

// rejectedMessage formats a LiteLLMRejected condition message. When the
// underlying error is a *litellm.RejectedError with a non-empty parsed
// Message, the message is surfaced verbatim (after clipping). Otherwise
// the pre-FIX2 generic shape ("LiteLLM rejected <op>: <errStr>") is
// returned, matching the prior contract for callers without a typed
// error (envelope parse failures, transient transport errors mis-
// classified as 4xx, etc.).
//
// FIX2.txt MEDIUM-5 (2026-05-22).
func rejectedMessage(opDesc string, err error, errStr string) string {
	var rej *litellm.RejectedError
	if errors.As(err, &rej) && rej.Message != "" {
		return clipMessage(fmt.Sprintf("LiteLLM rejected %s: %s", opDesc, rej.Message))
	}
	return clipMessage(fmt.Sprintf("LiteLLM rejected %s: %s", opDesc, errStr))
}

// clipMessage truncates s to maxRejectedMessageBytes; oversized strings
// get an ellipsis suffix so the truncation is visible.
func clipMessage(s string) string {
	if len(s) <= maxRejectedMessageBytes {
		return s
	}
	return s[:maxRejectedMessageBytes-3] + "..."
}
