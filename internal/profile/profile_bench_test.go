// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func BenchmarkApplyBackupProfile(b *testing.B) {
	profile, err := Load("backup")
	if err != nil {
		b.Fatal(err)
	}

	object := benchmarkObject(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := profile.Apply(object); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadBackupProfile(b *testing.B) {
	data, err := Builtin("backup")
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := LoadBytes(data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkObject(t testing.TB) state.Object {
	t.Helper()
	object, err := state.NewObject(
		state.Identity{
			Group:     "apps",
			Version:   "v1",
			Resource:  "deployments",
			Kind:      "Deployment",
			Namespace: "production",
			Name:      "api",
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":              "api",
				"namespace":         "production",
				"uid":               "4d8b3d22-1a65-4a39-a0ec-76db772cc0c5",
				"resourceVersion":   "12345",
				"generation":        int64(7),
				"creationTimestamp": "2026-08-30T12:00:00Z",
				"managedFields":     []any{map[string]any{"manager": "controller"}},
			},
			"spec": map[string]any{
				"replicas": int64(3),
				"selector": map[string]any{"matchLabels": map[string]any{"app": "api"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "api"}},
					"spec": map[string]any{"containers": []any{
						map[string]any{"name": "api", "image": "ghcr.io/example/api:v1.2.3"},
					}},
				},
			},
			"status": map[string]any{
				"replicas":      int64(3),
				"readyReplicas": int64(3),
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	return object
}
