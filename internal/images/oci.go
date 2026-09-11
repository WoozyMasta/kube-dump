// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package images

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	"github.com/woozymasta/orascope"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	remoteauth "oras.land/oras-go/v2/registry/remote/auth"
)

// PullOptions controls registry access for one local OCI layout capture.
type PullOptions struct {
	// Auth is the optional path-scoped registry credential adapter.
	Auth *orascope.Adapter
	// RegistryMirrors replaces an exact source registry host for remote pulls.
	// The original source reference remains the tag in the local OCI layout.
	RegistryMirrors map[string]string
	// Progress observes discovered and completed OCI graph nodes.
	Progress func(PullProgress)
	// Root is the directory containing the OCI Image Layout.
	Root string
	// Target is an optional OCI content store used instead of a local layout.
	// It allows callers such as the S3 backend to reuse content
	// by digest without staging every blob locally first.
	Target oras.Target
	// Reference is the original source reference to preserve in index.json.
	// When empty, the reference used for pulling is preserved.
	Reference string
	// Aliases are additional source references for the same pulled descriptor.
	// They are tagged after the content graph has been copied once.
	Aliases []string
	// RetryAttempts is the maximum number of attempts for replay-safe registry requests.
	RetryAttempts int
}

// PullProgress reports OCI graph transfer progress for one image pull.
// Total grows while ORAS discovers manifests, configs, and layers.
type PullProgress struct {
	// Total is the number of discovered graph nodes.
	Total int
	// Completed is the number of nodes already copied or found in the layout.
	Completed int
	// BytesTotal is the logical size of all discovered descriptors.
	BytesTotal int64
	// BytesCompleted is the logical size of completed descriptors.
	BytesCompleted int64
	// BytesTransferred is the size of descriptors copied into the layout.
	BytesTransferred int64
	// BytesReused is the size of descriptors already present in the layout.
	BytesReused int64
	// Layers is the number of image layer descriptors in the graph.
	Layers int
	// ReusedLayers is the number of image layers already present in the layout.
	ReusedLayers int
	// LayerBytesTotal is the logical size of image layers in the graph.
	LayerBytesTotal int64
	// LayerBytesTransferred is the size of newly copied image layers.
	LayerBytesTransferred int64
	// LayerBytesReused is the size of image layers reused from the layout.
	LayerBytesReused int64
}

// PublishOptions controls optional observation of image publication.
// Progress receives the number of publishable references
// and the number that have completed after each successful registry copy.
type PublishOptions struct {
	// Auth configures registry authentication for the target repositories.
	Auth *orascope.Adapter
	// Progress observes image-level publication without changing copy behavior.
	Progress func(total, completed int)
	// References limits publication to these saved source references.
	// An empty list publishes every preserved reference.
	References []string
	// RetryAttempts is the maximum number of attempts for replay-safe registry requests.
	RetryAttempts int
}

// publishTarget describes one saved reference and its destination repository.
// It is resolved before registry access so target collisions fail atomically.
type publishTarget struct {
	sourceRef        string
	normalized       string
	source           registry.Reference
	targetRepository string
	targetReference  string
}

// manifestResolver resolves a registry tag or digest to its manifest descriptor.
// It keeps digest pinning independently testable from the concrete remote repository.
type manifestResolver interface {
	Resolve(context.Context, string) (ocispec.Descriptor, error)
}

// pullProgress synchronizes ORAS callbacks,
// which may run concurrently for independent layers of one image.
type pullProgress struct {
	callback              func(PullProgress)
	total                 int
	completed             int
	bytesTotal            int64
	bytesCompleted        int64
	bytesTransferred      int64
	bytesReused           int64
	layers                int
	reusedLayers          int
	layerBytesTotal       int64
	layerBytesTransferred int64
	layerBytesReused      int64
	mutex                 sync.Mutex
}

// validatingStore refuses to reuse a local blob
// until its descriptor metadata and content digest have been verified.
// Corrupt CAS files are removed so ORAS can fetch a clean copy before any reference is tagged.
type validatingStore struct {
	*oci.Store
	validated map[string]struct{}
	root      string
	mutex     sync.Mutex
}

// Exists validates an existing blob and reports a corrupt blob
// as absent after removing only its content-addressed path.
func (s *validatingStore) Exists(ctx context.Context, descriptor ocispec.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return false, fmt.Errorf("validate local blob descriptor: %w", err)
	}

	if err := validateDescriptorBlob(s.root, descriptor); err == nil {
		s.mutex.Lock()
		s.validated[descriptor.Digest.String()] = struct{}{}
		s.mutex.Unlock()
		return true, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	blob := blobPath(s.root, descriptor.Digest)
	if removeErr := os.Remove(blob); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return false, fmt.Errorf("remove corrupt local blob %s: %w", descriptor.Digest, removeErr)
	}

	return false, nil
}

// newPullProgress creates the synchronized callback state used by ORAS copy hooks.
// A nil callback is retained as a no-op to keep the copy path identical with and without UI.
func newPullProgress(callback func(PullProgress)) *pullProgress {
	return &pullProgress{callback: callback}
}

// discoveredDescriptor records one graph descriptor and its logical size.
func (p *pullProgress) discoveredDescriptor(descriptor ocispec.Descriptor) {
	if p == nil || p.callback == nil {
		return
	}

	p.mutex.Lock()
	p.total++
	p.bytesTotal += max(descriptor.Size, 0)
	if isImageLayer(descriptor.MediaType) {
		p.layers++
		p.layerBytesTotal += max(descriptor.Size, 0)
	}
	progress := p.snapshotLocked()
	p.mutex.Unlock()
	p.callback(progress)
}

// completedDescriptor records a copied or skipped descriptor.
func (p *pullProgress) completedDescriptor(descriptor ocispec.Descriptor, reused bool) {
	if p == nil || p.callback == nil {
		return
	}

	p.mutex.Lock()
	p.completed++
	size := max(descriptor.Size, 0)
	p.bytesCompleted += size
	if reused {
		p.bytesReused += size
		if isImageLayer(descriptor.MediaType) {
			p.reusedLayers++
			p.layerBytesReused += size
		}
	} else {
		p.bytesTransferred += size
		if isImageLayer(descriptor.MediaType) {
			p.layerBytesTransferred += size
		}
	}
	progress := p.snapshotLocked()
	p.mutex.Unlock()
	p.callback(progress)
}

// snapshotLocked returns the current progress state while p.mutex is held.
func (p *pullProgress) snapshotLocked() PullProgress {
	return PullProgress{
		Total:                 p.total,
		Completed:             p.completed,
		BytesTotal:            p.bytesTotal,
		BytesCompleted:        p.bytesCompleted,
		BytesTransferred:      p.bytesTransferred,
		BytesReused:           p.bytesReused,
		Layers:                p.layers,
		ReusedLayers:          p.reusedLayers,
		LayerBytesTotal:       p.layerBytesTotal,
		LayerBytesTransferred: p.layerBytesTransferred,
		LayerBytesReused:      p.layerBytesReused,
	}
}

// copyOptions attaches synchronized progress callbacks to an ORAS copy.
func (p *pullProgress) copyOptions() oras.CopyOptions {
	options := oras.DefaultCopyOptions
	if p == nil || p.callback == nil {
		return options
	}

	options.PreCopy = func(_ context.Context, descriptor ocispec.Descriptor) error {
		p.discoveredDescriptor(descriptor)
		return nil
	}

	options.OnCopySkipped = func(_ context.Context, descriptor ocispec.Descriptor) error {
		p.discoveredDescriptor(descriptor)
		p.completedDescriptor(descriptor, true)
		return nil
	}

	options.PostCopy = func(_ context.Context, descriptor ocispec.Descriptor) error {
		p.completedDescriptor(descriptor, false)
		return nil
	}

	return options
}

// isImageLayer reports whether a descriptor contains an OCI image layer.
func isImageLayer(mediaType string) bool {
	return strings.HasPrefix(mediaType, ocispec.MediaTypeImageLayer) ||
		strings.HasPrefix(mediaType, "application/vnd.docker.image.rootfs.diff.")
}

// Pull copies one image reference and its complete OCI content graph into a local Image Layout.
// The destination reference is the normalized source reference,
// so it is retained in index.json as ref.name.
func Pull(ctx context.Context, source string, options PullOptions) (ocispec.Descriptor, error) {
	return pull(ctx, source, options)
}

// Tag adds a preserved reference alias to an existing local OCI descriptor.
// It only updates the layout index and never copies the descriptor graph again.
func Tag(ctx context.Context, root, reference string, descriptor ocispec.Descriptor) error {
	if ctx == nil {
		return errors.New("image tag context is required")
	}
	if root == "" {
		return errors.New("image layout root is required")
	}

	normalized, _, err := NormalizeReference(reference)
	if err != nil {
		return fmt.Errorf("parse image tag reference: %w", err)
	}

	store, err := oci.New(root)
	if err != nil {
		return fmt.Errorf("open OCI image layout: %w", err)
	}
	if err := store.Tag(ctx, descriptor, normalized); err != nil {
		return fmt.Errorf("tag image %q: %w", reference, err)
	}

	return nil
}

// PullPlatforms copies one manifest for every requested platform
// and publishes a new OCI image index under the normalized source reference.
// The index keeps the source reference stable while each platform-specific manifest
// remains a normal content-addressed OCI object.
// Missing platforms are returned as warnings;
// only a complete failure is returned as the final error.
func PullPlatforms(
	ctx context.Context,
	source string,
	platforms []Platform,
	options PullOptions,
) (ocispec.Descriptor, []error, error) {
	if len(platforms) == 0 {
		descriptor, err := Pull(ctx, source, options)
		return descriptor, nil, err
	}

	return pullPlatforms(ctx, source, options, platforms)
}

// PublishWithOptions copies every preserved source reference
// from a local OCI Image Layout to a registry
// and optionally reports completed image copies.
func PublishWithOptions(
	ctx context.Context,
	root,
	targetPrefix string,
	options PublishOptions,
) error {
	if ctx == nil {
		return errors.New("image publish context is required")
	}

	store, err := oci.New(root)
	if err != nil {
		return fmt.Errorf("open OCI image layout: %w", err)
	}
	if err := Validate(root); err != nil {
		return fmt.Errorf("validate image layout before publish: %w", err)
	}

	data, err := readImageIndex(root)
	if err != nil {
		return fmt.Errorf("read image index: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("parse image index: %w", err)
	}

	return PublishTargetWithOptions(ctx, store, index, targetPrefix, options)
}

// PublishTargetWithOptions copies preserved references from an OCI target to a registry.
// The supplied index is the source of truth for the reachable graph,
// so targets backed by object storage do not need a local staging directory.
func PublishTargetWithOptions(
	ctx context.Context,
	source oras.Target,
	index ocispec.Index,
	targetPrefix string,
	options PublishOptions,
) error {
	if ctx == nil {
		return errors.New("image publish context is required")
	}
	if source == nil {
		return errors.New("image publish source is required")
	}

	target, err := registry.ParseReference(targetPrefix)
	if err != nil {
		return fmt.Errorf("parse image target prefix: %w", err)
	}
	if target.Reference != "" {
		return errors.New("image target must be a registry repository prefix without a tag")
	}

	selected, err := selectPublishableReferences(index.Manifests, options.References)
	if err != nil {
		return err
	}
	publishTargets, err := resolvePublishTargets(index.Manifests, selected, target)
	if err != nil {
		return err
	}

	publishable := len(publishTargets)
	if options.Progress != nil {
		options.Progress(publishable, 0)
	}

	// The layout can contain platform-specific helper manifests in addition to user-facing references.
	// Publish only preserved references,
	// while the index remains the source of truth for their original registry and tag.
	published := 0
	for _, publishTarget := range publishTargets {
		repository, err := newRemoteRepository(registry.Reference{
			Registry:   target.Registry,
			Repository: publishTarget.targetRepository,
		}, options.RetryAttempts)
		if err != nil {
			return fmt.Errorf("create target image repository: %w", err)
		}

		if options.Auth != nil {
			if err := options.Auth.WrapRepository(repository); err != nil {
				return fmt.Errorf("configure target registry authentication: %w", err)
			}
		}

		if _, err := oras.Copy(
			ctx,
			source,
			publishTarget.normalized,
			repository,
			publishTarget.targetReference,
			oras.DefaultCopyOptions,
		); err != nil {
			return fmt.Errorf("publish image %q: %w", publishTarget.sourceRef, err)
		}

		published++
		if options.Progress != nil {
			options.Progress(publishable, published)
		}
	}

	if published == 0 {
		return errors.New("image layout contains no publishable references")
	}

	return nil
}

// resolvePublishTargets resolves selected saved references into target paths
// and rejects collisions before the first remote repository is contacted.
func resolvePublishTargets(
	manifests []ocispec.Descriptor,
	selected map[string]bool,
	target registry.Reference,
) ([]publishTarget, error) {
	targets := make([]publishTarget, 0, len(selected))
	byRepository := make(map[string]publishTarget, len(selected))
	seenReferences := make(map[string]struct{}, len(selected))

	for _, descriptor := range manifests {
		sourceRef, ok := descriptor.Annotations[ocispec.AnnotationRefName]
		if !ok ||
			strings.HasPrefix(sourceRef, "sha256:") ||
			!selected[sourceRef] {
			continue
		}

		normalized, source, err := NormalizeReference(sourceRef)
		if err != nil {
			return nil, fmt.Errorf("parse saved image reference %q: %w", sourceRef, err)
		}

		targetRepository := target.Repository + "/" + repositoryPathComponent(source.Registry) + "/" + source.Repository
		targetReference := strings.Replace(source.ReferenceOrDefault(), "@", "-", 1)
		candidate := publishTarget{
			sourceRef:        sourceRef,
			normalized:       normalized,
			source:           source,
			targetRepository: targetRepository,
			targetReference:  targetReference,
		}

		if previous, exists := byRepository[targetRepository]; exists &&
			(previous.source.Registry != candidate.source.Registry ||
				previous.source.Repository != candidate.source.Repository) {
			return nil, fmt.Errorf(
				"image publish target repository collision: %q and %q both map to %q",
				previous.sourceRef, sourceRef, targetRepository)
		}
		if _, exists := seenReferences[normalized]; exists {
			continue
		}

		seenReferences[normalized] = struct{}{}
		if _, exists := byRepository[targetRepository]; !exists {
			byRepository[targetRepository] = candidate
		}
		targets = append(targets, candidate)
	}

	return targets, nil
}

// selectPublishableReferences resolves requested references against the saved index.
// Resolution happens before any registry write so a typo cannot produce a partial push.
func selectPublishableReferences(manifests []ocispec.Descriptor, requested []string) (map[string]bool, error) {
	saved := make(map[string][]string)
	for _, descriptor := range manifests {
		reference, ok := descriptor.Annotations[ocispec.AnnotationRefName]
		if !ok || strings.HasPrefix(reference, "sha256:") {
			continue
		}

		normalized, _, err := NormalizeReference(reference)
		if err != nil {
			return nil, fmt.Errorf("parse saved image reference %q: %w", reference, err)
		}

		saved[normalized] = append(saved[normalized], reference)
	}

	if len(requested) == 0 {
		selected := make(map[string]bool)
		for _, references := range saved {
			for _, reference := range references {
				selected[reference] = true
			}
		}

		return selected, nil
	}

	selected := make(map[string]bool, len(requested))
	missing := make([]string, 0)
	for _, reference := range requested {
		normalized, _, err := NormalizeReference(reference)
		if err != nil {
			return nil, fmt.Errorf("parse requested image reference %q: %w", reference, err)
		}

		savedReferences, ok := saved[normalized]
		if !ok {
			missing = append(missing, reference)
			continue
		}

		for _, savedReference := range savedReferences {
			selected[savedReference] = true
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"requested image reference is not present in the saved layout: %s",
			strings.Join(missing, ", "))
	}

	return selected, nil
}

// pull copies one image reference and its complete manifest graph into the local OCI layout,
// skipping blobs that are already present by digest.
func pull(ctx context.Context, source string, options PullOptions) (ocispec.Descriptor, error) {
	if ctx == nil {
		return ocispec.Descriptor{}, errors.New("image pull context is required")
	}

	if options.Root == "" && options.Target == nil {
		return ocispec.Descriptor{}, errors.New("image layout root is required")
	}

	pullReference, parsed, err := NormalizeReference(source)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	destination := pullReference
	if options.Reference != "" {
		destination, _, err = NormalizeReference(options.Reference)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("parse preserved image reference: %w", err)
		}
	}

	remoteReference, err := applyRegistryMirror(parsed, options.RegistryMirrors)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("resolve image registry mirror: %w", err)
	}

	repository, err := newRemoteRepository(remoteReference, options.RetryAttempts)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create image repository: %w", err)
	}
	if options.Auth != nil {
		if err := options.Auth.WrapRepository(repository); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("configure image registry authentication: %w", err)
		}
	}

	// Resolve the mutable source tag once before copying the graph.
	// The copy then follows the immutable root digest even if the tag moves during transfer.
	pinnedReference, err := resolvePinnedReference(ctx, repository, remoteReference.ReferenceOrDefault())
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("resolve image %q: %w", source, err)
	}

	var destinationStore oras.Target
	var localStore *oci.Store
	var validated *validatingStore
	if options.Target != nil {
		destinationStore = options.Target
	} else {
		localStore, err = oci.New(options.Root)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("open OCI image layout: %w", err)
		}

		validated = &validatingStore{
			Store:     localStore,
			root:      options.Root,
			validated: make(map[string]struct{}),
		}
		destinationStore = validated
	}

	// Copy transfers the complete manifest DAG from the pinned root.
	// The OCI store skips blobs that already exist at the same content digest.
	pullProgress := newPullProgress(options.Progress)
	copyOptions := pullProgress.copyOptions()
	descriptor, err := oras.Copy(ctx, repository, pinnedReference, destinationStore, destination, copyOptions)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pull image %q: %w", source, err)
	}
	// ORAS may treat a digest-qualified destination as a content identity
	// rather than a visible OCI layout reference.
	// Tag it explicitly so runtime image identities remain discoverable alongside mutable source tags.
	tagger, ok := destinationStore.(content.Tagger)
	if !ok {
		return ocispec.Descriptor{}, errors.New("image target does not support tagging")
	}
	if err := tagger.Tag(ctx, descriptor, destination); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("tag pulled image %q: %w", destination, err)
	}
	if err := tagAliases(ctx, tagger, descriptor, options.Aliases); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("tag image aliases for %q: %w", source, err)
	}

	if options.Progress != nil && localStore != nil {
		graphProgress, err := inspectGraph(options.Root, descriptor, validated.validated)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("inspect saved image %q: %w", source, err)
		}
		options.Progress(graphProgress)
	}

	return descriptor, nil
}

// pullPlatforms copies an image for each requested platform
// and reports platform-specific failures without discarding successful copies.
func pullPlatforms(
	ctx context.Context,
	source string,
	options PullOptions,
	platforms []Platform,
) (ocispec.Descriptor, []error, error) {
	if ctx == nil {
		return ocispec.Descriptor{}, nil, errors.New("image pull context is required")
	}
	if options.Root == "" && options.Target == nil {
		return ocispec.Descriptor{}, nil, errors.New("image layout root is required")
	}

	normalized, parsed, err := NormalizeReference(source)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	remoteReference, err := applyRegistryMirror(parsed, options.RegistryMirrors)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("resolve image registry mirror: %w", err)
	}

	repository, err := newRemoteRepository(remoteReference, options.RetryAttempts)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("create image repository: %w", err)
	}
	if options.Auth != nil {
		if err := options.Auth.WrapRepository(repository); err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("configure image registry authentication: %w", err)
		}
	}

	// Resolve the mutable source tag once.
	// Every platform copy below must read the same manifest index
	// even if the tag is moved while the backup runs.
	pinnedReference, err := resolvePinnedReference(ctx, repository, remoteReference.ReferenceOrDefault())
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("resolve image %q: %w", source, err)
	}

	var destinationStore oras.Target
	var localStore *oci.Store
	var validated *validatingStore
	if options.Target != nil {
		destinationStore = options.Target
	} else {
		localStore, err = oci.New(options.Root)
		if err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("open OCI image layout: %w", err)
		}

		validated = &validatingStore{
			Store:     localStore,
			root:      options.Root,
			validated: make(map[string]struct{}),
		}
		destinationStore = validated
	}

	untagger, ok := destinationStore.(content.Untagger)
	if !ok {
		return ocispec.Descriptor{}, nil, errors.New("image target does not support temporary tags")
	}

	manifests := make([]ocispec.Descriptor, 0, len(platforms))
	warnings := make([]error, 0)
	pullProgress := newPullProgress(options.Progress)

	// Each platform is copied into a distinct local target.
	// A failed platform is reported as a warning so successful platform copies remain usable.
	for _, platform := range platforms {
		target := fmt.Sprintf("%s--platform-%s-%s", normalized, platform.OS, platform.Architecture)
		if platform.Variant != "" {
			target += "-" + platform.Variant
		}

		copyOptions := pullProgress.copyOptions()
		ociPlatform := platform.OCI()
		copyOptions.WithTargetPlatform(&ociPlatform)
		descriptor, err := oras.Copy(ctx, repository, pinnedReference, destinationStore, target, copyOptions)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("pull image %q for platform %s: %w", source, platform, err))
			continue
		}

		if err := untagger.Untag(ctx, target); err != nil {
			return ocispec.Descriptor{}, warnings, fmt.Errorf("remove temporary platform reference: %w", err)
		}

		descriptor.Platform = &ociPlatform
		manifests = append(manifests, descriptor)
	}

	if len(manifests) == 0 {
		return ocispec.Descriptor{}, warnings, fmt.Errorf("no requested platform was captured for image %q", source)
	}

	indexData, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     manifests,
	})
	if err != nil {
		return ocispec.Descriptor{}, warnings, fmt.Errorf("marshal image index: %w", err)
	}

	indexDescriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, indexData)
	if err := destinationStore.Push(ctx, indexDescriptor, strings.NewReader(string(indexData))); err != nil &&
		!errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, warnings, fmt.Errorf("store image index: %w", err)
	}

	tagger, ok := destinationStore.(content.Tagger)
	if !ok {
		return ocispec.Descriptor{}, warnings, errors.New("image target does not support tagging")
	}
	if err := tagger.Tag(ctx, indexDescriptor, normalized); err != nil {
		return ocispec.Descriptor{}, warnings, fmt.Errorf("tag image index: %w", err)
	}
	if err := tagAliases(ctx, tagger, indexDescriptor, options.Aliases); err != nil {
		return ocispec.Descriptor{}, warnings, fmt.Errorf("tag image aliases for %q: %w", source, err)
	}

	if options.Progress != nil && localStore != nil {
		graphProgress, err := inspectGraph(options.Root, indexDescriptor, validated.validated)
		if err != nil {
			return ocispec.Descriptor{}, warnings, fmt.Errorf("inspect saved image %q: %w", source, err)
		}
		options.Progress(graphProgress)
	}

	return indexDescriptor, warnings, nil
}

// resolvePinnedReference resolves a mutable source reference once
// and returns the validated digest used for every platform-specific copy.
func resolvePinnedReference(
	ctx context.Context,
	resolver manifestResolver,
	reference string,
) (string, error) {
	if resolver == nil {
		return "", errors.New("image manifest resolver is required")
	}

	descriptor, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return "", err
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return "", fmt.Errorf("registry returned invalid manifest digest: %w", err)
	}

	return descriptor.Digest.String(), nil
}

// tagAliases records every unique normalized source reference for one descriptor.
// Aliases share the descriptor and therefore do not trigger another graph transfer.
func tagAliases(
	ctx context.Context,
	tagger content.Tagger,
	descriptor ocispec.Descriptor,
	aliases []string,
) error {
	seen := make(map[string]struct{}, len(aliases))
	normalizedAliases := make([]string, 0, len(aliases))

	for _, alias := range aliases {
		normalized, _, err := NormalizeReference(alias)
		if err != nil {
			return fmt.Errorf("parse image alias %q: %w", alias, err)
		}

		if _, exists := seen[normalized]; exists {
			continue
		}

		seen[normalized] = struct{}{}
		normalizedAliases = append(normalizedAliases, normalized)
	}

	sort.Strings(normalizedAliases)
	for _, normalized := range normalizedAliases {
		if err := tagger.Tag(ctx, descriptor, normalized); err != nil {
			return fmt.Errorf("tag %q: %w", normalized, err)
		}
	}

	return nil
}

// NormalizeReference expands Docker's implicit registry and library rules,
// then validates the result with ORAS's distribution reference parser.
func NormalizeReference(value string) (string, registry.Reference, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", registry.Reference{}, errors.New("image reference is empty")
	}

	value = strings.TrimPrefix(value, "docker-pullable://")
	value = strings.TrimPrefix(value, "docker://")
	value = strings.TrimPrefix(value, "containerd://")
	value = strings.TrimPrefix(value, "cri-o://")

	parts := strings.SplitN(value, "/", 2)
	if len(parts) == 1 || !hasRegistryPrefix(parts[0]) {
		if len(parts) == 1 {
			value = "docker.io/library/" + value
		} else {
			value = "docker.io/" + value
		}
	}

	parsed, err := registry.ParseReference(value)
	if err != nil {
		return "", registry.Reference{}, fmt.Errorf("parse image reference %q: %w", value, err)
	}

	if parsed.Reference == "" {
		parsed.Reference = "latest"
	}

	return parsed.String(), parsed, nil
}

// hasRegistryPrefix distinguishes an explicit registry from Docker's short name.
func hasRegistryPrefix(value string) bool {
	return value == "localhost" || strings.Contains(value, ".") || strings.Contains(value, ":")
}

// ValidateRegistryMirrors validates the explicit source-to-target registry map.
// Both sides are registry hosts; repository prefixes and URLs are not accepted.
func ValidateRegistryMirrors(mirrors map[string]string) error {
	for source, target := range mirrors {
		if err := validateRegistryHost(source); err != nil {
			return fmt.Errorf("invalid source registry %q: %w", source, err)
		}
		if err := validateRegistryHost(target); err != nil {
			return fmt.Errorf("invalid mirror registry %q for %q: %w", target, source, err)
		}
	}

	return nil
}

// applyRegistryMirror replaces an exact registry host
// while preserving the repository and tag or digest from the original reference.
func applyRegistryMirror(reference registry.Reference, mirrors map[string]string) (registry.Reference, error) {
	target, ok := mirrors[reference.Registry]
	if !ok {
		return reference, nil
	}

	if err := validateRegistryHost(target); err != nil {
		return registry.Reference{}, fmt.Errorf("mirror for %q: %w", reference.Registry, err)
	}

	reference.Registry = strings.TrimSpace(target)
	return reference, nil
}

// validateRegistryHost accepts the host form understood by ORAS,
// including an optional port, but rejects repository paths and URL schemes.
func validateRegistryHost(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("registry host is empty")
	}

	parsed, err := registry.ParseReference(value + "/kube-dump-mirror-check")
	if err != nil {
		return fmt.Errorf("parse registry host: %w", err)
	}
	if parsed.Registry != value {
		return errors.New("registry mirror must be a host without a repository path")
	}
	if err := parsed.ValidateRegistry(); err != nil {
		return fmt.Errorf("validate registry host: %w", err)
	}

	return nil
}

// newRemoteRepository creates an ORAS repository and enables plain HTTP only for loopback registries,
// which are commonly used for local development and integration tests.
// Remote registry traffic remains HTTPS by default.
func newRemoteRepository(reference registry.Reference, attempts int) (*remote.Repository, error) {
	repository, err := remote.NewRepository(reference.Registry + "/" + reference.Repository)
	if err != nil {
		return nil, err
	}

	repository.PlainHTTP = isLoopbackRegistry(reference.Registry)
	repository.Client = &remoteauth.Client{
		Client: &http.Client{
			Transport: retry.NewTransport(http.DefaultTransport, retry.Config{
				Attempts: retry.NormalizeAttempts(attempts),
			}),
		},
		Cache: remoteauth.NewCache(),
	}
	return repository, nil
}

// isLoopbackRegistry reports whether a registry name resolves to a loopback host.
func isLoopbackRegistry(value string) bool {
	host := value
	if parsedHost, _, err := net.SplitHostPort(value); err == nil {
		host = parsedHost
	}

	if host == "localhost" ||
		host == "host.docker.internal" ||
		host == "host.containers.internal" {
		return true
	}

	return net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

// repositoryPathComponent maps a registry host to an injective repository path component
// while keeping ordinary DNS hostnames readable.
func repositoryPathComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if isSafeRepositoryHost(value) && !strings.HasPrefix(value, "x-") {
		return value
	}

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(value))
	return "x-" + strings.ToLower(encoded)
}

// isSafeRepositoryHost reports whether a normalized registry host
// can remain readable without replacing characters that carry identity information.
func isSafeRepositoryHost(value string) bool {
	if value == "" {
		return false
	}

	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '-' {
			continue
		}

		return false
	}

	return true
}

// OCI converts the compact platform contract to the OCI platform structure.
func (p Platform) OCI() ocispec.Platform {
	return ocispec.Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
	}
}

// ParsePlatform parses the OCI os/architecture[/variant] notation.
func ParsePlatform(value string) (Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, fmt.Errorf("invalid image platform %q: expected os/architecture[/variant]", value)
	}

	platform := Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		if parts[2] == "" {
			return Platform{}, fmt.Errorf("invalid image platform %q: empty variant", value)
		}

		platform.Variant = parts[2]
	}

	return platform, nil
}

// String returns the conventional os/architecture[/variant] form.
func (p Platform) String() string {
	value := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		value += "/" + p.Variant
	}

	return value
}
