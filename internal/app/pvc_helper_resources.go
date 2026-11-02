// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2021-2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"fmt"

	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResourceRequirements parses optional CPU and memory settings for PVC helper Pods.
func (o PvcHelperOptions) ResourceRequirements() (corev1.ResourceRequirements, error) {
	requests := make(corev1.ResourceList)
	limits := make(corev1.ResourceList)
	settings := []struct {
		flag    string
		value   string
		name    corev1.ResourceName
		request bool
	}{
		{flag: "--helper-cpu-request", value: o.CPURequest, name: corev1.ResourceCPU, request: true},
		{flag: "--helper-memory-request", value: o.MemoryRequest, name: corev1.ResourceMemory, request: true},
		{flag: "--helper-cpu-limit", value: o.CPULimit, name: corev1.ResourceCPU},
		{flag: "--helper-memory-limit", value: o.MemoryLimit, name: corev1.ResourceMemory},
	}

	for _, setting := range settings {
		if setting.value == "" {
			continue
		}

		quantity, err := resource.ParseQuantity(setting.value)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("parse %s %q: %w", setting.flag, setting.value, err)
		}
		if quantity.Sign() < 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("%s cannot be negative", setting.flag)
		}

		target := limits
		if setting.request {
			target = requests
		}
		target[setting.name] = quantity
	}

	requirements := corev1.ResourceRequirements{Requests: requests, Limits: limits}
	if err := pvc.ValidateHelperResources(requirements); err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("validate PVC helper resources: %w", err)
	}

	return requirements, nil
}
