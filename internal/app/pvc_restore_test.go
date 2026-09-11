// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	"go.yaml.in/yaml/v3"
)

func TestSpoolRestoreArtifactKeepsStableSource(t *testing.T) {
	t.Parallel()

	source, err := os.CreateTemp(t.TempDir(), "artifact-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if _, err := source.WriteString("verified archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	spool, err := spoolRestoreArtifact(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()

	if _, err := source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(5); err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(spool)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "verified archive" {
		t.Fatalf("spooled data = %q, want original artifact", data)
	}
}

func TestPVCRestoreResultTreatsCleanupWarningAsSuccess(t *testing.T) {
	t.Parallel()

	warning := &pvc.CleanupWarning{Resource: "helper Pod test/pod", Err: errors.New("delete failed")}
	if err := pvcRestoreResult(pvc.Ref{Namespace: "test", Name: "data"}, warning); err != nil {
		t.Fatalf("pvcRestoreResult() error = %v, want nil", err)
	}

	failure := errors.New("restore failed")
	if err := pvcRestoreResult(pvc.Ref{Namespace: "test", Name: "data"}, failure); !errors.Is(err, failure) {
		t.Fatalf("pvcRestoreResult() error = %v, want restore failure", err)
	}
}

func TestFindS3PVCArtifactSkipsIncompleteLatestRevision(t *testing.T) {
	claim := pvc.Ref{Namespace: "prod", Name: "data"}
	base := "backup/volumes/prod/data"
	metadata := pvc.NewMetadata(pvc.Pod, string(compress.Gzip))
	metadata.ContentSHA256 = strings.Repeat("0", 64)
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}

	objects := []s3TestObject{
		{Key: base + "/20260904T020000Z/metadata.yaml", Body: metadataData},
		{Key: base + "/20260904T010000Z/metadata.yaml", Body: metadataData},
		{Key: base + "/20260904T010000Z/data.tar.gz", Body: []byte("archive")},
		{Key: "backup/volumes/prod/other/20260904T030000Z/metadata.yaml", Body: metadataData},
	}
	server := httptest.NewServer(s3TestHandler(t, "bucket", objects))
	defer server.Close()

	store, err := export.NewS3Store(context.Background(), export.S3Options{
		URI:       "s3://bucket/backup",
		Endpoint:  server.URL,
		Insecure:  true,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	dataKey, _, err := findS3PVCArtifact(context.Background(), store, claim, "latest")
	if err != nil {
		t.Fatal(err)
	}

	want := base + "/20260904T010000Z/data.tar.gz"
	if dataKey != want {
		t.Fatalf("selected data key = %q, want %q", dataKey, want)
	}
}

func TestFindS3PVCArtifactUsesCaptureTimeForLatest(t *testing.T) {
	claim := pvc.Ref{Namespace: "prod", Name: "data"}
	base := "backup/volumes/prod/data"
	metadata := pvc.NewMetadata(pvc.Pod, string(compress.Gzip))
	metadata.ContentSHA256 = strings.Repeat("0", 64)
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}

	older := base + "/20260904T020000.000000001Z-ffffffffffffffff"
	newer := base + "/20260904T020000.000000002Z-0000000000000000"
	objects := []s3TestObject{
		{Key: older + "/metadata.yaml", Body: metadataData},
		{Key: older + "/data.tar.gz", Body: []byte("old archive")},
		{Key: newer + "/metadata.yaml", Body: metadataData},
		{Key: newer + "/data.tar.gz", Body: []byte("new archive")},
	}
	server := httptest.NewServer(s3TestHandler(t, "bucket", objects))
	defer server.Close()

	store, err := export.NewS3Store(context.Background(), export.S3Options{
		URI:       "s3://bucket/backup",
		Endpoint:  server.URL,
		Insecure:  true,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	dataKey, _, err := findS3PVCArtifact(context.Background(), store, claim, "latest")
	if err != nil {
		t.Fatal(err)
	}

	want := newer + "/data.tar.gz"
	if dataKey != want {
		t.Fatalf("selected data key = %q, want %q", dataKey, want)
	}
}

func TestRotatePVCS3RunsUsesCaptureTime(t *testing.T) {
	metadata := pvc.NewMetadata(pvc.Pod, string(compress.Gzip))
	metadata.ContentSHA256 = strings.Repeat("0", 64)
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}

	base := "backup/volumes/prod/data"
	older := base + "/20260904T020000.000000001Z-ffffffffffffffff"
	newer := base + "/20260904T020000.000000002Z-0000000000000000"
	objects := []s3TestObject{
		{Key: older + "/metadata.yaml", Body: metadataData},
		{Key: older + "/data.tar.gz", Body: []byte("old archive")},
		{Key: newer + "/metadata.yaml", Body: metadataData},
		{Key: newer + "/data.tar.gz", Body: []byte("new archive")},
	}
	byKey := make(map[string][]byte, len(objects))
	for _, object := range objects {
		byKey[object.Key] = object.Body
	}
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Has("delete") {
			data, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, readErr.Error(), http.StatusBadRequest)
				return
			}
			var payload struct {
				Objects []struct {
					Key string `xml:"Key"`
				} `xml:"Object"`
			}
			if err := xml.Unmarshal(data, &payload); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			for _, object := range payload.Objects {
				deleted = append(deleted, object.Key)
			}
			_, _ = fmt.Fprint(writer, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)
			return
		}
		if query.Get("list-type") == "2" {
			prefix := query.Get("prefix")
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(writer, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>bucket</Name>`)
			for _, object := range objects {
				if !strings.HasPrefix(object.Key, prefix) {
					continue
				}
				_, _ = fmt.Fprintf(writer,
					`<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>"etag"</ETag><Size>%d</Size></Contents>`,
					object.Key, time.Now().UTC().Format(time.RFC3339), len(object.Body))
			}
			_, _ = fmt.Fprint(writer, `</ListBucketResult>`)
			return
		}

		key := strings.TrimPrefix(request.URL.Path, "/bucket/")
		body, found := byKey[key]
		if !found {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("ETag", `"etag"`)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	store, err := export.NewS3Store(context.Background(), export.S3Options{
		URI:       "s3://bucket/backup",
		Endpoint:  server.URL,
		Insecure:  true,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	removed, err := rotatePVCS3Runs(context.Background(), store, 1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed revisions = %d, want 1", removed)
	}
	if len(deleted) != 2 || !strings.Contains(strings.Join(deleted, "\n"), older) {
		t.Fatalf("deleted objects = %v, want the older revision %q", deleted, older)
	}
	if strings.Contains(strings.Join(deleted, "\n"), newer) {
		t.Fatalf("newer revision was deleted: %v", deleted)
	}
}

func TestFindS3PVCArtifactReportsSkippedLatestRevisions(t *testing.T) {
	claim := pvc.Ref{Namespace: "prod", Name: "data"}
	metadata := pvc.NewMetadata(pvc.Pod, string(compress.Gzip))
	metadata.ContentSHA256 = strings.Repeat("0", 64)
	metadataData, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(s3TestHandler(t, "bucket", []s3TestObject{
		{Key: "backup/volumes/prod/data/20260904T020000Z/metadata.yaml", Body: metadataData},
	}))
	defer server.Close()

	store, err := export.NewS3Store(context.Background(), export.S3Options{
		URI:       "s3://bucket/backup",
		Endpoint:  server.URL,
		Insecure:  true,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = findS3PVCArtifact(context.Background(), store, claim, "latest")
	if err == nil || !strings.Contains(err.Error(), "no valid S3 PVC artifacts") {
		t.Fatalf("findS3PVCArtifact() error = %v, want skipped-revision diagnostic", err)
	}
}

type s3TestObject struct {
	Key  string
	Body []byte
}

func s3TestHandler(t *testing.T, bucket string, objects []s3TestObject) http.Handler {
	t.Helper()
	byKey := make(map[string][]byte, len(objects))
	for _, object := range objects {
		byKey[object.Key] = object.Body
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("list-type") == "2" {
			prefix := request.URL.Query().Get("prefix")
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(writer, `<?xml version="1.0" encoding="UTF-8"?>`)
			_, _ = fmt.Fprintf(writer, `<ListBucketResult><Name>%s</Name>`, bucket)
			for _, object := range objects {
				if !strings.HasPrefix(object.Key, prefix) {
					continue
				}

				_, _ = fmt.Fprintf(writer,
					`<Contents><Key>%s</Key><LastModified>2026-09-04T02:00:00Z</LastModified><ETag>"etag"</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>`,
					object.Key, len(object.Body))
			}
			_, _ = fmt.Fprint(writer, `</ListBucketResult>`)
			return
		}

		key := strings.TrimPrefix(request.URL.Path, "/"+bucket+"/")
		key, err := url.PathUnescape(key)
		if err != nil {
			http.Error(writer, "bad key", http.StatusBadRequest)
			return
		}

		body, found := byKey[path.Clean(key)]
		if !found {
			http.NotFound(writer, request)
			return
		}

		writer.Header().Set("ETag", `"etag"`)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	})
}
