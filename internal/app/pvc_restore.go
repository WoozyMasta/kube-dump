// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/version"
)

// restoreCountReader counts the uncompressed payload while it is streamed into a PVC.
type restoreCountReader struct {
	reader io.Reader // reader is the decoded archive payload stream.
	bytes  int64     // bytes is the number of decoded payload bytes returned to the helper strategy.
}

// restoreContextReader makes local archive spooling observe cancellation between reads.
type restoreContextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read checks cancellation before reading the next source chunk.
func (r *restoreContextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

// Execute restores one local PVC artifact into an existing target PVC.
func (c *PvcRestoreDirCommand) Execute(_ []string) error {
	return restorePVCFromDirectory(
		c.context(),
		c.Path,
		c.SourcePVC,
		c.TargetPVC,
		c.Revision,
		c.PvcRestoreOptions,
		c.clientOptions,
		c.progress,
	)
}

// Execute restores one S3 PVC artifact into an existing target PVC.
func (c *PvcRestoreS3Command) Execute(_ []string) (err error) {
	// Resolve the source artifact before touching the target cluster.
	if c.URI == "" {
		return errors.New("S3 PVC source is required")
	}

	source, err := parsePVCReference(c.SourcePVC, "source PVC")
	if err != nil {
		return err
	}
	target, err := parsePVCReference(c.TargetPVC, "target PVC")
	if err != nil {
		return err
	}
	if err := c.Existing.Validate(); err != nil {
		return err
	}

	// Metadata is small and identifies the exact data object without downloading it.
	store, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 PVC source: %w", err)
	}
	dataKey, metadata, err := findS3PVCArtifact(c.context(), store, source, c.Revision)
	if err != nil {
		return fmt.Errorf("select S3 PVC artifact: %w", err)
	}
	secretKey, err := readS3SecretKey(c.S3Destination)
	if err != nil {
		return fmt.Errorf("read S3 PVC credentials: %w", err)
	}
	identity, passphrase, err := loadRestoreIdentityMaterial(c.IdentityOptions, metadata.Encrypted)
	if err != nil {
		return err
	}
	client, err := kube.NewClient(c.context(), c.clientOptions)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for S3 PVC restore: %w", err)
	}

	// The Pod may need a different endpoint than the local coordinator.
	moverEndpoint := c.Endpoint
	if c.PodEndpoint != "" {
		moverEndpoint = c.PodEndpoint
	}

	if err := pvc.WithRestoreLock(c.context(), client.Config, target, func(restoreContext context.Context) (callbackErr error) {
		targetIdentity, err := pvc.ValidateRestoreTarget(restoreContext, client.Dynamic, target, metadata.SizeBytes)
		if err != nil {
			return fmt.Errorf("validate S3 PVC restore target: %w", err)
		}

		restorer, err := pvc.NewS3Restorer(pvc.S3RestorerOptions{
			Dynamic:                client.Dynamic,
			Image:                  c.Image,
			ImagePullPolicy:        c.ImagePullPolicy,
			AgentBinary:            c.HelperBinaryPath,
			URI:                    c.URI,
			Endpoint:               moverEndpoint,
			Insecure:               c.Insecure,
			Region:                 c.Region,
			Object:                 dataKey,
			Compression:            compress.Algorithm(metadata.Compression),
			Existing:               c.Existing,
			ExpectedSize:           metadata.SizeBytes,
			ExpectedTargetIdentity: &targetIdentity,
			ExpectedTargetSize:     metadata.SizeBytes,
			ExpectedSHA256:         metadata.ContentSHA256,
			Encrypted:              metadata.Encrypted,
			Identity:               identity,
			IdentityPassphrase:     passphrase,
			AccessKey:              c.AccessKey,
			SecretKey:              secretKey,
		})
		if err != nil {
			return fmt.Errorf("create S3 PVC restorer: %w", err)
		}

		item := target.Namespace + "/" + target.Name
		batch := c.progress.NewBatch("PVC restore", []string{item})
		defer func() { batch.Close(callbackErr) }()

		// Only lifecycle events cross the parent process; the data stream stays in the cluster.
		progressContext := pvc.WithProgress(restoreContext, func(event pvc.ProgressEvent) {
			batch.SetStage(item, string(event.Stage))
			batch.SetProgress(item, event.Percent)
		})
		if err := pvcRestoreResult(target, restorer.Restore(progressContext, target)); err != nil {
			batch.Fail(item)
			return fmt.Errorf("restore PVC %s/%s: %w", target.Namespace, target.Name, err)
		}

		if c.progress.Enabled() {
			c.progress.AddFinalMessage(fmt.Sprintf(
				"PVC restored: %s/%s; size: %s",
				target.Namespace, target.Name, formatBytes(metadata.SizeBytes)))
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// restorePVCFromDirectory selects a local artifact, validates the target PVC,
// and streams the decoded archive through a helper Pod.
func restorePVCFromDirectory(
	ctx context.Context,
	root, sourceText, targetText, revision string,
	restoreOptions PvcRestoreOptions,
	clientOptions kube.ClientOptions,
	manager *progress.Manager,
) error {
	// Select and open the exact revision before creating any Kubernetes helper resources.
	source, err := parsePVCReference(sourceText, "source PVC")
	if err != nil {
		return err
	}
	target, err := parsePVCReference(targetText, "target PVC")
	if err != nil {
		return err
	}
	if err := restoreOptions.Existing.Validate(); err != nil {
		return err
	}

	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		return fmt.Errorf("open PVC artifact store: %w", err)
	}
	artifact, err := store.FindArtifact(source, revision)
	if err != nil {
		return fmt.Errorf("select PVC artifact: %w", err)
	}
	data, err := store.OpenData(artifact)
	if err != nil {
		return fmt.Errorf("open PVC artifact: %w", err)
	}
	defer func() { _ = data.Close() }()

	client, err := kube.NewClient(ctx, clientOptions)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for PVC restore: %w", err)
	}

	// The selected metadata supplies the payload size used by target preflight.
	return pvc.WithRestoreLock(ctx, client.Config, target, func(restoreContext context.Context) error {
		strategy, err := newPVCRestoreStrategy(
			restoreContext,
			target,
			artifact.Metadata.SizeBytes,
			restoreOptions,
			client,
		)
		if err != nil {
			return err
		}

		return restorePVCStream(
			restoreContext,
			target,
			data,
			artifact.Metadata,
			restoreOptions,
			strategy,
			manager,
		)
	})
}

// loadRestoreIdentityMaterial validates configured identities
// and returns the original identity text plus the optional passphrase for a helper Pod.
func loadRestoreIdentityMaterial(options IdentityOptions, encrypted bool) ([]byte, string, error) {
	if !encrypted {
		return nil, "", nil
	}
	if _, err := loadConfiguredIdentities(options); err != nil {
		return nil, "", fmt.Errorf("load PVC restore identity: %w", err)
	}

	// Preserve the original identity file syntax for the helper instead of reserializing keys.
	var identity bytes.Buffer
	for _, filename := range options.IdentityFiles {
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, "", fmt.Errorf("read PVC restore identity %q: %w", filename, err)
		}

		identity.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			identity.WriteByte('\n')
		}
	}

	passphrase := options.IdentityPassphrase
	if options.IdentityPassphraseFile != "" {
		data, err := os.ReadFile(options.IdentityPassphraseFile)
		if err != nil {
			return nil, "", fmt.Errorf("read PVC restore identity passphrase: %w", err)
		}

		passphrase = strings.TrimSpace(string(data))
	}

	return identity.Bytes(), passphrase, nil
}

// newPVCRestoreStrategy creates the Kubernetes-backed stream strategy after target preflight.
func newPVCRestoreStrategy(
	ctx context.Context,
	target pvc.Ref,
	requiredBytes int64,
	options PvcRestoreOptions,
	client *kube.Client,
) (*pvc.PodStrategy, error) {
	if client == nil {
		return nil, errors.New("PVC restore Kubernetes client is required")
	}

	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("validate PVC restore target: %w", err)
	}
	targetIdentity, err := pvc.ValidateRestoreTarget(ctx, client.Dynamic, target, requiredBytes)
	if err != nil {
		return nil, fmt.Errorf("validate PVC restore target: %w", err)
	}

	if options.Image == "" {
		options.Image = version.ContainerImage()
	}

	strategy, err := pvc.NewPodStrategy(pvc.PodStrategyOptions{
		Dynamic: client.Dynamic,
		Exec:    pvc.RESTExecTransport{Config: client.Config},
		Image:   options.Image,
		Helper: pvc.HelperPodOptions{
			AgentBinary:            options.HelperBinaryPath,
			ExpectedTargetIdentity: &targetIdentity,
			ExpectedTargetSize:     requiredBytes,
			ImagePullPolicy:        options.ImagePullPolicy,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create PVC restore strategy: %w", err)
	}

	return strategy, nil
}

// restorePVCStream validates a local artifact before mutation,
// then verifies that the helper consumed the same complete decoded payload.
func restorePVCStream(
	ctx context.Context,
	target pvc.Ref,
	data io.ReadSeeker,
	metadata pvc.Metadata,
	options PvcRestoreOptions,
	strategy *pvc.PodStrategy,
	manager *progress.Manager,
) error {
	stable, err := spoolRestoreArtifact(ctx, data)
	if err != nil {
		return fmt.Errorf("spool PVC artifact before restore: %w", err)
	}
	defer func() {
		name := stable.Name()
		_ = stable.Close()
		_ = os.Remove(name)
	}()

	var identities []age.Identity
	if metadata.Encrypted {
		identities, err = loadConfiguredIdentities(options.IdentityOptions)
		if err != nil {
			return fmt.Errorf("load PVC artifact identities: %w", err)
		}
	}

	// Validate the complete decoded archive before the helper can mutate the target PVC.
	// Both passes use the private spool, so changes to the selected artifact
	// cannot replace the bytes after validation and before helper execution.
	if err := validateLocalPVCStream(ctx, stable, metadata, identities); err != nil {
		return fmt.Errorf("validate PVC artifact before restore: %w", err)
	}
	if _, err := stable.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind PVC artifact: %w", err)
	}

	decoded, err := openDecodedPVCStream(stable, metadata, identities)
	if err != nil {
		return err
	}
	defer func() { _ = decoded.Close() }()

	hash := sha256.New()
	count := &restoreCountReader{reader: io.TeeReader(decoded, hash)}

	// Count decoded bytes because metadata.SizeBytes describes the uncompressed payload.
	if manager != nil && manager.Enabled() {
		counter := manager.NewCounter("PVC restore", metadata.SizeBytes)
		count.reader = progress.TrackReader(count.reader, counter)
		defer counter.Complete()
	}

	restoreErr := strategy.Restore(ctx, target, count, options.Existing)
	if restoreErr != nil && !pvc.IsCleanupOnly(restoreErr) {
		return restoreErr
	}

	if err := validateRestoredMetadata(metadata, pvc.ArchiveStats{
		SizeBytes:     count.bytes,
		ContentSHA256: hex.EncodeToString(hash.Sum(nil)),
	}); err != nil {
		return errors.Join(err, restoreErr)
	}

	return pvcRestoreResult(target, restoreErr)
}

// spoolRestoreArtifact copies the selected local artifact to a private stable file.
// Validation and helper streaming then read the same bytes without buffering the archive in memory.
func spoolRestoreArtifact(ctx context.Context, source io.ReadSeeker) (*os.File, error) {
	if ctx == nil || source == nil {
		return nil, errors.New("PVC restore spool context and source are required")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind PVC artifact for spooling: %w", err)
	}

	spool, err := os.CreateTemp("", "kube-dump-pvc-restore-*")
	if err != nil {
		return nil, fmt.Errorf("create PVC restore spool: %w", err)
	}

	remove := true
	defer func() {
		if remove {
			_ = spool.Close()
			_ = os.Remove(spool.Name())
		}
	}()

	if _, err := io.Copy(spool, &restoreContextReader{ctx: ctx, reader: source}); err != nil {
		return nil, fmt.Errorf("copy PVC artifact to spool: %w", err)
	}
	if err := spool.Sync(); err != nil {
		return nil, fmt.Errorf("sync PVC restore spool: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind PVC restore spool: %w", err)
	}

	remove = false
	return spool, nil
}

// pvcRestoreResult keeps cleanup failures visible
// without turning an already completed destructive restore into a retryable command failure.
func pvcRestoreResult(target pvc.Ref, err error) error {
	if err == nil || !pvc.IsCleanupOnly(err) {
		return err
	}

	log.Warn().
		Str("component", "pvc").
		Str("namespace", target.Namespace).
		Str("pvc", target.Name).
		Err(err).
		Msg("PVC restore cleanup warning")

	return nil
}

// validateLocalPVCStream validates one complete local artifact pass without touching Kubernetes.
func validateLocalPVCStream(
	ctx context.Context,
	data io.ReadSeeker,
	metadata pvc.Metadata,
	identities []age.Identity,
) error {
	if _, err := data.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind PVC artifact for validation: %w", err)
	}

	decoded, err := openDecodedPVCStream(data, metadata, identities)
	if err != nil {
		return err
	}
	stats, validationErr := pvc.ValidateVolumeArchive(ctx, decoded)
	closeErr := decoded.Close()
	if validationErr != nil {
		return validationErr
	}
	if closeErr != nil {
		return fmt.Errorf("close PVC artifact validation stream: %w", closeErr)
	}

	return validateRestoredMetadata(metadata, stats)
}

// openDecodedPVCStream creates a fresh decrypting and decompressing view of an artifact.
func openDecodedPVCStream(
	data io.Reader,
	metadata pvc.Metadata,
	identities []age.Identity,
) (io.ReadCloser, error) {
	sourceReader, err := agecrypto.MaybeDecryptReader(data, identities)
	if err != nil {
		return nil, fmt.Errorf("decrypt PVC artifact: %w", err)
	}

	decoded, err := compress.NewReader(sourceReader, compress.Algorithm(metadata.Compression))
	if err != nil {
		return nil, fmt.Errorf("open PVC artifact compression: %w", err)
	}

	return decoded, nil
}

// findS3PVCArtifact resolves one metadata/data pair without downloading the payload locally.
func findS3PVCArtifact(ctx context.Context, store *export.S3Store, claim pvc.Ref, revision string) (string, pvc.Metadata, error) {
	if store == nil {
		return "", pvc.Metadata{}, errors.New("S3 PVC store is required")
	}

	objects, err := store.ListStoredObjects(ctx)
	if err != nil {
		return "", pvc.Metadata{}, fmt.Errorf("list S3 PVC artifacts: %w", err)
	}

	objectsByKey := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		objectsByKey[object.Key] = struct{}{}
	}

	base := path.Join(
		store.ObjectKey(),
		"volumes",
		state.EncodePathSegment(claim.Namespace),
		state.EncodePathSegment(claim.Name),
	)
	requested := strings.TrimSpace(revision)
	latest := requested == "" || requested == "latest"

	// candidate links one revision's metadata object with its derived data object.
	type candidate struct {
		timestamp string
		dataKey   string
		metadata  pvc.Metadata
	}
	candidates := make([]candidate, 0)
	skipped := make([]string, 0, 4)

	for _, object := range objects {
		// Metadata is the revision index; encryption determines the data filename suffix.
		boundary := base + "/"
		if !strings.HasPrefix(object.Key, boundary) {
			continue
		}
		relative := strings.TrimPrefix(object.Key, boundary)
		parts := strings.Split(relative, "/")
		if len(parts) != 2 || parts[1] != pvc.MetadataFilename || !pvc.IsTimestamp(parts[0]) {
			continue
		}
		if !latest && parts[0] != requested {
			continue
		}

		metadata, err := readS3PVCMetadata(ctx, store, object.Key)
		if err != nil {
			if !latest {
				return "", pvc.Metadata{}, fmt.Errorf("read S3 PVC metadata %q: %w", object.Key, err)
			}

			if len(skipped) < 4 {
				skipped = append(skipped, object.Key+": "+err.Error())
			}
			continue
		}

		dataKey := path.Join(base, parts[0], pvc.DataFilename(metadata.Compression, metadata.Encrypted))
		if _, found := objectsByKey[dataKey]; !found {
			if !latest {
				return "", pvc.Metadata{}, fmt.Errorf("S3 PVC artifact data %q is missing", dataKey)
			}

			if len(skipped) < 4 {
				skipped = append(skipped, dataKey+": data object is missing")
			}
			continue
		}

		candidates = append(candidates, candidate{
			timestamp: parts[0],
			dataKey:   dataKey,
			metadata:  metadata,
		})
	}

	if len(candidates) == 0 {
		if latest {
			if len(skipped) > 0 {
				return "", pvc.Metadata{}, fmt.Errorf(
					"no valid S3 PVC artifacts found for %s/%s; skipped: %s",
					claim.Namespace,
					claim.Name,
					strings.Join(skipped, "; "))
			}

			return "", pvc.Metadata{}, fmt.Errorf("no S3 PVC artifacts found for %s/%s", claim.Namespace, claim.Name)
		}

		return "", pvc.Metadata{}, fmt.Errorf("S3 PVC artifact %s/%s/%s was not found", claim.Namespace, claim.Name, requested)
	}

	sort.Slice(candidates, func(left, right int) bool {
		return pvc.CompareRevisions(candidates[left].timestamp, candidates[right].timestamp) > 0
	})

	return candidates[0].dataKey, candidates[0].metadata, nil
}

// Read forwards decoded payload bytes and records the number consumed.
func (r *restoreCountReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

// parsePVCReference parses and validates the namespace/name form used by restore flags.
func parsePVCReference(value, label string) (pvc.Ref, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return pvc.Ref{}, fmt.Errorf("%s must use NAMESPACE/NAME", label)
	}

	ref := pvc.Ref{Namespace: parts[0], Name: parts[1]}
	if err := ref.Validate(); err != nil {
		return pvc.Ref{}, fmt.Errorf("validate %s: %w", label, err)
	}

	return ref, nil
}

// validateRestoredMetadata verifies decoded archive statistics against metadata.
func validateRestoredMetadata(metadata pvc.Metadata, stats pvc.ArchiveStats) error {
	if metadata.SizeBytes != stats.SizeBytes {
		return fmt.Errorf("restored PVC payload size %d does not match metadata %d", stats.SizeBytes, metadata.SizeBytes)
	}

	if metadata.ContentSHA256 != stats.ContentSHA256 {
		return errors.New("restored PVC payload checksum does not match metadata")
	}

	return nil
}
