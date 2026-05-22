// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"
	"testing"
)

func TestOperator_PrefixedWithProductName(t *testing.T) {
	got := Operator()
	if !strings.HasPrefix(got, "alitellm-operator/") {
		t.Fatalf("Operator() must start with 'alitellm-operator/': %q", got)
	}
}

func TestOperator_LDFlagsInjectionSeam(t *testing.T) {
	saved := Version
	defer func() { Version = saved }()
	Version = "1.2.3"
	if got, want := Operator(), "alitellm-operator/1.2.3"; got != want {
		t.Fatalf("Operator(): got %q, want %q", got, want)
	}
}
