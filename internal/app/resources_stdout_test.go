// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourceStdoutSinkWritesCanonicalPathComment(t *testing.T) {
	t.Parallel()

	object, err := state.NewObject(state.Identity{
		Group:     "apps",
		Version:   "v1",
		Resource:  "deployments",
		Kind:      "Deployment",
		Namespace: "ingress-nginx",
		Name:      "controller",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "controller",
			"namespace": "ingress-nginx",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	sink := newResourceStdoutSink(&output)
	defer func() { _ = sink.Close() }()
	changed, err := sink.Write(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("stdout sink reported a changed persistent object")
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	text := output.String()
	if !strings.Contains(text, "# ./apps/v1/deployments/ingress-nginx/controller.yaml\n") {
		t.Fatalf("stdout output does not contain canonical path comment:\n%s", text)
	}
	if !strings.Contains(text, "apiVersion: apps/v1\n") {
		t.Fatalf("stdout output does not contain serialized object:\n%s", text)
	}
}

func TestResourceStdoutSinkFlushesCanonicalPathOrder(t *testing.T) {
	t.Parallel()

	objects := []state.Object{}
	for _, name := range []string{"zulu", "alpha"} {
		object, err := state.NewObject(state.Identity{
			Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Name: name,
		}, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": name},
		}})
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}

	var output bytes.Buffer
	sink := newResourceStdoutSink(&output)
	defer func() { _ = sink.Close() }()
	for _, object := range objects {
		if _, err := sink.Write(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if alpha, zulu := strings.Index(
		output.String(),
		"# ./apps/v1/deployments/_cluster/alpha.yaml",
	), strings.Index(
		output.String(),
		"# ./apps/v1/deployments/_cluster/zulu.yaml",
	); alpha < 0 || zulu < 0 || alpha > zulu {
		t.Fatalf("stdout documents are not sorted by canonical path:\n%s", output.String())
	}
}

func TestResourcesStdoutCommandDisablesProgress(t *testing.T) {
	t.Parallel()

	if !commandDisablesProgress(&ResourcesStdoutCommand{}) {
		t.Fatal("stdout resource command did not disable progress")
	}
	if commandDisablesProgress(&ResourcesDirCommand{}) {
		t.Fatal("directory resource command unexpectedly disabled progress")
	}
}
