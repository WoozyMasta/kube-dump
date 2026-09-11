// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateReplacesManagedReferencesAndWarnsOnOtherVersions(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	content := strings.Join([]string{
		"https://example.invalid?ref=v1.2.3",
		"ghcr.io/woozymasta/kube-dump:1.2.3",
		"github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v1.2.3",
		"https://github.com/WoozyMasta/kube-dump/releases/download/v1.2.3/kube-dump-linux-amd64",
		"newTag: &version 1.2.3",
		"example image: ghcr.io/acme/api:v9.8.7",
		"",
	}, "\n")
	if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := update(options{root: root, version: "v2.0.0"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(result.files) != 1 || result.files[0].replacements != 5 {
		t.Fatalf("unexpected update result: %+v", result.files)
	}
	if len(result.warnings) != 1 || result.warnings[0].version != "v9.8.7" {
		t.Fatalf("unexpected warnings: %+v", result.warnings)
	}

	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := strings.Join([]string{
		"https://example.invalid?ref=v2.0.0",
		"ghcr.io/woozymasta/kube-dump:2.0.0",
		"github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0",
		"https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0/kube-dump-linux-amd64",
		"newTag: &version 2.0.0",
		"example image: ghcr.io/acme/api:v9.8.7",
		"",
	}, "\n")
	if string(updated) != want {
		t.Fatalf("updated content = %q, want %q", updated, want)
	}
}

func TestUpdatePreservesNewTagWithoutAnchor(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "kustomization.yaml")
	if err := os.WriteFile(file, []byte("newTag: 1.2.3\n"), 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := update(options{root: root, version: "v2.0.0"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(updated) != "newTag: 2.0.0\n" {
		t.Fatalf("updated content = %q", updated)
	}
}

func TestUpdateReplacesPrereleaseReferences(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	content := strings.Join([]string{
		"https://example.invalid?ref=v2.0.0-rc.99",
		"ghcr.io/woozymasta/kube-dump:2.0.0-rc.99",
		"github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0-rc.99",
		"https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.99/kube-dump-linux-amd64",
		"newTag: &version 2.0.0-rc.99",
		"",
	}, "\n")
	if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := update(options{root: root, version: "v2.0.0"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(result.files) != 1 || result.files[0].replacements != 5 {
		t.Fatalf("unexpected update result: %+v", result.files)
	}

	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := strings.Join([]string{
		"https://example.invalid?ref=v2.0.0",
		"ghcr.io/woozymasta/kube-dump:2.0.0",
		"github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0",
		"https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0/kube-dump-linux-amd64",
		"newTag: &version 2.0.0",
		"",
	}, "\n")
	if string(updated) != want {
		t.Fatalf("updated content = %q, want %q", updated, want)
	}
}

func TestRunAcceptsPrereleaseVersion(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	if err := os.WriteFile(file, []byte(strings.Join([]string{
		"?ref=v2.0.0",
		"ghcr.io/woozymasta/kube-dump:2.0.0",
		"ghcr.io/woozymasta/kube-dump:2.0.0-debug",
		"",
	}, "\n")), 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var stdout, stderr strings.Builder
	if err := run([]string{
		"--version", "v2.0.0-rc.1",
		"--root", root,
		"--include", "README.md",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := strings.Join([]string{
		"?ref=v2.0.0-rc.1",
		"ghcr.io/woozymasta/kube-dump:2.0.0-rc.1",
		"ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug",
		"",
	}, "\n")
	if string(updated) != want {
		t.Fatalf("updated content = %q", updated)
	}
}

func TestRunAcceptsVersionWithoutPrefix(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	if err := os.WriteFile(file, []byte("?ref=v1.2.3\nkube-dump:1.2.3\n"), 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var stdout, stderr strings.Builder
	if err := run([]string{
		"--version", "2.0.0",
		"--root", root,
		"--include", "README.md",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(updated) != "?ref=v2.0.0\nkube-dump:2.0.0\n" {
		t.Fatalf("updated content = %q", updated)
	}
}

func TestUpdateCheckDoesNotModifyFiles(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	original := []byte("https://example.invalid?ref=v1.2.3\n")
	if err := os.WriteFile(file, original, 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := update(options{root: root, version: "v2.0.0", check: true})
	if err == nil || !strings.Contains(err.Error(), "stale managed") {
		t.Fatalf("check error = %v", err)
	}
	if len(result.files) != 1 || result.files[0].replacements != 1 {
		t.Fatalf("unexpected check result: %+v", result.files)
	}
	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(updated) != string(original) {
		t.Fatalf("check modified file: %q", updated)
	}
}

func TestUpdateUsesIncludeMasks(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "docs", "guide.md")
	ignored := filepath.Join(root, "README.md")
	for file, content := range map[string]string{
		selected: "?ref=v1.2.3\n",
		ignored:  "?ref=v1.2.3\n",
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}

	if _, err := update(options{
		root:    root,
		version: "v2.0.0",
		include: []string{"docs/*.md"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	selectedContent, err := os.ReadFile(selected)
	if err != nil {
		t.Fatalf("read selected fixture: %v", err)
	}
	ignoredContent, err := os.ReadFile(ignored)
	if err != nil {
		t.Fatalf("read ignored fixture: %v", err)
	}
	if string(selectedContent) != "?ref=v2.0.0\n" {
		t.Fatalf("selected content = %q", selectedContent)
	}
	if string(ignoredContent) != "?ref=v1.2.3\n" {
		t.Fatalf("ignored content = %q", ignoredContent)
	}
}

func TestUpdateSkipsChangelog(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "CHANGELOG.md")
	original := []byte("release v1.2.3\n")
	if err := os.WriteFile(file, original, 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := update(options{root: root, version: "v2.0.0"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(result.files) != 0 || len(result.warnings) != 0 {
		t.Fatalf("unexpected changelog result: %+v", result)
	}
	updated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(updated) != string(original) {
		t.Fatalf("changelog was modified: %q", updated)
	}
}
