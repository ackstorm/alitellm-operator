// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func TestIs4xxError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 rejected", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 400, Code: "400"}, true},
		{"422 rejected", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 422, Code: "422"}, true},
		{"500 not 4xx", &litellm.RejectedError{Method: "POST", Path: "/model/new", Status: 500, Code: "500"}, false},
		{"network not 4xx", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := is4xxError(tc.err); got != tc.want {
				t.Errorf("is4xxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
