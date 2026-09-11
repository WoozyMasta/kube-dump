// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package git provides the optional Git persistence used by backup commands.
// All repository operations are implemented with go-git
// and do not invoke an external Git executable.
package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gitlib "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/state"
)

const (
	defaultRemoteName = "origin"
	defaultAuthorName = "kube-dump"
	defaultAuthorMail = "kube-dump@localhost"
)

// Options controls optional repository initialization, commit, and push.
// SSH and HTTP credentials are supplied through files
// or explicit values so the package never depends on a system Git credential helper.
type Options struct {
	RemoteURL        string   `long:"remote-url"     description:"Clone or configure this Git remote URL"`
	RemoteName       string   `long:"remote-name"    description:"Remote name used for clone and push" default:"origin"`
	Branch           string   `long:"branch"         description:"Branch to use for backup commits and pushes" short:"b"`
	AuthorName       string   `long:"author-name"    description:"Commit author name; defaults to kube-dump"`
	AuthorEmail      string   `long:"author-email"   description:"Commit author email; defaults to kube-dump@localhost"`
	Username         string   `long:"username"       description:"HTTP Basic Auth username for the Git remote"`
	PasswordFile     string   `long:"password-file"  description:"Read the Git HTTP password from this file" auto-env:"false"`
	SSHKeyFile       string   `long:"ssh-key"        description:"SSH private key file for Git authentication" auto-env:"false"`
	SSHKeyPassphrase string   `long:"ssh-passphrase" description:"Passphrase for the configured SSH private key" auto-env:"false"`
	KnownHostsFile   string   `long:"known-hosts"    description:"known_hosts file used to verify the SSH Git host" auto-env:"false"`
	PushOptions      []string `long:"push-option"    description:"Push option forwarded to the Git remote as NAME or NAME=VALUE; repeatable for distinct names" env-delim:","`
	Commit           bool     `long:"commit"         description:"Create a commit containing managed backup changes" auto-env:"false" short:"c"`
	Push             bool     `long:"push"           description:"Push the managed backup commit to the configured remote" auto-env:"false" short:"P"`
}

// Result reports repository lifecycle actions performed by Prepare or Commit.
type Result struct {
	Initialized bool          // Initialized reports that a new local repository was created.
	Cloned      bool          // Cloned reports that the repository was obtained from a remote.
	Committed   bool          // Committed reports that a commit was created.
	Pushed      bool          // Pushed reports that the commit was sent to the configured remote.
	Hash        plumbing.Hash // Hash is the resulting commit hash when a commit was created.
}

// Prepare opens, clones, or initializes the repository at path when Git persistence is enabled.
// It must run before the backup creates files.
func Prepare(ctx context.Context, path string, opts Options) (*gitlib.Repository, Result, error) {
	if err := opts.validate(path); err != nil {
		return nil, Result{}, err
	}

	log.Logger.Debug().
		Str("component", "git").
		Str("path", path).
		Bool("commit", opts.Commit).
		Bool("push", opts.Push).
		Msg("preparing Git persistence")

	if ctx == nil {
		return nil, Result{}, errors.New("git context is required")
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolve Git directory: %w", err)
	}

	present, err := validateRepositoryPath(path)
	if err != nil {
		return nil, Result{}, err
	}

	// An empty target can be cloned when a remote is configured;
	// otherwise it becomes the location for a new repository initialized below.
	if !present {
		if opts.RemoteURL != "" {
			return cloneRepository(ctx, path, opts)
		}
		if err := os.MkdirAll(path, 0o750); err != nil {
			return nil, Result{}, fmt.Errorf("create Git directory: %w", err)
		}
	}

	// Handle an existing empty directory as a clone target before PlainOpen,
	// which cannot distinguish it from a path that needs initialization.
	if opts.RemoteURL != "" && directoryEmpty(path) {
		return cloneRepository(ctx, path, opts)
	}

	repository, err := gitlib.PlainOpen(path)
	if err != nil {
		if !errors.Is(err, gitlib.ErrRepositoryNotExists) {
			return nil, Result{}, fmt.Errorf("open Git repository: %w", err)
		}
		if opts.RemoteURL != "" && !directoryEmpty(path) {
			return nil, Result{}, errors.New("cannot clone Git repository into a non-empty directory")
		}

		repository, err = gitlib.PlainInit(path, false)
		if err != nil {
			return nil, Result{}, fmt.Errorf("initialize Git repository: %w", err)
		}
		if err := configureRemote(repository, opts); err != nil {
			return nil, Result{}, err
		}
		if err := ensureBranch(repository, opts.Branch); err != nil {
			return nil, Result{}, err
		}

		return repository, Result{Initialized: true}, nil
	}

	if err := configureRemote(repository, opts); err != nil {
		return nil, Result{}, err
	}
	if err := ensureBranch(repository, opts.Branch); err != nil {
		return nil, Result{}, err
	}

	return repository, Result{}, nil
}

// Commit stages only kube-dump's managed trees,
// creates one commit when their content changed,
// and optionally pushes the selected branch to the configured remote.
// Unstaged files outside those trees are deliberately left alone.
func Commit(ctx context.Context, repository *gitlib.Repository, opts Options, message string) (Result, error) {
	if repository == nil {
		return Result{}, errors.New("git repository is required")
	}
	if ctx == nil {
		return Result{}, errors.New("git context is required")
	}
	if !opts.Commit && !opts.Push {
		return Result{}, nil
	}
	if opts.Commit && message == "" {
		return Result{}, errors.New("git commit message is required")
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return Result{}, fmt.Errorf("open Git worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return Result{}, fmt.Errorf("read Git status: %w", err)
	}

	log.Logger.Debug().
		Str("component", "git").
		Int("status_entries", len(status)).
		Msg("checking managed Git changes")

	if status.IsClean() {
		// Pushing a clean repository is still meaningful;
		// committing a clean repository is not.
		if opts.Push {
			return push(ctx, repository, opts, plumbing.ZeroHash)
		}

		return Result{}, nil
	}

	if path := stagedUnmanagedPath(status); path != "" {
		return Result{}, fmt.Errorf("git index contains unmanaged path %q", path)
	}

	// Only kube-dump-owned trees may be staged.
	// User files in the same working tree must remain outside the generated backup commit.
	managedPaths := changedManagedPaths(status)
	if len(managedPaths) == 0 {
		// The worktree may be dirty only because the caller has unrelated files.
		// Do not create an empty backup commit or alter those files' status.
		if opts.Push {
			return push(ctx, repository, opts, plumbing.ZeroHash)
		}

		return Result{}, nil
	}

	log.Logger.Debug().
		Str("component", "git").
		Int("paths", len(managedPaths)).
		Msg("staging managed Git changes")

	originalIndex, err := repository.Storer.Index()
	if err != nil {
		return Result{}, fmt.Errorf("snapshot Git index: %w", err)
	}

	indexSnapshot := cloneIndex(originalIndex)
	restoreIndex := func(operationErr error) error {
		if err := repository.Storer.SetIndex(indexSnapshot); err != nil {
			return errors.Join(operationErr, fmt.Errorf("restore Git index: %w", err))
		}

		return operationErr
	}

	for _, path := range managedPaths {
		if err := worktree.AddWithOptions(&gitlib.AddOptions{Path: path}); err != nil {
			return Result{}, restoreIndex(fmt.Errorf("stage managed Git path %q: %w", path, err))
		}
	}

	commitHash, err := worktree.Commit(message, &gitlib.CommitOptions{Author: author(opts)})
	if err != nil {
		return Result{}, restoreIndex(fmt.Errorf("create Git commit: %w", err))
	}

	result := Result{Committed: true, Hash: commitHash}

	log.Logger.Debug().
		Str("component", "git").
		Str("commit", commitHash.String()).
		Msg("Git commit created")

	if opts.Push {
		pushed, err := push(ctx, repository, opts, commitHash)
		if err != nil {
			return result, err
		}

		result.Pushed = pushed.Pushed
	}

	return result, nil
}

// cloneIndex creates an independent snapshot of the Git index before staging.
// go-git mutates the index returned by the storer,
// so retaining only the pointer would not restore the caller's state after a failed commit.
func cloneIndex(source *index.Index) *index.Index {
	if source == nil {
		return &index.Index{Version: 2}
	}

	snapshot := *source
	snapshot.Entries = make([]*index.Entry, len(source.Entries))
	for position, entry := range source.Entries {
		if entry == nil {
			continue
		}

		entrySnapshot := *entry
		snapshot.Entries[position] = &entrySnapshot
	}

	return &snapshot
}

// changedManagedPaths returns changed canonical resource files.
// Sorting makes staging and diagnostics deterministic.
func changedManagedPaths(status gitlib.Status) []string {
	paths := make([]string, 0, len(status))
	for path, fileStatus := range status {
		if !isManagedPath(path) || fileStatus.Staging == gitlib.Unmodified && fileStatus.Worktree == gitlib.Unmodified {
			continue
		}

		paths = append(paths, path)
	}

	sort.Strings(paths)
	return paths
}

// stagedUnmanagedPath returns the first already-staged path outside the canonical resource layout.
// Refusing such a state preserves caller-owned index data
// instead of silently bundling it into a kube-dump commit.
func stagedUnmanagedPath(status gitlib.Status) string {
	paths := make([]string, 0, len(status))
	for path, fileStatus := range status {
		if !isManagedPath(path) && isStaged(fileStatus.Staging) {
			paths = append(paths, path)
		}
	}

	sort.Strings(paths)
	if len(paths) == 0 {
		return ""
	}

	return paths[0]
}

// isStaged distinguishes an actual index change from an untracked worktree entry,
// which go-git reports through the staging column as '?' as well.
func isStaged(status gitlib.StatusCode) bool {
	return status != gitlib.Unmodified && status != gitlib.Untracked
}

// isManagedPath checks whether path belongs to the canonical resource layout
// or contains the AES-SIV keyring required to restore encrypted fields.
func isManagedPath(path string) bool {
	path = strings.ReplaceAll(path, "\\", "/")
	if trimmed, ok := strings.CutPrefix(path, "resources/"); ok {
		return state.IsCanonicalObjectPath(trimmed) ||
			trimmed == ".kube-dump/ownership.yaml"
	}

	return strings.HasPrefix(path, ".kube-dump/crypto/")
}

// validate checks repository paths, remote requirements,
// and credential combinations before any filesystem or network operation begins.
func (o Options) validate(path string) error {
	if path == "" {
		return errors.New("git directory is required")
	}
	if !o.Commit && !o.Push {
		return nil
	}

	if o.Push && o.RemoteURL == "" && o.RemoteName == "" {
		return errors.New("git push requires a remote URL or remote name")
	}
	if o.Push {
		if _, err := pushOptions(o.PushOptions); err != nil {
			return err
		}
	}
	if o.PasswordFile != "" && o.Username == "" {
		return errors.New("git username is required with a password file")
	}
	if strings.ContainsAny(o.RemoteName, "/\\") {
		return fmt.Errorf("git remote name contains a path separator: %q", o.RemoteName)
	}
	if strings.ContainsAny(o.Branch, "\x00") || strings.HasPrefix(o.Branch, "-") {
		return fmt.Errorf("git branch name is invalid: %q", o.Branch)
	}

	return nil
}

// cloneOptions converts application options into a go-git clone request.
// Branch selection is restricted to one branch when explicitly requested.
func cloneOptions(opts Options) (*gitlib.CloneOptions, error) {
	authMethod, err := auth(opts)
	if err != nil {
		return nil, err
	}

	clone := &gitlib.CloneOptions{URL: opts.RemoteURL, Auth: authMethod}
	if opts.Branch != "" {
		clone.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
		clone.SingleBranch = true
	}

	return clone, nil
}

// cloneRepository clones a remote into an already validated destination
// and then aligns the worktree with the requested branch.
func cloneRepository(ctx context.Context, path string, opts Options) (*gitlib.Repository, Result, error) {
	clone, err := cloneOptions(opts)
	if err != nil {
		return nil, Result{}, err
	}

	repository, err := gitlib.PlainCloneContext(ctx, path, false, clone)
	if err != nil {
		return nil, Result{}, fmt.Errorf("clone Git repository: %w", err)
	}
	if err := ensureBranch(repository, opts.Branch); err != nil {
		return nil, Result{}, err
	}

	return repository, Result{Cloned: true}, nil
}

// configureRemote creates the requested remote
// or verifies that an existing remote points to the same URL instead of silently retargeting it.
func configureRemote(repository *gitlib.Repository, opts Options) error {
	if opts.RemoteURL == "" {
		return nil
	}

	name := opts.RemoteName
	if name == "" {
		name = defaultRemoteName
	}

	remote, err := repository.Remote(name)
	if errors.Is(err, gitlib.ErrRemoteNotFound) {
		// A missing remote is safe to create because its name and URL were validated before repository access.
		if _, err := repository.CreateRemote(&config.RemoteConfig{
			Name: name,
			URLs: []string{opts.RemoteURL},
		}); err != nil {
			return fmt.Errorf("configure Git remote %q: %w", name, err)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("read Git remote %q: %w", name, err)
	}

	if len(remote.Config().URLs) == 0 || remote.Config().URLs[0] != opts.RemoteURL {
		return fmt.Errorf("git remote %q already points to another URL", name)
	}

	return nil
}

// ensureBranch selects branch for the worktree,
// including an unborn branch in a freshly initialized repository.
func ensureBranch(repository *gitlib.Repository, branch string) error {
	if branch == "" {
		return nil
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("open Git worktree: %w", err)
	}

	reference := plumbing.NewBranchReferenceName(branch)
	head, headErr := repository.Head()
	if errors.Is(headErr, plumbing.ErrReferenceNotFound) {
		// PlainInit has no commit and therefore no HEAD target yet.
		// Point HEAD at the requested branch without attempting a checkout.
		if err := repository.Storer.SetReference(
			plumbing.NewSymbolicReference(plumbing.HEAD, reference),
		); err != nil {
			return fmt.Errorf("set unborn Git branch %q: %w", branch, err)
		}

		return nil
	}

	if headErr != nil {
		return fmt.Errorf("read Git HEAD: %w", headErr)
	}
	if headErr == nil && head.Name() == reference {
		return nil
	}

	_, referenceErr := repository.Reference(reference, true)
	create := errors.Is(referenceErr, plumbing.ErrReferenceNotFound)
	if referenceErr != nil && !create {
		return fmt.Errorf("read Git branch %q: %w", branch, referenceErr)
	}

	// Checkout creates the branch only when it does not already exist;
	// this preserves an existing branch history while switching the worktree.
	if err := worktree.Checkout(&gitlib.CheckoutOptions{Branch: reference, Create: create}); err != nil {
		return fmt.Errorf("checkout Git branch %q: %w", branch, err)
	}

	return nil
}

// push sends the current branch to the configured remote
// and treats an already-up-to-date remote as a successful no-op.
func push(ctx context.Context, repository *gitlib.Repository, opts Options, hash plumbing.Hash) (Result, error) {
	name := opts.RemoteName
	if name == "" {
		name = defaultRemoteName
	}

	remote, err := repository.Remote(name)
	if err != nil {
		return Result{}, fmt.Errorf("open Git remote %q: %w", name, err)
	}

	branch, err := currentBranch(repository)
	if err != nil {
		return Result{}, err
	}

	pushOptions, err := pushOptions(opts.PushOptions)
	if err != nil {
		return Result{}, err
	}

	ref := plumbing.NewBranchReferenceName(branch)
	authMethod, err := auth(opts)
	if err != nil {
		return Result{}, err
	}

	// Use an explicit same-branch refspec
	// so a configured branch cannot be accidentally pushed to a different remote branch.
	if err := remote.PushContext(ctx, &gitlib.PushOptions{
		RemoteName: name,
		RefSpecs:   []config.RefSpec{config.RefSpec(ref + ":" + ref)},
		Auth:       authMethod,
		Options:    pushOptions,
	}); err != nil && !errors.Is(err, gitlib.NoErrAlreadyUpToDate) {
		return Result{}, fmt.Errorf("push Git branch %q: %w", branch, err)
	}

	return Result{Pushed: true, Hash: hash}, nil
}

// pushOptions converts CLI values into go-git's push option map.
// The first equals sign separates an option name from its value,
// so values such as ci.variable=NAME=VALUE remain intact.
func pushOptions(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	result := make(map[string]string, len(values))
	for _, raw := range values {
		name, value, _ := strings.Cut(raw, "=")
		if name == "" {
			return nil, fmt.Errorf("git push option must not be empty: %q", raw)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("git push option %q is repeated", name)
		}
		result[name] = value
	}

	return result, nil
}

// currentBranch returns the short name of HEAD and rejects detached HEADs,
// which cannot be used as a stable push target.
func currentBranch(repository *gitlib.Repository) (string, error) {
	head, err := repository.Head()
	if err != nil {
		return "", fmt.Errorf("read Git HEAD: %w", err)
	}
	if !head.Name().IsBranch() {
		return "", errors.New("git repository is in detached HEAD state")
	}

	return head.Name().Short(), nil
}

// author resolves commit identity defaults and stamps the commit in UTC.
func author(opts Options) *object.Signature {
	name, email := opts.AuthorName, opts.AuthorEmail
	if name == "" {
		name = defaultAuthorName
	}
	if email == "" {
		email = defaultAuthorMail
	}

	return &object.Signature{Name: name, Email: email, When: time.Now().UTC()}
}

// auth builds an explicit go-git authentication method from configured SSH or HTTP credentials;
// nil means the remote needs no configured credentials.
func auth(opts Options) (transport.AuthMethod, error) {
	if opts.SSHKeyFile != "" {
		// A private key takes precedence over SSH-agent and HTTP settings.
		publicKeys, err := gitssh.NewPublicKeysFromFile("git", opts.SSHKeyFile, opts.SSHKeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("load Git SSH key: %w", err)
		}

		if opts.KnownHostsFile != "" {
			callback, err := gitssh.NewKnownHostsCallback(opts.KnownHostsFile)
			if err != nil {
				return nil, fmt.Errorf("load Git known_hosts: %w", err)
			}

			publicKeys.HostKeyCallback = callback
		}

		return publicKeys, nil
	}

	if opts.KnownHostsFile != "" {
		// Without a key file, use the SSH agent but still enforce known_hosts.
		agent, err := gitssh.NewSSHAgentAuth("git")
		if err != nil {
			return nil, fmt.Errorf("open Git SSH agent: %w", err)
		}

		callback, err := gitssh.NewKnownHostsCallback(opts.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("load Git known_hosts: %w", err)
		}

		agent.HostKeyCallback = callback
		return agent, nil
	}

	if opts.PasswordFile == "" {
		return nil, nil
	}

	password, err := readPasswordFile(opts.PasswordFile)
	if err != nil {
		return nil, err
	}

	return &githttp.BasicAuth{Username: opts.Username, Password: password}, nil
}

// readPasswordFile reads one bounded, trimmed password without exposing it in errors or logs.
func readPasswordFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open Git password file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// The limit prevents a misconfigured path from loading an unbounded file.
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read Git password file: %w", err)
	}

	password := strings.TrimSpace(string(data))
	if password == "" {
		return "", errors.New("git password file is empty")
	}

	return password, nil
}

// validateRepositoryPath checks an existing destination without following a symlink.
// Git writes repository metadata and backup files below this path;
// accepting a symlink would allow an unexpected target directory to be modified
// when the destination comes from configuration or an environment variable.
func validateRepositoryPath(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("git directory must not be a symlink: %q", path)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("git path is not a directory: %q", path)
		}

		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("inspect Git directory: %w", err)
}

// directoryEmpty reports whether path contains no entries.
// Errors are treated as non-empty so callers never clone into an unreadable destination.
func directoryEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}
