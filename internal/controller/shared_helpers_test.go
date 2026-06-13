// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestIs4xxStatus_TypedAndWrapped(t *testing.T) {
	base := &litellm.RejectedError{Status: 422, Method: "POST", Path: "/model/new"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bare 422", base, true},
		{"wrapped 422", fmt.Errorf("context: %w", base), true},
		{"bare 404", &litellm.RejectedError{Status: 404}, true},
		{"500 not 4xx", &litellm.RejectedError{Status: 500}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := is4xxStatus(tc.err); got != tc.want {
				t.Errorf("is4xxStatus(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
