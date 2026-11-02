package pvc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

type captureExecTransport struct {
	request ExecRequest
}

func (t *captureExecTransport) Exec(_ context.Context, request ExecRequest, _ io.Reader, _, _ io.Writer) error {
	t.request = request
	return nil
}

type outputExecTransport struct {
	output                     []byte
	err                        error
	exitWithoutAcknowledgement bool
}

type outputExecWithoutAcknowledgement struct {
	output  []byte
	request ExecRequest
}

func (t *outputExecWithoutAcknowledgement) Exec(_ context.Context, request ExecRequest, _ io.Reader, stdout, _ io.Writer) error {
	t.request = request
	_, err := stdout.Write(t.output)
	return err
}

func (t outputExecTransport) Exec(_ context.Context, _ ExecRequest, stdin io.Reader, stdout, _ io.Writer) error {
	acknowledgement := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(stdin)
		acknowledgement <- data
	}()

	if _, err := stdout.Write(t.output); err != nil {
		return err
	}
	if t.err != nil {
		return t.err
	}
	if t.exitWithoutAcknowledgement {
		return nil
	}
	if received := <-acknowledgement; string(received) != VolumeExportAcknowledgement {
		return errors.New("volume export acknowledgement is missing")
	}

	return nil
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestExecRequestValidation(t *testing.T) {
	t.Parallel()

	if err := (ExecRequest{Namespace: "default", Pod: "helper", Command: []string{"tar"}}).Validate(); err != nil {
		t.Fatalf("ExecRequest.Validate() error = %v", err)
	}
	if err := (ExecRequest{Namespace: "default", Pod: "helper"}).Validate(); err == nil {
		t.Fatal("ExecRequest.Validate() accepted an empty command")
	}
}

func TestRESTExecTransportValidation(t *testing.T) {
	t.Parallel()

	transport := RESTExecTransport{}
	if err := transport.Exec(nil, ExecRequest{}, nil, nil, nil); err == nil {
		t.Fatal("RESTExecTransport.Exec() accepted an invalid request")
	}
}

func TestPodStrategyExecUsesHelperMountPath(t *testing.T) {
	t.Parallel()

	transport := &captureExecTransport{}
	strategy := &PodStrategy{options: PodStrategyOptions{Exec: transport}}
	pod := HelperPod{
		Namespace:   "default",
		Name:        "helper",
		AgentBinary: "/bin/kube-dump",
		Container:   "helper",
		MountPath:   "/mounted volume",
	}

	want := []string{"/bin/kube-dump", "volume", "export", "/mounted volume"}
	if err := strategy.exec(context.Background(), pod, want, nil, io.Discard); err != nil {
		t.Fatalf("PodStrategy.exec() error = %v", err)
	}
	if !reflect.DeepEqual(transport.request.Command, want) {
		t.Fatalf("exec command = %#v, want %#v", transport.request.Command, want)
	}
}

func TestPodStrategyExecVerifiedArchiveRejectsTruncatedExecOutput(t *testing.T) {
	t.Parallel()

	archive := testVolumeArchive(t, 8*volumeTarBlockSize)
	truncated := archive[:len(archive)-3*volumeTarBlockSize]
	strategy := &PodStrategy{options: PodStrategyOptions{
		Exec: outputExecTransport{output: truncated, exitWithoutAcknowledgement: true},
	}}
	pod := HelperPod{AgentBinary: "/bin/kube-dump", MountPath: "/data"}

	_, err := strategy.execVerifiedArchive(context.Background(), pod, io.Discard)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("execVerifiedArchive() error = %v, want unexpected EOF", err)
	}
}

func TestPodStrategyExecVerifiedArchiveAcceptsCompleteOutput(t *testing.T) {
	t.Parallel()

	archive := testVolumeArchive(t, 8*volumeTarBlockSize)
	strategy := &PodStrategy{options: PodStrategyOptions{
		Exec: outputExecTransport{output: archive},
	}}
	pod := HelperPod{AgentBinary: "/bin/kube-dump", MountPath: "/data"}

	stats, err := strategy.execVerifiedArchive(context.Background(), pod, io.Discard)
	if err != nil {
		t.Fatalf("execVerifiedArchive() error = %v", err)
	}
	if stats.SizeBytes != int64(len(archive)) {
		t.Fatalf("validated bytes = %d, want %d", stats.SizeBytes, len(archive))
	}
}

func TestPodStrategyExecVerifiedArchiveAcceptsHelperCompressedOutput(t *testing.T) {
	t.Parallel()

	archive := testVolumeArchive(t, 8*volumeTarBlockSize)
	var compressed bytes.Buffer
	writer, err := compress.NewWriter(&compressed, compress.Config{Algorithm: compress.Gzip})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	transport := &outputExecWithoutAcknowledgement{output: compressed.Bytes()}
	strategy := &PodStrategy{options: PodStrategyOptions{
		Exec:            transport,
		Compression:     compress.Gzip,
		CompressionSide: CompressionSideHelper,
	}}
	pod := HelperPod{AgentBinary: "/bin/kube-dump", MountPath: "/data"}

	var stored bytes.Buffer
	stats, err := strategy.execVerifiedArchive(context.Background(), pod, &stored)
	if err != nil {
		t.Fatalf("execVerifiedArchive() error = %v", err)
	}
	if stats.SizeBytes != int64(len(archive)) {
		t.Fatalf("validated bytes = %d, want %d", stats.SizeBytes, len(archive))
	}
	if !bytes.Equal(stored.Bytes(), compressed.Bytes()) {
		t.Fatal("stored helper stream differs from compressed output")
	}
	wantCommand := []string{"/bin/kube-dump", "volume", "export", "/data", "--compression", "gzip"}
	if !reflect.DeepEqual(transport.request.Command, wantCommand) {
		t.Fatalf("helper command = %#v, want %#v", transport.request.Command, wantCommand)
	}
}

func TestPodStrategyExecVerifiedArchiveUnblocksAcknowledgementOnExecError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("exec transport disconnected")
	strategy := &PodStrategy{options: PodStrategyOptions{
		Exec: outputExecTransport{output: testVolumeArchive(t, 8*volumeTarBlockSize), err: wantErr},
	}}
	pod := HelperPod{AgentBinary: "/bin/kube-dump", MountPath: "/data"}
	result := make(chan error, 1)
	go func() {
		_, err := strategy.execVerifiedArchive(context.Background(), pod, io.Discard)
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, wantErr) {
			t.Fatalf("execVerifiedArchive() error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("execVerifiedArchive() deadlocked while closing a pending acknowledgement")
	}
}

func TestPodStrategyExecVerifiedArchiveReturnsIgnoredWriterError(t *testing.T) {
	t.Parallel()

	archive := testVolumeArchive(t, 8*volumeTarBlockSize)
	wantErr := errors.New("destination write failed")
	strategy := &PodStrategy{options: PodStrategyOptions{
		Exec: outputExecTransport{output: archive},
	}}
	pod := HelperPod{AgentBinary: "/bin/kube-dump", MountPath: "/data"}

	_, err := strategy.execVerifiedArchive(context.Background(), pod, failingWriter{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("execVerifiedArchive() error = %v, want %v", err, wantErr)
	}
}

func testVolumeArchive(t *testing.T, size int) []byte {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), bytes.Repeat([]byte{0x5a}, size), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), root, &archive); err != nil {
		t.Fatal(err)
	}

	return archive.Bytes()
}
