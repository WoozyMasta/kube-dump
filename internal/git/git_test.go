// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package git

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gitlib "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPrepareAndCommitUsesNativeRepository(t *testing.T) {
	path := t.TempDir()
	file := filepath.Join(path, "resources", "core", "v1", "configmaps", "default", "backup.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	options := Options{Commit: true, Branch: "backup", AuthorName: "Test", AuthorEmail: "test@example.invalid"}
	repository, result, err := Prepare(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Initialized || repository == nil {
		t.Fatalf("Prepare() = %#v, repository=%v", result, repository != nil)
	}

	committed, err := Commit(context.Background(), repository, options, "test backup")
	if err != nil {
		t.Fatal(err)
	}
	if !committed.Committed || committed.Hash.IsZero() {
		t.Fatalf("Commit() = %#v", committed)
	}
	if _, err := repository.CommitObject(committed.Hash); err != nil {
		t.Fatalf("read committed object: %v", err)
	}
}

func TestCommitLeavesUnmanagedFilesUnstaged(t *testing.T) {
	path := t.TempDir()
	if err := os.MkdirAll(
		filepath.Join(path, "resources", "core", "v1", "configmaps", "default"),
		0o750,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(
		path, "resources", "core", "v1", "configmaps", "default", "object.yaml"),
		[]byte("kind: ConfigMap\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(path, "notes.txt"),
		[]byte("caller data\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	options := Options{Commit: true}
	repository, _, err := Prepare(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(context.Background(), repository, options, "managed backup"); err != nil {
		t.Fatal(err)
	}

	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	status, err := worktree.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.File("notes.txt").Worktree != gitlib.Untracked {
		t.Fatalf("unmanaged file status = %#v, want untracked", status.File("notes.txt"))
	}
}

func TestManagedPathIncludesResourceOwnershipManifest(t *testing.T) {
	if !isManagedPath("resources/.kube-dump/ownership.yaml") {
		t.Fatal("resource ownership manifest is not managed by Git commits")
	}
	if isManagedPath(".kube-dump/ownership.yaml") {
		t.Fatal("root-level ownership manifest is outside the published resource tree")
	}
	if isManagedPath("core/v1/configmaps/default/caller-owned.yaml") {
		t.Fatal("root-level canonical-looking user path is managed by Git commits")
	}
	if isManagedPath(".kube-dump/resources/0123456789abcdef01234567.yaml") {
		t.Fatal("removed resource stream manifest is still managed by Git commits")
	}
}

func TestCommitRejectsPreStagedUnmanagedFiles(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(path, "notes.txt"),
		[]byte("caller data\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	options := Options{Commit: true}
	repository, _, err := Prepare(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}

	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("notes.txt"); err != nil {
		t.Fatal(err)
	}

	_, err = Commit(context.Background(), repository, options, "must fail")
	if err == nil || !strings.Contains(err.Error(), "unmanaged path") {
		t.Fatalf("Commit() error = %v, want unmanaged-path error", err)
	}
}

func TestCommitDoesNotStageBeforeValidatingMessage(t *testing.T) {
	path := t.TempDir()
	filename := filepath.Join(path, "resources", "core", "v1", "configmaps", "default", "object.yaml")
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	options := Options{Commit: true}
	repository, _, err := Prepare(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Commit(context.Background(), repository, options, ""); err == nil {
		t.Fatal("Commit() succeeded without a message")
	}

	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	status, err := worktree.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.File(filepath.ToSlash(filepath.Join("resources", "core", "v1", "configmaps", "default", "object.yaml"))).Staging != gitlib.Untracked {
		t.Fatalf("managed file was staged after validation failure: %#v", status.File("resources/core/v1/configmaps/default/object.yaml"))
	}
}

func TestPrepareInitializesRepositoryWithoutCommitOptions(t *testing.T) {
	repository, result, err := Prepare(
		context.Background(),
		filepath.Join(t.TempDir(), "backup"),
		Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if repository == nil || result != (Result{Initialized: true}) {
		t.Fatalf("Prepare() = %v, %#v", repository, result)
	}
}

func TestOpenExistingRepository(t *testing.T) {
	path := t.TempDir()
	if _, err := gitlib.PlainInit(path, false); err != nil {
		t.Fatal(err)
	}
	repository, result, err := Prepare(context.Background(), path, Options{Commit: true})
	if err != nil {
		t.Fatal(err)
	}
	if repository == nil || result != (Result{}) {
		t.Fatalf("Prepare() = %v, %#v", repository, result)
	}
}

func TestPrepareClonesIntoAnExistingEmptyDirectory(t *testing.T) {
	remotePath := filepath.Join(t.TempDir(), "remote.git")
	seedPath := filepath.Join(t.TempDir(), "seed")
	seed, err := gitlib.PlainInit(seedPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedPath, "README"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedWorktree, err := seed.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedWorktree.Add("README"); err != nil {
		t.Fatal(err)
	}

	commit, err := seedWorktree.Commit("seed", &gitlib.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if commit.IsZero() {
		t.Fatal("seed commit is empty")
	}
	if _, err := gitlib.PlainClone(remotePath, true, &gitlib.CloneOptions{URL: seedPath}); err != nil {
		t.Fatal(err)
	}

	destination := t.TempDir()
	options := Options{Commit: true, RemoteURL: remotePath, Branch: "master"}
	repository, result, err := Prepare(context.Background(), destination, options)
	if err != nil {
		t.Fatal(err)
	}
	if repository == nil || !result.Cloned {
		t.Fatalf("Prepare() = %#v, repository=%v", result, repository != nil)
	}
	if _, err := os.Stat(filepath.Join(destination, "README")); err != nil {
		t.Fatalf("cloned file is missing: %v", err)
	}
}

func TestPrepareRejectsSymlinkDestination(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "backup")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	_, _, err := Prepare(context.Background(), link, Options{Commit: true})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Prepare() error = %v", err)
	}
}

func TestPushOptions(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    map[string]string
		wantErr string
	}{
		{
			name:   "option without value",
			values: []string{"ci.skip"},
			want:   map[string]string{"ci.skip": ""},
		},
		{
			name:   "value keeps equals signs",
			values: []string{"ci.variable=MAX_RETRIES=10"},
			want:   map[string]string{"ci.variable": "MAX_RETRIES=10"},
		},
		{
			name:    "empty name",
			values:  []string{"=value"},
			wantErr: "must not be empty",
		},
		{
			name:    "repeated name",
			values:  []string{"ci.skip", "ci.skip"},
			wantErr: "is repeated",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := pushOptions(test.values)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("pushOptions() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("pushOptions() = %#v, want %#v", got, test.want)
			}
		})
	}
}
