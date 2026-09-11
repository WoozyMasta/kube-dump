// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/filelock"
	"github.com/woozymasta/kube-dump/v2/internal/state"
)

func TestResourceTreeContainsReportsUnreadableInput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := resourceTreeContainsSIV(root); err == nil {
		t.Fatal("resourceTreeContainsSIV() accepted a missing input root")
	}
}

func TestResourceTreeContainsReportsDecodeFailure(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "apps", "v1", "deployments", "prod", "app.yaml")
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("not yaml: ["), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resourceTreeContainsSIV(root); err == nil {
		t.Fatal("resourceTreeContainsSIV() accepted malformed resource data")
	}
}

func TestResourceKeyringRollbackRemovesNewKeyring(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rollback, err := prepareResourceKeyringRollback(root)
	if err != nil {
		t.Fatal(err)
	}

	cryptoRoot := filepath.Join(root, cryptoLayoutDirectory)
	if err := os.MkdirAll(filepath.Join(cryptoRoot, "keys"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cryptoRoot, "metadata.yaml"), []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cryptoRoot); !os.IsNotExist(err) {
		t.Fatalf("new keyring was not removed, stat error = %v", err)
	}
}

func TestResourceKeyringRollbackPreservesExistingKeyring(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cryptoRoot := filepath.Join(root, cryptoLayoutDirectory)
	if err := os.MkdirAll(cryptoRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cryptoRoot, "metadata.yaml")
	if err := os.WriteFile(marker, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	rollback, err := prepareResourceKeyringRollback(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing keyring was removed: %v", err)
	}
}

func TestResourceWriterLockDoesNotCreateDestination(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "clone-target")
	lockPath, err := resourceWriterLockPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	expectedParent, err := canonicalFilesystemPath(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(lockPath) != expectedParent {
		t.Fatalf("resource writer lock directory = %q, want %q", filepath.Dir(lockPath), expectedParent)
	}

	lock, err := filelock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("acquiring the writer lock created destination, stat error = %v", err)
	}
}

func TestValidateResourceTransformPathsRejectsNestedOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := validateResourceTransformPaths(root, filepath.Join(root, "decrypted")); err == nil {
		t.Fatal("validateResourceTransformPaths() accepted an output below input")
	}
}

func TestResourceWriterLockUsesCanonicalPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}

	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	aliasParent := filepath.Join(root, "alias")
	if err := os.Mkdir(realParent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}

	realLock, err := resourceWriterLockPath(filepath.Join(realParent, "backup"))
	if err != nil {
		t.Fatal(err)
	}
	aliasLock, err := resourceWriterLockPath(filepath.Join(aliasParent, "backup"))
	if err != nil {
		t.Fatal(err)
	}
	if realLock != aliasLock {
		t.Fatalf("lock paths differ for aliases: %q != %q", realLock, aliasLock)
	}
}

func TestResourceTransformPathUsesCanonicalIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	alias := filepath.Join(root, "nested", "..", "backup")
	first, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resourceTransformPath(alias)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("transaction paths differ for aliases: %q != %q", first, second)
	}
	other, err := resourceTransformPath(filepath.Join(root, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if first == other {
		t.Fatalf("sibling destinations share transaction path %q", first)
	}
}

func TestResourceTreeContainsRejectsCanonicalSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "outside.yaml")
	if err := os.WriteFile(target, []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "core", "v1", "configmaps", "default", "linked.yaml")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	if _, err := resourceTreeContainsSIV(root); err == nil {
		t.Fatal("resourceTreeContainsSIV() accepted a canonical symlink")
	}
}

func TestCopyResourceMetadataRejectsSymlink(t *testing.T) {
	t.Parallel()

	input := t.TempDir()
	output := t.TempDir()
	metadata := filepath.Join(input, ".kube-dump")
	if err := os.MkdirAll(metadata, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(input, "outside")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(metadata, "linked")); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	if err := copyResourceMetadata(input, output); err == nil {
		t.Fatal("copyResourceMetadata() accepted a symlink")
	}
}

func TestPublishResourceTreeReplacesPreviousGeneration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	destination := filepath.Join(root, "backup")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := publishResourceTree(staging, destination); err != nil {
		t.Fatalf("publishResourceTree() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "new.txt")); err != nil {
		t.Fatalf("new resource tree was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old resource tree remains, stat error = %v", err)
	}
}

func TestPublishResourceTransformPreservesMixedBackupRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(resourceRoot(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceRoot(staging), 0o750); err != nil {
		t.Fatal(err)
	}

	oldResource := filepath.Join(resourceRoot(destination), "apps", "v1", "deployments", "prod", "old.yaml")
	newResource := filepath.Join(resourceRoot(staging), "apps", "v1", "deployments", "prod", "new.yaml")
	if err := os.MkdirAll(filepath.Dir(oldResource), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(newResource), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldResource, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newResource, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCrypto := filepath.Join(destination, cryptoLayoutDirectory, "metadata.yaml")
	newCrypto := filepath.Join(staging, cryptoLayoutDirectory, "metadata.yaml")
	if err := os.MkdirAll(filepath.Dir(oldCrypto), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(newCrypto), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldCrypto, []byte("old-keyring"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newCrypto, []byte("new-keyring"), 0o600); err != nil {
		t.Fatal(err)
	}

	siblings := map[string]string{
		"images/index.json":    "images",
		"volumes/index.json":   "volumes",
		".git/HEAD":            "git",
		".kube-dump/notes.txt": "metadata",
		"custom.txt":           "custom",
	}
	for path, content := range siblings {
		filename := filepath.Join(destination, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := publishResourceTransform(staging, destination); err != nil {
		t.Fatalf("publishResourceTransform() error = %v", err)
	}
	if _, err := os.Stat(oldResource); !os.IsNotExist(err) {
		t.Fatalf("old resource remains, stat error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(resourceRoot(destination), "apps", "v1", "deployments", "prod", "new.yaml"))
	if err != nil {
		t.Fatalf("read published resource: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("published resource = %q, want new", data)
	}
	data, err = os.ReadFile(filepath.Join(destination, cryptoLayoutDirectory, "metadata.yaml"))
	if err != nil {
		t.Fatalf("read published crypto metadata: %v", err)
	}
	if string(data) != "new-keyring" {
		t.Fatalf("published crypto metadata = %q, want new-keyring", data)
	}
	for path, want := range siblings {
		data, err := os.ReadFile(filepath.Join(destination, path))
		if err != nil {
			t.Fatalf("read preserved sibling %q: %v", path, err)
		}
		if string(data) != want {
			t.Fatalf("preserved sibling %q = %q, want %q", path, data, want)
		}
	}
}

func TestPublishResourceTransformRemovesStaleCrypto(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(resourceRoot(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceRoot(staging), 0o750); err != nil {
		t.Fatal(err)
	}
	cryptoFile := filepath.Join(destination, cryptoLayoutDirectory, "metadata.yaml")
	if err := os.MkdirAll(filepath.Dir(cryptoFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cryptoFile, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := publishResourceTransform(staging, destination); err != nil {
		t.Fatalf("publishResourceTransform() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, cryptoLayoutDirectory)); !os.IsNotExist(err) {
		t.Fatalf("stale crypto metadata remains, stat error = %v", err)
	}
}

func TestPublishResourceTransformPreflightPreservesDestination(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(resourceRoot(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceRoot(staging), 0o750); err != nil {
		t.Fatal(err)
	}
	oldResource := filepath.Join(resourceRoot(destination), "old.yaml")
	if err := os.WriteFile(oldResource, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cryptoPath := filepath.Join(destination, cryptoLayoutDirectory)
	if err := os.MkdirAll(filepath.Dir(cryptoPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cryptoPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := publishResourceTransform(staging, destination); err == nil {
		t.Fatal("publishResourceTransform() accepted a file crypto path")
	}
	data, err := os.ReadFile(oldResource)
	if err != nil {
		t.Fatalf("read unchanged resource: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("resource changed after preflight failure: %q", data)
	}
	data, err = os.ReadFile(cryptoPath)
	if err != nil {
		t.Fatalf("read unchanged crypto path: %v", err)
	}
	if string(data) != "not a directory" {
		t.Fatalf("crypto path changed after preflight failure: %q", data)
	}
}

func TestPublishResourceTransformRejectsSymlinkedCryptoParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(resourceRoot(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceRoot(staging), 0o750); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.Mkdir(external, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(destination, filepath.Dir(cryptoLayoutDirectory))); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	if err := publishResourceTransform(staging, destination); err == nil {
		t.Fatal("publishResourceTransform() accepted a symlinked crypto parent")
	}
	if _, err := os.Stat(filepath.Join(external, filepath.Base(cryptoLayoutDirectory))); !os.IsNotExist(err) {
		t.Fatalf("publication followed crypto parent symlink, stat error = %v", err)
	}
}

func TestRecoverResourceTransformPreparedRestoresPair(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	resources := resourceRoot(destination)
	crypto := filepath.Join(destination, cryptoLayoutDirectory)
	if err := os.MkdirAll(resources, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(crypto, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "old.yaml"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crypto, "old.key"), []byte("old-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	transaction := &resourceTransformTransaction{
		path:        transactionPath,
		destination: destination,
		state: resourceTransformState{
			Version:      resourceTransformStateVersion,
			Phase:        resourceTransformPrepared,
			OldResources: true,
			OldCrypto:    true,
		},
	}
	if err := transaction.writeState(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(resources, filepath.Join(transactionPath, resourceTransformOldResources)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(crypto, filepath.Join(transactionPath, resourceTransformOldCrypto)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(transactionPath, resourceTransformNewResources), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(transactionPath, resourceTransformNewCrypto), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourceWriterState(destination); err != nil {
		t.Fatalf("recoverResourceWriterState() error = %v", err)
	}
	if got := readTestResourceFile(t, filepath.Join(resources, "old.yaml")); got != "old" {
		t.Fatalf("restored resource = %q, want old", got)
	}
	if got := readTestResourceFile(t, filepath.Join(crypto, "old.key")); got != "old-key" {
		t.Fatalf("restored crypto = %q, want old-key", got)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("prepared transaction remains, stat error = %v", err)
	}
}

func TestRecoverResourceTransformRejectsMalformedStateWithoutMutation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	resources := resourceRoot(destination)
	if err := os.MkdirAll(resources, 0o750); err != nil {
		t.Fatal(err)
	}
	resource := filepath.Join(resources, "old.yaml")
	if err := os.WriteFile(resource, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transactionPath, resourceTransformStateName), []byte(`{"version":999,"phase":"prepared","oldResources":true,"oldCrypto":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourceWriterState(destination); err == nil {
		t.Fatal("recoverResourceWriterState() accepted an unknown transaction version")
	}
	if got := readTestResourceFile(t, resource); got != "old" {
		t.Fatalf("resource changed after malformed state: %q", got)
	}
	if _, err := os.Stat(transactionPath); err != nil {
		t.Fatalf("malformed transaction was removed: %v", err)
	}
}

func TestRecoverResourceTransformRejectsOversizedState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	state := strings.Repeat("x", resourceTransformStateMaxBytes+1)
	if err := os.WriteFile(filepath.Join(transactionPath, resourceTransformStateName), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourceWriterState(destination); err == nil {
		t.Fatal("recoverResourceWriterState() accepted oversized state")
	}
	if _, err := os.Stat(transactionPath); err != nil {
		t.Fatalf("oversized transaction was removed: %v", err)
	}
}

func TestRecoverResourceTransformRejectsMissingStateWithData(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(transactionPath, resourceTransformOldResources)
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "old.yaml"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourceWriterState(destination); err == nil {
		t.Fatal("recoverResourceWriterState() accepted missing state")
	}
	if _, err := os.Stat(filepath.Join(dataPath, "old.yaml")); err != nil {
		t.Fatalf("transaction data was removed: %v", err)
	}
}

func TestRecoverResourceTransformRejectsSymlinkedState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "state-target")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(transactionPath, resourceTransformStateName)); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	if err := recoverResourceWriterState(destination); err == nil {
		t.Fatal("recoverResourceWriterState() accepted symlinked state")
	}
	if _, err := os.Stat(transactionPath); err != nil {
		t.Fatalf("symlinked transaction was removed: %v", err)
	}
}

func TestRecoverResourceTransformCommittedKeepsNewPair(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	resources := resourceRoot(destination)
	crypto := filepath.Join(destination, cryptoLayoutDirectory)
	if err := os.MkdirAll(resources, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(crypto, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "new.yaml"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crypto, "new.key"), []byte("new-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(transactionPath, resourceTransformOldResources), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(transactionPath, resourceTransformOldCrypto), 0o750); err != nil {
		t.Fatal(err)
	}
	transaction := &resourceTransformTransaction{
		path:        transactionPath,
		destination: destination,
		state: resourceTransformState{
			Version:      resourceTransformStateVersion,
			Phase:        resourceTransformCommitted,
			OldResources: true,
			OldCrypto:    true,
		},
	}
	if err := transaction.writeState(); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourceWriterState(destination); err != nil {
		t.Fatalf("recoverResourceWriterState() error = %v", err)
	}
	if got := readTestResourceFile(t, filepath.Join(resources, "new.yaml")); got != "new" {
		t.Fatalf("new resource changed = %q, want new", got)
	}
	if got := readTestResourceFile(t, filepath.Join(crypto, "new.key")); got != "new-key" {
		t.Fatalf("new crypto changed = %q, want new-key", got)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("committed transaction remains, stat error = %v", err)
	}
}

func TestPublishResourceTransformRollsBackWhenCommitFails(t *testing.T) {
	t.Parallel()

	destination, staging := newResourceTransformFixture(t, true)
	operations := defaultResourceTransformFS()
	stateWrites := 0
	operations.writeAtomic = func(path string, write func(io.Writer) error) error {
		stateWrites++
		if stateWrites == 2 {
			return errors.New("injected commit failure")
		}
		return writeAtomic(path, write)
	}

	if err := publishResourceTransformWithFS(staging, destination, operations); err == nil {
		t.Fatal("publishResourceTransformWithFS() succeeded after injected commit failure")
	}
	if got := readTestResourceFile(t, filepath.Join(resourceRoot(destination), "old.yaml")); got != "old" {
		t.Fatalf("rolled back resource = %q, want old", got)
	}
	if got := readTestResourceFile(t, filepath.Join(destination, cryptoLayoutDirectory, "old.key")); got != "old-key" {
		t.Fatalf("rolled back crypto = %q, want old-key", got)
	}
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("prepared transaction remains after rollback, stat error = %v", err)
	}
}

func TestPublishResourceTransformRollsBackWhenResourcesInstallFails(t *testing.T) {
	t.Parallel()

	destination, staging := newResourceTransformFixture(t, true)
	operations := defaultResourceTransformFS()
	operations.rename = func(source, target string) error {
		if filepath.Base(source) == resourceTransformNewResources &&
			filepath.Clean(target) == filepath.Clean(resourceRoot(destination)) {
			return errors.New("injected resource install failure")
		}
		return os.Rename(source, target)
	}

	if err := publishResourceTransformWithFS(staging, destination, operations); err == nil {
		t.Fatal("publishResourceTransformWithFS() succeeded after injected install failure")
	}
	if got := readTestResourceFile(t, filepath.Join(resourceRoot(destination), "old.yaml")); got != "old" {
		t.Fatalf("rolled back resource = %q, want old", got)
	}
	if got := readTestResourceFile(t, filepath.Join(destination, cryptoLayoutDirectory, "old.key")); got != "old-key" {
		t.Fatalf("rolled back crypto = %q, want old-key", got)
	}
}

func newResourceTransformFixture(t *testing.T, withStagingCrypto bool) (string, string) {
	t.Helper()

	root := t.TempDir()
	destination := filepath.Join(root, "backup")
	staging := filepath.Join(root, "staging")
	oldResource := filepath.Join(resourceRoot(destination), "old.yaml")
	newResource := filepath.Join(resourceRoot(staging), "new.yaml")
	oldCrypto := filepath.Join(destination, cryptoLayoutDirectory, "old.key")
	if err := os.MkdirAll(filepath.Dir(oldResource), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(newResource), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldCrypto), 0o750); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		oldResource: "old",
		newResource: "new",
		oldCrypto:   "old-key",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withStagingCrypto {
		path := filepath.Join(staging, cryptoLayoutDirectory, "new.key")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("new-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return destination, staging
}

func readTestResourceFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTransformResourceDirectoryPreservesOwnershipManifest(t *testing.T) {
	t.Parallel()

	input := t.TempDir()
	output := t.TempDir()
	manifest := resourceOwnershipManifest{
		Version: 1,
		Profile: "builtin:backup",
		Paths:   []string{"apps/v1/deployments/production/app.yaml"},
	}
	manifestPath := localResourceOwnershipManifestPath(input)
	if err := writeLocalResourceOwnershipManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	if err := transformResourceDirectory(
		context.Background(), input, output, nil,
		func(object state.Object) (state.Object, error) { return object, nil },
	); err != nil {
		t.Fatalf("transformResourceDirectory() error = %v", err)
	}

	got, found, err := readLocalResourceOwnershipManifest(localResourceOwnershipManifestPath(output))
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.Profile != manifest.Profile || len(got.Paths) != 1 || got.Paths[0] != manifest.Paths[0] {
		t.Fatalf("ownership manifest = %#v, want %#v", got, manifest)
	}
}

func TestRecoverResourcePublicationRestoresOrphanedBackup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "resources")
	prefix, err := resourcePublicationBackupPrefix(destination)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, prefix+"crash")
	if err := os.MkdirAll(backup, 0o750); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(backup, "old.yaml")
	if err := os.WriteFile(want, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourcePublication(destination); err != nil {
		t.Fatalf("recoverResourcePublication() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "old.yaml"))
	if err != nil {
		t.Fatalf("read recovered resource tree: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("recovered resource data = %q, want old", data)
	}
}

func TestRecoverResourcePublicationRejectsAmbiguousBackups(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "resources")
	prefix, err := resourcePublicationBackupPrefix(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		prefix + "one",
		prefix + "two",
	} {
		if err := os.Mkdir(filepath.Join(root, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if err := recoverResourcePublication(destination); err == nil {
		t.Fatal("recoverResourcePublication() accepted ambiguous backups")
	}
}

func TestRecoverResourcePublicationIgnoresSiblingBackups(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	destination := filepath.Join(root, "resources")
	sibling := filepath.Join(root, "other-resources")
	prefix, err := resourcePublicationBackupPrefix(sibling)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, prefix+"crash")
	if err := os.MkdirAll(backup, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := recoverResourcePublication(destination); err != nil {
		t.Fatalf("recoverResourcePublication() returned error for sibling backup: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("sibling backup was changed: %v", err)
	}
}
