// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

// ExtractOptions controls safe archive extraction.
type ExtractOptions struct {
	// Overwrite permits replacing existing regular files explicitly.
	Overwrite bool
}

// Extract decompresses and extracts an archive into destination.
// Archive paths are treated as untrusted input
// and are never allowed to create links or special filesystem nodes.
// Existing files are preserved unless Overwrite is explicitly enabled,
// and symlink components are rejected in both modes.
func Extract(source io.Reader, algorithm compress.Algorithm, destination string, options ExtractOptions) (int, error) {
	if source == nil {
		return 0, errors.New("archive source is required")
	}

	root, err := prepareRoot(destination)
	if err != nil {
		return 0, err
	}

	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return 0, fmt.Errorf("open archive destination root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()

	reader, err := compress.NewReader(source, algorithm)
	if err != nil {
		return 0, fmt.Errorf("open archive compressor: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tarReader := tar.NewReader(reader)
	entries := 0
	seen := make(map[string]struct{})
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return entries, nil
		}
		if nextErr != nil {
			return entries, fmt.Errorf("read archive entry: %w", nextErr)
		}

		name, err := validateArchivePath(header.Name)
		if err != nil {
			return entries, err
		}
		if _, found := seen[name]; found {
			return entries, fmt.Errorf("duplicate archive entry %q", name)
		}

		seen[name] = struct{}{}
		if err := extractEntry(tarReader, rootHandle, root, header, options); err != nil {
			return entries, err
		}

		entries++
	}
}

// extractEntry validates the path and dispatches one supported tar entry.
func extractEntry(reader *tar.Reader, rootHandle *os.Root, root string, header *tar.Header, options ExtractOptions) error {
	name, err := validateArchivePath(header.Name)
	if err != nil {
		return err
	}

	target, err := safeTarget(root, name)
	if err != nil {
		return err
	}

	// The volume archive starts with a metadata-bearing directory entry for the mounted root (`./`),
	// which is the extraction root itself and has no parent below it to validate.
	// All other entries still validate their parents before dispatching on the entry type,
	// preventing a directory entry from planting a symlink for a later file.
	if name != "." {
		if err := ensureSafeParents(root, filepath.Dir(target)); err != nil {
			return err
		}
	} else if header.Typeflag != tar.TypeDir {
		return errors.New("archive root entry must be a directory")
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if err := createDirectory(rootHandle, name, header.Mode); err != nil {
			return fmt.Errorf("extract directory %q: %w", name, err)
		}

	case tar.TypeReg:
		if err := extractRegularFile(reader, rootHandle, name, header.Size, header.Mode, options.Overwrite); err != nil {
			return fmt.Errorf("extract file %q: %w", name, err)
		}

	default:
		return fmt.Errorf("extract %q: archive entry type %d is not allowed", name, header.Typeflag)
	}

	return nil
}

// prepareRoot creates and validates the extraction root.
// Existing symlink and regular-file destinations are rejected
// because the root boundary is part of the extraction security contract.
func prepareRoot(destination string) (string, error) {
	if destination == "" {
		return "", errors.New("archive destination is required")
	}

	root, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve archive destination: %w", err)
	}

	if info, statErr := os.Lstat(root); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("archive destination is not a directory: %q", destination)
		}
	} else if os.IsNotExist(statErr) {
		if err := os.MkdirAll(root, 0o750); err != nil {
			return "", fmt.Errorf("create archive destination: %w", err)
		}
	} else {
		return "", fmt.Errorf("inspect archive destination: %w", statErr)
	}

	return root, nil
}

// validateArchivePath canonicalizes a tar path without permitting traversal.
// Both slash-separated parent components and platform-specific separators are rejected
// so an archive has the same safety properties on every OS.
func validateArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\:") {
		return "", fmt.Errorf("archive entry path is invalid: %q", name)
	}

	if slices.Contains(strings.Split(strings.TrimSuffix(name, "/"), "/"), "..") {
		return "", fmt.Errorf("archive entry path contains traversal: %q", name)
	}

	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry path escapes destination: %q", name)
	}

	return clean, nil
}

// safeTarget joins a validated archive path and verifies the root boundary.
// The second check is intentionally retained even after lexical validation
// to guard platform-specific filepath behavior.
func safeTarget(root, name string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry path escapes destination: %q", name)
	}

	return target, nil
}

// ensureSafeParents creates missing parent directories
// and rejects symlink components that could redirect a later file operation outside the root.
// Every existing component is checked with Lstat rather than followed.
func ensureSafeParents(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive parent escapes destination: %q", parent)
	}

	if relative == "." {
		return nil
	}

	return walkSafeParents(root, relative)
}

// walkSafeParents creates and validates each component below the extraction root.
// It is separated from the root-boundary check
// so callers cannot accidentally reuse the component walker with an unvalidated relative path.
func walkSafeParents(root, relative string) error {
	current := root
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err := ensureSafeParent(current); err != nil {
			return err
		}
	}

	return nil
}

// ensureSafeParent validates or creates one real directory component.
func ensureSafeParent(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o750); err != nil {
			return fmt.Errorf("create archive parent %q: %w", path, err)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect archive parent %q: %w", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("archive parent is not a real directory: %q", path)
	}

	return nil
}

// createDirectory creates a real directory without following an existing symlink.
func createDirectory(root *os.Root, target string, mode int64) error {
	info, err := root.Lstat(target)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("existing target is not a directory")
		}

		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	fileMode, err := safeFileMode(mode)
	if err != nil {
		return err
	}

	return root.Mkdir(target, fileMode)
}

// extractRegularFile writes one regular file without following an existing target
// and requires the tar payload size to be fully consumed.
// O_EXCL is used by default so an archive cannot silently overwrite an existing file.
func extractRegularFile(source io.Reader, root *os.Root, target string, size, mode int64, overwrite bool) error {
	if size < 0 {
		return errors.New("archive file size is negative")
	}

	fileMode, err := safeFileMode(mode)
	if err != nil {
		return err
	}
	if overwrite {
		if err := rejectSymlinkTarget(root, target); err != nil {
			return err
		}
		// Publish a complete temporary file instead of truncating a path that
		// could be replaced by a symlink after validation.
		return writeReplacementFile(source, root, target, size, fileMode)
	}

	file, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	removeFile := true
	defer func() {
		if removeFile {
			_ = file.Close()
			_ = root.Remove(target)
		}
	}()

	written, err := io.CopyN(file, source, size)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("wrote %d bytes, want %d", written, size)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync extracted file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close extracted file: %w", err)
	}
	removeFile = false

	return nil
}

// rejectSymlinkTarget prevents overwrite mode from replacing an existing link.
func rejectSymlinkTarget(root *os.Root, target string) error {
	info, err := root.Lstat(target)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("existing target is a symlink")
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing target: %w", err)
	}

	return nil
}

// writeReplacementFile writes a complete archive file beside the destination
// and replaces the destination only after the tar payload is fully consumed.
func writeReplacementFile(source io.Reader, root *os.Root, target string, size int64, mode os.FileMode) error {
	temporaryName, temporary, err := createRootTemporary(root, filepath.Dir(target), mode)
	if err != nil {
		return fmt.Errorf("create temporary archive file: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary archive file permissions: %w", err)
	}

	written, err := io.CopyN(temporary, source, size)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	if written != size {
		_ = temporary.Close()
		return fmt.Errorf("wrote %d bytes, want %d", written, size)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary archive file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceRootFile(root, temporaryName, target); err != nil {
		return err
	}

	removeTemporary = false
	return nil
}

// createRootTemporary creates a private temporary file
// without leaving the root-bound filesystem API
// for either name generation or file creation.
func createRootTemporary(root *os.Root, directory string, mode os.FileMode) (string, *os.File, error) {
	for attempt := range 100 {
		name := filepath.Join(directory, fmt.Sprintf(".kube-dump-%d.tmp", time.Now().UnixNano()+int64(attempt)))
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			return name, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}

	return "", nil, errors.New("create unique root-bound temporary archive file: too many collisions")
}

// replaceRootFile installs a complete temporary file below the extraction root.
func replaceRootFile(root *os.Root, source, target string) error {
	if runtime.GOOS == "windows" {
		if err := root.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return root.Rename(source, target)
}

// safeFileMode limits archive permissions before converting to os.FileMode.
func safeFileMode(mode int64) (os.FileMode, error) {
	if mode < 0 || mode > 0o777 {
		return 0, fmt.Errorf("archive mode %o is outside 000..777", mode)
	}

	return os.FileMode(mode), nil
}
