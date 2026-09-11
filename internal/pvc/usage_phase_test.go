// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestInspectUsageIgnoresTerminalPVCConsumers(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   map[string]any{"namespace": "default", "name": "data"},
			"spec":       map[string]any{"volumeMode": "Filesystem"},
			"status":     map[string]any{"phase": "Bound"},
		}},
		podUsingPVCPhase("succeeded", "Succeeded", "node-a"),
		podUsingPVCPhase("failed", "Failed", "node-b"),
		podUsingPVCPhase("running", "Running", "node-c"),
		podUsingPVCPhase("pending", "Pending", "node-d"),
		podUsingPVCPhase("unknown", "Unknown", "node-e"),
		podUsingPVCPhase("missing-phase", "", "node-f"),
	)

	usage, err := InspectUsage(context.Background(), client, Ref{Namespace: "default", Name: "data"})
	if err != nil {
		t.Fatalf("InspectUsage() error = %v", err)
	}

	wantNames := []string{"missing-phase", "pending", "running", "unknown"}
	if got := usage.PodNames; !slices.Equal(got, wantNames) {
		t.Fatalf("active Pod names = %v, want %v", got, wantNames)
	}
	wantNodes := []string{"node-c", "node-d", "node-e", "node-f"}
	if got := usage.Nodes; !slices.Equal(got, wantNodes) {
		t.Fatalf("active Pod nodes = %v, want %v", got, wantNodes)
	}
}

func podUsingPVCPhase(name, phase, node string) *unstructured.Unstructured {
	status := map[string]any{}
	if phase != "" {
		status["phase"] = phase
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{
			"nodeName": node,
			"volumes":  []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "data"}}},
		},
		"status": status,
	}}
}
