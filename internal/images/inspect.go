// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	maxImageLayoutMarkerBytes = 4 << 10
	maxImageJSONBytes         = 16 << 20
	maxImageIndexManifests    = 100_000
)

// Summary describes the content of a saved OCI Image Layout.
type Summary struct {
	// References contains source references recorded in index.json.
	References []string
	// Manifests is the number of top-level descriptors in index.json.
	Manifests int
	// Platforms is the number of descriptors carrying platform information.
	Platforms int
	// Blobs is the number of content-addressed files in blobs.
	Blobs int
	// Bytes is the total size of content-addressed blob files.
	Bytes int64
}

// Inspect validates the required OCI layout files and summarizes its index
// and content-addressed blobs without contacting Kubernetes or a registry.
func Inspect(root string) (Summary, error) {
	if root == "" {
		return Summary{}, errors.New("image layout root is required")
	}

	if info, err := os.Stat(root); err != nil {
		return Summary{}, fmt.Errorf("stat image layout: %w", err)
	} else if !info.IsDir() {
		return Summary{}, errors.New("image layout root must be a directory")
	}

	layoutData, err := readImageLayoutMarker(root)
	if err != nil {
		return Summary{}, fmt.Errorf("read OCI layout marker: %w", err)
	}

	indexData, err := readImageIndex(root)
	if err != nil {
		return Summary{}, fmt.Errorf("read image index: %w", err)
	}

	summary, err := InspectMetadata(layoutData, indexData)
	if err != nil {
		return Summary{}, err
	}
	if err := validateIndexGraph(root, indexData); err != nil {
		return Summary{}, fmt.Errorf("validate image layout: %w", err)
	}
	if err := countBlobs(root, &summary); err != nil {
		return Summary{}, err
	}

	return summary, nil
}

// Validate checks every descriptor reachable from index.json without contacting a registry.
// It is used before publishing a layout
// so a broken local cache cannot turn into a partially published image.
func Validate(root string) error {
	if root == "" {
		return errors.New("image layout root is required")
	}

	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat image layout: %w", err)
	}
	if !info.IsDir() {
		return errors.New("image layout root must be a directory")
	}

	layoutData, err := readImageLayoutMarker(root)
	if err != nil {
		return fmt.Errorf("read OCI layout marker: %w", err)
	}
	if err := validateLayoutMarkerData(layoutData); err != nil {
		return err
	}

	indexData, err := readImageIndex(root)
	if err != nil {
		return fmt.Errorf("read image index: %w", err)
	}

	if _, err := InspectMetadata(layoutData, indexData); err != nil {
		return err
	}
	if err := validateIndexGraph(root, indexData); err != nil {
		return fmt.Errorf("validate image graph: %w", err)
	}

	return nil
}

// readImageLayoutMarker bounds the fixed OCI metadata file before parsing it.
func readImageLayoutMarker(root string) ([]byte, error) {
	path := filepath.Join(root, "oci-layout")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxImageLayoutMarkerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageLayoutMarkerBytes {
		return nil, fmt.Errorf("OCI layout marker is larger than %d bytes", maxImageLayoutMarkerBytes)
	}

	return data, nil
}

// validateIndexGraph validates all top-level references
// and their reachable manifests, configs, indexes, and layers exactly once.
func validateIndexGraph(root string, indexData []byte) error {
	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("parse image index: %w", err)
	}
	if len(index.Manifests) == 0 {
		return errors.New("image index contains no manifests")
	}
	if err := validateIndexManifestCount(len(index.Manifests)); err != nil {
		return err
	}

	seen := make(map[string]struct{})
	for position, descriptor := range index.Manifests {
		if !isManifestDescriptor(descriptor.MediaType) {
			return fmt.Errorf(
				"top-level descriptor %d has unsupported media type %q",
				position, descriptor.MediaType,
			)
		}

		if err := inspectGraphDescriptor(
			root, descriptor, nil, seen, &PullProgress{},
		); err != nil {
			return fmt.Errorf("validate top-level descriptor %d: %w", position, err)
		}
	}

	return nil
}

// InspectMetadata summarizes OCI layout metadata without reading blobs.
func InspectMetadata(layoutData, indexData []byte) (Summary, error) {
	if err := validateLayoutMarkerData(layoutData); err != nil {
		return Summary{}, err
	}

	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return Summary{}, fmt.Errorf("parse image index: %w", err)
	}
	if err := validateIndexManifestCount(len(index.Manifests)); err != nil {
		return Summary{}, err
	}

	summary := Summary{Manifests: len(index.Manifests)}
	for _, descriptor := range index.Manifests {
		if descriptor.Platform != nil {
			summary.Platforms++
		}
		if reference, ok := descriptor.Annotations[ocispec.AnnotationRefName]; ok {
			summary.References = append(summary.References, reference)
		}
	}

	sort.Strings(summary.References)
	return summary, nil
}

// validateLayoutMarkerData checks the mandatory OCI layout marker bytes.
func validateLayoutMarkerData(data []byte) error {
	const supportedVersion = "1.0.0"

	var marker struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("parse OCI layout marker: %w", err)
	}
	if marker.ImageLayoutVersion != supportedVersion {
		return fmt.Errorf(
			"unsupported OCI layout version %q, want %q",
			marker.ImageLayoutVersion, supportedVersion,
		)
	}

	return nil
}

// countBlobs counts only regular files below the OCI content-addressed store.
func countBlobs(root string, summary *Summary) error {
	blobs := filepath.Join(root, "blobs")
	err := filepath.WalkDir(blobs, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		summary.Blobs++
		summary.Bytes += info.Size()

		return nil
	})
	if err != nil {
		return fmt.Errorf("scan OCI blobs: %w", err)
	}

	return nil
}

// inspectGraph summarizes the complete content graph rooted at descriptor.
// The existing set is captured before a pull
// and distinguishes transferred content from blobs already present in the local layout.
func inspectGraph(root string, descriptor ocispec.Descriptor, existing map[string]struct{}) (PullProgress, error) {
	progress := PullProgress{}
	seen := make(map[string]struct{})
	if err := inspectGraphDescriptor(root, descriptor, existing, seen, &progress); err != nil {
		return PullProgress{}, err
	}

	progress.Completed = progress.Total
	progress.BytesCompleted = progress.BytesTotal

	return progress, nil
}

// inspectGraphDescriptor walks one manifest graph node exactly once.
// It validates local content and accounts for logical, transferred, and reused sizes.
func inspectGraphDescriptor(
	root string,
	descriptor ocispec.Descriptor,
	existing, seen map[string]struct{},
	progress *PullProgress,
) error {
	if !isKnownImageDescriptor(descriptor.MediaType) {
		return fmt.Errorf("unsupported descriptor media type %q", descriptor.MediaType)
	}

	digest := descriptor.Digest.String()
	if digest == "" {
		return errors.New("image graph contains a descriptor without a digest")
	}
	if _, ok := seen[digest]; ok {
		return nil
	}

	seen[digest] = struct{}{}
	if err := validateDescriptorBlob(root, descriptor); err != nil {
		return fmt.Errorf("validate image graph blob %s: %w", digest, err)
	}

	size := max(descriptor.Size, 0)
	progress.Total++
	progress.BytesTotal += size
	if _, ok := existing[digest]; ok {
		progress.BytesReused += size
	} else {
		progress.BytesTransferred += size
	}

	if isImageLayer(descriptor.MediaType) {
		progress.Layers++
		progress.LayerBytesTotal += size
		if _, ok := existing[digest]; ok {
			progress.ReusedLayers++
			progress.LayerBytesReused += size
		} else {
			progress.LayerBytesTransferred += size
		}
	}

	if !isManifestDescriptor(descriptor.MediaType) {
		return nil
	}
	if descriptor.Size > maxImageJSONBytes {
		return fmt.Errorf(
			"image JSON descriptor is %d bytes, limit is %d",
			descriptor.Size, maxImageJSONBytes,
		)
	}

	data, err := os.ReadFile(blobPath(root, descriptor.Digest))
	if err != nil {
		return fmt.Errorf("read image graph blob %s: %w", digest, err)
	}

	switch descriptor.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var index ocispec.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("parse image index %s: %w", digest, err)
		}
		if err := validateIndexManifestCount(len(index.Manifests)); err != nil {
			return fmt.Errorf("validate image index %s: %w", digest, err)
		}

		for _, child := range index.Manifests {
			if err := inspectGraphDescriptor(root, child, existing, seen, progress); err != nil {
				return err
			}
		}

	default:
		var manifest ocispec.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("parse image manifest %s: %w", digest, err)
		}

		if err := inspectGraphDescriptor(root, manifest.Config, existing, seen, progress); err != nil {
			return err
		}
		for _, layer := range manifest.Layers {
			if err := inspectGraphDescriptor(root, layer, existing, seen, progress); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateIndexManifestCount bounds descriptor fan-out before graph traversal allocates more state.
func validateIndexManifestCount(count int) error {
	if count > maxImageIndexManifests {
		return fmt.Errorf(
			"image index contains %d manifests, limit is %d",
			count,
			maxImageIndexManifests,
		)
	}

	return nil
}

// readImageIndex reads the root index with the same bound used for embedded image JSON.
func readImageIndex(root string) ([]byte, error) {
	path := filepath.Join(root, "index.json")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxImageJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageJSONBytes {
		return nil, fmt.Errorf("image index is larger than %d bytes", maxImageJSONBytes)
	}

	return data, nil
}

// isKnownImageDescriptor accepts image manifest, config,
// and layer media types emitted by OCI and Docker registries.
func isKnownImageDescriptor(mediaType string) bool {
	return isManifestDescriptor(mediaType) ||
		mediaType == ocispec.MediaTypeImageConfig ||
		strings.HasPrefix(mediaType, "application/vnd.docker.container.image") ||
		isImageLayer(mediaType)
}

// validateDescriptorBlob verifies the regular file, exact size,
// and content digest for one content-addressed OCI blob.
func validateDescriptorBlob(root string, descriptor ocispec.Descriptor) error {
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("invalid digest %q: %w", descriptor.Digest, err)
	}
	if descriptor.Size < 0 {
		return fmt.Errorf("negative blob size %d", descriptor.Size)
	}
	if !descriptor.Digest.Algorithm().Available() {
		return fmt.Errorf("unsupported digest algorithm %q", descriptor.Digest.Algorithm())
	}

	path := blobPath(root, descriptor.Digest)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("blob is not a regular file")
	}
	if info.Size() != descriptor.Size {
		return fmt.Errorf("blob size is %d, want %d", info.Size(), descriptor.Size)
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}

	hash := descriptor.Digest.Algorithm().Hash()
	readBytes, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if readBytes != descriptor.Size {
		return fmt.Errorf("blob read %d bytes, want %d", readBytes, descriptor.Size)
	}
	if actual := godigest.NewDigest(descriptor.Digest.Algorithm(), hash); actual != descriptor.Digest {
		return fmt.Errorf("blob digest is %s, want %s", actual, descriptor.Digest)
	}

	return nil
}

// isManifestDescriptor identifies JSON descriptors whose contents reference more graph nodes.
func isManifestDescriptor(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageManifest ||
		mediaType == ocispec.MediaTypeImageIndex ||
		mediaType == "application/vnd.docker.distribution.manifest.v2+json" ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// blobPath returns the OCI content-addressed path for a descriptor digest.
func blobPath(root string, digest godigest.Digest) string {
	return filepath.Join(root, "blobs", digest.Algorithm().String(), digest.Encoded())
}
