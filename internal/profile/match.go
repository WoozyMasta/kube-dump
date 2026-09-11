// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"github.com/woozymasta/kube-dump/v2/pkg/profile"
	"k8s.io/apimachinery/pkg/labels"
	"oras.land/oras-go/v2/registry"
)

// AllowsResourceName applies the profile name filter that has no separate runtime selector.
// Other resource selection dimensions are merged into kube.Selection before collection starts.
func (p *CompiledProfile) AllowsResourceName(name string) bool {
	return p != nil && p.resourceNames.matches(name)
}

// PVCLabelSelector returns the compiled Kubernetes label selector for PVC discovery.
// An empty string means that the API request needs no label filter.
func (p *CompiledProfile) PVCLabelSelector() string {
	if p == nil || p.pvcSelection.labels == nil {
		return ""
	}

	return p.pvcSelection.labels.String()
}

// MatchesPVC reports whether a PVC passes the profile's metadata and name filters.
func (p *CompiledProfile) MatchesPVC(
	namespace, name string,
	objectLabels, objectAnnotations map[string]string,
) bool {
	if p == nil {
		return false
	}

	return p.pvcSelection.matches(
		nil, namespace, name, objectLabels, objectAnnotations,
	)
}

// MatchesPVCInNamespaces applies an effective namespace selection to PVC matching.
func (p *CompiledProfile) MatchesPVCInNamespaces(
	namespaces []string,
	namespace, name string,
	objectLabels, objectAnnotations map[string]string,
) bool {
	if p == nil {
		return false
	}

	return p.pvcSelection.matches(
		namespaces, namespace, name, objectLabels, objectAnnotations,
	)
}

// MatchesPVCInScope applies effective namespace and name selections to PVC matching.
func (p *CompiledProfile) MatchesPVCInScope(
	namespaces, names []string,
	namespace, name string,
	objectLabels, objectAnnotations map[string]string,
) bool {
	if p == nil {
		return false
	}

	return p.pvcSelection.matchesScope(
		namespaces, names, namespace, name, objectLabels, objectAnnotations,
	)
}

// MatchesImage reports whether an image use passes the profile's Pod and reference filters.
func (p *CompiledProfile) MatchesImage(
	namespace string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
) bool {
	if p == nil {
		return false
	}

	return p.imageSelection.matches(
		nil, namespace, "", objectLabels, objectAnnotations, containerType, reference, nil,
	)
}

// MatchesImageInNamespaces applies an effective namespace selection to image matching.
func (p *CompiledProfile) MatchesImageInNamespaces(
	namespaces []string,
	namespace string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
) bool {
	if p == nil {
		return false
	}

	return p.imageSelection.matches(
		namespaces, namespace, "", objectLabels, objectAnnotations, containerType, reference, nil,
	)
}

// MatchesImagePodInNamespaces applies Pod, container,
// and image-reference filters without requiring owner resolution.
func (p *CompiledProfile) MatchesImagePodInNamespaces(
	namespaces []string,
	namespace, podName string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
) bool {
	if p == nil {
		return false
	}

	return p.imageSelection.matchesPod(
		namespaces, namespace, podName, objectLabels, objectAnnotations, containerType, reference,
	)
}

// ImageOwnerSelectionEnabled reports whether image matching needs Pod owner objects.
func (p *CompiledProfile) ImageOwnerSelectionEnabled() bool {
	return p != nil && p.imageSelection.owners.enabled
}

// MatchesImageWithOwners applies image filters to a Pod and its resolved owner chain.
// An explicit namespace slice overrides profile namespaces for both Pods and owners.
func (p *CompiledProfile) MatchesImageWithOwners(
	namespaces []string,
	namespace string,
	podName string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
	owners []MatchObject,
) bool {
	if p == nil {
		return false
	}

	return p.imageSelection.matches(
		namespaces, namespace, podName, objectLabels, objectAnnotations, containerType, reference, owners,
	)
}

// matches evaluates Pod metadata, container, and image reference filters.
func (m imageSelectionMatcher) matches(
	namespaces []string,
	namespace string,
	podName string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
	owners []MatchObject,
) bool {
	if !m.matchesPod(
		namespaces, namespace, podName, objectLabels, objectAnnotations, containerType, reference,
	) {
		return false
	}
	if !m.owners.matches(namespaces, owners) {
		return false
	}

	return true
}

// matchesPod evaluates the portion of image selection that needs no owner API access.
func (m imageSelectionMatcher) matchesPod(
	namespaces []string,
	namespace, podName string,
	objectLabels, objectAnnotations map[string]string,
	containerType profile.ContainerType,
	reference registry.Reference,
) bool {
	return m.pods.matches(namespaces, namespace, podName, objectLabels, objectAnnotations) &&
		m.matchesImage(containerType, reference)
}

// matches evaluates a scope using profile values or an effective namespace override.
func (m selectionMatcher) matches(
	namespaces []string,
	namespace, name string,
	objectLabels, objectAnnotations map[string]string,
) bool {
	return m.matchesScope(namespaces, nil, namespace, name, objectLabels, objectAnnotations)
}

// matchesScope applies effective namespace and name selections to a scope.
func (m selectionMatcher) matchesScope(
	namespaces, names []string,
	namespace, name string,
	objectLabels, objectAnnotations map[string]string,
) bool {
	namespaceMatcher := m.namespaces
	if len(namespaces) > 0 {
		namespaceMatcher = newPatternMatcher(namespaces)
	}

	nameMatcher := m.names
	if len(names) > 0 {
		nameMatcher = newPatternMatcher(names)
	}

	return namespaceMatcher.matches(namespace) && nameMatcher.matches(name) &&
		m.matchesMetadata(objectLabels, objectAnnotations)
}

// matchesMetadata evaluates label and annotation selectors after scope matching.
func (m selectionMatcher) matchesMetadata(objectLabels, objectAnnotations map[string]string) bool {
	return m.labels.Matches(labels.Set(objectLabels)) &&
		m.annotations.Matches(labels.Set(objectAnnotations))
}

// matches reports whether a Pod owner chain satisfies the configured owner scope.
func (m ownerSelectionMatcher) matches(namespaces []string, owners []MatchObject) bool {
	if !m.enabled {
		return true
	}

	for _, owner := range owners {
		if len(m.resources) > 0 && !resourceExpressionsMatch(
			m.resources, owner.Group, owner.Version, owner.Resource, owner.Name,
		) {
			continue
		}

		if m.selection.matches(
			namespaces, owner.Namespace, owner.Name, owner.Labels, owner.Annotations,
		) {
			return true
		}
	}

	return false
}

// matchesImage evaluates container type and registry reference filters.
func (m imageSelectionMatcher) matchesImage(
	containerType profile.ContainerType,
	reference registry.Reference,
) bool {
	if len(m.containerTypes) > 0 {
		if _, ok := m.containerTypes[containerType]; !ok {
			return false
		}
	}

	if len(m.references) == 0 {
		return true
	}

	for _, matcher := range m.references {
		if !globMatches(matcher.registry, reference.Registry) ||
			!globMatches(matcher.repository, reference.Repository) {
			continue
		}

		if len(matcher.tags) == 0 {
			return true
		}

		for _, tag := range matcher.tags {
			if globMatches(tag, reference.Reference) {
				return true
			}
		}
	}

	return false
}
