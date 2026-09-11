// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package version

import "testing"

func TestContainerImageTag(t *testing.T) {
	tests := map[string]string{
		"2.0.0":         "2.0.0",
		"v2.7.14":       "2.7.14",
		"v2.0.0-rc.1":   "2.0.0-rc.1",
		"2.0.0-beta.12": "2.0.0-beta.12",
		"1.99.99":       "latest",
		"v1.2.3":        "latest",
		"v0.0.0-dev":    "latest",
		"dev":           "latest",
		"2.0":           "latest",
		"2.0.0-":        "latest",
		"2.0.0-rc.01":   "latest",
		"2.0.0-rc..1":   "latest",
		"2.0.0+build.1": "latest",
		"2.0.x":         "latest",
		"2..1":          "latest",
		"+2.0.0":        "latest",
		"02.0.0":        "latest",
		"2.00.0":        "latest",
		"":              "latest",
	}

	for input, want := range tests {
		if got := ContainerImageTag(input); got != want {
			t.Errorf("ContainerImageTag(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestContainerImage(t *testing.T) {
	original := Version
	originalContainerImage := ContainerImageOverride
	t.Cleanup(func() {
		Version = original
		ContainerImageOverride = originalContainerImage
	})

	Version = "v2.4.1"
	if got, want := ContainerImage(), "ghcr.io/woozymasta/kube-dump:2.4.1"; got != want {
		t.Fatalf("ContainerImage() = %q, want %q", got, want)
	}

	Version = "v2.0.0-rc.1"
	if got, want := ContainerImage(), "ghcr.io/woozymasta/kube-dump:2.0.0-rc.1"; got != want {
		t.Fatalf("ContainerImage() prerelease = %q, want %q", got, want)
	}

	ContainerImageOverride = "registry.example/kube-dump:custom"
	if got := ContainerImage(); got != ContainerImageOverride {
		t.Fatalf("ContainerImage() with override = %q, want %q", got, ContainerImageOverride)
	}
}
