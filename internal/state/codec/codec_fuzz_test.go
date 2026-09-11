// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package codec

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func FuzzUnmarshal(f *testing.F) {
	f.Add([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config\ndata:\n  value: stable\n"))
	f.Add([]byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: secret\ndata:\n  password: !kube-dump/age ciphertext\n"))
	f.Add([]byte("---\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		object, err := Unmarshal(data)
		if err != nil {
			return
		}

		if object == nil || object.Object == nil {
			t.Fatal("successful YAML decode returned an empty object")
		}

		repeated, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("second YAML decode failed: %v", err)
		}
		if !reflect.DeepEqual(repeated.Object, object.Object) {
			t.Fatal("YAML decoding changed the object between identical runs")
		}
	})
}

func FuzzMarshalUnmarshal(f *testing.F) {
	f.Add([]byte("stable payload"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		const maxFuzzPayload = 64 << 10
		if len(payload) > maxFuzzPayload {
			return
		}

		identity := state.Identity{
			Version:  "v1",
			Resource: "configmaps",
			Kind:     "ConfigMap",
			Name:     "config",
		}
		value := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "config",
			},
			"data": map[string]any{
				"payload": base64.StdEncoding.EncodeToString(payload),
			},
		}}
		object, err := state.NewObject(identity, value)
		if err != nil {
			t.Fatalf("state.NewObject() error: %v", err)
		}

		encoded, err := Marshal(object)
		if err != nil {
			t.Fatalf("Marshal() error: %v", err)
		}
		decoded, err := Unmarshal(encoded)
		if err != nil {
			t.Fatalf("Unmarshal() rejected Marshal() output: %v", err)
		}
		if !reflect.DeepEqual(decoded.Object, object.Value.Object) {
			t.Fatal("YAML round-trip changed the Kubernetes object")
		}

		decodedObject, err := state.NewObject(identity, decoded)
		if err != nil {
			t.Fatalf("state.NewObject() after decode error: %v", err)
		}
		reencoded, err := Marshal(decodedObject)
		if err != nil {
			t.Fatalf("second Marshal() error: %v", err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatal("canonical YAML encoding changed after a round-trip")
		}
	})
}
