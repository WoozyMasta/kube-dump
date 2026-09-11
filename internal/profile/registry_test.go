// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryManagesGlobalProfiles(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	path, err := registry.Copy("backup", "custom", false)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("CopyBuiltin() returned an empty path")
	}

	data, err := registry.Resolve("custom")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: custom") {
		t.Fatalf("resolved copied profile = %q", data)
	}

	which, err := registry.Which("custom")
	if err != nil || which != path {
		t.Fatalf("Which(custom) = %q, %v; want %q", which, err, path)
	}

	if err := registry.Remove("custom"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRejectsAmbiguousUnqualifiedNames(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Copy("backup", "backup", false); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.Resolve("backup"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Resolve(backup) error = %v, want ambiguity", err)
	}
	if _, err := registry.Resolve("builtin:backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("global:backup"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryInstallUsesMetadataNameAndRejectsOverwrite(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.yml")
	data := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: installed
  description: Installed profile
resources:
  selection: {}
  rules: []
`)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(filepath.Join(root, "profiles"))
	if err != nil {
		t.Fatal(err)
	}

	path, err := registry.Install(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "installed.yaml" {
		t.Fatalf("installed path = %q", path)
	}
	if _, err := registry.Install(source, false); err == nil {
		t.Fatal("Install() allowed overwrite without force")
	}
	if _, err := registry.Install(source, true); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryInstallDetectsYMLAlias(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profiles, 0o750); err != nil {
		t.Fatal(err)
	}

	data := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: same
  description: Same profile
resources:
  selection: {}
  rules: []
`)
	if err := os.WriteFile(filepath.Join(profiles, "same.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(root, "source.yaml")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := NewRegistry(profiles)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := registry.Install(source, false); err == nil {
		t.Fatal("Install() allowed duplicate alias with .yml extension")
	}

	path, err := registry.Install(source, true)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(profiles, "same.yml") {
		t.Fatalf("forced install path = %q, want existing .yml path", path)
	}
	if _, err := os.Stat(filepath.Join(profiles, "same.yaml")); !os.IsNotExist(err) {
		t.Fatalf("Install() created duplicate .yaml path, stat error = %v", err)
	}
}

func TestLoadFromUsesExplicitGlobalDirectory(t *testing.T) {
	directory := t.TempDir()
	registry, err := NewRegistry(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Copy("builtin:backup", "custom", false); err != nil {
		t.Fatal(err)
	}

	compiled, err := LoadFrom("custom", directory)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Name() != "custom" {
		t.Fatalf("LoadFrom(custom).Name() = %q, want custom", compiled.Name())
	}
}

func TestRegistryListsYMLProfilesAndSkipsUnsupportedEntries(t *testing.T) {
	directory := t.TempDir()
	profile := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: metadata-name
  description: Global YAML profile
resources:
  selection: {}
  rules: []
`)
	if err := os.WriteFile(filepath.Join(directory, "custom.yml"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ignored.txt"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "nested.yaml"), 0o750); err != nil {
		t.Fatal(err)
	}

	registry, err := NewRegistry(directory)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}

	var found *CatalogEntry
	for index := range entries {
		if entries[index].Name == "metadata-name" {
			found = &entries[index]
			break
		}
	}

	if found == nil {
		t.Fatal("List() did not include the .yml profile")
	}
	if found.Source != GlobalSource || found.Description != "Global YAML profile" {
		t.Fatalf("List() entry = %#v", *found)
	}
	if _, err := registry.Resolve("global:metadata-name"); err != nil {
		t.Fatalf("Resolve(global:metadata-name) error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name == "ignored" || entry.Name == "nested" {
			t.Fatalf("List() included unsupported entry %#v", entry)
		}
	}
}

func TestRegistryListIgnoresMissingDirectory(t *testing.T) {
	registry, err := NewRegistry(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := registry.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List() returned %d entries, want three built-ins", len(entries))
	}
}

func TestRegistryRejectsAmbiguousExtensionsAndTraversal(t *testing.T) {
	directory := t.TempDir()
	profile := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: same
  description: Test profile
resources:
  selection: {}
  rules: []
`)
	for _, extension := range []string{".yaml", ".yml"} {
		if err := os.WriteFile(filepath.Join(directory, "same"+extension), profile, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	registry, err := NewRegistry(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("global:same"); err == nil || !strings.Contains(err.Error(), "multiple files") {
		t.Fatalf("Resolve(global:same) error = %v", err)
	}
	if _, err := registry.Copy("backup", "../escape", false); err == nil || !strings.Contains(err.Error(), "invalid global profile name") {
		t.Fatalf("Copy() traversal error = %v", err)
	}
	if err := registry.Remove("../escape"); err == nil || !strings.Contains(err.Error(), "invalid global profile name") {
		t.Fatalf("Remove() traversal error = %v", err)
	}
}
