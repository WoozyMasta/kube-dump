// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/kube"
)

func TestResourceProgressGroup(t *testing.T) {
	tests := []struct {
		name string
		job  kube.CollectionJobInfo
		want string
	}{
		{
			name: "core kind",
			job:  kube.CollectionJobInfo{Kind: "ConfigMap", Resource: "configmaps"},
			want: "ConfigMap",
		},
		{
			name: "apps kind",
			job:  kube.CollectionJobInfo{Group: "apps", Kind: "Deployment", Resource: "deployments"},
			want: "Deployment",
		},
		{
			name: "custom resource definition",
			job:  kube.CollectionJobInfo{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"},
			want: "CRD",
		},
		{
			name: "horizontal pod autoscaler",
			job:  kube.CollectionJobInfo{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"},
			want: "HPA",
		},
		{
			name: "persistent volume claim",
			job:  kube.CollectionJobInfo{Group: "", Kind: "PersistentVolumeClaim"},
			want: "PVC",
		},
		{
			name: "pod disruption budget",
			job:  kube.CollectionJobInfo{Group: "policy", Kind: "PodDisruptionBudget"},
			want: "PDB",
		},
		{
			name: "rbac cluster role binding",
			job:  kube.CollectionJobInfo{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"},
			want: "CRB",
		},
		{
			name: "rbac role",
			job:  kube.CollectionJobInfo{Group: "rbac.authorization.k8s.io", Kind: "Role"},
			want: "Role",
		},
		{
			name: "secondary rbac resource",
			job:  kube.CollectionJobInfo{Group: "rbac.authorization.k8s.io", Kind: "SelfSubjectRulesReview"},
			want: "rbac.authorization.k8s.io",
		},
		{
			name: "network policy",
			job:  kube.CollectionJobInfo{Group: "networking.k8s.io", Kind: "NetworkPolicy"},
			want: "NetworkPolicy",
		},
		{
			name: "secondary networking resource",
			job:  kube.CollectionJobInfo{Group: "networking.k8s.io", Kind: "IngressClass"},
			want: "networking.k8s.io",
		},
		{
			name: "custom group",
			job:  kube.CollectionJobInfo{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute"},
			want: "gateway.networking.k8s.io",
		},
		{
			name: "unknown core kind",
			job:  kube.CollectionJobInfo{Resource: "widgets"},
			want: "widgets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resourceProgressGroupForWidth(test.job, true); got != test.want {
				t.Fatalf("resourceProgressGroupForWidth() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResourceProgressScope(t *testing.T) {
	if got := resourceProgressScope(kube.CollectionJobInfo{Namespace: "status"}); got != "status" {
		t.Fatalf("resourceProgressScope(namespaced) = %q, want status", got)
	}
	if got := resourceProgressScope(kube.CollectionJobInfo{}); got != "cluster" {
		t.Fatalf("resourceProgressScope(cluster) = %q, want cluster", got)
	}
}

func TestResourceProgressUsesFullLabelsOnWideTerminals(t *testing.T) {
	job := kube.CollectionJobInfo{Group: "", Kind: "PersistentVolumeClaim"}
	if got := resourceProgressGroupForWidth(job, false); got != "PersistentVolumeClaim" {
		t.Fatalf("resourceProgressGroupForWidth() = %q, want PersistentVolumeClaim", got)
	}
	if got := resourceProgressGroupForWidth(job, true); got != "PVC" {
		t.Fatalf("resourceProgressGroupForWidth() = %q, want PVC", got)
	}
}

func TestUseCompactResourceProgressLabels(t *testing.T) {
	longest := 0
	for kind := range resourceProgressKindAliases {
		if len(kind) > longest {
			longest = len(kind)
		}
	}

	if !useCompactResourceProgressLabels(longest*resourceProgressCompactThreshold - 1) {
		t.Fatal("useCompactResourceProgressLabels() disabled labels below threshold")
	}
	if useCompactResourceProgressLabels(longest * resourceProgressCompactThreshold) {
		t.Fatal("useCompactResourceProgressLabels() enabled labels at threshold")
	}
}
