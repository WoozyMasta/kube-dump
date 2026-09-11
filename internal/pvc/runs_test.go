// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"go.yaml.in/yaml/v3"
)

func TestPublishStagingDirectoryExposesOnlyCompletedTimestamp(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "volumes", "prod", "data")
	staging, timestamp, err := createStagingDirectory(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(staging), ".") {
		t.Fatalf("staging directory = %q, want hidden directory", staging)
	}
	if err := os.WriteFile(filepath.Join(staging, MetadataFilename), []byte("complete\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := publishStagingDirectory(staging, base, timestamp); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(base, timestamp, MetadataFilename)); err != nil {
		t.Fatalf("published metadata is missing: %v", err)
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != timestamp {
		t.Fatalf("artifact directory entries = %#v, want only %q", entries, timestamp)
	}
}

func TestNewRevisionIsUniqueAndTimestampCompatible(t *testing.T) {
	first, err := NewRevision()
	if err != nil {
		t.Fatal(err)
	}

	second, err := NewRevision()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("NewRevision() returned duplicate revisions %q", first)
	}
	if !IsTimestamp(first) || !IsTimestamp(second) {
		t.Fatalf("NewRevision() returned invalid revisions %q and %q", first, second)
	}
}

func TestRotateArtifactsKeepsLatestPerPVC(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "volumes", "prod", "data", "20260901T120000Z"),
		filepath.Join(root, "volumes", "prod", "data", "20260901T130000Z"),
		filepath.Join(root, "volumes", "prod", "data", "20260901T140000Z"),
		filepath.Join(root, "volumes", "other", "data", "20260901T120000Z"),
		filepath.Join(root, "volumes", "other", "data", "20260901T130000Z"),
	}
	for _, directory := range paths {
		writeCompleteArtifact(t, directory)
	}

	incomplete := filepath.Join(root, "volumes", "prod", "data", "20260901T150000Z")
	if err := os.MkdirAll(incomplete, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, MetadataFilename), []byte("incomplete"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}

	removed, err := RotateArtifacts(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed artifacts = %d, want 1", removed)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest artifact still exists, error = %v", err)
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatalf("retained artifact is missing: %v", err)
	}
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("retained artifact is missing: %v", err)
	}
	if _, err := os.Stat(paths[3]); err != nil {
		t.Fatalf("other PVC artifact was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
	if _, err := os.Stat(incomplete); err != nil {
		t.Fatalf("incomplete artifact was removed or counted: %v", err)
	}
}

// writeCompleteArtifact creates the smallest valid artifact used by retention tests.
func writeCompleteArtifact(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}

	data := []byte("data")
	metadata := NewMetadata(Pod, string(compress.Gzip))
	metadata.ContentSHA256 = hex.EncodeToString(make([]byte, 32))
	metadata.SizeBytes = int64(len(data))
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, MetadataFilename), metadataData, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, DataFilename(string(compress.Gzip), false)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// createTimestampDirectoryAt creates deterministic test fixtures
// without keeping the legacy production helper in the artifact implementation.
func createTimestampDirectoryAt(base, timestamp string) (string, error) {
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", err
	}

	directory := filepath.Join(base, timestamp)
	if err := os.Mkdir(directory, 0o750); err != nil {
		return "", err
	}

	return directory, nil
}

func TestRotateArtifactsWithZeroKeepsAll(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "volumes", "prod", "data", "20260901T120000Z"),
		filepath.Join(root, "volumes", "prod", "data", "20260901T130000Z"),
	}
	for _, directory := range paths {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := RotateArtifacts(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed artifacts = %d, want 0", removed)
	}
	for _, directory := range paths {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("artifact %q was removed: %v", directory, err)
		}
	}
}

func TestIsTimestamp(t *testing.T) {
	for _, value := range []string{
		"20260901T120000Z",
		"20260901T120000Z-01",
		"20260901T120000Z-0123456789abcdef",
		"20260901T120000.000000001Z-0123456789abcdef",
	} {
		if !IsTimestamp(value) {
			t.Errorf("IsTimestamp(%q) = false", value)
		}
	}
	for _, value := range []string{
		"20260901T120000.000000000Z",
		"20260901T120000.000000001Z",
		"20260901T120000.000000001Z-01",
		"20260901T120000.000000001Z-not-hex",
		"20260901T120000Z-00",
		"notes",
	} {
		if IsTimestamp(value) {
			t.Errorf("IsTimestamp(%q) = true", value)
		}
	}
}

func TestCompareRevisionsUsesNanosecondsBeforeSuffix(t *testing.T) {
	older := "20260901T120000.000000001Z-ffffffffffffffff"
	newer := "20260901T120000.000000002Z-0000000000000000"
	if CompareRevisions(older, newer) >= 0 {
		t.Fatalf("CompareRevisions(%q, %q) did not order capture time", older, newer)
	}
	if CompareRevisions(newer, older) <= 0 {
		t.Fatalf("CompareRevisions(%q, %q) did not order capture time", newer, older)
	}
}

func TestCompareRevisionsUsesNameAsTieBreaker(t *testing.T) {
	left := "20260901T120000.000000001Z-0000000000000001"
	right := "20260901T120000.000000001Z-0000000000000002"
	if CompareRevisions(left, right) >= 0 {
		t.Fatalf("CompareRevisions(%q, %q) did not use the revision name tie-breaker", left, right)
	}
}
