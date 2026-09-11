// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildManifestFromReleaseArchives(t *testing.T) {
	t.Parallel()

	buildDirectory := t.TempDir()
	archives := map[string][]byte{
		"kube-dump-linux-arm64.tar.gz":   []byte("linux arm64 archive"),
		"kube-dump-windows-amd64.tar.gz": []byte("windows amd64 archive"),
		"README.txt":                     []byte("ignored"),
	}
	for name, data := range archives {
		if err := os.WriteFile(filepath.Join(buildDirectory, name), data, 0o600); err != nil {
			t.Fatalf("write archive %q: %v", name, err)
		}
	}

	got, err := buildManifest(buildDirectory, "v2.0.0", "https://example.invalid/releases")
	if err != nil {
		t.Fatalf("buildManifest: %v", err)
	}
	if got.Spec.Version != "v2.0.0" {
		t.Fatalf("manifest version = %q", got.Spec.Version)
	}
	if len(got.Spec.Platforms) != 2 {
		t.Fatalf("platform count = %d, want 2", len(got.Spec.Platforms))
	}

	linux := got.Spec.Platforms[0]
	if linux.Selector.MatchLabels["os"] != "linux" || linux.Selector.MatchLabels["arch"] != "arm64" {
		t.Fatalf("linux selector = %+v", linux.Selector.MatchLabels)
	}
	if linux.Bin != "kube-dump-v2.0.0/kube-dump" {
		t.Fatalf("linux binary = %q", linux.Bin)
	}
	if linux.URI != "https://example.invalid/releases/v2.0.0/kube-dump-linux-arm64.tar.gz" {
		t.Fatalf("linux URI = %q", linux.URI)
	}
	linuxHash := sha256.Sum256(archives["kube-dump-linux-arm64.tar.gz"])
	if linux.SHA256 != hex.EncodeToString(linuxHash[:]) {
		t.Fatalf("linux checksum = %q", linux.SHA256)
	}

	windows := got.Spec.Platforms[1]
	if windows.Bin != "kube-dump-v2.0.0/kube-dump.exe" {
		t.Fatalf("windows binary = %q", windows.Bin)
	}
}

func TestFindReleaseArchivesRejectsEmptyDirectory(t *testing.T) {
	t.Parallel()

	_, err := findReleaseArchives(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no Krew release archives") {
		t.Fatalf("findReleaseArchives() error = %v", err)
	}
}

func TestRunAcceptsPrereleaseVersion(t *testing.T) {
	t.Parallel()

	buildDirectory := t.TempDir()
	archive := filepath.Join(buildDirectory, "kube-dump-linux-amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("linux archive"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	output := filepath.Join(t.TempDir(), "krew.yaml")

	if err := run([]string{
		"--build-dir", buildDirectory,
		"--version", "v2.0.0-rc.1",
		"--output", output,
	}, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), "version: v2.0.0-rc.1") {
		t.Fatalf("manifest does not contain prerelease version: %s", data)
	}
}

func TestBuildManifestReportsMissingArchive(t *testing.T) {
	t.Parallel()

	_, err := buildManifest(filepath.Join(t.TempDir(), "missing"), "v2.0.0", "https://example.invalid")
	if err == nil {
		t.Fatal("buildManifest() succeeded for missing build directory")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buildManifest() error = %v, want os.ErrNotExist", err)
	}
}
