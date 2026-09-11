// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package archive

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func TestPackDirectoryRoundTripVerification(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.MkdirAll(
		filepath.Join(source, "core", "v1", "configmaps", "default"),
		0o750,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, "core", "v1", "configmaps", "default", "object.yaml"),
		[]byte("kind: ConfigMap\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(
		filepath.Join(source, "apps", "v1", "deployments", "default"),
		0o750,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, "apps", "v1", "deployments", "default", "object.yaml"),
		[]byte("kind: Deployment\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(
		filepath.Join(source, ".git"),
		0o750,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, ".git", "config"),
		[]byte("private"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, "unrelated.txt"),
		[]byte("outside managed inventory"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(
		filepath.Join(source, ".kube-dump", "crypto", "keys"),
		0o750,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, ".kube-dump", "crypto", "metadata.yaml"),
		[]byte("version: 1\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(source, ".kube-dump", "crypto", "keys", "siv-test.age"),
		[]byte("encrypted key"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	var packed bytes.Buffer
	if err := PackDirectory(
		context.Background(),
		source,
		&packed,
		Config{Compression: compress.Config{Algorithm: compress.Zstandard}},
	); err != nil {
		t.Fatalf("PackDirectory() error = %v", err)
	}

	metadata, entries, err := Verify(bytes.NewReader(packed.Bytes()), compress.Zstandard)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if metadata.Compression != compress.Zstandard || entries != 14 {
		t.Fatalf("archive metadata = %#v, entries = %d", metadata, entries)
	}
}

func TestPackDirectoryRejectsSymlink(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "resources"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(source, "resources", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var packed bytes.Buffer
	if err := PackDirectory(context.Background(), source, &packed, Config{}); err == nil {
		t.Fatal("PackDirectory() accepted symlink")
	}
}

func TestReadEntriesStreamsArchiveInOrder(t *testing.T) {
	var packed bytes.Buffer
	source := t.TempDir()
	resourcePath := filepath.Join(source, "core", "v1", "configmaps", "default")
	if err := os.MkdirAll(resourcePath, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(resourcePath, "config.yaml"),
		[]byte("kind: ConfigMap\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	if err := PackDirectory(
		context.Background(), source,
		&packed,
		Config{Compression: compress.Config{Algorithm: compress.Gzip}},
	); err != nil {
		t.Fatal(err)
	}

	var paths []string
	if err := ReadEntries(bytes.NewReader(packed.Bytes()), compress.Gzip, func(entry ReaderEntry) error {
		paths = append(paths, entry.Path)
		if entry.Reader != nil {
			data, err := io.ReadAll(entry.Reader)
			if err != nil {
				return err
			}

			if int64(len(data)) != entry.Size {
				t.Fatalf("entry %q size = %d, want %d", entry.Path, len(data), entry.Size)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(paths) == 0 || paths[len(paths)-1] != "core/v1/configmaps/default/config.yaml" {
		t.Fatalf("streamed archive paths = %v", paths)
	}
}
