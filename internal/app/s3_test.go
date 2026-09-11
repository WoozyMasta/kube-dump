// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/export"
)

func TestResourceGenerationPointerData(t *testing.T) {
	generation := "0123456789abcdef0123456789abcdef"

	data, err := resourceGenerationPointerData(generation)
	if err != nil {
		t.Fatalf("resourceGenerationPointerData() error = %v", err)
	}

	var pointer resourceGenerationPointer
	if err := json.Unmarshal(data, &pointer); err != nil {
		t.Fatalf("decode generated pointer: %v", err)
	}
	if pointer.Version != resourceGenerationVersion || pointer.Generation != generation {
		t.Fatalf("pointer = %+v, want version %d and generation %q", pointer, resourceGenerationVersion, generation)
	}
}

func TestValidateResourceGenerationPointerRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	for _, generation := range []string{"", "../outside", "0123456789abcdef", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"} {
		t.Run(generation, func(t *testing.T) {
			err := validateResourceGenerationPointer(resourceGenerationPointer{
				Version:    resourceGenerationVersion,
				Generation: generation,
			})
			if err == nil {
				t.Fatal("unsafe generation was accepted")
			}
		})
	}
}

func TestResourceGenerationRetentionCandidatesKeepsReaderSafetyWindow(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	current := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	predecessor := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	old := "cccccccccccccccccccccccccccccccc"
	malformed := "not-a-generation"

	objects := []export.StoredObject{
		{Key: "resources/generations/" + current + "/resources/current.yaml", LastModified: now},
		{Key: "resources/generations/" + predecessor + "/resources/previous.yaml", LastModified: now.Add(-2 * time.Hour)},
		{Key: "resources/generations/" + old + "/resources/old.yaml", LastModified: now.Add(-3 * time.Hour)},
		{Key: "resources/generations/" + malformed + "/resources/foreign.yaml", LastModified: now.Add(-24 * time.Hour)},
		{Key: "resources/generations/outside/resources/foreign.yaml", LastModified: now.Add(-24 * time.Hour)},
	}

	got := resourceGenerationRetentionCandidates(objects, current, now)
	if len(got) != 1 || got[0] != old {
		t.Fatalf("retention candidates = %v, want [%q]", got, old)
	}
}

func TestNewS3StoreValidatesEndpointBeforeSecretFile(t *testing.T) {
	t.Parallel()

	_, err := newS3Store(context.Background(), S3Destination{
		URI:           "s3://bucket/backup",
		Endpoint:      "http://s3.example.test",
		SecretKeyFile: "missing-secret-file",
	})
	if err == nil || !strings.Contains(err.Error(), "requires --s3-insecure") {
		t.Fatalf("newS3Store() error = %v, want endpoint validation before secret file access", err)
	}
}

func TestUploadResourceTreeUploadsCompleteGeneration(t *testing.T) {
	t.Parallel()

	var (
		mutex sync.Mutex
		keys  []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		mutex.Lock()
		keys = append(keys, strings.TrimPrefix(request.URL.Path, "/bucket/"))
		mutex.Unlock()
		response.Header().Set("ETag", `"new"`)
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := export.NewS3Store(context.Background(), export.S3Options{
		URI:       "s3://bucket",
		Endpoint:  server.URL,
		Region:    "us-east-1",
		AccessKey: "access",
		SecretKey: "secret",
		Prefix:    "resources/generations/generation",
		Insecure:  true,
	})
	if err != nil {
		t.Fatalf("create S3 store: %v", err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".kube-dump", "crypto", "keys"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "resources", "apps", "v1", "deployments", "prod"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		filepath.Join(root, ".kube-dump", "crypto", "keys", "siv-key.age"):                "key",
		filepath.Join(root, ".kube-dump", "crypto", "metadata.yaml"):                      "metadata",
		filepath.Join(root, "resources", "apps", "v1", "deployments", "prod", "app.yaml"): "resource",
	} {
		if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := uploadResourceTree(context.Background(), store, root); err != nil {
		t.Fatalf("uploadResourceTree() error = %v", err)
	}
	mutex.Lock()
	gotKeys := append([]string(nil), keys...)
	mutex.Unlock()
	wantKeys := []string{
		"resources/generations/generation/.kube-dump/crypto/keys/siv-key.age",
		"resources/generations/generation/.kube-dump/crypto/metadata.yaml",
		"resources/generations/generation/resources/apps/v1/deployments/prod/app.yaml",
	}
	if strings.Join(gotKeys, "\n") != strings.Join(wantKeys, "\n") {
		t.Fatalf("uploaded keys = %v, want %v", gotKeys, wantKeys)
	}
}
