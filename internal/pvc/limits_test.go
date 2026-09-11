// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import "testing"

func TestParseSizeSupportsKubernetesAndFriendlyIECForms(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]int64{
		"1Gi":   1 << 30,
		"1GiB":  1 << 30,
		"1Gib":  1 << 30,
		"100Mi": 100 << 20,
		"1G":    1_000_000_000,
	} {
		got, err := ParseSize(input)
		if err != nil {
			t.Fatalf("ParseSize(%q) error = %v", input, err)
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestSizeUnmarshalText(t *testing.T) {
	t.Parallel()
	var size Size
	if err := size.UnmarshalText([]byte("1GiB")); err != nil {
		t.Fatalf("Size.UnmarshalText() error = %v", err)
	}
	if size != Size(1<<30) {
		t.Fatalf("Size.UnmarshalText() = %d, want %d", size, 1<<30)
	}
}

func TestSizeLimits(t *testing.T) {
	t.Parallel()
	limits := SizeLimits{MaxPVCBytes: 10, MaxNamespaceBytes: 100}
	if skip, reason := limits.ShouldSkip(11, 20); !skip || reason == "" {
		t.Fatalf("individual limit result = %t, %q", skip, reason)
	}
	if skip, reason := limits.ShouldSkip(10, 101); !skip || reason == "" {
		t.Fatalf("namespace limit result = %t, %q", skip, reason)
	}
	if skip, reason := limits.ShouldSkip(10, 100); skip || reason != "" {
		t.Fatalf("within limits result = %t, %q", skip, reason)
	}
}
