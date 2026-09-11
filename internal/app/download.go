// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	nativegit "github.com/woozymasta/kube-dump/v2/internal/git"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
)

// Execute downloads a Git worktree or clones a Git remote into a local path.
func (c *ResourceDownloadGitCommand) Execute(_ []string) error {
	if c.Positional.Source == "" || c.Positional.Destination == "" {
		return errors.New("git source and destination are required")
	}

	if isGitRemote(c.Positional.Source) {
		options := c.Git
		options.RemoteURL = c.Positional.Source
		if _, _, err := nativegit.Prepare(c.context(), c.Positional.Destination, options); err != nil {
			return fmt.Errorf("clone Git resource dump: %w", err)
		}

		log.Info().
			Str("component", "resource").
			Str("operation", "download").
			Str("backend", "git").
			Str("destination", c.Positional.Destination).
			Msg("resource download completed")

		return nil
	}

	if err := copyResourceTree(c.context(), c.Positional.Source, c.Positional.Destination); err != nil {
		return err
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "download").
		Str("backend", "directory").
		Str("destination", c.Positional.Destination).
		Msg("resource download completed")

	return nil
}

// Execute downloads a resource prefix or one selected S3 object.
func (c *ResourceDownloadS3Command) Execute(_ []string) error {
	if c.URI == "" || c.Positional.Destination == "" {
		return errors.New("S3 source and destination are required")
	}

	metadataStore, store, err := openCurrentS3ResourceStores(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 resource source: %w", err)
	}

	if c.Positional.Object == "" {
		if err := store.Download(c.context(), resourceRoot(c.Positional.Destination)); err != nil {
			return err
		}
		if err := downloadResourceMetadata(
			c.context(), metadataStore, c.Positional.Destination,
		); err != nil {
			return err
		}

		log.Info().
			Str("component", "resource").
			Str("operation", "download").
			Str("backend", "s3").
			Str("destination", c.Positional.Destination).
			Msg("resource download completed")

		return nil
	}

	key := filepath.ToSlash(filepath.Join(store.ObjectKey(), c.Positional.Object))
	target := filepath.Join(resourceRoot(c.Positional.Destination), filepath.FromSlash(c.Positional.Object))
	if err := ensureBelow(resourceRoot(c.Positional.Destination), target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create S3 resource destination: %w", err)
	}
	if err := store.ReadObject(c.context(), key, target); err != nil {
		return fmt.Errorf("download S3 resource object: %w", err)
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "download").
		Str("backend", "s3").
		Str("destination", target).
		Msg("resource download completed")

	return nil
}

// downloadResourceMetadata copies the shared AES-SIV metadata subtree
// beside the resource tree when a complete S3 resource capture is downloaded.
func downloadResourceMetadata(
	ctx context.Context,
	store *export.S3Store,
	destination string,
) error {
	return store.ForEachStoredFileStream(ctx, func(key string, reader io.Reader) error {
		relative := strings.TrimPrefix(strings.TrimPrefix(key, store.ObjectKey()), "/")
		if relative != "metadata.yaml" && !strings.HasPrefix(relative, "keys/") {
			return nil
		}

		target := filepath.Join(
			destination, filepath.FromSlash(cryptoLayoutDirectory), filepath.FromSlash(relative),
		)
		if err := ensureBelow(destination, target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create resource metadata directory: %w", err)
		}
		if err := writeAtomic(target, func(output io.Writer) error {
			return copyLimited(output, reader, maxRenderedMetadataBytes)
		}); err != nil {
			return fmt.Errorf("write resource metadata %q: %w", key, err)
		}

		return nil
	})
}

// copyLimited streams a bounded metadata object into an atomic destination.
func copyLimited(destination io.Writer, source io.Reader, limit int64) error {
	if destination == nil || source == nil || limit < 0 {
		return errors.New("bounded copy arguments are invalid")
	}

	written, err := io.Copy(destination, io.LimitReader(source, limit+1))
	if err != nil {
		return err
	}
	if written > limit {
		return fmt.Errorf("object exceeds %d-byte limit", limit)
	}

	return nil
}

// Execute copies a local resource archive to another local path.
func (c *ResourceDownloadArchiveCommand) Execute(_ []string) error {
	if c.Positional.Source == "" || c.Positional.Destination == "" {
		return errors.New("archive source and destination are required")
	}

	if err := copyLocalFile(c.Positional.Source, c.Positional.Destination); err != nil {
		return err
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "download").
		Str("backend", "archive").
		Str("destination", c.Positional.Destination).
		Msg("resource archive download completed")

	return nil
}

// Execute downloads one resource archive object from S3.
func (c *ResourceDownloadArchiveS3Command) Execute(_ []string) error {
	if c.URI == "" || c.Positional.Destination == "" {
		return errors.New("S3 archive source and destination are required")
	}

	destination := c.S3Destination
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 archive source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.Positional.Destination), 0o750); err != nil {
		return fmt.Errorf("create S3 archive destination: %w", err)
	}
	if err := store.ReadObject(c.context(), store.ObjectKey(), c.Positional.Destination); err != nil {
		return fmt.Errorf("download S3 resource archive: %w", err)
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "download").
		Str("backend", "archive-s3").
		Str("destination", c.Positional.Destination).
		Msg("resource archive download completed")

	return nil
}

// Execute downloads PVC artifacts from an S3 prefix into a local store.
func (c *PvcDownloadS3Command) Execute(_ []string) error {
	if c.URI == "" || c.Positional.Destination == "" {
		return errors.New("S3 PVC source and destination are required")
	}

	destination := c.S3Destination
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 PVC source: %w", err)
	}

	if c.Positional.Object != "" {
		if err := downloadStoredObject(
			c.context(), store, c.Positional.Object, c.Positional.Destination, c.progress,
		); err != nil {
			return err
		}
	} else if err := downloadStoredFiles(c.context(), store, c.Positional.Destination, c.progress); err != nil {
		return err
	}

	log.Info().
		Str("component", "pvc").
		Str("operation", "download").
		Str("backend", "s3").
		Str("destination", c.Positional.Destination).
		Bool("single_object", c.Positional.Object != "").
		Msg("PVC data download completed")

	return nil
}

// Execute downloads an OCI Image Layout from S3.
func (c *ImageDownloadS3Command) Execute(_ []string) error {
	if c.URI == "" || c.Positional.Destination == "" {
		return errors.New("S3 image source and destination are required")
	}

	destination := withS3LayoutPrefix(c.S3Destination, imageLayoutDirectory)
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 image source: %w", err)
	}

	writerLock, err := acquireImageWriterLock(c.context(), imageRoot(c.Positional.Destination))
	if err != nil {
		return fmt.Errorf("acquire image writer lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()

	if err := downloadImageLayout(c.context(), store, imageRoot(c.Positional.Destination), c.progress); err != nil {
		return fmt.Errorf("download S3 image layout: %w", err)
	}

	log.Info().
		Str("component", "images").
		Str("operation", "download").
		Str("backend", "s3").
		Str("destination", c.Positional.Destination).
		Msg("container image download completed")

	return nil
}

// copyResourceTree copies a loose resource tree without changing its files.
func copyResourceTree(ctx context.Context, source, destination string) error {
	return copyTree(ctx, source, destination, func(path string) bool {
		path = filepath.ToSlash(path)
		return path == ".git" || strings.HasPrefix(path, ".git/")
	})
}

// copyLocalFile copies a local archive through the common atomic output path.
func copyLocalFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open local source: %w", err)
	}
	defer func() { _ = input.Close() }()

	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return fmt.Errorf("create local archive destination: %w", err)
	}
	if err := writeAtomic(destination, func(output io.Writer) error {
		_, err := io.Copy(output, input)
		return err
	}); err != nil {
		return fmt.Errorf("copy local archive: %w", err)
	}

	return nil
}

// copyTree copies regular files and directories below a source root.
func copyTree(ctx context.Context, source, destination string, skip func(string) bool) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect download source: %w", err)
	}
	if !info.IsDir() {
		return errors.New("download source must be a directory")
	}
	if filepath.Clean(source) == filepath.Clean(destination) {
		return errors.New("download source and destination must differ")
	}
	if relative, err := filepath.Rel(source, destination); err == nil &&
		(relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
		return errors.New("download destination must not be inside the source")
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return fmt.Errorf("create download destination: %w", err)
	}

	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." || (skip != nil && skip(relative)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("download source contains unsupported entry %q", relative)
		}

		return copyLocalFile(path, target)
	})
}

// ensureBelow rejects a destination that escapes the selected root.
func ensureBelow(root, target string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}

	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination escapes root %q: %q", root, target)
	}

	return nil
}

// isGitRemote recognizes the URL and SCP-like forms accepted by go-git.
func isGitRemote(value string) bool {
	return strings.Contains(value, "://") || strings.HasPrefix(value, "git@")
}

// downloadStoredFiles copies every object below an S3 prefix into a local tree.
func downloadStoredFiles(
	ctx context.Context,
	store *export.S3Store,
	destination string,
	manager *progress.Manager,
) error {
	var counter *progress.Counter
	defer func() {
		if counter != nil {
			counter.Complete()
		}
	}()

	return store.ForEachStoredFileStreamWithProgress(ctx, func(key string, data io.Reader) error {
		relative := strings.TrimPrefix(strings.TrimPrefix(key, store.ObjectKey()), "/")
		if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) {
			return fmt.Errorf("invalid S3 object key %q", key)
		}

		target := filepath.Join(destination, filepath.FromSlash(relative))
		if err := ensureBelow(destination, target); err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}

		return writeAtomic(target, func(output io.Writer) error {
			_, err := io.Copy(output, data)
			return err
		})
	}, func(total, completed int) {
		if counter == nil {
			counter = manager.NewCounter("PVC files", int64(total))
		}
		if completed > 0 {
			counter.Increment()
		}
	})
}

// downloadStoredObject copies one object below an S3 prefix into a local tree.
func downloadStoredObject(
	ctx context.Context,
	store *export.S3Store,
	object, destination string,
	manager *progress.Manager,
) error {
	if object == "" {
		return errors.New("S3 object key is required")
	}

	target := filepath.Join(destination, filepath.FromSlash(object))
	if err := ensureBelow(destination, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create S3 object destination: %w", err)
	}

	key := filepath.ToSlash(filepath.Join(store.ObjectKey(), object))
	if err := store.ReadObject(ctx, key, target); err != nil {
		return fmt.Errorf("download S3 object: %w", err)
	}

	counter := manager.NewCounter("PVC files", 1)
	counter.Increment()
	counter.Complete()

	return nil
}
