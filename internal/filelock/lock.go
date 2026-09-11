// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package filelock provides a small cross-process advisory file lock.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const retryInterval = 50 * time.Millisecond

var errWouldBlock = errors.New("file lock is held")

// Lock is an advisory lock held by an open lock file.
type Lock struct {
	file *os.File
}

// Acquire waits until path can be locked or ctx is cancelled.
// The lock is OS-backed and therefore coordinates independent processes.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if ctx == nil {
		return nil, errors.New("file lock context is required")
	}
	if path == "" {
		return nil, errors.New("file lock path is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create file lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open file lock: %w", err)
	}

	for {
		err = tryLock(file)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if !errors.Is(err, errWouldBlock) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire file lock: %w", err)
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Close releases the OS lock and closes its lock file.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	unlockErr := unlock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
