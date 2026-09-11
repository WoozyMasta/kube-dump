// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package pvc defines storage backup strategy contracts
// and the helper-agent protocol used to stream portable PVC artifacts.
package pvc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const metadataVersion = 1

// Strategy identifies the mechanism used for PVC data transfer.
type Strategy string

const (
	// Pod streams data through a temporary helper Pod.
	Pod Strategy = "pod"
	// SnapshotCopy creates a portable archive from a CSI snapshot.
	SnapshotCopy Strategy = "snapshot-copy"
)

// Ref identifies a namespaced PersistentVolumeClaim.
type Ref struct {
	// Namespace is the claim namespace.
	Namespace string `json:"namespace" yaml:"namespace"`
	// Name is the claim name.
	Name string `json:"name" yaml:"name"`
}

// Metadata describes a versioned PVC data stream stored in an archive.
//
// SizeBytes and ContentSHA256 refer to the uncompressed payload.
// The supported strategies produce artifacts that can be transferred to another cluster.
type Metadata struct {
	// Strategy identifies the volume transfer mechanism.
	Strategy Strategy `json:"strategy" yaml:"strategy"`

	// Compression identifies the artifact payload codec.
	Compression string `json:"compression" yaml:"compression"`

	// ContentSHA256 authenticates the uncompressed payload bytes.
	ContentSHA256 string `json:"contentSha256,omitempty" yaml:"contentSha256,omitempty"`

	// Version identifies the PVC metadata schema.
	Version int `json:"version" yaml:"version"`

	// SizeBytes records the uncompressed payload size.
	SizeBytes int64 `json:"sizeBytes,omitempty" yaml:"sizeBytes,omitempty"`

	// Encrypted reports whether the artifact payload is encrypted.
	Encrypted bool `json:"encrypted" yaml:"encrypted"`

	// Portable reports whether the artifact can cross cluster boundaries.
	Portable bool `json:"portable" yaml:"portable"`
}

// CleanupWarning reports a temporary-resource cleanup failure
// after the data stream itself has completed successfully.
// The captured artifact remains valid.
type CleanupWarning struct {
	// Err contains the cleanup failure returned by Kubernetes.
	Err error
	// Resource identifies the temporary Kubernetes object that could not be removed.
	Resource string
}

// Error describes a cleanup failure without hiding the temporary resource name.
func (w *CleanupWarning) Error() string {
	if w == nil {
		return "PVC cleanup failed"
	}
	if w.Resource == "" {
		return fmt.Sprintf("PVC cleanup failed: %v", w.Err)
	}

	return fmt.Sprintf("cleanup %s failed: %v", w.Resource, w.Err)
}

// Unwrap exposes the Kubernetes cleanup error to errors.Is and errors.As.
func (w *CleanupWarning) Unwrap() error {
	if w == nil {
		return nil
	}

	return w.Err
}

// IsCleanupOnly reports whether err contains only cleanup warnings.
// It returns false when the same error also contains a capture or publication failure.
func IsCleanupOnly(err error) bool {
	return cleanupOnly(err)
}

// BackupStrategy probes and streams a PVC into a destination archive.
type BackupStrategy interface {
	Backup(ctx context.Context, pvc Ref, dst io.Writer) (Metadata, error)
}

// Validate checks that the PVC reference is safe to use in API paths.
func (r Ref) Validate() error {
	if r.Namespace == "" || r.Name == "" {
		return errors.New("pvc namespace and name are required")
	}

	if strings.ContainsAny(r.Namespace, "/\\") || strings.ContainsAny(r.Name, "/\\") {
		return fmt.Errorf("pvc reference contains a path separator: %q/%q", r.Namespace, r.Name)
	}

	return nil
}

// NewMetadata creates metadata with the current format version.
func NewMetadata(strategy Strategy, compression string) Metadata {
	return Metadata{Version: metadataVersion, Strategy: strategy, Compression: compression}
}

// Validate checks the versioned PVC archive metadata contract.
//
// Validation is intentionally strict because these fields control
// how bytes are decoded and whether the artifact is complete and portable.
func (m Metadata) Validate() error {
	if m.Version != metadataVersion {
		return fmt.Errorf("unsupported pvc metadata version %d", m.Version)
	}
	if err := m.Strategy.Validate(); err != nil {
		return err
	}

	if m.Compression == "" {
		return errors.New("pvc metadata compression is required")
	}

	if m.Strategy == SnapshotCopy && !m.Portable {
		return errors.New("snapshot-copy metadata must be portable")
	}

	if m.SizeBytes < 0 {
		return errors.New("pvc metadata size must not be negative")
	}

	if len(m.ContentSHA256) != 64 {
		return errors.New("portable PVC metadata must contain a SHA-256 digest")
	}
	if _, err := hex.DecodeString(m.ContentSHA256); err != nil {
		return fmt.Errorf("decode PVC metadata SHA-256: %w", err)
	}

	return nil
}

// Validate checks whether the strategy name is supported.
func (s Strategy) Validate() error {
	switch s {
	case Pod, SnapshotCopy:
		return nil
	default:
		return fmt.Errorf("unsupported PVC strategy %q", s)
	}
}

// cleanupOnly recursively checks joined and wrapped errors for non-cleanup failures.
func cleanupOnly(err error) bool {
	if err == nil {
		return false
	}

	if _, ok := err.(*CleanupWarning); ok {
		return true
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}

		for _, cause := range causes {
			if !cleanupOnly(cause) {
				return false
			}
		}

		return true
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cleanupOnly(wrapped.Unwrap())
	}

	return false
}

// UnmarshalText parses a PVC strategy from CLI or configuration text.
func (s *Strategy) UnmarshalText(value []byte) error {
	parsed := Strategy(string(value))
	if err := parsed.Validate(); err != nil {
		return err
	}

	*s = parsed
	return nil
}
