// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestValidateWatchNamespace(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		wantErr bool
	}{
		{"single valid", "litellm-system", false},
		{"default", "default", false},
		{"comma list rejected", "ns1,ns2", true},
		{"space list rejected", "ns1 ns2", true},
		{"empty rejected", "", true},
		{"uppercase rejected", "NS", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWatchNamespace(tt.ns)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateWatchNamespace(%q) err=%v, wantErr=%v", tt.ns, err, tt.wantErr)
			}
		})
	}
}
