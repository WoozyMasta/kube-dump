// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"testing"
	"time"
)

func TestTransferLimiterBlocksUntilRelease(t *testing.T) {
	t.Parallel()

	limiter := NewTransferLimiter(1)
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := limiter.Acquire(context.Background()); err == nil {
			close(acquired)
		}
	}()

	select {
	case <-acquired:
		t.Fatal("second transfer acquired a busy slot")
	case <-time.After(20 * time.Millisecond):
	}

	limiter.Release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second transfer did not acquire a released slot")
	}
	limiter.Release()
}
