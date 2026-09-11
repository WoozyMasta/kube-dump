// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"go.yaml.in/yaml/v3"
)

const (
	// MetadataFilename is the manifest stored beside every PVC data stream.
	MetadataFilename    = "metadata.yaml"
	maxPVCMetadataBytes = 1 << 20
)

// VolumeStore persists PVC data artifacts below a backup root.
type VolumeStore struct {
	// root is the absolute backup root containing PVC artifacts.
	root string
}

// Artifact describes one locally stored PVC stream and its validated metadata.
type Artifact struct {
	// PVC identifies the source or target PVC.
	PVC Ref
	// DirectoryPath is the timestamped directory containing this artifact.
	DirectoryPath string
	// DataPath is the local stream or snapshot reference path.
	DataPath string
	// Metadata is the validated artifact contract.
	Metadata Metadata
	// SizeBytes is the on-disk artifact size.
	SizeBytes int64
}

// NewVolumeStore creates a volume artifact store rooted at backupRoot.
func NewVolumeStore(backupRoot string) (VolumeStore, error) {
	if backupRoot == "" {
		return VolumeStore{}, errors.New("volume store root is required")
	}

	root, err := filepath.Abs(backupRoot)
	if err != nil {
		return VolumeStore{}, fmt.Errorf("resolve volume store root: %w", err)
	}

	return VolumeStore{root: root}, nil
}

// Write streams one PVC artifact and publishes metadata only after data succeeds.
//
// Data and metadata are written through private temporary files and renamed into place only after close.
// A cancelled or failed backup therefore cannot leave an artifact that looks complete.
func (s VolumeStore) Write(ctx context.Context, pvc Ref, strategy BackupStrategy) (Metadata, error) {
	metadata, _, err := s.WriteWithStats(ctx, pvc, strategy)
	return metadata, err
}

// WriteWithStats writes one PVC artifact and returns its on-disk payload size.
// The metadata size remains the uncompressed source size;
// SizeBytes is the actual stored file size after compression and optional age encryption.
func (s VolumeStore) WriteWithStats(ctx context.Context, pvc Ref, strategy BackupStrategy) (Metadata, int64, error) {
	if strategy == nil {
		return Metadata{}, 0, errors.New("volume backup strategy is required")
	}
	if ctx == nil {
		return Metadata{}, 0, errors.New("volume store context is required")
	}

	baseDirectory, err := s.directory(pvc)
	if err != nil {
		return Metadata{}, 0, err
	}

	directory, timestamp, err := createStagingDirectory(baseDirectory)
	if err != nil {
		return Metadata{}, 0, err
	}
	removeDirectory := true
	defer func() {
		if removeDirectory {
			_ = os.RemoveAll(directory)
		}
	}()

	dataFile, dataName, err := createTemporary(directory, "data-stream")
	if err != nil {
		return Metadata{}, 0, err
	}
	removeData := true
	defer func() {
		if removeData {
			_ = os.Remove(dataName)
		}
	}()

	metadata, backupErr := strategy.Backup(ctx, pvc, dataFile)
	if syncErr := dataFile.Sync(); syncErr != nil {
		backupErr = errors.Join(backupErr, fmt.Errorf("sync PVC artifact data: %w", syncErr))
	}
	if closeErr := dataFile.Close(); closeErr != nil {
		backupErr = errors.Join(backupErr, closeErr)
	}
	if backupErr != nil && !IsCleanupOnly(backupErr) {
		return Metadata{}, 0, fmt.Errorf("write PVC artifact data: %w", backupErr)
	}
	if err := metadata.Validate(); err != nil {
		return Metadata{}, 0, fmt.Errorf("validate PVC artifact metadata: %w", err)
	}

	dataTarget := filepath.Join(directory, volumeDataName(metadata.Compression, metadata.Encrypted))
	if err := publishFile(dataName, dataTarget); err != nil {
		return Metadata{}, 0, err
	}
	dataInfo, err := os.Stat(dataTarget)
	if err != nil {
		return Metadata{}, 0, fmt.Errorf("stat PVC artifact data: %w", err)
	}

	removeData = false
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		return Metadata{}, 0, fmt.Errorf("encode PVC artifact metadata: %w", err)
	}
	if err := writeAtomic(filepath.Join(directory, MetadataFilename), metadataData, 0o640); err != nil {
		return Metadata{}, 0, err
	}
	if err := publishStagingDirectory(directory, baseDirectory, timestamp); err != nil {
		return Metadata{}, 0, err
	}

	removeDirectory = false
	return metadata, dataInfo.Size(), backupErr
}

// ReadMetadata loads and validates the metadata for one listed PVC artifact.
func (s VolumeStore) ReadMetadata(artifact Artifact) (Metadata, error) {
	directory, err := s.artifactDirectory(artifact)
	if err != nil {
		return Metadata{}, err
	}

	metadataPath := filepath.Join(directory, MetadataFilename)
	if err := validateArtifactFile(metadataPath); err != nil {
		return Metadata{}, err
	}

	file, err := os.Open(metadataPath)
	if err != nil {
		return Metadata{}, fmt.Errorf("open PVC artifact metadata: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxPVCMetadataBytes+1))
	if err != nil {
		return Metadata{}, fmt.Errorf("read PVC artifact metadata: %w", err)
	}
	if len(data) > maxPVCMetadataBytes {
		return Metadata{}, fmt.Errorf("PVC artifact metadata exceeds %d bytes", maxPVCMetadataBytes)
	}

	var metadata Metadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode PVC artifact metadata: %w", err)
	}
	if err := metadata.Validate(); err != nil {
		return Metadata{}, fmt.Errorf("validate PVC artifact metadata: %w", err)
	}

	return metadata, nil
}

// ArtifactDirectory returns the validated directory of one listed PVC artifact.
func (s VolumeStore) ArtifactDirectory(artifact Artifact) (string, error) {
	return s.artifactDirectory(artifact)
}

// OpenData opens one validated PVC artifact stream for consumers
// that process individual artifacts outside the backup command.
func (s VolumeStore) OpenData(artifact Artifact) (*os.File, error) {
	directory, err := s.artifactDirectory(artifact)
	if err != nil {
		return nil, err
	}

	metadata, err := s.ReadMetadata(artifact)
	if err != nil {
		return nil, err
	}

	dataPath := filepath.Join(directory, volumeDataName(metadata.Compression, metadata.Encrypted))
	if err := validateArtifactFile(dataPath); err != nil {
		return nil, err
	}

	file, err := os.Open(dataPath)
	if err != nil {
		return nil, fmt.Errorf("open PVC artifact data: %w", err)
	}

	return file, nil
}

// FindArtifact selects one validated artifact for a PVC.
// An empty or latest revision selects the newest timestamped artifact.
func (s VolumeStore) FindArtifact(pvc Ref, revision string) (Artifact, error) {
	if err := pvc.Validate(); err != nil {
		return Artifact{}, err
	}

	revision = strings.TrimSpace(revision)
	latest := revision == "" || revision == "latest"
	if !latest {
		if !IsTimestamp(revision) {
			return Artifact{}, fmt.Errorf("PVC artifact revision %q is invalid", revision)
		}

		artifact, err := s.readArtifact(pvc, filepath.Join(s.directoryPath(pvc), revision))
		if err != nil {
			if os.IsNotExist(err) {
				return Artifact{}, fmt.Errorf("PVC artifact %s/%s/%s was not found", pvc.Namespace, pvc.Name, revision)
			}

			return Artifact{}, fmt.Errorf("read PVC artifact %s/%s/%s: %w", pvc.Namespace, pvc.Name, revision, err)
		}

		return artifact, nil
	}

	artifacts, skipped, err := s.listArtifactsForPVC(pvc)
	if err != nil {
		return Artifact{}, err
	}
	var selected Artifact
	for _, artifact := range artifacts {
		if selected.DirectoryPath == "" || CompareRevisions(
			filepath.Base(artifact.DirectoryPath),
			filepath.Base(selected.DirectoryPath),
		) > 0 {
			selected = artifact
		}
	}

	if selected.DirectoryPath != "" {
		return selected, nil
	}

	if latest {
		if len(skipped) > 0 {
			return Artifact{}, fmt.Errorf(
				"no valid PVC artifacts found for %s/%s; skipped: %s",
				pvc.Namespace,
				pvc.Name,
				strings.Join(skipped, "; "))
		}

		return Artifact{}, fmt.Errorf("no PVC artifacts found for %s/%s", pvc.Namespace, pvc.Name)
	}

	return Artifact{}, errors.New("PVC artifact was not found")
}

// listArtifactsForPVC scans only one PVC's artifact directory.
// Invalid or incomplete revisions are skipped
// so an unrelated broken PVC cannot block an exact or latest restore candidate.
func (s VolumeStore) listArtifactsForPVC(pvc Ref) ([]Artifact, []string, error) {
	base := s.directoryPath(pvc)
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read PVC artifacts for %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}

	artifacts := make([]Artifact, 0, len(entries))
	skipped := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !IsTimestamp(entry.Name()) {
			continue
		}

		directory := filepath.Join(base, entry.Name())
		artifact, err := s.readArtifact(pvc, directory)
		if err != nil {
			if len(skipped) < 4 {
				skipped = append(skipped, entry.Name()+": "+err.Error())
			}
			continue
		}

		artifacts = append(artifacts, artifact)
	}

	return artifacts, skipped, nil
}

// readArtifact validates one complete local artifact without scanning sibling revisions.
func (s VolumeStore) readArtifact(pvc Ref, directory string) (Artifact, error) {
	artifact := Artifact{PVC: pvc, DirectoryPath: directory}
	metadata, err := s.ReadMetadata(artifact)
	if err != nil {
		return Artifact{}, err
	}

	dataPath := filepath.Join(directory, volumeDataName(metadata.Compression, metadata.Encrypted))
	if err := validateArtifactFile(dataPath); err != nil {
		return Artifact{}, err
	}

	info, err := os.Stat(dataPath)
	if err != nil {
		return Artifact{}, fmt.Errorf("stat PVC artifact data: %w", err)
	}

	artifact.DataPath = dataPath
	artifact.Metadata = metadata
	artifact.SizeBytes = info.Size()
	return artifact, nil
}

// DataFilename returns the portable data filename for a PVC archive codec and encryption state.
// Remote movers use the same naming contract as local stores.
func DataFilename(compression string, encrypted bool) string {
	return volumeDataName(compression, encrypted)
}

// ListArtifacts validates and lists all PVC artifacts below the store root.
//
// The result is sorted by PVC identity
// so callers can produce stable output regardless of filesystem enumeration order.
func (s VolumeStore) ListArtifacts() ([]Artifact, error) {
	root := filepath.Join(s.root, "volumes")
	namespaceEntries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read volume artifacts: %w", err)
	}

	artifacts := make([]Artifact, 0)

	// The directory shape is volumes/<namespace>/<pvc>/<timestamp>.
	// Validate every segment while walking it so malformed
	// or manually edited trees do not become valid-looking restore candidates.
	for _, namespaceEntry := range namespaceEntries {
		if !namespaceEntry.IsDir() {
			return nil, fmt.Errorf("invalid volume namespace directory %q", namespaceEntry.Name())
		}

		namespace, err := state.DecodePathSegment(namespaceEntry.Name())
		if err != nil {
			return nil, fmt.Errorf("decode volume namespace directory %q: %w", namespaceEntry.Name(), err)
		}

		pvcEntries, err := os.ReadDir(filepath.Join(root, namespaceEntry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read volume namespace %q: %w", namespace, err)
		}

		for _, pvcEntry := range pvcEntries {
			if !pvcEntry.IsDir() {
				return nil, fmt.Errorf("invalid volume PVC directory %q", pvcEntry.Name())
			}

			name, err := state.DecodePathSegment(pvcEntry.Name())
			if err != nil {
				return nil, fmt.Errorf("decode volume PVC directory %q: %w", pvcEntry.Name(), err)
			}

			ref := Ref{Namespace: namespace, Name: name}
			if err := ref.Validate(); err != nil {
				return nil, fmt.Errorf("validate PVC artifact path %q/%q: %w", namespace, name, err)
			}

			artifactEntries, err := os.ReadDir(filepath.Join(root, namespaceEntry.Name(), pvcEntry.Name()))
			if err != nil {
				return nil, fmt.Errorf("read PVC artifact timestamps for %s/%s: %w", namespace, name, err)
			}

			for _, artifactEntry := range artifactEntries {
				if !artifactEntry.IsDir() || !IsTimestamp(artifactEntry.Name()) {
					continue
				}

				// Metadata selects the expected payload filename.
				// Do not infer the codec from arbitrary files in the timestamp directory.
				directory := filepath.Join(root, namespaceEntry.Name(), pvcEntry.Name(), artifactEntry.Name())
				artifact := Artifact{PVC: ref, DirectoryPath: directory}
				metadata, err := s.ReadMetadata(artifact)
				if err != nil {
					return nil, err
				}

				dataPath := filepath.Join(directory, volumeDataName(metadata.Compression, metadata.Encrypted))
				info, err := os.Lstat(dataPath)
				if err != nil {
					return nil, fmt.Errorf("stat PVC artifact %s/%s/%s: %w", namespace, name, artifactEntry.Name(), err)
				}
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("PVC artifact data is not a regular file: %s/%s/%s", namespace, name, artifactEntry.Name())
				}

				artifact.DataPath = dataPath
				artifact.Metadata = metadata
				artifact.SizeBytes = info.Size()
				artifacts = append(artifacts, artifact)
			}
		}
	}

	sort.Slice(artifacts, func(left, right int) bool {
		leftKey := artifacts[left].PVC.Namespace + "/" + artifacts[left].PVC.Name + "/" + artifacts[left].DirectoryPath
		rightKey := artifacts[right].PVC.Namespace + "/" + artifacts[right].PVC.Name + "/" + artifacts[right].DirectoryPath
		return leftKey < rightKey
	})

	return artifacts, nil
}

// volumeDataName returns the portable artifact filename for one codec and envelope.
// The age suffix is part of the filename contract
// so consumers can detect encryption without opening the payload first.
func volumeDataName(algorithm string, encrypted bool) string {
	name := "snapshot.json"
	if algorithm == string(compress.Gzip) {
		name = "data.tar.gz"
	} else if algorithm != "none" {
		name = "data.tar.zst"
	}

	if encrypted {
		name += ".age"
	}

	return name
}

// directory returns the safe deterministic parent directory for a PVC's timestamped artifacts.
func (s VolumeStore) directory(pvc Ref) (string, error) {
	if err := pvc.Validate(); err != nil {
		return "", err
	}

	directory := s.directoryPath(Ref{Namespace: pvc.Namespace, Name: pvc.Name})
	if err := validateArtifactDirectory(s.root, directory); err != nil {
		return "", err
	}

	return directory, nil
}

// artifactDirectory validates the path carried by a listed artifact
// and prevents callers from opening files outside the store root
// or the expected PVC/timestamp hierarchy.
func (s VolumeStore) artifactDirectory(artifact Artifact) (string, error) {
	if err := artifact.PVC.Validate(); err != nil {
		return "", err
	}
	if artifact.DirectoryPath == "" {
		return "", errors.New("PVC artifact directory is required")
	}

	directory := filepath.Clean(artifact.DirectoryPath)
	base := s.directoryPath(artifact.PVC)
	relative, err := filepath.Rel(base, directory)
	if err != nil || relative == "." || relative == ".." || strings.Contains(relative, string(filepath.Separator)) || !IsTimestamp(filepath.Base(directory)) {
		return "", fmt.Errorf("invalid PVC artifact directory: %q", artifact.DirectoryPath)
	}
	if err := validateArtifactDirectory(s.root, directory); err != nil {
		return "", err
	}

	return directory, nil
}

// directoryPath returns a path for an already validated PVC reference.
func (s VolumeStore) directoryPath(pvc Ref) string {
	return filepath.Join(
		s.root,
		"volumes",
		state.EncodePathSegment(pvc.Namespace),
		state.EncodePathSegment(pvc.Name),
	)
}

// validateArtifactDirectory rejects symlinked path components
// below the backup root before metadata or data files are accessed.
func validateArtifactDirectory(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("volume artifact path escapes store root: %q", target)
	}

	current := root
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect volume artifact path %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("volume artifact path traverses symlink: %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("volume artifact path is not a directory: %q", current)
		}
	}

	return nil
}

// validateArtifactFile rejects symlinks and non-regular artifact payloads.
func validateArtifactFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect volume artifact file %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("volume artifact file is not regular: %q", path)
	}

	return nil
}

// createTemporary opens a private temporary file in the destination directory.
func createTemporary(directory, name string) (*os.File, string, error) {
	file, err := os.CreateTemp(directory, "."+name+"-*.tmp")
	if err != nil {
		return nil, "", fmt.Errorf("create temporary volume artifact: %w", err)
	}

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, "", fmt.Errorf("set temporary volume artifact permissions: %w", err)
	}

	return file, file.Name(), nil
}

// publishFile atomically installs a completed data stream.
func publishFile(source, target string) error {
	if err := replaceFile(source, target); err != nil {
		return fmt.Errorf("publish PVC artifact data: %w", err)
	}

	return nil
}

// writeAtomic writes small metadata bytes through a synced temporary file.
func writeAtomic(target string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(target)
	file, name, err := createTemporary(directory, filepath.Base(target))
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write atomic metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync atomic metadata: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set atomic metadata permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close atomic metadata: %w", err)
	}
	if err := replaceFile(name, target); err != nil {
		return fmt.Errorf("publish atomic metadata: %w", err)
	}

	remove = false
	return nil
}

// replaceFile preserves the previous artifact during Unix replacement.
// The Windows fallback is necessarily a remove-then-rename sequence
// because the platform does not allow replacing an open destination with os.Rename.
func replaceFile(source, target string) error {
	return fileutil.ReplaceFile(source, target)
}
