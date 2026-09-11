// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package profile contains the public, serialized profile contract.
package profile

// ContainerType identifies the Pod container list in which an image is used.
type ContainerType string

const (
	// ContainerTypeContainer selects regular Pod containers.
	ContainerTypeContainer ContainerType = "container"
	// ContainerTypeInitContainer selects Pod init containers.
	ContainerTypeInitContainer ContainerType = "initContainer"
)

// Profile is the complete YAML policy consumed by kube-dump.
// Each data domain owns its selection settings
// so resource, PVC, and image collection can evolve independently
// without adding unrelated fields to one global selection block.
type Profile struct {
	// PVC defines optional namespace, metadata, and name filters for PVC data.
	// An empty selection includes every PVC allowed by runtime flags.
	PVC *PVCPolicy `json:"pvc,omitempty" yaml:"pvc,omitempty" jsonschema_extras:"x-order=50"`

	// Images defines optional Pod metadata, container, and image-reference filters.
	// An empty selection includes every image allowed by runtime flags.
	Images *ImagePolicy `json:"images,omitempty" yaml:"images,omitempty" jsonschema_extras:"x-order=60"`

	// Metadata gives the profile its catalog name and user-facing description.
	Metadata Metadata `json:"metadata" yaml:"metadata" jsonschema_extras:"x-order=30" jsonschema:"required"`

	// APIVersion identifies the profile contract version.
	APIVersion string `json:"apiVersion" yaml:"apiVersion" jsonschema_extras:"x-order=10" jsonschema:"required,enum=kube-dump/v2,example=kube-dump/v2"`

	// Kind identifies the document as a kube-dump profile.
	Kind string `json:"kind" yaml:"kind" jsonschema_extras:"x-order=20" jsonschema:"required,enum=Profile,example=Profile"`

	// Resources defines which Kubernetes API objects are collected
	// and how they are normalized before serialization.
	Resources ResourcePolicy `json:"resources" yaml:"resources" jsonschema_extras:"x-order=40" jsonschema:"required"`
}

// ResourcePolicy describes API-object selection and ordered object transforms.
// An empty policy includes every resource exposed by Kubernetes and applies no transformations.
type ResourcePolicy struct {
	// Selection defines the initial set of API objects admitted by the profile.
	// When omitted or empty, all discovered resources are included.
	Selection ResourceSelectionSpec `json:"selection,omitzero" yaml:"selection,omitempty" jsonschema_extras:"x-order=10"`

	// Rules are evaluated in declaration order after selection.
	// When omitted or empty, no fields are removed or encrypted.
	Rules []RuleSpec `json:"rules,omitempty" yaml:"rules,omitempty" jsonschema:"maxItems=512" jsonschema_extras:"x-order=20"`

	// OmitEmpty removes null values and empty maps or lists from resource objects before serialization.
	// Zero values, false values, and empty strings are kept.
	OmitEmpty bool `json:"omitEmpty,omitempty" yaml:"omitEmpty,omitempty" jsonschema_extras:"x-order=5"`
}

// PVCPolicy describes profile defaults for persistent-volume claim data.
type PVCPolicy struct {
	// Selection limits PVC data by namespace, name, and Kubernetes metadata.
	Selection PVCSelectionSpec `json:"selection" yaml:"selection"`
}

// ImagePolicy describes profile defaults for images referenced by Pods.
type ImagePolicy struct {
	// Selection limits image discovery by Pod namespace, metadata, container type, and image reference.
	Selection ImageSelectionSpec `json:"selection" yaml:"selection"`
}

// Metadata gives a profile a stable catalog alias and a description shown to users.
type Metadata struct {
	// Name is the lowercase alias used with --profile and profile commands.
	Name string `json:"name" yaml:"name" jsonschema_extras:"x-order=10" jsonschema:"required,minLength=1,maxLength=63,pattern=^[a-z0-9]([a-z0-9-]*[a-z0-9])?$,example=clean-export"`

	// Description is a concise user-facing explanation of the profile's purpose.
	Description string `json:"description" yaml:"description" jsonschema_extras:"x-order=20" jsonschema:"required,minLength=1,maxLength=512,example=Clean export suitable for sharing or redeployment"`
}

// PVCSelectionSpec selects PVC data by namespace, name, labels, and annotations.
// All populated filters are combined with AND semantics; an empty field does not restrict selection.
// Names are shell-style globs matched against PVC names.
type PVCSelectionSpec struct {
	// ObjectScope scopes claims before a backup strategy is selected.
	ObjectScope `json:",inline" yaml:",inline"`
}

// ImageSelectionSpec selects image references from Pods by namespace, name, labels, annotations, owners, and image reference.
// The embedded object selection applies to Pods.
// Owners adds optional filters for objects in the selected Pods' ownerReference chains.
// All populated filters are combined with AND semantics.
type ImageSelectionSpec struct {
	// ObjectScope filters the Pods from which image references are discovered.
	ObjectScope `json:",inline" yaml:",inline"`

	// Owners selects objects in the ownerReference chain of a selected Pod.
	// A Pod is included only when at least one owner matches all configured owner filters.
	// Use Resources to distinguish Deployments, DaemonSets, StatefulSets, Jobs, and other Kubernetes owner objects.
	Owners OwnerSelectionSpec `json:"owners,omitzero" yaml:"owners,omitempty" jsonschema_extras:"x-order=20"`

	// ContainerTypes limits matches to regular containers, init containers, or both.
	ContainerTypes []ContainerType `json:"containerTypes,omitempty" yaml:"containerTypes,omitempty" jsonschema_extras:"x-order=30" jsonschema:"maxItems=2,uniqueItems=true,enum=container,enum=initContainer,example=container,example=initContainer"`

	// References selects images by registry, repository, and optional tag globs.
	// Multiple entries are combined with OR semantics.
	References []ImageReferenceSpec `json:"references,omitempty" yaml:"references,omitempty" jsonschema_extras:"x-order=40" jsonschema:"maxItems=256,uniqueItems=true"`
}

// ObjectScope contains the selectors shared by resources, PVCs, Pods, and owner objects.
// Every populated field is combined with AND semantics.
// An empty field does not restrict the selected objects.
type ObjectScope struct {
	// LabelSelector filters objects by Kubernetes label-selector semantics.
	LabelSelector *Selector `json:"labelSelector,omitempty" yaml:"labelSelector,omitempty" jsonschema_extras:"x-order=30"`

	// AnnotationSelector filters objects by the same operators
	// and conjunction semantics as LabelSelector, applied to annotations.
	AnnotationSelector *AnnotationSelector `json:"annotationSelector,omitempty" yaml:"annotationSelector,omitempty" jsonschema_extras:"x-order=40"`

	// Namespaces limits objects to namespace names matching shell-style globs.
	// A leading `!` excludes matching namespaces; an empty list matches every namespace.
	// Quote values beginning with `!` in YAML because `!` otherwise starts a YAML tag.
	Namespaces []string `json:"namespaces,omitempty" yaml:"namespaces,omitempty" jsonschema_extras:"x-order=10" jsonschema:"maxItems=256,uniqueItems=true,minLength=1,maxLength=128,example=production,example=!kube-*"`

	// Names limits objects to names matching shell-style globs.
	// A leading `!` excludes matching names; an empty list matches every name.
	// Quote values beginning with `!` in YAML because `!` otherwise starts a YAML tag.
	Names []string `json:"names,omitempty" yaml:"names,omitempty" jsonschema_extras:"x-order=20" jsonschema:"maxItems=256,uniqueItems=true,minLength=1,maxLength=256,example=api-*,example=!api-temporary-*"`
}

// OwnerSelectionSpec selects owner objects from a Pod's ownerReference chain by namespace, name, labels, and annotations.
// Resources uses the same group/version/resource syntax as resource selection.
type OwnerSelectionSpec struct {
	// ObjectScope filters objects in the ownerReference chain.
	ObjectScope `json:",inline" yaml:",inline"`

	// Resources limits matching owners to Kubernetes resource identities such as `apps/v1/deployments` or `apps/v1/daemonsets`.
	// An empty list accepts any owner in the Pod's ownerReference chain.
	Resources []string `json:"resources,omitempty" yaml:"resources,omitempty" jsonschema_extras:"x-order=5" jsonschema:"maxItems=128,uniqueItems=true,minLength=1,maxLength=253,example=apps/v1/deployments"`
}

// ImageReferenceSpec matches an image reference without conflating repository selection with tag selection.
// Registry and repository use shell-style globs; an empty Tags list accepts any tag.
type ImageReferenceSpec struct {
	// Registry is the registry host glob, for example docker.io or ghcr.io.
	Registry string `json:"registry" yaml:"registry" jsonschema_extras:"x-order=10" jsonschema:"required,minLength=1,maxLength=253,example=docker.io"`

	// Repository is the repository path glob without the registry or tag.
	Repository string `json:"repository" yaml:"repository" jsonschema_extras:"x-order=20" jsonschema:"required,minLength=1,maxLength=512,example=library/*"`

	// Tags accepts image tags matching at least one shell-style glob pattern.
	// An empty list accepts tagged and digest-pinned references.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty" jsonschema_extras:"x-order=30" jsonschema:"maxItems=64,uniqueItems=true,minLength=1,maxLength=128,example=MR-*,example=*-rc.*"`
}

// ResourceSelectionSpec selects Kubernetes API objects by resource, namespace, name, labels, and annotations before rule processing.
// Resource expressions are evaluated in order, and the last matching expression wins.
type ResourceSelectionSpec struct {
	// ObjectScope provides common namespace, name, and metadata filters for API objects.
	ObjectScope `json:",inline" yaml:",inline"`

	// Resources is an ordered list of `group/version/resource[:name]` expressions.
	// A leading `!` excludes matching resources or a specific object name.
	// Quote values beginning with `!` in YAML because `!` otherwise starts a YAML tag.
	Resources []string `json:"resources,omitempty" yaml:"resources,omitempty" jsonschema_extras:"x-order=5" jsonschema:"maxItems=512,uniqueItems=true,minLength=1,maxLength=253,example=core/v1/configmaps,example=!core/v1/configmaps:kube-root-ca.crt"`
}

// RuleSpec describes one ordered transformation applied to matching objects.
// Exactly one action, Remove or Encrypt, must be set.
// Rules without a name receive a generated diagnostic name during compilation.
type RuleSpec struct {
	// Name labels the rule in validation errors and diagnostics.
	Name string `json:"name,omitempty" yaml:"name,omitempty" jsonschema_extras:"x-order=10" jsonschema:"maxLength=63,pattern=^[a-z0-9]([a-z0-9-]*[a-z0-9])?$,example=remove-runtime-metadata"`

	// Match selects the objects affected by this rule; all populated match fields must pass.
	Match MatchSpec `json:"match" yaml:"match" jsonschema_extras:"x-order=20" jsonschema:"required"`

	// Remove lists dot/bracket field paths deleted before serialization.
	Remove []string `json:"remove,omitempty" yaml:"remove,omitempty" jsonschema_extras:"x-order=30" jsonschema:"oneof_required=remove,maxItems=128,uniqueItems=true,minLength=1,maxLength=1024,example=.metadata.creationTimestamp"`

	// Encrypt lists dot/bracket field paths protected when runtime encryption is configured.
	// Without recipients or an AES-SIV keyring, the selected fields remain plaintext.
	Encrypt []string `json:"encrypt,omitempty" yaml:"encrypt,omitempty" jsonschema_extras:"x-order=40" jsonschema:"oneof_required=encrypt,maxItems=128,uniqueItems=true,minLength=1,maxLength=1024,example=.data.*"`
}

// MatchSpec selects objects by resource identity and metadata.
// Empty fields match any value; populated fields are combined with AND semantics.
type MatchSpec struct {
	// ObjectScope defines the object boundary for this rule.
	ObjectScope `json:",inline" yaml:",inline"`

	// Resources restricts the rule to `group/version/resource[:name]` expressions.
	Resources []string `json:"resources,omitempty" yaml:"resources,omitempty" jsonschema_extras:"x-order=5" jsonschema:"maxItems=512,uniqueItems=true,minLength=1,maxLength=253,example=core/v1/secrets,example=apps/v1/deployments:api"`
}

// Selector is a Kubernetes-style conjunction of label requirements.
// Both maps and expressions must match when both are present.
type Selector struct {
	// MatchLabels requires exact key/value pairs.
	MatchLabels map[string]string `jsonschema_extras:"x-order=10" json:"matchLabels,omitempty" yaml:"matchLabels,omitempty"`

	// MatchExpressions supports membership and key-existence operators.
	MatchExpressions []SelectorRequirement `jsonschema_extras:"x-order=20" json:"matchExpressions,omitempty" yaml:"matchExpressions,omitempty" jsonschema:"maxItems=64,uniqueItems=true"`
}

// AnnotationSelector applies Kubernetes-style requirements to annotations.
// Its operators and conjunction semantics are the same as Selector's.
type AnnotationSelector struct {
	// MatchLabels requires exact annotation key/value pairs.
	MatchLabels map[string]string `jsonschema_extras:"x-order=10" json:"matchLabels,omitempty" yaml:"matchLabels,omitempty"`

	// MatchExpressions supports membership and key-existence operators.
	MatchExpressions []SelectorRequirement `jsonschema_extras:"x-order=20" json:"matchExpressions,omitempty" yaml:"matchExpressions,omitempty" jsonschema:"maxItems=64,uniqueItems=true"`
}

// SelectorRequirement is one operator-based label or annotation requirement.
// Values are used by In and NotIn; Exists and DoesNotExist use no values.
type SelectorRequirement struct {
	// Key is the label or annotation key to test.
	Key string `json:"key" yaml:"key" jsonschema_extras:"x-order=10" jsonschema:"required,minLength=1,maxLength=253,example=environment"`

	// Operator determines how Key and Values are evaluated.
	Operator string `json:"operator" yaml:"operator" jsonschema_extras:"x-order=20" jsonschema:"required,enum=In,enum=NotIn,enum=Exists,enum=DoesNotExist,example=In"`

	// Values contains operands for In and NotIn operators.
	Values []string `json:"values,omitempty" yaml:"values,omitempty" jsonschema_extras:"x-order=30" jsonschema:"maxItems=64,uniqueItems=true,example=prod"`
}
