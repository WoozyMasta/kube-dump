// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestInspectUsageSelectsRWOConsumerNode(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   map[string]any{"namespace": "default", "name": "data"},
			"spec":       map[string]any{"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem"},
		}},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"namespace": "default", "name": "consumer"},
			"spec": map[string]any{
				"nodeName": "node-a",
				"volumes":  []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "data"}}},
			},
		}},
	)

	usage, err := InspectUsage(context.Background(), client, Ref{Namespace: "default", Name: "data"})
	if err != nil {
		t.Fatalf("InspectUsage() error = %v", err)
	}
	node, err := SelectHelperNode(usage)
	if err != nil {
		t.Fatalf("SelectHelperNode() error = %v", err)
	}
	if node != "node-a" {
		t.Fatalf("selected node = %q, want node-a", node)
	}
}

func TestSelectHelperNodeRejectsAmbiguousOrRWOPUsage(t *testing.T) {
	t.Parallel()
	for name, usage := range map[string]Usage{
		"ambiguous RWO": {
			AccessModes: []string{"ReadWriteOnce"},
			PodNames:    []string{"a", "b"},
			Nodes:       []string{"node-a", "node-b"},
		},
		"in-use RWOP": {
			AccessModes: []string{"ReadWriteOncePod"},
			PodNames:    []string{"a"},
			Nodes:       []string{"node-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SelectHelperNode(usage); err == nil {
				t.Fatal("SelectHelperNode() unexpectedly succeeded")
			}
		})
	}
}

func TestPodReferencesPVC(t *testing.T) {
	t.Parallel()
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "consumer"},
		"spec": map[string]any{
			"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "data"}}},
		},
	}}
	if !podReferencesPVC(pod, "data") {
		t.Fatal("podReferencesPVC() returned false for referenced PVC")
	}
}

func TestValidateRestoreTarget(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"namespace":       "default",
				"name":            "target",
				"uid":             "target-uid",
				"resourceVersion": "1",
			},
			"spec":   map[string]any{"volumeMode": "Filesystem"},
			"status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "1Gi"}},
		}},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"namespace": "default", "name": "unrelated"},
		}},
	)

	if _, err := ValidateRestoreTarget(
		context.Background(),
		client,
		Ref{Namespace: "default", Name: "target"},
		1<<30,
	); err != nil {
		t.Fatalf("ValidateRestoreTarget() error = %v", err)
	}
	if _, err := ValidateRestoreTarget(
		context.Background(),
		client,
		Ref{Namespace: "default", Name: "target"},
		1<<30+1,
	); err == nil {
		t.Fatal("ValidateRestoreTarget() accepted a payload larger than the target PVC")
	}

	consumer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": "default", "name": "consumer"},
		"spec": map[string]any{
			"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "target"}}},
		},
	}}

	if _, err := client.
		Resource(podListResource).
		Namespace("default").
		Create(context.Background(), consumer, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateRestoreTarget(
		context.Background(),
		client,
		Ref{Namespace: "default", Name: "target"},
		0,
	); err == nil {
		t.Fatal("ValidateRestoreTarget() accepted an in-use PVC")
	}
}

func TestInspectUsageFallsBackToRequestedCapacity(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   map[string]any{"namespace": "default", "name": "target"},
			"spec": map[string]any{
				"resources": map[string]any{
					"requests": map[string]any{"storage": "2Gi"},
				},
			},
			"status": map[string]any{"phase": "Bound"},
		}},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"namespace": "default", "name": "unrelated"},
		}},
	)

	usage, err := InspectUsage(context.Background(), client, Ref{Namespace: "default", Name: "target"})
	if err != nil {
		t.Fatalf("InspectUsage() error = %v", err)
	}
	if usage.CapacityBytes != 2<<30 {
		t.Fatalf("capacity = %d, want %d", usage.CapacityBytes, 2<<30)
	}
}
