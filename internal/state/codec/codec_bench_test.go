// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package codec

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func BenchmarkMarshalResource(b *testing.B) {
	object := benchmarkCodecObject(b)

	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(object); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalResource(b *testing.B) {
	data, err := Marshal(benchmarkCodecObject(b))
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := Unmarshal(data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkCodecObject(t testing.TB) state.Object {
	t.Helper()
	object, err := state.NewObject(
		state.Identity{
			Version:   "v1",
			Resource:  "configmaps",
			Kind:      "ConfigMap",
			Namespace: "production",
			Name:      "application",
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "application",
				"namespace": "production",
				"labels":    map[string]any{"app": "api", "environment": "production"},
			},
			"data": map[string]any{
				"application.yaml": "server:\n  port: 8080\nfeatures:\n  cache: true\n",
				"message":          "a stable configuration value",
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	return object
}
