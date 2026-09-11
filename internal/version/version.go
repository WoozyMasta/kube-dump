// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package version contains build metadata exposed by the CLI.
package version

import (
	"strconv"
	"strings"
)

const containerImageRepository = "ghcr.io/woozymasta/kube-dump"

var (
	// Version is the application version, normally set at build time.
	Version = "v2.0.0-dev"
	// Commit is the source revision, normally set at build time.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp, normally set at build time.
	BuildTime = "unknown"
	// URL is the project URL, normally set at build time.
	URL = "https://github.com/WoozyMasta/kube-dump"
	// ContainerImageOverride overrides the version-derived container image when set at build time.
	ContainerImageOverride string
)

// ContainerImage returns the default Kubernetes container image for the current CLI version.
// Release versions are reused as image tags;
// development and unsupported versions deliberately fall back to the moving latest tag.
func ContainerImage() string {
	if ContainerImageOverride != "" {
		return ContainerImageOverride
	}

	return containerImageRepository + ":" + ContainerImageTag(Version)
}

// ContainerImageTag converts a CLI version into a safe container-image tag.
//
// Stable and prerelease X.Y.Z versions with a major version
// of at least 2 are eligible for a versioned image.
// The leading v belongs to the Go module and release tag,
// so it is always removed from the container tag.
func ContainerImageTag(value string) string {
	value = strings.TrimPrefix(value, "v")
	parts := strings.SplitN(value, "-", 2)
	base := strings.Split(parts[0], ".")
	if len(base) != 3 {
		return "latest"
	}

	major, err := strconv.Atoi(base[0])
	if err != nil || major < 2 || !isVersionNumber(base[0]) {
		return "latest"
	}

	for _, part := range base[1:] {
		if !isVersionNumber(part) {
			return "latest"
		}
	}
	if len(parts) == 2 && !isPrerelease(parts[1]) {
		return "latest"
	}

	return value
}

// isPrerelease reports whether value contains only Docker-tag-safe prerelease identifiers.
func isPrerelease(value string) bool {
	if value == "" {
		return false
	}

	for identifier := range strings.SplitSeq(value, ".") {
		if identifier == "" {
			return false
		}

		numeric := true
		for _, char := range identifier {
			if (char < '0' || char > '9') && (char < 'a' || char > 'z') &&
				(char < 'A' || char > 'Z') && char != '-' {
				return false
			}

			if char < '0' || char > '9' {
				numeric = false
			}
		}

		if numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}

	return true
}

// isVersionNumber accepts the canonical decimal representation used by an X.Y.Z release
// and rejects signs and leading zeroes.
func isVersionNumber(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}

	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}

	return true
}
