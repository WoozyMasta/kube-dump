// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package age

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxRecipientSourceSize int64 = 1 << 20
	recipientSourceTimeout       = 15 * time.Second
)

// LoadRecipientValues loads recipient lines
// from local files, HTTP(S) URLs, and public GitHub user key endpoints.
// The returned values are suitable for both local parsing and forwarding to a PVC helper Pod.
func LoadRecipientValues(files, urls, githubUsers []string) ([]string, error) {
	if err := validateRecipientSources(urls, githubUsers); err != nil {
		return nil, err
	}

	values := make([]string, 0)

	for _, path := range files {
		data, err := readKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("read recipient file %q: %w", path, err)
		}

		values = appendRecipientLines(values, data)
	}

	for _, source := range urls {
		data, err := loadRecipientURL(source)
		if err != nil {
			return nil, fmt.Errorf("read recipient URL %q: %w", source, err)
		}

		values = appendRecipientLines(values, data)
	}

	for _, username := range githubUsers {
		source, err := githubRecipientURL(username)
		if err != nil {
			return nil, err
		}

		data, err := loadRecipientURL(source)
		if err != nil {
			return nil, fmt.Errorf("read GitHub recipients for user %q: %w", username, err)
		}

		values = appendRecipientLines(values, data)
	}

	if len(values) == 0 {
		return nil, nil
	}

	return values, nil
}

// loadRecipientURL accepts HTTP and HTTPS sources and bounds both connection time
// and response size because recipients are configuration, not an unbounded data stream.
func loadRecipientURL(source string) ([]byte, error) {
	return loadRecipientURLWithClient(source, newRecipientHTTPClient())
}

// loadRecipientURLWithClient is split out so URL parsing
// and response limits can be tested without making network requests to external services.
func loadRecipientURLWithClient(source string, client *http.Client) ([]byte, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if err := validateRecipientURL(parsed); err != nil {
		return nil, err
	}

	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "text/plain, application/json")
	request.Header.Set("User-Agent", "kube-dump")

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxRecipientSourceSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > maxRecipientSourceSize {
		return nil, fmt.Errorf("response exceeds %d bytes", maxRecipientSourceSize)
	}

	return data, nil
}

// validateRecipientSources checks every source identifier
// before any source is read or requested, so a malformed later value cannot cause partial work.
func validateRecipientSources(urls, githubUsers []string) error {
	for _, source := range urls {
		parsed, err := url.Parse(source)
		if err != nil {
			return fmt.Errorf("parse recipient URL: %w", err)
		}
		if err := validateRecipientURL(parsed); err != nil {
			return err
		}
	}

	for _, username := range githubUsers {
		if _, err := githubRecipientURL(username); err != nil {
			return err
		}
	}

	return nil
}

// validateRecipientURL checks the transport and authority without making a request.
func validateRecipientURL(parsed *url.URL) error {
	if parsed == nil {
		return errors.New("recipient URL is empty")
	}

	if !isRecipientURLScheme(parsed.Scheme) || parsed.Host == "" {
		return fmt.Errorf("recipient URL must use HTTP or HTTPS: %q", parsed.String())
	}

	return nil
}

// newRecipientHTTPClient limits recipient URL requests
// and prevents HTTPS sources from being downgraded to an unencrypted transport by redirects.
func newRecipientHTTPClient() *http.Client {
	return &http.Client{
		Timeout: recipientSourceTimeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if !isRecipientURLScheme(next.URL.Scheme) {
				return fmt.Errorf("redirect target must use HTTP or HTTPS: %q", next.URL.String())
			}

			if len(via) > 0 && strings.EqualFold(via[0].URL.Scheme, "https") &&
				!strings.EqualFold(next.URL.Scheme, "https") {
				return fmt.Errorf("HTTPS recipient URL cannot redirect to HTTP: %q", next.URL.String())
			}

			return nil
		},
	}
}

// isRecipientURLScheme reports whether a URL uses a supported transport.
func isRecipientURLScheme(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

// githubRecipientURL returns GitHub's public SSH-key endpoint for a user.
func githubRecipientURL(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("GitHub recipient username is empty")
	}
	if strings.ContainsAny(username, "/?#") {
		return "", fmt.Errorf("invalid GitHub recipient username %q", username)
	}

	return "https://github.com/" + url.PathEscape(username) + ".keys", nil
}

// appendRecipientLines removes blank and comment lines while preserving the recipient text,
// including optional SSH comments after a public key.
func appendRecipientLines(values []string, data []byte) []string {
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		values = append(values, line)
	}

	return values
}
