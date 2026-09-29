package images

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestValidateRejectsOversizedLayoutMarker(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "oci-layout"),
		bytes.Repeat([]byte("x"), maxImageLayoutMarkerBytes+1),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	if err := Validate(root); err == nil {
		t.Fatal("Validate() accepted an oversized OCI layout marker")
	}
}

func TestIsImageLayerRecognizesOCIAndDockerMediaTypes(t *testing.T) {
	t.Parallel()

	for _, mediaType := range []string{
		ocispec.MediaTypeImageLayer,
		ocispec.MediaTypeImageLayerGzip,
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
	} {
		if !isImageLayer(mediaType) {
			t.Errorf("isImageLayer(%q) = false", mediaType)
		}
	}
}

func descriptorFor(mediaType string, data []byte) ocispec.Descriptor {
	digest := godigest.FromBytes(data)
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}
}

func TestInspectLayout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	config := []byte(`{"architecture":"amd64"}`)
	configDescriptor := descriptorFor("application/vnd.oci.image.config.v1+json", config)
	manifest := ocispec.Manifest{
		SchemaVersion: 2,
		Config:        configDescriptor,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDescriptor := descriptorFor(ocispec.MediaTypeImageManifest, manifestData)

	for _, item := range []struct {
		descriptor ocispec.Descriptor
		data       []byte
	}{
		{configDescriptor, config},
		{manifestDescriptor, manifestData},
	} {
		path := filepath.Join(root, "blobs", item.descriptor.Digest.Algorithm().String(), item.descriptor.Digest.Encoded())
		if err := os.WriteFile(path, item.data, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	first := manifestDescriptor
	first.Annotations = map[string]string{ocispec.AnnotationRefName: "ghcr.io/acme/api:v1"}
	second := manifestDescriptor
	second.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}

	index := ocispec.Index{
		SchemaVersion: 2,
		Manifests: []ocispec.Descriptor{
			first,
			second,
		},
	}

	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "sha256", "blob"), []byte("content"), 0o640); err != nil {
		t.Fatal(err)
	}

	summary, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Manifests != 2 || summary.Platforms != 1 || summary.Blobs != 3 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(summary.References) != 1 || summary.References[0] != "ghcr.io/acme/api:v1" {
		t.Fatalf("references = %#v", summary.References)
	}
}

func TestValidateRejectsCorruptReachableBlob(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	data := []byte(`{"schemaVersion":2}`)
	descriptor := descriptorFor(ocispec.MediaTypeImageManifest, data)
	index, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		Manifests:     []ocispec.Descriptor{descriptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), index, 0o640); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "blobs", "sha256", descriptor.Digest.Encoded())
	if err := os.WriteFile(blob, []byte(`{"schemaVersion":2,"corrupt":true}`), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Validate(root); err == nil {
		t.Fatal("Validate() accepted a corrupt reachable blob")
	}
}

func TestInspectMetadataDoesNotRequireBlobs(t *testing.T) {
	index := ocispec.Index{
		SchemaVersion: 2,
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{ocispec.AnnotationRefName: "example/api:v1"}},
		},
	}

	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}

	summary, err := InspectMetadata([]byte(`{"imageLayoutVersion":"1.0.0"}`), indexData)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Manifests != 1 || summary.Blobs != 0 || summary.References[0] != "example/api:v1" {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestInspectRejectsUnsupportedLayoutVersion(t *testing.T) {
	t.Parallel()

	if _, err := InspectMetadata(
		[]byte(`{"imageLayoutVersion":"2.0.0"}`),
		[]byte(`{"schemaVersion":2}`),
	); err == nil {
		t.Fatal("InspectMetadata() accepted an unsupported OCI layout version")
	}
}

func TestValidateAcceptsUnknownLeafMediaType(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}

	configData := []byte(`{}`)
	config := descriptorFor(ocispec.MediaTypeEmptyJSON, configData)
	manifestData, err := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		Config:        config,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := descriptorFor(ocispec.MediaTypeImageManifest, manifestData)

	for _, item := range []struct {
		descriptor ocispec.Descriptor
		data       []byte
	}{
		{config, configData},
		{manifest, manifestData},
	} {
		path := filepath.Join(root, "blobs", item.descriptor.Digest.Algorithm().String(), item.descriptor.Digest.Encoded())
		if err := os.WriteFile(path, item.data, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	index, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		Manifests:     []ocispec.Descriptor{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := validateIndexGraph(root, index); err != nil {
		t.Fatalf("validateIndexGraph() rejected an unknown leaf media type: %v", err)
	}
}

func TestValidateRejectsUnsupportedTopLevelMediaType(t *testing.T) {
	t.Parallel()

	index, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		Manifests: []ocispec.Descriptor{{
			MediaType: "application/octet-stream",
			Digest:    godigest.FromString("content"),
			Size:      7,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := validateIndexGraph(t.TempDir(), index); err == nil {
		t.Fatal("validateIndexGraph() accepted an unsupported top-level media type")
	}
}
