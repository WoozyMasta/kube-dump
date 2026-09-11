// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
)

const (
	maxVolumeArchiveEntries              = 1_000_000
	tarEntryDirectory       tarEntryType = iota + 1
	tarEntryRegular
	tarEntrySymlink
)

// ArchiveStats describes the decoded bytes consumed from a PVC archive.
type ArchiveStats struct {
	// ContentSHA256 is the SHA-256 digest of the complete decoded tar stream.
	ContentSHA256 string
	// SizeBytes is the complete decoded tar stream size.
	SizeBytes int64
}

// archiveStatsReader hashes and counts every encoded byte consumed by tar.Reader.
type archiveStatsReader struct {
	reader io.Reader
	hash   hash.Hash
	size   int64
}

type tarEntryType uint8

// ValidateVolumeArchive reads and validates a complete decoded PVC archive.
// It does not touch a destination volume and retains no archive payload in memory.
func ValidateVolumeArchive(ctx context.Context, src io.Reader) (ArchiveStats, error) {
	if ctx == nil || src == nil {
		return ArchiveStats{}, errors.New("volume archive validation context and source are required")
	}

	entries := make(map[string]tarEntryType)
	descendants := make(map[string]bool)
	rootSeen := false
	statsReader := &archiveStatsReader{
		reader: &contextReader{ctx: ctx, reader: src},
		hash:   sha256.New(),
	}
	reader := tar.NewReader(statsReader)

	for {
		if err := ctx.Err(); err != nil {
			return ArchiveStats{}, err
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if !rootSeen {
				return ArchiveStats{}, errors.New("volume archive is missing the root metadata record")
			}
			return ArchiveStats{
				SizeBytes:     statsReader.size,
				ContentSHA256: hex.EncodeToString(statsReader.hash.Sum(nil)),
			}, nil
		}
		if err != nil {
			return ArchiveStats{}, fmt.Errorf("read volume archive: %w", err)
		}
		if err := validateArchiveMetadata(header); err != nil {
			return ArchiveStats{}, err
		}

		name, entryType, err := validateArchiveHeader(header)
		if err != nil {
			return ArchiveStats{}, err
		}
		if !rootSeen {
			if name != "." {
				return ArchiveStats{}, errors.New("volume archive is missing the root metadata record")
			}
			rootSeen = true
		} else if name == "." {
			return ArchiveStats{}, errors.New("volume archive contains duplicate root metadata records")
		}
		if _, exists := entries[name]; exists {
			return ArchiveStats{}, fmt.Errorf("volume archive contains duplicate path %q", name)
		}

		if len(entries) >= maxVolumeArchiveEntries {
			return ArchiveStats{}, fmt.Errorf(
				"volume archive contains more than %d entries",
				maxVolumeArchiveEntries,
			)
		}

		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if parentType, exists := entries[parent]; exists && parentType != tarEntryDirectory {
				return ArchiveStats{}, fmt.Errorf("volume archive path %q traverses non-directory %q", name, parent)
			}
		}
		if entryType != tarEntryDirectory && descendants[name] {
			return ArchiveStats{}, fmt.Errorf("volume archive path %q conflicts with a child path", name)
		}

		entries[name] = entryType
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			descendants[parent] = true
		}

		if entryType != tarEntryRegular {
			continue
		}

		written, err := consumeArchivePayload(reader, header.Size)
		if err != nil {
			return ArchiveStats{}, fmt.Errorf("read volume archive payload %q: %w", name, err)
		}
		if written != header.Size {
			return ArchiveStats{}, fmt.Errorf("volume archive payload %q has %d bytes, want %d", name, written, header.Size)
		}
	}
}

// consumeArchivePayload drains one tar entry through a fixed-size buffer.
// The archive may legitimately contain large PVC files,
// so validation must stream the payload without imposing an artificial total-size limit.
func consumeArchivePayload(reader io.Reader, expected int64) (int64, error) {
	buffer := make([]byte, 32<<10)
	var written int64
	for remaining := expected; remaining > 0; {
		chunk := min(int64(len(buffer)), remaining)
		read, err := io.ReadFull(reader, buffer[:chunk])
		written += int64(read)
		if err != nil {
			return written, err
		}

		remaining -= int64(read)
	}

	return written, nil
}

// Read forwards archive bytes and records the exact encoded stream consumed.
func (r *archiveStatsReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.size += int64(n)
	}

	return n, err
}

// validateArchiveHeader checks one tar header without consuming its payload.
func validateArchiveHeader(header *tar.Header) (string, tarEntryType, error) {
	if header.Name == rootArchiveName {
		if header.PAXRecords[rootMetadataPAXKey] != rootMetadataPAXValue {
			return "", 0, errors.New("volume archive root metadata record is invalid")
		}
		if header.Typeflag != tar.TypeDir || header.Size != 0 {
			return "", 0, errors.New("volume archive root metadata record must be a directory")
		}
		if _, err := archiveFileMode(header.Mode); err != nil {
			return "", 0, err
		}

		return ".", tarEntryDirectory, nil
	}
	if header.PAXRecords[rootMetadataPAXKey] != "" {
		return "", 0, fmt.Errorf("volume archive root metadata marker is misplaced at %q", header.Name)
	}

	name, err := safeArchiveName(header.Name)
	if err != nil {
		return "", 0, err
	}
	if header.Size < 0 {
		return "", 0, fmt.Errorf("volume archive entry %q has negative size", name)
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if header.Size != 0 {
			return "", 0, fmt.Errorf("volume archive directory %q has a payload", name)
		}
		if _, err := archiveFileMode(header.Mode); err != nil {
			return "", 0, err
		}

		return name, tarEntryDirectory, nil

	case tar.TypeReg:
		if _, err := archiveFileMode(header.Mode); err != nil {
			return "", 0, err
		}

		return name, tarEntryRegular, nil

	case tar.TypeSymlink:
		if header.Size != 0 {
			return "", 0, fmt.Errorf("volume symlink %q has a payload", name)
		}
		if _, err := archiveFileMode(header.Mode); err != nil {
			return "", 0, err
		}
		if err := validateExportSymlink(name, header.Linkname); err != nil {
			return "", 0, err
		}

		return name, tarEntrySymlink, nil

	default:
		return "", 0, fmt.Errorf("volume archive entry %q has unsupported type %d", name, header.Typeflag)
	}
}

// validateArchiveMetadata rejects ownership and timestamp values
// that cannot be represented by the restore API.
func validateArchiveMetadata(header *tar.Header) error {
	if header.Uid < 0 || uint64(header.Uid) > uint64(^uint32(0)) {
		return fmt.Errorf("volume archive entry %q has UID %d out of range", header.Name, header.Uid)
	}
	if header.Gid < 0 || uint64(header.Gid) > uint64(^uint32(0)) {
		return fmt.Errorf("volume archive entry %q has GID %d out of range", header.Name, header.Gid)
	}
	if !header.ModTime.IsZero() && (header.ModTime.Year() < 1 || header.ModTime.Year() > 9999) {
		return fmt.Errorf("volume archive entry %q has mtime out of range", header.Name)
	}

	return nil
}
