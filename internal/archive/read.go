// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

// ReaderEntry describes one validated archive entry while it is being read.
// The callback must consume Reader completely before returning;
// only the current entry is kept available,
// so callers can process large archives without extracting them or retaining all entry data.
type ReaderEntry struct {
	// Reader supplies the current regular file contents.
	Reader io.Reader
	// Path is the validated slash-separated archive path.
	Path string
	// Size is the exact number of bytes in Reader.
	Size int64
	// Dir reports whether the entry is a directory.
	Dir bool
}

// ReadEntries streams validated entries from a compressed archive.
// It rejects duplicate paths and unsupported tar entry types.
// The callback runs in archive order and must read at most Size bytes from regular files.
func ReadEntries(source io.Reader, algorithm compress.Algorithm, callback func(ReaderEntry) error) error {
	if source == nil || callback == nil {
		return errors.New("archive source and callback are required")
	}

	reader, err := compress.NewReader(source, algorithm)
	if err != nil {
		return fmt.Errorf("open archive compressor: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tarReader := tar.NewReader(reader)
	seen := make(map[string]struct{})
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}

		name, err := validateArchivePath(header.Name)
		if err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate archive entry %q", name)
		}

		seen[name] = struct{}{}
		entry := ReaderEntry{
			Path: name,
			Size: header.Size,
			Dir:  header.Typeflag == tar.TypeDir,
		}

		switch header.Typeflag {
		case tar.TypeDir:
			entry.Size = 0

		case tar.TypeReg:
			if header.Size < 0 {
				return fmt.Errorf("archive entry %q has a negative size", name)
			}
			entry.Reader = io.LimitReader(tarReader, header.Size)

		default:
			return fmt.Errorf("archive entry %q has unsupported type %d", name, header.Typeflag)
		}

		if err := callback(entry); err != nil {
			return err
		}

		// A callback may inspect only part of a file.
		// Drain the bounded reader before advancing tarReder
		// so the next header starts at a valid offset.
		if entry.Reader != nil {
			if _, err := io.Copy(io.Discard, entry.Reader); err != nil {
				return fmt.Errorf("consume archive entry %q: %w", name, err)
			}
		}
	}
}
