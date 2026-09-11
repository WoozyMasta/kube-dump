// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"go.yaml.in/yaml/v3"
)

type testBackupStrategy struct{}

func (testBackupStrategy) Backup(_ context.Context, _ Ref, dst io.Writer) (Metadata, error) {
	content := []byte("compressed stream")
	if _, err := dst.Write(content); err != nil {
		return Metadata{}, err
	}

	hash := sha256.Sum256(content)
	return Metadata{
		Version: metadataVersion, Strategy: Pod,
		Compression: string(compress.Zstandard), Portable: true,
		SizeBytes: int64(len(content)), ContentSHA256: hex.EncodeToString(hash[:]),
	}, nil
}

type encryptedTestBackupStrategy struct{}

func (encryptedTestBackupStrategy) Backup(_ context.Context, _ Ref, dst io.Writer) (Metadata, error) {
	content := []byte("encrypted compressed stream")
	if _, err := dst.Write(content); err != nil {
		return Metadata{}, err
	}

	hash := sha256.Sum256(content)
	return Metadata{
		Version: metadataVersion, Strategy: Pod,
		Compression: string(compress.Gzip), Portable: true, Encrypted: true,
		SizeBytes: int64(len(content)), ContentSHA256: hex.EncodeToString(hash[:]),
	}, nil
}

func TestVolumeStoreWriteAndOpenData(t *testing.T) {
	t.Parallel()
	store, err := NewVolumeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	pvc := Ref{Namespace: "prod-app", Name: "data"}
	if _, err := store.Write(context.Background(), pvc, testBackupStrategy{}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(artifacts))
	}

	artifact := artifacts[0]
	metadata, err := store.ReadMetadata(artifact)
	if err != nil {
		t.Fatalf("ReadMetadata() error = %v", err)
	}
	if metadata.Strategy != Pod {
		t.Fatalf("metadata strategy = %q", metadata.Strategy)
	}

	data, err := store.OpenData(artifact)
	if err != nil {
		t.Fatalf("OpenData() error = %v", err)
	}
	defer data.Close()

	content, err := io.ReadAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("compressed stream")) {
		t.Fatalf("stored data = %q", content)
	}
}

func TestVolumeStoreReadMetadataRejectsOversizedFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewVolumeStore(root)
	if err != nil {
		t.Fatal(err)
	}

	claim := Ref{Namespace: "default", Name: "data"}
	revision := "20260909T010203Z"
	directory := filepath.Join(store.directoryPath(claim), revision)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, MetadataFilename),
		bytes.Repeat([]byte("x"), maxPVCMetadataBytes+1),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	_, err = store.ReadMetadata(Artifact{PVC: claim, DirectoryPath: directory})
	if err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
		t.Fatalf("ReadMetadata() error = %v, want size-limit error", err)
	}
}

func TestVolumeStoreKeepsMultipleArtifactsPerPVC(t *testing.T) {
	t.Parallel()

	store, err := NewVolumeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claim := Ref{Namespace: "prod-app", Name: "data"}

	for range 2 {
		if _, err := store.Write(context.Background(), claim, testBackupStrategy{}); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifact count = %d, want 2", len(artifacts))
	}
	if artifacts[0].DirectoryPath == artifacts[1].DirectoryPath {
		t.Fatalf("artifacts share directory %q", artifacts[0].DirectoryPath)
	}
	for _, artifact := range artifacts {
		if filepath.Dir(filepath.Dir(artifact.DirectoryPath)) != filepath.Join(store.root, "volumes", "prod-app") {
			t.Fatalf("artifact directory has unexpected layout: %q", artifact.DirectoryPath)
		}
	}
}

func TestVolumeStoreUsesSafePortablePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewVolumeStore(root)
	if err != nil {
		t.Fatal(err)
	}

	directory, err := store.directory(Ref{Namespace: "con", Name: "prn"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("artifact directory unexpectedly exists, error = %v", err)
	}
	if got := filepath.Base(directory); got != "%70rn" {
		t.Fatalf("portable reserved segment = %q", got)
	}

	readable, err := store.directory(Ref{Namespace: "prod-app", Name: "data"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "volumes", "prod-app", "data")
	if readable != want {
		t.Fatalf("readable artifact directory = %q, want %q", readable, want)
	}
	if got := state.EncodePathSegment("a.b"); got != "a.b" {
		t.Fatalf("encoded segment = %q", got)
	}
}

func TestVolumeStoreAddsAgeSuffixToEncryptedData(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewVolumeStore(root)
	if err != nil {
		t.Fatal(err)
	}

	pvc := Ref{Namespace: "prod-app", Name: "encrypted"}
	if _, err := store.Write(context.Background(), pvc, encryptedTestBackupStrategy{}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(artifacts))
	}
	if _, err := os.Stat(artifacts[0].DataPath); err != nil {
		t.Fatalf("encrypted PVC data path is missing: %v", err)
	}
}

func TestVolumeStoreFindArtifactSelectsLatestOrRevision(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewVolumeStore(root)
	if err != nil {
		t.Fatal(err)
	}

	claim := Ref{Namespace: "prod", Name: "data"}
	for _, timestamp := range []string{
		"20260904T010000Z",
		"20260904T020000Z",
		"20260904T020000.000000001Z-0000000000000000",
	} {
		directory, err := createTimestampDirectoryAt(store.directoryPath(claim), timestamp)
		if err != nil {
			t.Fatal(err)
		}
		metadata := NewMetadata(Pod, string(compress.Gzip))
		metadata.ContentSHA256 = hex.EncodeToString(make([]byte, 32))
		metadataData, err := yaml.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, MetadataFilename), metadataData, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(
			directory,
			DataFilename(string(compress.Gzip), false)),
			[]byte("data"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	corruptDirectory, err := createTimestampDirectoryAt(store.directoryPath(claim), "20260904T030000Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDirectory, MetadataFilename), []byte("broken"), 0o640); err != nil {
		t.Fatal(err)
	}

	latest, err := store.FindArtifact(claim, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(latest.DirectoryPath) != "20260904T020000.000000001Z-0000000000000000" {
		t.Fatalf("latest artifact = %q", latest.DirectoryPath)
	}
	selected, err := store.FindArtifact(claim, "20260904T010000Z")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(selected.DirectoryPath) != "20260904T010000Z" {
		t.Fatalf("selected artifact = %q", selected.DirectoryPath)
	}
	if _, err := store.FindArtifact(claim, "20260904T030000Z"); err == nil || !strings.Contains(err.Error(), "20260904T030000Z") {
		t.Fatalf("FindArtifact() corrupt exact revision error = %v", err)
	}
}

func TestVolumeStoreFindArtifactIgnoresUnrelatedCorruption(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewVolumeStore(root)
	if err != nil {
		t.Fatal(err)
	}

	target := Ref{Namespace: "prod", Name: "target"}
	if _, err := store.Write(context.Background(), target, testBackupStrategy{}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	broken := filepath.Join(
		store.directoryPath(Ref{Namespace: "other", Name: "broken"}),
		"20260904T010000Z",
	)
	if err := os.MkdirAll(broken, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, MetadataFilename), []byte("not metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	artifact, err := store.FindArtifact(target, "latest")
	if err != nil {
		t.Fatalf("FindArtifact() error = %v", err)
	}
	if artifact.PVC != target {
		t.Fatalf("FindArtifact() PVC = %#v, want %#v", artifact.PVC, target)
	}
}

func TestVolumeStorePublishesArtifactWhenCleanupWarns(t *testing.T) {
	t.Parallel()

	store, err := NewVolumeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	claim := Ref{Namespace: "default", Name: "cleanup-warning"}
	metadata, err := store.Write(context.Background(), claim, cleanupWarningStrategy{})
	if err == nil || !IsCleanupOnly(err) {
		t.Fatalf("Write() error = %v, want cleanup-only warning", err)
	}
	if metadata.Strategy != Pod {
		t.Fatalf("Write() metadata strategy = %q, want %q", metadata.Strategy, Pod)
	}

	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(artifacts))
	}

	data, err := store.OpenData(artifacts[0])
	if err != nil {
		t.Fatalf("OpenData() error = %v, want published data", err)
	}
	defer data.Close()
	content, err := io.ReadAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("cleanup warning stream")) {
		t.Fatalf("stored data = %q", content)
	}
}

type cleanupWarningStrategy struct{}

func (cleanupWarningStrategy) Backup(_ context.Context, _ Ref, dst io.Writer) (Metadata, error) {
	content := []byte("cleanup warning stream")
	if _, err := dst.Write(content); err != nil {
		return Metadata{}, err
	}

	hash := sha256.Sum256(content)
	return Metadata{
		Version:       metadataVersion,
		Strategy:      Pod,
		Compression:   string(compress.Gzip),
		Portable:      true,
		SizeBytes:     int64(len(content)),
		ContentSHA256: hex.EncodeToString(hash[:]),
	}, &CleanupWarning{
		Resource: "helper Pod default/helper",
		Err:      errors.New("forbidden"),
	}
}
