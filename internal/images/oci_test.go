package images

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

type fakeManifestResolver struct {
	reference  string
	descriptor ocispec.Descriptor
}

func (r *fakeManifestResolver) Resolve(_ context.Context, reference string) (ocispec.Descriptor, error) {
	r.reference = reference
	return r.descriptor, nil
}

func TestResolvePinnedReferenceUsesResolvedDigest(t *testing.T) {
	t.Parallel()

	resolver := &fakeManifestResolver{
		descriptor: content.NewDescriptorFromBytes(
			ocispec.MediaTypeImageManifest,
			[]byte("manifest"),
		),
	}
	pinned, err := resolvePinnedReference(context.Background(), resolver, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if pinned != resolver.descriptor.Digest.String() {
		t.Fatalf("pinned reference = %q, want %q", pinned, resolver.descriptor.Digest)
	}
	if resolver.reference != "v1" {
		t.Fatalf("resolver reference = %q, want v1", resolver.reference)
	}
}

func TestResolvePinnedReferenceRejectsMissingDigest(t *testing.T) {
	t.Parallel()

	_, err := resolvePinnedReference(context.Background(), &fakeManifestResolver{}, "latest")
	if err == nil {
		t.Fatal("resolvePinnedReference() accepted a missing digest")
	}
}

func TestPullProgressTracksGraphNodes(t *testing.T) {
	t.Parallel()

	var progress []PullProgress
	observer := newPullProgress(func(value PullProgress) {
		progress = append(progress, value)
	})
	observer.discoveredDescriptor(ocispec.Descriptor{})
	observer.discoveredDescriptor(ocispec.Descriptor{})
	observer.completedDescriptor(ocispec.Descriptor{}, false)
	observer.completedDescriptor(ocispec.Descriptor{}, false)

	if len(progress) != 4 {
		t.Fatalf("progress callbacks = %d, want 4", len(progress))
	}
	if progress[0] != (PullProgress{Total: 1}) || progress[1] != (PullProgress{Total: 2}) {
		t.Fatalf("discovery progress = %#v, want 1/0 then 2/0", progress[:2])
	}
	if progress[2] != (PullProgress{Total: 2, Completed: 1}) ||
		progress[3] != (PullProgress{Total: 2, Completed: 2}) {
		t.Fatalf("completion progress = %#v, want 1/2 then 2/2", progress[2:])
	}
}

func TestValidatingStoreRemovesCorruptBlobInsteadOfReusingIt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	data := []byte("valid blob")
	descriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, data)
	blob := blobPath(root, descriptor.Digest)
	if err := os.MkdirAll(filepath.Dir(blob), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("corrupt!"), 0o640); err != nil {
		t.Fatal(err)
	}

	store := &validatingStore{
		root:      root,
		validated: make(map[string]struct{}),
	}
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatalf("validatingStore.Exists() error = %v", err)
	}
	if exists {
		t.Fatal("validatingStore.Exists() reused a corrupt blob")
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("corrupt blob was not removed, stat error = %v", err)
	}
}

func TestValidatingStoreAcceptsOnlyMatchingBlob(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	data := []byte("valid blob")
	descriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, data)
	blob := blobPath(root, descriptor.Digest)
	if err := os.MkdirAll(filepath.Dir(blob), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, data, 0o640); err != nil {
		t.Fatal(err)
	}

	store := &validatingStore{
		root:      root,
		validated: make(map[string]struct{}),
	}
	exists, err := store.Exists(context.Background(), descriptor)
	if err != nil {
		t.Fatalf("validatingStore.Exists() error = %v", err)
	}
	if !exists {
		t.Fatal("validatingStore.Exists() rejected a valid blob")
	}
	if _, ok := store.validated[descriptor.Digest.String()]; !ok {
		t.Fatal("validatingStore.Exists() did not record the validated blob")
	}
}

func TestPullProgressTracksTransferAndReuseMetrics(t *testing.T) {
	t.Parallel()

	var latest PullProgress
	observer := newPullProgress(func(value PullProgress) { latest = value })
	layer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Size:      100,
	}

	observer.discoveredDescriptor(layer)
	observer.completedDescriptor(layer, false)
	observer.discoveredDescriptor(ocispec.Descriptor{Size: 20})
	observer.completedDescriptor(ocispec.Descriptor{Size: 20}, true)

	if latest.Layers != 1 || latest.ReusedLayers != 0 {
		t.Fatalf("layer counts after reuse setup = %#v", latest)
	}
	if latest.LayerBytesTotal != 100 || latest.LayerBytesTransferred != 100 {
		t.Fatalf("layer transfer metrics = %#v", latest)
	}
	if latest.BytesTransferred != 100 || latest.BytesReused != 20 {
		t.Fatalf("graph transfer metrics = %#v", latest)
	}

	observer.discoveredDescriptor(layer)
	observer.completedDescriptor(layer, true)
	if latest.ReusedLayers != 1 || latest.LayerBytesReused != 100 {
		t.Fatalf("reused layer metrics = %#v", latest)
	}
}

func TestNormalizeReference(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "short image",
			input: "nginx",
			want:  "docker.io/library/nginx:latest",
		},
		{
			name:  "short repository with tag",
			input: "acme/api:v1",
			want:  "docker.io/acme/api:v1",
		},
		{
			name:  "qualified repository",
			input: "ghcr.io/acme/api",
			want:  "ghcr.io/acme/api:latest",
		},
		{
			name:  "qualified image",
			input: "ghcr.io/acme/api@sha256:" + digest('a'),
			want:  "ghcr.io/acme/api@sha256:" + digest('a'),
		},
		{
			name:  "runtime prefix",
			input: "docker-pullable://ghcr.io/acme/api@sha256:" + digest('b'),
			want:  "ghcr.io/acme/api@sha256:" + digest('b'),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := NormalizeReference(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("NormalizeReference() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSelectPublishableReferences(t *testing.T) {
	t.Parallel()
	manifests := []ocispec.Descriptor{
		{Annotations: map[string]string{ocispec.AnnotationRefName: "docker.io/library/nginx:latest"}},
		{Annotations: map[string]string{ocispec.AnnotationRefName: "ghcr.io/acme/api:v1"}},
		{Annotations: map[string]string{ocispec.AnnotationRefName: "ghcr.io/acme/api:v1--platform-linux-amd64"}},
	}

	selected, err := selectPublishableReferences(manifests, []string{"nginx:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if !selected["docker.io/library/nginx:latest"] || len(selected) != 1 {
		t.Fatalf("selected references = %#v", selected)
	}
	if _, err := selectPublishableReferences(manifests, []string{"missing:latest"}); err == nil {
		t.Fatal("missing reference was accepted")
	}
}

func TestSelectPublishableReferencesKeepsPlatformLikeUserTags(t *testing.T) {
	t.Parallel()

	reference := "ghcr.io/acme/api:v1--platform-linux-amd64"
	selected, err := selectPublishableReferences([]ocispec.Descriptor{
		{Annotations: map[string]string{ocispec.AnnotationRefName: reference}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selected[reference] {
		t.Fatalf("selectPublishableReferences() dropped a user tag: %#v", selected)
	}
}

func TestResolvePublishTargetsSeparatesRegistryHostsWithPorts(t *testing.T) {
	t.Parallel()

	manifests := []ocispec.Descriptor{
		{Annotations: map[string]string{
			ocispec.AnnotationRefName: "registry.example:443/acme/api:v1",
		}},
		{Annotations: map[string]string{
			ocispec.AnnotationRefName: "registry.example-443/acme/api:v1",
		}},
	}
	target, err := registry.ParseReference("mirror.example/kube-dump")
	if err != nil {
		t.Fatal(err)
	}

	targets, err := resolvePublishTargets(manifests, map[string]bool{
		"registry.example:443/acme/api:v1": true,
		"registry.example-443/acme/api:v1": true,
	}, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].targetRepository == targets[1].targetRepository {
		t.Fatalf("resolvePublishTargets() target repositories = %#v, want distinct paths", targets)
	}
}

func TestResolvePublishTargetsAllowsTagsFromOneSourceRepository(t *testing.T) {
	t.Parallel()

	manifests := []ocispec.Descriptor{
		{Annotations: map[string]string{
			ocispec.AnnotationRefName: "registry.example/acme/api:v1",
		}},
		{Annotations: map[string]string{
			ocispec.AnnotationRefName: "registry.example/acme/api:v2",
		}},
	}
	target, err := registry.ParseReference("mirror.example/kube-dump")
	if err != nil {
		t.Fatal(err)
	}

	targets, err := resolvePublishTargets(manifests, map[string]bool{
		"registry.example/acme/api:v1": true,
		"registry.example/acme/api:v2": true,
	}, target)
	if err != nil {
		t.Fatalf("resolvePublishTargets() error = %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("resolvePublishTargets() returned %d targets, want 2", len(targets))
	}
}

func TestTagAliasesSharesOneDescriptor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := oci.New(root)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte(`{"schemaVersion":2}`)
	descriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, data)
	if err := store.Push(context.Background(), descriptor, bytes.NewReader(data)); err != nil {
		t.Fatalf("push descriptor: %v", err)
	}
	if err := tagAliases(context.Background(), store, descriptor, []string{
		"acme/api:latest",
		"acme/api:v1",
		"acme/api:latest",
	}); err != nil {
		t.Fatalf("tagAliases() error = %v", err)
	}

	for _, reference := range []string{"docker.io/acme/api:latest", "docker.io/acme/api:v1"} {
		resolved, err := store.Resolve(context.Background(), reference)
		if err != nil {
			t.Fatalf("resolve %q: %v", reference, err)
		}
		if resolved.Digest != descriptor.Digest {
			t.Fatalf("resolved %q digest = %s, want %s", reference, resolved.Digest, descriptor.Digest)
		}
	}

}

func TestOCIStorePreservesDigestQualifiedReference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := oci.New(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"schemaVersion":2}`)
	descriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, data)
	if err := store.Push(context.Background(), descriptor, bytes.NewReader(data)); err != nil {
		t.Fatalf("push descriptor: %v", err)
	}
	reference := "registry.example/app@" + descriptor.Digest.String()
	if err := store.Tag(context.Background(), descriptor, reference); err != nil {
		t.Fatalf("tag digest-qualified reference: %v", err)
	}
	resolved, err := store.Resolve(context.Background(), reference)
	if err != nil {
		t.Fatalf("resolve digest-qualified reference: %v", err)
	}
	if resolved.Digest != descriptor.Digest {
		t.Fatalf("resolved digest = %s, want %s", resolved.Digest, descriptor.Digest)
	}

	data, err = os.ReadFile(filepath.Join(root, ocispec.ImageIndexFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(reference)) {
		t.Fatalf("OCI index does not preserve %q: %s", reference, data)
	}
}

func TestNormalizeReferenceRejectsEmptyValue(t *testing.T) {
	if _, _, err := NormalizeReference(" "); err == nil {
		t.Fatal("NormalizeReference() accepted an empty value")
	}
}

func TestParsePlatform(t *testing.T) {
	platform, err := ParsePlatform("linux/arm64/v8")
	if err != nil {
		t.Fatal(err)
	}
	if platform.String() != "linux/arm64/v8" {
		t.Fatalf("platform = %q", platform)
	}
	if _, err := ParsePlatform("linux"); err == nil {
		t.Fatal("ParsePlatform() accepted a platform without architecture")
	}
}

func TestIsLoopbackRegistry(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "localhost", value: "localhost:5000", want: true},
		{name: "IPv4 loopback", value: "127.0.0.1:5000", want: true},
		{name: "IPv6 loopback", value: "[::1]:5000", want: true},
		{name: "remote host", value: "registry.example.com:5000", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isLoopbackRegistry(test.value); got != test.want {
				t.Fatalf("isLoopbackRegistry(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestNewRemoteRepositoryUsesPlainHTTPForLoopback(t *testing.T) {
	parsed, err := registry.ParseReference("127.0.0.1:5000/example/image:v1")
	if err != nil {
		t.Fatal(err)
	}

	repository, err := newRemoteRepository(parsed, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !repository.PlainHTTP {
		t.Fatal("newRemoteRepository() did not enable plain HTTP for loopback")
	}
}

func TestRepositoryPathComponent(t *testing.T) {
	if got := repositoryPathComponent("registry.example"); got != "registry.example" {
		t.Fatalf("repositoryPathComponent() = %q, want readable hostname", got)
	}
	withPort := repositoryPathComponent("registry.example:443")
	withoutPort := repositoryPathComponent("registry.example-443")
	if withPort == withoutPort {
		t.Fatalf("repositoryPathComponent() collided: %q", withPort)
	}
	if !strings.HasPrefix(withPort, "x-") {
		t.Fatalf("repositoryPathComponent() = %q, want encoded special host", withPort)
	}
}

func TestApplyRegistryMirrorPreservesRepositoryAndReference(t *testing.T) {
	parsed, err := registry.ParseReference("docker.io/library/alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}

	got, err := applyRegistryMirror(parsed, map[string]string{
		"docker.io": "mirror.example.com:5000",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.Registry != "mirror.example.com:5000" {
		t.Fatalf("registry = %q", got.Registry)
	}
	if got.Repository != parsed.Repository {
		t.Fatalf("repository = %q, want %q", got.Repository, parsed.Repository)
	}
	if got.Reference != parsed.Reference {
		t.Fatalf("reference = %q, want %q", got.Reference, parsed.Reference)
	}
}

func TestApplyRegistryMirrorLeavesUnmatchedReference(t *testing.T) {
	parsed, err := registry.ParseReference("ghcr.io/acme/api@sha256:" + digest('a'))
	if err != nil {
		t.Fatal(err)
	}

	got, err := applyRegistryMirror(parsed, map[string]string{
		"docker.io": "mirror.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != parsed {
		t.Fatalf("reference changed: got %#v, want %#v", got, parsed)
	}
}

func TestValidateRegistryMirrorsRejectsRepositoryPath(t *testing.T) {
	err := ValidateRegistryMirrors(map[string]string{
		"docker.io": "mirror.example.com/team",
	})
	if err == nil {
		t.Fatal("ValidateRegistryMirrors() accepted a repository path")
	}
}

func TestValidateRegistryMirrorsAcceptsHosts(t *testing.T) {
	err := ValidateRegistryMirrors(map[string]string{
		"docker.io": "mirror.example.com:5000",
	})
	if err != nil {
		t.Fatalf("ValidateRegistryMirrors() error = %v", err)
	}
}
