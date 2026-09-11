// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/woozymasta/kube-dump

package export

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

func TestS3ImageStoreReusesCommittedBlobWithVerifiedMarker(t *testing.T) {
	t.Parallel()

	data := []byte("committed image blob")
	descriptor := descriptorForTestData(data)
	var bodyReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			if !strings.HasSuffix(request.URL.Path, blobCommitSuffix) {
				bodyReads.Add(1)
				http.Error(response, "unexpected blob body read", http.StatusInternalServerError)
				return
			}

			marker, err := json.Marshal(blobCommit{
				Version: blobCommitVersion,
				Digest:  descriptor.Digest.Encoded(),
				Size:    descriptor.Size,
				ETag:    `"blob-etag"`,
			})
			if err != nil {
				t.Fatalf("marshal test marker: %v", err)
			}
			response.Header().Set("Content-Length", strconv.Itoa(len(marker)))
			response.Header().Set("ETag", `"marker-etag"`)
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(marker)
			return
		}
		if request.Method != http.MethodHead {
			response.WriteHeader(http.StatusOK)
			return
		}

		response.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
		response.Header().Set("ETag", `"blob-etag"`)
		response.Header().Set("X-Amz-Meta-Kube-Dump-Sha256", descriptor.Digest.Encoded())
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestS3ImageStore(t, server.URL)
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("S3 image store did not reuse a committed blob")
	}
	if bodyReads.Load() != 0 {
		t.Fatalf("committed blob body reads = %d, want 0", bodyReads.Load())
	}
}

func TestS3ImageStoreRejectsWrongSizeWithoutBodyRead(t *testing.T) {
	t.Parallel()

	descriptor := descriptorForTestData([]byte("expected image blob"))
	var bodyReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			bodyReads.Add(1)
		}
		if request.Method == http.MethodHead {
			response.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size+1, 10))
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestS3ImageStore(t, server.URL)
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("S3 image store reused a blob with the wrong size")
	}
	if bodyReads.Load() != 0 {
		t.Fatalf("wrong-size blob body reads = %d, want 0", bodyReads.Load())
	}
}

func TestS3ImageStoreDoesNotReuseUnmarkedBlob(t *testing.T) {
	t.Parallel()

	descriptor := descriptorForTestData([]byte("unmarked image blob"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, blobCommitSuffix) {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Method == http.MethodHead {
			response.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
			response.Header().Set("ETag", `"blob-etag"`)
			response.Header().Set("X-Amz-Meta-Kube-Dump-Sha256", descriptor.Digest.Encoded())
			response.Header().Set("X-Amz-Meta-Kube-Dump-Sha256", descriptor.Digest.Encoded())
			response.WriteHeader(http.StatusOK)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestS3ImageStore(t, server.URL)
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("unmarked blob was reused")
	}
}

func TestS3ImageStoreRejectsBlobWithWrongDigestMetadata(t *testing.T) {
	t.Parallel()

	descriptor := descriptorForTestData([]byte("expected image blob"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			response.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
			response.Header().Set("ETag", `"blob-etag"`)
			response.Header().Set("X-Amz-Meta-Kube-Dump-Sha256", "wrong-digest")
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestS3ImageStore(t, server.URL)
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("blob with wrong digest metadata was reused")
	}
}

func TestS3ImageStoreRejectsStaleBlobMarker(t *testing.T) {
	t.Parallel()

	descriptor := descriptorForTestData([]byte("same-size image blob"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, blobCommitSuffix) {
			marker, err := json.Marshal(blobCommit{
				Version: blobCommitVersion,
				Digest:  descriptor.Digest.Encoded(),
				Size:    descriptor.Size,
				ETag:    `"old-etag"`,
			})
			if err != nil {
				t.Fatalf("marshal test marker: %v", err)
			}
			response.Header().Set("Content-Length", strconv.Itoa(len(marker)))
			response.Header().Set("ETag", `"marker-etag"`)
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(marker)
			return
		}
		if request.Method == http.MethodHead {
			response.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
			response.Header().Set("ETag", `"new-etag"`)
			response.Header().Set("X-Amz-Meta-Kube-Dump-Sha256", descriptor.Digest.Encoded())
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestS3ImageStore(t, server.URL)
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("blob with stale commit marker was reused")
	}
}

func TestS3ImageStoreTagsDoNotShareDescriptorAnnotations(t *testing.T) {
	t.Parallel()

	store, err := NewS3ImageStore(&S3Store{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := descriptorForTestData([]byte("manifest"))
	descriptor.Annotations = map[string]string{"org.opencontainers.image.title": "example"}

	if err := store.Tag(context.Background(), descriptor, "registry.example/app:v1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), descriptor, "registry.example/app:stable"); err != nil {
		t.Fatal(err)
	}

	index := store.Index()
	if len(index.Manifests) != 2 {
		t.Fatalf("manifest count = %d, want 2", len(index.Manifests))
	}
	if got := index.Manifests[0].Annotations[ocispec.AnnotationRefName]; got != "registry.example/app:v1" {
		t.Fatalf("first reference = %q", got)
	}
	if got := index.Manifests[1].Annotations[ocispec.AnnotationRefName]; got != "registry.example/app:stable" {
		t.Fatalf("second reference = %q", got)
	}
	if _, found := descriptor.Annotations[ocispec.AnnotationRefName]; found {
		t.Fatal("Tag() mutated the caller's descriptor annotations")
	}

	index.Manifests[0].Annotations[ocispec.AnnotationRefName] = "mutated"
	if got := store.Index().Manifests[0].Annotations[ocispec.AnnotationRefName]; got != "registry.example/app:v1" {
		t.Fatalf("Index() exposed mutable descriptor annotations: %q", got)
	}
}

func TestNewS3ImageStoreForCapturePinsIndexETag(t *testing.T) {
	t.Parallel()

	var indexIfMatch atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/index.json") {
			response.Header().Set("Content-Length", "2")
			response.Header().Set("ETag", `"index-etag"`)
			response.WriteHeader(http.StatusOK)
			return
		}
		if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/index.json") {
			indexIfMatch.Store(request.Header.Get("If-Match"))
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s3Store := newTestS3Store(t, server.URL)
	imageStore, err := NewS3ImageStoreForCapture(context.Background(), s3Store, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageStore.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := indexIfMatch.Load().(string); got != `"index-etag"` {
		t.Fatalf("index If-Match = %q, want %q", got, `"index-etag"`)
	}
}

func TestNewS3ImageStoreForCapturePreservesExistingIndex(t *testing.T) {
	t.Parallel()

	indexData, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		Manifests: []ocispec.Descriptor{{
			Digest: digest.FromString("existing"),
			Annotations: map[string]string{
				ocispec.AnnotationRefName: "registry.example/existing:v1",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/index.json") {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		response.Header().Set("ETag", `"index-etag"`)
		response.Header().Set("Content-Length", strconv.Itoa(len(indexData)))
		if request.Method == http.MethodHead {
			response.WriteHeader(http.StatusOK)
			return
		}
		_, _ = response.Write(indexData)
	}))
	defer server.Close()

	imageStore, err := NewS3ImageStoreForCapture(
		context.Background(),
		newTestS3Store(t, server.URL),
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	index := imageStore.Index()
	if len(index.Manifests) != 1 ||
		index.Manifests[0].Annotations[ocispec.AnnotationRefName] != "registry.example/existing:v1" {
		t.Fatalf("preserved image index = %#v", index)
	}
}

func TestS3ImageStoreCommitRejectsStaleIndex(t *testing.T) {
	t.Parallel()

	var indexCommitted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/index.json") {
			response.Header().Set("Content-Length", "2")
			response.Header().Set("ETag", `"index-etag"`)
			response.WriteHeader(http.StatusOK)
			return
		}
		if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/index.json") {
			if indexCommitted.Swap(true) {
				response.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	first, err := NewS3ImageStoreForCapture(context.Background(), newTestS3Store(t, server.URL), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewS3ImageStoreForCapture(context.Background(), newTestS3Store(t, server.URL), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(context.Background()); err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	if err := second.Commit(context.Background()); err == nil {
		t.Fatal("second Commit() succeeded with a stale index identity")
	}
}

func descriptorForTestData(data []byte) ocispec.Descriptor {
	return content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, data)
}

func newTestS3ImageStore(t *testing.T, endpoint string) *S3ImageStore {
	t.Helper()

	store := newTestS3Store(t, endpoint)
	imageStore, err := NewS3ImageStore(store)
	if err != nil {
		t.Fatal(err)
	}

	return imageStore
}

func newTestS3Store(t *testing.T, endpoint string) *S3Store {
	t.Helper()

	store, err := NewS3Store(context.Background(), S3Options{
		URI:       "s3://bucket/layout",
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: "access",
		SecretKey: "secret",
		Insecure:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	return store
}
