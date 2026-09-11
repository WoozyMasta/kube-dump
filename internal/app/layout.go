// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"path/filepath"

	"github.com/woozymasta/kube-dump/v2/internal/filelock"
)

const (
	resourceMetadataDirectory   = ".kube-dump"
	cryptoLayoutDirectory       = resourceMetadataDirectory + "/crypto"
	resourceLayoutDirectory     = "resources"
	resourceGenerationDirectory = "generations"
	resourceCurrentPointer      = "current.json"
	imageLayoutDirectory        = "images"
)

// resourceRoot returns the resource subtree of a shared capture directory.
func resourceRoot(root string) string {
	return filepath.Join(root, resourceLayoutDirectory)
}

// imageRoot returns the OCI Image Layout subtree of a shared capture directory.
func imageRoot(root string) string {
	return filepath.Join(root, imageLayoutDirectory)
}

// acquireImageWriterLock serializes local OCI layout writers
// without placing a lock file inside the user-visible image layout.
func acquireImageWriterLock(ctx context.Context, root string) (*filelock.Lock, error) {
	if root == "" {
		return nil, errors.New("image writer lock root is required")
	}

	absolute, err := canonicalFilesystemPath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve image writer lock root: %w", err)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	lockPath := filepath.Join(
		filepath.Dir(absolute),
		fmt.Sprintf(".kube-dump-image-writer-%x.lock", digest[:12]),
	)
	lock, err := filelock.Acquire(ctx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("acquire image writer lock: %w", err)
	}

	return lock, nil
}

// withS3LayoutPrefix adds one logical artifact type below an S3 destination.
func withS3LayoutPrefix(destination S3Destination, directory string) S3Destination {
	destination.Prefix = path.Join(destination.Prefix, directory)
	return destination
}
