// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SnapshotCopyStrategy backs up a temporary PVC cloned from a CSI snapshot.
type SnapshotCopyStrategy struct {
	// BackupStrategy streams the temporary clone into the final artifact.
	BackupStrategy BackupStrategy
	// NamePrefix controls temporary resource names.
	NamePrefix string
	// Snapshots manages the VolumeSnapshot lifecycle.
	Snapshots SnapshotClient
}

// Validate checks snapshot-copy dependencies without mounting the source PVC.
func (s SnapshotCopyStrategy) Validate(ctx context.Context, pvc Ref) error {
	if s.BackupStrategy == nil {
		return errors.New("snapshot-copy backup strategy is required")
	}

	return s.Snapshots.Validate(ctx, pvc)
}

// Backup creates a snapshot clone, streams it, and removes temporary resources.
//
// Snapshot and clone lifetimes are nested:
// the clone is removed first because it depends on the snapshot,
// then the snapshot is removed after the stream has completed or failed.
func (s SnapshotCopyStrategy) Backup(ctx context.Context, pvc Ref, dst io.Writer) (metadata Metadata, err error) {
	if ctx == nil || dst == nil {
		return Metadata{}, errors.New("snapshot-copy context and destination are required")
	}
	if err := s.Validate(ctx, pvc); err != nil {
		return Metadata{}, err
	}
	defer func() {
		if err == nil || IsCleanupOnly(err) {
			reportProgress(ctx, ProgressEvent{Stage: StageCompleted, Percent: 100})
		}
	}()

	// Progress percentages describe lifecycle milestones rather than bytes:
	// snapshot creation and clone readiness happen before the agent can stream any data,
	// while the nested strategy owns the middle range.
	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})

	snapshot, err := s.createReadySnapshot(ctx, pvc)
	if err != nil {
		return Metadata{}, err
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if cleanupErr := s.Snapshots.Delete(cleanupContext, snapshot); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("VolumeSnapshot %s/%s", snapshot.Namespace, snapshot.Name),
				Err:      cleanupErr,
			})
		}
	}()

	cloneName := clonePVCName(s.NamePrefix, pvc)
	clone, _, err := s.createClone(ctx, pvc, snapshot, cloneName, metav1.CreateOptions{})
	if err != nil {
		return Metadata{}, err
	}
	reportProgress(ctx, ProgressEvent{Stage: StageCloneCreated, Percent: 40})
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if cleanupErr := s.deleteClone(cleanupContext, clone); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("snapshot clone PVC %s/%s", clone.Namespace, clone.Name),
				Err:      cleanupErr,
			})
		}
	}()

	// A clone using WaitForFirstConsumer remains Pending until a consuming Pod exists.
	// PodStrategy.Backup creates that Pod and waits for it to become ready,
	// which both triggers binding and proves that the clone is mountable.
	innerContext := WithProgress(ctx, func(event ProgressEvent) {
		percent := min(40+event.Percent/2, 90)
		stage := event.Stage
		if stage == StagePreparing {
			stage = "preparing clone"
		}
		reportProgress(ctx, ProgressEvent{Stage: stage, Percent: percent})
	})

	// The clone may use WaitForFirstConsumer,
	// so the nested Pod strategy both binds the clone and transfers its contents.
	// Its progress is mapped into the 40..90 range reserved for data preparation and streaming.
	metadata, err = s.BackupStrategy.Backup(innerContext, clone, dst)
	if err != nil && !IsCleanupOnly(err) {
		return Metadata{}, err
	}

	metadata.Strategy = SnapshotCopy
	metadata.Portable = true
	if err := metadata.Validate(); err != nil {
		return Metadata{}, err
	}

	reportProgress(ctx, ProgressEvent{Stage: StageFinalizing, Percent: 95})

	return metadata, err
}

// createReadySnapshot creates one snapshot and waits for the CSI controller to make that object ready.
// The controller owns retrying the underlying CSI operation;
// kube-dump only removes the request after the task has failed.
func (s SnapshotCopyStrategy) createReadySnapshot(ctx context.Context, pvc Ref) (SnapshotHandle, error) {
	snapshot, err := s.Snapshots.create(
		ctx,
		pvc,
		snapshotName(s.NamePrefix, pvc),
		metav1.CreateOptions{},
	)
	if err != nil {
		return SnapshotHandle{}, fmt.Errorf(
			"create snapshot-copy source for PVC %s/%s: %w",
			pvc.Namespace,
			pvc.Name,
			err,
		)
	}

	reportProgress(ctx, ProgressEvent{Stage: StageSnapshotPending, Percent: 15})

	if err := s.Snapshots.WaitReady(ctx, snapshot); err != nil {
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		cleanupErr := s.Snapshots.Delete(cleanupContext, snapshot)
		cancel()
		if cleanupErr != nil {
			return SnapshotHandle{}, fmt.Errorf(
				"wait for snapshot-copy source for PVC %s/%s: %w; cleanup failed: %v",
				pvc.Namespace,
				pvc.Name,
				err,
				cleanupErr,
			)
		}

		return SnapshotHandle{}, fmt.Errorf(
			"wait for snapshot-copy source for PVC %s/%s: %w",
			pvc.Namespace,
			pvc.Name,
			err,
		)
	}

	reportProgress(ctx, ProgressEvent{Stage: StageSnapshotReady, Percent: 35})

	return snapshot, nil
}

// createClone creates a PVC whose dataSource is the ready VolumeSnapshot.
func (s SnapshotCopyStrategy) createClone(
	ctx context.Context,
	source Ref,
	snapshot SnapshotHandle,
	name string,
	options metav1.CreateOptions,
) (Ref, *unstructured.Unstructured, error) {
	object, err := s.cloneObject(ctx, source, snapshot, name)
	if err != nil {
		return Ref{}, nil, err
	}

	created, err := s.Snapshots.Dynamic.
		Resource(pvcResource).
		Namespace(source.Namespace).
		Create(ctx, object, options)
	if err != nil {
		return Ref{}, nil, fmt.Errorf("create snapshot clone PVC %q: %w", name, err)
	}

	return Ref{
		Namespace: source.Namespace,
		Name:      created.GetName(),
	}, object, nil
}

// cloneObject builds a temporary PVC that requests the source claim's storage
// and uses the supplied VolumeSnapshot as its data source.
func (s SnapshotCopyStrategy) cloneObject(
	ctx context.Context,
	source Ref,
	snapshot SnapshotHandle,
	name string,
) (*unstructured.Unstructured, error) {
	claim, err := s.Snapshots.Dynamic.
		Resource(pvcResource).
		Namespace(source.Namespace).
		Get(ctx, source.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get source PVC for snapshot clone: %w", err)
	}

	spec, found, err := unstructured.NestedMap(claim.Object, "spec")
	if err != nil || !found {
		return nil, errors.New("source PVC spec is required for snapshot clone")
	}

	cloneSpec := map[string]any{}
	for _, field := range []string{"accessModes", "resources", "storageClassName", "volumeMode"} {
		if value, found := spec[field]; found {
			cloneSpec[field] = value
		}
	}

	cloneSpec["dataSource"] = map[string]any{
		"apiGroup": snapshotGroup, "kind": "VolumeSnapshot", "name": snapshot.Name,
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{
			"generateName": name,
			"labels":       temporaryResourceLabels(source, snapshot.RunID),
		},
		"spec": cloneSpec,
	}}, nil
}

// deleteClone removes a temporary clone PVC idempotently.
func (s SnapshotCopyStrategy) deleteClone(ctx context.Context, clone Ref) error {
	err := s.Snapshots.Dynamic.
		Resource(pvcResource).
		Namespace(clone.Namespace).
		Delete(ctx, clone.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete snapshot clone PVC %q: %w", clone.Name, err)
	}

	return nil
}

// clonePVCName returns a DNS-compatible prefix for a temporary clone PVC.
func clonePVCName(prefix string, _ Ref) string {
	if prefix == "" {
		prefix = "kube-dump"
	}

	return prefix + "-clone-"
}
