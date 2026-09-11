// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"k8s.io/apimachinery/pkg/labels"
)

// CompiledProfile is an immutable profile ready for concurrent collection workers.
type CompiledProfile struct {
	// name is the resolved profile name.
	name string
	// imageSpec retains profile defaults for image selection.
	imageSpec contract.ImageSelectionSpec
	// spec retains selection defaults for command-line merging.
	spec contract.ResourceSelectionSpec
	// pvcSelection matches PVC metadata before a backup strategy is started.
	pvcSelection selectionMatcher
	// pvcSpec retains profile defaults for PVC data selection.
	pvcSpec contract.PVCSelectionSpec
	// resourceNames matches profile-only resource object-name filters.
	resourceNames patternMatcher
	// rules contains executable normalization rules.
	rules []compiledRule
	// imageSelection matches Pod/owner metadata and image references before pulling.
	imageSelection imageSelectionMatcher
	// omitEmpty removes explicitly empty fields before resource serialization.
	omitEmpty bool
}

// selectionMatcher is the compiled metadata boundary shared by object types.
type selectionMatcher struct {
	// labels is the compiled label selector.
	labels labels.Selector
	// annotations is the compiled annotation selector.
	annotations labels.Selector
	// namespaces contains ordered namespace glob patterns.
	namespaces patternMatcher
	// names contains ordered object-name glob patterns.
	names patternMatcher
}

// imageSelectionMatcher extends metadata matching with container and reference filters.
type imageSelectionMatcher struct {
	// containerTypes contains allowed regular or init container kinds.
	containerTypes map[contract.ContainerType]struct{}
	// pods matches the selected Pod namespace and metadata.
	pods selectionMatcher
	// references contains compiled registry, repository, and tag globs.
	references []imageReferenceMatcher
	// owners matches objects in the Pod's ownerReference chain.
	owners ownerSelectionMatcher
}

// ownerSelectionMatcher matches objects in a Pod ownerReference chain.
type ownerSelectionMatcher struct {
	// selection matches owner object metadata and resource identity.
	selection selectionMatcher
	// resources contains allowed owner GVR expressions.
	resources []ResourceExpression
	// enabled indicates that the profile requested owner-based filtering.
	enabled bool
}

// MatchObject contains the identity and metadata needed for owner matching.
type MatchObject struct {
	// Labels contains the object's labels.
	Labels map[string]string
	// Annotations contains the object's annotations.
	Annotations map[string]string
	// Group is the API group; it is empty for core resources.
	Group string
	// Version is the API version.
	Version string
	// Resource is the plural API resource name.
	Resource string
	// Namespace is the object's namespace.
	Namespace string
	// Name is the object's metadata.name.
	Name string
}

// imageReferenceMatcher is a validated image reference pattern.
type imageReferenceMatcher struct {
	// registry matches the normalized registry host.
	registry string
	// repository matches the normalized repository path.
	repository string
	// tags contains tag patterns; empty means any tag or digest.
	tags []string
}

// compiledRule is the runtime representation of one declarative rule.
type compiledRule struct {
	// action is the operation applied to matching objects.
	action string
	// labels is the compiled label selector.
	labels labels.Selector
	// annotations is the compiled annotation selector.
	annotations labels.Selector
	// name identifies the rule for diagnostics.
	name string
	// namespaces matches rule namespaces.
	namespaces patternMatcher
	// names matches object names.
	names patternMatcher
	// resources matches rule resource expressions.
	resources []ResourceExpression
	// paths contains parsed object field paths.
	paths [][]string
}

// patternMatcher evaluates ordered shell-style glob patterns.
// Positive patterns form an allowlist; a leading ! excludes matching values.
type patternMatcher struct {
	patterns []string
}

// matchContext snapshots immutable object metadata before normalization starts.
type matchContext struct {
	// labels contains the object's labels.
	labels map[string]string
	// annotations contains the object's annotations.
	annotations map[string]string
	// group is the object's API group.
	group string
	// version is the object's API version.
	version string
	// resource is the object's plural API resource.
	resource string
	// namespace is the object's namespace.
	namespace string
	// name is the object's metadata.name.
	name string
}
