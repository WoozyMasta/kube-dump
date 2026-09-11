// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package export writes normalized Kubernetes objects to local destinations.
package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
)

// DirectorySink writes one canonical YAML document per Kubernetes object.
// The layout uses readable Kubernetes identities and contains no run metadata.
type DirectorySink struct {
	// root is the canonical export directory.
	root string
}

// NewDirectorySink creates the destination directory if necessary.
func NewDirectorySink(root string) (*DirectorySink, error) {
	if root == "" {
		return nil, errors.New("export directory is required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create export directory: %w", err)
	}

	return &DirectorySink{root: root}, nil
}

// CanonicalObjectPath returns the slash-separated relative path
// used for one object in every resource artifact backend.
// The result is independent of the host operating system,
// so it is safe to print in portable YAML comments.
func CanonicalObjectPath(identity state.Identity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", fmt.Errorf("validate object identity: %w", err)
	}

	group := identity.Group
	if group == "" {
		group = "core"
	}
	namespace := identity.Namespace
	if namespace == "" {
		namespace = "_cluster"
	}

	return path.Join(
		state.EncodePathSegment(group),
		state.EncodePathSegment(identity.Version),
		state.EncodePathSegment(identity.Resource),
		state.EncodePathSegment(namespace),
		state.EncodePathSegment(identity.Name)+".yaml",
	), nil
}

// Write serializes one object and atomically replaces its destination file
// only when the serialized bytes differ from the existing file.
func (s *DirectorySink) Write(ctx context.Context, object state.Object) (bool, error) {
	if s == nil {
		return false, errors.New("export directory sink is nil")
	}
	if ctx == nil {
		return false, errors.New("export directory context is required")
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}

	data, err := codec.Marshal(object)
	if err != nil {
		return false, err
	}

	path, err := s.objectPath(object.Identity)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("create object directory: %w", err)
	}

	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read existing object %q: %w", path, err)
	}

	changed := !bytes.Equal(old, data)
	if !changed {
		// Avoid touching mtime or replacing the inode when the canonical bytes are unchanged;
		// this is what keeps repeated Git backups noise-free.
		return false, nil
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".kube-dump-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temporary object file: %w", err)
	}

	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()

	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("set object file mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("write object file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close object file: %w", err)
	}

	// Publish only after the complete temporary file is closed;
	// readers never observe a partially written YAML.
	if err := fileutil.ReplaceFile(temporaryName, path); err != nil {
		return false, fmt.Errorf("replace object file: %w", err)
	}

	return changed, nil
}

// objectPath maps an object identity to a versioned canonical YAML path.
func (s *DirectorySink) objectPath(identity state.Identity) (string, error) {
	relative, err := CanonicalObjectPath(identity)
	if err != nil {
		return "", err
	}

	return filepath.Join(s.root, filepath.FromSlash(relative)), nil
}
