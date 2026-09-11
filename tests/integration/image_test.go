//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/woozymasta/orascope"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// TestImageSaveAndInspectAgainstRegistry verifies image discovery,
// OCI transfer, local layout creation, and inspection through the built CLI.
func TestImageSaveAndInspectAgainstRegistry(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	seedRegistryImage(t, service, nil)
	namespace := createImagePodFixture(t, environment, service.reference)
	layout := filepath.Join(environment.root, "image-layout")

	environment.mustRunKubeCLI(t, "save image layout",
		"image", "save", "dir", layout,
		"--namespace", namespace,
	)

	data := []byte(readTestFile(t, filepath.Join(layout, "images", ocispec.ImageIndexFile)))
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse image layout index: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("image layout manifest count = %d, want 1", len(index.Manifests))
	}
	if got := index.Manifests[0].Annotations[ocispec.AnnotationRefName]; got != service.reference {
		t.Fatalf("saved image reference = %q, want %q", got, service.reference)
	}

	inspectResult := environment.mustRunCLI(t, "inspect image layout", "image", "inspect", "dir", layout)
	requireContains(t, inspectResult.Stdout, service.reference)

	targetPrefix := service.endpoint[len("http://"):] + "/published"
	environment.mustRunCLI(t, "push image layout",
		"image", "push", "dir", layout, targetPrefix,
	)

	assertImagePublished(t, service.reference, targetPrefix)
}

// TestImageSaveMergesSharedLayout verifies that separate captures can add references
// to one local OCI layout and that --prune intentionally narrows it.
func TestImageSaveMergesSharedLayout(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	secondReference := service.endpoint[len("http://"):] + "/integration/second:v1"
	seedRegistryImageAtReference(t, service, service.reference, nil)
	seedRegistryImageAtReference(t, service, secondReference, nil)
	firstNamespace := createImagePodFixture(t, environment, service.reference)
	secondNamespace := createImagePodFixture(t, environment, secondReference)
	layout := filepath.Join(environment.root, "shared-image-layout")

	environment.mustRunKubeCLI(t, "save first shared image layout",
		"image", "save", "dir", layout,
		"--namespace", firstNamespace,
	)
	environment.mustRunKubeCLI(t, "merge second shared image layout",
		"image", "save", "dir", layout,
		"--namespace", secondNamespace,
	)
	assertImageLayoutReferences(t, layout, service.reference, secondReference)

	environment.mustRunKubeCLI(t, "prune shared image layout",
		"image", "save", "dir", layout,
		"--namespace", firstNamespace,
		"--prune",
	)
	assertImageLayoutReferences(t, layout, service.reference)
}

func assertImageLayoutReferences(t *testing.T, layout string, want ...string) {
	t.Helper()

	data := []byte(readTestFile(t, filepath.Join(layout, "images", ocispec.ImageIndexFile)))
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse shared image layout index: %v", err)
	}
	got := make(map[string]struct{}, len(index.Manifests))
	for _, descriptor := range index.Manifests {
		got[descriptor.Annotations[ocispec.AnnotationRefName]] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("shared image references = %#v, want %#v", got, want)
	}
	for _, reference := range want {
		if _, ok := got[reference]; !ok {
			t.Fatalf("shared image layout does not contain %q: %#v", reference, got)
		}
	}
}

// TestImageSaveRejectsIncompletePlatformSet verifies
// that a missing requested platform fails the capture before a local OCI layout is published.
func TestImageSaveRejectsIncompletePlatformSet(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	seedRegistryImage(t, service, nil)
	namespace := createImagePodFixture(t, environment, service.reference)
	layout := filepath.Join(environment.root, "incomplete-platform-layout")

	environment.mustFailCLI(t, "reject missing image platform", appendKubeCLIArgs(
		environment.kubeconfig,
		"image", "save", "dir", layout,
		"--namespace", namespace,
		"--image-platform", "windows/amd64",
	)...)
	if _, err := os.Stat(filepath.Join(layout, "images", ocispec.ImageIndexFile)); !os.IsNotExist(err) {
		t.Fatalf("incomplete platform capture published image index: %v", err)
	}
}

// TestImageSaveResolvesPlatformAliasesIndependently verifies
// that mutable tags sharing one runtime digest keep their own platform-specific registry resolves.
func TestImageSaveResolvesPlatformAliasesIndependently(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	stable := service.endpoint[len("http://"):] + "/integration/aliases:stable"
	latest := service.endpoint[len("http://"):] + "/integration/aliases:latest"
	stableDigest := seedRegistryImageAtReferenceWithLayer(t, service, stable, []byte("stable image\n"), nil)
	latestDigest := seedRegistryImageAtReferenceWithLayer(t, service, latest, []byte("latest image\n"), nil)
	namespace := createImagePodFixtureWithRuntime(t, environment, stable, stableDigest)
	addImagePodFixtureWithRuntime(t, environment, namespace, "latest", latest, stableDigest)
	layout := filepath.Join(environment.root, "platform-alias-layout")

	environment.mustRunKubeCLI(t, "save independently resolved image aliases",
		"image", "save", "dir", layout,
		"--namespace", namespace,
		"--image-platform", "linux/amd64",
	)

	data := []byte(readTestFile(t, filepath.Join(layout, "images", ocispec.ImageIndexFile)))
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse image layout index: %v", err)
	}

	descriptors := make(map[string]ocispec.Descriptor, len(index.Manifests))
	for _, descriptor := range index.Manifests {
		reference := descriptor.Annotations[ocispec.AnnotationRefName]
		descriptors[reference] = descriptor
	}
	for _, reference := range []string{stable, latest} {
		if _, ok := descriptors[reference]; !ok {
			t.Fatalf("saved image layout does not contain %q: %#v", reference, descriptors)
		}
	}
	if descriptors[stable].Digest == descriptors[latest].Digest {
		t.Fatalf("platform aliases share one descriptor: %s", descriptors[stable].Digest)
	}
	stableName := stable[:strings.LastIndexByte(stable, ':')]
	runtimeReference := stableName + "@" + stableDigest
	if _, ok := descriptors[runtimeReference]; !ok {
		t.Fatalf("saved image layout does not contain runtime reference %q: %#v", runtimeReference, descriptors)
	}
	if latestDigest == stableDigest {
		t.Fatal("test fixtures unexpectedly share one manifest digest")
	}
}

// TestImagePushSelectedReferencesAgainstRegistry verifies that image push can publish
// one saved reference without publishing the other references in the layout.
func TestImagePushSelectedReferencesAgainstRegistry(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	otherReference := service.endpoint[len("http://"):] + "/integration/other:v1"
	seedRegistryImageAtReference(t, service, service.reference, nil)
	seedRegistryImageAtReference(t, service, otherReference, nil)
	namespace := createImagePodFixture(t, environment, service.reference)
	addImagePodFixture(t, environment, namespace, "other", otherReference)
	layout := filepath.Join(environment.root, "selected-image-layout")

	environment.mustRunKubeCLI(t, "save image layout for selective push",
		"image", "save", "dir", layout,
		"--namespace", namespace,
	)

	targetPrefix := service.endpoint[len("http://"):] + "/published-selected"
	environment.mustRunCLI(t, "push selected image",
		"image", "push", "dir", layout, targetPrefix, service.reference,
	)

	assertImagePublished(t, service.reference, targetPrefix)
	assertImageNotPublished(t, otherReference, targetPrefix)
}

// TestImageBoundaryProfileAgainstKind verifies Pod metadata, container type,
// and image reference filters before registry access.
func TestImageBoundaryProfileAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.registryServiceForTest(t)
	otherReference := service.endpoint[len("http://"):] + "/other/fixture:v1"
	excludedReference := service.endpoint[len("http://"):] + "/integration/excluded:v1"
	seedRegistryImageAtReference(t, service, service.reference, nil)
	seedRegistryImageAtReference(t, service, otherReference, nil)
	seedRegistryImageAtReference(t, service, excludedReference, nil)
	createBoundaryImageFixture(t, environment, service.reference, otherReference, excludedReference)
	profilePath := writeBoundaryProfile(t, environment.root)
	layout := filepath.Join(environment.root, "boundary-image-layout")

	environment.mustRunKubeCLI(t, "save images with boundary profile",
		"image", "save", "dir", layout,
		"--profile", profilePath,
	)

	data := []byte(readTestFile(t, filepath.Join(layout, "images", ocispec.ImageIndexFile)))
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse boundary image layout index: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("boundary image manifest count = %d, want 1", len(index.Manifests))
	}
	if got := index.Manifests[0].Annotations[ocispec.AnnotationRefName]; got != service.reference {
		t.Fatalf("boundary image reference = %q, want %q", got, service.reference)
	}
}

// TestImageSaveAuthenticationAgainstRegistry verifies Docker-compatible credentials
// and ensures invalid credentials fail through the CLI.
func TestImageSaveAuthenticationAgainstRegistry(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.privateRegistryServiceForTest(t)
	pathAuthDirectory := filepath.Join(environment.root, "path-docker-auth")
	if err := writeDockerAuthConfigForRepository(pathAuthDirectory, service, "integration"); err != nil {
		t.Fatalf("write path-scoped Docker auth config: %v", err)
	}

	auth, err := orascope.New(
		orascope.WithDockerAuthConfigJSON(
			dockerAuthConfigDataForRepository(service, "integration"),
		),
		orascope.WithoutDiscovery(),
	)
	if err != nil {
		t.Fatalf("create fixture registry credentials: %v", err)
	}
	seedRegistryImage(t, service, auth)
	namespace := createImagePodFixture(t, environment, service.reference)

	layout := filepath.Join(environment.root, "authenticated-image-layout")
	environment.mustRunKubeCLIWithEnv(t, "save image layout with valid credentials",
		[]string{"DOCKER_CONFIG=" + pathAuthDirectory},
		"image", "save", "dir", layout,
		"--namespace", namespace,
	)

	invalidService := *service
	invalidService.password = "wrong-password"
	invalidDirectory := filepath.Join(environment.root, "invalid-docker-auth")
	if err := writeDockerAuthConfigForRepository(invalidDirectory, &invalidService, "integration"); err != nil {
		t.Fatalf("write invalid Docker auth config: %v", err)
	}
	failed := environment.mustFailKubeCLIWithEnv(t, "save image layout with invalid credentials",
		[]string{"DOCKER_CONFIG=" + invalidDirectory},
		"image", "save", "dir", filepath.Join(environment.root, "invalid-layout"),
		"--namespace", namespace,
	)
	if strings.Contains(failed.Stdout+failed.Stderr, service.password) {
		t.Fatal("registry password appeared in CLI diagnostics")
	}
}

// TestImageSaveUsesPullSecretAgainstRegistry verifies Kubernetes imagePullSecret credentials
// when the process has no usable local Docker auth configuration.
func TestImageSaveUsesPullSecretAgainstRegistry(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.privateRegistryServiceForTest(t)
	auth, err := orascope.New(
		orascope.WithDockerAuthConfigJSON(dockerAuthConfigData(service)),
		orascope.WithoutDiscovery(),
	)
	if err != nil {
		t.Fatalf("create fixture registry credentials: %v", err)
	}

	seedRegistryImage(t, service, auth)
	namespace := createImagePodFixture(t, environment, service.reference, true)
	emptyAuthDirectory := filepath.Join(environment.root, "empty-docker-auth")
	if err := os.MkdirAll(emptyAuthDirectory, 0o700); err != nil {
		t.Fatalf("create empty Docker auth directory: %v", err)
	}

	environment.mustRunKubeCLIWithEnv(t, "save image layout with imagePullSecret",
		[]string{"DOCKER_CONFIG=" + emptyAuthDirectory},
		"image", "save", "dir", filepath.Join(environment.root, "pull-secret-layout"),
		"--namespace", namespace,
		"--image-pull-secrets",
	)
}

// TestImageSaveAndDownloadFromS3AgainstKind verifies an OCI layout round-trip
// through the S3 backend while the source image comes from a real registry.
func TestImageSaveAndDownloadFromS3AgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	registryService := environment.registryServiceForTest(t)
	seedRegistryImage(t, registryService, nil)
	namespace := createImagePodFixture(t, environment, registryService.reference)
	s3 := environment.s3ServiceForTest(t)
	uri := fmt.Sprintf("s3://%s", s3.bucket)
	saveArgs := append([]string{"image", "save", "s3"}, s3Args(uri, s3)...)
	saveArgs = append(saveArgs,
		"--namespace", namespace,
	)
	environment.mustRunKubeCLI(t, "save images to S3", saveArgs...)
	// A second capture must reuse the content-addressed blobs already stored
	// below the same S3 prefix instead of staging and uploading them again.
	environment.mustRunKubeCLI(t, "reuse images in S3", saveArgs...)

	inspectArgs := append([]string{"image", "inspect", "s3"}, s3Args(uri, s3)...)
	inspectResult := environment.mustRunCLI(t, "inspect images in S3", inspectArgs...)
	requireContains(t, inspectResult.Stdout, registryService.reference)

	targetPrefix := registryService.endpoint[len("http://"):] + "/published-s3"
	pushArgs := append([]string{
		"image", "push", "s3", targetPrefix,
	}, s3Args(uri, s3)...)
	environment.mustRunCLI(t, "push images from S3", pushArgs...)
	assertImagePublished(t, registryService.reference, targetPrefix)

	destination := filepath.Join(environment.root, "s3-image-download")
	downloadArgs := append([]string{
		"image", "download", "s3", destination,
	}, s3Args(uri, s3)...)
	environment.mustRunCLI(t, "download images from S3", downloadArgs...)

	requireContains(t, readTestFile(t, filepath.Join(destination, "images", ocispec.ImageIndexFile)), registryService.reference)
}

// assertImagePublished verifies that a pushed layout created the expected registry tag
// below the target prefix used by the image commands.
func assertImagePublished(t *testing.T, source, targetPrefix string) {
	t.Helper()

	parsedSource, err := registry.ParseReference(strings.TrimPrefix(source, "http://"))
	if err != nil {
		t.Fatalf("parse source image reference for push assertion: %v", err)
	}
	parsedTarget, err := registry.ParseReference(
		targetPrefix + "/" + publishedRegistryPath(parsedSource.Registry) +
			"/" + parsedSource.Repository + ":v1",
	)
	if err != nil {
		t.Fatalf("parse target image reference for push assertion: %v", err)
	}

	targetRepository, err := remote.NewRepository(parsedTarget.Registry + "/" + parsedTarget.Repository)
	if err != nil {
		t.Fatalf("create target registry repository for push assertion: %v", err)
	}
	targetRepository.PlainHTTP = true
	if _, err := targetRepository.Resolve(context.Background(), "v1"); err != nil {
		t.Fatalf("resolve pushed image: %v", err)
	}
}

// assertImageNotPublished verifies that selective push did not create the target tag.
func assertImageNotPublished(t *testing.T, source, targetPrefix string) {
	t.Helper()

	parsedSource, err := registry.ParseReference(strings.TrimPrefix(source, "http://"))
	if err != nil {
		t.Fatalf("parse source image reference for push assertion: %v", err)
	}
	parsedTarget, err := registry.ParseReference(
		targetPrefix + "/" + publishedRegistryPath(parsedSource.Registry) +
			"/" + parsedSource.Repository + ":v1",
	)
	if err != nil {
		t.Fatalf("parse target image reference: %v", err)
	}

	targetRepository, err := remote.NewRepository(parsedTarget.Registry + "/" + parsedTarget.Repository)
	if err != nil {
		t.Fatalf("create target registry repository for push assertion: %v", err)
	}
	targetRepository.PlainHTTP = true
	if _, err := targetRepository.Resolve(context.Background(), "v1"); err == nil {
		t.Fatalf("image %q was published unexpectedly", source)
	}
}

// publishedRegistryPath mirrors the injective registry-host component used by image publication.
// Hosts containing a port are encoded to avoid collisions
// with ordinary hostnames such as registry.example-443.
func publishedRegistryPath(registryHost string) string {
	for _, char := range registryHost {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '-' {
			continue
		}

		encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(registryHost))
		return "x-" + strings.ToLower(encoded)
	}

	return registryHost
}

// seedRegistryImage pushes a deterministic single-platform image
// without downloading or invoking an external image builder.
func seedRegistryImage(t *testing.T, service *registryService, auth *orascope.Adapter) {
	seedRegistryImageAtReference(t, service, service.reference, auth)
}

// seedRegistryImageAtReference pushes the deterministic fixture to an explicit repository reference.
func seedRegistryImageAtReference(
	t *testing.T,
	service *registryService,
	reference string,
	auth *orascope.Adapter,
) string {
	return seedRegistryImageAtReferenceWithLayer(
		t,
		service,
		reference,
		[]byte("kube-dump integration image layer\n"),
		auth,
	)
}

// seedRegistryImageAtReferenceWithLayer pushes a deterministic fixture with caller-selected layer bytes.
func seedRegistryImageAtReferenceWithLayer(
	t *testing.T,
	service *registryService,
	reference string,
	layer []byte,
	auth *orascope.Adapter,
) string {
	t.Helper()

	ctx := context.Background()
	store := memory.New()
	layerDigest := digest.FromBytes(layer)
	config := []byte(fmt.Sprintf(
		`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]}}`,
		layerDigest.String(),
	))
	configDescriptor := content.NewDescriptorFromBytes(
		ocispec.MediaTypeImageConfig,
		config,
	)
	layerDescriptor := content.NewDescriptorFromBytes(
		ocispec.MediaTypeImageLayer,
		layer,
	)
	if err := store.Push(ctx, configDescriptor, bytes.NewReader(config)); err != nil {
		t.Fatalf("store image config: %v", err)
	}
	if err := store.Push(ctx, layerDescriptor, bytes.NewReader(layer)); err != nil {
		t.Fatalf("store image layer: %v", err)
	}

	manifestData, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDescriptor,
		Layers:    []ocispec.Descriptor{layerDescriptor},
	})
	if err != nil {
		t.Fatalf("marshal fixture image manifest: %v", err)
	}
	manifestDescriptor := content.NewDescriptorFromBytes(
		ocispec.MediaTypeImageManifest,
		manifestData,
	)
	if err := store.Push(ctx, manifestDescriptor, bytes.NewReader(manifestData)); err != nil {
		t.Fatalf("store fixture image manifest: %v", err)
	}
	parsed, err := registry.ParseReference(strings.TrimPrefix(reference, "http://"))
	if err != nil {
		t.Fatalf("parse fixture image reference: %v", err)
	}
	tag := parsed.ReferenceOrDefault()
	if err := store.Tag(ctx, manifestDescriptor, tag); err != nil {
		t.Fatalf("tag fixture image manifest: %v", err)
	}

	repository, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		t.Fatalf("create fixture registry repository: %v", err)
	}
	repository.PlainHTTP = true
	if auth != nil {
		if err := auth.WrapRepository(repository); err != nil {
			t.Fatalf("configure fixture registry credentials: %v", err)
		}
	}
	if _, err := oras.Copy(ctx, store, tag, repository, tag, oras.DefaultCopyOptions); err != nil {
		t.Fatalf("push fixture image: %v", err)
	}

	return manifestDescriptor.Digest.String()
}

// createImagePodFixture creates a Pod object without scheduling it,
// so the test exercises image discovery while the kind node does not pull the image.
func createImagePodFixture(
	t *testing.T,
	environment *integrationEnvironment,
	image string,
	pullSecret ...bool,
) string {
	return createImagePodFixtureWithRuntime(t, environment, image, "", pullSecret...)
}

// createImagePodFixtureWithRuntime creates an image fixture with an optional runtime image identity.
func createImagePodFixtureWithRuntime(
	t *testing.T,
	environment *integrationEnvironment,
	image, runtimeDigest string,
	pullSecret ...bool,
) string {
	t.Helper()

	client := environment.client(t)
	namespace := uniqueName("kube-dump-image")
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create image fixture namespace: %v", err)
	}

	usePullSecret := len(pullSecret) > 0 && pullSecret[0]
	if usePullSecret {
		if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "regcred"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerAuthConfigData(environment.privateRegistry),
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create imagePullSecret fixture: %v", err)
		}
	}

	pod, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector: map[string]string{
				"kube-dump.integration/unschedulable": "true",
			},
			ImagePullSecrets: func() []corev1.LocalObjectReference {
				if !usePullSecret {
					return nil
				}

				return []corev1.LocalObjectReference{{Name: "regcred"}}
			}(),
			Containers: []corev1.Container{{
				Name:            "fixture",
				Image:           image,
				ImagePullPolicy: corev1.PullNever,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create image fixture Pod: %v", err)
	}
	if runtimeDigest != "" {
		setImageFixtureRuntimeStatus(t, client, namespace, pod.Name, "fixture", image, runtimeDigest)
	}

	return namespace
}

// addImagePodFixture adds a second unscheduled Pod to an existing image fixture namespace.
func addImagePodFixture(
	t *testing.T,
	environment *integrationEnvironment,
	namespace, podName, image string,
) {
	addImagePodFixtureWithRuntime(t, environment, namespace, podName, image, "")
}

// addImagePodFixtureWithRuntime adds an image fixture with an optional runtime image identity.
func addImagePodFixtureWithRuntime(
	t *testing.T,
	environment *integrationEnvironment,
	namespace, podName, image, runtimeDigest string,
) {
	t.Helper()

	client := environment.client(t)
	ctx := context.Background()
	pod, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector: map[string]string{
				"kube-dump.integration/unschedulable": "true",
			},
			Containers: []corev1.Container{{
				Name:            podName,
				Image:           image,
				ImagePullPolicy: corev1.PullNever,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create additional image fixture Pod: %v", err)
	}
	if runtimeDigest != "" {
		setImageFixtureRuntimeStatus(t, client, namespace, pod.Name, podName, image, runtimeDigest)
	}
}

// setImageFixtureRuntimeStatus updates status after creation despite controller resource-version races.
func setImageFixtureRuntimeStatus(
	t *testing.T,
	client kubernetes.Interface,
	namespace, podName, containerName, image, runtimeDigest string,
) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 5; attempt++ {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read image fixture Pod before status update: %v", err)
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:    containerName,
			ImageID: "docker-pullable://" + strings.TrimPrefix(image, "http://") + "@" + runtimeDigest,
		}}
		if _, err := client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err == nil {
			return
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("update image fixture runtime status: %v", err)
		}
	}

	t.Fatalf("update image fixture runtime status: resource version stayed in conflict")
}

// createBoundaryImageFixture creates one matching Pod and one Pod excluded by annotation.
// The matching Pod also contains a regular image that fails the init-container filter,
// making each selection dimension observable.
func createBoundaryImageFixture(
	t *testing.T,
	environment *integrationEnvironment,
	selectedImage, regularImage, excludedImage string,
) string {
	t.Helper()

	client := environment.client(t)
	namespace := uniqueName("kube-dump-image-boundary")
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create boundary image namespace: %v", err)
	}

	matchingMeta := boundaryObjectMeta("boundary-matching")
	_, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: matchingMeta,
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "regular",
				Image:           regularImage,
				ImagePullPolicy: corev1.PullNever,
			}},
			InitContainers: []corev1.Container{{
				Name:            "selected-init",
				Image:           selectedImage,
				ImagePullPolicy: corev1.PullNever,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create matching image Pod: %v", err)
	}

	excludedMeta := boundaryObjectMeta("boundary-excluded")
	delete(excludedMeta.Annotations, "integration.kube-dump/selected")
	_, err = client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: excludedMeta,
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "excluded",
				Image:           excludedImage,
				ImagePullPolicy: corev1.PullNever,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create excluded image Pod: %v", err)
	}

	return namespace
}
