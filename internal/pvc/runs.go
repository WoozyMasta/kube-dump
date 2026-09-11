// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/state"
)

const (
	pvcTimestampLayout = "20060102T150405Z"
	pvcOperationLayout = "20060102T150405.000000000Z"
)

// NewOperationID returns a high-resolution identifier
// for temporary Kubernetes resources created during one command invocation.
// It is not part of the persisted PVC artifact layout.
func NewOperationID() string {
	return time.Now().UTC().Format(pvcOperationLayout)
}

// NewTimestamp returns a compact, sortable UTC directory name for one PVC artifact.
// The timestamp is intentionally precise to seconds;
// a collision for the same PVC is handled by createTimestampDirectory
// with a numeric suffix instead of exposing subsecond noise in the layout.
func NewTimestamp() string {
	return time.Now().UTC().Format(pvcTimestampLayout)
}

// NewRevision returns a sortable and process-independent PVC artifact revision.
// The nanosecond timestamp orders captures made in one second;
// the random suffix keeps concurrent direct-S3 captures unique.
func NewRevision() (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate PVC artifact revision: %w", err)
	}

	return time.Now().UTC().Format(pvcOperationLayout) + "-" + hex.EncodeToString(suffix), nil
}

// RotateArtifacts removes PVC artifact directories older than keep for every namespace/PVC pair below root.
// Unrelated files and directories are ignored.
func RotateArtifacts(root string, keep int) (int, error) {
	if root == "" {
		return 0, errors.New("PVC backup root is required")
	}
	if keep < 0 {
		return 0, errors.New("PVC artifact retention cannot be negative")
	}

	// Zero means unlimited retention; do not scan or mutate the store in that mode.
	if keep == 0 {
		return 0, nil
	}

	volumeRoot := filepath.Join(root, "volumes")
	store := VolumeStore{root: root}
	namespaces, err := os.ReadDir(volumeRoot)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read PVC artifact root: %w", err)
	}

	removed := 0

	// Retention is evaluated independently for every encoded namespace/PVC pair,
	// so backups from different claims never evict one another.
	for _, namespace := range namespaces {
		if !namespace.IsDir() {
			continue
		}

		namespacePath := filepath.Join(volumeRoot, namespace.Name())
		claims, err := os.ReadDir(namespacePath)
		if err != nil {
			return removed, fmt.Errorf("read PVC namespace directory %q: %w", namespace.Name(), err)
		}

		for _, claim := range claims {
			if !claim.IsDir() {
				continue
			}

			claimPath := filepath.Join(namespacePath, claim.Name())
			namespaceName, err := state.DecodePathSegment(namespace.Name())
			if err != nil {
				continue
			}

			claimName, err := state.DecodePathSegment(claim.Name())
			if err != nil {
				continue
			}

			claim := Ref{
				Namespace: namespaceName,
				Name:      claimName,
			}
			if err := claim.Validate(); err != nil {
				continue
			}

			artifacts, _, err := store.listArtifactsForPVC(claim)
			if err != nil {
				return removed, err
			}

			timestamps := make([]string, 0, len(artifacts))
			for _, artifact := range artifacts {
				timestamps = append(timestamps, filepath.Base(artifact.DirectoryPath))
			}
			sort.Slice(timestamps, func(left, right int) bool {
				return CompareRevisions(timestamps[left], timestamps[right]) > 0
			})

			for _, timestamp := range timestamps[min(keep, len(timestamps)):] {
				if err := os.RemoveAll(filepath.Join(claimPath, timestamp)); err != nil {
					return removed, fmt.Errorf("remove expired PVC artifact %q: %w", timestamp, err)
				}

				removed++
			}
		}
	}

	return removed, nil
}

// CompareRevisions compares two valid PVC artifact revisions by capture time,
// then by the complete revision name for deterministic tie-breaking.
func CompareRevisions(left, right string) int {
	leftTime, leftValid := parseRevision(left)
	rightTime, rightValid := parseRevision(right)
	if leftValid && rightValid && !leftTime.Equal(rightTime) {
		if leftTime.Before(rightTime) {
			return -1
		}

		return 1
	}

	return strings.Compare(left, right)
}

// IsTimestamp reports whether value is a supported PVC artifact revision.
// It accepts second-based local revisions and both old and new direct-S3 revisions.
func IsTimestamp(value string) bool {
	_, ok := parseRevision(value)
	return ok
}

// parseRevision parses a local or direct-S3 revision without inspecting storage.
func parseRevision(value string) (time.Time, bool) {
	base := value
	timestamp, suffix, hasSuffix := strings.Cut(value, "-")

	if hasSuffix {
		base = timestamp
		switch len(suffix) {
		case 2:
			if strings.Contains(base, ".") {
				return time.Time{}, false
			}

			number, err := strconv.Atoi(suffix)
			if err != nil || number < 1 || number > 99 {
				return time.Time{}, false
			}

		case 16:
			if _, err := hex.DecodeString(suffix); err != nil {
				return time.Time{}, false
			}

		default:
			return time.Time{}, false
		}
	}

	if strings.Contains(base, ".") && !hasSuffix {
		return time.Time{}, false
	}

	layout := pvcTimestampLayout
	if strings.Contains(base, ".") {
		layout = pvcOperationLayout
	}
	parsed, err := time.Parse(layout, base)
	if err != nil || parsed.UTC().Format(layout) != base {
		return time.Time{}, false
	}

	return parsed.UTC(), true
}

// createStagingDirectory creates a hidden artifact directory that is invisible to readers.
func createStagingDirectory(base string) (string, string, error) {
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", "", fmt.Errorf("create PVC artifact directory: %w", err)
	}

	timestamp := NewTimestamp()
	directory, err := os.MkdirTemp(base, "."+timestamp+"-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("create PVC artifact staging directory: %w", err)
	}

	// #nosec G302 -- the staging directory is intentionally group-traversable.
	if err := os.Chmod(directory, 0o750); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", fmt.Errorf("set PVC artifact staging directory permissions: %w", err)
	}

	return directory, timestamp, nil
}

// publishStagingDirectory atomically exposes a completed artifact under a timestamped name.
func publishStagingDirectory(staging, base, timestamp string) error {
	// The data and metadata files are synced independently.
	// Sync their directory entries before exposing the whole artifact by rename.
	if err := fileutil.SyncDirectory(staging); err != nil {
		return fmt.Errorf("sync PVC artifact staging directory: %w", err)
	}

	for suffix := 0; suffix <= 99; suffix++ {
		name := timestamp
		if suffix > 0 {
			name = fmt.Sprintf("%s-%02d", timestamp, suffix)
		}

		target := filepath.Join(base, name)
		if err := os.Rename(staging, target); err == nil {
			if err := fileutil.SyncDirectory(base); err != nil {
				return fmt.Errorf("sync PVC artifact directory: %w", err)
			}

			return nil
		} else if !os.IsExist(err) {
			// Windows reports an existing destination as access denied for directory renames.
			if _, statErr := os.Lstat(target); statErr != nil {
				return fmt.Errorf("publish PVC artifact directory: %w", err)
			}
		}
	}

	return errors.New("publish PVC artifact directory: more than 99 captures in one second")
}
