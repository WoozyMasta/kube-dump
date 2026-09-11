//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// TestPVCDataSaveAgainstKind verifies direct filesystem capture through a real PVC,
// helper Pod, Kubernetes exec stream, and local artifact store.
func TestPVCDataSaveAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createPVCFixture(t, environment)
	destination := filepath.Join(environment.root, "pvc-capture")

	environment.mustRunKubeCLI(t, "save PVC data",
		"pvc", "save", "dir", destination,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--pvc", fixture.pvc,
		"--compression", "gzip",
	)

	inspectResult := environment.mustRunCLI(t, "inspect PVC data", "pvc", "inspect", "dir", destination)
	requireContains(t, inspectResult.Stdout, "PVC artifacts: 1")

	archivePath, archiveSize := findPVCArchive(t, destination, "data.tar.gz")
	if archiveSize == 0 {
		t.Fatal("PVC archive is empty")
	}
	assertPVCArchive(t, archivePath, false)

	extracted := filepath.Join(environment.root, "pvc-extracted")
	environment.mustRunCLI(t, "extract PVC data", "pvc", "extract", archivePath, extracted)
	assertExtractedPVCData(t, extracted)
}

// TestPVCDataRestoreAgainstKind verifies revision selection
// and streaming a local PVC archive into a separate, pre-created target PVC.
func TestPVCDataRestoreAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	source := createPVCFixture(t, environment)
	target := createEmptyPVCFixture(t, environment, "restore-target")
	destination := filepath.Join(environment.root, "pvc-restore-source")

	environment.mustRunKubeCLI(t, "save PVC restore source",
		"pvc", "save", "dir", destination,
		"--strategy", "pod",
		"--image", source.helperImage,
		"--namespace", source.namespace,
		"--pvc", source.pvc,
		"--compression", "gzip",
	)

	environment.mustRunKubeCLI(t, "restore PVC archive",
		"pvc", "restore", "dir", destination,
		"--pvc", source.namespace+"/"+source.pvc,
		"--target-pvc", target.namespace+"/"+target.pvc,
		"--image", target.helperImage,
	)

	result := environment.runtime.exec(
		context.Background(),
		environment.cluster+"-control-plane",
		"cat", target.hostPath+"/site/index.txt",
	)
	if result.Err != nil {
		fatalCommand(t, "read restored PVC data", result)
	}
	if strings.TrimSpace(result.Stdout) != "kube-dump integration" {
		t.Fatalf("unexpected restored PVC data: %q", result.Stdout)
	}
}

// TestPVCEncryptedDataSaveAndExtractAgainstKind verifies age encryption of the complete PVC payload,
// including filename detection, metadata, and extraction.
func TestPVCEncryptedDataSaveAndExtractAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createPVCFixture(t, environment)
	keys := writeAgeTestKeys(t, environment.root, "pvc-archive")
	destination := filepath.Join(environment.root, "pvc-encrypted-capture")

	environment.mustRunKubeCLI(t, "save encrypted PVC data",
		"pvc", "save", "dir", destination,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--pvc", fixture.pvc,
		"--compression", "gzip",
		"--recipients-file", keys.recipient,
	)

	inspectResult := environment.mustRunCLI(t, "inspect encrypted PVC data",
		"pvc", "inspect", "dir", destination,
	)
	requireContains(t, inspectResult.Stdout, "PVC artifacts: 1")

	archivePath, archiveSize := findPVCArchive(t, destination, "data.tar.gz.age")
	if archiveSize == 0 {
		t.Fatal("encrypted PVC archive is empty")
	}
	assertPVCArchive(t, archivePath, true)

	extracted := filepath.Join(environment.root, "pvc-encrypted-extracted")
	environment.mustRunCLI(t, "extract encrypted PVC data",
		"pvc", "extract", archivePath, extracted,
		"--identity", keys.identity,
	)
	assertExtractedPVCData(t, extracted)
}

// TestPVCBoundaryProfileAgainstKind verifies profile name, label,
// and annotation filters before a helper Pod is created for PVC data capture.
func TestPVCBoundaryProfileAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createBoundaryPVCFixture(t, environment)
	profilePath := writeBoundaryProfile(t, environment.root)
	destination := filepath.Join(environment.root, "boundary-pvc-capture")

	environment.mustRunKubeCLI(t, "save PVC data with boundary profile",
		"pvc", "save", "dir", destination,
		"--profile", profilePath,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--compression", "gzip",
	)

	inspectResult := environment.mustRunCLI(t, "inspect boundary PVC data",
		"pvc", "inspect", "dir", destination,
	)
	requireContains(t, inspectResult.Stdout, "PVC artifacts: 1")

	archivePath, archiveSize := findPVCArchive(t, destination, "data.tar.gz")
	if archiveSize == 0 {
		t.Fatal("boundary PVC archive is empty")
	}

	extracted := filepath.Join(environment.root, "boundary-pvc-extracted")
	environment.mustRunCLI(t, "extract boundary PVC data", "pvc", "extract", archivePath, extracted)
	content := readTestFile(t, filepath.Join(extracted, "site", "index.txt"))
	if strings.TrimSpace(content) != "kube-dump integration" {
		t.Fatalf("unexpected boundary PVC data: %q", content)
	}
}

// TestPVCDataS3SaveAndDownloadAgainstKind verifies the complete PVC artifact transfer
// through S3 while retaining the archive format for extraction.
func TestPVCDataS3SaveAndDownloadAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createPVCFixture(t, environment)
	target := createEmptyPVCFixture(t, environment, "s3-restore-target")
	s3 := environment.s3ServiceForTest(t)
	uri := "s3://" + s3.bucket + "/pvc/" + fixture.namespace
	saveArgs := append([]string{"pvc", "save", "s3"}, s3Args(uri, s3)...)
	saveArgs = append(saveArgs,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--pvc", fixture.pvc,
		"--compression", "gzip",
		"--s3-pod-endpoint", s3.podEndpoint,
	)
	environment.mustRunKubeCLI(t, "save PVC data to S3", saveArgs...)

	restoreArgs := append([]string{"pvc", "restore", "s3"}, s3Args(uri, s3)...)
	restoreArgs = append(restoreArgs,
		"--pvc", fixture.namespace+"/"+fixture.pvc,
		"--target-pvc", target.namespace+"/"+target.pvc,
		"--image", target.helperImage,
		"--s3-pod-endpoint", s3.podEndpoint,
	)
	environment.mustRunKubeCLI(t, "restore PVC data from S3", restoreArgs...)

	result := environment.runtime.exec(
		context.Background(),
		environment.cluster+"-control-plane",
		"cat", target.hostPath+"/site/index.txt",
	)
	if result.Err != nil {
		fatalCommand(t, "read restored S3 PVC data", result)
	}
	if strings.TrimSpace(result.Stdout) != "kube-dump integration" {
		t.Fatalf("unexpected restored S3 PVC data: %q", result.Stdout)
	}

	encryptedTarget := createEmptyPVCFixture(t, environment, "s3-encrypted-restore-target")
	keys := writeAgeTestKeys(t, environment.root, "pvc-s3-restore")
	encryptedURI := "s3://" + s3.bucket + "/pvc-encrypted/" + fixture.namespace
	encryptedSaveArgs := append([]string{"pvc", "save", "s3"}, s3Args(encryptedURI, s3)...)
	encryptedSaveArgs = append(encryptedSaveArgs,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--pvc", fixture.pvc,
		"--compression", "gzip",
		"--recipients-file", keys.recipient,
		"--s3-pod-endpoint", s3.podEndpoint,
	)
	environment.mustRunKubeCLI(t, "save encrypted PVC data to S3", encryptedSaveArgs...)

	encryptedRestoreArgs := append([]string{"pvc", "restore", "s3"}, s3Args(encryptedURI, s3)...)
	encryptedRestoreArgs = append(encryptedRestoreArgs,
		"--pvc", fixture.namespace+"/"+fixture.pvc,
		"--target-pvc", encryptedTarget.namespace+"/"+encryptedTarget.pvc,
		"--image", encryptedTarget.helperImage,
		"--identity", keys.identity,
		"--s3-pod-endpoint", s3.podEndpoint,
	)
	environment.mustRunKubeCLI(t, "restore encrypted PVC data from S3", encryptedRestoreArgs...)

	encryptedResult := environment.runtime.exec(
		context.Background(),
		environment.cluster+"-control-plane",
		"cat", encryptedTarget.hostPath+"/site/index.txt",
	)
	if encryptedResult.Err != nil {
		fatalCommand(t, "read restored encrypted S3 PVC data", encryptedResult)
	}
	if strings.TrimSpace(encryptedResult.Stdout) != "kube-dump integration" {
		t.Fatalf("unexpected encrypted S3 PVC data: %q", encryptedResult.Stdout)
	}

	inspectArgs := append([]string{"pvc", "inspect", "s3"}, s3Args(uri, s3)...)
	inspectResult := environment.mustRunCLI(t, "inspect PVC data in S3", inspectArgs...)
	requireContains(t, inspectResult.Stdout, "1")

	destination := filepath.Join(environment.root, "pvc-s3-download")
	downloadArgs := append([]string{
		"pvc", "download", "s3", destination,
	}, s3Args(uri, s3)...)
	environment.mustRunCLI(t, "download PVC data from S3", downloadArgs...)

	archivePath, archiveSize := findPVCArchive(t, destination, "data.tar.gz")
	if archiveSize == 0 {
		t.Fatal("downloaded PVC archive is empty")
	}

	assertPVCArchive(t, archivePath, false)
	extracted := filepath.Join(environment.root, "pvc-s3-extracted")
	environment.mustRunCLI(t, "extract downloaded PVC data", "pvc", "extract", archivePath, extracted)
	content := readTestFile(t, filepath.Join(extracted, "site", "index.txt"))
	if strings.TrimSpace(content) != "kube-dump integration" {
		t.Fatalf("unexpected extracted S3 PVC data: %q", content)
	}
}

// TestPVCDataMaxSizeSkipsBeforeHelperCreation verifies that a size limit prevents an artifact
// from being created for an otherwise selected claim.
func TestPVCDataMaxSizeSkipsBeforeHelperCreation(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createPVCFixture(t, environment)
	destination := filepath.Join(environment.root, "pvc-size-limit")

	environment.mustRunKubeCLI(t, "apply PVC size limit",
		"pvc", "save", "dir", destination,
		"--strategy", "pod",
		"--image", fixture.helperImage,
		"--namespace", fixture.namespace,
		"--pvc", fixture.pvc,
		"--max-size", "1MiB",
	)

	archivePath, _ := findPVCArchiveIfPresent(t, destination, "data.tar.zst")
	if archivePath != "" {
		t.Fatalf("size-limited PVC unexpectedly produced an archive: %s", archivePath)
	}
}

// assertPVCArchive verifies metadata, the uncompressed payload digest,
// and the mode recorded for the seeded file in a plaintext PVC archive.
func assertPVCArchive(t *testing.T, archivePath string, encrypted bool) {
	t.Helper()

	metadataData, err := os.ReadFile(filepath.Join(filepath.Dir(archivePath), "metadata.yaml"))
	if err != nil {
		t.Fatalf("read PVC metadata: %v", err)
	}

	var metadata pvc.Metadata
	if err := yaml.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatalf("decode PVC metadata: %v", err)
	}
	if metadata.Encrypted != encrypted {
		t.Fatalf("PVC metadata encrypted = %t, want %t", metadata.Encrypted, encrypted)
	}
	if metadata.ContentSHA256 == "" {
		t.Fatal("PVC metadata has no content digest")
	}
	if encrypted {
		return
	}

	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read PVC archive: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		t.Fatalf("open PVC gzip stream: %v", err)
	}
	payload, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		t.Fatalf("read PVC tar stream: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close PVC gzip stream: %v", closeErr)
	}

	digest := sha256.Sum256(payload)
	if got := hex.EncodeToString(digest[:]); got != metadata.ContentSHA256 {
		t.Fatalf("PVC content digest = %s, want %s", got, metadata.ContentSHA256)
	}
	assertPVCArchiveEntry(t, payload, "site/index.txt", 0o640, "kube-dump integration")
	assertPVCArchiveEntry(t, payload, "private/index.txt", 0o600, "private kube-dump data")
}

// assertPVCArchiveEntry checks one tar entry without depending on host filesystem modes.
func assertPVCArchiveEntry(t *testing.T, payload []byte, name string, wantMode int64, wantContent string) {
	t.Helper()

	reader := tar.NewReader(bytes.NewReader(payload))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("PVC archive entry %q is missing", name)
		}
		if err != nil {
			t.Fatalf("read PVC tar entry: %v", err)
		}
		if header.Name != name {
			continue
		}

		if header.Mode&0o777 != wantMode {
			t.Fatalf("PVC archive mode for %q = %o, want %o", name, header.Mode&0o777, wantMode)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read PVC archive file %q: %v", name, err)
		}
		if strings.TrimSpace(string(content)) != wantContent {
			t.Fatalf("unexpected PVC archive data: %q", content)
		}

		return
	}
}

// assertExtractedPVCData verifies the stable fixture content after extraction.
func assertExtractedPVCData(t *testing.T, root string) {
	t.Helper()

	content := readTestFile(t, filepath.Join(root, "site", "index.txt"))
	if strings.TrimSpace(content) != "kube-dump integration" {
		t.Fatalf("unexpected extracted PVC data: %q", content)
	}

	privateContent := readTestFile(t, filepath.Join(root, "private", "index.txt"))
	if strings.TrimSpace(privateContent) != "private kube-dump data" {
		t.Fatalf("unexpected extracted private PVC data: %q", privateContent)
	}
}

// findPVCArchive locates one compressed PVC artifact in a local store.
func findPVCArchive(t *testing.T, root, name string) (string, int64) {
	t.Helper()

	archivePath, archiveSize := findPVCArchiveIfPresent(t, root, name)
	if archivePath == "" {
		t.Fatalf("PVC archive %q is missing below %s", name, root)
	}

	return archivePath, archiveSize
}

// findPVCArchiveIfPresent locates an optional compressed PVC artifact.
func findPVCArchiveIfPresent(t *testing.T, root, name string) (string, int64) {
	t.Helper()

	var archivePath string
	var archiveSize int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != name {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		archivePath = path
		archiveSize = info.Size()
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0
		}
		t.Fatalf("find PVC archive: %v", err)
	}

	return archivePath, archiveSize
}

// pvcFixture contains the objects and helper image used by one PVC test.
type pvcFixture struct {
	helperImage string
	hostPath    string
	namespace   string
	pvc         string
}

// boundaryPVCFixture contains selected and deliberately non-matching claims.
type boundaryPVCFixture struct {
	pvcFixture
}

// createPVCFixture creates a statically bound hostPath PVC and seeds its node data.
func createPVCFixture(t *testing.T, environment *integrationEnvironment) pvcFixture {
	return createPVCFixtureWithMetadataAndSeed(t, environment, "data", metav1.ObjectMeta{Name: "data"}, true)
}

// createPVCFixtureWithMetadata creates a static claim with caller-controlled metadata.
func createPVCFixtureWithMetadata(
	t *testing.T,
	environment *integrationEnvironment,
	pvcName string,
	metadata metav1.ObjectMeta,
) pvcFixture {
	return createPVCFixtureWithMetadataAndSeed(t, environment, pvcName, metadata, true)
}

// createEmptyPVCFixture creates a static claim without user data for restore tests.
func createEmptyPVCFixture(t *testing.T, environment *integrationEnvironment, pvcName string) pvcFixture {
	return createPVCFixtureWithMetadataAndSeed(t, environment, pvcName, metav1.ObjectMeta{Name: pvcName}, false)
}

func createPVCFixtureWithMetadataAndSeed(
	t *testing.T,
	environment *integrationEnvironment,
	pvcName string,
	metadata metav1.ObjectMeta,
	seed bool,
) pvcFixture {
	t.Helper()

	helperImage, err := environment.prepareVolumeImage()
	if err != nil {
		t.Fatalf("prepare PVC helper image: %v", err)
	}

	client := environment.client(t)
	ctx := context.Background()
	namespace := uniqueName("kube-dump-pvc")
	storageClass := uniqueName("kube-dump")
	pvName := uniqueName("kube-dump-pv")
	hostPath := "/var/lib/kube-dump-integration/" + pvName
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create PVC fixture namespace: %v", err)
	}

	_, err = client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: hostPath},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create fixture PV: %v", err)
	}

	_, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metadata,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create fixture PVC: %v", err)
	}

	waitForPVCBound(t, client, namespace, pvcName)
	fixture := pvcFixture{
		helperImage: helperImage,
		hostPath:    hostPath,
		namespace:   namespace,
		pvc:         pvcName,
	}
	if !seed {
		return fixture
	}

	seedCommand := fmt.Sprintf(`mkdir -p '%[1]s/site' '%[1]s/private' &&
printf 'kube-dump integration\n' > '%[1]s/site/index.txt' &&
chmod 640 '%[1]s/site/index.txt' &&
printf 'private kube-dump data\n' > '%[1]s/private/index.txt' &&
chmod 700 '%[1]s/private' && chmod 600 '%[1]s/private/index.txt' &&
chown -R 65532:65532 '%[1]s/private'`, hostPath)
	seedResult := environment.runtime.exec(
		context.Background(),
		environment.cluster+"-control-plane",
		"sh", "-c",
		seedCommand,
	)
	if seedResult.Err != nil {
		fatalCommand(t, "seed PVC hostPath data", seedResult)
	}

	return fixture
}

// createBoundaryPVCFixture creates two bound claims in one namespace
// so profile filters can be tested without allowing an excluded claim to reach the helper.
func createBoundaryPVCFixture(t *testing.T, environment *integrationEnvironment) boundaryPVCFixture {
	t.Helper()

	helperImage, err := environment.prepareVolumeImage()
	if err != nil {
		t.Fatalf("prepare boundary PVC helper image: %v", err)
	}

	client := environment.client(t)
	ctx := context.Background()
	namespace := uniqueName("kube-dump-pvc-boundary")
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create boundary PVC namespace: %v", err)
	}

	claims := []struct {
		name     string
		metadata metav1.ObjectMeta
		path     string
	}{
		{
			name:     "selected-data",
			metadata: boundaryObjectMeta("selected-data"),
			path:     "/var/lib/kube-dump-integration/boundary-selected",
		},
		{
			name: "selected-unmatched",
			metadata: metav1.ObjectMeta{
				Name: "selected-unmatched",
				Labels: map[string]string{
					"integration.kube-dump/profile": "boundary",
				},
			},
			path: "/var/lib/kube-dump-integration/boundary-unmatched",
		},
	}

	for _, item := range claims {
		storageClass := uniqueName("kube-dump-boundary")
		pvName := uniqueName("kube-dump-boundary-pv")
		_, err := client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: corev1.PersistentVolumeSpec{
				Capacity: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				StorageClassName:              storageClass,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: item.path},
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create boundary PV %s: %v", item.name, err)
		}

		storageClassCopy := storageClass
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: item.metadata,
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &storageClassCopy,
				VolumeName:       pvName,
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				}},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create boundary PVC %s: %v", item.name, err)
		}
		waitForPVCBound(t, client, namespace, item.name)
	}

	seedResult := environment.runtime.exec(
		context.Background(),
		environment.cluster+"-control-plane",
		"sh", "-c",
		`mkdir -p '/var/lib/kube-dump-integration/boundary-selected/site' '/var/lib/kube-dump-integration/boundary-selected/private' &&
printf 'kube-dump integration\n' > '/var/lib/kube-dump-integration/boundary-selected/site/index.txt' &&
chmod 640 '/var/lib/kube-dump-integration/boundary-selected/site/index.txt' &&
printf 'private kube-dump data\n' > '/var/lib/kube-dump-integration/boundary-selected/private/index.txt' &&
chmod 700 '/var/lib/kube-dump-integration/boundary-selected/private' &&
chmod 600 '/var/lib/kube-dump-integration/boundary-selected/private/index.txt' &&
chown -R 65532:65532 '/var/lib/kube-dump-integration/boundary-selected/private'`,
	)
	if seedResult.Err != nil {
		fatalCommand(t, "seed boundary PVC data", seedResult)
	}

	return boundaryPVCFixture{
		pvcFixture: pvcFixture{
			helperImage: helperImage,
			namespace:   namespace,
			pvc:         "selected-data",
		},
	}
}

// waitForPVCBound waits until the static claim is usable by the writer Pod.
func waitForPVCBound(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		claim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		return claim.Status.Phase == corev1.ClaimBound, nil
	})
	if err != nil {
		t.Fatalf("wait for PVC %s/%s to bind: %v", namespace, name, err)
	}
}
