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
	"strings"

	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"go.yaml.in/yaml/v3"
)

const maxS3PVCMetadataBytes = 1 << 20

// rotatePVCS3Runs removes timestamped PVC artifact prefixes
// older than keep independently for every namespace/PVC pair.
func rotatePVCS3Runs(ctx context.Context, store *export.S3Store, keep int) (int, error) {
	if store == nil {
		return 0, errors.New("PVC S3 store is required")
	}
	if keep < 0 {
		return 0, errors.New("PVC artifact retention cannot be negative")
	}
	if keep == 0 {
		return 0, nil
	}

	objects, err := store.ListStoredObjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list PVC S3 artifacts: %w", err)
	}

	objectsByKey := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		objectsByKey[object.Key] = struct{}{}
	}

	artifacts := make(map[string]map[string]struct{})
	for _, object := range objects {
		relative := strings.TrimPrefix(strings.TrimPrefix(object.Key, store.ObjectKey()), "/")
		parts := strings.Split(relative, "/")
		if len(parts) != 5 || parts[0] != "volumes" || parts[1] == "" ||
			parts[2] == "" || !pvc.IsTimestamp(parts[3]) || parts[4] != pvc.MetadataFilename {
			continue
		}

		metadata, err := readS3PVCMetadata(ctx, store, object.Key)
		if err != nil {
			// Invalid or incomplete revisions are not counted and are never removed by retention.
			continue
		}

		dataKey := path.Join(store.ObjectKey(), "volumes", parts[1], parts[2], parts[3],
			pvc.DataFilename(metadata.Compression, metadata.Encrypted))
		if _, found := objectsByKey[dataKey]; !found {
			continue
		}

		claimPrefix := path.Join(parts[0], parts[1], parts[2])
		if artifacts[claimPrefix] == nil {
			artifacts[claimPrefix] = make(map[string]struct{})
		}
		artifacts[claimPrefix][parts[3]] = struct{}{}
	}

	removed := 0
	for claimPrefix, timestamps := range artifacts {
		ordered := make([]string, 0, len(timestamps))
		for timestamp := range timestamps {
			ordered = append(ordered, timestamp)
		}
		sort.Slice(ordered, func(left, right int) bool {
			return pvc.CompareRevisions(ordered[left], ordered[right]) > 0
		})

		for _, timestamp := range ordered[min(keep, len(ordered)):] {
			prefix := path.Join(claimPrefix, timestamp)
			if err := store.DeletePrefix(ctx, prefix); err != nil {
				return removed, fmt.Errorf("remove expired PVC S3 artifact %q: %w", prefix, err)
			}

			removed++
		}
	}

	return removed, nil
}

// readS3PVCMetadata reads and validates the small metadata commit marker.
func readS3PVCMetadata(ctx context.Context, store *export.S3Store, key string) (pvc.Metadata, error) {
	body, err := store.OpenObject(ctx, key)
	if err != nil {
		return pvc.Metadata{}, err
	}
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(io.LimitReader(body, maxS3PVCMetadataBytes+1))
	if err != nil {
		return pvc.Metadata{}, err
	}
	if len(data) > maxS3PVCMetadataBytes {
		return pvc.Metadata{}, errors.New("S3 PVC metadata is too large")
	}

	var metadata pvc.Metadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return pvc.Metadata{}, err
	}
	if err := metadata.Validate(); err != nil {
		return pvc.Metadata{}, err
	}

	return metadata, nil
}
