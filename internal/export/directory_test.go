// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package export

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDirectorySinkDetectsCaseOnlyChanges(t *testing.T) {
	t.Parallel()

	sink, err := NewDirectorySink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := state.Identity{
		Version: "v1", Resource: "configmaps", Kind: "ConfigMap",
		Namespace: "default", Name: "settings",
	}
	object := func(value string) state.Object {
		result, err := state.NewObject(identity, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "settings", "namespace": "default"},
			"data":       map[string]any{"value": value},
		}})
		if err != nil {
			t.Fatal(err)
		}

		return result
	}

	if changed, err := sink.Write(context.Background(), object("A")); err != nil || !changed {
		t.Fatalf("first Write() = changed:%v, error:%v", changed, err)
	}
	if changed, err := sink.Write(context.Background(), object("a")); err != nil || !changed {
		t.Fatalf("case-only Write() = changed:%v, error:%v", changed, err)
	}
}

func TestDirectorySinkPreservesMtimeForUnchangedObject(t *testing.T) {
	t.Parallel()

	sink, err := NewDirectorySink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := state.Identity{
		Version: "v1", Resource: "configmaps", Kind: "ConfigMap",
		Namespace: "default", Name: "settings",
	}
	object, err := state.NewObject(identity, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "settings", "namespace": "default"},
		"data":       map[string]any{"value": "same"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if changed, err := sink.Write(context.Background(), object); err != nil || !changed {
		t.Fatalf("first Write() = changed:%v, error:%v", changed, err)
	}
	path, err := sink.objectPath(identity)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if changed, err := sink.Write(context.Background(), object); err != nil || changed {
		t.Fatalf("unchanged Write() = changed:%v, error:%v", changed, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("unchanged Write() changed mtime from %v to %v", before.ModTime(), after.ModTime())
	}
}

func TestDirectorySinkEscapesWindowsUnsafeObjectName(t *testing.T) {
	t.Parallel()

	sink, err := NewDirectorySink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := state.Identity{
		Group:    "rbac.authorization.k8s.io",
		Version:  "v1",
		Resource: "clusterrolebindings",
		Kind:     "ClusterRoleBinding",
		Name:     "metrics-server:system:auth-delegator",
	}

	path, err := sink.objectPath(identity)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(path), "metrics-server%3Asystem%3Aauth-delegator.yaml"; got != want {
		t.Fatalf("objectPath() base = %q, want %q", got, want)
	}
}

func TestCanonicalObjectPathUsesPortableSeparators(t *testing.T) {
	t.Parallel()

	path, err := CanonicalObjectPath(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments",
		Kind: "Deployment", Namespace: "ingress-nginx", Name: "controller",
	})
	if err != nil {
		t.Fatal(err)
	}

	const want = "apps/v1/deployments/ingress-nginx/controller.yaml"
	if path != want {
		t.Fatalf("CanonicalObjectPath() = %q, want %q", path, want)
	}
}
