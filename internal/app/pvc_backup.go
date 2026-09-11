// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"sync"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var persistentVolumeClaims = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

const defaultPVCListPageSize int64 = 500

const maxPVCBackupErrors = 8

// pvcBackupFailure keeps a task error associated with its claim for stable aggregation.
type pvcBackupFailure struct {
	err   error   // err contains the task failure with its operation context.
	claim pvc.Ref // claim identifies the PVC whose task failed.
}

// pvcBackupResult contains the metrics produced by one successful claim task.
type pvcBackupResult struct {
	claim       pvc.Ref
	metadata    pvc.Metadata
	storedBytes int64
	storedKnown bool
}

// pvcBackupSummary aggregates source and destination sizes for one command.
type pvcBackupSummary struct {
	claims         int
	sourceBytes    int64
	storedBytes    int64
	storedKnown    bool
	encryptedClaim int
}

// backupPVCData writes selected PVC artifacts to a volume store.
//
// The collection and strategy selection stay independent from the destination
// so local and S3 destinations can reuse the same per-PVC pipeline.
func backupPVCData(
	ctx context.Context,
	destination string,
	runID string,
	options PvcSaveOptions,
	clientOptions kube.ClientOptions,
	manager *progress.Manager,
	remoteOptions *pvc.S3MoverOptions,
) (err error) {
	if destination == "" && remoteOptions == nil {
		return errors.New("PVC data destination is required")
	}

	recipientValues, err := loadRecipientValues(
		options.Recipients,
		options.RecipientsFiles,
		options.RecipientsURLs,
		options.RecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("load PVC data recipients: %w", err)
	}

	recipients, err := parseRecipientValues(recipientValues)
	if err != nil {
		return fmt.Errorf("parse PVC data recipients: %w", err)
	}

	client, err := kube.NewClient(ctx, clientOptions)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for PVC data: %w", err)
	}

	var store *pvc.VolumeStore
	if destination != "" {
		volumeStore, storeErr := pvc.NewVolumeStore(destination)
		if storeErr != nil {
			return fmt.Errorf("create PVC data store: %w", storeErr)
		}

		store = &volumeStore
	}

	var strategy pvc.BackupStrategy
	if remoteOptions == nil {
		// Local destinations use a client-mediated strategy
		// that writes the decoded stream into VolumeStore after the helper finishes exporting it.
		strategy, err = newPVCStrategies(client, options, recipients, runID)
		if err != nil {
			return err
		}
	} else {
		// Direct S3 keeps the data plane inside Kubernetes.
		// The parent process still builds the strategy and receives metadata/progress from the helper.
		remoteOptions.Dynamic = client.Dynamic
		remoteOptions.Image = options.Image
		remoteOptions.ImagePullPolicy = options.ImagePullPolicy
		remoteOptions.AgentBinary = options.HelperBinaryPath
		remoteOptions.Compression = options.Compression
		remoteOptions.Recipients = recipientValues
		remoteOptions.RunID = runID
		remoteOptions.RetryAttempts = retry.Attempts(ctx)
		strategy, err = newPVCRemoteStrategies(client, options, remoteOptions)
		if err != nil {
			return err
		}
	}

	compiled, err := profile.LoadFrom(options.Profile, options.Directory)
	if err != nil {
		return fmt.Errorf("load PVC profile: %w", err)
	}

	namespaces := options.Namespaces
	if len(namespaces) == 0 {
		namespaces = compiled.PVCSelectionDefaults().Namespaces
	}

	patterns := options.PVCs
	if len(patterns) == 0 {
		patterns = compiled.PVCSelectionDefaults().Names
	}

	claims, err := listPVCs(ctx, client.Dynamic, namespaces, patterns, compiled)
	if err != nil {
		return err
	}

	claims, err = filterPVCSizeLimits(ctx, client.Dynamic, claims, pvc.SizeLimits{
		MaxPVCBytes:       int64(options.MaxSize),
		MaxNamespaceBytes: int64(options.MaxNamespaceSize),
	})
	if err != nil {
		return err
	}

	if err := pvc.ValidateBackupQuotas(
		ctx,
		client.Dynamic,
		claims,
		options.Strategy,
		options.Concurrency,
		remoteOptions != nil && (remoteOptions.AccessKey != "" || remoteOptions.SecretKey != ""),
	); err != nil {
		return err
	}

	if len(claims) == 0 {
		log.Warn().
			Str("component", "pvc").
			Str("operation", "prepare").
			Str("profile", compiled.Name()).
			Str("strategy", string(options.Strategy)).
			Int("namespaces", len(namespaces)).
			Int("claims", len(claims)).
			Msg("PVC data backup selected no claims")
	}

	log.Debug().
		Str("component", "pvc").
		Str("operation", "prepare").
		Str("profile", compiled.Name()).
		Str("strategy", string(options.Strategy)).
		Int("namespaces", len(namespaces)).
		Int("claims", len(claims)).
		Msg("PVC data backup prepared")

	progressItems := make([]string, 0, len(claims))
	for _, claim := range claims {
		progressItems = append(progressItems, claim.Namespace+"/"+claim.Name)
	}

	batch := manager.NewBatch("PVCs", progressItems)
	defer func() { batch.Close(err) }()

	summary, err := backupPVCClaims(ctx, store, claims, strategy, options.Concurrency, batch)
	if err != nil {
		return err
	}

	if manager.Enabled() {
		message := fmt.Sprintf(
			"PVC data saved: %d claims; source: %s; encrypted: %d",
			summary.claims,
			formatBytes(summary.sourceBytes),
			summary.encryptedClaim,
		)
		if summary.storedKnown {
			message += "; stored: " + formatBytes(summary.storedBytes)
		}
		manager.AddFinalMessage(message)
	} else {
		event := log.Info().
			Str("component", "pvc").
			Int("claims", summary.claims).
			Int64("source_size_bytes", summary.sourceBytes).
			Int("encrypted_claims", summary.encryptedClaim)
		if summary.storedKnown {
			event = event.Int64("stored_size_bytes", summary.storedBytes)
		}
		event.Msg("PVC data backup completed")
	}

	return nil
}

// backupPVCClaims runs independent pipelines through a bounded worker pool.
// The lifecycle limit prevents thousands of simultaneous snapshot/clone API operations,
// while the strategy's transfer limiter separately bounds streams.
func backupPVCClaims(
	ctx context.Context,
	store *pvc.VolumeStore,
	claims []pvc.Ref,
	strategy pvc.BackupStrategy,
	concurrency int,
	batch *progress.Batch,
) (pvcBackupSummary, error) {
	if ctx == nil {
		return pvcBackupSummary{}, errors.New("PVC backup context is required")
	}
	if concurrency <= 0 {
		return pvcBackupSummary{}, errors.New("PVC lifecycle concurrency must be positive")
	}

	failures := make(chan pvcBackupFailure, len(claims))
	results := make(chan pvcBackupResult, len(claims))

	workers := min(len(claims), concurrency)
	if workers == 0 {
		return pvcBackupSummary{}, nil
	}

	jobs := make(chan pvc.Ref)
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)
	for range workers {
		go func() {
			defer waitGroup.Done()
			for {
				select {
				case <-ctx.Done():
					return

				case claim, open := <-jobs:
					if !open {
						return
					}

					result, err := backupPVCClaim(
						ctx,
						store,
						claim,
						strategy,
						batch,
					)
					if err != nil {
						failures <- pvcBackupFailure{claim: claim, err: err}
						continue
					}
					results <- result
				}
			}
		}()
	}

sendClaims:
	for _, claim := range claims {
		select {
		case jobs <- claim:
		case <-ctx.Done():
			break sendClaims
		}
	}
	close(jobs)
	waitGroup.Wait()
	close(failures)
	close(results)

	if err := ctx.Err(); err != nil {
		return pvcBackupSummary{}, err
	}

	// Aggregate successful results separately from failures
	// so a non-strict multi-PVC run can report every completed claim and every failed claim.
	summary := pvcBackupSummary{}
	for result := range results {
		summary.claims++
		summary.sourceBytes += result.metadata.SizeBytes
		if result.metadata.Encrypted {
			summary.encryptedClaim++
		}
		if result.storedKnown {
			summary.storedKnown = true
			summary.storedBytes += result.storedBytes
		}
	}

	failed := make([]pvcBackupFailure, 0, len(failures))
	for failure := range failures {
		failed = append(failed, failure)
	}
	if len(failed) == 0 {
		return summary, nil
	}

	// Sort failures because worker completion order is nondeterministic.
	// Stable error order makes CLI output and tests reproducible.
	sort.Slice(failed, func(left, right int) bool {
		return failed[left].claim.Namespace+"/"+failed[left].claim.Name <
			failed[right].claim.Namespace+"/"+failed[right].claim.Name
	})

	errorsList := make([]error, 0, min(len(failed), maxPVCBackupErrors)+1)
	for index, failure := range failed {
		if index == maxPVCBackupErrors {
			break
		}

		errorsList = append(errorsList, failure.err)
	}

	if omitted := len(failed) - len(errorsList); omitted > 0 {
		errorsList = append(errorsList, fmt.Errorf(
			"%d additional PVC failure details omitted",
			omitted,
		))
	}

	return summary, fmt.Errorf(
		"PVC backup failed for %d of %d claims: %w",
		len(failed),
		len(claims),
		errors.Join(errorsList...),
	)
}

// backupPVCClaim runs one complete PVC pipeline and updates its progress row.
func backupPVCClaim(
	ctx context.Context,
	store *pvc.VolumeStore,
	claim pvc.Ref,
	strategy pvc.BackupStrategy,
	batch *progress.Batch,
) (pvcBackupResult, error) {
	progressItem := claim.Namespace + "/" + claim.Name
	claimContext := pvc.WithProgress(ctx, func(event pvc.ProgressEvent) {
		batch.SetStage(progressItem, string(event.Stage))
		batch.SetProgress(progressItem, event.Percent)
	})
	if strategy == nil {
		batch.Fail(progressItem)
		return pvcBackupResult{}, errors.New("PVC backup strategy is not configured")
	}

	var (
		metadata    pvc.Metadata
		storedBytes int64
		storedKnown bool
		err         error
	)

	if store == nil {
		// The S3 mover publishes from the helper Pod, so it does not use a local writer.
		metadata, err = strategy.Backup(claimContext, claim, io.Discard)
	} else {
		metadata, storedBytes, err = store.WriteWithStats(claimContext, claim, strategy)
		storedKnown = err == nil || pvc.IsCleanupOnly(err)
	}
	if err != nil && !pvc.IsCleanupOnly(err) {
		batch.Fail(progressItem)
		return pvcBackupResult{}, fmt.Errorf("back up PVC %s/%s: %w", claim.Namespace, claim.Name, err)
	}

	if err != nil {
		log.Warn().
			Str("component", "pvc").
			Str("namespace", claim.Namespace).
			Str("pvc", claim.Name).
			Err(err).
			Msg("PVC data cleanup warning")
	}

	completionLogEvent(batch.Enabled()).
		Str("component", "pvc").
		Str("namespace", claim.Namespace).
		Str("pvc", claim.Name).
		Str("strategy", string(metadata.Strategy)).
		Int64("size_bytes", metadata.SizeBytes).
		Msg("PVC data backup completed")

	batch.Complete(progressItem)
	return pvcBackupResult{
		claim:       claim,
		metadata:    metadata,
		storedBytes: storedBytes,
		storedKnown: storedKnown,
	}, nil
}

// newPVCStrategies creates the strategy graph once per command invocation.
func newPVCStrategies(
	client *kube.Client,
	options PvcSaveOptions,
	recipients []age.Recipient,
	runID string,
) (pvc.BackupStrategy, error) {
	if client == nil || client.Dynamic == nil {
		return nil, errors.New("PVC Kubernetes client is required")
	}

	direct, err := pvc.NewPodStrategy(pvc.PodStrategyOptions{
		Dynamic:     client.Dynamic,
		Exec:        pvc.RESTExecTransport{Config: client.Config},
		Image:       options.Image,
		Compression: options.Compression,
		Recipients:  recipients,
		Helper: pvc.HelperPodOptions{
			AgentBinary:     options.HelperBinaryPath,
			ImagePullPolicy: options.ImagePullPolicy,
			RunID:           runID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create PVC Pod strategy: %w", err)
	}

	snapshotClient := pvc.SnapshotClient{
		Dynamic:   client.Dynamic,
		Discovery: client.Discovery,
		ClassName: options.SnapshotClass,
		RunID:     runID,
	}
	snapshotCopy := pvc.SnapshotCopyStrategy{
		BackupStrategy: direct,
		Snapshots:      snapshotClient,
		NamePrefix:     "kube-dump",
	}

	var selected pvc.BackupStrategy
	switch options.Strategy {
	case pvc.Pod:
		selected = direct
	case pvc.SnapshotCopy:
		selected = snapshotCopy
	default:
		return nil, fmt.Errorf("unsupported PVC data strategy %q", options.Strategy)
	}

	return selected, nil
}

// newPVCRemoteStrategies composes a direct S3 mover with the optional snapshot-copy lifecycle.
// The mover always streams from its mounted PVC;
// SnapshotCopyStrategy changes only the source PVC and resulting metadata.
func newPVCRemoteStrategies(
	client *kube.Client,
	options PvcSaveOptions,
	remoteOptions *pvc.S3MoverOptions,
) (pvc.BackupStrategy, error) {
	if client == nil || client.Dynamic == nil {
		return nil, errors.New("PVC Kubernetes client is required")
	}
	if remoteOptions == nil {
		return nil, errors.New("remote PVC options are required")
	}

	mover, err := pvc.NewS3Mover(*remoteOptions)
	if err != nil {
		return nil, fmt.Errorf("create PVC S3 mover: %w", err)
	}
	if options.Strategy == pvc.Pod {
		return mover, nil
	}
	if options.Strategy != pvc.SnapshotCopy {
		return nil, fmt.Errorf("unsupported PVC data strategy %q", options.Strategy)
	}

	snapshotCopy := pvc.SnapshotCopyStrategy{
		BackupStrategy: mover,
		Snapshots: pvc.SnapshotClient{
			Dynamic:   client.Dynamic,
			Discovery: client.Discovery,
			ClassName: options.SnapshotClass,
			RunID:     remoteOptions.RunID,
		},
		NamePrefix: "kube-dump",
	}

	return snapshotCopy, nil
}

// listPVCs lists and deterministically filters PVC references for one run.
func listPVCs(
	ctx context.Context,
	client dynamic.Interface,
	namespaces, patterns []string,
	compiled *profile.CompiledProfile,
) ([]pvc.Ref, error) {
	if client == nil {
		return nil, errors.New("PVC dynamic client is required")
	}
	if compiled == nil {
		return nil, errors.New("PVC profile is required")
	}
	if err := validatePVCNamePatterns(patterns); err != nil {
		return nil, err
	}

	namesToList := []string{metav1.NamespaceAll}
	if len(namespaces) > 0 && namespacePatternsAreExact(namespaces) {
		// Exact namespace filters can be pushed into namespaced API requests,
		// avoiding a cluster-wide PVC list. Glob patterns still require the broad scan.
		allowedNamespaces := make(map[string]struct{}, len(namespaces))
		for _, namespace := range namespaces {
			allowedNamespaces[namespace] = struct{}{}
		}

		namesToList = namesToList[:0]
		for namespace := range allowedNamespaces {
			namesToList = append(namesToList, namespace)
		}
		sort.Strings(namesToList)
	}

	var claims []pvc.Ref
	listOptions := metav1.ListOptions{LabelSelector: compiled.PVCLabelSelector()}
	for _, namespace := range namesToList {
		continueToken := ""
		for {
			pageOptions := listOptions
			pageOptions.Continue = continueToken
			pageOptions.Limit = defaultPVCListPageSize
			list, err := client.Resource(persistentVolumeClaims).Namespace(namespace).List(ctx, pageOptions)
			if err != nil {
				return nil, fmt.Errorf("list PVCs in namespace %q: %w", namespace, err)
			}

			for _, item := range list.Items {
				// Label filtering is server-side;
				// names, annotations, and glob patterns are evaluated locally
				// because the profile matcher owns their semantics.
				if !compiled.MatchesPVCInScope(
					namespaces,
					patterns,
					item.GetNamespace(),
					item.GetName(),
					item.GetLabels(),
					item.GetAnnotations(),
				) {
					continue
				}

				claims = append(claims, pvc.Ref{Namespace: item.GetNamespace(), Name: item.GetName()})
			}

			continueToken = list.GetContinue()
			if continueToken == "" {
				break
			}
		}
	}

	// Kubernetes does not guarantee list order;
	// sort before creating progress rows and helper work so repeated runs remain visually stable.
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].Namespace+"/"+claims[i].Name < claims[j].Namespace+"/"+claims[j].Name
	})

	return claims, nil
}

// validatePVCNamePatterns rejects malformed shell globs before any API request.
func validatePVCNamePatterns(patterns []string) error {
	for _, pattern := range patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid PVC name pattern %q: %w", pattern, err)
		}
	}

	return nil
}

// filterPVCSizeLimits measures selected claims once
// and removes claims that exceed either configured limit
// before any helper Pod or snapshot is created.
func filterPVCSizeLimits(
	ctx context.Context,
	client dynamic.Interface,
	claims []pvc.Ref,
	limits pvc.SizeLimits,
) ([]pvc.Ref, error) {
	if limits.MaxPVCBytes <= 0 && limits.MaxNamespaceBytes <= 0 {
		return claims, nil
	}

	sizes := make(map[string]int64, len(claims))
	namespaceTotals := make(map[string]int64)

	for _, claim := range claims {
		size, err := pvc.MeasureSize(ctx, client, claim)
		if err != nil {
			return nil, fmt.Errorf("measure PVC %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		sizes[claim.Namespace+"\x00"+claim.Name] = size
		namespaceTotals[claim.Namespace] += size
	}

	selected := make([]pvc.Ref, 0, len(claims))
	for _, claim := range claims {
		size := sizes[claim.Namespace+"\x00"+claim.Name]
		skip, reason := limits.ShouldSkip(size, namespaceTotals[claim.Namespace])

		if skip {
			log.Info().
				Str("component", "pvc").
				Str("namespace", claim.Namespace).
				Str("pvc", claim.Name).
				Int64("size_bytes", size).
				Str("reason", reason).
				Msg("PVC data backup skipped")
			continue
		}

		selected = append(selected, claim)
	}

	return selected, nil
}
