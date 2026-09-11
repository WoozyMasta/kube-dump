// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"bytes"
	"strconv"
	"testing"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func BenchmarkResourcePipeline(b *testing.B) {
	compiled, err := profile.Load("backup")
	if err != nil {
		b.Fatal(err)
	}
	object := benchmarkResourceObject(b)

	ageIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		b.Fatal(err)
	}
	sivCipher, err := siv.New(make([]byte, siv.KeySize))
	if err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name    string
		options fieldcrypto.Options
	}{
		{
			name: "plain",
			options: fieldcrypto.Options{
				Mode: fieldcrypto.Plain,
			}},
		{
			name: "age",
			options: fieldcrypto.Options{
				Mode:       fieldcrypto.Age,
				Recipients: []age.Recipient{ageIdentity.Recipient()},
			}},
		{
			name: "aes-siv",
			options: fieldcrypto.Options{
				Mode:      fieldcrypto.AES256SIV,
				SIVCipher: sivCipher,
				SIVKeyID:  "0123456789abcdef",
			}},
	}

	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			data, err := renderResourceManifest(object, compiled, test.options)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()

			for b.Loop() {
				if _, err := renderResourceManifest(object, compiled, test.options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkResourceArchive measures the complete resource path for a small archive.
// The compressor is created once per archive,
// matching production use where many resource manifests share one output stream.
func BenchmarkResourceArchive(b *testing.B) {
	compiled, err := profile.Load("backup")
	if err != nil {
		b.Fatal(err)
	}
	object := benchmarkResourceObject(b)

	ageIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		b.Fatal(err)
	}
	sivCipher, err := siv.New(make([]byte, siv.KeySize))
	if err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name    string
		options fieldcrypto.Options
	}{
		{
			name: "plain",
			options: fieldcrypto.Options{
				Mode: fieldcrypto.Plain,
			},
		},
		{
			name: "age",
			options: fieldcrypto.Options{
				Mode:       fieldcrypto.Age,
				Recipients: []age.Recipient{ageIdentity.Recipient()},
			},
		},
		{
			name: "aes-siv",
			options: fieldcrypto.Options{
				Mode:      fieldcrypto.AES256SIV,
				SIVCipher: sivCipher,
				SIVKeyID:  "0123456789abcdef",
			},
		},
	}

	const resourceCount = 128
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			manifest, err := renderResourceManifest(object, compiled, test.options)
			if err != nil {
				b.Fatal(err)
			}

			_, err = renderResourceArchive(
				object,
				compiled,
				test.options,
				resourceCount,
			)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(resourceCount * len(manifest)))
			b.ReportAllocs()

			for b.Loop() {
				if _, err := renderResourceArchive(
					object,
					compiled,
					test.options,
					resourceCount,
				); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// renderResourceManifest runs one resource through normalization, field encryption,
// and manifest encoding without creating an archive writer.
func renderResourceManifest(
	object state.Object,
	compiled *profile.CompiledProfile,
	options fieldcrypto.Options,
) ([]byte, error) {
	normalized, err := compiled.Apply(object)
	if err != nil {
		return nil, err
	}

	encrypted, err := fieldcrypto.EncryptFields(
		normalized,
		compiled.EncryptionPaths(normalized),
		options,
	)
	if err != nil {
		return nil, err
	}
	manifest, err := codec.Marshal(encrypted)
	if err != nil {
		return nil, err
	}

	return manifest, nil
}

// renderResourceArchive renders resourceCount manifests into one compressed archive
// so codec initialization is measured once per archive operation.
func renderResourceArchive(
	object state.Object,
	compiled *profile.CompiledProfile,
	options fieldcrypto.Options,
	resourceCount int,
) ([]byte, error) {
	var output bytes.Buffer
	writer, err := archive.NewBackupWriter(
		&output,
		archive.Config{Compression: compress.Config{Algorithm: compress.Zstandard}},
	)
	if err != nil {
		return nil, err
	}

	for index := range resourceCount {
		manifest, err := renderResourceManifest(object, compiled, options)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Add(archive.Entry{
			Path:   "core/v1/secrets/production/api-credentials-" + strconv.Itoa(index) + ".yaml",
			Mode:   0o640,
			Size:   int64(len(manifest)),
			Reader: bytes.NewReader(manifest),
		}); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return output.Bytes(), nil
}

// benchmarkResourceObject returns a Secret fixture with removable and encrypted fields.
func benchmarkResourceObject(t testing.TB) state.Object {
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
			"metadata": map[string]any{
				"name":              "api-credentials",
				"namespace":         "production",
				"uid":               "4d8b3d22-1a65-4a39-a0ec-76db772cc0c5",
				"resourceVersion":   "12345",
				"creationTimestamp": "2026-08-30T12:00:00Z",
			},
			"type": "Opaque",
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
