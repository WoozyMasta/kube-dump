// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import "context"

type progressReporterKey struct{}

// ProgressStage identifies a visible phase of one PVC backup.
type ProgressStage string

const (
	// StagePreparing means that the source and transfer prerequisites are being prepared.
	StagePreparing ProgressStage = "preparing"
	// StageHelperReady means that a helper Pod is ready to access the PVC.
	StageHelperReady ProgressStage = "helper ready"
	// StageSnapshotPending means that Kubernetes accepted the snapshot request
	// and the CSI driver has not reported it ready yet.
	StageSnapshotPending ProgressStage = "snapshot pending"
	// StageSnapshotReady means that the CSI snapshot is ready to use.
	StageSnapshotReady ProgressStage = "snapshot ready"
	// StageCloneCreated means that the temporary snapshot clone PVC was created.
	StageCloneCreated ProgressStage = "clone created"
	// StageStreaming means that the PVC data is being streamed to a local destination.
	StageStreaming ProgressStage = "streaming"
	// StageUploading means that the helper Pod is uploading PVC data to S3.
	StageUploading ProgressStage = "uploading"
	// StageFinalizing means that the data stream and temporary resources are being finalized.
	StageFinalizing ProgressStage = "finalizing"
	// StageCompleted means that the PVC artifact was successfully produced.
	StageCompleted ProgressStage = "done"
)

// ProgressEvent reports a lifecycle phase and its weighted percentage.
// Percent is monotonic for one backup and ranges from 0 through 100.
type ProgressEvent struct {
	// Stage is the current operation phase shown beside the PVC identity.
	Stage ProgressStage
	// Percent is the lifecycle progress, not a percentage of PVC bytes.
	Percent int
}

// ProgressReporter receives progress updates from a PVC strategy.
type ProgressReporter func(ProgressEvent)

// WithProgress attaches a progress reporter to a PVC backup context.
// A nil context or reporter is returned unchanged
// so optional progress never changes the strategy contract.
func WithProgress(ctx context.Context, reporter ProgressReporter) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}

	return context.WithValue(ctx, progressReporterKey{}, reporter)
}

// reportProgress sends an optional lifecycle update attached to ctx.
func reportProgress(ctx context.Context, event ProgressEvent) {
	if ctx == nil {
		return
	}

	reporter, _ := ctx.Value(progressReporterKey{}).(ProgressReporter)
	if reporter != nil {
		reporter(event)
	}
}
