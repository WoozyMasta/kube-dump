// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// resourceQuotaResource identifies namespace ResourceQuota objects.
var resourceQuotaResource = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}

// quotaRequirement describes one temporary resource count enforced by a quota.
type quotaRequirement struct {
	name   string   // name is the human-readable resource category used in errors.
	keys   []string // keys contains equivalent ResourceQuota resource names accepted by Kubernetes versions.
	amount int64    // amount is the number of temporary objects required concurrently.
}

// ValidateBackupQuotas checks whether temporary Kubernetes objects
// fit the current namespace ResourceQuota limits before a PVC backup starts.
//
// The required count is bounded by both the requested concurrency
// and the number of selected claims in the namespace.
// Quotas are only a preflight observation,
// so another writer can still consume the remaining capacity
// before a helper object is created.
func ValidateBackupQuotas(
	ctx context.Context,
	client dynamic.Interface,
	claims []Ref,
	strategy Strategy,
	concurrency int,
	staticS3Credentials bool,
) error {
	if ctx == nil {
		return errors.New("PVC quota context is required")
	}
	if client == nil {
		return errors.New("PVC quota dynamic client is required")
	}
	if err := strategy.Validate(); err != nil {
		return err
	}
	if concurrency <= 0 {
		return errors.New("PVC lifecycle concurrency must be positive")
	}
	if len(claims) == 0 {
		return nil
	}

	claimsByNamespace := make(map[string]int, len(claims))
	for _, claim := range claims {
		if err := claim.Validate(); err != nil {
			return err
		}
		claimsByNamespace[claim.Namespace]++
	}

	namespaces := make([]string, 0, len(claimsByNamespace))
	for namespace := range claimsByNamespace {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)

	for _, namespace := range namespaces {
		required := min(claimsByNamespace[namespace], concurrency)
		requirements := backupQuotaRequirements(strategy, required, staticS3Credentials)
		if err := validateNamespaceQuotas(ctx, client, namespace, requirements); err != nil {
			return err
		}
	}

	return nil
}

// backupQuotaRequirements returns temporary object counts for one PVC task batch.
func backupQuotaRequirements(strategy Strategy, amount int, staticS3Credentials bool) []quotaRequirement {
	requirements := []quotaRequirement{{
		name:   "temporary Pods",
		keys:   []string{"pods", "count/pods"},
		amount: int64(amount),
	}}

	if strategy == SnapshotCopy {
		requirements = append(requirements,
			quotaRequirement{
				name:   "snapshot clone PVCs",
				keys:   []string{"persistentvolumeclaims", "count/persistentvolumeclaims"},
				amount: int64(amount),
			},
			quotaRequirement{
				name: "temporary VolumeSnapshots",
				keys: []string{
					"count/volumesnapshots.snapshot.storage.k8s.io",
					"count/volumesnapshots",
					"volumesnapshots.snapshot.storage.k8s.io",
				},
				amount: int64(amount),
			},
		)
	}

	if staticS3Credentials {
		requirements = append(requirements, quotaRequirement{
			name:   "temporary S3 Secrets",
			keys:   []string{"secrets", "count/secrets"},
			amount: int64(amount),
		})
	}

	return requirements
}

// validateNamespaceQuotas checks all ResourceQuota objects in one namespace.
func validateNamespaceQuotas(
	ctx context.Context,
	client dynamic.Interface,
	namespace string,
	requirements []quotaRequirement,
) error {
	quotas, err := client.Resource(resourceQuotaResource).
		Namespace(namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("PVC backup preflight: list ResourceQuota in namespace %q: %w", namespace, err)
	}

	for _, quota := range quotas.Items {
		hard, found, err := quotaQuantities(quota, "status", "hard")
		if err != nil {
			return fmt.Errorf("PVC backup preflight: read ResourceQuota %s/%s: %w", namespace, quota.GetName(), err)
		}

		if !found {
			hard, found, err = quotaQuantities(quota, "spec", "hard")
			if err != nil {
				return fmt.Errorf("PVC backup preflight: read ResourceQuota %s/%s: %w", namespace, quota.GetName(), err)
			}
		}

		if !found {
			continue
		}

		used, usedFound, err := quotaQuantities(quota, "status", "used")
		if err != nil {
			return fmt.Errorf("PVC backup preflight: read ResourceQuota %s/%s usage: %w", namespace, quota.GetName(), err)
		}

		for _, requirement := range requirements {
			limit, applies := findQuotaQuantity(hard, requirement.keys)
			if !applies {
				continue
			}

			if !usedFound {
				return fmt.Errorf(
					"PVC backup preflight: ResourceQuota %s/%s has no usage for %s",
					namespace,
					quota.GetName(),
					requirement.name,
				)
			}

			current, _ := findQuotaQuantity(used, requirement.keys)
			available := limit.DeepCopy()
			available.Sub(current)
			needed := apiresource.NewQuantity(requirement.amount, apiresource.DecimalSI)
			if available.Cmp(*needed) < 0 {
				return fmt.Errorf(
					"PVC backup preflight: namespace %s ResourceQuota %s has insufficient capacity for %s: need %d, available %s (used %s, hard %s)",
					namespace,
					quota.GetName(),
					requirement.name,
					requirement.amount,
					available.String(),
					current.String(),
					limit.String(),
				)
			}
		}
	}

	return nil
}

// quotaQuantities decodes a ResourceQuota quantity map from an unstructured object.
func quotaQuantities(object unstructured.Unstructured, path ...string) (map[string]apiresource.Quantity, bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil || !found {
		return nil, found, err
	}

	values, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%s must be an object", strings.Join(path, "."))
	}

	result := make(map[string]apiresource.Quantity, len(values))
	for key, raw := range values {
		text, ok := raw.(string)
		if !ok {
			return nil, false, fmt.Errorf("quota value %q must be a string", key)
		}

		quantity, err := apiresource.ParseQuantity(text)
		if err != nil {
			return nil, false, fmt.Errorf("parse quota value %q: %w", key, err)
		}

		result[key] = quantity
	}

	return result, true, nil
}

// findQuotaQuantity returns the first quantity present under an equivalent quota key.
func findQuotaQuantity(values map[string]apiresource.Quantity, keys []string) (apiresource.Quantity, bool) {
	for _, key := range keys {
		if value, found := values[key]; found {
			return value, true
		}
	}

	return apiresource.Quantity{}, false
}
