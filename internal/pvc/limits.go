// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// SizeLimits controls automatic PVC backup skipping.
// Limits are evaluated from measured requested/capacity bytes before backup data is streamed,
// so skipped PVCs do not create partial artifacts.
type SizeLimits struct {
	// MaxPVCBytes skips an individual PVC above this size; zero disables it.
	MaxPVCBytes int64
	// MaxNamespaceBytes skips every PVC in a namespace above this total; zero disables it.
	MaxNamespaceBytes int64
}

// Size is a non-negative byte quantity parsed with Kubernetes quantity rules.
type Size int64

// UnmarshalText parses Kubernetes and human-friendly binary quantity suffixes.
func (s *Size) UnmarshalText(value []byte) error {
	bytes, err := ParseSize(string(value))
	if err != nil {
		return err
	}

	*s = Size(bytes)
	return nil
}

// ParseSize parses Kubernetes quantities, including human-friendly binary
// suffixes such as 1GiB in addition to the native 1Gi form.
func ParseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("size is empty")
	}

	value = normalizeBinarySuffix(value)
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", value, err)
	}
	if quantity.Sign() < 0 {
		return 0, errors.New("size must not be negative")
	}

	bytes := quantity.Value()
	if bytes < 0 {
		return 0, fmt.Errorf("size %q is outside int64 range", value)
	}

	return bytes, nil
}

// MeasureSize reads the requested PVC storage size with a status fallback.
// Requests represent intended capacity; status.capacity is used only when a
// provider has not preserved the request in the returned object.
func MeasureSize(ctx context.Context, client dynamic.Interface, pvc Ref) (int64, error) {
	if client == nil || ctx == nil {
		return 0, errors.New("PVC size client and context are required")
	}
	if err := pvc.Validate(); err != nil {
		return 0, err
	}

	object, err := client.
		Resource(pvcResource).
		Namespace(pvc.Namespace).
		Get(ctx, pvc.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get PVC %q: %w", pvc.Name, err)
	}

	for _, path := range [][3]string{
		{"spec", "resources", "requests"},
		{"status", "capacity", ""},
	} {
		value, found, err := nestedStorageValue(object, path[0], path[1], path[2])
		if err != nil {
			return 0, fmt.Errorf("read PVC %q size: %w", pvc.Name, err)
		}
		if found {
			return ParseSize(value)
		}
	}

	return 0, fmt.Errorf("PVC %s/%s has no storage size", pvc.Namespace, pvc.Name)
}

// ShouldSkip evaluates per-PVC and per-namespace limits for one object.
func (l SizeLimits) ShouldSkip(size, namespaceTotal int64) (bool, string) {
	if l.MaxPVCBytes > 0 && size > l.MaxPVCBytes {
		return true, "PVC size exceeds volume-max-size"
	}

	if l.MaxNamespaceBytes > 0 && namespaceTotal > l.MaxNamespaceBytes {
		return true, "namespace total exceeds volume-max-namespace-size"
	}

	return false, ""
}

// normalizeBinarySuffix maps friendly IEC suffixes to Kubernetes syntax.
func normalizeBinarySuffix(value string) string {
	for _, suffix := range []string{"EiB", "PiB", "TiB", "GiB", "MiB", "KiB", "Eib", "Pib", "Tib", "Gib", "Mib", "Kib"} {
		if prefix, found := strings.CutSuffix(value, suffix); found {
			return prefix + suffix[:2]
		}
	}

	return value
}

// nestedStorageValue reads a storage quantity from an unstructured object.
func nestedStorageValue(object *unstructured.Unstructured, first, second, third string) (string, bool, error) {
	path := []string{first, second}
	if third != "" {
		path = append(path, third)
	}

	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil || !found {
		return "", found, err
	}

	if storageValue, ok := value.(string); ok {
		return storageValue, true, nil
	}
	values, ok := value.(map[string]any)
	if !ok {
		return "", false, fmt.Errorf("%s must be an object or string", strings.Join(path, "."))
	}

	storage, found := values["storage"]
	if !found {
		return "", false, nil
	}
	storageValue, ok := storage.(string)
	if !ok {
		return "", false, errors.New("storage quantity must be a string")
	}

	return storageValue, true, nil
}
