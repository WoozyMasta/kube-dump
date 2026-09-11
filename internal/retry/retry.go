// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package retry provides shared retry configuration and replay-safe HTTP transport.
package retry

import "context"

// DefaultAttempts is the default maximum number of attempts for one retryable request.
const DefaultAttempts = 4

type attemptsKey struct{}

// WithAttempts attaches the maximum request attempts to a command context.
func WithAttempts(ctx context.Context, attempts int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, attemptsKey{}, NormalizeAttempts(attempts))
}

// NormalizeAttempts returns the configured attempts or the default value when unset.
func NormalizeAttempts(attempts int) int {
	if attempts < 1 {
		return DefaultAttempts
	}

	return attempts
}

// Attempts returns the configured maximum number of attempts for one request.
func Attempts(ctx context.Context) int {
	if ctx == nil {
		return DefaultAttempts
	}

	value, ok := ctx.Value(attemptsKey{}).(int)
	if !ok {
		return DefaultAttempts
	}

	return NormalizeAttempts(value)
}
