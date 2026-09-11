package pvc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSnapshotCreateOmitsClassByDefault(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "volumesnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		spec, found, err := unstructured.NestedMap(object.Object, "spec")
		if err != nil || !found {
			t.Fatalf("snapshot spec = %#v, found = %t, error = %v", spec, found, err)
		}
		if _, found := spec["volumeSnapshotClassName"]; found {
			t.Fatal("snapshot unexpectedly selected a VolumeSnapshotClass")
		}

		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "snapshot-default"},
		}}, nil
	})

	snapshot, err := (SnapshotClient{
		Dynamic:   client,
		Discovery: &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}},
	}).create(context.Background(), Ref{Namespace: "default", Name: "data"}, "kube-dump-snapshot-", metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if snapshot.ClassName != "" {
		t.Fatalf("snapshot class = %q, want empty", snapshot.ClassName)
	}
}

func TestSnapshotCreateIncludesRunIDLabel(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "volumesnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		labels, found, err := unstructured.NestedStringMap(object.Object, "metadata", "labels")
		if err != nil || !found {
			t.Fatalf("snapshot labels = %#v, found = %t, error = %v", labels, found, err)
		}
		if labels["kube-dump/run-id"] != "20260902T120000.000000000Z" {
			t.Fatalf("snapshot run label = %q", labels["kube-dump/run-id"])
		}

		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "snapshot-run"},
		}}, nil
	})

	snapshot, err := (SnapshotClient{
		Dynamic:   client,
		Discovery: &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}},
		RunID:     "20260902T120000.000000000Z",
	}).create(context.Background(), Ref{Namespace: "default", Name: "data"}, "kube-dump-snapshot-", metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if snapshot.RunID != "20260902T120000.000000000Z" {
		t.Fatalf("snapshot run ID = %q", snapshot.RunID)
	}
}

func TestSnapshotWaitReadyRetriesTransportError(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	attempts := 0
	client.PrependReactor("get", "volumesnapshots", func(ktesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, net.UnknownNetworkError("connection reset")
		}

		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "snapshot-ready", "namespace": "default"},
			"status":   map[string]any{"readyToUse": true},
		}}, nil
	})

	snapshotClient := SnapshotClient{Dynamic: client, ReadyTimeout: 3 * time.Second}
	err := snapshotClient.WaitReady(context.Background(), SnapshotHandle{Namespace: "default", Name: "snapshot-ready"})
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("WaitReady() GET attempts = %d, want 2", attempts)
	}
}

func TestSnapshotWaitReadyIncludesLastCSIErrorOnTimeout(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "volumesnapshots", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "snapshot-pending", "namespace": "default"},
			"status": map[string]any{
				"error": map[string]any{"message": "CSI operation is still in progress"},
			},
		}}, nil
	})

	err := (SnapshotClient{Dynamic: client, ReadyTimeout: 10 * time.Millisecond}).WaitReady(
		context.Background(),
		SnapshotHandle{Namespace: "default", Name: "snapshot-pending"},
	)
	if err == nil || !strings.Contains(err.Error(), "CSI operation is still in progress") {
		t.Fatalf("WaitReady() error = %v, want the last CSI error", err)
	}
}

func TestSnapshotCreateUsesExplicitClass(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "volumesnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		spec, _, _ := unstructured.NestedMap(object.Object, "spec")
		if got, _ := spec["volumeSnapshotClassName"].(string); got != "snapshot-class" {
			t.Fatalf("snapshot class = %q, want snapshot-class", got)
		}

		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "snapshot-explicit"},
		}}, nil
	})

	snapshot, err := (SnapshotClient{
		Dynamic:   client,
		Discovery: &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}},
		ClassName: "snapshot-class",
	}).create(context.Background(), Ref{Namespace: "default", Name: "data"}, "kube-dump-snapshot-", metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if snapshot.ClassName != "snapshot-class" {
		t.Fatalf("snapshot class = %q, want snapshot-class", snapshot.ClassName)
	}
}
