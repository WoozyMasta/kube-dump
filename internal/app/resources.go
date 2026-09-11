// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/keyring"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/filelock"
	nativegit "github.com/woozymasta/kube-dump/v2/internal/git"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/version"
)

// Execute writes selected Kubernetes objects as a directory tree.
func (c *ResourcesDirCommand) Execute(_ []string) error {
	if c.Path == "" {
		return errors.New("resource backup destination is required")
	}

	lockPath, err := resourceWriterLockPath(c.Path)
	if err != nil {
		return err
	}
	writerLock, err := filelock.Acquire(c.context(), lockPath)
	if err != nil {
		return fmt.Errorf("acquire resource writer lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()
	if err := recoverResourceWriterState(c.Path); err != nil {
		return fmt.Errorf("recover resource writer state: %w", err)
	}

	rollbackKeyring, err := prepareResourceKeyringRollback(c.Path)
	if err != nil {
		return fmt.Errorf("prepare resource keyring rollback: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = rollbackKeyring()
		}
	}()

	staging, err := newResourceTreeStaging(resourceRoot(c.Path))
	if err != nil {
		return fmt.Errorf("create resource staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := copyExistingResourceTree(c.context(), resourceRoot(c.Path), staging); err != nil {
		return fmt.Errorf("stage existing resources: %w", err)
	}

	prepared, err := prepareResourceCollection(
		c.resourceOptions,
		c.selection,
		c.Path,
	)
	if err != nil {
		return fmt.Errorf("prepare resource backup: %w", err)
	}

	sink, err := export.NewDirectorySink(staging)
	if err != nil {
		return fmt.Errorf("open resource destination: %w", err)
	}

	compiled, result, err := collectResources(
		c.context(),
		sink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := reconcileLocalResourceOwnership(
		staging,
		c.resourceOptions.Profile,
		result,
		c.Prune,
	); err != nil {
		return fmt.Errorf("update resource ownership: %w", err)
	}
	if err := finalizeResourceTree(staging, resourceRoot(c.Path), result); err != nil {
		return err
	}
	published = true

	logResourceBackup("directory", c.Path, compiled, result, c.progress)

	return nil
}

// requireCompleteResourceCollection prevents warnings from being mistaken for a published snapshot.
// The caller must stage collection output until this check succeeds.
func requireCompleteResourceCollection(result kube.CollectionResult) error {
	if resourceCollectionComplete(result) {
		return nil
	}

	details := make([]error, 0, 2+len(result.Warnings))
	details = append(details, fmt.Errorf(
		"resource collection is incomplete: failed=%d warnings=%d",
		result.Failed,
		result.WarningTotal(),
	))
	details = append(details, result.Warnings...)
	if omitted := result.OmittedWarningDetails(); omitted > 0 {
		details = append(details, fmt.Errorf("%d warning details omitted", omitted))
	}

	return fmt.Errorf("previous resource snapshot was kept: %w", errors.Join(details...))
}

// copyExistingResourceTree seeds staging so a successful backup preserves files
// outside the current selection until the caller explicitly requests pruning.
func copyExistingResourceTree(ctx context.Context, source, destination string) error {
	if source == "" || destination == "" {
		return errors.New("existing resource source and staging destination are required")
	}
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect existing resource tree: %w", err)
	}

	return copyResourceTree(ctx, source, destination)
}

// finalizeResourceTree publishes staged resources only after a complete collection.
func finalizeResourceTree(source, destination string, result kube.CollectionResult) error {
	if err := requireCompleteResourceCollection(result); err != nil {
		return err
	}

	if err := publishResourceTree(source, destination); err != nil {
		return fmt.Errorf("publish resource backup: %w", err)
	}

	return nil
}

// Execute writes selected Kubernetes objects to one local archive.
func (c *ResourcesArchiveCommand) Execute(_ []string) error {
	if c.Path == "" {
		return errors.New("resource archive destination is required")
	}

	archiveRecipients, err := prepareArchiveRecipients(
		c.Format,
		c.ArchiveRecipients,
		c.ArchiveRecipientsFiles,
		c.ArchiveRecipientsURLs,
		c.ArchiveRecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("prepare archive encryption: %w", err)
	}

	root, err := os.MkdirTemp("", "kube-dump-archive-")
	if err != nil {
		return fmt.Errorf("create temporary archive directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	prepared, err := prepareResourceCollection(
		c.resourceOptions,
		c.selection,
		root,
	)
	if err != nil {
		return fmt.Errorf("prepare resource backup: %w", err)
	}

	sink, err := export.NewDirectorySink(resourceRoot(root))
	if err != nil {
		return fmt.Errorf("open temporary resource destination: %w", err)
	}

	compiled, result, err := collectResources(
		c.context(),
		sink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := requireCompleteResourceCollection(result); err != nil {
		return err
	}

	if err := writeBackupArchive(
		c.context(),
		root,
		c.Path,
		c.Format,
		archiveRecipients,
		c.progress,
	); err != nil {
		return fmt.Errorf("write resource archive: %w", err)
	}

	logResourceBackup("archive", c.Path, compiled, result, c.progress)

	return nil
}

// Execute writes selected Kubernetes objects to one archive object in S3.
func (c *ResourcesArchiveS3Command) Execute(_ []string) error {
	if c.URI == "" {
		return errors.New("S3 archive destination is required")
	}

	archiveRecipients, err := prepareArchiveRecipients(
		c.Format,
		c.ArchiveRecipients,
		c.ArchiveRecipientsFiles,
		c.ArchiveRecipientsURLs,
		c.ArchiveRecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("prepare archive encryption: %w", err)
	}

	store, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 destination: %w", err)
	}

	key := store.ObjectKey()
	if key == "" {
		return errors.New("S3 archive URI must include an object key")
	}

	root, err := os.MkdirTemp("", "kube-dump-archive-s3-")
	if err != nil {
		return fmt.Errorf("create temporary archive directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	// Stage the complete backup locally because archive compression
	// and optional encryption need a seekable tree before the final object can be uploaded.
	prepared, err := prepareResourceCollection(
		c.resourceOptions,
		c.selection,
		root,
	)
	if err != nil {
		return fmt.Errorf("prepare resource backup: %w", err)
	}

	sink, err := export.NewDirectorySink(resourceRoot(root))
	if err != nil {
		return fmt.Errorf("open temporary resource destination: %w", err)
	}

	compiled, result, err := collectResources(
		c.context(),
		sink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := requireCompleteResourceCollection(result); err != nil {
		return err
	}

	archivePath := filepath.Join(root, "backup"+archiveSuffix(c.Format))
	if err := writeBackupArchive(
		c.context(),
		root,
		archivePath,
		c.Format,
		archiveRecipients,
		c.progress,
	); err != nil {
		return fmt.Errorf("create S3 archive: %w", err)
	}

	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("stat S3 archive: %w", err)
	}

	// Upload the finished archive as one S3 object.
	// The temporary tree is not exposed, so consumers cannot observe a partially written archive.
	uploadCounter := c.progress.NewCounter("Archive upload bytes", archiveInfo.Size())
	defer uploadCounter.Complete()
	if err := store.PutFileWithProgress(c.context(), key, archivePath, uploadCounter.Add); err != nil {
		return fmt.Errorf("upload S3 archive: %w", err)
	}

	logResourceBackup("archive-s3", c.URI, compiled, result, c.progress)

	return nil
}

// Execute writes selected Kubernetes objects into a Git worktree.
func (c *ResourcesGitCommand) Execute(_ []string) error {
	if c.Path == "" {
		return errors.New("resource backup destination is required")
	}

	lockPath, err := resourceWriterLockPath(c.Path)
	if err != nil {
		return err
	}
	writerLock, err := filelock.Acquire(c.context(), lockPath)
	if err != nil {
		return fmt.Errorf("acquire resource writer lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()
	if err := recoverResourceWriterState(c.Path); err != nil {
		return fmt.Errorf("recover resource writer state: %w", err)
	}

	rollbackKeyring, err := prepareResourceKeyringRollback(c.Path)
	if err != nil {
		return fmt.Errorf("prepare resource keyring rollback: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = rollbackKeyring()
		}
	}()

	repository, _, err := nativegit.Prepare(c.context(), c.Path, c.Git)
	if err != nil {
		return fmt.Errorf("prepare Git destination: %w", err)
	}

	staging, err := newResourceTreeStaging(resourceRoot(c.Path))
	if err != nil {
		return fmt.Errorf("create resource staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := copyExistingResourceTree(c.context(), resourceRoot(c.Path), staging); err != nil {
		return fmt.Errorf("stage existing resources: %w", err)
	}

	prepared, err := prepareResourceCollection(
		c.resourceOptions,
		c.selection,
		c.Path,
	)
	if err != nil {
		return fmt.Errorf("prepare resource backup: %w", err)
	}

	sink, err := export.NewDirectorySink(staging)
	if err != nil {
		return fmt.Errorf("open resource destination: %w", err)
	}

	compiled, result, err := collectResources(
		c.context(),
		sink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := reconcileLocalResourceOwnership(
		staging,
		c.resourceOptions.Profile,
		result,
		c.Prune,
	); err != nil {
		return fmt.Errorf("update resource ownership: %w", err)
	}
	if err := finalizeResourceTree(staging, resourceRoot(c.Path), result); err != nil {
		return err
	}
	published = true

	message := resourceBackupCommitMessage(
		compiled.Name(),
		c.clientOptions.Context,
		prepared.selection,
		prepared.fieldEncryption.Mode,
		result,
		version.Version,
	)

	gitResult, err := nativegit.Commit(c.context(), repository, c.Git, message)
	if err != nil {
		return fmt.Errorf("finish Git backup: %w", err)
	}
	if c.Git.Commit || c.Git.Push {
		logGitBackupResult(c.Path, c.Git, gitResult)
	}

	logResourceBackup("git", c.Path, compiled, result, c.progress)

	return nil
}

// logGitBackupResult reports the requested Git persistence outcome separately from collection totals,
// so a clean worktree is distinguishable from a new commit.
func logGitBackupResult(destination string, options nativegit.Options, result nativegit.Result) {
	event := log.Info().
		Str("component", "git").
		Str("destination", destination).
		Bool("commit_requested", options.Commit).
		Bool("push_requested", options.Push).
		Bool("committed", result.Committed).
		Bool("pushed", result.Pushed)

	if !result.Hash.IsZero() {
		event = event.Str("commit", result.Hash.String())
	}

	switch {
	case result.Committed:
		event.Msg("Git backup commit created")

	case result.Pushed:
		event.Msg("Git backup is up to date")

	default:
		event.Msg("Git backup has no changes")
	}
}

// resourceBackupCommitMessage creates a stable Git subject
// and trailer block that records the effective backup scope
// without copying credentials or other sensitive command-line values into repository history.
func resourceBackupCommitMessage(
	profileName, contextName string,
	selection kube.Selection,
	mode fieldcrypto.Mode,
	result kube.CollectionResult,
	versionName string,
) string {
	trailers := []string{"Kube-Dump-Profile: " + gitTrailerValue(profileName)}

	if contextName != "" {
		trailers = append(trailers, "Kube-Dump-Context: "+gitTrailerValue(contextName))
	}

	trailers = append(trailers,
		"Kube-Dump-Namespaces: "+selectedNamespaces(selection),
		"Kube-Dump-Resource-Count: "+strconv.Itoa(result.Collected),
		"Kube-Dump-Changed: "+strconv.Itoa(result.Changed),
		"Kube-Dump-Field-Encryption: "+gitTrailerValue(string(mode)),
		"Kube-Dump-Version: "+gitTrailerValue(versionName),
	)

	if len(selection.ExcludeNamespaces) > 0 {
		excluded := append([]string(nil), selection.ExcludeNamespaces...)
		sort.Strings(excluded)
		trailers = append(
			trailers,
			"Kube-Dump-Excluded-Namespaces: "+gitTrailerValue(strings.Join(excluded, ",")),
		)
	}

	return "kube-dump: " + gitTrailerValue(profileName) + "\n\n" + strings.Join(trailers, "\n")
}

// selectedNamespaces renders an order-independent namespace scope for a trailer.
func selectedNamespaces(selection kube.Selection) string {
	if len(selection.Namespaces) == 0 {
		return "all"
	}

	namespaces := append([]string(nil), selection.Namespaces...)
	sort.Strings(namespaces)

	return gitTrailerValue(strings.Join(namespaces, ","))
}

// gitTrailerValue keeps external names on one line
// so they cannot alter the commit message structure
// when copied from kubeconfig or profile input.
func gitTrailerValue(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}

	return value
}

// Execute writes canonical resource files below an S3 URI.
func (c *ResourcesS3Command) Execute(_ []string) error {
	if c.URI == "" {
		return errors.New("S3 destination is required")
	}

	baseStore, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 destination: %w", err)
	}

	previousGeneration, expectedPointer, found, err := readCurrentResourceGeneration(
		c.context(), baseStore,
	)
	if err != nil {
		return fmt.Errorf("read current S3 resource generation: %w", err)
	}

	generation, err := newResourceGeneration()
	if err != nil {
		return err
	}

	generationStore, err := newS3Store(
		c.context(),
		withS3LayoutPrefix(c.S3Destination, resourceGenerationPrefix(generation)),
	)
	if err != nil {
		return fmt.Errorf("open S3 resource generation: %w", err)
	}

	cleanupGeneration := func(operationErr error) error {
		if cleanupErr := cleanupS3ResourceGeneration(baseStore, generation); cleanupErr != nil {
			return errors.Join(operationErr, fmt.Errorf("clean incomplete S3 resource generation: %w", cleanupErr))
		}

		return operationErr
	}

	var previousStore *export.S3Store
	if found {
		previousStore, err = newS3Store(
			c.context(),
			withS3LayoutPrefix(c.S3Destination, resourceGenerationPrefix(previousGeneration)),
		)
		if err != nil {
			return fmt.Errorf("open previous S3 resource generation: %w", err)
		}
	}

	root, err := os.MkdirTemp("", "kube-dump-s3-resources-")
	if err != nil {
		return fmt.Errorf("create temporary S3 backup directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	if previousStore != nil {
		if err := previousStore.Download(c.context(), root); err != nil {
			return fmt.Errorf("stage previous S3 resource generation: %w", err)
		}
	}

	prepared, err := prepareResourceCollection(
		c.resourceOptions,
		c.selection,
		root,
	)
	if err != nil {
		return fmt.Errorf("prepare resource backup: %w", err)
	}

	directorySink, err := export.NewDirectorySink(resourceRoot(root))
	if err != nil {
		return fmt.Errorf("open temporary S3 directory: %w", err)
	}

	compiled, result, err := collectResources(
		c.context(),
		directorySink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return err
	}
	if err := requireCompleteResourceCollection(result); err != nil {
		return err
	}
	if err := reconcileLocalResourceOwnership(
		resourceRoot(root),
		c.resourceOptions.Profile,
		result,
		c.Prune,
	); err != nil {
		return cleanupGeneration(fmt.Errorf("update staged resource ownership: %w", err))
	}
	if err := uploadResourceTree(c.context(), generationStore, root); err != nil {
		return cleanupGeneration(fmt.Errorf("upload S3 resource generation: %w", err))
	}

	pointerData, err := resourceGenerationPointerData(generation)
	if err != nil {
		return cleanupGeneration(err)
	}
	if err := baseStore.PutBytesIfMatch(
		c.context(),
		resourceCurrentPointerKey(),
		pointerData,
		"application/json",
		expectedPointer,
	); err != nil {
		return cleanupGeneration(fmt.Errorf("publish current S3 resource generation: %w", err))
	}

	if c.Prune {
		if err := pruneS3ResourceGenerations(c.context(), baseStore, generation); err != nil {
			log.Warn().
				Err(err).
				Str("component", "resource").
				Str("generation", generation).
				Msg("resource snapshot published but S3 generation retention failed")
		}
	}

	logResourceBackup("s3", c.URI, compiled, result, c.progress)

	return nil
}

// uploadResourceTree uploads the complete staged tree into one immutable S3 generation.
func uploadResourceTree(
	ctx context.Context,
	store *export.S3Store,
	root string,
) error {
	if store == nil || root == "" {
		return errors.New("S3 resource store and staging root are required")
	}

	return filepath.Walk(root, func(filename string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("inspect staged resource %q: %w", filename, err)
		}
		if info.IsDir() {
			return nil
		}

		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return fmt.Errorf("resolve staged resource path %q: %w", filename, err)
		}

		relative = filepath.ToSlash(relative)
		if relative != ".kube-dump" && !strings.HasPrefix(relative, ".kube-dump/") &&
			relative != resourceLayoutDirectory && !strings.HasPrefix(relative, resourceLayoutDirectory+"/") {
			return nil
		}

		if err := store.PutFile(ctx, path.Join(store.ObjectKey(), relative), filename); err != nil {
			return fmt.Errorf("upload staged resource %q: %w", relative, err)
		}

		return nil
	})
}

// fieldEncryptionOptions converts CLI inputs into the collector's field-encryption configuration.
// Without recipients, resource backup intentionally keeps selected fields plain.
func (o ResourceOptions) fieldEncryptionOptions(root string) (fieldcrypto.Options, error) {
	mode := o.FieldEncryption
	if mode == "" {
		mode = fieldcrypto.Age
	}
	if err := mode.Validate(); err != nil {
		return fieldcrypto.Options{}, err
	}

	if len(o.Recipients) == 0 &&
		len(o.RecipientsFiles) == 0 &&
		len(o.RecipientsURLs) == 0 &&
		len(o.RecipientGitHubUsers) == 0 {
		// No recipients means plaintext output for age mode.
		// AES-SIV is different: it still needs an age-wrapped keyring for its field key.
		if mode == fieldcrypto.AES256SIV {
			return fieldcrypto.Options{}, errors.New("AES-SIV field encryption requires at least one age recipient")
		}

		return fieldcrypto.Options{Mode: fieldcrypto.Plain}, nil
	}

	recipients, err := parseRecipients(
		o.Recipients,
		o.RecipientsFiles,
		o.RecipientsURLs,
		o.RecipientGitHubUsers,
	)
	if err != nil {
		return fieldcrypto.Options{}, fmt.Errorf("parse recipient: %w", err)
	}

	if mode == fieldcrypto.AES256SIV {
		// AES-SIV uses a local keyring so repeated backups can reuse the same deterministic field key.
		// Opening it here keeps key management outside the per-object collection loop.
		if root == "" {
			return fieldcrypto.Options{}, errors.New("AES-SIV field encryption requires a local backup root")
		}

		identities, err := loadConfiguredIdentities(o.IdentityOptions)
		if err != nil {
			return fieldcrypto.Options{}, fmt.Errorf("load AES-SIV identities: %w", err)
		}
		if err := agecrypto.ValidateRecipientsIdentities(recipients, identities); err != nil {
			return fieldcrypto.Options{}, fmt.Errorf(
				"validate AES-SIV recipient and identity: %w", err)
		}

		keys, err := keyring.Open(root, recipients, identities)
		if err != nil {
			return fieldcrypto.Options{}, fmt.Errorf("open AES-SIV keyring: %w", err)
		}

		keyID, cipher, err := keys.Active()
		if err != nil {
			return fieldcrypto.Options{}, err
		}

		return fieldcrypto.Options{
			Mode:      fieldcrypto.AES256SIV,
			SIVKeyID:  keyID,
			SIVCipher: cipher,
		}, nil
	}

	return fieldcrypto.Options{
		Mode:       fieldcrypto.Age,
		Recipients: recipients,
	}, nil
}

// preparedResourceCollection contains all local configuration needed
// before a Kubernetes client or destination side effect is created.
type preparedResourceCollection struct {
	// compiled is the validated profile used to normalize collected objects.
	compiled *profile.CompiledProfile
	// selection is the effective selection after profile defaults are merged.
	selection kube.Selection
	// fieldEncryption is the ready-to-use field encryption configuration.
	fieldEncryption fieldcrypto.Options
}

// prepareResourceCollection validates profiles, selection, and field encryption
// before resource collection can contact Kubernetes or write its destination.
func prepareResourceCollection(
	o ResourceOptions,
	selection kube.Selection,
	root string,
) (*preparedResourceCollection, error) {
	compiled, err := profile.LoadFrom(o.Profile, o.Directory)
	if err != nil {
		return nil, fmt.Errorf("load normalization profile: %w", err)
	}

	defaults := compiled.SelectionDefaults()
	selection, err = kube.EffectiveSelection(defaults, selection)
	if err != nil {
		return nil, fmt.Errorf("merge resource selection: %w", err)
	}

	fieldEncryption, err := o.fieldEncryptionOptions(root)
	if err != nil {
		return nil, fmt.Errorf("configure field encryption: %w", err)
	}

	log.Debug().
		Str("component", "backup").
		Str("operation", "prepare").
		Str("profile", compiled.Name()).
		Int("namespaces", len(selection.Namespaces)).
		Int("resources", len(selection.Resources)).
		Str("field_encryption", string(fieldEncryption.Mode)).
		Msg("Kubernetes resource backup prepared")

	return &preparedResourceCollection{
		compiled:        compiled,
		selection:       selection,
		fieldEncryption: fieldEncryption,
	}, nil
}

// collectResources creates the Kubernetes client and executes a prepared
// collection configuration.
func collectResources(
	ctx context.Context,
	sink kube.StateSink,
	manager *progress.Manager,
	o ResourceOptions,
	prepared *preparedResourceCollection,
	clientOptions kube.ClientOptions,
) (*profile.CompiledProfile, kube.CollectionResult, error) {
	if prepared == nil {
		return nil, kube.CollectionResult{}, errors.New("prepared resource collection is nil")
	}

	client, err := kube.NewClient(ctx, clientOptions)
	if err != nil {
		return nil, kube.CollectionResult{}, fmt.Errorf("create Kubernetes client: %w", err)
	}

	collector := kube.Collector{
		Dynamic:         client.Dynamic,
		Discovery:       client.Discovery,
		Sink:            sink,
		Profile:         prepared.compiled,
		Selection:       prepared.selection,
		FieldEncryption: prepared.fieldEncryption,
		Collection:      o.CollectionOptions,
	}

	observer := newResourceProgress(manager)
	collector.Observer = observer
	result, err := collector.Collect(ctx)
	observer.Close()
	if err != nil {
		return nil, result, fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := validateCollectionResult(result); err != nil {
		return nil, result, err
	}
	if result.Collected == 0 {
		log.Warn().
			Str("component", "backup").
			Str("operation", "collect").
			Str("profile", prepared.compiled.Name()).
			Int("listed", result.Listed).
			Int("selected", result.Selected).
			Int("collected", result.Collected).
			Msg("Kubernetes resource backup selected no objects")
	}

	return prepared.compiled, result, nil
}

// validateCollectionResult rejects a warning-only run that collected nothing.
// Non-strict mode still permits partial backups,
// but an unavailable API or unusable discovery cannot be reported as a successful empty backup.
func validateCollectionResult(result kube.CollectionResult) error {
	if result.WarningTotal() == 0 || result.Collected > 0 {
		return nil
	}

	details := append([]error(nil), result.Warnings...)
	if omitted := result.OmittedWarningDetails(); omitted > 0 {
		details = append(details, fmt.Errorf("%d warning details omitted", omitted))
	}

	return fmt.Errorf(
		"resource backup collected no objects: %w",
		errors.Join(details...),
	)
}

// writeBackupArchive packs a temporary canonical directory and publishes it atomically.
func writeBackupArchive(
	ctx context.Context,
	root, output string,
	format ArchiveFormat,
	recipients []age.Recipient,
	manager *progress.Manager,
) error {
	algorithm, encrypted, err := archiveConfig(format)
	if err != nil {
		return err
	}

	if encrypted && len(recipients) == 0 {
		return errors.New("encrypted archive output requires at least one recipient")
	}

	var counter *progress.Counter
	if manager != nil && manager.Enabled() {
		total, err := archive.MeasureDirectory(root)
		if err != nil {
			return fmt.Errorf("measure archive source: %w", err)
		}

		counter = manager.NewCounter("Archive bytes", total)
		defer counter.Complete()
	}

	var progress func(int64)
	if counter != nil {
		progress = counter.Add
	}
	config := archive.Config{
		Compression: compress.Config{Algorithm: algorithm},
		Progress:    progress,
	}

	return writeAtomic(output, func(destination io.Writer) error {
		if !encrypted {
			return archive.PackDirectory(ctx, root, destination, config)
		}

		writer, err := agecrypto.NewEncryptWriter(destination, recipients, agecrypto.EncryptOptions{})
		if err != nil {
			return err
		}
		packErr := archive.PackDirectory(ctx, root, writer, config)

		closeErr := writer.Close()
		if packErr != nil {
			return packErr
		}

		return closeErr
	})
}

// prepareArchiveRecipients validates the archive format and resolves all recipient sources
// before Kubernetes collection or archive staging begins.
func prepareArchiveRecipients(
	format ArchiveFormat,
	values, files, urls, githubUsers []string,
) ([]age.Recipient, error) {
	_, encrypted, err := archiveConfig(format)
	if err != nil {
		return nil, err
	}

	recipients, err := parseRecipients(values, files, urls, githubUsers)
	if err != nil {
		return nil, err
	}
	if encrypted && len(recipients) == 0 {
		return nil, errors.New("encrypted archive output requires at least one recipient")
	}

	return recipients, nil
}

// archiveConfig maps a CLI archive format to its codec and encryption mode.
func archiveConfig(format ArchiveFormat) (compress.Algorithm, bool, error) {
	switch format {
	case TarGZIPFormat:
		return compress.Gzip, false, nil

	case TarGZIPAgeFormat:
		return compress.Gzip, true, nil

	case TarZSTDFormat:
		return compress.Zstandard, false, nil

	case TarZSTDAgeFormat:
		return compress.Zstandard, true, nil

	default:
		return "", false, fmt.Errorf("unsupported archive output format %q", format)
	}
}

// archiveSuffix returns the filename suffix used for a selected archive format.
func archiveSuffix(format ArchiveFormat) string {
	switch format {
	case TarGZIPFormat:
		return ".tar.gz"

	case TarGZIPAgeFormat:
		return ".tar.gz.age"

	case TarZSTDAgeFormat:
		return ".tar.zst.age"

	default:
		return ".tar.zst"
	}
}

// logResourceBackup emits the common completion event for resource backends.
func logResourceBackup(
	backend, destination string,
	compiled *profile.CompiledProfile,
	result kube.CollectionResult,
	manager *progress.Manager,
) {
	warningCount := result.WarningTotal()
	for _, warning := range result.Warnings {
		log.Warn().
			Str("component", "backup").
			Str("destination", destination).
			Str("backend", backend).
			Str("profile", compiled.Name()).
			Err(warning).
			Msg("Kubernetes resource backup warning")
	}
	if omitted := result.OmittedWarningDetails(); omitted > 0 {
		log.Warn().
			Str("component", "backup").
			Str("destination", destination).
			Str("backend", backend).
			Str("profile", compiled.Name()).
			Int("warnings_omitted", omitted).
			Msg("Kubernetes resource backup warning details omitted")
	}

	// A live renderer is not enough to suppress the summary:
	// empty selections create no bars and still need an informational result in the log.
	progressActive := manager != nil && manager.Enabled() && result.Listed > 0
	event := completionLogEvent(progressActive)
	event.
		Str("component", "backup").
		Str("destination", destination).
		Str("backend", backend).
		Str("profile", compiled.Name()).
		Int("collected", result.Collected).
		Int("changed", result.Changed).
		Int("skipped", result.Skipped).
		Int("failed", result.Failed).
		Int("warnings", warningCount).
		Int("warning_details", len(result.Warnings)).
		Int("warnings_omitted", result.OmittedWarningDetails()).
		Bool("completed_with_warnings", warningCount > 0)

	if warningCount > 0 {
		event.Msg("Kubernetes resource backup completed with warnings")
		return
	}

	event.Msg("Kubernetes resource backup completed")
}

// newS3Store translates CLI S3 options into the export backend configuration.
func newS3Store(ctx context.Context, destination S3Destination) (*export.S3Store, error) {
	if err := export.ValidateEndpoint(destination.Endpoint, destination.Insecure); err != nil {
		return nil, err
	}

	secretKey, err := readS3SecretKey(destination)
	if err != nil {
		return nil, fmt.Errorf("read S3 secret key file: %w", err)
	}

	return export.NewS3Store(ctx, export.S3Options{
		URI:       destination.URI,
		Endpoint:  destination.Endpoint,
		Insecure:  destination.Insecure,
		Region:    destination.Region,
		Bucket:    destination.Bucket,
		Prefix:    destination.Prefix,
		AccessKey: destination.AccessKey,
		SecretKey: secretKey,
	})
}
