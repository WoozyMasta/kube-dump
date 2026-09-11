// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// ExistingEmptyOnly permits import only into an empty destination directory.
	ExistingEmptyOnly ExistingPolicy = "empty-only"

	// ExistingMerge overlays regular files and creates missing directories.
	ExistingMerge ExistingPolicy = "merge"

	// ExistingReplace permits replacing regular files while retaining the root.
	ExistingReplace ExistingPolicy = "replace"

	rootArchiveName      = "./"
	rootMetadataPAXKey   = "KUBE_DUMP_ROOT_METADATA"
	rootMetadataPAXValue = "1"
)

// ExistingPolicy controls how volume import treats an existing destination.
type ExistingPolicy string

// archiveMetadata contains the portable filesystem metadata restored from one tar entry.
type archiveMetadata struct {
	modTime   time.Time
	name      string
	uid       int
	gid       int
	mode      os.FileMode
	entryType tarEntryType
}

// contextReader makes blocked stream reads observe cancellation between chunks.
type contextReader struct {
	ctx    context.Context // ctx cancels reads when the parent volume operation is interrupted.
	reader io.Reader       // reader is the archive stream source.
}

// Read checks context cancellation before reading the next stream chunk.
func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}

// UnmarshalText parses a PVC existing-data policy from CLI or configuration.
func (p *ExistingPolicy) UnmarshalText(value []byte) error {
	parsed := ExistingPolicy(string(value))
	if err := parsed.Validate(); err != nil {
		return err
	}

	*p = parsed
	return nil
}

// ExportVolume writes a protocol-clean tar stream for a mounted volume root.
// Symlinks are preserved without following their targets;
// hard links, devices, and other special files are rejected because they have no portable contract.
func ExportVolume(ctx context.Context, root string, dst io.Writer) error {
	if ctx == nil || dst == nil {
		return errors.New("volume export context and destination are required")
	}

	root, err := validateVolumeRoot(root)
	if err != nil {
		return err
	}

	volumeRoot, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open volume root: %w", err)
	}
	defer func() { _ = volumeRoot.Close() }()

	writer := tar.NewWriter(dst)
	rootInfo, err := volumeRoot.Stat(".")
	if err != nil {
		return fmt.Errorf("stat volume root metadata: %w", err)
	}
	rootHeader, err := tar.FileInfoHeader(rootInfo, "")
	if err != nil {
		return fmt.Errorf("create volume root header: %w", err)
	}
	rootHeader.Name = rootArchiveName
	rootHeader.PAXRecords = map[string]string{rootMetadataPAXKey: rootMetadataPAXValue}
	if err := writer.WriteHeader(rootHeader); err != nil {
		return fmt.Errorf("write volume root header: %w", err)
	}

	// WalkDir supplies deterministic lexical traversal,
	// while OpenRoot keeps file opens anchored below the validated volume root.
	// Every entry is checked before its header is emitted;
	// links are recorded rather than followed,
	// and special files are rejected because they cannot be restored portably.
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat volume entry %q: %w", path, err)
		}

		isSymlink := info.Mode()&os.ModeSymlink != 0
		if info.Mode().IsRegular() {
			hardLinked, err := hasMultipleHardLinks(path, info)
			if err != nil {
				return fmt.Errorf("inspect hard links for volume entry %q: %w", path, err)
			}
			if hardLinked {
				return fmt.Errorf("volume entry %q is a hard link; hard-link topology is unsupported", path)
			}
		}
		if !isSymlink && !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("volume entry %q has unsupported type %s", path, info.Mode())
		}
		if path == root {
			return nil
		}

		name, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize volume entry %q: %w", path, err)
		}

		name = filepath.ToSlash(name)
		linkTarget := ""
		if isSymlink {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read volume symlink %q: %w", name, err)
			}
			if err := validateExportSymlink(name, linkTarget); err != nil {
				return err
			}
		}

		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("create volume header %q: %w", name, err)
		}

		header.Name = name
		if info.IsDir() {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write volume header %q: %w", name, err)
		}
		if isSymlink || !info.Mode().IsRegular() {
			return nil
		}

		fileName, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize volume file %q: %w", path, err)
		}

		// Open regular files through the validated root
		// so a concurrent rename cannot redirect the read outside the mounted volume.
		file, err := volumeRoot.Open(fileName)
		if err != nil {
			return fmt.Errorf("open volume file %q: %w", name, err)
		}

		_, copyErr := io.Copy(writer, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("read volume file %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close volume file %q: %w", name, closeErr)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("export volume: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close volume archive: %w", err)
	}

	return nil
}

// ImportVolume validates the complete tar stream before extracting it into a mounted volume root.
func ImportVolume(ctx context.Context, root string, src io.Reader, policy ExistingPolicy) error {
	if ctx == nil || src == nil {
		return errors.New("volume import context and source are required")
	}
	if err := policy.Validate(); err != nil {
		return err
	}

	archive, err := spoolVolumeArchive(ctx, src)
	if err != nil {
		return err
	}
	defer func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	}()

	if _, err := ValidateVolumeArchive(ctx, archive); err != nil {
		return fmt.Errorf("validate volume archive before import: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind volume archive before import: %w", err)
	}

	return ImportVerifiedVolume(ctx, root, archive, policy)
}

// ImportVerifiedVolume extracts a complete archive after the caller validated it in full.
// The source must be stable between validation and extraction;
// this function deliberately skips the private spool and second validation pass.
// It is intended for internal orchestrators that own both guarantees.
func ImportVerifiedVolume(ctx context.Context, root string, src io.Reader, policy ExistingPolicy) error {
	if ctx == nil || src == nil {
		return errors.New("verified volume import context and source are required")
	}
	if err := policy.Validate(); err != nil {
		return err
	}

	return importVerifiedVolume(ctx, root, src, policy)
}

// spoolVolumeArchive stores the decoded archive stream on disk before validation.
// A private spool gives extraction a stable second pass without buffering a PVC archive in memory.
func spoolVolumeArchive(ctx context.Context, src io.Reader) (*os.File, error) {
	archive, err := os.CreateTemp("", "kube-dump-volume-import-*")
	if err != nil {
		return nil, fmt.Errorf("create volume archive spool: %w", err)
	}

	remove := true
	defer func() {
		if remove {
			_ = archive.Close()
			_ = os.Remove(archive.Name())
		}
	}()

	if _, err := io.Copy(archive, &contextReader{ctx: ctx, reader: src}); err != nil {
		return nil, fmt.Errorf("spool volume archive: %w", err)
	}
	if err := archive.Sync(); err != nil {
		return nil, fmt.Errorf("sync volume archive spool: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind volume archive spool: %w", err)
	}

	remove = false
	return archive, nil
}

// importVerifiedVolume extracts an archive that has already passed full validation.
func importVerifiedVolume(ctx context.Context, root string, src io.Reader, policy ExistingPolicy) error {
	root, err := validateVolumeRoot(root)
	if err != nil {
		return err
	}

	volumeRoot, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open volume root: %w", err)
	}
	defer func() { _ = volumeRoot.Close() }()

	if policy == ExistingEmptyOnly {
		empty, err := directoryEmpty(volumeRoot)
		if err != nil {
			return err
		}

		if !empty {
			return errors.New("volume destination is not empty")
		}
	}

	reader := tar.NewReader(src)
	cleared := policy != ExistingReplace
	directories := make([]archiveMetadata, 0)
	rootSeen := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if !rootSeen {
				return errors.New("volume archive is missing the root metadata record")
			}
			if !cleared {
				if err := clearVolumeDirectory(volumeRoot); err != nil {
					return err
				}
			}
			for _, directory := range slices.Backward(directories) {
				if err := applyArchiveMetadata(volumeRoot, directory); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read volume archive: %w", err)
		}

		if err := validateArchiveMetadata(header); err != nil {
			return err
		}
		name, entryType, err := validateArchiveHeader(header)
		if err != nil {
			return err
		}

		if !rootSeen {
			if name != "." {
				return errors.New("volume archive is missing the root metadata record")
			}
			rootSeen = true
		} else if name == "." {
			return errors.New("volume archive contains duplicate root metadata records")
		}

		if !cleared {
			if err := clearVolumeDirectory(volumeRoot); err != nil {
				return err
			}
			cleared = true
		}

		// Only directories, regular files,
		// and validated relative symlinks are part of the portable volume contract.
		// Restore only the portable subset emitted by ExportVolume.
		// In particular, never materialize devices or other special files from an untrusted archive.
		switch entryType {
		case tarEntryDirectory:
			mode, err := archiveFileMode(header.Mode)
			if err != nil {
				return err
			}
			if err := makeRootDirectory(volumeRoot, name, mode); err != nil {
				return err
			}
			directories = append(directories, newArchiveMetadata(name, header, entryType, mode))

		case tarEntryRegular:
			mode, err := archiveFileMode(header.Mode)
			if err != nil {
				return err
			}
			if err := writeVolumeFile(ctx, volumeRoot, name, reader, header.Size, policy, mode); err != nil {
				return err
			}
			if err := applyArchiveMetadata(volumeRoot, newArchiveMetadata(name, header, entryType, mode)); err != nil {
				return err
			}

		case tarEntrySymlink:
			if err := validateImportSymlink(root, name, header.Linkname); err != nil {
				return err
			}
			if err := makeRootDirectory(volumeRoot, filepath.Dir(filepath.FromSlash(name)), 0o700); err != nil {
				return err
			}

			if _, err := volumeRoot.Lstat(filepath.FromSlash(name)); err == nil {
				return fmt.Errorf("volume symlink target already exists: %q", name)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect volume symlink %q: %w", name, err)
			}
			if err := volumeRoot.Symlink(header.Linkname, filepath.FromSlash(name)); err != nil {
				return fmt.Errorf("create volume symlink %q: %w", name, err)
			}
			mode, err := archiveFileMode(header.Mode)
			if err != nil {
				return err
			}
			if err := applyArchiveMetadata(volumeRoot, newArchiveMetadata(name, header, entryType, mode)); err != nil {
				return err
			}

		default:
			return fmt.Errorf("volume archive entry %q has unsupported type %d", name, header.Typeflag)
		}
	}
}

// Validate checks whether an existing-data policy is supported.
func (p ExistingPolicy) Validate() error {
	switch p {
	case ExistingEmptyOnly, ExistingMerge, ExistingReplace:
		return nil
	default:
		return fmt.Errorf("unsupported volume existing-data policy %q", p)
	}
}

// validateVolumeRoot creates and validates the filesystem root used by an agent.
func validateVolumeRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("volume root must be an absolute path")
	}

	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", fmt.Errorf("create volume root: %w", err)
		}

		info, err = os.Lstat(root)
	}
	if err != nil {
		return "", fmt.Errorf("stat volume root: %w", err)
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("volume root must be a directory")
	}

	return root, nil
}

// directoryEmpty reports whether a directory has no entries.
func directoryEmpty(root *os.Root) (bool, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return false, fmt.Errorf("read volume destination: %w", err)
	}

	if len(entries) == 1 && entries[0].Name() == "lost+found" {
		info, err := entries[0].Info()
		if err != nil {
			return false, fmt.Errorf("inspect volume destination: %w", err)
		}

		if info.IsDir() {
			return true, nil
		}
	}

	return len(entries) == 0, nil
}

// clearVolumeDirectory removes existing entries while preserving the mounted root.
// ExistingReplace is explicitly destructive;
// Root.RemoveAll keeps removal anchored below the opened root
// and does not follow a symlink outside it.
func clearVolumeDirectory(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read volume destination for replacement: %w", err)
	}

	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove existing volume entry %q: %w", entry.Name(), err)
		}
	}

	return nil
}

// safeArchiveName rejects absolute and parent-traversing tar paths.
func safeArchiveName(name string) (string, error) {
	if name == "" ||
		strings.ContainsRune(name, '\x00') ||
		strings.Contains(name, "\\") {
		return "", fmt.Errorf("invalid volume archive path %q", name)
	}

	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean == "." ||
		clean == ".." ||
		strings.HasPrefix(clean, "../") ||
		strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("volume archive path escapes root: %q", name)
	}

	return clean, nil
}

// validateExportSymlink rejects links that point outside the mounted volume.
func validateExportSymlink(name, target string) error {
	if target == "" ||
		strings.ContainsRune(target, '\x00') ||
		filepath.IsAbs(target) ||
		strings.Contains(target, "\\") {
		return fmt.Errorf("volume symlink %q has an unsafe target %q", name, target)
	}

	resolved := filepath.Clean(filepath.Join(filepath.Dir(name), filepath.FromSlash(target)))
	if resolved == ".." ||
		strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("volume symlink %q escapes root: %q", name, target)
	}

	return nil
}

// validateImportSymlink applies the same containment check to archive entries.
func validateImportSymlink(root, name, target string) error {
	if err := validateExportSymlink(name, target); err != nil {
		return err
	}

	resolved := filepath.Clean(filepath.Join(
		root,
		filepath.Dir(filepath.FromSlash(name)),
		filepath.FromSlash(target),
	))

	relative, err := filepath.Rel(root, resolved)
	if err != nil ||
		relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("volume symlink %q escapes root: %q", name, target)
	}

	return nil
}

// makeRootDirectory creates a destination directory
// without following a pre-existing symlink in its path.
// Root.MkdirAll also rejects an escaping symlink
// if a path component changes during the operation.
func makeRootDirectory(root *os.Root, name string, mode os.FileMode) error {
	name = filepath.Clean(name)
	if name == "." {
		return nil
	}

	parts := strings.Split(name, string(filepath.Separator))
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}

		info, err := root.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("volume directory target is not a directory: %q", name)
			}
			continue
		}

		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect volume directory %q: %w", name, err)
		}

		break
	}

	if err := root.MkdirAll(name, mode.Perm()); err != nil {
		return fmt.Errorf("create volume directory %q: %w", name, err)
	}

	return nil
}

// writeVolumeFile writes one regular tar entry with explicit overwrite policy.
func writeVolumeFile(
	ctx context.Context,
	root *os.Root,
	name string,
	src io.Reader,
	size int64,
	policy ExistingPolicy,
	mode os.FileMode,
) error {
	if size < 0 {
		return fmt.Errorf("volume file %q has negative size", name)
	}
	if err := makeRootDirectory(root, filepath.Dir(filepath.FromSlash(name)), 0o700); err != nil {
		return err
	}

	name = filepath.FromSlash(name)
	if info, err := root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("volume file target is not a regular file: %q", name)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect volume file %q: %w", name, err)
	}

	// Write beside the destination and rename only after the complete payload is available.
	// This avoids truncating a path that could be replaced by a symlink between validation and open.
	if policy != ExistingEmptyOnly {
		return writeReplacementFile(ctx, root, name, src, size, mode)
	}

	// O_EXCL preserves empty-only semantics
	// even if another process creates the destination after the initial directory check.
	flags := os.O_CREATE | os.O_WRONLY | os.O_EXCL
	file, err := root.OpenFile(name, flags, mode.Perm())
	if err != nil {
		return fmt.Errorf("create volume file %q: %w", name, err)
	}

	written, copyErr := io.CopyN(file, &contextReader{ctx: ctx, reader: src}, size)
	closeErr := file.Close()
	if copyErr != nil {
		_ = root.Remove(name)
		return fmt.Errorf("write volume file %q: %w", name, copyErr)
	}
	if closeErr != nil {
		_ = root.Remove(name)
		return fmt.Errorf("close volume file %q: %w", name, closeErr)
	}
	if written != size {
		_ = root.Remove(name)
		return fmt.Errorf("write volume file %q: wrote %d bytes, want %d", name, written, size)
	}

	return nil
}

// writeReplacementFile writes a regular volume file
// to a private temporary sibling and publishes it with rename.
// A failed or cancelled copy never leaves a partial destination file behind.
func writeReplacementFile(
	ctx context.Context,
	root *os.Root,
	target string,
	src io.Reader,
	size int64,
	mode os.FileMode,
) error {
	directory := filepath.Dir(target)
	temporaryName, err := rootTemporaryName(root, directory)
	if err != nil {
		return fmt.Errorf("create temporary volume file %q: %w", target, err)
	}

	temporary, err := root.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary volume file %q: %w", target, err)
	}

	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary volume file permissions %q: %w", target, err)
	}

	written, copyErr := io.CopyN(temporary, &contextReader{ctx: ctx, reader: src}, size)
	if copyErr != nil {
		_ = temporary.Close()
		return fmt.Errorf("write volume file %q: %w", target, copyErr)
	}
	if written != size {
		_ = temporary.Close()
		return fmt.Errorf("write volume file %q: wrote %d bytes, want %d", target, written, size)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync volume file %q: %w", target, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close volume file %q: %w", target, err)
	}
	if err := root.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("publish volume file %q: %w", target, err)
	}

	removeTemporary = false
	return nil
}

// rootTemporaryName returns a random name in the target's parent directory.
// It avoids reopening an absolute path during the replacement race window.
func rootTemporaryName(root *os.Root, directory string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}

	name := filepath.Join(directory, ".kube-dump-"+hex.EncodeToString(random[:])+".tmp")
	if _, err := root.Lstat(name); err == nil {
		return rootTemporaryName(root, directory)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	return name, nil
}

// archiveFileMode validates a tar mode before converting it to os.FileMode.
func archiveFileMode(mode int64) (os.FileMode, error) {
	if mode < 0 || mode&^0o7777 != 0 {
		return 0, fmt.Errorf("volume archive mode %d is out of range", mode)
	}

	result := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		result |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		result |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		result |= os.ModeSticky
	}

	return result, nil
}

// newArchiveMetadata copies validated tar metadata before the reader advances to another header.
func newArchiveMetadata(
	name string,
	header *tar.Header,
	entryType tarEntryType,
	mode os.FileMode,
) archiveMetadata {
	return archiveMetadata{
		name:      name,
		entryType: entryType,
		mode:      mode,
		uid:       header.Uid,
		gid:       header.Gid,
		modTime:   header.ModTime,
	}
}

// applyArchiveMetadata applies metadata only after the entry content exists completely.
// Directories are called after all children so their final mode cannot block extraction.
func applyArchiveMetadata(root *os.Root, metadata archiveMetadata) error {
	if err := applyArchiveOwner(root, metadata.name, metadata.entryType, metadata.uid, metadata.gid); err != nil {
		return fmt.Errorf("restore owner for %q: %w", metadata.name, err)
	}
	if metadata.entryType == tarEntrySymlink {
		return nil
	}

	if err := root.Chmod(metadata.name, metadata.mode); err != nil {
		return fmt.Errorf("restore mode for %q: %w", metadata.name, err)
	}
	if metadata.modTime.IsZero() {
		return nil
	}
	if err := root.Chtimes(metadata.name, metadata.modTime, metadata.modTime); err != nil {
		return fmt.Errorf("restore mtime for %q: %w", metadata.name, err)
	}

	return nil
}
