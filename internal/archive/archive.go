// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package archive writes streaming tar archives with the project compression
// codecs.
package archive

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/state"
)

const (
	metadataPath    = ".kube-dump/archive.json"
	metadataVersion = 1
)

// Entry describes one regular file or directory in an archive.
// Directories must not carry a reader or size;
// regular files must provide a reader whenever Size is non-zero.
// Paths are validated as relative archive paths before they reach the tar writer.
type Entry struct {
	// ModTime is the source modification time preserved in the archive header.
	ModTime time.Time
	// Reader provides the entry contents; directories leave it nil.
	Reader io.Reader
	// Path is the slash-separated archive-relative entry name.
	Path string
	// Mode contains the portable permission bits for the entry.
	Mode int64
	// Size is the number of content bytes for regular files.
	Size int64
	// Dir reports whether the entry represents a directory.
	Dir bool
}

// Config controls archive compression.
// The selected algorithm is also written into backup metadata
// so readers do not need to infer it from a filename.
type Config struct {
	// Progress receives the number of uncompressed source bytes read.
	// It is presentation-only and is never required for archive creation.
	Progress func(int64)
	// Compression controls the codec used for the outer archive stream.
	Compression compress.Config
}

// Metadata identifies the archive format, schema version, and compression used for the stream.
// It is the first entry in every backup archive.
type Metadata struct {
	// CreatedAt is the UTC time when the archive stream was created.
	CreatedAt time.Time `json:"createdAt"`
	// Format identifies the archive container format.
	Format string `json:"format"`
	// Compression identifies the codec used for the archive payload.
	Compression compress.Algorithm `json:"compression"`
	// Version is the metadata schema version.
	Version int `json:"version"`
}

// Report summarizes an archive without extracting or decoding Kubernetes objects.
// Resource counts are keyed by group/version/resource and include only canonical YAML object entries;
// archive metadata and cryptographic support files are excluded.
type Report struct {
	// Resources counts objects by group/version/resource.
	Resources map[string]int
	// Metadata contains the archive format, compression, and creation time.
	Metadata Metadata
	// Entries is the number of non-metadata archive entries.
	Entries int
	// Objects is the number of canonical Kubernetes YAML files.
	Objects int
	// PVCArtifacts is the number of PVC data payloads in the archive.
	PVCArtifacts int
	// PVCBytes is the total stored size of PVC data payloads.
	PVCBytes int64
}

// Writer streams tar entries through the configured compressor.
type Writer struct {
	// tar writes archive entries to the compressed stream.
	tar *tar.Writer
	// compression owns the codec and flushes its trailer on close.
	compression io.WriteCloser
	// closed prevents writes after the archive trailer has been finalized.
	closed bool
}

// NewWriter creates a compressed streaming archive writer.
// The caller owns the destination and must close the returned writer
// to flush tar and compressor trailers in the correct order.
func NewWriter(destination io.Writer, config Config) (*Writer, error) {
	if destination == nil {
		return nil, errors.New("archive destination is required")
	}

	compressionWriter, err := compress.NewWriter(destination, config.Compression)
	if err != nil {
		return nil, fmt.Errorf("create archive compressor: %w", err)
	}

	return &Writer{
		tar:         tar.NewWriter(compressionWriter),
		compression: compressionWriter,
	}, nil
}

// NewBackupWriter creates a writer with the required versioned metadata entry.
// If metadata creation fails, the partially initialized writer is closed before the error is returned.
func NewBackupWriter(destination io.Writer, config Config) (*Writer, error) {
	writer, err := NewWriter(destination, config)
	if err != nil {
		return nil, err
	}

	algorithm := config.Compression.Algorithm
	if algorithm == "" {
		algorithm = compress.DefaultAlgorithm
	}

	metadata, err := json.Marshal(Metadata{
		Format: "kube-dump", Version: metadataVersion, Compression: algorithm,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("marshal archive metadata: %w", err)
	}

	if err := writer.Add(Entry{
		Path:   metadataPath,
		Mode:   0o644,
		Size:   int64(len(metadata)),
		Reader: bytes.NewReader(metadata),
	}); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("write archive metadata: %w", err)
	}

	return writer, nil
}

// Inspect reads and validates every archive entry and returns a resource summary.
// It consumes the supplied stream once and never extracts files to disk.
func Inspect(source io.Reader, algorithm compress.Algorithm) (Report, error) {
	if source == nil {
		return Report{}, errors.New("archive source is required")
	}

	reader, err := compress.NewReader(source, algorithm)
	if err != nil {
		return Report{}, fmt.Errorf("open archive compressor: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tarReader := tar.NewReader(reader)
	metadata, err := readMetadata(tarReader)
	if err != nil {
		return Report{}, err
	}

	report := Report{Metadata: metadata, Resources: make(map[string]int)}
	seen := map[string]struct{}{metadataPath: {}}

	// Inspection validates the complete stream, including entries that are not counted as resources.
	// A valid summary must never hide a malformed archive member encountered after the first useful object.
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return report, nil
		}
		if nextErr != nil {
			return Report{}, fmt.Errorf("read archive entry: %w", nextErr)
		}

		name, err := validateArchivePath(header.Name)
		if err != nil {
			return Report{}, err
		}
		if _, found := seen[name]; found {
			return Report{}, fmt.Errorf("duplicate archive entry %q", name)
		}

		seen[name] = struct{}{}
		if err := verifyEntry(tarReader, header); err != nil {
			return Report{}, err
		}

		report.Entries++

		if header.Typeflag != tar.TypeReg {
			continue
		}
		if isPVCDataPath(name) {
			report.PVCArtifacts++
			report.PVCBytes += header.Size
			continue
		}
		if strings.HasPrefix(name, "volumes/") {
			continue
		}
		resourcePath, ok := strings.CutPrefix(name, "resources/")
		if !ok {
			continue
		}
		if !state.IsCanonicalObjectPath(resourcePath) {
			continue
		}

		parts := strings.Split(resourcePath, "/")
		resource := strings.Join(parts[:3], "/")
		report.Objects++
		report.Resources[resource]++
	}
}

// isPVCDataPath identifies stored PVC payloads without opening their contents.
func isPVCDataPath(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 5 ||
		parts[0] != "volumes" ||
		parts[1] == "" ||
		parts[2] == "" ||
		parts[3] == "" {
		return false
	}

	return parts[4] == "data.tar.gz" ||
		parts[4] == "data.tar.gz.age" ||
		parts[4] == "data.tar.zst" ||
		parts[4] == "data.tar.zst.age" ||
		parts[4] == "snapshot.json"
}

// ReadMetadata reads and validates the first metadata entry of an archive.
// Only the metadata entry is consumed;
// callers that need full integrity verification should use Verify instead.
func ReadMetadata(source io.Reader, algorithm compress.Algorithm) (Metadata, error) {
	if source == nil {
		return Metadata{}, errors.New("archive source is required")
	}

	reader, err := compress.NewReader(source, algorithm)
	if err != nil {
		return Metadata{}, fmt.Errorf("open archive compressor: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tarReader := tar.NewReader(reader)
	return readMetadata(tarReader)
}

// Verify reads every archive entry without writing extracted data.
// It validates path safety, supported tar types, file sizes,
// and complete payload reads while keeping extraction side-effect free.
func Verify(source io.Reader, algorithm compress.Algorithm) (Metadata, int, error) {
	if source == nil {
		return Metadata{}, 0, errors.New("archive source is required")
	}

	reader, err := compress.NewReader(source, algorithm)
	if err != nil {
		return Metadata{}, 0, fmt.Errorf("open archive compressor: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tarReader := tar.NewReader(reader)
	metadata, err := readMetadata(tarReader)
	if err != nil {
		return Metadata{}, 0, err
	}

	entries := 0
	seen := map[string]struct{}{metadataPath: {}}
	for {
		// Reading to EOF is required even when the metadata is valid:
		// malformed trailing entries and truncated payloads must be reported by verify.
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return metadata, entries, nil
		}
		if err != nil {
			return Metadata{}, entries, fmt.Errorf("read archive entry: %w", err)
		}

		name, err := validateArchivePath(header.Name)
		if err != nil {
			return Metadata{}, entries, err
		}
		if _, found := seen[name]; found {
			return Metadata{}, entries, fmt.Errorf("duplicate archive entry %q", name)
		}

		seen[name] = struct{}{}
		if err := verifyEntry(tarReader, header); err != nil {
			return Metadata{}, entries, err
		}

		entries++
	}
}

// readMetadata consumes and validates the mandatory first archive entry.
func readMetadata(reader *tar.Reader) (Metadata, error) {
	header, err := reader.Next()
	if err != nil {
		return Metadata{}, fmt.Errorf("read archive metadata header: %w", err)
	}

	if header.Name != metadataPath || header.Typeflag != tar.TypeReg || header.Size > 1<<20 {
		return Metadata{}, errors.New("archive metadata entry is missing or invalid")
	}

	data, err := io.ReadAll(io.LimitReader(reader, header.Size))
	if err != nil {
		return Metadata{}, fmt.Errorf("read archive metadata: %w", err)
	}

	return decodeMetadata(data)
}

// verifyEntry validates one non-metadata entry and consumes its payload.
func verifyEntry(reader *tar.Reader, header *tar.Header) error {
	name, err := validateArchivePath(header.Name)
	if err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		// Directory entries have no payload,
		// but their names still need validation to prevent traversal hidden in directory headers.
		return nil

	case tar.TypeReg:
		if header.Size < 0 {
			return fmt.Errorf("archive file %q has negative size", name)
		}

		read, err := io.Copy(io.Discard, io.LimitReader(reader, header.Size))
		if err != nil {
			return fmt.Errorf("read archive file %q: %w", name, err)
		}

		if read != header.Size {
			return fmt.Errorf("read archive file %q: read %d bytes, want %d", name, read, header.Size)
		}

		return nil

	default:
		return fmt.Errorf("archive entry %q has unsupported type %d", name, header.Typeflag)
	}
}

// decodeMetadata validates the fixed archive metadata contract.
func decodeMetadata(data []byte) (Metadata, error) {
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode archive metadata: %w", err)
	}

	if metadata.Format != "kube-dump" || metadata.Version != metadataVersion {
		return Metadata{}, fmt.Errorf("unsupported archive metadata %q v%d", metadata.Format, metadata.Version)
	}

	if err := (compress.Config{Algorithm: metadata.Compression}).Validate(); err != nil {
		return Metadata{}, fmt.Errorf("validate archive metadata compression: %w", err)
	}

	return metadata, nil
}

// Add appends one regular file or directory to the archive.
// For regular files it copies exactly Entry.Size bytes,
// preventing a short source read from silently producing a truncated but apparently valid tar entry.
func (w *Writer) Add(entry Entry) error {
	if w == nil || w.closed {
		return errors.New("archive writer is closed")
	}

	if err := validateEntry(entry); err != nil {
		return err
	}

	header := &tar.Header{
		Name:    path.Clean(entry.Path),
		Mode:    entry.Mode,
		ModTime: entry.ModTime.UTC(),
	}

	if entry.Dir {
		header.Typeflag = tar.TypeDir
		header.Name = strings.TrimSuffix(header.Name, "/") + "/"
	} else {
		header.Typeflag = tar.TypeReg
		header.Size = entry.Size
	}

	if err := w.tar.WriteHeader(header); err != nil {
		return fmt.Errorf("write archive header %q: %w", entry.Path, err)
	}
	if entry.Dir || entry.Size == 0 {
		return nil
	}

	written, err := io.CopyN(w.tar, entry.Reader, entry.Size)
	if err != nil {
		return fmt.Errorf("write archive entry %q: %w", entry.Path, err)
	}
	if written != entry.Size {
		return fmt.Errorf("write archive entry %q: wrote %d bytes, want %d", entry.Path, written, entry.Size)
	}

	return nil
}

// Close flushes tar and compression frames in the required order.
// Close is idempotent so deferred cleanup can safely run after an earlier close error.
func (w *Writer) Close() error {
	if w == nil || w.closed {
		return nil
	}

	w.closed = true
	if err := w.tar.Close(); err != nil {
		_ = w.compression.Close()
		return fmt.Errorf("close tar archive: %w", err)
	}
	if err := w.compression.Close(); err != nil {
		return fmt.Errorf("close archive compressor: %w", err)
	}

	return nil
}

// validateEntry prevents archive writers from generating ambiguous
// or unsafe paths that later extraction code would have to reinterpret.
// It also enforces the data/metadata invariant expected by Add.
func validateEntry(entry Entry) error {
	if err := validateEntryPath(entry.Path); err != nil {
		return err
	}

	if entry.Mode < 0 {
		return fmt.Errorf("archive entry %q has negative mode", entry.Path)
	}

	if entry.Dir {
		// A directory is represented only by its tar header.
		// Accepting a reader here would make the archive contract
		// dependent on tar implementation details and could hide a caller bug.
		if entry.Size != 0 || entry.Reader != nil {
			return fmt.Errorf("directory entry %q must not have data", entry.Path)
		}
		return nil
	}

	if entry.Size < 0 {
		return fmt.Errorf("archive entry %q has negative size", entry.Path)
	}
	if entry.Size > 0 && entry.Reader == nil {
		return fmt.Errorf("archive entry %q requires a reader", entry.Path)
	}

	return nil
}

// validateEntryPath rejects archive names that could escape the archive root.
func validateEntryPath(value string) error {
	if value == "" {
		return errors.New("archive entry path is required")
	}

	if strings.Contains(value, "\\") {
		return fmt.Errorf("archive entry path contains '\\': %q", value)
	}

	clean := path.Clean(value)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("archive entry path is not relative and safe: %q", value)
	}

	return nil
}
