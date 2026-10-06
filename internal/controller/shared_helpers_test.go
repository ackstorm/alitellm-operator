// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
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

// TestNewAckMissingFn_PolicyByPermanence: Orphan drains (nil) only on a
// permanent cause; a transient one (outage, 401) keeps the finalizer and
// defers, so the LiteLLM row is deleted once LiteLLM is reachable again.
// Delete blocks regardless.
func TestNewAckMissingFn_PolicyByPermanence(t *testing.T) {
	cases := []struct {
		policy    deletionpolicy.Policy
		permanent bool
		wantErr   bool
		wantEvent string
	}{
		{deletionpolicy.Orphan, true, false, "LiteLLMDeleteOrphaned"},
		{deletionpolicy.Orphan, false, true, "LiteLLMDeleteDeferred"},
		{deletionpolicy.Delete, true, true, "LiteLLMDeleteBlocked"},
		{deletionpolicy.Delete, false, true, "LiteLLMDeleteBlocked"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/permanent=%v", tc.policy, tc.permanent), func(t *testing.T) {
			rec := record.NewFakeRecorder(1)
			obj := &litellmv1alpha1.LiteLLMModel{}
			err := newAckMissingFn(rec, obj, "LiteLLMModel", "ns", "m", tc.policy)("why", tc.permanent)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if ev := <-rec.Events; !strings.Contains(ev, tc.wantEvent) {
				t.Errorf("event %q, want reason %s", ev, tc.wantEvent)
			}
		})
	}
}

func TestAckUnavailable_OnlyAbsentIsPermanent(t *testing.T) {
	for _, reason := range []string{"", "Unreachable", "Connecting", "BadMasterKey", "SecretNotFound", "InvalidEndpoint", "InsecureEndpoint", reasonAbsent} {
		_, permanent := ackUnavailable(connection.ConnectionSnapshot{Reason: reason})
		if permanent != (reason == reasonAbsent) {
			t.Errorf("reason %q: permanent=%v", reason, permanent)
		}
	}
}
