// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

const (
	ociLayoutKey        = "oci-layout"
	ociIndexKey         = "index.json"
	blobCommitSuffix    = ".kube-dump-committed"
	blobCommitVersion   = 1
	maxS3ImageManifests = 100_000
)

// blobCommit records the object identity verified before an image blob is reused.
type blobCommit struct {
	Digest    string `json:"digest"`
	ETag      string `json:"etag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Version   int    `json:"version"`
	Size      int64  `json:"size"`
}

// S3ImageStore is an OCI content-addressed store backed by one S3 prefix.
// Blob writes are immediately durable, while the index is kept in memory until
// Commit so an interrupted capture leaves the previous index usable.
type S3ImageStore struct {
	store         *S3Store
	index         ocispec.Index
	indexIdentity *ObjectIdentity
}

// NewS3ImageStore creates an empty OCI index over an existing S3 content store.
// Existing blobs remain available through Exists and are reused by ORAS.
func NewS3ImageStore(store *S3Store) (*S3ImageStore, error) {
	if store == nil {
		return nil, errors.New("S3 image store is required")
	}

	return &S3ImageStore{
		store: store,
		index: ocispec.Index{
			SchemaVersion: 2,
			MediaType:     ocispec.MediaTypeImageIndex,
			Manifests:     make([]ocispec.Descriptor, 0),
		},
	}, nil
}

// LoadS3ImageStore opens the committed OCI index below an S3 prefix.
// The returned store can be used as an ORAS source
// without downloading blobs that are not reachable from the index.
func LoadS3ImageStore(ctx context.Context, store *S3Store) (*S3ImageStore, error) {
	if ctx == nil {
		return nil, errors.New("S3 image store context is required")
	}

	imageStore, err := NewS3ImageStore(store)
	if err != nil {
		return nil, err
	}

	head, err := store.headObject(ctx, ociIndexKey)
	if err != nil {
		return nil, fmt.Errorf("read S3 image index metadata: %w", err)
	}

	identity := ObjectIdentity{
		ETag:      awsStringValue(head.ETag),
		VersionID: awsStringValue(head.VersionId),
		Size:      awsInt64Value(head.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("read S3 image index identity: %w", err)
	}

	index, err := readS3ImageIndex(ctx, store, identity)
	if err != nil {
		return nil, err
	}
	imageStore.index = index
	imageStore.indexIdentity = &identity
	return imageStore, nil
}

// NewS3ImageStoreForCapture opens a pending index
// while pinning the currently committed index for the final conditional publication.
// Existing references are preserved when preserveExisting is true.
func NewS3ImageStoreForCapture(
	ctx context.Context,
	store *S3Store,
	preserveExisting bool,
) (*S3ImageStore, error) {
	if ctx == nil {
		return nil, errors.New("S3 image store context is required")
	}

	imageStore, err := NewS3ImageStore(store)
	if err != nil {
		return nil, err
	}

	head, err := store.headObject(ctx, ociIndexKey)
	if err != nil {
		if isS3NotFound(err) {
			return imageStore, nil
		}
		return nil, fmt.Errorf("read S3 image index metadata: %w", err)
	}

	identity := ObjectIdentity{
		ETag:      awsStringValue(head.ETag),
		VersionID: awsStringValue(head.VersionId),
		Size:      awsInt64Value(head.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("read S3 image index identity: %w", err)
	}

	if preserveExisting {
		index, err := readS3ImageIndex(ctx, store, identity)
		if err != nil {
			return nil, err
		}
		imageStore.index = index
	}
	imageStore.indexIdentity = &identity
	return imageStore, nil
}

func readS3ImageIndex(ctx context.Context, store *S3Store, identity ObjectIdentity) (ocispec.Index, error) {
	body, _, err := store.OpenObjectWithIdentity(ctx, ociIndexKey, &identity)
	if err != nil {
		return ocispec.Index{}, fmt.Errorf("read S3 image index: %w", err)
	}
	defer func() { _ = body.Close() }()

	data, err := readObjectLimited(body, maxBufferedS3ObjectBytes)
	if err != nil {
		return ocispec.Index{}, fmt.Errorf("read S3 image index data: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return ocispec.Index{}, fmt.Errorf("parse S3 image index: %w", err)
	}
	if index.SchemaVersion != 2 {
		return ocispec.Index{}, fmt.Errorf("unsupported S3 image index schema version %d", index.SchemaVersion)
	}
	if len(index.Manifests) > maxS3ImageManifests {
		return ocispec.Index{}, fmt.Errorf("S3 image index contains more than %d manifests", maxS3ImageManifests)
	}
	if index.Manifests == nil {
		index.Manifests = make([]ocispec.Descriptor, 0)
	}

	return index, nil
}

// Index returns a detached copy of the committed or pending OCI index.
func (s *S3ImageStore) Index() ocispec.Index {
	if s == nil {
		return ocispec.Index{}
	}

	index := s.index
	index.Manifests = make([]ocispec.Descriptor, len(s.index.Manifests))
	for manifestIndex, descriptor := range s.index.Manifests {
		index.Manifests[manifestIndex] = cloneImageDescriptor(descriptor)
	}
	return index
}

// Fetch opens a content blob from the S3 OCI layout.
func (s *S3ImageStore) Fetch(ctx context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	if err := validateImageDescriptor(descriptor); err != nil {
		return nil, err
	}

	return s.store.OpenObject(ctx, s.blobObjectKey(descriptor.Digest))
}

// Exists reports whether an OCI blob is already present in S3.
func (s *S3ImageStore) Exists(ctx context.Context, descriptor ocispec.Descriptor) (bool, error) {
	if err := validateImageDescriptor(descriptor); err != nil {
		return false, err
	}

	head, err := s.store.headObject(ctx, s.blobKey(descriptor.Digest))
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect S3 image blob %s: %w", descriptor.Digest, err)
	}
	if awsInt64Value(head.ContentLength) != descriptor.Size {
		return false, nil
	}

	if head.Metadata[contentDigestMetadata] != descriptor.Digest.Encoded() {
		return false, nil
	}

	identity := ObjectIdentity{
		ETag:      awsStringValue(head.ETag),
		VersionID: awsStringValue(head.VersionId),
		Size:      awsInt64Value(head.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		return false, nil
	}

	markerBody, _, err := s.store.openObjectWithIdentity(ctx, s.blobCommitKey(descriptor.Digest), nil)
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read S3 image blob commit marker: %w", err)
	}
	defer func() { _ = markerBody.Close() }()

	data, err := readObjectLimited(markerBody, 16<<10)
	if err != nil {
		return false, nil
	}

	var marker blobCommit
	if err := json.Unmarshal(data, &marker); err != nil {
		return false, nil
	}
	if !validateBlobCommit(marker, descriptor, identity) {
		return false, nil
	}

	return true, nil
}

// Push writes one verified OCI blob to S3 unless its digest key already exists.
func (s *S3ImageStore) Push(
	ctx context.Context,
	descriptor ocispec.Descriptor,
	reader io.Reader,
) error {
	if err := validateImageDescriptor(descriptor); err != nil {
		return err
	}

	exists, err := s.Exists(ctx, descriptor)
	if err != nil {
		return err
	}
	if exists {
		return errdef.ErrAlreadyExists
	}

	verified := content.NewVerifyReader(reader, descriptor)
	metadata := map[string]string{
		contentDigestMetadata: descriptor.Digest.Encoded(),
	}
	if err := s.store.putReaderWithProgress(
		ctx,
		s.blobObjectKey(descriptor.Digest),
		verified,
		descriptor.MediaType,
		metadata,
		nil,
	); err != nil {
		return err
	}
	if err := verified.Verify(); err != nil {
		return fmt.Errorf("verify S3 image blob %s: %w", descriptor.Digest, err)
	}

	head, err := s.store.headObject(ctx, s.blobKey(descriptor.Digest))
	if err != nil {
		return fmt.Errorf("verify stored S3 image blob %s: %w", descriptor.Digest, err)
	}

	identity := ObjectIdentity{
		ETag:      awsStringValue(head.ETag),
		VersionID: awsStringValue(head.VersionId),
		Size:      awsInt64Value(head.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("verify stored S3 image blob %s identity: %w", descriptor.Digest, err)
	}
	if identity.Size != descriptor.Size ||
		head.Metadata[contentDigestMetadata] != descriptor.Digest.Encoded() {
		return fmt.Errorf("verify stored S3 image blob %s metadata: object does not match descriptor", descriptor.Digest)
	}

	marker, err := json.Marshal(blobCommit{
		Version:   blobCommitVersion,
		Digest:    descriptor.Digest.Encoded(),
		Size:      descriptor.Size,
		ETag:      identity.ETag,
		VersionID: identity.VersionID,
	})
	if err != nil {
		return fmt.Errorf("marshal S3 image blob commit marker: %w", err)
	}
	if err := s.store.PutBytes(ctx, s.blobCommitKey(descriptor.Digest), marker, "application/json"); err != nil {
		return fmt.Errorf("commit S3 image blob %s: %w", descriptor.Digest, err)
	}

	return nil
}

// validateBlobCommit checks that a marker describes the exact S3 object read by HEAD.
func validateBlobCommit(marker blobCommit, descriptor ocispec.Descriptor, identity ObjectIdentity) bool {
	return marker.Version == blobCommitVersion &&
		marker.Digest == descriptor.Digest.Encoded() &&
		marker.Size == descriptor.Size &&
		marker.ETag == identity.ETag &&
		marker.VersionID == identity.VersionID
}

// Resolve resolves a reference already tagged during the current capture.
func (s *S3ImageStore) Resolve(_ context.Context, reference string) (ocispec.Descriptor, error) {
	for _, descriptor := range s.index.Manifests {
		if descriptor.Annotations[ocispec.AnnotationRefName] == reference ||
			descriptor.Digest.String() == reference {
			return cloneImageDescriptor(descriptor), nil
		}
	}

	return ocispec.Descriptor{}, fmt.Errorf("resolve image reference %q: %w", reference, errdef.ErrNotFound)
}

// Tag records a reference in the pending OCI index without publishing it yet.
func (s *S3ImageStore) Tag(_ context.Context, descriptor ocispec.Descriptor, reference string) error {
	if err := validateImageDescriptor(descriptor); err != nil {
		return err
	}
	if reference == "" {
		return errors.New("image reference is required")
	}

	descriptor = cloneImageDescriptor(descriptor)
	if descriptor.Annotations == nil {
		descriptor.Annotations = make(map[string]string)
	}

	descriptor.Annotations[ocispec.AnnotationRefName] = reference
	for index, existing := range s.index.Manifests {
		if existing.Annotations[ocispec.AnnotationRefName] == reference {
			s.index.Manifests[index] = descriptor
			return nil
		}
	}
	if len(s.index.Manifests) >= maxS3ImageManifests {
		return fmt.Errorf("S3 image index contains more than %d manifests", maxS3ImageManifests)
	}

	s.index.Manifests = append(s.index.Manifests, descriptor)
	return nil
}

// cloneImageDescriptor detaches mutable descriptor fields from callers and index entries.
func cloneImageDescriptor(descriptor ocispec.Descriptor) ocispec.Descriptor {
	descriptor.URLs = slices.Clone(descriptor.URLs)
	descriptor.Annotations = maps.Clone(descriptor.Annotations)
	descriptor.Data = slices.Clone(descriptor.Data)
	if descriptor.Platform != nil {
		platform := *descriptor.Platform
		platform.OSFeatures = slices.Clone(descriptor.Platform.OSFeatures)
		descriptor.Platform = &platform
	}

	return descriptor
}

// Untag removes a temporary reference from the pending OCI index.
func (s *S3ImageStore) Untag(_ context.Context, reference string) error {
	for index, descriptor := range s.index.Manifests {
		if descriptor.Annotations[ocispec.AnnotationRefName] != reference {
			continue
		}

		s.index.Manifests = append(s.index.Manifests[:index], s.index.Manifests[index+1:]...)
		return nil
	}

	return nil
}

// Commit writes the OCI layout marker and publishes index.json last.
func (s *S3ImageStore) Commit(ctx context.Context) error {
	if s == nil || s.store == nil {
		return errors.New("S3 image store is not initialized")
	}

	layout, err := json.Marshal(struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}{ImageLayoutVersion: "1.0.0"})
	if err != nil {
		return fmt.Errorf("marshal OCI layout marker: %w", err)
	}
	if err := s.store.PutBytes(ctx, ociLayoutKey, layout, "application/json"); err != nil {
		return fmt.Errorf("publish OCI layout marker: %w", err)
	}

	index, err := json.Marshal(s.index)
	if err != nil {
		return fmt.Errorf("marshal OCI image index: %w", err)
	}
	if err := s.store.PutBytesIfMatch(ctx, ociIndexKey, index, ocispec.MediaTypeImageIndex, s.indexIdentity); err != nil {
		return fmt.Errorf("publish OCI image index: %w", err)
	}

	return nil
}

// blobKey returns the relative OCI content path for one digest.
func (s *S3ImageStore) blobKey(value digest.Digest) string {
	return path.Join("blobs", value.Algorithm().String(), value.Encoded())
}

// blobObjectKey returns the complete bucket key for one OCI content blob.
func (s *S3ImageStore) blobObjectKey(value digest.Digest) string {
	return path.Join(s.store.ObjectKey(), s.blobKey(value))
}

// blobCommitKey identifies the durable marker written after blob verification.
func (s *S3ImageStore) blobCommitKey(value digest.Digest) string {
	return s.blobKey(value) + blobCommitSuffix
}

// awsStringValue returns an empty value for an absent AWS SDK string field.
func awsStringValue(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

// awsInt64Value returns zero for an absent AWS SDK integer field.
func awsInt64Value(value *int64) int64 {
	if value == nil {
		return 0
	}

	return *value
}

// validateImageDescriptor checks the digest and size before any S3 operation.
func validateImageDescriptor(descriptor ocispec.Descriptor) error {
	if descriptor.Size < 0 {
		return errors.New("image descriptor size must not be negative")
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("validate image descriptor digest: %w", err)
	}

	return nil
}
