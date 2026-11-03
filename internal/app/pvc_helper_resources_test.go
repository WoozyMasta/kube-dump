// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2021-2026 WoozyMasta
// Source: https://github.com/woozymasta/kube-dump

package app

import (
	"strings"
	"testing"
)

func TestPvcHelperOptionsResourceRequirements(t *testing.T) {
	t.Parallel()

	options := PvcHelperOptions{
		CPURequest:    "250m",
		MemoryRequest: "128Mi",
		CPULimit:      "1",
		MemoryLimit:   "512Mi",
	}

	got, err := options.ResourceRequirements()
	if err != nil {
		t.Fatalf("ResourceRequirements() error = %v", err)
	}
	if got.Requests.Cpu().String() != "250m" || got.Requests.Memory().String() != "128Mi" {
		t.Fatalf("resource requests = %#v", got.Requests)
	}
	if got.Limits.Cpu().String() != "1" || got.Limits.Memory().String() != "512Mi" {
		t.Fatalf("resource limits = %#v", got.Limits)
	}
}

func TestPvcHelperOptionsResourceRequirementsOmitUnsetValues(t *testing.T) {
	t.Parallel()

	got, err := (PvcHelperOptions{}).ResourceRequirements()
	if err != nil {
		t.Fatalf("ResourceRequirements() error = %v", err)
	}
	if len(got.Requests) != 0 || len(got.Limits) != 0 {
		t.Fatalf("unset resource requirements = %#v, want empty", got)
	}
}

func TestPvcHelperOptionsResourceRequirementsRejectInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options PvcHelperOptions
		want    string
	}{
		{
			name:    "malformed quantity",
			options: PvcHelperOptions{CPURequest: "not-a-quantity"},
			want:    "--helper-cpu-request",
		},
		{
			name:    "negative quantity",
			options: PvcHelperOptions{MemoryLimit: "-1Mi"},
			want:    "cannot be negative",
		},
		{
			name: "request above limit",
			options: PvcHelperOptions{
				CPURequest: "1500m",
				CPULimit:   "1",
			},
			want: "exceeds its limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := test.options.ResourceRequirements()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResourceRequirements() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
