// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"go.yaml.in/yaml/v3"
)

const (
	resourceOwnershipManifestVersion  = 1
	maxResourceOwnershipManifestBytes = 1 << 20
	resourceOwnershipManifestPath     = ".kube-dump/ownership.yaml"
)

// resourceOwnershipManifest records the canonical files in one resource destination.
// It is stored inside the resource tree so pruning metadata
// and resource files become visible through the same directory publication.
type resourceOwnershipManifest struct {
	Profile string   `yaml:"profile"`
	Paths   []string `yaml:"paths"`
	Version int      `yaml:"version"`
}

// reconcileLocalResourceOwnership prunes only files listed by the previous manifest,
// then publishes the new manifest below the supplied resource tree.
func reconcileLocalResourceOwnership(
	resourceTree, profileSource string,
	result kube.CollectionResult,
	prune bool,
) error {
	if resourceTree == "" {
		return errors.New("resource tree is required")
	}
	if !resourceCollectionComplete(result) {
		logResourcePruneSkipped(resourceTree, result)
		return nil
	}

	manifest, err := newResourceOwnershipManifest(profileSource, result)
	if err != nil {
		return err
	}

	manifestFile := localResourceOwnershipManifestPath(resourceTree)
	previous, found, err := readLocalResourceOwnershipManifest(manifestFile)
	if err != nil {
		return err
	}
	if found {
		if prune {
			if err := removeUnownedLocalResources(resourceTree, previous, manifest); err != nil {
				return err
			}
		} else {
			manifest.Paths = unionResourceOwnershipPaths(previous.Paths, manifest.Paths)
		}
	}

	return writeLocalResourceOwnershipManifest(manifestFile, manifest)
}

// unionResourceOwnershipPaths keeps ownership history when pruning is disabled.
// Files retained by an ordinary backup must remain eligible for a later prune.
func unionResourceOwnershipPaths(previous, current []string) []string {
	paths := make([]string, 0, len(previous)+len(current))
	paths = append(paths, previous...)
	paths = append(paths, current...)
	sort.Strings(paths)
	return slices.Compact(paths)
}

// resourceCollectionComplete permits pruning only when every selected job was handled without warnings.
// A partial run must never make old files disappear.
func resourceCollectionComplete(result kube.CollectionResult) bool {
	return result.Failed == 0 && result.WarningTotal() == 0
}

// newResourceOwnershipManifest builds and validates the current destination manifest.
func newResourceOwnershipManifest(
	profileSource string,
	result kube.CollectionResult,
) (resourceOwnershipManifest, error) {
	paths := make([]string, 0, len(result.Identities))
	for _, identity := range result.Identities {
		objectPath, err := export.CanonicalObjectPath(identity)
		if err != nil {
			return resourceOwnershipManifest{}, fmt.Errorf("build resource ownership path: %w", err)
		}
		paths = append(paths, objectPath)
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)

	manifest := resourceOwnershipManifest{
		Version: resourceOwnershipManifestVersion,
		Profile: profileSource,
		Paths:   paths,
	}
	if err := validateResourceOwnershipManifest(manifest); err != nil {
		return resourceOwnershipManifest{}, err
	}

	return manifest, nil
}

// validateResourceOwnershipManifest rejects forged or ambiguous paths before pruning.
func validateResourceOwnershipManifest(manifest resourceOwnershipManifest) error {
	if manifest.Version != resourceOwnershipManifestVersion {
		return fmt.Errorf("unsupported resource ownership manifest version %d", manifest.Version)
	}

	previous := ""
	for _, objectPath := range manifest.Paths {
		if !state.IsCanonicalObjectPath(objectPath) || path.Clean(objectPath) != objectPath {
			return fmt.Errorf("invalid resource ownership path %q", objectPath)
		}
		if objectPath == previous {
			return fmt.Errorf("duplicate resource ownership path %q", objectPath)
		}
		previous = objectPath
	}

	return nil
}

// localResourceOwnershipManifestPath returns the fixed manifest location below the destination root.
func localResourceOwnershipManifestPath(root string) string {
	return filepath.Join(root, filepath.FromSlash(resourceOwnershipManifestPath))
}

// readLocalResourceOwnershipManifest reads one manifest and treats absence as a first run.
func readLocalResourceOwnershipManifest(filename string) (resourceOwnershipManifest, bool, error) {
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return resourceOwnershipManifest{}, false, nil
	}
	if err != nil {
		return resourceOwnershipManifest{}, false, fmt.Errorf("inspect resource ownership manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return resourceOwnershipManifest{}, false, fmt.Errorf("resource ownership manifest is not a regular file: %q", filename)
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		return resourceOwnershipManifest{}, false, fmt.Errorf("read resource ownership manifest: %w", err)
	}
	if len(data) > maxResourceOwnershipManifestBytes {
		return resourceOwnershipManifest{}, false, fmt.Errorf("resource ownership manifest exceeds %d bytes", maxResourceOwnershipManifestBytes)
	}

	var manifest resourceOwnershipManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return resourceOwnershipManifest{}, false, fmt.Errorf("decode resource ownership manifest: %w", err)
	}
	if err := validateResourceOwnershipManifest(manifest); err != nil {
		return resourceOwnershipManifest{}, false, err
	}

	return manifest, true, nil
}

// writeLocalResourceOwnershipManifest publishes the manifest atomically.
func writeLocalResourceOwnershipManifest(filename string, manifest resourceOwnershipManifest) error {
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode resource ownership manifest: %w", err)
	}

	if len(data) > maxResourceOwnershipManifestBytes {
		return fmt.Errorf(
			"resource ownership manifest exceeds %d bytes",
			maxResourceOwnershipManifestBytes,
		)
	}

	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return fmt.Errorf("create resource ownership directory: %w", err)
	}
	if err := writeAtomic(filename, func(output io.Writer) error {
		_, err := output.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("write resource ownership manifest: %w", err)
	}

	return nil
}

// removeUnownedLocalResources removes paths that disappeared from the previous manifest.
func removeUnownedLocalResources(root string, previous, current resourceOwnershipManifest) error {
	currentPaths := make(map[string]struct{}, len(current.Paths))
	for _, objectPath := range current.Paths {
		currentPaths[objectPath] = struct{}{}
	}

	for _, objectPath := range previous.Paths {
		if _, exists := currentPaths[objectPath]; exists {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(objectPath))
		if err := validatePruneTarget(root, target); err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale resource %q: %w", objectPath, err)
		}
	}

	return nil
}

// validatePruneTarget rejects symlinked path components before deleting a file.
func validatePruneTarget(root, target string) error {
	if err := ensureBelow(root, target); err != nil {
		return err
	}

	relative, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve prune target: %w", err)
	}

	current := root
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect prune target %q: %w", target, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to prune through symlink %q", current)
		}
	}

	return nil
}

// logResourcePruneSkipped makes the fail-closed behavior visible
// without turning a best-effort resource backup into a second error.
func logResourcePruneSkipped(destination string, result kube.CollectionResult) {
	log.Warn().
		Str("component", "backup").
		Str("destination", destination).
		Int("failed", result.Failed).
		Int("warnings", result.WarningTotal()).
		Int("warnings_omitted", result.OmittedWarningDetails()).
		Msg("skip resource pruning after incomplete collection")
}
