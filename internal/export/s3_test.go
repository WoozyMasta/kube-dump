// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package export

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestValidateEndpointRequiresExplicitInsecureOptIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		insecure bool
		wantErr  bool
	}{
		{name: "empty", endpoint: ""},
		{name: "https", endpoint: "https://s3.example.test"},
		{name: "http requires opt in", endpoint: "http://s3.example.test", wantErr: true},
		{name: "http opt in", endpoint: "http://s3.example.test", insecure: true},
		{name: "loopback requires opt in", endpoint: "https://127.0.0.1:9000", wantErr: true},
		{name: "loopback opt in", endpoint: "http://127.0.0.1:9000", insecure: true},
		{name: "unsupported scheme", endpoint: "file:///tmp/s3", wantErr: true},
		{name: "userinfo rejected", endpoint: "https://user:pass@s3.example.test", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateEndpoint(test.endpoint, test.insecure)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateEndpoint(%q, %t) error = %v, wantErr=%t", test.endpoint, test.insecure, err, test.wantErr)
			}
		})
	}
}

func TestIsS3NotFoundUsesAPIErrorCode(t *testing.T) {
	err := fmt.Errorf("head object: %w", &smithy.GenericAPIError{Code: "NoSuchKey"})
	if !isS3NotFound(err) {
		t.Fatal("isS3NotFound() = false, want true for NoSuchKey")
	}

	err = fmt.Errorf("head object: %w", &smithy.GenericAPIError{Code: "AccessDenied"})
	if isS3NotFound(err) {
		t.Fatal("isS3NotFound() = true, want false for AccessDenied")
	}

	if isS3NotFound(errors.New("object not found")) {
		t.Fatal("isS3NotFound() accepted an untyped message")
	}
}

func TestS3DirectoryPrefixKeepsSiblingPrefixesOut(t *testing.T) {
	if got := s3DirectoryPrefix("backup"); got != "backup/" {
		t.Fatalf("s3DirectoryPrefix() = %q, want backup/", got)
	}
	if got := s3DirectoryPrefix(""); got != "" {
		t.Fatalf("s3DirectoryPrefix(empty) = %q, want empty prefix", got)
	}

	for _, test := range []struct {
		prefix string
		key    string
		want   string
		ok     bool
	}{
		{prefix: "backup/resources", key: "backup/resources/apps/v1/deployments.yaml", want: "apps/v1/deployments.yaml", ok: true},
		{prefix: "backup/resources", key: "backup/resources-old/object", ok: false},
		{prefix: "", key: "object", want: "object", ok: true},
	} {
		got, ok := s3RelativeKey(test.prefix, test.key)
		if got != test.want || ok != test.ok {
			t.Fatalf("s3RelativeKey(%q, %q) = %q, %t; want %q, %t", test.prefix, test.key, got, ok, test.want, test.ok)
		}
	}
}

func TestObjectIdentityRequiresVersionMarker(t *testing.T) {
	if err := (ObjectIdentity{}).Validate(); err == nil {
		t.Fatal("ObjectIdentity.Validate() accepted an unpinnable object")
	}

	for _, identity := range []ObjectIdentity{
		{ETag: `"etag"`},
		{VersionID: "version-id"},
	} {
		if err := identity.Validate(); err != nil {
			t.Fatalf("ObjectIdentity.Validate(%#v) error = %v", identity, err)
		}
	}
}

func TestReadObjectLimitedRejectsOversizedBody(t *testing.T) {
	data, err := readObjectLimited(strings.NewReader("abcd"), 3)
	if err == nil {
		t.Fatal("readObjectLimited() accepted an oversized object")
	}
	if data != nil {
		t.Fatalf("readObjectLimited() returned data on error: %q", data)
	}

	data, err = readObjectLimited(strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatalf("readObjectLimited() error = %v", err)
	}
	if string(data) != "abc" {
		t.Fatalf("readObjectLimited() = %q, want abc", data)
	}
}

func TestWriteFileAtomicPreservesExistingFileOnSourceError(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &failingReader{data: []byte("new"), err: errors.New("stream failed")}
	if err := writeFileAtomic(destination, source); err == nil {
		t.Fatal("writeFileAtomic() accepted a failed source")
	}

	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Fatalf("destination changed after failed write: %q", data)
	}
}

func TestWriteFileAtomicPublishesCompleteFile(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "object")
	if err := writeFileAtomic(destination, bytes.NewReader([]byte("new"))); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("destination = %q, want new", data)
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}

	count := copy(buffer, r.data)
	r.data = r.data[count:]
	return count, nil
}
