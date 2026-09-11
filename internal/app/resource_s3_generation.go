// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/export"
)

const (
	resourceGenerationVersion = 1
	maxResourcePointerBytes   = 4 << 10
	resourceGenerationKeep    = 2
	resourceGenerationGrace   = time.Hour
	resourceGenerationCleanup = 30 * time.Second
)

// resourceGenerationPointer identifies the complete S3 resource tree selected by readers.
// The pointer is published only after every object in the generation is uploaded.
type resourceGenerationPointer struct {
	Generation string `json:"generation"`
	Version    int    `json:"version"`
}

// cleanupS3ResourceGeneration removes an unpublished generation with a fresh bounded context.
// The caller's capture context may already be cancelled when cleanup starts.
func cleanupS3ResourceGeneration(store *export.S3Store, generation string) error {
	if store == nil || generation == "" {
		return errors.New("S3 resource store and generation are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), resourceGenerationCleanup)
	defer cancel()

	return store.DeletePrefix(ctx, resourceGenerationPrefix(generation))
}

// resourceGenerationRetentionCandidates selects old owned generations that are safe to remove.
// It keeps the current generation and one predecessor even after the grace period.
func resourceGenerationRetentionCandidates(
	objects []export.StoredObject,
	current string,
	now time.Time,
) []string {
	type generationInfo struct {
		modified time.Time
		name     string
	}

	byName := make(map[string]generationInfo)
	for _, object := range objects {
		relative, ok := resourceGenerationRelativeKey(object.Key)
		if !ok {
			continue
		}

		generation, _, _ := strings.Cut(relative, "/")
		if err := validateResourceGenerationPointer(resourceGenerationPointer{
			Version:    resourceGenerationVersion,
			Generation: generation,
		}); err != nil {
			continue
		}
		if object.LastModified.IsZero() {
			continue
		}

		candidate := byName[generation]
		candidate.name = generation
		if object.LastModified.After(candidate.modified) {
			candidate.modified = object.LastModified
		}
		byName[generation] = candidate
	}

	ordered := make([]generationInfo, 0, len(byName))
	for _, generation := range byName {
		ordered = append(ordered, generation)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].modified.Equal(ordered[right].modified) {
			return ordered[left].name > ordered[right].name
		}

		return ordered[left].modified.After(ordered[right].modified)
	})

	expired := make([]string, 0)
	predecessors := 0
	for _, generation := range ordered {
		if generation.name == current {
			continue
		}
		if predecessors < resourceGenerationKeep-1 {
			predecessors++
			continue
		}
		if now.Sub(generation.modified) < resourceGenerationGrace {
			continue
		}

		expired = append(expired, generation.name)
	}

	return expired
}

// pruneS3ResourceGenerations removes only old, complete generations below the resource prefix.
// The current generation and one predecessor remain available to readers.
func pruneS3ResourceGenerations(
	ctx context.Context,
	store *export.S3Store,
	current string,
) error {
	if store == nil {
		return errors.New("S3 resource store is required")
	}

	objects, err := store.ListStoredObjects(ctx)
	if err != nil {
		return fmt.Errorf("list S3 resource generations: %w", err)
	}

	prefix := path.Join(store.ObjectKey(), resourceGenerationPrefix("")) + "/"
	basePrefix := strings.Trim(store.ObjectKey(), "/")
	relativeObjects := make([]export.StoredObject, 0, len(objects))
	for _, object := range objects {
		if !strings.HasPrefix(object.Key, prefix) {
			continue
		}

		if basePrefix != "" {
			object.Key = strings.TrimPrefix(object.Key, basePrefix+"/")
		}
		relativeObjects = append(relativeObjects, object)
	}

	candidates := resourceGenerationRetentionCandidates(relativeObjects, current, time.Now())
	for _, generation := range candidates {
		if err := store.DeletePrefix(ctx, resourceGenerationPrefix(generation)); err != nil {
			return fmt.Errorf("remove expired S3 resource generation %q: %w", generation, err)
		}
	}

	return nil
}

// resourceGenerationRelativeKey extracts a generation-relative object key from a listing.
func resourceGenerationRelativeKey(key string) (string, bool) {
	prefix := resourceGenerationPrefix("") + "/"
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}

	return strings.TrimPrefix(key, prefix), true
}

// newResourceGeneration returns an opaque collision-resistant S3 directory name.
func newResourceGeneration() (string, error) {
	var data [16]byte
	if _, err := io.ReadFull(rand.Reader, data[:]); err != nil {
		return "", fmt.Errorf("generate resource generation: %w", err)
	}

	return hex.EncodeToString(data[:]), nil
}

// resourceGenerationPrefix returns the S3 prefix containing one complete resource tree.
func resourceGenerationPrefix(generation string) string {
	return path.Join(resourceLayoutDirectory, resourceGenerationDirectory, generation)
}

// resourceCurrentPointerKey returns the S3 object used as the publication boundary.
func resourceCurrentPointerKey() string {
	return path.Join(resourceLayoutDirectory, resourceCurrentPointer)
}

// readCurrentResourceGeneration reads and pins the current S3 generation pointer.
func readCurrentResourceGeneration(
	ctx context.Context,
	store *export.S3Store,
) (string, *export.ObjectIdentity, bool, error) {
	if store == nil {
		return "", nil, false, errors.New("S3 resource store is required")
	}

	found, err := store.ObjectExists(ctx, resourceCurrentPointerKey())
	if err != nil {
		return "", nil, false, fmt.Errorf("inspect S3 resource pointer: %w", err)
	}
	if !found {
		return "", nil, false, nil
	}

	identity, err := store.HeadObjectIdentity(ctx, resourceCurrentPointerKey())
	if err != nil {
		return "", nil, false, fmt.Errorf("pin S3 resource pointer: %w", err)
	}
	body, _, err := store.OpenObjectWithIdentity(ctx, resourceCurrentPointerKey(), &identity)
	if err != nil {
		return "", nil, false, fmt.Errorf("open S3 resource pointer: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxResourcePointerBytes+1))
	closeErr := body.Close()
	if readErr != nil {
		return "", nil, false, fmt.Errorf("read S3 resource pointer: %w", readErr)
	}
	if closeErr != nil {
		return "", nil, false, fmt.Errorf("close S3 resource pointer: %w", closeErr)
	}
	if len(data) > maxResourcePointerBytes {
		return "", nil, false, fmt.Errorf("S3 resource pointer exceeds %d bytes", maxResourcePointerBytes)
	}

	var pointer resourceGenerationPointer
	if err := json.Unmarshal(data, &pointer); err != nil {
		return "", nil, false, fmt.Errorf("decode S3 resource pointer: %w", err)
	}
	if err := validateResourceGenerationPointer(pointer); err != nil {
		return "", nil, false, err
	}

	return pointer.Generation, &identity, true, nil
}

// validateResourceGenerationPointer rejects pointers that could escape the generation prefix.
func validateResourceGenerationPointer(pointer resourceGenerationPointer) error {
	if pointer.Version != resourceGenerationVersion {
		return fmt.Errorf("unsupported S3 resource pointer version %d", pointer.Version)
	}
	if len(pointer.Generation) != 32 || path.Clean(pointer.Generation) != pointer.Generation {
		return fmt.Errorf("invalid S3 resource generation %q", pointer.Generation)
	}
	if _, err := hex.DecodeString(pointer.Generation); err != nil {
		return fmt.Errorf("invalid S3 resource generation %q: %w", pointer.Generation, err)
	}

	return nil
}

// resourceGenerationPointerData encodes the pointer with stable JSON formatting.
func resourceGenerationPointerData(generation string) ([]byte, error) {
	pointer := resourceGenerationPointer{
		Version:    resourceGenerationVersion,
		Generation: generation,
	}
	if err := validateResourceGenerationPointer(pointer); err != nil {
		return nil, err
	}

	data, err := json.Marshal(pointer)
	if err != nil {
		return nil, fmt.Errorf("encode S3 resource pointer: %w", err)
	}

	return data, nil
}

// openCurrentS3ResourceStores resolves the committed generation for resource readers.
// A missing pointer is an invalid resource backup, not an invitation to read a partial tree.
func openCurrentS3ResourceStores(
	ctx context.Context,
	destination S3Destination,
) (*export.S3Store, *export.S3Store, error) {
	baseStore, err := newS3Store(ctx, destination)
	if err != nil {
		return nil, nil, err
	}

	generation, _, found, err := readCurrentResourceGeneration(ctx, baseStore)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, errors.New("S3 resource generation pointer is missing")
	}

	generationDestination := withS3LayoutPrefix(destination, resourceGenerationPrefix(generation))
	metadataStore, err := newS3Store(
		ctx, withS3LayoutPrefix(generationDestination, cryptoLayoutDirectory),
	)
	if err != nil {
		return nil, nil, err
	}
	resourceStore, err := newS3Store(
		ctx, withS3LayoutPrefix(generationDestination, resourceLayoutDirectory),
	)
	if err != nil {
		return nil, nil, err
	}

	return metadataStore, resourceStore, nil
}
