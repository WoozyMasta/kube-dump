// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestS3MoverHelperOptionsBuildDirectUploadCommand(t *testing.T) {
	t.Parallel()

	mover := &S3Mover{options: S3MoverOptions{
		Image:       "example/kube-dump:test",
		AgentBinary: "/bin/kube-dump",
		URI:         "s3://bucket/run-123",
		Endpoint:    "http://s3.example.test",
		Insecure:    true,
		Region:      "us-east-1",
		Compression: compress.Gzip,
		Recipients:  []string{"age1example"},
	}}
	claim := Ref{Namespace: "team-a", Name: "data"}
	options := mover.helperOptions(claim, "credentials", "20260902T120000Z")
	object := helperPodObject(claim, "kube-dump-mover-", options)

	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("read mover containers: %#v, found=%t error=%v", containers, found, err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("mover container has unexpected type %T", containers[0])
	}
	command, found, err := unstructured.NestedStringSlice(container, "command")
	if err != nil || !found {
		t.Fatalf("read mover command: found=%t error=%v", found, err)
	}

	wantObject := "volumes/" + state.EncodePathSegment(claim.Namespace) + "/" + state.EncodePathSegment(claim.Name) + "/20260902T120000Z"
	want := []string{
		"/bin/kube-dump",
		"volume",
		"upload",
		"s3://bucket/run-123",
		wantObject,
		"/data",
		"/dev/termination-log",
		"--strategy=pod",
		"--compression=gzip",
		"--retry-attempts=4",
		"--s3-endpoint=http://s3.example.test",
		"--s3-insecure",
		"--s3-region=us-east-1",
		"--recipient=age1example",
	}
	if got := command; len(got) != len(want) {
		t.Fatalf("mover command = %#v, want %#v", got, want)
	}
	for index := range want {
		if command[index] != want[index] {
			t.Fatalf("mover command = %#v, want %#v", command, want)
		}
	}
	if container["terminationMessagePolicy"] != "File" {
		t.Fatalf("termination message policy = %#v, want File", container["terminationMessagePolicy"])
	}

	envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
	if err != nil || !found || len(envFrom) != 1 {
		t.Fatalf("mover envFrom = %#v, found=%t error=%v", envFrom, found, err)
	}
	automount, found, err := unstructured.NestedBool(object.Object, "spec", "automountServiceAccountToken")
	if err != nil || !found || automount {
		t.Fatalf("mover automount token = %t, found=%t error=%v; static credentials should use Secret", automount, found, err)
	}
}

func TestS3MoverUsesServiceAccountWithoutStaticCredentials(t *testing.T) {
	t.Parallel()

	mover := &S3Mover{options: S3MoverOptions{
		Image: "example/kube-dump:test",
		URI:   "s3://bucket/run-123",
	}}
	options := mover.helperOptions(Ref{Namespace: "team-a", Name: "data"}, "", "20260902T120000Z")
	object := helperPodObject(Ref{Namespace: "team-a", Name: "data"}, "kube-dump-mover-", options)

	automount, found, err := unstructured.NestedBool(object.Object, "spec", "automountServiceAccountToken")
	if err != nil || !found || !automount {
		t.Fatalf("mover automount token = %t, found=%t error=%v; ambient credentials need service account token", automount, found, err)
	}
}

func TestNewS3MoverConfiguresDefaultTransferLimiter(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mover, err := NewS3Mover(S3MoverOptions{
		Dynamic: client,
		Image:   "example/kube-dump:test",
		URI:     "s3://bucket/run-123",
	})
	if err != nil {
		t.Fatalf("NewS3Mover() error = %v", err)
	}
	if mover.options.TransferLimiter == nil {
		t.Fatal("NewS3Mover() left the transfer limiter nil")
	}
	if got := cap(mover.options.TransferLimiter.slots); got != DefaultTransferConcurrency {
		t.Fatalf("default transfer concurrency = %d, want %d", got, DefaultTransferConcurrency)
	}
}

func TestNewS3MoverPreservesTransferLimiter(t *testing.T) {
	t.Parallel()

	limiter := NewTransferLimiter(1)
	mover, err := NewS3Mover(S3MoverOptions{
		Dynamic:         fake.NewSimpleDynamicClient(runtime.NewScheme()),
		Image:           "example/kube-dump:test",
		URI:             "s3://bucket/run-123",
		TransferLimiter: limiter,
	})
	if err != nil {
		t.Fatalf("NewS3Mover() error = %v", err)
	}
	if mover.options.TransferLimiter != limiter {
		t.Fatal("NewS3Mover() replaced the configured transfer limiter")
	}
}

func TestS3MoverPreservesMetadataWhenHelperCleanupFails(t *testing.T) {
	t.Parallel()
	metadata := NewMetadata(Pod, string(compress.Gzip))
	metadata.Portable = true
	metadata.ContentSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	resultData, err := json.Marshal(struct {
		Metadata Metadata `json:"metadata"`
	}{Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "mover"},
		}}, nil
	})
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "mover"},
			"status": map[string]any{
				"phase": "Succeeded",
				"containerStatuses": []any{map[string]any{
					"state": map[string]any{"terminated": map[string]any{
						"message": string(resultData),
					}},
				}},
			},
		}}, nil
	})
	client.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	mover, err := NewS3Mover(S3MoverOptions{
		Dynamic: client,
		Image:   "example/kube-dump:test",
		URI:     "s3://bucket/run-123",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := mover.Backup(context.Background(), Ref{Namespace: "default", Name: "data"}, io.Discard)
	if err == nil || !IsCleanupOnly(err) {
		t.Fatalf("S3Mover.Backup() error = %v, want cleanup-only warning", err)
	}
	if got != metadata {
		t.Fatalf("S3Mover.Backup() metadata = %#v, want %#v", got, metadata)
	}
}

func TestS3MoverWaitsForTransferSlotBeforeCreatingResources(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	limiter := NewTransferLimiter(1)
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatalf("occupy transfer slot: %v", err)
	}
	defer limiter.Release()

	mover, err := NewS3Mover(S3MoverOptions{
		Dynamic:         client,
		Image:           "example/kube-dump:test",
		URI:             "s3://bucket/run-123",
		TransferLimiter: limiter,
	})
	if err != nil {
		t.Fatalf("NewS3Mover() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = mover.Backup(ctx, Ref{Namespace: "default", Name: "data"}, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("S3Mover.Backup() error = %v, want context deadline exceeded", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			t.Fatalf("S3Mover.Backup() created %s before acquiring a transfer slot", action.GetResource().Resource)
		}
	}
}

func TestHelperPodWaitCompletedReadsTerminationMessage(t *testing.T) {
	t.Parallel()

	metadata := NewMetadata(Pod, string(compress.Gzip))
	metadata.ContentSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	metadata.Portable = true
	resultData, err := json.Marshal(struct {
		Metadata Metadata `json:"metadata"`
	}{Metadata: metadata})
	if err != nil {
		t.Fatalf("marshal mover result: %v", err)
	}

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "mover"},
			"status": map[string]any{
				"phase": "Succeeded",
				"containerStatuses": []any{map[string]any{
					"state": map[string]any{"terminated": map[string]any{
						"message": string(resultData),
					}},
				}},
			},
		}}, nil
	})

	message, err := (HelperPodOptions{Dynamic: client}).WaitCompleted(
		context.Background(),
		HelperPod{Namespace: "default", Name: "mover"},
		time.Second,
	)
	if err != nil {
		t.Fatalf("WaitCompleted() error = %v", err)
	}
	if message != string(resultData) {
		t.Fatalf("termination message = %q, want %q", message, resultData)
	}

}

func TestDecodeMoverResultAcceptsErrorWithoutMetadata(t *testing.T) {
	t.Parallel()

	metadata, err := decodeMoverResult(`{"error":"upload failed"}`)
	if err == nil || err.Error() != "upload failed" {
		t.Fatalf("decodeMoverResult() error = %v, want upload failed", err)
	}
	if metadata != (Metadata{}) {
		t.Fatalf("decodeMoverResult() metadata = %#v, want zero metadata", metadata)
	}
}
