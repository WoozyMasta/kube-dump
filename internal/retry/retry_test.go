// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package retry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWithAttempts(t *testing.T) {
	tests := []struct {
		name    string
		context context.Context
		want    int
	}{
		{name: "default", context: context.Background(), want: DefaultAttempts},
		{name: "configured", context: WithAttempts(context.Background(), 2), want: 2},
		{name: "non-positive", context: WithAttempts(context.Background(), 0), want: DefaultAttempts},
		{name: "nil", context: nil, want: DefaultAttempts},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Attempts(test.context); got != test.want {
				t.Fatalf("Attempts() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestTransportRetriesTransientResponse(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if requests < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport, Config{
		Attempts:  3,
		BaseDelay: time.Nanosecond,
		MaxDelay:  time.Nanosecond,
	})}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer response.Body.Close()
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestTransportDoesNotRetryNonReplayableMethods(t *testing.T) {
	requests := 0
	transport := NewTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("temporary failure")
	}), Config{Attempts: 3, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond})

	request, err := http.NewRequest(http.MethodPost, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.RoundTrip(request)
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestTransportRetriesConnectionReset(t *testing.T) {
	requests := 0
	transport := NewTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests < 2 {
			return nil, syscall.ECONNRESET
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    request,
		}, nil
	}), Config{Attempts: 2, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond})

	request, err := http.NewRequest(http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() failed: %v", err)
	}
	defer response.Body.Close()
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
