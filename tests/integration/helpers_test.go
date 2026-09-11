//go:build integration

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package integration_test

import (
	"io"
	"os"
	"strings"
	"testing"
)

// s3Args returns the S3 URI and named storage options used by the integration fixtures.
// Optional connection settings must remain named flags.
func s3Args(uri string, service *s3Service) []string {
	return append([]string{"--s3-uri", uri}, s3StorageArgs(service)...)
}

// mustRunCLI executes a CLI operation and fails the test
// with both process streams when the command returns an error.
func (e *integrationEnvironment) mustRunCLI(
	t *testing.T,
	operation string,
	args ...string,
) commandResult {
	t.Helper()

	result := e.runCLI(args...)
	if result.Err != nil {
		fatalCommand(t, operation, result)
	}

	return result
}

// mustFailCLI executes a CLI operation and fails the test if it succeeds.
func (e *integrationEnvironment) mustFailCLI(
	t *testing.T,
	operation string,
	args ...string,
) commandResult {
	t.Helper()

	result := e.runCLI(args...)
	if result.Err == nil {
		t.Fatalf("%s unexpectedly succeeded", operation)
	}

	return result
}

// mustRunKubeCLI executes a CLI operation with the common Kubernetes test options.
// Keeping these flags here makes individual scenarios
// show only the options relevant to the behavior under test.
func (e *integrationEnvironment) mustRunKubeCLI(
	t *testing.T,
	operation string,
	args ...string,
) commandResult {
	t.Helper()

	return e.mustRunCLI(t, operation, appendKubeCLIArgs(e.kubeconfig, args...)...)
}

// mustRunKubeCLIWithEnv is the environment-aware variant used by auth tests.
func (e *integrationEnvironment) mustRunKubeCLIWithEnv(
	t *testing.T,
	operation string,
	environment []string,
	args ...string,
) commandResult {
	t.Helper()

	result := e.runCLIWithEnv(environment, appendKubeCLIArgs(e.kubeconfig, args...)...)
	if result.Err != nil {
		fatalCommand(t, operation, result)
	}

	return result
}

// mustFailKubeCLIWithEnv executes an expected-to-fail Kubernetes CLI operation
// with explicit environment overrides.
func (e *integrationEnvironment) mustFailKubeCLIWithEnv(
	t *testing.T,
	operation string,
	environment []string,
	args ...string,
) commandResult {
	t.Helper()

	result := e.runCLIWithEnv(environment, appendKubeCLIArgs(e.kubeconfig, args...)...)
	if result.Err == nil {
		t.Fatalf("%s unexpectedly succeeded", operation)
	}

	return result
}

// appendKubeCLIArgs adds stable non-behavioral flags shared by Kubernetes CLI scenarios
// while preserving each test's operation-specific arguments.
func appendKubeCLIArgs(kubeconfig string, args ...string) []string {
	result := make([]string, 0, len(args)+6)
	result = append(result, args...)
	result = append(result,
		"--kubeconfig", kubeconfig,
		"--no-progress",
		"--log-level", "warn",
	)

	return result
}

// s3StorageArgs returns the credentials and endpoint flags for a test S3 fixture.
// The secret is passed through its temporary file, never as a flag.
func s3StorageArgs(service *s3Service) []string {
	return []string{
		"--s3-endpoint", service.endpoint,
		"--s3-insecure",
		"--s3-region", "us-east-1",
		"--s3-access-key", service.accessKey,
		"--s3-secret-key-file", service.secretKeyFile,
	}
}

// readTestFile reads one expected artifact and reports a focused test error.
func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test artifact %q: %v", path, err)
	}

	return string(data)
}

// requireContains checks that output contains every expected fragment.
func requireContains(t *testing.T, output string, expected ...string) {
	t.Helper()

	for _, fragment := range expected {
		if !strings.Contains(output, fragment) {
			t.Fatalf("output does not contain %q:\n%s", fragment, output)
		}
	}
}

// fatalCommand reports both process streams without exposing environment data.
func fatalCommand(t *testing.T, operation string, result commandResult) {
	t.Helper()
	t.Fatalf("%s failed: %s\n%s", operation, result.Command, formatCommandOutput(result))
}

// formatCommandOutput renders captured streams for a concise failure report.
func formatCommandOutput(result commandResult) string {
	var output strings.Builder
	if result.Stdout != "" {
		_, _ = io.WriteString(&output, "stdout:\n")
		_, _ = io.WriteString(&output, result.Stdout)
	}

	if result.Stderr != "" {
		if output.Len() > 0 {
			_, _ = io.WriteString(&output, "\n")
		}
		_, _ = io.WriteString(&output, "stderr:\n")
		_, _ = io.WriteString(&output, result.Stderr)
	}

	if output.Len() == 0 {
		return "no process output"
	}

	return strings.TrimRight(output.String(), "\r\n")
}
