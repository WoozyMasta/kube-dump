package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func TestExtractRoundTrip(t *testing.T) {
	t.Parallel()

	var archiveData bytes.Buffer
	writer, err := NewWriter(&archiveData, Config{Compression: compress.Config{Algorithm: compress.Gzip}})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	data := []byte("safe archive content")
	if err := writer.Add(Entry{Path: "nested/object", Mode: 0o640, Size: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
		t.Fatalf("Writer.Add() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close() error = %v", err)
	}

	destination := t.TempDir()
	entries, err := Extract(bytes.NewReader(archiveData.Bytes()), compress.Gzip, destination, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if entries != 1 {
		t.Fatalf("Extract() entries = %d, want 1", entries)
	}
	got, err := os.ReadFile(filepath.Join(destination, "nested", "object"))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("extracted data = %q, want %q", got, data)
	}
}

func TestExtractAcceptsRootDirectoryEntry(t *testing.T) {
	t.Parallel()

	archiveData := rawArchive(t, compress.Gzip, tar.Header{
		Name:     "./",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}, nil)

	entries, err := Extract(bytes.NewReader(archiveData), compress.Gzip, t.TempDir(), ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract() rejected the root directory entry: %v", err)
	}
	if entries != 1 {
		t.Fatalf("Extract() entries = %d, want 1", entries)
	}
}

func TestExtractRejectsTraversalAndLinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header tar.Header
	}{
		{name: "traversal", header: tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}},
		{name: "absolute", header: tar.Header{Name: "/escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}},
		{name: "symlink", header: tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../../escape"}},
		{name: "hardlink", header: tar.Header{Name: "link", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "object"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archiveData := rawArchive(t, compress.Zstandard, test.header, []byte("x"))
			if _, err := Extract(bytes.NewReader(archiveData), compress.Zstandard, t.TempDir(), ExtractOptions{}); err == nil {
				t.Fatal("Extract() accepted unsafe archive entry")
			}
		})
	}
}

func TestExtractDoesNotOverwriteByDefault(t *testing.T) {
	t.Parallel()

	archiveData := rawArchive(t, compress.Gzip, tar.Header{Name: "object", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, []byte("n"))
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "object"), []byte("old"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if _, err := Extract(bytes.NewReader(archiveData), compress.Gzip, destination, ExtractOptions{}); err == nil {
		t.Fatal("Extract() overwrote an existing file")
	}
}

func TestExtractRejectsSymlinkOverwrite(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("old"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	link := filepath.Join(destination, "object")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	archiveData := rawArchive(t, compress.Gzip, tar.Header{Name: "object", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, []byte("n"))
	if _, err := Extract(bytes.NewReader(archiveData), compress.Gzip, destination, ExtractOptions{Overwrite: true}); err == nil {
		t.Fatal("Extract() followed an existing symlink during overwrite")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("outside target changed to %q", got)
	}
}

func TestExtractRegularFileRemovesPartialFile(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	root, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatalf("os.OpenRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()

	err = extractRegularFile(bytes.NewReader([]byte("partial")), root, "object", 100, 0o600, false)
	if err == nil {
		t.Fatal("extractRegularFile() accepted a truncated source")
	}
	if _, err := os.Stat(filepath.Join(destination, "object")); !os.IsNotExist(err) {
		t.Fatalf("partial extracted file still exists: %v", err)
	}
}

// rawArchive creates an intentionally low-level fixture for hostile tar
// headers that the safe Writer correctly refuses to generate.
func rawArchive(t *testing.T, algorithm compress.Algorithm, header tar.Header, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	compressor, err := compress.NewWriter(&output, compress.Config{Algorithm: algorithm})
	if err != nil {
		t.Fatalf("compress.NewWriter() error = %v", err)
	}
	tarWriter := tar.NewWriter(compressor)
	if err := tarWriter.WriteHeader(&header); err != nil {
		t.Fatalf("tar.WriteHeader() error = %v", err)
	}
	if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatalf("tar.Write() error = %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar.Close() error = %v", err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("compressor.Close() error = %v", err)
	}
	return output.Bytes()
}
