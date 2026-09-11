// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woozymasta/kube-dump/v2/internal/images"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	remoteauth "oras.land/oras-go/v2/registry/remote/auth"
)

func TestGroupImageReferencesPreservesAliases(t *testing.T) {
	t.Parallel()

	digestOne := strings.Repeat("1", 64)
	digestTwo := strings.Repeat("2", 64)
	references := []images.Reference{
		{Pull: "registry.example/app@sha256:" + digestOne, Source: "registry.example/app:stable"},
		{Pull: "registry.example/app@sha256:" + digestOne, Source: "registry.example/app:latest"},
		{Pull: "registry.example/other@sha256:" + digestTwo, Source: "registry.example/other:v1"},
		{Pull: "registry.example/app@sha256:" + digestOne, Source: "registry.example/app:stable"},
	}

	groups, err := groupImageReferences(references)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groupImageReferences() returned %d groups, want 2", len(groups))
	}
	if got, want := groups[0].primary.Source, "registry.example/app:stable"; got != want {
		t.Fatalf("primary source = %q, want %q", got, want)
	}
	if got, want := groups[0].sourceAliases, []string{
		"registry.example/app:latest",
		"registry.example/app:stable",
	}; !equalStrings(got, want) {
		t.Fatalf("source aliases = %#v, want %#v", got, want)
	}
}

func TestParseImagePlatformsDeduplicatesValues(t *testing.T) {
	t.Parallel()

	got, err := parseImagePlatforms([]string{
		"linux/amd64",
		"linux/arm64",
		"linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("parseImagePlatforms() returned %d platforms, want 2", len(got))
	}
	if got[0].String() != "linux/amd64" || got[1].String() != "linux/arm64" {
		t.Fatalf("parseImagePlatforms() = %#v, want linux/amd64 and linux/arm64", got)
	}
}

func TestImageOwnerMatchObjectsDoesNotIncludePod(t *testing.T) {
	t.Parallel()

	owners := imageOwnerMatchObjects([]images.Owner{{
		Group: "apps", Version: "v1", Resource: "deployments",
		Namespace: "prod", Name: "api",
	}})
	if len(owners) != 1 {
		t.Fatalf("imageOwnerMatchObjects() returned %d owners, want 1", len(owners))
	}
	if owners[0].Resource != "deployments" || owners[0].Name != "api" {
		t.Fatalf("imageOwnerMatchObjects() = %#v", owners)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}

func TestGroupImageReferencesPreservesAliasesAndTransferCount(t *testing.T) {
	t.Parallel()

	references := []images.Reference{
		{Pull: "registry.example/app@sha256:" + strings.Repeat("a", 64), Source: "registry.example/app:stable"},
		{Pull: "registry.example/app@sha256:" + strings.Repeat("a", 64), Source: "registry.example/app:latest"},
		{Pull: "registry.example/other@sha256:" + strings.Repeat("b", 64), Source: "registry.example/other:v1"},
	}

	groups, err := groupImageReferences(references)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if got := strings.Join(groups[0].sourceAliases, ","); got != "registry.example/app:latest,registry.example/app:stable" {
		t.Fatalf("source aliases = %q", got)
	}
	if len(groups[0].references) != 2 {
		t.Fatalf("group references = %d, want 2", len(groups[0].references))
	}
}

func TestPlatformSourceReferencesResolveEachAlias(t *testing.T) {
	t.Parallel()

	reference := images.Reference{Source: "registry.example/app:stable"}
	got := platformSourceReferences(reference, []string{
		"registry.example/app:latest",
		"registry.example/app:stable",
		"registry.example/app:latest",
	})
	want := []string{
		"registry.example/app:latest",
		"registry.example/app:stable",
	}
	if !equalStrings(got, want) {
		t.Fatalf("platform source references = %#v, want %#v", got, want)
	}
}

func TestGroupImageReferencesRejectsAliasCollision(t *testing.T) {
	t.Parallel()

	references := []images.Reference{
		{Pull: "registry.example/app@sha256:" + strings.Repeat("a", 64), Source: "registry.example/app:latest"},
		{Pull: "registry.example/app@sha256:" + strings.Repeat("b", 64), Source: "registry.example/app:latest"},
	}

	if _, err := groupImageReferences(references); err == nil {
		t.Fatal("groupImageReferences() accepted an alias collision")
	}
}

func TestImageRegistryAuthCandidatesKeepSecretsSeparate(t *testing.T) {
	t.Parallel()

	secret := func(namespace, name, username string) *unstructured.Unstructured {
		config := []byte(`{"auths":{"registry.example":{"auth":"` +
			base64.StdEncoding.EncodeToString([]byte(username+":password")) +
			`"}}}`)
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"namespace": namespace,
				"name":      name,
			},
			"data": map[string]any{
				".dockerconfigjson": base64.StdEncoding.EncodeToString(config),
			},
		}}
	}

	client := dynamicfake.NewSimpleDynamicClient(
		runtime.NewScheme(),
		secret("team-a", "registry-a", "account-a"),
		secret("team-b", "registry-b", "account-b"),
	)
	candidates, err := imageRegistryAuthCandidates(context.Background(), client, []images.Reference{
		{Namespace: "team-a", PullSecrets: []string{"registry-a"}},
		{Namespace: "team-b", PullSecrets: []string{"registry-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("credential candidates = %d, want two Secrets plus fallback", len(candidates))
	}
	if candidates[0].secret.Namespace != "team-a" || candidates[1].secret.Namespace != "team-b" {
		t.Fatalf("candidate provenance = %#v", candidates)
	}

	contextWithScope := remoteauth.AppendRepositoryScope(
		context.Background(),
		registry.Reference{Registry: "registry.example", Repository: "application/image"},
		remoteauth.ActionPull,
	)
	first, err := candidates[0].adapter.CredentialFunc(nil)(contextWithScope, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := candidates[1].adapter.CredentialFunc(nil)(contextWithScope, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if first.Username != "account-a" || second.Username != "account-b" {
		t.Fatalf("candidate credentials = %#v and %#v", first, second)
	}
}

func TestImageAuthScopesKeepDigestAliasesSeparate(t *testing.T) {
	t.Parallel()

	pull := "registry.example/app@sha256:" + strings.Repeat("a", 64)
	groups, err := groupImageReferences([]images.Reference{
		{
			Namespace:   "team-a",
			Pull:        pull,
			Source:      "registry.example/app:stable",
			PullSecrets: []string{"registry-a"},
		},
		{
			Namespace:   "team-b",
			Pull:        pull,
			Source:      "registry.example/app:latest",
			PullSecrets: []string{"registry-b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	scopes, err := imageAuthScopes(groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 {
		t.Fatalf("auth scopes = %d, want 2", len(scopes))
	}
	if scopes[0].primary.Namespace != "team-a" || scopes[1].primary.Namespace != "team-b" {
		t.Fatalf("scope namespaces = %q and %q", scopes[0].primary.Namespace, scopes[1].primary.Namespace)
	}
	if got := strings.Join(scopes[0].aliases, ","); got != "registry.example/app:stable" {
		t.Fatalf("first scope aliases = %q", got)
	}
	if got := strings.Join(scopes[1].aliases, ","); got != "registry.example/app:latest" {
		t.Fatalf("second scope aliases = %q", got)
	}
}

func TestPullProgressAccumulatorCombinesIndependentStreams(t *testing.T) {
	t.Parallel()

	var observed []images.PullProgress
	progress := newPullProgressAccumulator(func(value images.PullProgress) {
		observed = append(observed, value)
	})
	progress.observe(images.PullProgress{
		Total:            2,
		Completed:        1,
		BytesTotal:       100,
		BytesCompleted:   40,
		BytesTransferred: 30,
		Layers:           1,
		LayerBytesTotal:  80,
	})
	progress.observe(images.PullProgress{
		Total:                 2,
		Completed:             2,
		BytesTotal:            100,
		BytesCompleted:        100,
		BytesTransferred:      90,
		Layers:                1,
		LayerBytesTotal:       80,
		LayerBytesTransferred: 70,
	})
	progress.reset()
	progress.observe(images.PullProgress{
		Total:                 1,
		Completed:             1,
		BytesTotal:            50,
		BytesCompleted:        50,
		BytesTransferred:      50,
		Layers:                1,
		LayerBytesTotal:       50,
		LayerBytesTransferred: 50,
	})

	want := images.PullProgress{
		Total:                 3,
		Completed:             3,
		BytesTotal:            150,
		BytesCompleted:        150,
		BytesTransferred:      140,
		Layers:                2,
		LayerBytesTotal:       130,
		LayerBytesTransferred: 120,
	}
	if got := progress.snapshot(); got != want {
		t.Fatalf("progress snapshot = %#v, want %#v", got, want)
	}
	if len(observed) != 3 {
		t.Fatalf("progress callbacks = %d, want 3", len(observed))
	}
}

func TestPublishImageLayoutReplacesStaleReferences(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	createTestImageLayout(t, source, "registry.example/app:new")
	createTestImageLayout(t, target, "registry.example/app:old")

	if err := publishImageLayout(source, target, false); err != nil {
		t.Fatalf("publishImageLayout() error = %v", err)
	}

	summary, err := images.Inspect(target)
	if err != nil {
		t.Fatalf("inspect published layout: %v", err)
	}
	if !equalStrings(summary.References, []string{"registry.example/app:new"}) {
		t.Fatalf("published references = %#v, want new reference only", summary.References)
	}

	indexData, err := os.ReadFile(filepath.Join(target, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(indexData, []byte("registry.example/app:old")) {
		t.Fatal("published index retained stale reference")
	}
}

func TestPublishImageLayoutPrunesStaleManagedFiles(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	createTestImageLayout(t, source, "registry.example/app:new")
	createTestImageLayout(t, target, "registry.example/app:old")
	userFile := filepath.Join(target, "notes.txt")
	if err := os.WriteFile(userFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBlobs := imageLayoutBlobFiles(t, target)

	if err := publishImageLayout(source, target, true); err != nil {
		t.Fatalf("publishImageLayout() error = %v", err)
	}

	for _, oldBlob := range oldBlobs {
		if _, err := os.Stat(oldBlob); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale blob %q remains: %v", oldBlob, err)
		}
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "keep" {
		t.Fatalf("prune changed user file: data=%q error=%v", data, err)
	}
}

func TestPublishImageLayoutRejectsInvalidSourceWithoutChangingTarget(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "target")
	createTestImageLayout(t, target, "registry.example/app:old")
	before, err := os.ReadFile(filepath.Join(target, "index.json"))
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "invalid")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := publishImageLayout(source, target, false); err == nil {
		t.Fatal("publishImageLayout() accepted an empty source index")
	}
	after, err := os.ReadFile(filepath.Join(target, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("invalid source changed the existing target index")
	}
}

func TestPublishImageLayoutRejectsDestinationSymlink(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	createTestImageLayout(t, source, "registry.example/app:new")
	createTestImageLayout(t, target, "registry.example/app:old")

	external := filepath.Join(t.TempDir(), "external-index.json")
	externalData := []byte("external")
	if err := os.WriteFile(external, externalData, 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(target, "index.json")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, indexPath); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	if err := publishImageLayout(source, target, false); err == nil {
		t.Fatal("publishImageLayout() accepted a destination symlink")
	}
	data, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, externalData) {
		t.Fatal("publication changed the symlink target")
	}
}

func TestPublishImageLayoutRejectsSourceSocket(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	createTestImageLayout(t, source, "registry.example/app:new")
	createTestImageLayout(t, target, "registry.example/app:old")

	socket := filepath.Join(source, "unexpected.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	}()

	oldIndex, err := os.ReadFile(filepath.Join(target, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publishImageLayout(source, target, false); err == nil {
		t.Fatal("publishImageLayout() accepted a source socket")
	}
	newIndex, err := os.ReadFile(filepath.Join(target, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newIndex, oldIndex) {
		t.Fatal("failed publication changed the destination index")
	}
}

func TestFinalizeCapturedImageLayoutKeepsPreviousLayoutAfterCaptureFailure(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	destination := filepath.Join(t.TempDir(), "destination")
	createTestImageLayout(t, source, "registry.example/app:partial")
	createTestImageLayout(t, destination, "registry.example/app:old")
	before, err := os.ReadFile(filepath.Join(destination, "index.json"))
	if err != nil {
		t.Fatal(err)
	}

	captureErr := errors.New("one image failed")
	if err := finalizeCapturedImageLayout(source, destination, captureErr, false); !errors.Is(err, captureErr) {
		t.Fatalf("finalizeCapturedImageLayout() error = %v, want capture error", err)
	}

	summary, err := images.Inspect(destination)
	if err != nil {
		t.Fatalf("inspect previous layout: %v", err)
	}
	if !equalStrings(summary.References, []string{"registry.example/app:old"}) {
		t.Fatalf("published references = %#v, want previous reference", summary.References)
	}
	after, err := os.ReadFile(filepath.Join(destination, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("capture failure changed the previous image layout")
	}
}

func TestFinalizeCapturedImageLayoutDoesNotPublishAfterCancellation(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "source")
	destination := filepath.Join(t.TempDir(), "destination")
	createTestImageLayout(t, source, "registry.example/app:cancelled")

	if err := finalizeCapturedImageLayout(source, destination, context.Canceled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("finalizeCapturedImageLayout() error = %v, want cancellation", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled capture published destination: %v", err)
	}
}

func imageLayoutBlobFiles(t *testing.T, root string) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// createTestImageLayout writes one complete manifest graph for publication tests.
func createTestImageLayout(t *testing.T, root, reference string) {
	t.Helper()

	store, err := oci.New(root)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	configData := []byte(`{"architecture":"amd64","os":"linux","reference":"` + reference + `"}`)
	config := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}

	layerData := []byte("test layer: " + reference)
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, layerData)
	if err := store.Push(ctx, layer, bytes.NewReader(layerData)); err != nil {
		t.Fatal(err)
	}

	manifestData, err := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config:        config,
		Layers:        []ocispec.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}

	manifest := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, manifest, reference); err != nil {
		t.Fatal(err)
	}
}
