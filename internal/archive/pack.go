// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// contextReader makes local archive reads observe cancellation between chunks.
type contextReader struct {
	// ctx cancels reads when the parent operation is interrupted.
	ctx context.Context
	// reader is the underlying archive content source.
	reader io.Reader
}

// PackDirectory streams a canonical backup directory into a compressed archive.
// Symlinks and special files are rejected to keep the archive root-bound.
// Walk order is normalized by filepath.WalkDir,
// while the archive writer preserves each entry's canonical relative path and metadata.
func PackDirectory(ctx context.Context, source string, dst io.Writer, config Config) error {
	if ctx == nil || dst == nil {
		return errors.New("archive context and destination are required")
	}

	root, err := validateDirectory(source)
	if err != nil {
		return err
	}

	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open archive root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()

	writer, err := NewBackupWriter(dst, config)
	if err != nil {
		return err
	}

	packRoots, err := packRoots(root)
	if err != nil {
		_ = writer.Close()
		return err
	}

	for _, sourceRoot := range packRoots {
		sourcePath := filepath.Join(root, sourceRoot)
		info, statErr := os.Lstat(sourcePath)
		if os.IsNotExist(statErr) {
			continue
		}

		if statErr != nil {
			_ = writer.Close()
			return fmt.Errorf("inspect archive tree %q: %w", sourceRoot, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			_ = writer.Close()
			return fmt.Errorf("archive tree %q is a symlink", sourceRoot)
		}

		if err := filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
			return packEntry(ctx, root, rootHandle, writer, path, entry, walkErr, config.Progress)
		}); err != nil {
			_ = writer.Close()
			return fmt.Errorf("pack archive: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close packed archive: %w", err)
	}

	return nil
}

// MeasureDirectory returns the uncompressed regular-file bytes that PackDirectory will read.
// It follows the same canonical roots and rejects the same unsupported entry types,
// allowing callers to show truthful input progress without inspecting or buffering archive output.
func MeasureDirectory(source string) (int64, error) {
	root, err := validateDirectory(source)
	if err != nil {
		return 0, err
	}

	roots, err := packRoots(root)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, sourceRoot := range roots {
		sourcePath := filepath.Join(root, sourceRoot)
		if _, err := os.Lstat(sourcePath); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return 0, fmt.Errorf("inspect archive tree %q: %w", sourceRoot, err)
		}

		err := filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat archive entry %q: %w", path, err)
			}
			if err := validatePackEntryType(info, path); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}

			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("measure archive: %w", err)
		}
	}

	return total, nil
}

// packRoots returns canonical resource directories and the AES-SIV keyring.
// Other regular files and hidden metadata stay outside the archive layout.
func packRoots(root string) ([]string, error) {
	var roots []string
	var cryptoRoot string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read archive source: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		if entry.Name() == ".kube-dump" {
			if _, err := os.Lstat(filepath.Join(root, ".kube-dump", "crypto")); err == nil {
				cryptoRoot = filepath.Join(".kube-dump", "crypto")
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("inspect AES-SIV keyring: %w", err)
			}

			continue
		}
		roots = append(roots, entry.Name())
	}

	if cryptoRoot != "" {
		// Keyring entries must precede resources so streaming readers can open
		// AES-SIV before the first encrypted manifest is encountered.
		roots = append([]string{cryptoRoot}, roots...)
	}

	return roots, nil
}

// packEntry converts one filesystem entry into a validated archive entry.
func packEntry(
	ctx context.Context,
	root string,
	rootHandle *os.Root,
	writer *Writer,
	currentPath string,
	entry fs.DirEntry,
	walkErr error,
	progress func(int64),
) error {
	if walkErr != nil {
		return walkErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat archive entry %q: %w", currentPath, err)
	}
	if currentPath == root {
		return nil
	}
	if err := validatePackEntryType(info, currentPath); err != nil {
		return err
	}

	name, err := packEntryName(root, currentPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		// Directory headers make the archive layout explicit
		// and allow extraction to create parents before their children.
		return writer.Add(Entry{
			Path:    name + "/",
			Mode:    int64(info.Mode().Perm()),
			ModTime: info.ModTime(),
			Dir:     true,
		})
	}

	return packRegularFile(ctx, rootHandle, writer, name, info, progress)
}

// validatePackEntryType rejects links and special files
// before they reach the root-anchored file opener.
func validatePackEntryType(info fs.FileInfo, path string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("archive entry %q has unsupported type %s", path, info.Mode())
	}

	return nil
}

// packEntryName converts a walked path into a safe archive-relative name.
func packEntryName(root, currentPath string) (string, error) {
	name, err := filepath.Rel(root, currentPath)
	if err != nil {
		return "", fmt.Errorf("relativize archive entry %q: %w", currentPath, err)
	}

	name = filepath.ToSlash(name)
	if name == "." ||
		name == "" ||
		strings.HasPrefix(name, "../") ||
		strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry escapes root: %q", name)
	}

	return name, nil
}

// packRegularFile streams one regular file and closes it before returning.
func packRegularFile(
	ctx context.Context,
	rootHandle *os.Root,
	writer *Writer,
	name string,
	info fs.FileInfo,
	progress func(int64),
) error {
	file, err := rootHandle.Open(filepath.FromSlash(name))
	if err != nil {
		return fmt.Errorf("open archive entry %q: %w", name, err)
	}

	addErr := writer.Add(Entry{
		Path: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
		Size: info.Size(), Reader: &progressReader{
			reader:   &contextReader{ctx: ctx, reader: file},
			progress: progress,
		},
	})

	closeErr := file.Close()
	if addErr != nil {
		return fmt.Errorf("write archive entry %q: %w", name, addErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close archive entry %q: %w", name, closeErr)
	}

	return nil
}

// progressReader reports source bytes consumed by the tar writer.
type progressReader struct {
	reader   io.Reader
	progress func(int64)
}

// Read forwards source data and reports only bytes returned by the source.
func (r *progressReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 && r.progress != nil {
		r.progress(int64(read))
	}

	return read, err
}

// validateDirectory ensures the archive source is a non-symlink directory.
func validateDirectory(source string) (string, error) {
	if source == "" {
		return "", errors.New("archive source directory is required")
	}

	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve archive source: %w", err)
	}

	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat archive source: %w", err)
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("archive source must be a directory")
	}

	return filepath.Clean(absolute), nil
}

// Read checks cancellation before reading the next archive chunk.
func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
