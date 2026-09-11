// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"reflect"
	"testing"
)

func TestProgressContextReportsEvents(t *testing.T) {
	var events []ProgressEvent
	ctx := WithProgress(context.Background(), func(event ProgressEvent) {
		events = append(events, event)
	})

	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})
	reportProgress(ctx, ProgressEvent{Stage: StageStreaming, Percent: 25})

	want := []ProgressEvent{
		{Stage: StagePreparing, Percent: 5},
		{Stage: StageStreaming, Percent: 25},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("reported events = %#v, want %#v", events, want)
	}
}

func TestProgressContextIsOptional(t *testing.T) {
	if got := WithProgress(nil, func(ProgressEvent) {}); got != nil {
		t.Fatal("WithProgress(nil, reporter) returned a non-nil context")
	}
	if got := WithProgress(context.Background(), nil); got == nil {
		t.Fatal("WithProgress(context, nil) returned a nil context")
	}

	reportProgress(context.Background(), ProgressEvent{Stage: StageCompleted, Percent: 100})
}
