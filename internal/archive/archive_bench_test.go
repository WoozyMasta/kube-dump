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
	"strconv"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func BenchmarkPackDirectory(b *testing.B) {
	for _, fixture := range archiveBenchmarkFixtures() {
		root, total := createArchiveBenchmarkFixture(b, fixture.files, fixture.fileSize)

		for _, algorithm := range []compress.Algorithm{compress.Zstandard, compress.Gzip} {
			b.Run(fixture.name+"/"+string(algorithm), func(b *testing.B) {
				b.SetBytes(total)
				b.ReportAllocs()
				for b.Loop() {
					var output bytes.Buffer
					if err := PackDirectory(
						context.Background(), root, &output,
						Config{Compression: compress.Config{Algorithm: algorithm}},
					); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkReadEntries(b *testing.B) {
	for _, fixture := range archiveBenchmarkFixtures() {
		root, total := createArchiveBenchmarkFixture(b, fixture.files, fixture.fileSize)

		for _, algorithm := range []compress.Algorithm{compress.Zstandard, compress.Gzip} {
			var archiveData bytes.Buffer
			if err := PackDirectory(
				context.Background(), root, &archiveData,
				Config{Compression: compress.Config{Algorithm: algorithm}},
			); err != nil {
				b.Fatal(err)
			}
			data := append([]byte(nil), archiveData.Bytes()...)

			b.Run(fixture.name+"/"+string(algorithm), func(b *testing.B) {
				b.SetBytes(total)
				b.ReportAllocs()
				for b.Loop() {
					if err := ReadEntries(bytes.NewReader(data), algorithm, func(entry ReaderEntry) error {
						if entry.Reader == nil {
							return nil
						}

						_, err := io.Copy(io.Discard, entry.Reader)
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkExtractArchive measures the restore path for compressed resource trees.
func BenchmarkExtractArchive(b *testing.B) {
	for _, fixture := range archiveBenchmarkFixtures() {
		root, total := createArchiveBenchmarkFixture(b, fixture.files, fixture.fileSize)

		for _, algorithm := range []compress.Algorithm{compress.Zstandard, compress.Gzip} {
			var archiveData bytes.Buffer
			if err := PackDirectory(
				context.Background(), root, &archiveData,
				Config{Compression: compress.Config{Algorithm: algorithm}},
			); err != nil {
				b.Fatal(err)
			}
			data := append([]byte(nil), archiveData.Bytes()...)

			b.Run(fixture.name+"/"+string(algorithm), func(b *testing.B) {
				destination := b.TempDir()
				b.SetBytes(total)
				b.ReportAllocs()
				for b.Loop() {
					if _, err := Extract(
						bytes.NewReader(data), algorithm, destination,
						ExtractOptions{Overwrite: true},
					); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

type archiveBenchmarkFixture struct {
	name     string // name identifies the fixture dimensions in benchmark output.
	files    int    // files is the number of regular files in the generated archive tree.
	fileSize int    // fileSize is the exact size of each generated file in bytes.
}

// archiveBenchmarkFixtures returns small, medium, and large archive workloads.
func archiveBenchmarkFixtures() []archiveBenchmarkFixture {
	return []archiveBenchmarkFixture{
		{name: "small-1-file", files: 1, fileSize: 8 << 10},
		{name: "medium-8-files", files: 8, fileSize: 64 << 10},
		{name: "large-32-files", files: 32, fileSize: 1 << 20},
	}
}

// createArchiveBenchmarkFixture builds a canonical resource tree for one workload.
func createArchiveBenchmarkFixture(t testing.TB, files, fileSize int) (string, int64) {
	t.Helper()
	root := t.TempDir()
	pattern := []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  value: stable\n")
	payload := bytes.Repeat(pattern, fileSize/len(pattern)+1)
	payload = payload[:fileSize]
	var total int64

	for index := range files {
		path := filepath.Join(
			root,
			"resources",
			"core",
			"v1",
			"configmaps",
			"default",
			"object-"+strconv.Itoa(index)+".yaml",
		)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o640); err != nil {
			t.Fatal(err)
		}
		total += int64(len(payload))
	}

	return root, total
}
