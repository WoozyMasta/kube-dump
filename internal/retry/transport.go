// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package retry

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"syscall"
	"time"
)

const (
	defaultBaseDelay = 250 * time.Millisecond
	defaultMaxDelay  = 5 * time.Second
)

// Config controls the replay-safe HTTP retry transport.
type Config struct {
	// Attempts is the maximum number of attempts, including the initial request.
	Attempts int
	// BaseDelay is the initial exponential backoff delay.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff delay.
	MaxDelay time.Duration
}

// Transport retries replay-safe HTTP requests after transient network failures
// and temporary server responses. Requests with a non-rewindable body are sent once.
type Transport struct {
	base      http.RoundTripper
	attempts  int
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewTransport creates a retry transport over base.
func NewTransport(base http.RoundTripper, config Config) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}

	config.Attempts = NormalizeAttempts(config.Attempts)
	if config.BaseDelay <= 0 {
		config.BaseDelay = defaultBaseDelay
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = defaultMaxDelay
	}

	return &Transport{
		base:      base,
		attempts:  config.Attempts,
		baseDelay: config.BaseDelay,
		maxDelay:  config.MaxDelay,
	}
}

// RoundTrip executes the request and retries only when replaying it is safe.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil {
		return http.DefaultTransport.RoundTrip(req)
	}

	if t.base == nil || req == nil || !replayableMethod(req.Method) ||
		t.attempts < 2 || (req.Body != nil && req.GetBody == nil) {
		return t.roundTrip(req)
	}

	// Rewind the request body before every retry.
	// A request with a body is retry-safe only when the standard library supplied a fresh copy.
	for attempt := 0; ; attempt++ {
		response, err := t.roundTrip(req)
		if attempt+1 >= t.attempts || !retryable(response, err) {
			return response, err
		}

		if response != nil {
			_ = response.Body.Close()
		}
		if req.Body != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return response, err
			}

			req.Body = body
		}

		if err := wait(req.Context(), backoff(attempt, t.baseDelay, t.maxDelay)); err != nil {
			return nil, err
		}
	}
}

// roundTrip delegates to the configured transport
// so the retry loop has one call site and nil transports can be handled by RoundTrip.
func (t *Transport) roundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

// replayableMethod lists methods that can be repeated without changing server state.
func replayableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// retryable classifies only transient responses and transport failures.
// Authentication, validation, and other permanent responses must reach callers immediately.
func retryable(response *http.Response, err error) bool {
	if err != nil {
		return transientError(err)
	}

	if response == nil {
		return true
	}

	switch response.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true

	default:
		return false
	}
}

// transientError recognizes connection failures that commonly recover on a later attempt.
// Cancellation and deadline errors are deliberately excluded so Ctrl+C is not delayed by retries.
func transientError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}

	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

// backoff calculates capped exponential delay with cryptographic jitter.
// Jitter prevents concurrent clients from retrying the same upstream at once.
func backoff(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	delay := baseDelay
	for range attempt {
		if delay >= maxDelay/2 {
			delay = maxDelay
			break
		}
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(delay/2)+1))
	if err != nil {
		return delay
	}

	return delay/2 + time.Duration(jitter.Int64())
}

// wait sleeps without making cancellation wait for the full backoff interval.
func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
