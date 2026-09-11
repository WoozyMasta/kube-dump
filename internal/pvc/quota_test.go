// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestValidateBackupQuotasRejectsInsufficientSnapshotCopyCapacity(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), resourceQuota(
		"default-resource-quota",
		"default",
		map[string]string{
			"pods":                   "4",
			"persistentvolumeclaims": "1",
			"count/volumesnapshots.snapshot.storage.k8s.io": "4",
		},
		map[string]string{
			"pods":                   "0",
			"persistentvolumeclaims": "1",
			"count/volumesnapshots.snapshot.storage.k8s.io": "0",
		},
	))

	err := ValidateBackupQuotas(
		context.Background(),
		client,
		[]Ref{{Namespace: "default", Name: "data"}},
		SnapshotCopy,
		1,
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "snapshot clone PVCs") {
		t.Fatalf("ValidateBackupQuotas() error = %v, want clone PVC quota error", err)
	}
}

func TestValidateBackupQuotasUsesRequestedConcurrency(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), resourceQuota(
		"pod-quota",
		"default",
		map[string]string{"pods": "2"},
		map[string]string{"pods": "1"},
	))
	claims := []Ref{
		{Namespace: "default", Name: "first"},
		{Namespace: "default", Name: "second"},
	}

	if err := ValidateBackupQuotas(context.Background(), client, claims, Pod, 1, false); err != nil {
		t.Fatalf("ValidateBackupQuotas() with concurrency 1 error = %v", err)
	}
	if err := ValidateBackupQuotas(context.Background(), client, claims, Pod, 2, false); err == nil {
		t.Fatal("ValidateBackupQuotas() with concurrency 2 succeeded despite insufficient Pod quota")
	}
}

func resourceQuota(
	name, namespace string,
	hard, used map[string]string,
) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]any{
			"hard": quotaMap(hard),
			"used": quotaMap(used),
		},
	}}
}

func quotaMap(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}

	return result
}
