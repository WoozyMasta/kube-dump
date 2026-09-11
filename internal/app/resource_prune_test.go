// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/woozymasta/kube-dump

package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/state"
)

func TestReconcileLocalResourceOwnershipPrunesPreviousManifest(t *testing.T) {
	root := resourceRoot(t.TempDir())
	oldIdentity := testResourceIdentity("old")
	currentIdentity := testResourceIdentity("current")

	oldPath := writeResourceOwnershipTestFile(t, root, oldIdentity)
	currentPath := writeResourceOwnershipTestFile(t, root, currentIdentity)
	result := kube.CollectionResult{
		Identities: []state.Identity{oldIdentity, currentIdentity},
	}
	if err := reconcileLocalResourceOwnership(root, "builtin:backup", result, true); err != nil {
		t.Fatalf("write initial ownership manifest: %v", err)
	}

	if err := reconcileLocalResourceOwnership(root, "builtin:export", kube.CollectionResult{
		Identities: []state.Identity{currentIdentity},
	}, true); err != nil {
		t.Fatalf("reconcile ownership manifest: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old stream file still exists, stat error: %v", err)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("current resource file was removed: %v", err)
	}
}

func TestReconcileLocalResourceOwnershipKeepsUnionWithoutPrune(t *testing.T) {
	root := resourceRoot(t.TempDir())
	oldIdentity := testResourceIdentity("old")
	currentIdentity := testResourceIdentity("current")

	writeResourceOwnershipTestFile(t, root, oldIdentity)
	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Identities: []state.Identity{oldIdentity},
	}, false); err != nil {
		t.Fatalf("write initial ownership manifest: %v", err)
	}
	writeResourceOwnershipTestFile(t, root, currentIdentity)
	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Identities: []state.Identity{currentIdentity},
	}, false); err != nil {
		t.Fatalf("write union ownership manifest: %v", err)
	}

	manifest, found, err := readLocalResourceOwnershipManifest(localResourceOwnershipManifestPath(root))
	if err != nil {
		t.Fatalf("read union ownership manifest: %v", err)
	}
	if !found || len(manifest.Paths) != 2 {
		t.Fatalf("ownership paths = %#v, want two paths", manifest.Paths)
	}

	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Identities: []state.Identity{currentIdentity},
	}, true); err != nil {
		t.Fatalf("prune union ownership manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "v1", "deployments", "production", "old.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale union file still exists, stat error: %v", err)
	}
}

func TestReconcileLocalResourceOwnershipFirstPrunePreservesUnknownFiles(t *testing.T) {
	root := resourceRoot(t.TempDir())
	unknown := filepath.Join(root, "apps", "v1", "deployments", "production", "unknown.yaml")
	if err := os.MkdirAll(filepath.Dir(unknown), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("user data\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{}, true); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown canonical-looking file was removed: %v", err)
	}
}

func TestReconcileLocalResourceOwnershipSkipsIncompleteCollection(t *testing.T) {
	root := resourceRoot(t.TempDir())
	identity := testResourceIdentity("old")
	filename := writeResourceOwnershipTestFile(t, root, identity)
	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Identities: []state.Identity{identity},
	}, true); err != nil {
		t.Fatalf("write ownership manifest: %v", err)
	}

	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Warnings: []error{os.ErrPermission},
	}, true); err != nil {
		t.Fatalf("incomplete collection should be skipped: %v", err)
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatalf("stale file was removed after incomplete collection: %v", err)
	}
}

func TestReconcileLocalResourceOwnershipRejectsInvalidManifest(t *testing.T) {
	root := resourceRoot(t.TempDir())
	identity := testResourceIdentity("old")
	filename := writeResourceOwnershipTestFile(t, root, identity)
	manifestPath := localResourceOwnershipManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		t.Fatalf("create manifest directory: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("version: 1\npaths:\n  - ../../outside.yaml\n"), 0o640); err != nil {
		t.Fatalf("write invalid manifest: %v", err)
	}

	if err := reconcileLocalResourceOwnership(root, "builtin:backup", kube.CollectionResult{
		Identities: []state.Identity{identity},
	}, true); err == nil {
		t.Fatal("invalid manifest was accepted")
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatalf("invalid manifest removed resource file: %v", err)
	}
}

func TestReadLocalResourceOwnershipManifestRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}

	root := resourceRoot(t.TempDir())
	manifestPath := localResourceOwnershipManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(target, []byte("version: 1\npaths: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readLocalResourceOwnershipManifest(manifestPath); err == nil {
		t.Fatal("symlinked ownership manifest was accepted")
	}
}

func testResourceIdentity(name string) state.Identity {
	return state.Identity{
		Group:     "apps",
		Version:   "v1",
		Resource:  "deployments",
		Kind:      "Deployment",
		Namespace: "production",
		Name:      name,
	}
}

func writeResourceOwnershipTestFile(t *testing.T, root string, identity state.Identity) string {
	t.Helper()
	relative, err := export.CanonicalObjectPath(identity)
	if err != nil {
		t.Fatalf("build canonical path: %v", err)
	}
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		t.Fatalf("create resource directory: %v", err)
	}
	if err := os.WriteFile(filename, []byte("test\n"), 0o640); err != nil {
		t.Fatalf("write resource file: %v", err)
	}

	return filename
}
