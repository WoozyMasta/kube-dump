// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/images"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"github.com/woozymasta/orascope"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

var secretsResource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

const maxImageCaptureErrors = 8

// Execute captures discovered container images into a local OCI Image Layout.
func (c *ImagesDirCommand) Execute(_ []string) error {
	if c.Path == "" {
		return errors.New("image layout destination is required")
	}

	return captureImagesToDirectory(
		c.context(),
		imageRoot(c.Path),
		c.imageOptions,
		c.clientOptions,
		c.progress,
		c.Path,
		c.Prune,
	)
}

// captureImagesToDirectory captures a complete layout beside the destination
// and publishes its index only after every referenced blob has been validated.
func captureImagesToDirectory(
	ctx context.Context,
	root string,
	options ImageOptions,
	clientOptions kube.ClientOptions,
	manager *progress.Manager,
	destination string,
	prune bool,
) error {
	writerLock, err := acquireImageWriterLock(ctx, root)
	if err != nil {
		return err
	}
	defer func() { _ = writerLock.Close() }()

	staging, err := newImageLayoutStaging(root)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if !prune {
		if err := seedImageLayout(root, staging); err != nil {
			return fmt.Errorf("preserve existing image layout: %w", err)
		}
	}

	captureErr := captureImages(ctx, staging, options, clientOptions, manager, destination, nil)
	return finalizeCapturedImageLayout(staging, root, captureErr, prune)
}

// finalizeCapturedImageLayout publishes the staged layout only after a complete capture.
// Any capture error leaves the previous destination untouched.
func finalizeCapturedImageLayout(source, destination string, captureErr error, prune bool) error {
	if captureErr != nil {
		return captureErr
	}

	if err := images.Validate(source); err != nil {
		return fmt.Errorf("validate captured image layout: %w", err)
	}

	if err := publishImageLayout(source, destination, prune); err != nil {
		return fmt.Errorf("publish captured image layout: %w", err)
	}

	return nil
}

// seedImageLayout copies an existing valid layout into staging before capture.
// This makes a default save additive:
// references from other clusters or profiles remain reachable in the shared OCI index.
func seedImageLayout(source, destination string) error {
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect existing image layout: %w", err)
	}

	if err := images.Validate(source); err != nil {
		return fmt.Errorf("validate existing image layout: %w", err)
	}

	return publishImageLayout(source, destination, false)
}

// captureImages discovers Pod image references and writes their OCI content
// to the supplied local root or content target.
// Callers are responsible for preparing and publishing the destination
// when the target has transactional metadata such as an S3 image index.
func captureImages(
	ctx context.Context,
	root string,
	options ImageOptions,
	clientOptions kube.ClientOptions,
	manager *progress.Manager,
	destination string,
	target oras.Target,
) error {
	if root == "" && target == nil {
		return errors.New("image layout destination is required")
	}
	if err := images.ValidateRegistryMirrors(options.RegistryMirrors); err != nil {
		return fmt.Errorf("validate registry mirrors: %w", err)
	}

	compiled, err := profile.LoadFrom(options.Profile, options.Directory)
	if err != nil {
		return fmt.Errorf("load image profile: %w", err)
	}

	platforms, err := parseImagePlatforms(options.Platforms)
	if err != nil {
		return err
	}

	var adapter *orascope.Adapter
	if !options.PullSecrets {
		adapter, err = orascope.NewDefault()
		if err != nil {
			return fmt.Errorf("load registry credentials: %w", err)
		}
	}

	client, err := kube.NewClient(ctx, clientOptions)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for images: %w", err)
	}

	selection := compiled.ImageSelectionDefaults()
	discoveryNamespaces := options.Namespaces
	if len(discoveryNamespaces) == 0 {
		discoveryNamespaces = imageSelectionNamespaces(selection)
	}

	// Owner-aware profiles need the discovery pass to retain owner metadata;
	// the simpler path avoids resolving owner objects when the profile does not use owner filters.
	var references []images.Reference
	if compiled.ImageOwnerSelectionEnabled() {
		references, err = images.DiscoverWithOwners(
			ctx,
			client.Dynamic,
			client.Discovery,
			discoveryNamespaces,
			func(reference images.Reference) (bool, error) {
				_, selected, filterErr := imageReferencePodSelection(
					compiled, selection, options.Namespaces, reference,
				)
				return selected, filterErr
			},
		)
	} else {
		references, err = images.Discover(ctx, client.Dynamic, discoveryNamespaces)
	}
	if err != nil {
		return fmt.Errorf("discover Pod images: %w", err)
	}

	filteredReferences := make([]images.Reference, 0, len(references))

	// Discovery is intentionally broader than the final profile match.
	// Image references carry Pod and owner metadata,
	// so all profile predicates are evaluated here before
	// any registry credentials or network pulls are used.
	for _, reference := range references {
		parsed, selected, selectionErr := imageReferencePodSelection(
			compiled, selection, options.Namespaces, reference,
		)
		if selectionErr != nil {
			return fmt.Errorf("parse image %q for profile selection: %w", reference.Source, selectionErr)
		}
		if !selected {
			continue
		}

		containerType := contract.ContainerTypeContainer
		if reference.Init {
			containerType = contract.ContainerTypeInitContainer
		}

		owners := []profile.MatchObject(nil)
		if compiled.ImageOwnerSelectionEnabled() {
			owners = imageOwnerMatchObjects(reference.Owners)
		}

		if !compiled.MatchesImageWithOwners(
			options.Namespaces,
			reference.Namespace,
			reference.Pod,
			reference.PodLabels,
			reference.PodAnnotations,
			containerType,
			parsed,
			owners,
		) {
			continue
		}

		filteredReferences = append(filteredReferences, reference)
	}

	references = filteredReferences

	log.Debug().
		Str("component", "images").
		Str("operation", "discover").
		Int("namespaces", len(discoveryNamespaces)).
		Int("references", len(references)).
		Msg("container image references discovered")

	if len(references) == 0 {
		log.Warn().
			Str("component", "images").
			Str("operation", "discover").
			Str("profile", compiled.Name()).
			Int("namespaces", len(discoveryNamespaces)).
			Int("references", len(references)).
			Msg("container image capture selected no images")

		if target == nil {
			if _, err := oci.New(root); err != nil {
				return fmt.Errorf("initialize empty image layout: %w", err)
			}
		}

		return nil
	}

	referenceGroups, err := groupImageReferences(references)
	if err != nil {
		return fmt.Errorf("group image references: %w", err)
	}
	uniqueReferences := make([]images.Reference, 0, len(referenceGroups))
	for _, group := range referenceGroups {
		uniqueReferences = append(uniqueReferences, group.primary)
	}

	// Pulls are kept sequential so the per-image progress rows remain stable
	// and registry failures can be reported without coordinating workers.
	stored, failures := 0, 0
	var failureErrors []error
	var (
		layerBytes            int64
		transferredBytes      int64
		reusedBytes           int64
		layers                int
		reusedLayers          int
		layerTransferredBytes int64
		layerReusedBytes      int64
	)

	counter := manager.NewCounter("Images", int64(len(uniqueReferences)))
	current := manager.NewCurrentPercentCounter("image")
	defer func() {
		current.Abort()
		if ctx.Err() != nil {
			counter.Abort()
			return
		}

		counter.Complete()
	}()

	for _, group := range referenceGroups {
		reference := group.primary
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("capture container images: %w", err)
		}

		label := imageProgressLabel(reference)
		current.Reset(label)
		current.SetProgress(100, 5)
		var (
			pullProgress images.PullProgress
			pullErr      error
			warnings     []error
			progressSet  bool
		)

		log.Trace().
			Str("component", "images").
			Str("operation", "pull").
			Str("image", reference.Source).
			Str("namespace", reference.Namespace).
			Str("pod", reference.Pod).
			Str("container", reference.Container).
			Msg("pulling container image")

		if reference.Runtime == "" || reference.Pull == reference.Source {
			log.Warn().Str("image", reference.Source).
				Str("pull", reference.Pull).
				Str("runtime", reference.Runtime).
				Str("namespace", reference.Namespace).
				Str("pod", reference.Pod).
				Str("container", reference.Container).
				Msg("Runtime image digest unavailable; resolving the mutable Pod image reference")
		}

		pullOptions := images.PullOptions{
			Aliases:         nil,
			Root:            root,
			Target:          target,
			Auth:            nil,
			Reference:       reference.Source,
			RegistryMirrors: options.RegistryMirrors,
			Progress: func(progress images.PullProgress) {
				percent := 0
				if progress.BytesTotal > 0 {
					percent = int(progress.BytesCompleted * 90 / progress.BytesTotal)
				} else if progress.Total > 0 {
					percent = progress.Completed * 90 / progress.Total
				}

				current.SetProgress(100, int64(min(95, 5+percent)))
			},
			RetryAttempts: retry.Attempts(ctx),
		}

		// A digest group can have aliases used by Pods with different imagePullSecrets.
		// Pull each credential scope independently while keeping one transfer identity
		// and one progress row for the group.
		capturedAliases := make(map[string]struct{}, len(group.sourceAliases))
		scopes, scopeErr := imageAuthScopes(group)
		if scopeErr != nil {
			pullErr = scopeErr
		}
		if !options.PullSecrets {
			scopes = []imageAuthScope{{
				references: group.references,
				aliases:    group.sourceAliases,
				primary:    group.primary,
			}}
		}

		var authFailures []error
		for _, scope := range scopes {
			if pullErr != nil {
				break
			}

			aliases := uncapturedImageAliases(scope.aliases, capturedAliases)
			if len(aliases) == 0 {
				continue
			}

			authCandidates := []imageAuthCandidate{{adapter: adapter}}
			if options.PullSecrets {
				authCandidates, err = imageRegistryAuthCandidates(ctx, client.Dynamic, scope.references)
				if err != nil {
					pullErr = fmt.Errorf("load registry credentials: %w", err)
					break
				}
			}

			progress, scopeWarnings, scopeErr := pullImageWithAuthCandidates(
				ctx,
				scope.primary,
				aliases,
				platforms,
				pullOptions,
				authCandidates,
			)
			if scopeErr != nil {
				if !isAuthenticationFailure(scopeErr) {
					pullErr = scopeErr
					break
				}

				authFailures = append(authFailures, scopeErr)
				continue
			}

			if !progressSet {
				pullProgress = progress
				progressSet = true
			}
			warnings = append(warnings, scopeWarnings...)
			for _, alias := range aliases {
				capturedAliases[alias] = struct{}{}
			}
		}

		if pullErr == nil && len(capturedAliases) != len(group.sourceAliases) {
			pullErr = errors.Join(
				errors.Join(authFailures...),
				errors.New("not all image aliases were captured"),
			)
		}

		if err := ctx.Err(); err != nil {
			return fmt.Errorf("capture container images: %w", err)
		}

		for _, warning := range warnings {
			log.Warn().
				Err(warning).
				Str("image", reference.Source).
				Msg("Requested image platform was not captured")
		}

		if pullErr != nil {
			failures++
			failureErrors = appendImageCaptureError(failureErrors, pullErr, reference.Source)
			current.SetLabel("failed: " + label)
			log.Error().
				Err(pullErr).
				Str("image", reference.Source).
				Str("namespace", reference.Namespace).
				Str("pod", reference.Pod).
				Msg("Failed to capture container image")

			counter.Increment()
			continue
		}

		stored++
		current.SetProgress(100, 100)
		stats := pullProgress
		layerBytes += stats.LayerBytesTotal
		transferredBytes += stats.BytesTransferred
		reusedBytes += stats.BytesReused
		layers += stats.Layers
		reusedLayers += stats.ReusedLayers
		layerTransferredBytes += stats.LayerBytesTransferred
		layerReusedBytes += stats.LayerBytesReused
		counter.Increment()
		log.Debug().
			Str("image", reference.Source).
			Msg("Captured container image")
	}

	if manager.Enabled() {
		manager.AddFinalMessage(fmt.Sprintf(
			"Images saved; layers: %d (downloaded %d, reused %d); size: %s; transferred: %s; reused: %s",
			layers,
			max(layers-reusedLayers, 0),
			reusedLayers,
			formatBytes(layerBytes),
			formatBytes(layerTransferredBytes),
			formatBytes(layerReusedBytes),
		))
	} else {
		log.Info().
			Str("destination", destination).
			Int("discovered", len(references)).
			Int("unique", len(uniqueReferences)).
			Int("stored", stored).
			Int("failed", failures).
			Int("layers", layers).
			Int("downloaded_layers", max(layers-reusedLayers, 0)).
			Int("reused_layers", reusedLayers).
			Int64("layer_bytes", layerBytes).
			Int64("transferred_bytes", transferredBytes).
			Int64("reused_bytes", reusedBytes).
			Int64("layer_transferred_bytes", layerTransferredBytes).
			Int64("layer_reused_bytes", layerReusedBytes).
			Msg("Container image capture completed")
	}

	if failures > 0 {
		if omitted := failures - len(failureErrors); omitted > 0 {
			failureErrors = append(failureErrors, fmt.Errorf(
				"%d additional image failure details omitted",
				omitted,
			))
		}

		summary := fmt.Errorf("capture failed for %d of %d image references", failures, len(referenceGroups))
		return errors.Join(summary, errors.Join(failureErrors...))
	}

	return nil
}

// imageOwnerMatchObjects converts only resolved ownerReferences.
// Pod metadata is matched separately and must not satisfy an owner selector.
func imageOwnerMatchObjects(owners []images.Owner) []profile.MatchObject {
	result := make([]profile.MatchObject, 0, len(owners))
	for _, owner := range owners {
		result = append(result, profile.MatchObject{
			Group:       owner.Group,
			Version:     owner.Version,
			Resource:    owner.Resource,
			Namespace:   owner.Namespace,
			Name:        owner.Name,
			Labels:      owner.Labels,
			Annotations: owner.Annotations,
		})
	}

	return result
}

// imageAuthCandidate is one isolated credential source for a grouped image pull.
// The adapter is never assembled from Secrets belonging to unrelated groups.
type imageAuthCandidate struct {
	adapter *orascope.Adapter
	secret  types.NamespacedName
}

// imageAuthScope groups aliases that share the same Pod credential boundary.
// A shared runtime digest does not permit credentials to cross that boundary.
type imageAuthScope struct {
	references []images.Reference
	aliases    []string
	primary    images.Reference
}

// imageAuthScopes splits a digest group by namespace and ordered Secret names.
func imageAuthScopes(group imageReferenceGroup) ([]imageAuthScope, error) {
	scopes := make([]imageAuthScope, 0, len(group.references))
	indexes := make(map[string]int, len(group.references))
	aliases := make([]map[string]struct{}, 0, len(group.references))

	for _, reference := range group.references {
		key := reference.Namespace + "\x00" + strings.Join(reference.PullSecrets, "\x00")
		index, exists := indexes[key]
		if !exists {
			index = len(scopes)
			indexes[key] = index
			scopes = append(scopes, imageAuthScope{
				references: []images.Reference{reference},
				primary:    reference,
			})
			aliases = append(aliases, make(map[string]struct{}))
		} else {
			scopes[index].references = append(scopes[index].references, reference)
		}

		alias := reference.Source
		if alias == "" {
			alias = reference.Pull
		}
		normalized, _, err := images.NormalizeReference(alias)
		if err != nil {
			return nil, fmt.Errorf("normalize image alias %q: %w", alias, err)
		}
		if _, exists := aliases[index][normalized]; exists {
			continue
		}

		aliases[index][normalized] = struct{}{}
		scopes[index].aliases = append(scopes[index].aliases, normalized)
	}

	return scopes, nil
}

// uncapturedImageAliases returns a stable scope subset not stored by an earlier scope.
func uncapturedImageAliases(aliases []string, captured map[string]struct{}) []string {
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if _, exists := captured[alias]; exists {
			continue
		}

		result = append(result, alias)
	}

	return result
}

// pullImageWithAuthCandidates tries isolated credentials in order.
// A second candidate is used only after an authentication response;
// registry, network, and content failures remain terminal for the image.
func pullImageWithAuthCandidates(
	ctx context.Context,
	reference images.Reference,
	sourceAliases []string,
	platforms []images.Platform,
	options images.PullOptions,
	candidates []imageAuthCandidate,
) (images.PullProgress, []error, error) {
	if len(candidates) == 0 {
		return images.PullProgress{}, nil, errors.New("image registry credentials are unavailable")
	}

	var failures []error
	for index, candidate := range candidates {
		attemptOptions := options
		attemptOptions.Auth = candidate.adapter
		progress, warnings, err := pullImageReference(
			ctx,
			reference,
			sourceAliases,
			platforms,
			attemptOptions,
		)
		if err == nil {
			return progress, warnings, nil
		}

		if candidate.secret.Name != "" {
			err = fmt.Errorf(
				"credentials from Secret %s/%s: %w",
				candidate.secret.Namespace,
				candidate.secret.Name,
				err,
			)
		}
		failures = append(failures, err)
		attemptErrors := append([]error{err}, warnings...)
		if index == len(candidates)-1 || !isAuthenticationFailure(errors.Join(attemptErrors...)) {
			return progress, warnings, errors.Join(failures...)
		}
	}

	return images.PullProgress{}, nil, errors.Join(failures...)
}

// pullImageReference captures one runtime digest and its declared source aliases.
func pullImageReference(
	ctx context.Context,
	reference images.Reference,
	sourceAliases []string,
	platforms []images.Platform,
	options images.PullOptions,
) (images.PullProgress, []error, error) {
	if len(platforms) == 0 {
		progress, err := pullImageReferenceSet(ctx, reference, sourceAliases, options)
		return progress, nil, err
	}

	progress := newPullProgressAccumulator(options.Progress)
	options.Progress = progress.observe
	var (
		warnings     []error
		platformErrs []error
	)
	for _, source := range platformSourceReferences(reference, sourceAliases) {
		platformOptions := options
		platformOptions.Aliases = nil
		_, sourceWarnings, sourceErr := images.PullPlatforms(
			ctx,
			source,
			platforms,
			platformOptions,
		)
		warnings = append(warnings, sourceWarnings...)
		if sourceErr != nil {
			platformErrs = append(platformErrs, fmt.Errorf(
				"capture image %q for requested platforms: %w",
				source,
				sourceErr,
			))
		}
		progress.reset()
	}

	runtimeOptions := options
	runtimeOptions.Aliases = nil
	runtimeOptions.Reference = reference.Pull
	_, runtimeErr := images.Pull(ctx, reference.Pull, runtimeOptions)
	if runtimeErr != nil {
		platformErrs = append(platformErrs, fmt.Errorf(
			"capture runtime digest %q: %w",
			reference.Pull,
			runtimeErr,
		))
	}
	if len(warnings) > 0 {
		platformErrs = append(platformErrs, errors.Join(
			errors.New("not all requested image platforms were captured"),
			errors.Join(warnings...),
		))
	}

	return progress.snapshot(), warnings, errors.Join(platformErrs...)
}

// isAuthenticationFailure limits credential fallback to explicit registry auth responses.
func isAuthenticationFailure(err error) bool {
	if response, ok := errors.AsType[*errcode.ErrorResponse](err); ok {
		return response.StatusCode == 401 || response.StatusCode == 403
	}

	if responseErrors, ok := errors.AsType[errcode.Errors](err); ok {
		for _, responseError := range responseErrors {
			if responseError.Code == errcode.ErrorCodeUnauthorized ||
				responseError.Code == errcode.ErrorCodeDenied {
				return true
			}
		}
	}

	return false
}

// platformSourceReferences returns each distinct source reference that must be resolved.
// Different mutable tags may share a runtime digest today but point to different manifests
// when this capture starts, so only duplicate copies of one normalized tag may be collapsed.
func platformSourceReferences(reference images.Reference, aliases []string) []string {
	candidates := make([]string, 0, len(aliases)+1)
	if reference.Source != "" {
		candidates = append(candidates, reference.Source)
	}
	candidates = append(candidates, aliases...)

	seen := make(map[string]struct{}, len(candidates))
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		normalized, _, err := images.NormalizeReference(candidate)
		if err != nil {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}

		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)

	return result
}

// pullImageReferenceSet captures the runtime digest
// and every declared source tag independently.
// The local OCI store deduplicates identical blobs,
// while different tag resolutions remain separate descriptors
// instead of being silently relabeled as the runtime image.
func pullImageReferenceSet(
	ctx context.Context,
	reference images.Reference,
	sourceAliases []string,
	options images.PullOptions,
) (images.PullProgress, error) {
	progress := newPullProgressAccumulator(options.Progress)
	options.Progress = progress.observe
	runtimeOptions := options
	runtimeOptions.Reference = reference.Pull
	runtimeOptions.Aliases = nil

	errorsFound := make([]error, 0, len(sourceAliases)+1)
	if _, err := images.Pull(ctx, reference.Pull, runtimeOptions); err != nil {
		errorsFound = append(errorsFound, fmt.Errorf("capture runtime digest %q: %w", reference.Pull, err))
	}
	progress.reset()

	normalizedPull, _, err := images.NormalizeReference(reference.Pull)
	if err != nil {
		return progress.snapshot(), errors.Join(errorsFound...)
	}
	for _, alias := range sourceAliases {
		if alias == normalizedPull {
			continue
		}

		aliasOptions := options
		aliasOptions.Reference = alias
		aliasOptions.Aliases = nil
		if _, err := images.Pull(ctx, alias, aliasOptions); err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("capture source reference %q: %w", alias, err))
		}
		progress.reset()
	}

	return progress.snapshot(), errors.Join(errorsFound...)
}

// pullProgressAccumulator combines independent ORAS progress streams into one image-level total.
// ORAS reports cumulative values per pull,
// so each stream must be reduced to a delta before it is added to the aggregate.
type pullProgressAccumulator struct {
	callback  func(images.PullProgress)
	aggregate images.PullProgress
	previous  images.PullProgress
	mutex     sync.Mutex
}

// newPullProgressAccumulator creates an accumulator for cumulative ORAS snapshots.
func newPullProgressAccumulator(callback func(images.PullProgress)) *pullProgressAccumulator {
	return &pullProgressAccumulator{callback: callback}
}

// observe adds one cumulative ORAS snapshot to the aggregate progress.
func (a *pullProgressAccumulator) observe(current images.PullProgress) {
	if a == nil {
		return
	}

	a.mutex.Lock()
	delta := subtractPullProgress(current, a.previous)
	a.aggregate = addPullProgress(a.aggregate, delta)
	a.previous = current
	progress := a.aggregate
	callback := a.callback
	a.mutex.Unlock()

	if callback != nil {
		callback(progress)
	}
}

// reset starts a new independent ORAS stream while keeping the accumulated total.
func (a *pullProgressAccumulator) reset() {
	if a == nil {
		return
	}

	a.mutex.Lock()
	a.previous = images.PullProgress{}
	a.mutex.Unlock()
}

// snapshot returns the aggregate progress collected so far.
func (a *pullProgressAccumulator) snapshot() images.PullProgress {
	if a == nil {
		return images.PullProgress{}
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.aggregate
}

// addPullProgress combines the counters from two independent progress totals.
func addPullProgress(left, right images.PullProgress) images.PullProgress {
	return images.PullProgress{
		Total:                 left.Total + right.Total,
		Completed:             left.Completed + right.Completed,
		BytesTotal:            left.BytesTotal + right.BytesTotal,
		BytesCompleted:        left.BytesCompleted + right.BytesCompleted,
		BytesTransferred:      left.BytesTransferred + right.BytesTransferred,
		BytesReused:           left.BytesReused + right.BytesReused,
		Layers:                left.Layers + right.Layers,
		ReusedLayers:          left.ReusedLayers + right.ReusedLayers,
		LayerBytesTotal:       left.LayerBytesTotal + right.LayerBytesTotal,
		LayerBytesTransferred: left.LayerBytesTransferred + right.LayerBytesTransferred,
		LayerBytesReused:      left.LayerBytesReused + right.LayerBytesReused,
	}
}

// subtractPullProgress converts a cumulative snapshot into a non-negative delta.
func subtractPullProgress(current, previous images.PullProgress) images.PullProgress {
	return images.PullProgress{
		Total:                 nonNegativeProgressDelta(current.Total, previous.Total),
		Completed:             nonNegativeProgressDelta(current.Completed, previous.Completed),
		BytesTotal:            nonNegativeProgressDeltaInt64(current.BytesTotal, previous.BytesTotal),
		BytesCompleted:        nonNegativeProgressDeltaInt64(current.BytesCompleted, previous.BytesCompleted),
		BytesTransferred:      nonNegativeProgressDeltaInt64(current.BytesTransferred, previous.BytesTransferred),
		BytesReused:           nonNegativeProgressDeltaInt64(current.BytesReused, previous.BytesReused),
		Layers:                nonNegativeProgressDelta(current.Layers, previous.Layers),
		ReusedLayers:          nonNegativeProgressDelta(current.ReusedLayers, previous.ReusedLayers),
		LayerBytesTotal:       nonNegativeProgressDeltaInt64(current.LayerBytesTotal, previous.LayerBytesTotal),
		LayerBytesTransferred: nonNegativeProgressDeltaInt64(current.LayerBytesTransferred, previous.LayerBytesTransferred),
		LayerBytesReused:      nonNegativeProgressDeltaInt64(current.LayerBytesReused, previous.LayerBytesReused),
	}
}

// nonNegativeProgressDelta returns a delta and treats a counter reset as a new value.
func nonNegativeProgressDelta(current, previous int) int {
	if current < previous {
		return current
	}

	return current - previous
}

// nonNegativeProgressDeltaInt64 is the int64 variant of nonNegativeProgressDelta.
func nonNegativeProgressDeltaInt64(current, previous int64) int64 {
	if current < previous {
		return current
	}

	return current - previous
}

// appendImageCaptureError keeps the returned aggregate useful for large image sets.
// Every failure is still reflected in the counters and logs,
// but only a bounded number of detailed causes is returned to the caller.
func appendImageCaptureError(previous []error, err error, reference string) []error {
	if err == nil || len(previous) >= maxImageCaptureErrors {
		return previous
	}

	return append(previous, fmt.Errorf("image %q: %w", reference, err))
}

// imageReferenceGroup keeps one transfer reference together
// with every source alias and the runtime digest alias for that identity.
type imageReferenceGroup struct {
	references    []images.Reference
	sourceAliases []string
	primary       images.Reference
}

// groupImageReferences deduplicates transfers without discarding source aliases.
// Discovery order is already deterministic, so the first reference is stable.
func groupImageReferences(references []images.Reference) ([]imageReferenceGroup, error) {
	groups := make([]imageReferenceGroup, 0, len(references))
	indexes := make(map[string]int, len(references))
	aliasOwners := make(map[string]string, len(references))

	for _, reference := range references {
		if reference.Pull == "" {
			return nil, errors.New("image reference has an empty pull identity")
		}

		index, exists := indexes[reference.Pull]
		if !exists {
			index = len(groups)
			indexes[reference.Pull] = index
			groups = append(groups, imageReferenceGroup{
				primary:    reference,
				references: []images.Reference{reference},
			})
		} else {
			groups[index].references = append(groups[index].references, reference)
		}

		alias := reference.Source
		if alias == "" {
			alias = reference.Pull
		}

		normalizedAlias, _, err := images.NormalizeReference(alias)
		if err != nil {
			return nil, fmt.Errorf("normalize image alias %q: %w", alias, err)
		}

		if owner, exists := aliasOwners[normalizedAlias]; exists && owner != reference.Pull {
			return nil, fmt.Errorf(
				"image alias %q resolves to multiple pull identities %q and %q",
				normalizedAlias, owner, reference.Pull,
			)
		}

		aliasOwners[normalizedAlias] = reference.Pull
	}

	for index := range groups {
		seen := make(map[string]struct{}, len(groups[index].references))
		for _, reference := range groups[index].references {
			alias := reference.Source
			if alias == "" {
				alias = reference.Pull
			}

			normalized, _, err := images.NormalizeReference(alias)
			if err != nil {
				return nil, fmt.Errorf("normalize image alias %q: %w", alias, err)
			}

			if _, exists := seen[normalized]; exists {
				continue
			}

			seen[normalized] = struct{}{}
			groups[index].sourceAliases = append(groups[index].sourceAliases, normalized)
		}

		sort.Strings(groups[index].sourceAliases)
	}

	return groups, nil
}

// imageProgressLabel keeps the declared image reference visible in progress output.
func imageProgressLabel(reference images.Reference) string {
	label := strings.TrimSpace(reference.Source)
	if label == "" {
		label = strings.TrimSpace(reference.Pull)
	}

	return label
}

// imageReferencePodSelection applies Pod, container,
// and image-reference filters before owner resolution or registry access.
func imageReferencePodSelection(
	compiled *profile.CompiledProfile,
	selection contract.ImageSelectionSpec,
	namespaces []string,
	reference images.Reference,
) (registry.Reference, bool, error) {
	parsed := registry.Reference{}
	if len(selection.References) > 0 {
		var err error
		_, parsed, err = images.NormalizeReference(reference.Source)
		if err != nil {
			return registry.Reference{}, false, err
		}
	}

	containerType := contract.ContainerTypeContainer
	if reference.Init {
		containerType = contract.ContainerTypeInitContainer
	}

	return parsed, compiled.MatchesImagePodInNamespaces(
		namespaces,
		reference.Namespace,
		reference.Pod,
		reference.PodLabels,
		reference.PodAnnotations,
		containerType,
		parsed,
	), nil
}

// imageSelectionNamespaces returns the namespace union needed for Pod discovery.
func imageSelectionNamespaces(selection contract.ImageSelectionSpec) []string {
	if !namespacePatternsAreExact(selection.Namespaces) ||
		!namespacePatternsAreExact(selection.Owners.Namespaces) {
		return nil
	}

	values := make(map[string]struct{}, len(selection.Namespaces)+len(selection.Owners.Namespaces))
	for _, namespace := range selection.Namespaces {
		values[namespace] = struct{}{}
	}
	for _, namespace := range selection.Owners.Namespaces {
		values[namespace] = struct{}{}
	}

	result := make([]string, 0, len(values))
	for namespace := range values {
		result = append(result, namespace)
	}
	sort.Strings(result)

	return result
}

// namespacePatternsAreExact reports whether values can be used
// as an exact Kubernetes namespace list scope without losing profile filtering semantics.
func namespacePatternsAreExact(values []string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, "!") || strings.ContainsAny(value, "*?\\[") {
			return false
		}
	}

	return true
}

// parseImagePlatforms validates repeatable CLI platform values once per run.
func parseImagePlatforms(values []string) ([]images.Platform, error) {
	platforms := make([]images.Platform, 0, len(values))
	seen := make(map[images.Platform]struct{}, len(values))
	for _, value := range values {
		platform, err := images.ParsePlatform(value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[platform]; exists {
			continue
		}

		seen[platform] = struct{}{}
		platforms = append(platforms, platform)
	}

	return platforms, nil
}

// imageRegistryAuthCandidates builds one credential candidate per Secret,
// followed by the normal ORAScope adapter as a host-level fallback.
func imageRegistryAuthCandidates(
	ctx context.Context,
	client dynamic.Interface,
	references []images.Reference,
) ([]imageAuthCandidate, error) {
	defaultAdapter, err := orascope.NewDefault()
	if err != nil {
		return nil, err
	}

	candidates := make([]imageAuthCandidate, 0)
	seen := make(map[types.NamespacedName]struct{})
	for _, reference := range references {
		for _, name := range reference.PullSecrets {
			key := types.NamespacedName{Namespace: reference.Namespace, Name: name}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			secret, err := client.
				Resource(secretsResource).
				Namespace(key.Namespace).
				Get(ctx, key.Name, metav1.GetOptions{})
			if err != nil {
				log.Warn().
					Err(err).Str("namespace", key.Namespace).
					Str("secret", key.Name).
					Msg("Failed to read imagePullSecret")
				continue
			}

			configs, ok := dockerAuthConfigsFromSecret(secret, key)
			if !ok {
				continue
			}

			data, err := json.Marshal(struct {
				Auths map[string]json.RawMessage `json:"auths"`
			}{Auths: configs})
			if err != nil {
				return nil, fmt.Errorf("marshal image credentials from %s/%s: %w", key.Namespace, key.Name, err)
			}

			adapter, err := orascope.New(
				orascope.WithoutDiscovery(),
				orascope.WithDockerAuthConfigJSON(data),
				orascope.WithCredentialFallback(defaultAdapter.CredentialFunc(nil)),
			)
			if err != nil {
				return nil, fmt.Errorf("configure image credentials from %s/%s: %w", key.Namespace, key.Name, err)
			}

			candidates = append(candidates, imageAuthCandidate{adapter: adapter, secret: key})
		}
	}

	candidates = append(candidates, imageAuthCandidate{adapter: defaultAdapter})
	return candidates, nil
}

// dockerAuthConfigsFromSecret extracts supported Docker auth data without
// retaining or logging decoded credential values outside the adapter.
func dockerAuthConfigsFromSecret(
	secret *unstructured.Unstructured,
	key types.NamespacedName,
) (map[string]json.RawMessage, bool) {
	data, found, err := unstructured.NestedStringMap(secret.Object, "data")
	if err != nil || !found {
		log.Warn().
			Str("namespace", key.Namespace).
			Str("secret", key.Name).
			Msg("imagePullSecret has no readable data")
		return nil, false
	}

	for _, field := range []string{".dockerconfigjson", ".dockercfg"} {
		encoded, ok := data[field]
		if !ok {
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			log.Warn().
				Err(err).
				Str("namespace", key.Namespace).
				Str("secret", key.Name).
				Msg("Failed to decode imagePullSecret")
			return nil, false
		}

		var config map[string]json.RawMessage
		if err := json.Unmarshal(decoded, &config); err != nil {
			log.Warn().
				Err(err).
				Str("namespace", key.Namespace).
				Str("secret", key.Name).
				Msg("Failed to parse imagePullSecret")
			return nil, false
		}

		if field == ".dockerconfigjson" {
			auths, ok := config["auths"]
			var authConfigs map[string]json.RawMessage
			if !ok || json.Unmarshal(auths, &authConfigs) != nil {
				log.Warn().
					Str("namespace", key.Namespace).
					Str("secret", key.Name).
					Msg("imagePullSecret has no auths map")
				return nil, false
			}
			config = authConfigs
		}

		return config, true
	}

	log.Warn().
		Str("namespace", key.Namespace).
		Str("secret", key.Name).
		Msg("imagePullSecret is not a supported Docker credential Secret")
	return nil, false
}

// Execute captures images directly into the S3-backed OCI content store.
// Existing blobs are reused by digest, and the index is committed only after
// all selected image references have been captured successfully.
func (c *ImagesS3Command) Execute(_ []string) error {
	if c.URI == "" {
		return errors.New("S3 image destination is required")
	}

	destination := withS3LayoutPrefix(c.S3Destination, imageLayoutDirectory)
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 image destination: %w", err)
	}

	imageStore, err := export.NewS3ImageStoreForCapture(c.context(), store, true)
	if err != nil {
		return fmt.Errorf("create S3 image store: %w", err)
	}

	captureErr := captureImages(
		c.context(),
		"",
		c.imageOptions,
		c.clientOptions,
		c.progress,
		c.URI,
		imageStore,
	)
	if captureErr != nil {
		return fmt.Errorf("capture images to S3: %w", captureErr)
	}
	if err := imageStore.Commit(c.context()); err != nil {
		return fmt.Errorf("commit S3 image layout: %w", err)
	}

	return nil
}

// newImageLayoutStaging creates a sibling directory on the same filesystem
// so file publication can preserve the previous index on capture failure.
func newImageLayoutStaging(root string) (string, error) {
	if root == "" {
		return "", errors.New("image layout root is required")
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("create image layout parent: %w", err)
	}

	return os.MkdirTemp(parent, ".kube-dump-images-staging-")
}

// publishImageLayout copies a validated layout into the destination while replacing index.json last.
// Readers therefore retain the previous graph until all blobs for the new graph are available.
func publishImageLayout(source, destination string, prune bool) error {
	if source == "" || destination == "" {
		return errors.New("image layout source and destination are required")
	}
	if filepath.Clean(source) == filepath.Clean(destination) {
		return errors.New("image layout source and destination must differ")
	}
	if err := images.Validate(source); err != nil {
		return fmt.Errorf("validate image layout before publication: %w", err)
	}

	if err := os.MkdirAll(destination, 0o750); err != nil {
		return fmt.Errorf("create image layout destination: %w", err)
	}
	if err := validateImageLayoutDestination(destination); err != nil {
		return err
	}

	files := make([]string, 0)
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("image layout contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect image layout entry %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("image layout contains unsupported entry %q", path)
		}
		files = append(files, path)

		return nil
	})
	if err != nil {
		return fmt.Errorf("scan image layout for publication: %w", err)
	}

	sort.SliceStable(files, func(left, right int) bool {
		leftIndex := filepath.Base(files[left]) == "index.json"
		rightIndex := filepath.Base(files[right]) == "index.json"
		return !leftIndex && rightIndex
	})

	for _, sourceFile := range files {
		relative, err := filepath.Rel(source, sourceFile)
		if err != nil {
			return fmt.Errorf("resolve image layout path: %w", err)
		}

		targetFile := filepath.Join(destination, relative)
		if err := copyImageLayoutFile(sourceFile, targetFile); err != nil {
			return fmt.Errorf("publish image layout file %q: %w", relative, err)
		}
	}

	if prune {
		if err := pruneImageLayout(destination, source, files); err != nil {
			return fmt.Errorf("prune image layout: %w", err)
		}
	}

	return nil
}

// pruneImageLayout removes only OCI files managed by the published layout.
// User files outside oci-layout, index.json, and blobs remain untouched.
func pruneImageLayout(destination, source string, sourceFiles []string) error {
	keep := make(map[string]struct{}, len(sourceFiles))
	for _, sourceFile := range sourceFiles {
		relative, err := filepath.Rel(source, sourceFile)
		if err != nil {
			return fmt.Errorf("resolve source image layout path: %w", err)
		}

		keep[filepath.ToSlash(relative)] = struct{}{}
	}

	var stale []string
	err := filepath.WalkDir(destination, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		relative, err := filepath.Rel(destination, path)
		if err != nil {
			return fmt.Errorf("resolve destination image layout path: %w", err)
		}
		relative = filepath.ToSlash(relative)
		if !isManagedImageLayoutPath(relative) {
			return nil
		}
		if _, found := keep[relative]; !found {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, path := range stale {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale image layout file %q: %w", path, err)
		}
	}

	return nil
}

// isManagedImageLayoutPath identifies files that can be removed by --prune.
func isManagedImageLayoutPath(relative string) bool {
	return relative == "oci-layout" ||
		relative == "index.json" ||
		strings.HasPrefix(relative, "blobs/")
}

// validateImageLayoutDestination rejects links and special files before publication.
// Otherwise an existing directory symlink could redirect a blob write outside the selected layout.
func validateImageLayoutDestination(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("image layout destination contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect image layout destination entry %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("image layout destination contains unsupported entry %q", path)
		}

		return nil
	})
}

// copyImageLayoutFile atomically copies one OCI layout file to its destination.
func copyImageLayoutFile(source, destination string) error {
	if equal, err := imageLayoutFilesEqual(source, destination); err != nil {
		return err
	} else if equal {
		return nil
	}

	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()

	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	return writeAtomic(destination, func(output io.Writer) error {
		_, err := io.Copy(output, input)
		return err
	})
}

// imageLayoutFilesEqual compares existing files without buffering their content.
func imageLayoutFilesEqual(left, right string) (bool, error) {
	leftInfo, err := os.Lstat(left)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	rightInfo, err := os.Lstat(right)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !leftInfo.Mode().IsRegular() || !rightInfo.Mode().IsRegular() {
		return false, nil
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}

	leftFile, err := os.Open(left)
	if err != nil {
		return false, err
	}
	defer func() { _ = leftFile.Close() }()

	rightFile, err := os.Open(right)
	if err != nil {
		return false, err
	}
	defer func() { _ = rightFile.Close() }()

	leftBuffer := make([]byte, 64<<10)
	rightBuffer := make([]byte, 64<<10)
	for {
		leftRead, leftErr := leftFile.Read(leftBuffer)
		rightRead, rightErr := rightFile.Read(rightBuffer)
		if leftRead != rightRead || !bytes.Equal(leftBuffer[:leftRead], rightBuffer[:rightRead]) {
			return false, nil
		}
		if leftErr == io.EOF || rightErr == io.EOF {
			return leftErr == io.EOF && rightErr == io.EOF, nil
		}
		if leftErr != nil {
			return false, leftErr
		}
		if rightErr != nil {
			return false, rightErr
		}
	}
}

// Execute publishes a local OCI Image Layout to the explicit target prefix.
func (c *ImagePushDirCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("image push output is required")
	}

	auth, err := orascope.NewDefault()
	if err != nil {
		return fmt.Errorf("load registry credentials: %w", err)
	}
	if err := publishImages(
		c.context(),
		imageRoot(c.Positional.Path),
		c.Positional.Target,
		c.Positional.Images,
		auth,
		c.progress,
	); err != nil {
		return fmt.Errorf("publish images: %w", err)
	}

	return nil
}

// Execute downloads an S3 OCI Image Layout and publishes it to the explicit target prefix.
// Registry authentication is handled by ORAScope.
func (c *ImagePushS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("image push output is required")
	}

	auth, err := orascope.NewDefault()
	if err != nil {
		return fmt.Errorf("load registry credentials: %w", err)
	}

	destination := withS3LayoutPrefix(c.S3Destination, imageLayoutDirectory)
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 image source: %w", err)
	}
	imageStore, err := export.LoadS3ImageStore(c.context(), store)
	if err != nil {
		return fmt.Errorf("open S3 image layout: %w", err)
	}

	if err := publishImageTarget(
		c.context(),
		imageStore,
		imageStore.Index(),
		c.Positional.Target,
		c.Positional.Images,
		auth,
		c.progress,
	); err != nil {
		return fmt.Errorf("publish images: %w", err)
	}

	return nil
}

// publishImages adapts image-level ORAS callbacks to the shared terminal counter.
func publishImages(
	ctx context.Context,
	root, target string,
	references []string,
	auth *orascope.Adapter,
	manager *progress.Manager,
) error {
	var counter *progress.Counter
	defer func() {
		if counter != nil {
			counter.Complete()
		}
	}()

	return images.PublishWithOptions(ctx, root, target, images.PublishOptions{
		Auth:          auth,
		References:    references,
		RetryAttempts: retry.Attempts(ctx),
		Progress: func(total, completed int) {
			if counter == nil {
				counter = manager.NewCounter("Images", int64(total))
			}
			if completed > 0 {
				counter.Increment()
			}
		},
	})
}

// publishImageTarget adapts an already opened OCI target
// to the shared image-level progress counter used by local and S3 publication.
func publishImageTarget(
	ctx context.Context,
	source oras.Target,
	index ocispec.Index,
	target string,
	references []string,
	auth *orascope.Adapter,
	manager *progress.Manager,
) error {
	var counter *progress.Counter
	defer func() {
		if counter != nil {
			counter.Complete()
		}
	}()

	return images.PublishTargetWithOptions(ctx, source, index, target, images.PublishOptions{
		Auth:          auth,
		References:    references,
		RetryAttempts: retry.Attempts(ctx),
		Progress: func(total, completed int) {
			if counter == nil {
				counter = manager.NewCounter("Images", int64(total))
			}
			if completed > 0 {
				counter.Increment()
			}
		},
	})
}
