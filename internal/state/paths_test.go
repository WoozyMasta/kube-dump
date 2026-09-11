// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package state

import "testing"

func TestPathSegmentRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
	}{
		{name: "metrics-server:system:auth-delegator", encoded: "metrics-server%3Asystem%3Aauth-delegator"},
		{name: "value%20with%percent", encoded: "value%2520with%25percent"},
		{name: "CON.txt", encoded: "%43ON.txt"},
		{name: "trailing.", encoded: "trailing%2E"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EncodePathSegment(test.name); got != test.encoded {
				t.Fatalf("EncodePathSegment() = %q, want %q", got, test.encoded)
			}

			decoded, err := DecodePathSegment(test.encoded)
			if err != nil {
				t.Fatalf("DecodePathSegment() error = %v", err)
			}
			if decoded != test.name {
				t.Fatalf("DecodePathSegment() = %q, want %q", decoded, test.name)
			}
		})
	}
}

func TestDecodePathSegmentRejectsMalformedEscape(t *testing.T) {
	if _, err := DecodePathSegment("invalid%2"); err == nil {
		t.Fatal("DecodePathSegment() accepted a malformed escape")
	}
}

func TestIsCanonicalObjectPathRejectsAlternativeEscaping(t *testing.T) {
	t.Parallel()

	if !IsCanonicalObjectPath("apps/v1/deployments/production/frontend.yaml") {
		t.Fatal("IsCanonicalObjectPath() rejected a canonical path")
	}
	for _, path := range []string{
		"apps/v1/deployments/production/%66rontend.yaml",
		"apps/v1/deployments/production/front%65nd.yaml",
		"apps/v1/deployments/production/frontend%2Eyaml.yaml",
		"apps/v1/deployments/production/frontend%2.yaml",
	} {
		if IsCanonicalObjectPath(path) {
			t.Fatalf("IsCanonicalObjectPath() accepted non-canonical path %q", path)
		}
	}
}
