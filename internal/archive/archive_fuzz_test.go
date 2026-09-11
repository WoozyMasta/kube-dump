// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package archive

import (
	"bytes"
	"io"
	"path"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func FuzzReadEntries(f *testing.F) {
	for _, algorithm := range []compress.Algorithm{compress.Zstandard, compress.Gzip} {
		f.Add(fuzzArchiveSeed(f, algorithm), string(algorithm))
	}
	f.Add([]byte("not an archive"), "zstd")

	f.Fuzz(func(t *testing.T, data []byte, algorithm string) {
		if algorithm != string(compress.Zstandard) && algorithm != string(compress.Gzip) {
			return
		}

		shortEntry := false
		err := ReadEntries(bytes.NewReader(data), compress.Algorithm(algorithm), func(entry ReaderEntry) error {
			clean, err := validateArchivePath(entry.Path)
			if err != nil || clean != entry.Path {
				t.Fatalf("reader returned an unsafe path %q: clean=%q error=%v", entry.Path, clean, err)
			}

			if entry.Dir {
				if entry.Reader != nil || entry.Size != 0 {
					t.Fatalf("directory entry has file payload: %#v", entry)
				}

				return nil
			}
			if entry.Reader == nil || entry.Size < 0 {
				t.Fatalf("regular entry has invalid payload: %#v", entry)
			}

			read, err := io.Copy(io.Discard, entry.Reader)
			if err != nil {
				return err
			}
			if read != entry.Size {
				shortEntry = true
			}

			return nil
		})
		if err == nil && shortEntry {
			t.Fatal("ReadEntries accepted a regular entry shorter than its tar header")
		}
	})
}

func FuzzValidateArchivePath(f *testing.F) {
	for _, seed := range []string{
		".",
		"resources/object.yaml",
		"nested/../object.yaml",
		"../escape",
		"C:\\escape",
		"/absolute",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		clean, err := validateArchivePath(value)
		if err != nil {
			return
		}

		// `.` is the valid canonical name of the mounted volume root directory.
		if clean == ".." || path.IsAbs(clean) || clean != path.Clean(clean) {
			t.Fatalf("validateArchivePath(%q) returned non-canonical path %q", value, clean)
		}

		repeated, err := validateArchivePath(clean)
		if err != nil || repeated != clean {
			t.Fatalf("archive path normalization is not idempotent: %q -> %q, error=%v", clean, repeated, err)
		}
	})
}

// fuzzArchiveSeed creates a valid compressed archive for fuzzing mutations.
func fuzzArchiveSeed(t testing.TB, algorithm compress.Algorithm) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := NewWriter(&compressed, Config{Compression: compress.Config{Algorithm: algorithm}})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("apiVersion: v1\nkind: ConfigMap\n")
	if err := writer.Add(Entry{Path: "resources/object.yaml", Mode: 0o640, Size: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return compressed.Bytes()
}
