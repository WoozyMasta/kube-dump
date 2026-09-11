// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package age

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadRecipientURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("# comment\nage1example\nssh-ed25519 AAAA comment\n"))
	}))
	defer server.Close()

	source, err := loadRecipientURLWithClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("load recipient URL: %v", err)
	}

	values := appendRecipientLines(nil, source)
	if want := []string{"age1example", "ssh-ed25519 AAAA comment"}; !equalStrings(values, want) {
		t.Fatalf("recipient values = %#v, want %#v", values, want)
	}
}

func TestLoadRecipientHTTPURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("age1example\n"))
	}))
	defer server.Close()

	data, err := loadRecipientURLWithClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("load HTTP recipient URL: %v", err)
	}
	if string(data) != "age1example\n" {
		t.Fatalf("recipient data = %q, want %q", data, "age1example\n")
	}
}

func TestLoadRecipientValuesValidatesSourcesBeforeFetching(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = writer.Write([]byte("age1example\n"))
	}))
	defer server.Close()

	if _, err := LoadRecipientValues(nil, []string{server.URL}, []string{"invalid/name"}); err == nil {
		t.Fatal("expected invalid GitHub username to be rejected")
	}
	if requests != 0 {
		t.Fatalf("recipient URL requests = %d, want 0", requests)
	}
}

func TestLoadRecipientURLRejectsHTTPSDowngrade(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("age1example\n"))
	}))
	defer httpServer.Close()

	httpsServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", httpServer.URL)
		writer.WriteHeader(http.StatusFound)
	}))
	defer httpsServer.Close()

	client := httpsServer.Client()
	policy := newRecipientHTTPClient()
	client.CheckRedirect = policy.CheckRedirect
	client.Timeout = policy.Timeout

	if _, err := loadRecipientURLWithClient(httpsServer.URL, client); err == nil {
		t.Fatal("expected HTTPS-to-HTTP redirect to be rejected")
	}
}

func TestGitHubRecipientURL(t *testing.T) {
	source, err := githubRecipientURL("WoozyMasta")
	if err != nil {
		t.Fatalf("build GitHub recipient URL: %v", err)
	}
	if want := "https://github.com/WoozyMasta.keys"; source != want {
		t.Fatalf("GitHub recipient URL = %q, want %q", source, want)
	}

	for _, username := range []string{"", "user/name", "user?keys"} {
		if _, err := githubRecipientURL(username); err == nil {
			t.Fatalf("expected invalid GitHub username %q to be rejected", username)
		}
	}
}

func equalStrings(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}
