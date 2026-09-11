// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fields

import (
	"testing"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func BenchmarkEncryptSecretFields(b *testing.B) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		b.Fatal(err)
	}

	object := benchmarkSecretObject(b)
	options := Options{
		Mode:       Age,
		Recipients: []age.Recipient{identity.Recipient()},
	}
	paths := [][]string{{"data", "password"}, {"data", "token"}}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncryptFields(object, paths, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRestoreSecretFields(b *testing.B) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		b.Fatal(err)
	}

	object, err := EncryptFields(
		benchmarkSecretObject(b),
		[][]string{{"data", "password"}, {"data", "token"}},
		Options{
			Mode:       Age,
			Recipients: []age.Recipient{identity.Recipient()},
		},
	)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := RestoreFields(object, []age.Identity{identity}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncryptSecretFieldsAES256SIV(b *testing.B) {
	cipher, err := siv.New(make([]byte, siv.KeySize))
	if err != nil {
		b.Fatal(err)
	}

	object := benchmarkSecretObject(b)
	options := Options{
		Mode:      AES256SIV,
		SIVCipher: cipher,
		SIVKeyID:  "0123456789abcdef",
	}
	paths := [][]string{{"data", "password"}, {"data", "token"}}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncryptFields(object, paths, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRestoreSecretFieldsAES256SIV(b *testing.B) {
	cipher, err := siv.New(make([]byte, siv.KeySize))
	if err != nil {
		b.Fatal(err)
	}
	object, err := EncryptFields(
		benchmarkSecretObject(b),
		[][]string{{"data", "password"}, {"data", "token"}},
		Options{
			Mode:      AES256SIV,
			SIVCipher: cipher,
			SIVKeyID:  "0123456789abcdef",
		},
	)
	if err != nil {
		b.Fatal(err)
	}
	resolver := testSIVResolver{keyID: "0123456789abcdef", cipher: cipher}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := RestoreFields(object, nil, resolver); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkSecretObject(t testing.TB) state.Object {
	t.Helper()
	object, err := state.NewObject(
		state.Identity{
			Version:   "v1",
			Resource:  "secrets",
			Kind:      "Secret",
			Namespace: "production",
			Name:      "api-credentials",
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "api-credentials", "namespace": "production"},
			"type":       "Opaque",
			"data": map[string]any{
				"username": "R29sb3ZhY2hMZW5hCg==",
				"password": "0YfRgtC+LdGC0Yst0YXQvtGC0LXQuy3RgtGD0YIt0YPQstC40LTQtdGC0Yw/Cg==",
				"token":    "0L3QuNGF0YPRjy3RgdC10LHQtS3RgtGLLdC70Y7QsdC+0L/Ri9GC0L3Ri9C5IQo=",
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	return object
}
