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
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/images"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

// Execute inspects a local OCI Image Layout.
func (c *InspectImagesDirCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	source := imageRoot(c.Path)
	summary, err := images.Inspect(source)
	if err != nil {
		return fmt.Errorf("inspect image layout: %w", err)
	}

	return writeImageSummary(c.output, source, summary)
}

// Execute inspects an OCI Image Layout stored below an S3 URI.
func (c *InspectImagesS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	store, err := newS3Store(c.context(), withS3LayoutPrefix(c.S3Destination, imageLayoutDirectory))
	if err != nil {
		return fmt.Errorf("open S3 image source: %w", err)
	}

	objects, err := store.ListStoredObjects(c.context())
	if err != nil {
		return fmt.Errorf("list S3 image layout: %w", err)
	}

	var layoutData, indexData []byte
	summary := images.Summary{}
	for _, object := range objects {
		relative := strings.TrimPrefix(strings.TrimPrefix(object.Key, store.ObjectKey()), "/")
		switch relative {
		case "oci-layout":
			layoutData, err = readS3Object(c.context(), store, object.Key)

		case "index.json":
			indexData, err = readS3Object(c.context(), store, object.Key)

		default:
			if strings.HasPrefix(relative, "blobs/") && !strings.HasSuffix(relative, "/") {
				summary.Blobs++
				summary.Bytes += object.Size
			}
		}
		if err != nil {
			return fmt.Errorf("read S3 image metadata: %w", err)
		}
	}

	summaryMetadata, err := images.InspectMetadata(layoutData, indexData)
	if err != nil {
		return fmt.Errorf("inspect S3 image layout: %w", err)
	}

	summary.Manifests = summaryMetadata.Manifests
	summary.Platforms = summaryMetadata.Platforms
	summary.References = summaryMetadata.References

	return writeImageSummary(c.output, c.URI, summary)
}

// readS3Object reads only small metadata objects needed by inspection.
func readS3Object(ctx context.Context, store *export.S3Store, key string) ([]byte, error) {
	body, err := store.OpenObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	return readLimited(body, maxRenderedMetadataBytes)
}

// Execute inspects PVC artifacts in a local capture directory.
func (c *InspectPvcDirCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	info, err := os.Stat(c.Path)
	if err != nil {
		return fmt.Errorf("inspect PVC input: %w", err)
	}
	if !info.IsDir() {
		return errors.New("inspect PVC dir source must be a directory")
	}

	report, err := inspectPVCDirectory(c.context(), c.Path)
	if err != nil {
		return err
	}

	return writeCaptureReport(c.output, c.Path, report)
}

// Execute inspects PVC artifacts stored below an S3 URI.
func (c *InspectPvcS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	store, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open PVC S3 source: %w", err)
	}

	objects, err := store.ListStoredObjects(c.context())
	if err != nil {
		return fmt.Errorf("list S3 PVC artifacts: %w", err)
	}

	report := inspectPVCObjects(store.ObjectKey(), objects)
	return writeCaptureReport(c.output, c.URI, report)
}

// downloadImageLayout stages one S3 layout and publishes it only after graph validation.
func downloadImageLayout(
	ctx context.Context,
	store *export.S3Store,
	root string,
	manager *progress.Manager,
) error {
	staging, err := newImageLayoutStaging(root)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := downloadImageLayoutInto(ctx, store, staging, manager); err != nil {
		return err
	}
	if err := images.Validate(staging); err != nil {
		return fmt.Errorf("validate downloaded image layout: %w", err)
	}

	return publishImageLayout(staging, root, true)
}

// downloadImageLayoutInto copies only the OCI graph reachable
// from index.json into a fresh local layout;
// orphaned content-addressed blobs are ignored.
func downloadImageLayoutInto(
	ctx context.Context,
	store *export.S3Store,
	root string,
	manager *progress.Manager,
) error {
	imageStore, err := export.LoadS3ImageStore(ctx, store)
	if err != nil {
		return err
	}

	index := imageStore.Index()
	destination, err := oci.New(root)
	if err != nil {
		return fmt.Errorf("create local OCI image layout: %w", err)
	}

	counter := manager.NewCounter("Images", int64(len(index.Manifests)))
	defer counter.Complete()
	for _, descriptor := range index.Manifests {
		reference := descriptor.Digest.String()
		if tagged := descriptor.Annotations[ocispec.AnnotationRefName]; tagged != "" {
			reference = tagged
		}

		if _, err := oras.Copy(
			ctx,
			imageStore,
			reference,
			destination,
			reference,
			oras.DefaultCopyOptions,
		); err != nil {
			return fmt.Errorf("download image %q from S3: %w", reference, err)
		}

		counter.Increment()
	}

	return nil
}

// writeImageSummary renders stable human-readable OCI layout statistics.
func writeImageSummary(output io.Writer, source string, summary images.Summary) error {
	if _, err := fmt.Fprintf(output,
		"Source:        %s\nManifests:     %d\nPlatforms:     %d\nBlobs:         %d\nBlob bytes:    %d\n",
		source, summary.Manifests, summary.Platforms, summary.Blobs, summary.Bytes); err != nil {
		return err
	}
	if len(summary.References) == 0 {
		return nil
	}

	if _, err := io.WriteString(output, "\nReferences:\n"); err != nil {
		return err
	}
	for _, reference := range summary.References {
		if _, err := fmt.Fprintf(output, "  %s\n", reference); err != nil {
			return err
		}
	}

	return nil
}

// Execute inspects a local capture directory.
func (c *InspectDirCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	info, err := os.Stat(c.Path)
	if err != nil {
		return fmt.Errorf("inspect capture input: %w", err)
	}
	if !info.IsDir() {
		return errors.New("inspect dir source must be a directory")
	}

	report, err := inspectCaptureDirectory(c.context(), c.Path)
	if err != nil {
		return err
	}

	return writeCaptureReport(c.output, c.Path, report)
}

// Execute inspects a capture stored in a Git worktree.
func (c *InspectGitCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	report, err := inspectCaptureDirectory(c.context(), c.Path)
	if err != nil {
		return fmt.Errorf("inspect Git capture: %w", err)
	}

	return writeCaptureReport(c.output, c.Path, report)
}

// Execute inspects a capture stored below an S3 URI.
func (c *InspectS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	store, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 source: %w", err)
	}

	algorithm, encrypted, archiveErr := archiveAlgorithm(c.URI)
	if archiveErr == nil {
		return c.inspectS3Archive(store, algorithm, encrypted)
	}

	return c.inspectS3Resources()
}

// inspectS3Archive opens and validates one remote archive object.
func (c *InspectS3Command) inspectS3Archive(
	store *export.S3Store,
	algorithm compress.Algorithm,
	encrypted bool,
) error {
	object, err := store.OpenObject(c.context(), store.ObjectKey())
	if err != nil {
		return fmt.Errorf("open S3 archive: %w", err)
	}
	defer func() { _ = object.Close() }()
	var source io.Reader = object

	if encrypted {
		identities, err := loadConfiguredIdentities(c.IdentityOptions)
		if err != nil {
			return fmt.Errorf("load archive identities: %w", err)
		}

		source, err = agecrypto.OpenDecryptReader(object, identities)
		if err != nil {
			return fmt.Errorf("open encrypted S3 archive: %w", err)
		}
	}

	report, err := archive.Inspect(source, algorithm)
	if err != nil {
		return fmt.Errorf("inspect S3 archive: %w", err)
	}

	return writeArchiveReport(c.output, c.URI, encrypted, report)
}

// inspectS3Resources summarizes a remote resource layout from object keys.
// It does not download or decode resource bodies.
func (c *InspectS3Command) inspectS3Resources() error {
	_, store, err := openCurrentS3ResourceStores(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 resource source: %w", err)
	}

	objects, err := store.ListStoredObjects(c.context())
	if err != nil {
		return fmt.Errorf("list S3 resources: %w", err)
	}
	report := inspectResourceObjects(store.ObjectKey(), objects)

	return writeCaptureReport(c.output, c.URI, report)
}

// inspectResourceObjects summarizes resource keys and sizes without reading any YAML body from S3.
func inspectResourceObjects(prefix string, objects []export.StoredObject) archive.Report {
	report := archive.Report{Resources: make(map[string]int)}
	for _, object := range objects {
		relative := strings.TrimPrefix(strings.TrimPrefix(object.Key, prefix), "/")
		canonicalPath, ok := canonicalResourcePath(relative)
		if !strings.HasSuffix(canonicalPath, ".yaml") || !ok {
			continue
		}

		parts := strings.Split(filepath.ToSlash(canonicalPath), "/")
		report.Objects++
		report.Resources[strings.Join(parts[:3], "/")]++
	}

	return report
}

// inspectPVCObjects summarizes PVC payload objects using S3 listing metadata.
func inspectPVCObjects(prefix string, objects []export.StoredObject) archive.Report {
	var report archive.Report
	for _, object := range objects {
		relative := strings.TrimPrefix(strings.TrimPrefix(object.Key, prefix), "/")
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 5 || parts[0] != "volumes" || parts[1] == "" ||
			parts[2] == "" || !pvc.IsTimestamp(parts[3]) {
			continue
		}

		if !isPVCDataName(parts[4]) {
			continue
		}

		report.PVCArtifacts++
		report.PVCBytes += object.Size
	}

	return report
}

// inspectCaptureDirectory summarizes canonical files without decoding objects.
func inspectCaptureDirectory(ctx context.Context, root string) (archive.Report, error) {
	if ctx == nil {
		return archive.Report{}, errors.New("inspect context is required")
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return archive.Report{}, fmt.Errorf("resolve capture directory: %w", err)
	}
	if err := recoverResourcePublication(resourceRoot(root)); err != nil {
		return archive.Report{}, fmt.Errorf("recover resource publication: %w", err)
	}

	report := archive.Report{Resources: make(map[string]int)}

	// One walk handles both resource and PVC layouts. Hidden transport metadata is skipped
	// while resource counts are derived from canonical paths only.
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root {
				return filepath.SkipDir
			}

			return nil
		}

		if isPVCDataFile(root, path) {
			info, err := entry.Info()
			if err != nil {
				return err
			}

			report.PVCArtifacts++
			report.PVCBytes += info.Size()
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(relative), "volumes/") {
			return nil
		}

		canonicalPath, ok := canonicalResourcePath(relative)
		if !ok {
			return nil
		}

		parts := strings.Split(filepath.ToSlash(canonicalPath), "/")
		resource := strings.Join(parts[:3], "/")
		report.Objects++
		report.Resources[resource]++

		return nil
	})
	if err != nil {
		return archive.Report{}, fmt.Errorf("scan capture directory: %w", err)
	}

	return report, nil
}

// inspectPVCDirectory summarizes only PVC payload files below a capture root.
func inspectPVCDirectory(ctx context.Context, root string) (archive.Report, error) {
	if ctx == nil {
		return archive.Report{}, errors.New("inspect PVC context is required")
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return archive.Report{}, fmt.Errorf("resolve PVC directory: %w", err)
	}

	var report archive.Report
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() {
			return nil
		}
		if !isPVCDataFile(root, path) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		report.PVCArtifacts++
		report.PVCBytes += info.Size()
		return nil
	})
	if err != nil {
		return archive.Report{}, fmt.Errorf("scan PVC directory: %w", err)
	}

	return report, nil
}

// isPVCDataFile identifies one stored PVC payload by its canonical volume path.
func isPVCDataFile(root, file string) bool {
	relative, err := filepath.Rel(root, file)
	if err != nil {
		return false
	}

	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 5 || parts[0] != "volumes" || parts[1] == "" ||
		parts[2] == "" || !pvc.IsTimestamp(parts[3]) {
		return false
	}

	return isPVCDataName(parts[4])
}

// isPVCDataName identifies a canonical PVC payload filename.
func isPVCDataName(name string) bool {
	switch name {
	case "data.tar.gz", "data.tar.gz.age", "data.tar.zst", "data.tar.zst.age", "snapshot.json":
		return true
	default:
		return false
	}
}

// writeCaptureReport renders a directory or object-store summary.
func writeCaptureReport(output io.Writer, source string, report archive.Report) error {
	return writeReport(output, "Source", source, false, false, report)
}

// Execute inspects one archive without extracting its contents.
func (c *InspectArchiveCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("inspect output is required")
	}

	algorithm, encrypted, err := archiveAlgorithm(c.Positional.Path)
	if err != nil {
		return err
	}

	input, err := os.Open(c.Positional.Path)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = input.Close() }()

	var source io.Reader = input
	if encrypted {
		identities, err := loadConfiguredIdentities(c.IdentityOptions)
		if err != nil {
			return fmt.Errorf("load archive identities: %w", err)
		}

		source, err = agecrypto.OpenDecryptReader(input, identities)
		if err != nil {
			return fmt.Errorf("open encrypted archive: %w", err)
		}
	}

	report, err := archive.Inspect(source, algorithm)
	if err != nil {
		return fmt.Errorf("inspect archive: %w", err)
	}
	if err := writeArchiveReport(c.output, c.Positional.Path, encrypted, report); err != nil {
		return fmt.Errorf("write archive report: %w", err)
	}

	return nil
}

// writeArchiveReport renders the stable human-readable archive summary.
func writeArchiveReport(output io.Writer, path string, encrypted bool, report archive.Report) error {
	return writeReport(output, "Archive", path, encrypted, true, report)
}

// writeReport renders the shared human-readable inspection summary.
func writeReport(
	output io.Writer,
	label, source string,
	encrypted, archiveMetadata bool,
	report archive.Report,
) error {
	if _, err := fmt.Fprintf(output, "%s:        %s\n", label, source); err != nil {
		return err
	}

	if archiveMetadata {
		if _, err := fmt.Fprintf(
			output, "Created:       %s\n",
			report.Metadata.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(
			output,
			"Compression:   %s\n", report.Metadata.Compression); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(
			output,
			"Encrypted:     %t\n", encrypted); err != nil {
			return err
		}
	}

	backupType := "empty"

	// Report the semantic contents instead of exposing backend-specific path details;
	// the same summary is used for directories, S3 prefixes, and archives.
	switch {
	case report.Objects > 0 && report.PVCArtifacts > 0:
		backupType = "Kubernetes resources and PVC data"
	case report.Objects > 0:
		backupType = "Kubernetes resources"
	case report.PVCArtifacts > 0:
		backupType = "PVC data"
	}

	if _, err := fmt.Fprintf(output,
		"Type:          %s\nEntries:       %d\nObjects:       %d\nPVC artifacts: %d\nPVC bytes:     %d\n",
		backupType, report.Entries, report.Objects, report.PVCArtifacts, report.PVCBytes); err != nil {
		return err
	}
	if len(report.Resources) == 0 {
		return nil
	}
	if _, err := io.WriteString(output, "\nResources:\n"); err != nil {
		return err
	}

	resources := make([]string, 0, len(report.Resources))
	for resource := range report.Resources {
		resources = append(resources, resource)
	}

	sort.Strings(resources)
	for _, resource := range resources {
		if _, err := fmt.Fprintf(output, "  %-40s %d\n", resource, report.Resources[resource]); err != nil {
			return err
		}
	}

	return nil
}
