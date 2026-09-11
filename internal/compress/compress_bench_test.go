// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package compress

import (
	"bytes"
	"io"
	"testing"
)

func BenchmarkCompress(b *testing.B) {
	payload := bytes.Repeat(
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: application\ndata:\n  value: stable\n---\n"),
		512,
	)

	for _, algorithm := range []Algorithm{Zstandard, Gzip} {
		b.Run(string(algorithm), func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for b.Loop() {
				var output bytes.Buffer
				writer, err := NewWriter(&output, Config{Algorithm: algorithm})
				if err != nil {
					b.Fatal(err)
				}
				if _, err := writer.Write(payload); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecompress(b *testing.B) {
	payload := bytes.Repeat(
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: application\ndata:\n  value: stable\n---\n"),
		512,
	)

	for _, algorithm := range []Algorithm{Zstandard, Gzip} {
		var compressed bytes.Buffer
		writer, err := NewWriter(&compressed, Config{Algorithm: algorithm})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := writer.Write(payload); err != nil {
			b.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}

		data := compressed.Bytes()
		b.Run(string(algorithm), func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for b.Loop() {
				reader, err := NewReader(bytes.NewReader(data), algorithm)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, reader); err != nil {
					b.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
