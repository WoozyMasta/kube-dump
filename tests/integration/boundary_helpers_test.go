//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// boundaryProfileData is deliberately kept as a repository fixture
// so changes to the profile contract are visible in reviews alongside the integration tests.
//
//go:embed testdata/boundary-profile.yaml
var boundaryProfileData []byte

// writeBoundaryProfile writes the shared profile to a temporary path accepted by the CLI.
func writeBoundaryProfile(t *testing.T, root string) string {
	t.Helper()

	path := filepath.Join(root, "boundary-profile.yaml")
	if err := os.WriteFile(path, boundaryProfileData, 0o600); err != nil {
		t.Fatalf("write boundary profile: %v", err)
	}

	return path
}

// boundaryObjectMeta returns metadata satisfying the common profile selectors.
func boundaryObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"integration.kube-dump/profile": "boundary",
		},
		Annotations: map[string]string{
			"integration.kube-dump/selected":  "true",
			"integration.kube-dump/transient": "remove-me",
		},
	}
}
