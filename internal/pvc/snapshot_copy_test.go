// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"io"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type recordingBackupStrategy struct {
	claim Ref
}

func (s *recordingBackupStrategy) Backup(_ context.Context, pvc Ref, _ io.Writer) (Metadata, error) {
	s.claim = pvc
	metadata := NewMetadata(Pod, "gzip")
	metadata.ContentSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	return metadata, nil
}

func TestSnapshotCopyWaitsThroughCSIStatusFailure(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())

	var created []string
	var deleted []string
	getAttempts := 0
	client.PrependReactor(
		"create",
		"volumesnapshots",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			name := "snapshot-single"
			created = append(created, name)

			return true, &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": name},
			}}, nil
		})

	client.PrependReactor(
		"get",
		"volumesnapshots",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			getAttempts++
			name := action.(ktesting.GetAction).GetName()
			object := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": name},
			}}
			if getAttempts == 1 {
				object.Object["status"] = map[string]any{
					"error": map[string]any{"message": "CSI operation is still in progress"},
				}
			} else {
				object.Object["status"] = map[string]any{"readyToUse": true}
			}

			return true, object, nil
		})

	client.PrependReactor(
		"delete",
		"volumesnapshots",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			deleted = append(deleted, action.(ktesting.DeleteAction).GetName())
			return true, nil, nil
		})

	strategy := SnapshotCopyStrategy{
		NamePrefix: "kube-dump",
		Snapshots: SnapshotClient{
			Dynamic: client,
			Discovery: &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{
				Resources: []*metav1.APIResourceList{{
					GroupVersion: snapshotGroupVersion,
				}},
			}},
		},
	}

	snapshot, err := strategy.createReadySnapshot(context.Background(), Ref{Namespace: "default", Name: "data"})
	if err != nil {
		t.Fatalf("createReadySnapshot() error = %v", err)
	}
	if snapshot.Name != "snapshot-single" {
		t.Fatalf("snapshot name = %q, want snapshot-single", snapshot.Name)
	}
	if !reflect.DeepEqual(created, []string{"snapshot-single"}) {
		t.Fatalf("created snapshots = %#v", created)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted snapshots = %#v, want no retry cleanup", deleted)
	}
}

func TestSnapshotCopyBacksUpCloneAndCleansUpInDependencyOrder(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      "data",
			"namespace": "default",
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": "1Gi"},
			},
			"storageClassName": "csi-hostpath",
			"volumeMode":       "Filesystem",
		},
	}})
	cleanup := make([]string, 0, 2)
	client.PrependReactor("create", "volumesnapshots", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": snapshotGroupVersion,
			"kind":       "VolumeSnapshot",
			"metadata":   map[string]any{"name": "snapshot-1", "namespace": "default"},
		}}, nil
	})
	client.PrependReactor("get", "volumesnapshots", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": snapshotGroupVersion,
			"kind":       "VolumeSnapshot",
			"metadata":   map[string]any{"name": "snapshot-1", "namespace": "default"},
			"status":     map[string]any{"readyToUse": true},
		}}, nil
	})
	client.PrependReactor("create", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   map[string]any{"name": "clone-1", "namespace": "default"},
		}}, nil
	})
	client.PrependReactor("delete", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		cleanup = append(cleanup, "pvc/"+action.(ktesting.DeleteAction).GetName())
		return true, nil, nil
	})
	client.PrependReactor("delete", "volumesnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		cleanup = append(cleanup, "snapshot/"+action.(ktesting.DeleteAction).GetName())
		return true, nil, nil
	})

	backup := &recordingBackupStrategy{}
	var events []ProgressEvent
	strategy := SnapshotCopyStrategy{
		BackupStrategy: backup,
		NamePrefix:     "kube-dump",
		Snapshots: SnapshotClient{
			Dynamic: client,
			Discovery: &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{
				Resources: []*metav1.APIResourceList{{GroupVersion: snapshotGroupVersion}},
			}},
		},
	}

	metadata, err := strategy.Backup(
		WithProgress(context.Background(), func(event ProgressEvent) {
			events = append(events, event)
		}),
		Ref{Namespace: "default", Name: "data"},
		io.Discard,
	)
	if err != nil {
		t.Fatalf("SnapshotCopyStrategy.Backup() error = %v", err)
	}
	if backup.claim != (Ref{Namespace: "default", Name: "clone-1"}) {
		t.Fatalf("backup claim = %#v, want clone-1", backup.claim)
	}
	if metadata.Strategy != SnapshotCopy || !metadata.Portable {
		t.Fatalf("snapshot-copy metadata = %#v", metadata)
	}
	if !reflect.DeepEqual(cleanup, []string{"pvc/clone-1", "snapshot/snapshot-1"}) {
		t.Fatalf("cleanup order = %#v, want clone before snapshot", cleanup)
	}
	wantEvents := []ProgressEvent{
		{Stage: StagePreparing, Percent: 5},
		{Stage: StageSnapshotPending, Percent: 15},
		{Stage: StageSnapshotReady, Percent: 35},
		{Stage: StageCloneCreated, Percent: 40},
		{Stage: StageFinalizing, Percent: 95},
		{Stage: StageCompleted, Percent: 100},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("snapshot-copy progress events = %#v, want %#v", events, wantEvents)
	}
}
