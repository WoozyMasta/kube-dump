package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func TestWriterRoundTrip(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Zstandard}})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Add(Entry{Path: "resources/", Mode: 0o755, Dir: true}); err != nil {
		t.Fatalf("Writer.Add() directory error = %v", err)
	}
	data := []byte("canonical resource")
	if err := writer.Add(Entry{Path: "resources/object.yaml", Mode: 0o644, Size: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
		t.Fatalf("Writer.Add() file error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}

	reader, err := compress.NewReader(bytes.NewReader(output.Bytes()), compress.Zstandard)
	if err != nil {
		t.Fatalf("compress.NewReader() error = %v", err)
	}
	tarReader := tar.NewReader(reader)
	header, err := tarReader.Next()
	if err != nil || header.Name != "resources/" || !header.FileInfo().IsDir() {
		t.Fatalf("first archive entry = %#v, error = %v", header, err)
	}
	header, err = tarReader.Next()
	if err != nil || header.Name != "resources/object.yaml" {
		t.Fatalf("second archive entry = %#v, error = %v", header, err)
	}
	decoded, err := io.ReadAll(tarReader)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("archive data = %q, want %q", decoded, data)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader.Close() error = %v", err)
	}
}

func TestWriterRejectsUnsafeEntry(t *testing.T) {
	t.Parallel()

	writer, err := NewWriter(io.Discard, Config{})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	defer writer.Close()
	if err := writer.Add(Entry{Path: "../escape", Mode: 0o644, Size: 1, Reader: bytes.NewReader([]byte("x"))}); err == nil {
		t.Fatal("Writer.Add() accepted a traversal path")
	}
}

func TestArchiveReadersRejectNilSource(t *testing.T) {
	if _, err := ReadMetadata(nil, compress.Zstandard); err == nil {
		t.Fatal("ReadMetadata() accepted a nil source")
	}
	if _, _, err := Verify(nil, compress.Zstandard); err == nil {
		t.Fatal("Verify() accepted a nil source")
	}
}

func TestWriterOutputIsDeterministic(t *testing.T) {
	t.Parallel()

	data := []byte("stable bytes")
	encode := func() []byte {
		var output bytes.Buffer
		writer, err := NewWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
		if err != nil {
			t.Fatalf("NewWriter() error = %v", err)
		}
		if err := writer.Add(Entry{Path: "object", Mode: 0o644, ModTime: fixedTime(), Size: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
			t.Fatalf("Writer.Add() error = %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("Writer.Close() error = %v", err)
		}
		return output.Bytes()
	}
	if !bytes.Equal(encode(), encode()) {
		t.Fatal("archive output is not deterministic")
	}
}

func TestBackupMetadataRoundTrip(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewBackupWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Zstandard}})
	if err != nil {
		t.Fatalf("NewBackupWriter() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}
	metadata, err := ReadMetadata(bytes.NewReader(output.Bytes()), compress.Zstandard)
	if err != nil {
		t.Fatalf("ReadMetadata() error = %v", err)
	}
	if metadata.Format != "kube-dump" || metadata.Version != 1 || metadata.Compression != compress.Zstandard {
		t.Fatalf("unexpected archive metadata: %#v", metadata)
	}
	if metadata.CreatedAt.IsZero() {
		t.Fatal("archive metadata has no creation time")
	}
}

func TestInspectCountsCanonicalResources(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewBackupWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
	if err != nil {
		t.Fatalf("NewBackupWriter() error = %v", err)
	}
	entries := []Entry{
		{Path: "resources/apps/v1/deployments/default/api.yaml", Size: 1, Reader: bytes.NewReader([]byte("a"))},
		{Path: "resources/apps/v1/deployments/status/api.yaml", Size: 1, Reader: bytes.NewReader([]byte("b"))},
		{Path: ".kube-dump/crypto/metadata.yaml", Size: 1, Reader: bytes.NewReader([]byte("c"))},
	}
	for _, entry := range entries {
		if err := writer.Add(entry); err != nil {
			t.Fatalf("Writer.Add() error = %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}

	report, err := Inspect(bytes.NewReader(output.Bytes()), compress.Gzip)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if report.Entries != 3 || report.Objects != 2 || report.Resources["apps/v1/deployments"] != 2 {
		t.Fatalf("Inspect() report = %#v", report)
	}
}

func TestInspectCountsPVCData(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewBackupWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
	if err != nil {
		t.Fatalf("NewBackupWriter() error = %v", err)
	}
	if err := writer.Add(Entry{
		Path: "volumes/default/database/20260902T120000Z/data.tar.zst", Size: 42,
		Reader: bytes.NewReader(make([]byte, 42)),
	}); err != nil {
		t.Fatalf("Writer.Add() error = %v", err)
	}
	if err := writer.Add(Entry{
		Path: "volumes/default/database/20260902T120000Z/metadata.yaml", Size: 1,
		Reader: bytes.NewReader([]byte("x")),
	}); err != nil {
		t.Fatalf("Writer.Add() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}

	report, err := Inspect(bytes.NewReader(output.Bytes()), compress.Gzip)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if report.PVCArtifacts != 1 || report.PVCBytes != 42 || report.Objects != 0 {
		t.Fatalf("Inspect() report = %#v", report)
	}
}

func TestVerifyReadsArchiveWithoutExtraction(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewBackupWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
	if err != nil {
		t.Fatalf("NewBackupWriter() error = %v", err)
	}
	data := []byte("resource")
	if err := writer.Add(Entry{Path: "object.yaml", Mode: 0o644, Size: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
		t.Fatalf("Writer.Add() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}
	metadata, entries, err := Verify(bytes.NewReader(output.Bytes()), compress.Gzip)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if metadata.Compression != compress.Gzip || entries != 1 {
		t.Fatalf("Verify() = %#v, %d", metadata, entries)
	}
}

func TestArchiveReadersRejectDuplicateEntries(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer, err := NewBackupWriter(&output, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
	if err != nil {
		t.Fatalf("NewBackupWriter() error = %v", err)
	}
	entry := Entry{Path: "object.yaml", Mode: 0o644, Size: 1, Reader: bytes.NewReader([]byte("x"))}
	if err := writer.Add(entry); err != nil {
		t.Fatalf("Writer.Add() first error = %v", err)
	}
	entry.Reader = bytes.NewReader([]byte("y"))
	if err := writer.Add(entry); err != nil {
		t.Fatalf("Writer.Add() second error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}
	if _, _, err := Verify(bytes.NewReader(output.Bytes()), compress.Gzip); err == nil {
		t.Fatal("Verify() accepted duplicate archive entries")
	}
	if _, err := Extract(bytes.NewReader(output.Bytes()), compress.Gzip, t.TempDir(), ExtractOptions{}); err == nil {
		t.Fatal("Extract() accepted duplicate archive entries")
	}
}

// fixedTime keeps the deterministic archive fixture independent of wall clock time.
func fixedTime() (value time.Time) {
	return time.Unix(0, 0).UTC()
}
