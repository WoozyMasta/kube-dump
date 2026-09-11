// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/keyring"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/filelock"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
)

const (
	resourceTransformStateName     = "state.json"
	resourceTransformOldResources  = "old-resources"
	resourceTransformOldCrypto     = "old-crypto"
	resourceTransformNewResources  = "new-resources"
	resourceTransformNewCrypto     = "new-crypto"
	resourceTransformStateVersion  = 1
	resourceTransformStateMaxBytes = 4 << 10
)

const (
	resourceTransformPrepared  = "prepared"
	resourceTransformCommitted = "committed"
)

// resourceTransformState is the bounded on-disk journal for one transform.
type resourceTransformState struct {
	Phase        string `json:"phase"`
	Version      int    `json:"version"`
	OldResources bool   `json:"oldResources"`
	OldCrypto    bool   `json:"oldCrypto"`
}

// resourceTransformFS isolates filesystem effects so rollback failures are deterministic in tests.
type resourceTransformFS struct {
	rename        func(string, string) error
	removeAll     func(string) error
	syncDirectory func(string) error
	writeAtomic   func(string, func(io.Writer) error) error
}

// defaultResourceTransformFS returns the production filesystem operations.
func defaultResourceTransformFS() resourceTransformFS {
	return resourceTransformFS{
		rename:        os.Rename,
		removeAll:     os.RemoveAll,
		syncDirectory: fileutil.SyncDirectory,
		writeAtomic:   writeAtomic,
	}
}

// Execute decrypts inline resource fields and retains reversible markers.
func (c *DecryptResourceCommand) Execute(_ []string) error {
	if c.Positional.Input == "" || c.Positional.Output == "" {
		return errors.New("input and output resource directories are required")
	}
	if filepath.Clean(c.Positional.Input) == filepath.Clean(c.Positional.Output) {
		return errors.New("input and output resource directories must differ")
	}
	if err := validateResourceTransformPaths(c.Positional.Input, c.Positional.Output); err != nil {
		return err
	}

	lockPath, err := resourceWriterLockPath(c.Positional.Output)
	if err != nil {
		return err
	}
	writerLock, err := filelock.Acquire(c.context(), lockPath)
	if err != nil {
		return fmt.Errorf("acquire resource transform lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()
	if err := recoverResourceWriterState(c.Positional.Output); err != nil {
		return fmt.Errorf("recover resource writer state: %w", err)
	}

	inputRoot := resourceRoot(c.Positional.Input)
	staging, err := newResourceTransformStaging(c.Positional.Output)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	outputRoot := resourceRoot(staging)
	identities, err := loadResourceIdentities(c.IdentityOptions)
	if err != nil {
		return err
	}

	var keys fieldcrypto.SIVKeyResolver
	containsSIV, err := resourceTreeContainsSIV(inputRoot)
	if err != nil {
		return fmt.Errorf("inspect resource encryption markers: %w", err)
	}
	if containsSIV {
		keys, err = keyring.Open(c.Positional.Input, nil, identities)
		if err != nil {
			return fmt.Errorf("open resource AES-SIV keyring: %w", err)
		}
	}

	if err := copyResourceMetadata(c.Positional.Input, staging); err != nil {
		return err
	}

	if err := transformResourceDirectory(
		c.context(),
		inputRoot,
		outputRoot,
		c.progress,
		func(object state.Object,
		) (state.Object, error) {
			return fieldcrypto.DecryptFields(object, identities, keys)
		}); err != nil {
		return err
	}

	if err := publishResourceTransform(staging, c.Positional.Output); err != nil {
		return fmt.Errorf("publish decrypted resources: %w", err)
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "decrypt").
		Str("destination", c.Positional.Output).
		Msg("resource field decryption completed")

	return nil
}

// Execute re-encrypts reversible plaintext markers with their recorded algorithms.
func (c *EncryptResourceCommand) Execute(_ []string) error {
	if c.Positional.Input == "" || c.Positional.Output == "" {
		return errors.New("input and output resource directories are required")
	}
	if filepath.Clean(c.Positional.Input) == filepath.Clean(c.Positional.Output) {
		return errors.New("input and output resource directories must differ")
	}
	if err := validateResourceTransformPaths(c.Positional.Input, c.Positional.Output); err != nil {
		return err
	}

	lockPath, err := resourceWriterLockPath(c.Positional.Output)
	if err != nil {
		return err
	}
	writerLock, err := filelock.Acquire(c.context(), lockPath)
	if err != nil {
		return fmt.Errorf("acquire resource transform lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()

	if err := recoverResourceWriterState(c.Positional.Output); err != nil {
		return fmt.Errorf("recover resource writer state: %w", err)
	}

	inputRoot := resourceRoot(c.Positional.Input)
	staging, err := newResourceTransformStaging(c.Positional.Output)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	outputRoot := resourceRoot(staging)
	recipients, err := parseRecipients(
		c.Recipients,
		c.RecipientsFiles,
		c.RecipientsURLs,
		c.RecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("load resource recipients: %w", err)
	}

	identities, err := loadResourceIdentities(c.IdentityOptions)
	if err != nil {
		return err
	}

	// Metadata and keyring files are copied into staging;
	// only canonical resource files pass through the decrypt/encrypt callback.
	if err := copyResourceMetadata(c.Positional.Input, staging); err != nil {
		return err
	}

	var keyID string
	var cipherKey *siv.Cipher
	containsDecryptedSIV, err := resourceTreeContainsDecryptedSIV(inputRoot)
	if err != nil {
		return fmt.Errorf("inspect resource encryption markers: %w", err)
	}

	if containsDecryptedSIV {
		// A decrypted SIV marker carries no ciphertext key material.
		// Re-encryption therefore opens the destination keyring before transforming any object.
		keyringOptions := staging
		opened, err := keyring.Open(keyringOptions, recipients, identities)
		if err != nil {
			return fmt.Errorf("open resource AES-SIV keyring: %w", err)
		}

		activeKeyID, cipher, err := opened.Active()
		if err != nil {
			return fmt.Errorf("load active resource AES-SIV key: %w", err)
		}

		keyID = activeKeyID
		cipherKey = cipher
	}

	if err := transformResourceDirectory(
		c.context(),
		inputRoot,
		outputRoot,
		c.progress,
		func(object state.Object,
		) (state.Object, error) {
			options := fieldcrypto.Options{Recipients: recipients, SIVKeyID: keyID}
			options.SIVCipher = cipherKey
			return fieldcrypto.EncryptDecryptedFields(object, options)
		}); err != nil {
		return err
	}
	if err := publishResourceTransform(staging, c.Positional.Output); err != nil {
		return fmt.Errorf("publish encrypted resources: %w", err)
	}

	log.Info().
		Str("component", "resource").
		Str("operation", "encrypt").
		Str("destination", c.Positional.Output).
		Msg("resource field encryption completed")

	return nil
}

// validateResourceTransformPaths rejects roots that overlap through ordinary paths
// or resolved symlinks/junctions, preventing output from entering input.
func validateResourceTransformPaths(input, output string) error {
	inputPath, err := canonicalFilesystemPath(input)
	if err != nil {
		return fmt.Errorf("resolve resource input path: %w", err)
	}
	outputPath, err := canonicalFilesystemPath(output)
	if err != nil {
		return fmt.Errorf("resolve resource output path: %w", err)
	}
	if resourcePathsOverlap(inputPath, outputPath) {
		return errors.New("input and output resource directories must not overlap")
	}

	return nil
}

// canonicalFilesystemPath resolves an existing path or its nearest existing parent.
func canonicalFilesystemPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	if _, err := os.Lstat(absolute); err == nil {
		return filepath.EvalSymlinks(absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	parent, err := canonicalFilesystemPath(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}

	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// resourcePathsOverlap reports whether either path contains the other.
func resourcePathsOverlap(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}

	return resourcePathContains(left, right) || resourcePathContains(right, left)
}

// resourcePathContains reports whether target is below root.
func resourcePathContains(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." {
		return false
	}

	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// newResourceTreeStaging creates a sibling tree on the destination filesystem.
func newResourceTreeStaging(output string) (string, error) {
	if err := recoverResourcePublication(output); err != nil {
		return "", fmt.Errorf("recover resource publication: %w", err)
	}

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("create resource output parent: %w", err)
	}

	staging, err := os.MkdirTemp(parent, ".kube-dump-resource-staging-")
	if err != nil {
		return "", fmt.Errorf("create resource staging directory: %w", err)
	}

	return staging, nil
}

// newResourceTransformStaging creates a temporary sibling for a resources subtree.
// Unlike a save, a transform must not recover or replace the whole destination root.
func newResourceTransformStaging(output string) (string, error) {
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("create resource transform parent: %w", err)
	}

	staging, err := os.MkdirTemp(parent, ".kube-dump-resource-transform-")
	if err != nil {
		return "", fmt.Errorf("create resource transform staging directory: %w", err)
	}

	return staging, nil
}

// resourceWriterLockPath returns the shared sibling lock
// used by local resource writers and AES-SIV keyring maintenance for one destination.
// Keeping it outside the destination avoids making
// a new Git clone target non-empty or dirtying an existing worktree.
func resourceWriterLockPath(root string) (string, error) {
	if root == "" {
		return "", errors.New("resource writer lock destination is required")
	}

	absolute, err := canonicalFilesystemPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve resource writer lock destination: %w", err)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	name := fmt.Sprintf(".kube-dump-resource-writer-%x.lock", digest[:12])
	return filepath.Join(filepath.Dir(absolute), name), nil
}

// resourcePublicationBackupPrefix identifies recovery directories for one canonical destination.
func resourcePublicationBackupPrefix(destination string) (string, error) {
	canonical, err := canonicalFilesystemPath(destination)
	if err != nil {
		return "", fmt.Errorf("resolve resource publication destination: %w", err)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(canonical)))
	return fmt.Sprintf(".kube-dump-resource-backup-%x-", digest[:12]), nil
}

// recoverResourceWriterState repairs pending transforms
// before another writer reads or changes the destination.
// Legacy single-tree backups are recovered separately because they have no transaction state.
func recoverResourceWriterState(destination string) error {
	if err := recoverResourceTransform(destination); err != nil {
		return err
	}
	if err := recoverResourcePublication(resourceRoot(destination)); err != nil {
		return fmt.Errorf("recover resource publication: %w", err)
	}
	if err := recoverResourcePublication(filepath.Join(destination, cryptoLayoutDirectory)); err != nil {
		return fmt.Errorf("recover resource crypto publication: %w", err)
	}

	return nil
}

// resourceTransformPath returns the one transaction directory for a canonical destination.
func resourceTransformPath(destination string) (string, error) {
	canonical, err := canonicalFilesystemPath(destination)
	if err != nil {
		return "", fmt.Errorf("resolve resource transform destination: %w", err)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(canonical)))
	name := fmt.Sprintf(".kube-dump-resource-transform-%x", digest[:12])

	return filepath.Join(filepath.Dir(canonical), name), nil
}

// recoverResourceTransform repairs the exact transaction for one destination.
// A prepared transaction is rolled back; a committed transaction only needs cleanup.
func recoverResourceTransform(destination string) error {
	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		return err
	}

	info, err := os.Lstat(transactionPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect resource transform transaction: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("resource transform transaction path is not a directory")
	}
	entries, err := os.ReadDir(transactionPath)
	if err != nil {
		return fmt.Errorf("read resource transform transaction: %w", err)
	}
	// A crash after the state file was removed can leave only this empty directory.
	// It contains no recovery data and is safe to remove.
	if len(entries) == 0 {
		if err := os.Remove(transactionPath); err != nil {
			return fmt.Errorf("remove completed resource transform transaction: %w", err)
		}
		if err := fileutil.SyncDirectory(filepath.Dir(transactionPath)); err != nil {
			return fmt.Errorf("sync completed resource transform transaction: %w", err)
		}
		return nil
	}
	if err := validateResourceTransformTransaction(transactionPath); err != nil {
		return err
	}

	state, err := readResourceTransformState(transactionPath)
	if err != nil {
		return err
	}
	transaction := &resourceTransformTransaction{
		path:        transactionPath,
		destination: destination,
		state:       state,
		fs:          defaultResourceTransformFS(),
	}
	if state.Phase == resourceTransformPrepared {
		return transaction.rollback()
	}

	return transaction.cleanup()
}

// validateResourceTransformTransaction rejects unknown or unsafe transaction entries.
func validateResourceTransformTransaction(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read resource transform transaction: %w", err)
	}

	for _, entry := range entries {
		if entry.Name() == resourceTransformStateName {
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return errors.New("resource transform state is not a regular file")
			}

			continue
		}

		switch entry.Name() {
		case resourceTransformOldResources, resourceTransformOldCrypto,
			resourceTransformNewResources, resourceTransformNewCrypto:
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return fmt.Errorf("resource transform transaction tree is invalid: %q", entry.Name())
			}

		default:
			return fmt.Errorf("resource transform transaction contains unsupported entry %q", entry.Name())
		}
	}

	if _, err := os.Lstat(filepath.Join(path, resourceTransformStateName)); errors.Is(err, os.ErrNotExist) {
		return errors.New("resource transform transaction state is missing")
	} else if err != nil {
		return fmt.Errorf("inspect resource transform state: %w", err)
	}

	return nil
}

// readResourceTransformState reads and validates a small JSON state document.
func readResourceTransformState(path string) (resourceTransformState, error) {
	statePath := filepath.Join(path, resourceTransformStateName)
	file, err := os.Open(statePath)
	if err != nil {
		return resourceTransformState{}, fmt.Errorf("open resource transform state: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, resourceTransformStateMaxBytes+1))
	if err != nil {
		return resourceTransformState{}, fmt.Errorf("read resource transform state: %w", err)
	}
	if len(data) > resourceTransformStateMaxBytes {
		return resourceTransformState{}, errors.New("resource transform state is too large")
	}

	var value resourceTransformState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return resourceTransformState{}, fmt.Errorf("decode resource transform state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return resourceTransformState{}, errors.New("resource transform state contains multiple JSON values")
		}
		return resourceTransformState{}, fmt.Errorf("decode resource transform state trailer: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return resourceTransformState{}, fmt.Errorf("inspect resource transform state fields: %w", err)
	}
	for _, field := range []string{"version", "phase", "oldResources", "oldCrypto"} {
		if _, found := fields[field]; !found {
			return resourceTransformState{}, fmt.Errorf("resource transform state field %q is missing", field)
		}
	}

	if value.Version != resourceTransformStateVersion {
		return resourceTransformState{}, fmt.Errorf("unsupported resource transform state version %d", value.Version)
	}
	if value.Phase != resourceTransformPrepared && value.Phase != resourceTransformCommitted {
		return resourceTransformState{}, fmt.Errorf("unsupported resource transform phase %q", value.Phase)
	}

	return value, nil
}

// publishResourceTransform atomically coordinates resources and the private keyring.
// The transaction owns both old and new trees until it is committed;
// all other top-level destination entries remain outside its scope.
func publishResourceTransform(staging, destination string) error {
	return publishResourceTransformWithFS(staging, destination, defaultResourceTransformFS())
}

// publishResourceTransformWithFS publishes a transform with injectable filesystem operations for tests.
func publishResourceTransformWithFS(staging, destination string, fs resourceTransformFS) error {
	transaction, err := prepareResourceTransformWithFS(staging, destination, fs)
	if err != nil {
		return err
	}

	if err := transaction.install(); err != nil {
		rollbackErr := transaction.rollback()
		return errors.Join(err, rollbackErr)
	}

	if err := transaction.commit(); err != nil {
		if transaction.state.Phase == resourceTransformCommitted {
			log.Warn().Err(err).Str("path", transaction.path).Msg("resource transform committed with cleanup pending")
			return err
		}

		rollbackErr := transaction.rollback()
		return errors.Join(err, rollbackErr)
	}

	if err := transaction.cleanup(); err != nil {
		log.Warn().Err(err).Str("path", transaction.path).Msg("failed to clean resource transform transaction")
	}

	return nil
}

type resourceTransformTransaction struct {
	fs            resourceTransformFS
	path          string
	destination   string
	staging       string
	state         resourceTransformState
	stagingCrypto bool
}

// ensureFS fills omitted test operations with their production implementations.
func (t *resourceTransformTransaction) ensureFS() {
	defaults := defaultResourceTransformFS()
	if t.fs.rename == nil {
		t.fs.rename = defaults.rename
	}
	if t.fs.removeAll == nil {
		t.fs.removeAll = defaults.removeAll
	}
	if t.fs.syncDirectory == nil {
		t.fs.syncDirectory = defaults.syncDirectory
	}
	if t.fs.writeAtomic == nil {
		t.fs.writeAtomic = defaults.writeAtomic
	}
}

// prepareResourceTransformWithFS creates the journal before moving either managed tree.
func prepareResourceTransformWithFS(
	staging, destination string,
	fs resourceTransformFS,
) (*resourceTransformTransaction, error) {
	if err := validateResourceTransformStaging(staging, destination); err != nil {
		return nil, err
	}

	transactionPath, err := resourceTransformPath(destination)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(transactionPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("resource transform transaction path is not a directory")
		}
		return nil, errors.New("resource transform transaction already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect resource transform transaction: %w", err)
	}

	resourceExists, err := validateResourceTransformDestination(resourceRoot(destination))
	if err != nil {
		return nil, err
	}
	cryptoExists, err := validateResourceTransformCryptoDestination(destination)
	if err != nil {
		return nil, err
	}
	stagingCrypto, err := resourceTransformStagingCryptoExists(staging)
	if err != nil {
		return nil, fmt.Errorf("inspect staged resource crypto metadata: %w", err)
	}

	if err := os.Mkdir(transactionPath, 0o700); err != nil {
		return nil, fmt.Errorf("create resource transform transaction: %w", err)
	}
	if err := fileutil.SyncDirectory(filepath.Dir(transactionPath)); err != nil {
		return nil, fmt.Errorf("sync resource transform transaction parent: %w", err)
	}

	transaction := &resourceTransformTransaction{
		path:        transactionPath,
		destination: destination,
		staging:     staging,
		state: resourceTransformState{
			Version:      resourceTransformStateVersion,
			Phase:        resourceTransformPrepared,
			OldResources: resourceExists,
			OldCrypto:    cryptoExists,
		},
		stagingCrypto: stagingCrypto,
		fs:            fs,
	}
	if err := transaction.writeState(); err != nil {
		return nil, errors.Join(err, os.RemoveAll(transactionPath))
	}

	return transaction, nil
}

// validateResourceTransformStaging checks the two trees before transaction state is created.
func validateResourceTransformStaging(staging, destination string) error {
	if staging == "" || destination == "" {
		return errors.New("resource transform staging and destination are required")
	}
	if _, err := validateResourceTransformDestination(resourceRoot(staging)); err != nil {
		return fmt.Errorf("validate staged resources: %w", err)
	}

	if _, err := validateResourceTransformCryptoDestination(staging); err != nil {
		return fmt.Errorf("validate staged resource crypto metadata: %w", err)
	}
	if _, err := validateResourceTransformDestination(destination); err != nil {
		return fmt.Errorf("validate resource transform destination: %w", err)
	}

	return nil
}

// validateResourceTransformDestination accepts only a real directory or absence.
func validateResourceTransformDestination(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("path must be a directory or absent: %q", path)
	}

	return true, nil
}

// validateResourceTransformCryptoDestination also validates the private metadata parent.
func validateResourceTransformCryptoDestination(root string) (bool, error) {
	if _, err := validateResourceTransformDestination(root); err != nil {
		return false, err
	}
	metadataRoot := filepath.Join(root, filepath.Dir(cryptoLayoutDirectory))
	metadataExists, err := validateResourceTransformDestination(metadataRoot)
	if err != nil {
		return false, err
	}
	if !metadataExists {
		return false, nil
	}

	return validateResourceTransformDestination(filepath.Join(root, cryptoLayoutDirectory))
}

// resourceTransformStagingCryptoExists reports whether staging carries a keyring tree.
func resourceTransformStagingCryptoExists(staging string) (bool, error) {
	info, err := os.Lstat(filepath.Join(staging, cryptoLayoutDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("staged resource crypto metadata must be a directory")
	}

	return true, nil
}

// writeState durably records a bounded state before the first managed rename.
func (t *resourceTransformTransaction) writeState() error {
	t.ensureFS()
	data, err := json.Marshal(t.state)
	if err != nil {
		return fmt.Errorf("encode resource transform state: %w", err)
	}
	if len(data) > resourceTransformStateMaxBytes {
		return errors.New("resource transform state is too large")
	}
	statePath := filepath.Join(t.path, resourceTransformStateName)
	if err := t.fs.writeAtomic(statePath, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("write resource transform state: %w", err)
	}
	if err := t.fs.syncDirectory(t.path); err != nil {
		return fmt.Errorf("sync resource transform state: %w", err)
	}

	return nil
}

// install moves staged and old trees into the transaction, then installs both new trees.
func (t *resourceTransformTransaction) install() error {
	if err := t.moveStagedTrees(); err != nil {
		return err
	}
	if err := t.moveOldTrees(); err != nil {
		return err
	}

	if err := t.installTree(
		resourceTransformNewCrypto,
		filepath.Join(t.destination, cryptoLayoutDirectory),
		t.stagingCrypto,
	); err != nil {
		return err
	}

	return t.installTree(resourceTransformNewResources, resourceRoot(t.destination), true)
}

// moveStagedTrees takes ownership of the staged trees before touching destination paths.
func (t *resourceTransformTransaction) moveStagedTrees() error {
	if err := t.movePath(
		resourceRoot(t.staging),
		filepath.Join(t.path, resourceTransformNewResources),
	); err != nil {
		return fmt.Errorf("stage new resources: %w", err)
	}

	if t.stagingCrypto {
		if err := t.movePath(
			filepath.Join(t.staging, cryptoLayoutDirectory),
			filepath.Join(t.path, resourceTransformNewCrypto),
		); err != nil {
			return fmt.Errorf("stage new resource crypto metadata: %w", err)
		}
	}

	return nil
}

// moveOldTrees places the previous managed trees in the journal for rollback.
func (t *resourceTransformTransaction) moveOldTrees() error {
	if t.state.OldResources {
		if err := t.movePath(
			resourceRoot(t.destination),
			filepath.Join(t.path, resourceTransformOldResources),
		); err != nil {
			return fmt.Errorf("move old resources: %w", err)
		}
	}

	if t.state.OldCrypto {
		if err := t.movePath(
			filepath.Join(t.destination, cryptoLayoutDirectory),
			filepath.Join(t.path, resourceTransformOldCrypto),
		); err != nil {
			return fmt.Errorf("move old resource crypto metadata: %w", err)
		}
	}

	return nil
}

// movePath renames one managed tree and syncs both directory entries.
func (t *resourceTransformTransaction) movePath(source, target string) error {
	t.ensureFS()
	if err := t.fs.rename(source, target); err != nil {
		return err
	}
	if err := t.fs.syncDirectory(filepath.Dir(source)); err != nil {
		return fmt.Errorf("sync source directory: %w", err)
	}
	if err := t.fs.syncDirectory(filepath.Dir(target)); err != nil {
		return fmt.Errorf("sync transaction directory: %w", err)
	}

	return nil
}

// installTree installs one staged tree, or leaves the destination absent when disabled.
func (t *resourceTransformTransaction) installTree(name, destination string, enabled bool) error {
	if !enabled {
		return nil
	}

	source := filepath.Join(t.path, name)
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return fmt.Errorf("create managed destination parent: %w", err)
	}
	if err := t.movePath(source, destination); err != nil {
		return fmt.Errorf("install %q: %w", destination, err)
	}

	return nil
}

// commit marks both managed trees visible and makes rollback no longer applicable.
func (t *resourceTransformTransaction) commit() error {
	previous := t.state.Phase
	t.state.Phase = resourceTransformCommitted
	if err := t.writeState(); err != nil {
		persisted, readErr := readResourceTransformState(t.path)
		if readErr == nil && persisted.Phase == resourceTransformCommitted {
			t.state = persisted
			return err
		}

		t.state.Phase = previous
		return err
	}

	return nil
}

// cleanup removes only the transaction after a committed publication.
func (t *resourceTransformTransaction) cleanup() error {
	t.ensureFS()

	for _, name := range []string{
		resourceTransformOldResources,
		resourceTransformOldCrypto,
		resourceTransformNewResources,
		resourceTransformNewCrypto,
	} {
		if err := t.fs.removeAll(filepath.Join(t.path, name)); err != nil {
			return fmt.Errorf("remove resource transform tree %q: %w", name, err)
		}
		if err := t.fs.syncDirectory(t.path); err != nil {
			return fmt.Errorf("sync removed resource transform tree %q: %w", name, err)
		}
	}

	if err := t.fs.removeAll(filepath.Join(t.path, resourceTransformStateName)); err != nil {
		return fmt.Errorf("remove resource transform state: %w", err)
	}
	if err := t.fs.syncDirectory(t.path); err != nil {
		return fmt.Errorf("sync removed resource transform state: %w", err)
	}
	if err := t.fs.removeAll(t.path); err != nil {
		return fmt.Errorf("remove empty resource transform transaction: %w", err)
	}
	if err := t.fs.syncDirectory(filepath.Dir(t.path)); err != nil {
		return fmt.Errorf("sync removed resource transform transaction: %w", err)
	}

	return nil
}

// rollback restores both managed trees from a prepared transaction.
func (t *resourceTransformTransaction) rollback() error {
	details := make([]error, 0, 2)
	if err := t.rollbackTree(
		resourceRoot(t.destination),
		filepath.Join(t.path, resourceTransformOldResources),
		t.state.OldResources,
	); err != nil {
		details = append(details, fmt.Errorf("rollback resources: %w", err))
	}

	if err := t.rollbackTree(
		filepath.Join(t.destination, cryptoLayoutDirectory),
		filepath.Join(t.path, resourceTransformOldCrypto),
		t.state.OldCrypto,
	); err != nil {
		details = append(details, fmt.Errorf("rollback resource crypto metadata: %w", err))
	}

	if len(details) > 0 {
		return errors.Join(details...)
	}

	return t.cleanup()
}

// rollbackTree removes a new managed tree and restores its old tree when present.
func (t *resourceTransformTransaction) rollbackTree(destination, backup string, oldExists bool) error {
	t.ensureFS()
	backupExists, err := resourceTransformDirectoryExists(backup)
	if err != nil {
		return err
	}

	if backupExists {
		if err := t.removeTree(destination); err != nil {
			return err
		}
		if err := t.fs.rename(backup, destination); err != nil {
			return err
		}
		if err := t.fs.syncDirectory(filepath.Dir(backup)); err != nil {
			return err
		}
		if err := t.fs.syncDirectory(filepath.Dir(destination)); err != nil {
			return err
		}
		return nil
	}

	if oldExists {
		// The backup was already restored before a retry or interruption.
		return nil
	}

	return t.removeTree(destination)
}

func resourceTransformDirectoryExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("transaction tree is not a directory: %q", path)
	}

	return true, nil
}

func (t *resourceTransformTransaction) removeTree(path string) error {
	t.ensureFS()
	exists, err := resourceTransformDirectoryExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := t.fs.removeAll(path); err != nil {
		return err
	}

	return t.fs.syncDirectory(filepath.Dir(path))
}

// prepareResourceKeyringRollback returns cleanup for a keyring created by the current run.
// An existing or non-empty keyring is never removed when collection fails.
func prepareResourceKeyringRollback(root string) (func() error, error) {
	cryptoRoot := filepath.Join(root, cryptoLayoutDirectory)
	info, err := os.Lstat(cryptoRoot)
	if errors.Is(err, os.ErrNotExist) {
		return func() error { return os.RemoveAll(cryptoRoot) }, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect resource AES-SIV keyring: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return func() error { return nil }, nil
	}

	entries, err := os.ReadDir(cryptoRoot)
	if err != nil {
		return nil, fmt.Errorf("read resource AES-SIV keyring: %w", err)
	}
	if len(entries) > 0 {
		return func() error { return nil }, nil
	}

	return func() error { return os.RemoveAll(cryptoRoot) }, nil
}

// recoverResourcePublication restores the previous tree left aside
// by an interrupted two-step directory replacement.
// It only considers directories with kube-dump's private backup prefix
// and refuses to guess when several candidates exist.
func recoverResourcePublication(destination string) error {
	if destination == "" {
		return errors.New("resource destination is required")
	}

	parent := filepath.Dir(destination)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list resource publication directory: %w", err)
	}

	prefix, err := resourcePublicationBackupPrefix(destination)
	if err != nil {
		return err
	}

	backups := make([]string, 0, 1)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) ||
			entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}

		backups = append(backups, filepath.Join(parent, entry.Name()))
	}

	if len(backups) == 0 {
		return nil
	}

	if _, err := os.Lstat(destination); err == nil {
		// The new tree is already visible, so exact-destination backups are stale.
		for _, backup := range backups {
			if err := os.RemoveAll(backup); err != nil {
				return fmt.Errorf("remove stale resource backup: %w", err)
			}
		}
		if len(backups) > 0 {
			if err := fileutil.SyncDirectory(parent); err != nil {
				return fmt.Errorf("sync stale resource backups: %w", err)
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect resource destination: %w", err)
	}

	if len(backups) != 1 {
		return fmt.Errorf("cannot recover resource destination: found %d backup trees", len(backups))
	}

	if err := os.Rename(backups[0], destination); err != nil {
		return fmt.Errorf("restore resource backup tree: %w", err)
	}
	if err := fileutil.SyncDirectory(parent); err != nil {
		return fmt.Errorf("sync restored resource destination: %w", err)
	}

	return nil
}

// publishResourceTree replaces the destination only after staging is complete.
// The old tree is renamed aside first so a failed staging transform never touches it;
// a failed publication attempts to restore the old tree.
func publishResourceTree(staging, destination string) error {
	if staging == "" || destination == "" {
		return errors.New("resource staging and destination are required")
	}
	if filepath.Clean(staging) == filepath.Clean(destination) {
		return errors.New("resource staging and destination must differ")
	}
	if err := recoverResourcePublication(destination); err != nil {
		return fmt.Errorf("recover resource publication: %w", err)
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create resource destination parent: %w", err)
	}

	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, destination); err != nil {
			return fmt.Errorf("publish resource staging tree: %w", err)
		}
		if err := fileutil.SyncDirectory(parent); err != nil {
			return fmt.Errorf("sync resource destination parent: %w", err)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect resource destination: %w", err)
	}
	if !info.IsDir() {
		return errors.New("resource destination must be a directory")
	}

	backupPrefix, err := resourcePublicationBackupPrefix(destination)
	if err != nil {
		return err
	}
	backup, err := os.MkdirTemp(parent, backupPrefix)
	if err != nil {
		return fmt.Errorf("reserve resource destination backup: %w", err)
	}
	backupName := backup
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("prepare resource destination backup: %w", err)
	}

	if err := os.Rename(destination, backupName); err != nil {
		return fmt.Errorf("move existing resource destination: %w", err)
	}
	if err := fileutil.SyncDirectory(parent); err != nil {
		restoreErr := os.Rename(backupName, destination)
		return errors.Join(
			fmt.Errorf("sync resource destination parent: %w", err),
			restoreErr,
		)
	}
	if err := os.Rename(staging, destination); err != nil {
		restoreErr := os.Rename(backupName, destination)
		return errors.Join(
			fmt.Errorf("publish resource staging tree: %w", err),
			restoreErr,
		)
	}
	if err := fileutil.SyncDirectory(parent); err != nil {
		return fmt.Errorf("sync published resource tree: %w", err)
	}

	if err := os.RemoveAll(backupName); err != nil {
		log.Warn().Err(err).Str("path", backupName).Msg("failed to remove previous resource tree")
	} else if err := fileutil.SyncDirectory(parent); err != nil {
		log.Warn().Err(err).Str("path", parent).Msg("failed to sync removed resource tree")
	}

	return nil
}

// loadResourceIdentities loads age identities when resource fields or a keyring require decryption.
// An empty list is valid for plaintext-only input.
func loadResourceIdentities(options IdentityOptions) ([]age.Identity, error) {
	if len(options.IdentityFiles) == 0 {
		return nil, nil
	}

	identities, err := loadConfiguredIdentities(options)
	if err != nil {
		return nil, fmt.Errorf("load resource identities: %w", err)
	}

	return identities, nil
}

// transformResourceDirectory copies metadata and rewrites canonical resources.
func transformResourceDirectory(
	ctx context.Context,
	input, output string,
	manager *progress.Manager,
	transform func(state.Object) (state.Object, error),
) error {
	if ctx == nil || transform == nil {
		return errors.New("resource transform context and callback are required")
	}

	info, err := os.Stat(input)
	if err != nil {
		return fmt.Errorf("inspect resource input: %w", err)
	}
	if !info.IsDir() {
		return errors.New("resource crypto input must be a directory")
	}
	if err := os.MkdirAll(output, 0o750); err != nil {
		return fmt.Errorf("create resource output: %w", err)
	}
	if err := copyResourceOwnershipManifest(input, output); err != nil {
		return fmt.Errorf("copy resource ownership manifest: %w", err)
	}

	var counter *progress.Counter
	if manager != nil && manager.Enabled() {
		total, err := countCanonicalResourceFiles(input)
		if err != nil {
			return fmt.Errorf("count resource input: %w", err)
		}

		counter = manager.NewCounter("Resource files", total)
		defer counter.Complete()
	}

	// Walk the source tree once, leaving transport metadata untouched
	// and transforming only canonical Kubernetes object paths.
	// This preserves the keyring and archive bookkeeping needed by later operations.
	return filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		relative, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		if relative == "." || strings.HasPrefix(filepath.ToSlash(relative), ".kube-dump/") {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !state.IsCanonicalObjectPath(filepath.ToSlash(relative)) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("resource input contains unsupported entry %q", relative)
		}

		//nolint:gosec // canonical paths stay below the selected root.
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read resource %q: %w", path, err)
		}

		value, err := codec.Unmarshal(data)
		if err != nil {
			return fmt.Errorf("decode resource %q: %w", path, err)
		}

		object, err := objectFromResourcePath(input, path, value)
		if err != nil {
			return fmt.Errorf("identify resource %q: %w", path, err)
		}

		result, err := transform(object)
		if err != nil {
			return fmt.Errorf("transform resource %q: %w", path, err)
		}

		encoded, err := codec.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode resource %q: %w", path, err)
		}

		target := filepath.Join(output, filepath.FromSlash(filepath.ToSlash(relative)))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create resource directory: %w", err)
		}
		if err := os.WriteFile(target, encoded, 0o600); err != nil {
			return fmt.Errorf("write resource %q: %w", target, err)
		}

		if counter != nil {
			counter.Increment()
		}

		return nil
	})
}

// copyResourceOwnershipManifest carries prune history through a resource transform.
// The manifest is validated first and its paths are never passed to field encryption.
func copyResourceOwnershipManifest(input, output string) error {
	inputPath := localResourceOwnershipManifestPath(input)
	_, found, err := readLocalResourceOwnershipManifest(inputPath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read resource ownership manifest: %w", err)
	}
	outputPath := localResourceOwnershipManifestPath(output)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("create resource ownership directory: %w", err)
	}
	if err := writeAtomic(outputPath, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("write resource ownership manifest: %w", err)
	}

	return nil
}

// countCanonicalResourceFiles counts transformable resources
// for a truthful file progress total without scanning metadata or Git bookkeeping.
func countCanonicalResourceFiles(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}

		if relative == ".kube-dump" || strings.HasPrefix(relative, ".kube-dump/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}
		if !entry.IsDir() && state.IsCanonicalObjectPath(relative) {
			total++
		}

		return nil
	})

	return total, err
}

// copyResourceMetadata keeps the AES-SIV keyring available
// for a later re-encryption operation without exposing it to Kubernetes output.
func copyResourceMetadata(input, output string) error {
	root := filepath.Join(input, resourceMetadataDirectory)
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect resource metadata: %w", err)
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("resource metadata path must be a directory")
	}

	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}

		target := filepath.Join(output, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("resource metadata contains unsupported entry %q", relative)
		}

		//nolint:gosec // metadata stays below the selected root.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		//nolint:gosec // target is derived below the output root.
		return os.WriteFile(target, data, 0o600)
	})
}

// resourceTreeContainsSIV detects encrypted AES-SIV markers without loading key material;
// the keyring is opened only when a resource needs it.
func resourceTreeContainsSIV(root string) (bool, error) {
	return resourceTreeContains(root, fieldcrypto.ContainsSIV)
}

// resourceTreeContainsDecryptedSIV detects plaintext markers awaiting re-encryption.
func resourceTreeContainsDecryptedSIV(root string) (bool, error) {
	return resourceTreeContains(root, fieldcrypto.ContainsDecryptedSIV)
}

// resourceTreeContains applies a marker predicate to canonical resource files.
func resourceTreeContains(root string, predicate func(state.Object) bool) (bool, error) {
	var found bool
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found || entry.IsDir() {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !state.IsCanonicalObjectPath(filepath.ToSlash(relative)) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("resource input contains unsupported entry %q", relative)
		}

		//nolint:gosec // canonical paths stay below the selected root.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		value, err := codec.Unmarshal(data)
		if err != nil {
			return err
		}

		object, err := objectFromResourcePath(root, path, value)
		if err != nil {
			return err
		}

		found = predicate(object)
		return nil
	})

	return found, err
}
