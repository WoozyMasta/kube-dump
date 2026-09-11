// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package images

import "testing"

func FuzzNormalizeReference(f *testing.F) {
	for _, seed := range []string{"nginx:latest", "ghcr.io/example/api:v1.2.3", "docker://alpine"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		normalized, _, err := NormalizeReference(value)
		if err != nil {
			return
		}

		if normalized == "" {
			t.Fatal("successful image normalization returned an empty reference")
		}

		repeated, _, err := NormalizeReference(normalized)
		if err != nil || repeated != normalized {
			t.Fatalf("image normalization is not idempotent: %q -> %q, error=%v", normalized, repeated, err)
		}
	})
}

func FuzzParsePlatform(f *testing.F) {
	for _, seed := range []string{"linux/amd64", "linux/arm64/v8", "windows/amd64"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		platform, err := ParsePlatform(value)
		if err != nil {
			return
		}

		encoded := platform.String()
		decoded, err := ParsePlatform(encoded)
		if err != nil || decoded != platform {
			t.Fatalf("platform is not stable after String(): %q -> %#v, error=%v", encoded, decoded, err)
		}
	})
}
