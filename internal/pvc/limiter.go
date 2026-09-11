// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import "context"

// DefaultTransferConcurrency is the default number
// of PVC data streams that may run at the same time for one command invocation.
const DefaultTransferConcurrency = 2

// DefaultLifecycleConcurrency is the maximum number
// of PVC snapshot/clone lifecycles started concurrently by the application.
const DefaultLifecycleConcurrency = 4

// TransferLimiter bounds the number of PVC data streams active at once.
// Snapshot preparation stays outside this limit,
// so a slow CSI operation does not prevent a ready PVC from using an available transfer slot.
type TransferLimiter struct {
	slots chan struct{}
}

// NewTransferLimiter creates a transfer limiter with the requested capacity.
// A non-positive limit returns nil, which means that transfer is unlimited.
func NewTransferLimiter(limit int) *TransferLimiter {
	if limit <= 0 {
		return nil
	}

	return &TransferLimiter{slots: make(chan struct{}, limit)}
}

// Acquire waits until one transfer slot is available or the context ends.
func (l *TransferLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}

	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns one previously acquired transfer slot.
func (l *TransferLimiter) Release() {
	if l == nil {
		return
	}

	<-l.slots
}
