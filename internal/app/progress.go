// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
)

// resourceProgressKindAliases contains only well-established Kubernetes abbreviations
// that make long progress labels easier to scan.
var resourceProgressKindAliases = map[string]string{
	"ClusterRoleBinding":       "CRB",
	"CustomResourceDefinition": "CRD",
	"HorizontalPodAutoscaler":  "HPA",
	"PersistentVolume":         "PV",
	"PersistentVolumeClaim":    "PVC",
	"PodDisruptionBudget":      "PDB",
}

const resourceProgressCompactThreshold = 3

// resourceProgress adapts Kubernetes collection events to generic progress bars.
// The collector remains independent from the terminal renderer
// and can still be used by non-interactive callers without constructing this observer.
type resourceProgress struct {
	operation     *progress.Operation
	compactLabels bool
}

// newResourceProgress creates a resource progress observer for one command.
func newResourceProgress(manager *progress.Manager) *resourceProgress {
	if manager == nil {
		return nil
	}

	return &resourceProgress{
		operation:     manager.NewOperation(),
		compactLabels: useCompactResourceProgressLabels(manager.Columns()),
	}
}

// Plan creates bars for all resource groups and collection scopes.
func (p *resourceProgress) Plan(plan kube.CollectionPlan) {
	if p == nil || p.operation == nil {
		return
	}

	jobs := make([]progress.Job, 0, len(plan.Jobs))
	for _, job := range plan.Jobs {
		group := p.group(job)
		scope := resourceProgressScope(job)
		jobs = append(jobs, progress.Job{Group: group, Scope: scope})
	}

	p.operation.SetPlan(progress.Plan{
		Scopes: plan.Scopes,
		Jobs:   jobs,
	})
}

// List adds the API list size to the corresponding resource denominator.
func (p *resourceProgress) List(event kube.CollectionListEvent) {
	if p == nil || p.operation == nil {
		return
	}

	p.operation.AddTotal(p.group(event.Job), event.Objects)
}

// Object advances a resource bar for every listed object, including skipped ones.
func (p *resourceProgress) Object(event kube.CollectionObjectEvent) {
	if p == nil || p.operation == nil {
		return
	}

	p.operation.Increment(p.group(event.Job))
}

// JobFinished advances the namespace or cluster scope after a job stops.
func (p *resourceProgress) JobFinished(event kube.CollectionJobEvent) {
	if p == nil || p.operation == nil {
		return
	}

	p.operation.FinishJob(progress.Job{
		Group:  p.group(event.Job),
		Scope:  resourceProgressScope(event.Job),
		Failed: event.Err != nil,
	})
}

// Close finalizes the operation after all collector jobs have reported.
func (p *resourceProgress) Close() {
	if p == nil || p.operation == nil {
		return
	}

	p.operation.Close()
}

// resourceProgressGroupForWidth chooses a display group
// and optionally uses compact labels for common long Kubernetes kinds.
//
// RBAC resources stay separate because all four kinds are common and useful progress units.
// Ingress and NetworkPolicy receive the same treatment;
// secondary networking resources remain grouped by API group.
func resourceProgressGroupForWidth(job kube.CollectionJobInfo, compactLabels bool) string {
	switch job.Group {
	case "", "apps", "batch":
		if job.Kind != "" {
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}

	case "rbac.authorization.k8s.io":
		switch job.Kind {
		case "ClusterRoleBinding", "ClusterRole", "RoleBinding", "Role":
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}

	case "networking.k8s.io":
		switch job.Kind {
		case "Ingress", "NetworkPolicy":
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}

	case "autoscaling":
		if job.Kind == "HorizontalPodAutoscaler" {
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}

	case "policy":
		if job.Kind == "PodDisruptionBudget" {
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}

	case "apiextensions.k8s.io":
		if job.Kind == "CustomResourceDefinition" {
			return resourceProgressKindLabel(job.Kind, compactLabels)
		}
	}

	if job.Group != "" {
		return job.Group
	}

	return job.Resource
}

// resourceProgressKindLabel returns a compact label while keeping unknown
// and already concise Kubernetes kinds unchanged.
func resourceProgressKindLabel(kind string, compactLabels bool) string {
	if !compactLabels {
		return kind
	}

	if alias, ok := resourceProgressKindAliases[kind]; ok {
		return alias
	}

	return kind
}

// group returns the display group used consistently by all observer events.
func (p *resourceProgress) group(job kube.CollectionJobInfo) string {
	return resourceProgressGroupForWidth(job, p.compactLabels)
}

// useCompactResourceProgressLabels enables aliases only when the longest known full kind
// would consume more than one third of the terminal width.
func useCompactResourceProgressLabels(columns int) bool {
	longest := 0
	for kind := range resourceProgressKindAliases {
		if len(kind) > longest {
			longest = len(kind)
		}
	}

	return columns > 0 && longest*resourceProgressCompactThreshold > columns
}

// resourceProgressScope returns the namespace label used by the scope bar.
func resourceProgressScope(job kube.CollectionJobInfo) string {
	if job.Namespace == "" {
		return "cluster"
	}

	return job.Namespace
}

// completionLogEvent keeps terminal summaries at debug
// when a progress UI already presents the same totals,
// while preserving them for non-interactive runs.
func completionLogEvent(progressActive bool) *zerolog.Event {
	if progressActive {
		return log.Debug()
	}

	return log.Info()
}
