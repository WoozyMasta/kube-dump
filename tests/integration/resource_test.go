//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	gitlib "github.com/go-git/go-git/v5"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const configMapName = "fixture"

// TestResourceSaveAgainstKind proves the public CLI path against a real API.
// The fixture deliberately narrows resources so this smoke test remains fast
// and diagnoses parser, kubeconfig, selection, and directory output together.
func TestResourceSaveAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	namespace := createConfigMapFixture(t, environment)

	destination := filepath.Join(environment.root, "resource-capture")
	environment.mustRunKubeCLI(t, "run resource save smoke test",
		"resource", "save", "dir", destination,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
	)

	artifact := filepath.Join(
		destination,
		"resources", "core", "v1", "configmaps", namespace, configMapName+".yaml",
	)
	content := readTestFile(t, artifact)
	requireContains(t, content,
		"apiVersion: v1",
		"kind: ConfigMap",
		"name: fixture",
		"key: value",
	)
}

// TestResourceBackupUsesBuiltinProfile verifies the default profile rather than an explicit resource override.
// The generated root CA ConfigMap is a representative object that the backup profile must omit.
func TestResourceBackupUsesBuiltinProfile(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	namespace := createSecretFixture(t, environment)
	ensureRootCAConfigMap(t, environment, namespace)
	destination := filepath.Join(environment.root, "resource-profile")

	environment.mustRunKubeCLI(t, "run resource backup with the default profile",
		"resource", "save", "dir", destination,
		"--namespace", namespace,
	)

	configMap := filepath.Join(
		destination, "resources", "core", "v1", "configmaps", namespace, configMapName+".yaml",
	)
	secret := filepath.Join(
		destination, "resources", "core", "v1", "secrets", namespace, "credentials.yaml",
	)
	if _, err := os.Stat(configMap); err != nil {
		t.Fatalf("profile backup did not save ConfigMap: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("profile backup did not save Secret: %v", err)
	}

	rootCA := filepath.Join(
		destination, "resources", "core", "v1", "configmaps", namespace, "kube-root-ca.crt.yaml",
	)
	if _, err := os.Stat(rootCA); !os.IsNotExist(err) {
		t.Fatalf("profile backup saved generated root CA ConfigMap: %v", err)
	}
}

// TestResourceSaveWithRestrictedRBAC verifies that forbidden resources
// do not turn an incomplete collection into a published partial snapshot.
func TestResourceSaveWithRestrictedRBAC(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	fixture := createRestrictedResourceFixture(t, environment)

	partialDestination := filepath.Join(environment.root, "resource-rbac-partial")
	partial := environment.mustFailCLI(t, "reject partial resource backup with restricted RBAC",
		"resource", "save", "dir", partialDestination,
		"--kubeconfig", fixture.kubeconfig,
		"--namespace", fixture.namespace,
		"--no-progress",
		"--log-level", "warn",
	)
	requireContains(t, partial.Stderr, "previous resource snapshot was kept", "forbidden")

	for _, artifact := range []string{
		filepath.Join(partialDestination, "resources", "core", "v1", "configmaps", fixture.namespace, "allowed-configmap.yaml"),
		filepath.Join(partialDestination, "resources", "apps", "v1", "deployments", fixture.namespace, "allowed-deployment.yaml"),
	} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("incomplete restricted backup published artifact %q: %v", artifact, err)
		}
	}

	forbiddenArtifact := filepath.Join(
		partialDestination, "resources", "core", "v1", "secrets", fixture.namespace, "forbidden-secret.yaml",
	)
	if _, err := os.Stat(forbiddenArtifact); !os.IsNotExist(err) {
		t.Fatalf("incomplete restricted backup published forbidden Secret: %v", err)
	}
}

// TestResourceBoundaryProfileAgainstKind verifies combined resource selection,
// normalization rules, and age field decryption through the real CLI.
func TestResourceBoundaryProfileAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.client(t)
	namespace := uniqueName("kube-dump-boundary-resource")
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create boundary resource namespace: %v", err)
	}

	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: boundaryObjectMeta("boundary-included"),
		Data:       map[string]string{"key": "value"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create included ConfigMap: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: boundaryObjectMeta("boundary-excluded"),
		Data:       map[string]string{"key": "excluded"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create excluded ConfigMap: %v", err)
	}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: boundaryObjectMeta("boundary-secret"),
		Data:       map[string][]byte{"password": []byte("secret-value")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create boundary Secret: %v", err)
	}
	if _, err := client.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
		ObjectMeta: boundaryObjectMeta("boundary-service"),
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Port: 80,
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create non-selected Service: %v", err)
	}

	profilePath := writeBoundaryProfile(t, environment.root)
	keys := writeAgeTestKeys(t, environment.root, "boundary-resource")
	destination := filepath.Join(environment.root, "boundary-resource-capture")
	environment.mustRunKubeCLI(t, "save resources with boundary profile",
		"resource", "save", "dir", destination,
		"--profile", profilePath,
		"--recipients-file", keys.recipient,
	)

	included := filepath.Join(
		destination, "resources", "core", "v1", "configmaps", namespace, "boundary-included.yaml",
	)
	if _, err := os.Stat(included); err != nil {
		t.Fatalf("included ConfigMap was not saved: %v", err)
	}

	excluded := filepath.Join(
		destination, "resources", "core", "v1", "configmaps", namespace, "boundary-excluded.yaml",
	)
	if _, err := os.Stat(excluded); !os.IsNotExist(err) {
		t.Fatalf("excluded ConfigMap was saved: %v", err)
	}

	notSelected := filepath.Join(
		destination, "resources", "core", "v1", "services", namespace, "boundary-service.yaml",
	)
	if _, err := os.Stat(notSelected); !os.IsNotExist(err) {
		t.Fatalf("non-selected Service was saved: %v", err)
	}

	secretPath := filepath.Join(
		destination, "resources", "core", "v1", "secrets", namespace, "boundary-secret.yaml",
	)
	secret := readTestFile(t, secretPath)
	requireContains(t, secret, "!kube-dump/age", "metadata:", "namespace:", "name:")
	if strings.Contains(secret, "integration.kube-dump/transient") {
		t.Logf("boundary Secret artifact:\n%s", secret)
		t.Fatal("transient annotation was not removed")
	}

	catResult := environment.mustRunCLI(t, "cat boundary-profile resources",
		"resource", "cat", "dir", destination,
		"--identity", keys.identity,
	)
	requireContains(t, catResult.Stdout,
		"name: boundary-secret",
		"namespace: "+namespace,
		"c2VjcmV0LXZhbHVl",
	)
	if strings.Contains(catResult.Stdout, "!kube-dump/age") {
		t.Fatal("cat output still contains encrypted field markers")
	}
}

// TestResourceFieldEncryptionCLI verifies the two supported inline field encryption modes
// through the real capture and cat commands.
// AES-SIV is captured twice to protect its stable ciphertext contract.
func TestResourceFieldEncryptionCLI(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		marker        string
		deterministic bool
	}{
		{name: "age", mode: "age", marker: "!kube-dump/age"},
		{name: "aes-siv", mode: "aes-siv", marker: "!kube-dump/aes-siv", deterministic: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newIntegrationEnvironment(t)
			namespace := createSecretFixture(t, environment)
			keys := writeAgeTestKeys(t, environment.root, test.name)
			destination := filepath.Join(environment.root, "resource-encrypted-"+test.name)
			args := []string{
				"resource", "save", "dir", destination,
				"--namespace", namespace,
				"--recipients-file", keys.recipient,
			}
			if test.mode == "aes-siv" {
				args = append(args, "--field-encryption", test.mode, "--identity", keys.identity)
			}

			environment.mustRunKubeCLI(t, "save encrypted resources", args...)
			secretPath := filepath.Join(
				destination, "resources", "core", "v1", "secrets", namespace, "credentials.yaml",
			)
			encrypted := readTestFile(t, secretPath)
			requireContains(t, encrypted, test.marker)

			if test.deterministic {
				before := encrypted
				environment.mustRunKubeCLI(t, "repeat AES-SIV resource backup", args...)
				if after := readTestFile(t, secretPath); after != before {
					t.Fatal("AES-SIV ciphertext changed during an identical backup")
				}
			}

			catResult := environment.mustRunCLI(t, "cat encrypted resources",
				"resource", "cat", "dir", destination,
				"--identity", keys.identity,
			)
			requireContains(t, catResult.Stdout, "c2VjcmV0LXZhbHVl")
			if strings.Contains(catResult.Stdout, test.marker) {
				t.Fatalf("cat output still contains %s marker", test.marker)
			}
		})
	}
}

// TestResourceSaveFailsWithoutKubernetes verifies that an API failure returns a non-zero status
// instead of being reported as an empty successful backup.
func TestResourceSaveFailsWithoutKubernetes(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	destination := filepath.Join(environment.root, "resource-failure")
	missingKubeconfig := filepath.Join(environment.root, "missing-kubeconfig")

	environment.mustFailCLI(t, "run resource save without Kubernetes",
		"resource", "save", "dir", destination,
		"--kubeconfig", missingKubeconfig,
		"--resource", "core/v1/configmaps",
		"--namespace", "missing",
		"--no-progress",
		"--log-level", "warn",
	)
}

// TestResourceArchiveRoundTripAgainstKind exercises archive creation, inspection,
// and extraction through the built CLI for both compressors.
func TestResourceArchiveRoundTripAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)

	tests := []struct {
		format string
	}{
		{format: "tar.gz"},
		{format: "tar.zst"},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			namespace := createConfigMapFixture(t, environment)
			archivePath := filepath.Join(environment.root, "resource-"+test.format)

			environment.mustRunKubeCLI(t, "create resource archive",
				"resource", "save", "archive", archivePath,
				"--resource", "core/v1/configmaps",
				"--namespace", namespace,
				"--format", test.format,
			)

			info, err := os.Stat(archivePath)
			if err != nil {
				t.Fatalf("stat resource archive: %v", err)
			}
			if info.Size() == 0 {
				t.Fatal("resource archive is empty")
			}

			inspectResult := environment.mustRunCLI(t, "inspect resource archive",
				"resource", "inspect", "archive", archivePath,
			)
			requireContains(t, inspectResult.Stdout, "core/v1/configmaps")

			extracted := filepath.Join(environment.root, "extracted-"+test.format)
			environment.mustRunCLI(t, "extract resource archive",
				"resource", "extract", "archive", archivePath, extracted,
				"--no-progress",
			)

			artifact := filepath.Join(
				extracted,
				"resources", "core", "v1", "configmaps", namespace, "fixture.yaml",
			)
			requireContains(t, readTestFile(t, artifact), "key: value")
		})
	}
}

// TestResourceEncryptedArchiveCLI verifies whole-archive age encryption at the CLI boundary,
// including cat and extract consumers.
func TestResourceEncryptedArchiveCLI(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	namespace := createConfigMapFixture(t, environment)
	keys := writeAgeTestKeys(t, environment.root, "archive")
	archivePath := filepath.Join(environment.root, "resource.tar.zst.age")

	environment.mustRunKubeCLI(t, "save encrypted resource archive",
		"resource", "save", "archive", archivePath,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
		"--format", "tar.zst.age",
		"--archive-recipients-file", keys.recipient,
	)

	catResult := environment.mustRunCLI(t, "cat encrypted resource archive",
		"resource", "cat", "archive", archivePath,
		"--identity", keys.identity,
	)
	requireContains(t, catResult.Stdout, "kind: ConfigMap", "name: fixture")

	extracted := filepath.Join(environment.root, "encrypted-archive-extracted")
	environment.mustRunCLI(t, "extract encrypted resource archive",
		"resource", "extract", "archive", archivePath, extracted,
		"--identity", keys.identity,
	)
	artifact := filepath.Join(
		extracted, "resources", "core", "v1", "configmaps", namespace, configMapName+".yaml",
	)
	requireContains(t, readTestFile(t, artifact), "key: value")
}

// TestResourceSaveAndDownloadFromS3AgainstKind verifies the S3 backend
// through the CLI while Kubernetes remains the source of the captured object.
func TestResourceSaveAndDownloadFromS3AgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.s3ServiceForTest(t)
	namespace := createConfigMapFixture(t, environment)
	uri := fmt.Sprintf("s3://%s", service.bucket)
	saveArgs := append([]string{"resource", "save", "s3"}, s3Args(uri, service)...)
	saveArgs = append(saveArgs,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
	)
	environment.mustRunKubeCLI(t, "save resources to S3", saveArgs...)

	inspectArgs := append([]string{"resource", "inspect", "s3"}, s3Args(uri, service)...)
	inspectResult := environment.mustRunCLI(t, "inspect resources in S3", inspectArgs...)
	requireContains(t, inspectResult.Stdout, "Objects:")

	catArgs := append([]string{"resource", "cat", "s3"}, s3Args(uri, service)...)
	catResult := environment.mustRunCLI(t, "cat resources from S3", catArgs...)
	requireContains(t, catResult.Stdout, "kind: ConfigMap", "name: fixture")

	destination := filepath.Join(environment.root, "s3-download")
	downloadArgs := append([]string{
		"resource", "download", "s3", destination,
	}, s3Args(uri, service)...)
	environment.mustRunCLI(t, "download resources from S3", downloadArgs...)

	artifact := filepath.Join(
		destination,
		"resources", "core", "v1", "configmaps", namespace, configMapName+".yaml",
	)
	requireContains(t, readTestFile(t, artifact), "key: value")
}

// TestResourceS3PruneRemovesStaleOwnedObjects verifies
// that --prune removes objects missing from the next complete backup in the same destination.
func TestResourceS3PruneRemovesStaleOwnedObjects(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.s3ServiceForTest(t)
	firstNamespace := createConfigMapFixture(t, environment)
	if _, err := environment.client(t).CoreV1().ConfigMaps(firstNamespace).Create(
		context.Background(),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "stale"},
			Data:       map[string]string{"key": "stale"},
		},
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create stale ConfigMap: %v", err)
	}

	uri := fmt.Sprintf("s3://%s/resource-prune", service.bucket)
	save := func(namespace string) {
		t.Helper()
		args := append([]string{"resource", "save", "s3"}, s3Args(uri, service)...)
		args = append(args,
			"--resource", "core/v1/configmaps",
			"--namespace", namespace,
			"--prune",
		)
		environment.mustRunKubeCLI(t, "save prunable resources to S3", args...)
	}

	save(firstNamespace)
	if err := environment.client(t).CoreV1().ConfigMaps(firstNamespace).Delete(
		context.Background(), "stale", metav1.DeleteOptions{},
	); err != nil {
		t.Fatalf("delete stale ConfigMap: %v", err)
	}
	save(firstNamespace)

	catArgs := append([]string{"resource", "cat", "s3"}, s3Args(uri, service)...)
	catResult := environment.mustRunCLI(t, "cat pruned resources from S3", catArgs...)
	requireContains(t, catResult.Stdout,
		"name: fixture",
		"namespace: "+firstNamespace,
	)
	if strings.Contains(catResult.Stdout, "name: stale") {
		t.Fatal("S3 prune retained a stale object owned by the current stream")
	}
}

// TestResourceAES256SIVS3BackupReusesKeyring verifies that repeated S3 backups reuse
// the existing deterministic field key instead of creating a new keyring.
func TestResourceAES256SIVS3BackupReusesKeyring(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.s3ServiceForTest(t)
	namespace := createSecretFixture(t, environment)
	keys := writeAgeTestKeys(t, environment.root, "s3-aes-siv")
	uri := fmt.Sprintf("s3://%s/%s", service.bucket, namespace)

	saveArgs := append([]string{"resource", "save", "s3"}, s3Args(uri, service)...)
	saveArgs = append(saveArgs,
		"--resource", "core/v1/secrets",
		"--namespace", namespace,
		"--recipients-file", keys.recipient,
		"--field-encryption", "aes-siv",
		"--identity", keys.identity,
	)
	environment.mustRunKubeCLI(t, "save AES-SIV resources to S3", saveArgs...)

	firstRoot := filepath.Join(environment.root, "s3-aes-siv-first")
	firstDownload := append([]string{
		"resource", "download", "s3", firstRoot,
	}, s3Args(uri, service)...)
	environment.mustRunCLI(t, "download first AES-SIV S3 backup", firstDownload...)
	first := readTestFile(t, filepath.Join(
		firstRoot, "resources", "core", "v1", "secrets", namespace, "credentials.yaml",
	))
	requireContains(t, first, "!kube-dump/aes-siv")

	environment.mustRunKubeCLI(t, "repeat AES-SIV resources to S3", saveArgs...)

	secondRoot := filepath.Join(environment.root, "s3-aes-siv-second")
	secondDownload := append([]string{
		"resource", "download", "s3", secondRoot,
	}, s3Args(uri, service)...)
	environment.mustRunCLI(t, "download repeated AES-SIV S3 backup", secondDownload...)
	second := readTestFile(t, filepath.Join(
		secondRoot, "resources", "core", "v1", "secrets", namespace, "credentials.yaml",
	))
	if second != first {
		t.Fatal("repeated AES-SIV S3 backup changed its ciphertext")
	}
}

// TestResourceArchiveS3RoundTripAgainstKind covers the remote single-archive path,
// including archive inspection, YAML rendering, and extraction.
func TestResourceArchiveS3RoundTripAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	service := environment.s3ServiceForTest(t)
	namespace := createConfigMapFixture(t, environment)
	uri := fmt.Sprintf("s3://%s/%s.tar.zst", service.bucket, namespace)

	saveArgs := append([]string{"resource", "save", "archive-s3"}, s3Args(uri, service)...)
	saveArgs = append(saveArgs,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
	)
	environment.mustRunKubeCLI(t, "save resources as an S3 archive", saveArgs...)

	inspectArgs := append([]string{"resource", "inspect", "archive-s3"}, s3Args(uri, service)...)
	inspectResult := environment.mustRunCLI(t, "inspect S3 resource archive", inspectArgs...)
	requireContains(t, inspectResult.Stdout, "Objects:")

	catArgs := append([]string{"resource", "cat", "archive-s3"}, s3Args(uri, service)...)
	catResult := environment.mustRunCLI(t, "cat S3 resource archive", catArgs...)
	requireContains(t, catResult.Stdout, "kind: ConfigMap", "name: fixture")

	extracted := filepath.Join(environment.root, "s3-archive-extracted")
	extractArgs := append([]string{
		"resource", "extract", "archive-s3", extracted,
	}, s3Args(uri, service)...)
	environment.mustRunCLI(t, "extract S3 resource archive", extractArgs...)
	artifact := filepath.Join(
		extracted, "resources", "core", "v1", "configmaps", namespace, configMapName+".yaml",
	)
	requireContains(t, readTestFile(t, artifact), "key: value")
}

// TestResourceSaveGitCreatesTrailersAgainstKind verifies native Git persistence
// and the metadata attached to a real backup commit.
func TestResourceSaveGitCreatesTrailersAgainstKind(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	namespace := createConfigMapFixture(t, environment)
	destination := filepath.Join(environment.root, "resource-git")

	environment.mustRunKubeCLI(t, "save resources to Git",
		"resource", "save", "git", destination,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
		"--git-commit",
	)

	repository, err := gitlib.PlainOpen(destination)
	if err != nil {
		t.Fatalf("open captured Git repository: %v", err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatalf("read captured Git HEAD: %v", err)
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("read captured Git commit: %v", err)
	}

	for _, expected := range []string{
		"kube-dump: backup",
		"Kube-Dump-Profile: backup",
		"Kube-Dump-Namespaces: " + namespace,
		"Kube-Dump-Resource-Count: 2",
		"Kube-Dump-Changed: 2",
	} {
		if !strings.Contains(commit.Message, expected) {
			t.Fatalf("Git commit does not contain %q:\n%s", expected, commit.Message)
		}
	}

	firstHead := head.Hash()
	environment.mustRunKubeCLI(t, "repeat unchanged resource Git backup",
		"resource", "save", "git", destination,
		"--resource", "core/v1/configmaps",
		"--namespace", namespace,
		"--git-commit",
	)

	repository, err = gitlib.PlainOpen(destination)
	if err != nil {
		t.Fatalf("reopen captured Git repository: %v", err)
	}
	head, err = repository.Head()
	if err != nil {
		t.Fatalf("read repeated Git HEAD: %v", err)
	}
	if head.Hash() != firstHead {
		t.Fatal("unchanged resource backup created an empty Git commit")
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatalf("open Git worktree: %v", err)
	}
	status, err := worktree.Status()
	if err != nil {
		t.Fatalf("read Git worktree status: %v", err)
	}
	if !status.IsClean() {
		t.Fatalf("repeated resource backup left Git worktree dirty: %s", status)
	}
}

type ageTestKeys struct {
	recipient string
	identity  string
}

// restrictedResourceFixture contains a namespace and kubeconfig
// for a limited ServiceAccount used to exercise real Kubernetes permission failures.
type restrictedResourceFixture struct {
	namespace  string
	kubeconfig string
}

// createRestrictedResourceFixture creates allowed and forbidden resources,
// binds a narrow Role, and writes a token-based kubeconfig for the CLI.
func createRestrictedResourceFixture(t *testing.T, environment *integrationEnvironment) restrictedResourceFixture {
	t.Helper()

	client := environment.client(t)
	ctx := context.Background()
	namespace := uniqueName("kube-dump-rbac")
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create restricted RBAC namespace: %v", err)
	}

	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "allowed-configmap"},
		Data:       map[string]string{"key": "value"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RBAC ConfigMap: %v", err)
	}
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "forbidden-secret"},
		Data:       map[string][]byte{"key": []byte("value")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RBAC Secret: %v", err)
	}
	if _, err := client.AppsV1().Deployments(namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "allowed-deployment"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "allowed"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "allowed"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: "registry.k8s.io/pause:3.10",
				}}},
			},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RBAC Deployment: %v", err)
	}

	serviceAccount := "resource-reader"
	if _, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create restricted ServiceAccount: %v", err)
	}
	roleName := "resource-reader"
	if _, err := client.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list"}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create restricted Role: %v", err)
	}
	if _, err := client.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      serviceAccount,
			Namespace: namespace,
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create restricted RoleBinding: %v", err)
	}

	expirationSeconds := int64(600)
	token, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expirationSeconds},
		}, metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create restricted ServiceAccount token: %v", err)
	}

	adminConfig, err := clientcmd.LoadFromFile(environment.kubeconfig)
	if err != nil {
		t.Fatalf("load admin kubeconfig: %v", err)
	}
	adminContext, ok := adminConfig.Contexts[adminConfig.CurrentContext]
	if !ok {
		t.Fatalf("admin kubeconfig context %q is missing", adminConfig.CurrentContext)
	}
	cluster, ok := adminConfig.Clusters[adminContext.Cluster]
	if !ok {
		t.Fatalf("admin kubeconfig cluster %q is missing", adminContext.Cluster)
	}

	restrictedConfig := &clientcmdapi.Config{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "restricted",
		Clusters: map[string]*clientcmdapi.Cluster{
			"kind": {
				Server:                   cluster.Server,
				CertificateAuthorityData: cluster.CertificateAuthorityData,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"restricted": {Token: token.Status.Token},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"restricted": {Cluster: "kind", AuthInfo: "restricted"},
		},
	}
	restrictedKubeconfig := filepath.Join(environment.root, "restricted-kubeconfig")
	if err := clientcmd.WriteToFile(*restrictedConfig, restrictedKubeconfig); err != nil {
		t.Fatalf("write restricted kubeconfig: %v", err)
	}

	return restrictedResourceFixture{namespace: namespace, kubeconfig: restrictedKubeconfig}
}

// writeAgeTestKeys creates disposable native age credentials for one CLI test.
func writeAgeTestKeys(t *testing.T, root, name string) ageTestKeys {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age integration identity: %v", err)
	}
	identityPath := filepath.Join(root, name+"-identity.txt")
	recipientPath := filepath.Join(root, name+"-recipient.txt")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write age integration identity: %v", err)
	}
	if err := os.WriteFile(recipientPath, []byte(identity.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatalf("write age integration recipient: %v", err)
	}

	return ageTestKeys{recipient: recipientPath, identity: identityPath}
}

// createConfigMapFixture creates one isolated namespace
// and a ConfigMap whose content is stable enough to assert in every resource integration scenario.
func createConfigMapFixture(t *testing.T, environment *integrationEnvironment) string {
	t.Helper()

	client := environment.client(t)
	namespace := uniqueName("kube-dump-fixture")
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create fixture namespace: %v", err)
	}

	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "fixture",
			Labels: map[string]string{"integration": "true"},
		},
		Data: map[string]string{"key": "value"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create fixture ConfigMap: %v", err)
	}

	return namespace
}

// createSecretFixture extends the standard resource fixture with one Secret.
func createSecretFixture(t *testing.T, environment *integrationEnvironment) string {
	t.Helper()

	namespace := createConfigMapFixture(t, environment)
	if _, err := environment.client(t).CoreV1().Secrets(namespace).Create(
		context.Background(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "credentials"},
			Data:       map[string][]byte{"password": []byte("secret-value")},
		},
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create fixture Secret: %v", err)
	}

	return namespace
}

// ensureRootCAConfigMap makes the profile exclusion test
// independent of the timing of Kubernetes' namespace controller.
func ensureRootCAConfigMap(t *testing.T, environment *integrationEnvironment, namespace string) {
	t.Helper()

	client := environment.client(t)
	configMaps := client.CoreV1().ConfigMaps(namespace)
	if _, err := configMaps.Get(context.Background(), "kube-root-ca.crt", metav1.GetOptions{}); err == nil {
		return
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("check generated root CA ConfigMap: %v", err)
	}

	if _, err := configMaps.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt"},
		Data:       map[string]string{"ca.crt": "integration"},
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create generated root CA ConfigMap: %v", err)
	}
}
