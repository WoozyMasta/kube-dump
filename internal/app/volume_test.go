// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
)

func TestVolumeCommandsRoundTrip(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	export := VolumeExportCommand{}
	export.commandContext = commandContext{ctx: context.Background()}
	export.commandStreams = commandStreams{output: &archive}
	export.Positional.Path = source

	if err := export.Execute(nil); err != nil {
		t.Fatalf("volume export: %v", err)
	}

	destination := t.TempDir()
	importCommand := VolumeImportCommand{}
	importCommand.commandContext = commandContext{ctx: context.Background()}
	importCommand.commandStreams = commandStreams{input: bytes.NewReader(archive.Bytes())}
	importCommand.Positional.Path = destination
	importCommand.Positional.Existing = "empty-only"

	if err := importCommand.Execute(nil); err != nil {
		t.Fatalf("volume import: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destination, "nested", "value"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("imported data = %q, want payload", data)
	}
}

func TestValidateVolumeUploadObject(t *testing.T) {
	t.Parallel()

	for _, object := range []string{"volumes/ns/pvc", "volumes/ns/pvc/data"} {
		if err := validateVolumeUploadObject(object); err != nil {
			t.Errorf("validateVolumeUploadObject(%q) error = %v", object, err)
		}
	}
	for _, object := range []string{"", "/volumes/ns/pvc", "volumes/../pvc", "volumes\\..\\pvc"} {
		if err := validateVolumeUploadObject(object); err == nil {
			t.Errorf("validateVolumeUploadObject(%q) succeeded, want error", object)
		}
	}
}

func TestWriteVolumeUploadStreamProducesPortableArchive(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "site"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "site", "index.txt"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	metadata, err := writeVolumeUploadStream(
		context.Background(),
		source,
		&archive,
		compress.Gzip,
		nil,
		pvc.Pod,
	)
	if err != nil {
		t.Fatalf("writeVolumeUploadStream() error = %v", err)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("uploaded metadata is invalid: %v", err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("open gzip archive: %v", err)
	}
	tarReader := tar.NewReader(reader)
	found := false
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if header.Name != "site/index.txt" {
			continue
		}

		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read tar payload: %v", err)
		}
		if string(data) != "payload" {
			t.Fatalf("tar payload = %q, want payload", data)
		}
		found = true
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip archive: %v", err)
	}
	if !found {
		t.Fatal("portable archive does not contain site/index.txt")
	}
	if metadata.ContentSHA256 == "" || strings.Trim(metadata.ContentSHA256, "0") == "" {
		t.Fatalf("metadata has an invalid content digest: %q", metadata.ContentSHA256)
	}
}

func TestWriteUploadErrorKeepsTerminationResultBounded(t *testing.T) {
	t.Parallel()

	resultPath := filepath.Join(t.TempDir(), "termination-log")
	command := VolumeUploadCommand{}
	command.Positional.Result = resultPath
	original := errors.New(strings.Repeat("backend failure: ", 10_000))
	if err := command.writeUploadError(original); !errors.Is(err, original) {
		t.Fatalf("writeUploadError() error = %v, want original error", err)
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxVolumeUploadResultBytes {
		t.Fatalf("termination result size = %d, want <= %d", len(data), maxVolumeUploadResultBytes)
	}

	var result volumeUploadResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("termination result is not valid JSON: %v", err)
	}
	if result.Metadata != nil {
		t.Fatal("error termination result unexpectedly contains metadata")
	}
	if !strings.HasSuffix(result.Error, "... [truncated]") {
		t.Fatalf("termination result error = %q, want truncation marker", result.Error)
	}
}

func TestVolumeDecodedStreamCloseReportsAllErrors(t *testing.T) {
	t.Parallel()

	first := errors.New("decoder close failed")
	second := errors.New("response close failed")
	stream := volumeDecodedStream{
		closers: []io.Closer{failingCloser{err: second}, failingCloser{err: first}},
	}

	err := stream.Close()
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("volumeDecodedStream.Close() error = %v, want both close errors", err)
	}
}

type failingCloser struct {
	err error
}

func (c failingCloser) Close() error {
	return c.err
}
