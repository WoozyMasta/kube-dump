//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	integrationCommandTimeout = 2 * time.Minute
	clusterReadinessTimeout   = 2 * time.Minute
	seaweedfsImage            = "chrislusf/seaweedfs@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5"
	registryImage             = "registry@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33"
)

// integrationEnvironment owns the external resources shared by one test.
type integrationEnvironment struct {
	binary          string             // binary is the absolute path to the CLI built for this test run.
	kindPath        string             // kindPath is the selected kind executable used for cluster lifecycle commands.
	cluster         string             // cluster is the unique kind cluster name used by this environment.
	clusterStarted  bool               // clusterStarted reports that kind creation was attempted and cleanup is required.
	kubeconfig      string             // kubeconfig is a temporary kubeconfig written by kind.
	root            string             // root is the temporary directory for all test-owned files.
	kindEnv         []string           // kindEnv contains provider-specific environment variables for kind.
	runtime         containerRuntime   // runtime runs the container commands used by kind and service fixtures.
	services        []serviceContainer // services contains every test-owned container that must be removed at cleanup.
	s3              *s3Service         // s3 is the lazily started S3 fixture for tests that need object storage.
	registry        *registryService   // registry is the lazily started OCI registry fixture for image tests.
	privateRegistry *registryService   // privateRegistry is the registry fixture used by authentication tests.
	volumeImage     string             // volumeImage is the locally loaded helper image used by PVC tests.
}

// serviceContainer identifies a disposable container started by the harness.
type serviceContainer struct {
	container string // container is the runtime container name.
}

// containerRuntime is the small Docker/Podman command adapter used by tests.
type containerRuntime struct {
	name string // name is the selected provider name accepted by kind.
	path string // path is the absolute executable path used for service containers.
}

// s3Service describes the temporary SeaweedFS endpoint and its test credentials.
type s3Service struct {
	container     string // container is the runtime container name.
	endpoint      string // endpoint is the HTTP endpoint reachable from the test runner.
	podEndpoint   string // podEndpoint is the endpoint reachable from Kubernetes mover Pods.
	bucket        string // bucket is the pre-created test bucket.
	accessKey     string // accessKey is the test-only S3 access key.
	secretKeyFile string // secretKeyFile contains the test-only secret used by the CLI.
}

// registryService describes the temporary unauthenticated OCI registry.
type registryService struct {
	container string // container is the runtime container name.
	endpoint  string // endpoint is the loopback HTTP endpoint exposed to the host.
	reference string // reference is the fixture image reference pushed into the registry.
	username  string // username are credentials accepted by a private registry.
	password  string // password are credentials accepted by a private registry.
}

// commandResult preserves separate process streams for useful failure output.
type commandResult struct {
	Command string // Command is the redacted, human-readable command line.
	Stdout  string // Stdout contains the complete standard output.
	Stderr  string // Stderr contains the complete standard error.
	Err     error  // Err is the process or context error, if execution failed.
}

var packageIntegrationEnvironment *integrationEnvironment

// TestMain prepares one isolated environment for the integration package
// and removes it after all CLI-level tests have finished.
func TestMain(main *testing.M) {
	environment, err := setupIntegrationEnvironment()
	if err != nil {
		if environment != nil {
			cleanupIntegrationEnvironment(environment, true)
		}
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n", err)
		os.Exit(1)
	}

	packageIntegrationEnvironment = environment
	exitCode := main.Run()
	cleanupIntegrationEnvironment(environment, exitCode != 0)
	os.Exit(exitCode)
}

// newIntegrationEnvironment returns the package-scoped kind environment.
func newIntegrationEnvironment(t *testing.T) *integrationEnvironment {
	t.Helper()

	if packageIntegrationEnvironment == nil {
		t.Fatal("integration environment was not initialized")
	}

	return packageIntegrationEnvironment
}

// setupIntegrationEnvironment builds the binary
// and starts an isolated kind cluster with an explicitly managed kubeconfig.
func setupIntegrationEnvironment() (*integrationEnvironment, error) {
	kindPath, err := integrationExecutable("KUBE_DUMP_INTEGRATION_KIND", "kind")
	if err != nil {
		return nil, err
	}

	runtimeName, runtimePath, err := selectContainerRuntime()
	if err != nil {
		return nil, err
	}

	goPath, err := lookupExecutable("go")
	if err != nil {
		return nil, err
	}

	root, err := os.MkdirTemp("", "kube-dump-integration-")
	if err != nil {
		return nil, fmt.Errorf("create integration temporary directory: %w", err)
	}

	environment := &integrationEnvironment{
		cluster:    uniqueName("kube-dump-it"),
		kubeconfig: filepath.Join(root, "kubeconfig"),
		kindPath:   kindPath,
		root:       root,
		kindEnv:    []string{"KIND_EXPERIMENTAL_PROVIDER=" + runtimeName},
		runtime:    containerRuntime{name: runtimeName, path: runtimePath},
	}
	environment.binary, err = buildBinary(goPath, root)
	if err != nil {
		return environment, err
	}

	environment.clusterStarted = true
	createContext, createCancel := context.WithTimeout(context.Background(), clusterReadinessTimeout)
	createResult := runCommand(
		createContext,
		root,
		environment.kindEnv,
		kindPath,
		"create", "cluster",
		"--name", environment.cluster,
		"--kubeconfig", environment.kubeconfig,
		"--wait", "120s",
	)
	createCancel()
	if createResult.Err != nil {
		return environment, fmt.Errorf(
			"create kind cluster failed: %s\n%s",
			createResult.Command,
			formatCommandOutput(createResult),
		)
	}
	if err := waitForKubernetes(environment.kubeconfig); err != nil {
		return environment, fmt.Errorf("wait for kind cluster readiness: %w", err)
	}

	return environment, nil
}

// cleanupIntegrationEnvironment removes the package environment
// and preserves diagnostics when setup or one of the tests fails.
func cleanupIntegrationEnvironment(environment *integrationEnvironment, failed bool) {
	if environment == nil {
		return
	}

	keep := os.Getenv("KUBE_DUMP_KEEP_INTEGRATION_ENV") == "1"
	for _, service := range environment.services {
		if failed {
			if err := exportContainerLogs(environment, service.container); err != nil {
				fmt.Fprintf(os.Stderr, "export service logs failed: %v\n", err)
			}
		}
		if keep {
			continue
		}

		removeContext, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
		removeResult := environment.runtime.remove(removeContext, service.container)
		cancel()
		if removeResult.Err != nil {
			fmt.Fprintf(
				os.Stderr,
				"remove service container failed: %s\n%s\n",
				removeResult.Command,
				formatCommandOutput(removeResult),
			)
		}
	}
	if failed && environment.clusterStarted {
		if err := exportKindLogs(environment); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"export kind diagnostics failed: %v\n", err)
		}
	}
	if keep {
		fmt.Fprintf(
			os.Stderr,
			"keeping integration environment: cluster=%s kubeconfig=%s\n",
			environment.cluster,
			environment.kubeconfig,
		)
		return
	}
	if environment.clusterStarted {
		deleteContext, cancel := context.WithTimeout(context.Background(), clusterReadinessTimeout)
		deleteResult := runCommand(
			deleteContext,
			environment.root,
			environment.kindEnv,
			environment.kindPath,
			"delete", "cluster",
			"--name", environment.cluster,
		)
		cancel()
		if deleteResult.Err != nil {
			fmt.Fprintf(
				os.Stderr,
				"delete kind cluster failed: %s\n%s\n",
				deleteResult.Command,
				formatCommandOutput(deleteResult),
			)
		}
	}
	if err := os.RemoveAll(environment.root); err != nil {
		fmt.Fprintf(os.Stderr, "remove integration temporary directory failed: %v\n", err)
	}
}

// buildBinary compiles the repository's command once into the test directory.
func buildBinary(goPath, root string) (string, error) {
	name := "kube-dump"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(root, name)
	result := runCommand(
		context.Background(),
		repositoryRoot(),
		[]string{"CGO_ENABLED=0", "GOWORK=off"},
		goPath,
		"build", "-trimpath", "-o", binary, "./cmd/kube-dump",
	)
	if result.Err != nil {
		return binary, fmt.Errorf("build kube-dump failed: %s\n%s", result.Command, formatCommandOutput(result))
	}

	return binary, nil
}

// waitForKubernetes verifies that the explicit kubeconfig reaches the new API.
func waitForKubernetes(kubeconfig string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Discovery's legacy ServerVersion method has no context parameter.
	// Bound each request explicitly
	// so a stale or unreachable published port cannot block TestMain cleanup forever.
	config.Timeout = 5 * time.Second

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), clusterReadinessTimeout)
	defer cancel()

	var lastErr error
	for {
		if _, err := client.Discovery().ServerVersion(); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("API did not become ready: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// runCLI executes the built binary with a per-command timeout and explicit kubeconfig.
// No shell is involved, so paths and arguments stay unambiguous.
func (e *integrationEnvironment) runCLI(args ...string) commandResult {
	return e.runCLIWithEnv(nil, args...)
}

// runCLIWithEnv executes the built binary with explicit environment overrides.
func (e *integrationEnvironment) runCLIWithEnv(environment []string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
	defer cancel()

	return runCommand(ctx, e.root, environment, e.binary, args...)
}

// client creates a client-go client from the environment's explicit config.
func (e *integrationEnvironment) client(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	config, err := clientcmd.BuildConfigFromFlags("", e.kubeconfig)
	if err != nil {
		t.Fatalf("load integration kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create integration Kubernetes client: %v", err)
	}

	return client
}

// prepareVolumeImage builds and loads a minimal Linux helper image once per run.
func (e *integrationEnvironment) prepareVolumeImage() (string, error) {
	if e.volumeImage != "" {
		return e.volumeImage, nil
	}
	if configured := os.Getenv("KUBE_DUMP_INTEGRATION_VOLUME_IMAGE"); configured != "" {
		e.volumeImage = configured
		return configured, nil
	}

	goPath, err := lookupExecutable("go")
	if err != nil {
		return "", err
	}
	contextDirectory, err := os.MkdirTemp(e.root, "volume-image-")
	if err != nil {
		return "", fmt.Errorf("create volume image context: %w", err)
	}

	binary := filepath.Join(contextDirectory, "kube-dump")
	buildContext, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
	defer cancel()
	result := runCommand(
		buildContext,
		repositoryRoot(),
		[]string{
			"CGO_ENABLED=0",
			"GOWORK=off",
			"GOOS=linux",
			"GOARCH=amd64",
		},
		goPath,
		"build", "-trimpath", "-o", binary, "./cmd/kube-dump",
	)
	if result.Err != nil {
		return "", fmt.Errorf("build Linux volume helper: %s\n%s", result.Command, formatCommandOutput(result))
	}

	dockerfile := filepath.Join(contextDirectory, "Dockerfile")
	if err := os.WriteFile(
		dockerfile,
		[]byte(`FROM scratch
COPY kube-dump /kube-dump
USER 65532:65532
ENTRYPOINT ["/kube-dump"]`),
		0o600,
	); err != nil {
		return "", fmt.Errorf("write volume helper Dockerfile: %w", err)
	}

	// Avoid the implicit Always pull policy applied to images tagged latest.
	// The image is loaded directly into kind,
	// so a non-latest tag keeps the helper Pod local and makes the test independent of registry access.
	tag := uniqueName("kube-dump-volume") + ":integration"
	result = e.runtime.build(buildContext, contextDirectory, dockerfile, tag)
	if result.Err != nil {
		return "", fmt.Errorf("build volume helper image: %s\n%s", result.Command, formatCommandOutput(result))
	}

	result = runCommand(
		buildContext,
		e.root,
		e.kindEnv,
		e.kindPath,
		"load", "docker-image",
		"--name", e.cluster,
		tag,
	)
	if result.Err != nil {
		return "", fmt.Errorf("load volume helper image into kind: %s\n%s", result.Command, formatCommandOutput(result))
	}

	e.volumeImage = tag
	return tag, nil
}

// runCommand starts one external process without invoking a shell.
func runCommand(ctx context.Context, dir string, extraEnv []string, name string, args ...string) commandResult {
	command := commandLine(name, args...)
	process := exec.CommandContext(ctx, name, args...)
	process.Dir = dir
	process.Env = commandEnvironment(extraEnv)

	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err := process.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("command timeout: %w", ctx.Err())
	}

	return commandResult{
		Command: command,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Err:     err,
	}
}

// start starts one detached service container with a dynamically published port.
func (r containerRuntime) start(
	ctx context.Context,
	name string,
	image string,
	containerPort string,
	environment []string,
	mounts []string,
	command ...string,
) commandResult {
	args := []string{
		"run", "--detach", "--rm", "--name", name,
		"--publish", "127.0.0.1::" + containerPort,
	}
	for _, value := range environment {
		args = append(args, "--env", value)
	}
	for _, mount := range mounts {
		args = append(args, "--volume", mount)
	}
	args = append(args, image)
	args = append(args, command...)

	return runCommand(ctx, "", nil, r.path, args...)
}

// build creates a test-owned container image from an explicit build context.
func (r containerRuntime) build(
	ctx context.Context,
	contextDirectory string,
	dockerfile string,
	tag string,
) commandResult {
	return runCommand(
		ctx,
		contextDirectory,
		nil,
		r.path,
		"build",
		"--tag", tag,
		"--file", dockerfile,
		contextDirectory,
	)
}

// exec runs a command in a test-owned container.
func (r containerRuntime) exec(ctx context.Context, name string, args ...string) commandResult {
	return runCommand(ctx, "", nil, r.path, append([]string{"exec", name}, args...)...)
}

// remove forcibly removes one test-owned service container.
func (r containerRuntime) remove(ctx context.Context, name string) commandResult {
	return runCommand(ctx, "", nil, r.path, "rm", "--force", name)
}

// logs captures one service container's output into the test-owned directory.
func (r containerRuntime) logs(ctx context.Context, name string) commandResult {
	return runCommand(ctx, "", nil, r.path, "logs", name)
}

// mappedPort resolves a dynamically published container port on the host.
func (r containerRuntime) mappedPort(ctx context.Context, name, containerPort string) (int, error) {
	result := runCommand(ctx, "", nil, r.path, "port", name, containerPort)
	if result.Err != nil {
		return 0, fmt.Errorf("resolve mapped port: %s\n%s", result.Command, formatCommandOutput(result))
	}

	value := strings.TrimSpace(result.Stdout)
	if separator := strings.LastIndex(value, "->"); separator >= 0 {
		value = strings.TrimSpace(value[separator+2:])
	}
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return 0, fmt.Errorf("parse mapped port %q: %w", value, err)
	}

	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return 0, fmt.Errorf("invalid mapped port %q", port)
	}

	return number, nil
}

// startS3 starts SeaweedFS lazily and waits for its S3 endpoint to answer.
func (e *integrationEnvironment) startS3() (*s3Service, error) {
	if e.s3 != nil {
		return e.s3, nil
	}

	accessKey := "kube-dump-integration"
	secretKey := "integration-secret"
	secretKeyFile := filepath.Join(e.root, "s3-secret-key")
	if err := os.WriteFile(secretKeyFile, []byte(secretKey+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write S3 test secret file: %w", err)
	}

	image := os.Getenv("KUBE_DUMP_SEAWEEDFS_IMAGE")
	if image == "" {
		image = seaweedfsImage
	}
	service := &s3Service{
		container:     uniqueName("kube-dump-s3"),
		bucket:        uniqueName("kube-dump"),
		accessKey:     accessKey,
		secretKeyFile: secretKeyFile,
	}
	e.s3 = service

	ctx, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
	defer cancel()
	result := e.runtime.start(
		ctx,
		service.container,
		image,
		"8333/tcp",
		[]string{
			"AWS_ACCESS_KEY_ID=" + accessKey,
			"AWS_SECRET_ACCESS_KEY=" + secretKey,
			"S3_BUCKET=" + service.bucket,
		},
		nil,
		"mini", "-dir=/data",
	)
	if result.Err != nil {
		return service, fmt.Errorf("start SeaweedFS: %s\n%s", result.Command, formatCommandOutput(result))
	}
	e.services = append(e.services, serviceContainer{container: service.container})

	port, err := e.runtime.mappedPort(ctx, service.container, "8333/tcp")
	if err != nil {
		return service, err
	}

	service.endpoint = fmt.Sprintf("http://%s:%d", integrationHost(), port)
	podHost := "host.docker.internal"
	if e.runtime.name == "podman" {
		podHost = "host.containers.internal"
	}
	service.podEndpoint = fmt.Sprintf("http://%s:%d", podHost, port)
	if err := waitForHTTP(ctx, service.endpoint); err != nil {
		return service, fmt.Errorf("wait for SeaweedFS S3 endpoint: %w", err)
	}

	return service, nil
}

// startRegistry starts the local registry used by image tests.
func (e *integrationEnvironment) startRegistry() (*registryService, error) {
	return e.startRegistryMode(false)
}

// startAuthenticatedRegistry starts the private registry used by auth tests.
func (e *integrationEnvironment) startAuthenticatedRegistry() (*registryService, error) {
	return e.startRegistryMode(true)
}

// startRegistryMode starts a registry with optional htpasswd authentication.
func (e *integrationEnvironment) startRegistryMode(authenticated bool) (*registryService, error) {
	if authenticated && e.privateRegistry != nil {
		return e.privateRegistry, nil
	}
	if !authenticated && e.registry != nil {
		return e.registry, nil
	}

	image := os.Getenv("KUBE_DUMP_REGISTRY_IMAGE")
	if image == "" {
		image = registryImage
	}
	service := &registryService{
		container: uniqueName("kube-dump-registry"),
	}
	if authenticated {
		service.username = "kube-dump-integration"
		service.password = "integration-password"
	}
	if authenticated {
		e.privateRegistry = service
	} else {
		e.registry = service
	}

	var environment, mounts []string
	if authenticated {
		filename := filepath.Join(e.root, service.container+".htpasswd")
		hash, err := bcrypt.GenerateFromPassword([]byte(service.password), bcrypt.MinCost)
		if err != nil {
			return service, fmt.Errorf("generate registry test password hash: %w", err)
		}
		if err := os.WriteFile(
			filename,
			[]byte(service.username+":"+string(hash)+"\n"),
			0o600,
		); err != nil {
			return service, fmt.Errorf("write registry password file: %w", err)
		}
		mounts = []string{filename + ":/auth/htpasswd:ro"}
		environment = []string{
			"REGISTRY_AUTH=htpasswd",
			"REGISTRY_AUTH_HTPASSWD_REALM=Registry Realm",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
	defer cancel()
	result := e.runtime.start(
		ctx,
		service.container,
		image,
		"5000/tcp",
		environment,
		mounts,
	)
	if result.Err != nil {
		return service, fmt.Errorf("start OCI registry: %s\n%s", result.Command, formatCommandOutput(result))
	}
	e.services = append(e.services, serviceContainer{container: service.container})

	port, err := e.runtime.mappedPort(ctx, service.container, "5000/tcp")
	if err != nil {
		return service, err
	}

	service.endpoint = fmt.Sprintf("http://%s:%d", integrationHost(), port)
	if err := waitForHTTP(ctx, service.endpoint+"/v2/"); err != nil {
		return service, fmt.Errorf("wait for OCI registry endpoint: %w", err)
	}

	service.reference = service.endpoint[len("http://"):] + "/integration/fixture:v1"

	return service, nil
}

// registryServiceForTest starts and returns the package's shared registry fixture.
func (e *integrationEnvironment) registryServiceForTest(t *testing.T) *registryService {
	t.Helper()

	service, err := e.startRegistry()
	if err != nil {
		t.Fatalf("start OCI registry: %v", err)
	}

	return service
}

// privateRegistryServiceForTest starts and returns the authenticated registry.
func (e *integrationEnvironment) privateRegistryServiceForTest(t *testing.T) *registryService {
	t.Helper()

	service, err := e.startAuthenticatedRegistry()
	if err != nil {
		t.Fatalf("start authenticated OCI registry: %v", err)
	}

	return service
}

// writeDockerAuthConfig writes temporary credentials consumed by ORAScope.
func writeDockerAuthConfig(directory string, service *registryService) error {
	return writeDockerAuthConfigForRepository(directory, service, "")
}

// writeDockerAuthConfigForRepository writes credentials scoped to one repository.
func writeDockerAuthConfigForRepository(directory string, service *registryService, repository string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Docker auth directory: %w", err)
	}

	if err := os.WriteFile(
		filepath.Join(directory, "config.json"),
		dockerAuthConfigDataForRepository(service, repository),
		0o600,
	); err != nil {
		return fmt.Errorf("write Docker auth config: %w", err)
	}

	return nil
}

// dockerAuthConfigData returns a Docker-compatible auth document for a service.
func dockerAuthConfigData(service *registryService) []byte {
	return dockerAuthConfigDataForRepository(service, "")
}

// dockerAuthConfigDataForRepository returns scoped Docker auth configuration.
func dockerAuthConfigDataForRepository(service *registryService, repository string) []byte {
	encoded := base64.StdEncoding.EncodeToString(
		[]byte(service.username + ":" + service.password),
	)
	path := service.endpoint[len("http://"):]
	if repository != "" {
		path += "/" + repository
	}
	config := fmt.Sprintf(
		`{"auths":{"%s":{"auth":"%s"}}}`,
		path,
		encoded,
	)

	return []byte(config + "\n")
}

// s3ServiceForTest starts and returns the package's shared S3 fixture.
func (e *integrationEnvironment) s3ServiceForTest(t *testing.T) *s3Service {
	t.Helper()

	service, err := e.startS3()
	if err != nil {
		t.Fatalf("start SeaweedFS: %v", err)
	}

	return service
}

// exportContainerLogs writes disposable service diagnostics without exposing
// command environment values in the test output.
func exportContainerLogs(environment *integrationEnvironment, container string) error {
	ctx, cancel := context.WithTimeout(context.Background(), integrationCommandTimeout)
	defer cancel()
	result := environment.runtime.logs(ctx, container)
	if result.Err != nil {
		return fmt.Errorf("%s\n%s", result.Command, formatCommandOutput(result))
	}

	filename := filepath.Join(environment.root, container+".log")
	if err := os.WriteFile(filename, []byte(formatCommandOutput(result)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write service logs: %w", err)
	}

	fmt.Fprintf(os.Stderr, "service diagnostics saved to %s\n", filename)
	return nil
}

// waitForHTTP polls an endpoint until the service accepts an HTTP connection.
func waitForHTTP(ctx context.Context, endpoint string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if requestErr == nil {
				return nil
			}

			err = requestErr
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", err, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// commandEnvironment applies overrides without leaving conflicting duplicate entries
// inherited from the developer or CI environment.
func commandEnvironment(overrides []string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}

	keys := make(map[string]struct{}, len(overrides))
	for _, override := range overrides {
		key, _, ok := strings.Cut(override, "=")
		if ok {
			keys[key] = struct{}{}
		}
	}

	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := keys[key]; !overridden {
			environment = append(environment, entry)
		}
	}

	return append(environment, overrides...)
}

// exportKindLogs preserves node and control-plane diagnostics before cleanup.
func exportKindLogs(environment *integrationEnvironment) error {
	logDirectory := filepath.Join(environment.root, "kind-logs")
	if err := os.MkdirAll(logDirectory, 0o750); err != nil {
		return fmt.Errorf("create kind diagnostics directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), clusterReadinessTimeout)
	defer cancel()
	result := runCommand(
		ctx,
		environment.root,
		environment.kindEnv,
		environment.kindPath,
		"export", "logs",
		"--name", environment.cluster,
		logDirectory,
	)
	if result.Err != nil {
		return fmt.Errorf("%s\n%s", result.Command, formatCommandOutput(result))
	}

	fmt.Fprintf(os.Stderr, "kind diagnostics saved to %s\n", logDirectory)
	return nil
}

// lookupExecutable resolves one required integration prerequisite.
func lookupExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("integration prerequisite %q is unavailable: %w", name, err)
	}

	return path, nil
}

// selectContainerRuntime selects the explicitly configured container provider.
// Docker is the default so a direct `go test` run has the same contract as Make.
func selectContainerRuntime() (string, string, error) {
	selected := os.Getenv("KUBE_DUMP_INTEGRATION_RUNTIME")
	if selected == "" {
		selected = "docker"
	}
	if selected != "docker" && selected != "podman" {
		return "", "", fmt.Errorf("KUBE_DUMP_INTEGRATION_RUNTIME must be docker or podman, got %q", selected)
	}

	path, err := lookupExecutable(selected)
	if err != nil {
		return "", "", err
	}

	return selected, path, nil
}

// integrationExecutable resolves an executable from an environment override
// or from the supplied default name and reports a setup error when unavailable.
func integrationExecutable(environment, fallback string) (string, error) {
	name := os.Getenv(environment)
	if name == "" {
		name = fallback
	}

	return lookupExecutable(name)
}

// integrationHost returns the loopback address used by local test services.
func integrationHost() string {
	return "127.0.0.1"
}

// repositoryRoot locates the module root from the test source location.
func repositoryRoot() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}

	return filepath.Dir(filepath.Dir(filepath.Dir(source)))
}

// uniqueName creates a DNS-compatible name that is unique enough for one run.
func uniqueName(prefix string) string {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	return strings.ToLower(prefix + "-" + suffix)
}

// commandLine quotes arguments for diagnostics while keeping them readable.
func commandLine(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}

	return strings.Join(parts, " ")
}
