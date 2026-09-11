// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package profile defines declarative Kubernetes selection and normalization profiles.
package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"strings"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"go.yaml.in/yaml/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

const maxProfileSize = 1 << 20

const (
	ruleRemove  = "remove"
	ruleEncrypt = "encrypt"
)

// Load resolves a built-in or global profile and then treats the source as a file path.
func Load(source string) (*CompiledProfile, error) {
	compiled, _, err := LoadResolvedFrom(source, "")
	return compiled, err
}

// LoadFrom resolves and compiles a profile using an explicit global profile directory.
func LoadFrom(source, directory string) (*CompiledProfile, error) {
	compiled, _, err := LoadResolvedFrom(source, directory)
	return compiled, err
}

// Resolve returns raw YAML for a built-in, global, or filesystem profile source.
func Resolve(source string) ([]byte, error) {
	data, err := ResolveFrom(source, "")
	if err != nil {
		return nil, err
	}

	return data, nil
}

// ResolveFrom reads a profile using an explicit global profile directory.
func ResolveFrom(source, directory string) ([]byte, error) {
	registry, err := NewRegistry(directory)
	if err != nil {
		return nil, err
	}

	if source == "" {
		source = defaultProfileName(source)
	}

	return registry.Resolve(source)
}

// LoadResolved resolves and compiles a profile while returning its source bytes.
func LoadResolved(source string) (*CompiledProfile, []byte, error) {
	return LoadResolvedFrom(source, "")
}

// LoadResolvedFrom resolves and compiles a profile using an explicit global directory.
func LoadResolvedFrom(source, directory string) (*CompiledProfile, []byte, error) {
	data, err := ResolveFrom(source, directory)
	if err != nil {
		return nil, nil, err
	}

	compiled, err := LoadBytes(data)
	if err != nil {
		return nil, nil, fmt.Errorf("load profile %q: %w", source, err)
	}

	return compiled, data, nil
}

// LoadBytes parses one strict profile document and rejects trailing YAML documents.
func LoadBytes(data []byte) (*CompiledProfile, error) {
	if len(data) > maxProfileSize {
		return nil, fmt.Errorf("profile exceeds the %d-byte limit", maxProfileSize)
	}

	var document contract.Profile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode profile: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple YAML documents are not supported")
		}

		return nil, fmt.Errorf("decode profile: %w", err)
	}

	if err := validateSchema(document); err != nil {
		return nil, err
	}

	return Compile(document)
}

// Compile validates selectors, paths, and profile metadata once.
func Compile(document contract.Profile) (*CompiledProfile, error) {
	// Compile all user input into immutable matchers once.
	// Runtime collection then performs matching without reparsing globs, selectors, or field paths.
	if document.APIVersion != "kube-dump/v2" || document.Kind != "Profile" {
		return nil, errors.New("profile apiVersion must be kube-dump/v2 and kind must be Profile")
	}
	if document.Metadata.Name == "" {
		return nil, errors.New("profile metadata.name is required")
	}

	if _, err := compileResourceExpressions(document.Resources.Selection.Resources); err != nil {
		return nil, fmt.Errorf("validate profile selection: %w", err)
	}

	resourceScope, err := compileSelectionMatcher(document.Resources.Selection.ObjectScope)
	if err != nil {
		return nil, fmt.Errorf("compile resource selection: %w", err)
	}

	var pvcSpec contract.PVCSelectionSpec
	if document.PVC != nil {
		pvcSpec = document.PVC.Selection
	}

	var imageSpec contract.ImageSelectionSpec
	if document.Images != nil {
		imageSpec = document.Images.Selection
	}

	pvcSelection, err := compileSelectionMatcher(pvcSpec.ObjectScope)
	if err != nil {
		return nil, fmt.Errorf("compile PVC selection: %w", err)
	}

	imageSelection, err := compileImageSelection(imageSpec)
	if err != nil {
		return nil, fmt.Errorf("compile image selection: %w", err)
	}

	compiled := &CompiledProfile{
		name:           document.Metadata.Name,
		omitEmpty:      document.Resources.OmitEmpty,
		spec:           document.Resources.Selection,
		resourceNames:  resourceScope.names,
		pvcSpec:        pvcSpec,
		imageSpec:      imageSpec,
		pvcSelection:   pvcSelection,
		imageSelection: imageSelection,
		rules:          make([]compiledRule, 0, len(document.Resources.Rules)),
	}
	names := make(map[string]struct{}, len(document.Resources.Rules))

	// Rule names identify diagnostics
	// and must be unique even when the name was generated for an unnamed rule.
	for index, spec := range document.Resources.Rules {
		name := spec.Name
		if name == "" {
			name = fmt.Sprintf("rule-%d", index+1)
		}
		if _, exists := names[name]; exists {
			return nil, fmt.Errorf("compile profile rule %d: duplicate rule name %q", index, name)
		}

		names[name] = struct{}{}

		rule, err := compileRule(name, spec)
		if err != nil {
			return nil, fmt.Errorf("compile profile rule %d: %w", index, err)
		}

		compiled.rules = append(compiled.rules, rule)
	}

	return compiled, nil
}

// EncryptionPaths returns field paths from matching encryption rules.
// The result is copied so callers can apply the paths without mutating the compiled profile.
func (p *CompiledProfile) EncryptionPaths(object state.Object) [][]string {
	if p == nil || object.Value == nil {
		return nil
	}

	context := newMatchContext(object)
	var result [][]string
	for _, rule := range p.rules {
		if rule.action != ruleEncrypt || !rule.matches(context) {
			continue
		}

		for _, path := range rule.paths {
			result = append(result, append([]string(nil), path...))
		}
	}

	return result
}

// Name returns the profile metadata name.
func (p *CompiledProfile) Name() string {
	if p == nil {
		return ""
	}

	return p.name
}

// SelectionDefaults returns a copy of profile-level selection defaults.
// Callers can use it to fill unset runtime options without mutating the profile.
func (p *CompiledProfile) SelectionDefaults() contract.ResourceSelectionSpec {
	if p == nil {
		return contract.ResourceSelectionSpec{}
	}

	selection := p.spec
	selection.ObjectScope = cloneObjectScope(p.spec.ObjectScope)
	selection.Resources = append([]string(nil), p.spec.Resources...)

	return selection
}

// PVCSelectionDefaults returns profile defaults for PVC data collection.
// The returned namespace slice can be changed without mutating the compiled profile.
func (p *CompiledProfile) PVCSelectionDefaults() contract.PVCSelectionSpec {
	if p == nil {
		return contract.PVCSelectionSpec{}
	}

	selection := p.pvcSpec
	selection.ObjectScope = cloneObjectScope(p.pvcSpec.ObjectScope)

	return selection
}

// ImageSelectionDefaults returns profile defaults for image collection.
// The returned namespace slice can be changed without mutating the compiled profile.
func (p *CompiledProfile) ImageSelectionDefaults() contract.ImageSelectionSpec {
	if p == nil {
		return contract.ImageSelectionSpec{}
	}

	selection := p.imageSpec
	selection.ObjectScope = cloneObjectScope(p.imageSpec.ObjectScope)
	selection.Owners = cloneOwnerSelection(p.imageSpec.Owners)
	selection.ContainerTypes = append([]contract.ContainerType(nil), p.imageSpec.ContainerTypes...)
	selection.References = append([]contract.ImageReferenceSpec(nil), p.imageSpec.References...)
	for index := range selection.References {
		selection.References[index].Tags = append([]string(nil), p.imageSpec.References[index].Tags...)
	}

	return selection
}

// Apply returns a deep-copied object after applying every matching normalization rule.
func (p *CompiledProfile) Apply(object state.Object) (state.Object, error) {
	if p == nil {
		return state.Object{}, errors.New("profile is nil")
	}
	if err := object.Validate(); err != nil {
		return state.Object{}, fmt.Errorf("validate object before profile: %w", err)
	}

	context := newMatchContext(object)
	value := object.Value.DeepCopy()
	for _, rule := range p.rules {
		if !rule.matches(context) {
			continue
		}

		if rule.action != ruleRemove {
			continue
		}

		for _, path := range rule.paths {
			unstructured.RemoveNestedField(value.Object, path...)
		}
	}
	if p.omitEmpty {
		omitEmptyFields(value.Object)
	}

	result, err := state.NewObject(object.Identity, value)
	if err != nil {
		return state.Object{}, err
	}

	if err := result.ValidateCanonical(); err != nil {
		return state.Object{}, fmt.Errorf("validate profile result: %w", err)
	}

	return result, nil
}

// compileSelectionMatcher prepares shared metadata, namespace, and name filters.
func compileSelectionMatcher(spec contract.ObjectScope) (selectionMatcher, error) {
	labelsSelector, err := compileSelector(spec.LabelSelector, "labelSelector")
	if err != nil {
		return selectionMatcher{}, err
	}

	annotationsSelector, err := compileAnnotationSelector(spec.AnnotationSelector, "annotationSelector")
	if err != nil {
		return selectionMatcher{}, err
	}
	if err := validateGlobPatterns("namespaces", spec.Namespaces); err != nil {
		return selectionMatcher{}, err
	}
	if err := validateGlobPatterns("names", spec.Names); err != nil {
		return selectionMatcher{}, err
	}

	return selectionMatcher{
		namespaces:  newPatternMatcher(spec.Namespaces),
		names:       newPatternMatcher(spec.Names),
		labels:      labelsSelector,
		annotations: annotationsSelector,
	}, nil
}

// compileImageSelection prepares all filters used before image registry access.
func compileImageSelection(spec contract.ImageSelectionSpec) (imageSelectionMatcher, error) {
	// Pod filters and owner filters are independent scopes:
	// a reference must come from an allowed Pod,
	// and owner predicates apply only when configured.
	pods, err := compileSelectionMatcher(spec.ObjectScope)
	if err != nil {
		return imageSelectionMatcher{}, err
	}

	ownerSelection, err := compileSelectionMatcher(spec.Owners.ObjectScope)
	if err != nil {
		return imageSelectionMatcher{}, fmt.Errorf("owners: %w", err)
	}

	ownerResources, err := compileResourceExpressions(spec.Owners.Resources)
	if err != nil {
		return imageSelectionMatcher{}, fmt.Errorf("owners resources: %w", err)
	}
	for _, resource := range ownerResources {
		if resource.Exclude {
			return imageSelectionMatcher{}, errors.New("owners resources cannot contain exclusions")
		}
	}

	containerTypes := make(map[contract.ContainerType]struct{}, len(spec.ContainerTypes))

	// An empty containerTypes list means all supported container kinds.
	// When the list is present, reject unknown values during profile loading
	// instead of silently dropping image references at runtime.
	for _, containerType := range spec.ContainerTypes {
		switch containerType {
		case contract.ContainerTypeContainer, contract.ContainerTypeInitContainer:
			containerTypes[containerType] = struct{}{}

		default:
			return imageSelectionMatcher{}, fmt.Errorf("unsupported container type %q", containerType)
		}
	}

	references := make([]imageReferenceMatcher, 0, len(spec.References))
	for index, reference := range spec.References {
		if reference.Registry == "" || reference.Repository == "" {
			return imageSelectionMatcher{}, fmt.Errorf(
				"references[%d] registry and repository are required", index,
			)
		}

		if err := validateGlobPatterns(
			fmt.Sprintf("references[%d]", index),
			[]string{reference.Registry, reference.Repository},
		); err != nil {
			return imageSelectionMatcher{}, err
		}

		if err := validateGlobPatterns(
			fmt.Sprintf("references[%d].tags", index), reference.Tags,
		); err != nil {
			return imageSelectionMatcher{}, err
		}

		references = append(references, imageReferenceMatcher{
			registry:   reference.Registry,
			repository: reference.Repository,
			tags:       append([]string(nil), reference.Tags...),
		})
	}

	return imageSelectionMatcher{
		pods: pods,
		owners: ownerSelectionMatcher{
			enabled: !objectScopeEmpty(spec.Owners.ObjectScope) ||
				len(spec.Owners.Resources) > 0,
			selection: ownerSelection,
			resources: ownerResources,
		},
		containerTypes: containerTypes,
		references:     references,
	}, nil
}

// objectScopeEmpty reports whether a scope has no positive filters.
func objectScopeEmpty(spec contract.ObjectScope) bool {
	return len(spec.Namespaces) == 0 && len(spec.Names) == 0 &&
		spec.LabelSelector == nil && spec.AnnotationSelector == nil
}

// cloneObjectScope copies mutable slices and selector values used by a profile selection.
func cloneObjectScope(scope contract.ObjectScope) contract.ObjectScope {
	result := scope
	result.Namespaces = append([]string(nil), scope.Namespaces...)
	result.Names = append([]string(nil), scope.Names...)

	if scope.LabelSelector != nil {
		selector := *scope.LabelSelector
		selector.MatchLabels = maps.Clone(scope.LabelSelector.MatchLabels)
		selector.MatchExpressions = cloneRequirements(scope.LabelSelector.MatchExpressions)
		result.LabelSelector = &selector
	}
	if scope.AnnotationSelector != nil {
		selector := *scope.AnnotationSelector
		selector.MatchLabels = maps.Clone(scope.AnnotationSelector.MatchLabels)
		selector.MatchExpressions = cloneRequirements(scope.AnnotationSelector.MatchExpressions)
		result.AnnotationSelector = &selector
	}

	return result
}

// cloneRequirements copies selector requirements without sharing backing arrays.
func cloneRequirements(values []contract.SelectorRequirement) []contract.SelectorRequirement {
	result := append([]contract.SelectorRequirement(nil), values...)
	for index := range result {
		result[index].Values = append([]string(nil), values[index].Values...)
	}

	return result
}

// cloneOwnerSelection copies owner selectors and resource expressions.
func cloneOwnerSelection(selection contract.OwnerSelectionSpec) contract.OwnerSelectionSpec {
	result := selection
	result.ObjectScope = cloneObjectScope(selection.ObjectScope)
	result.Resources = append([]string(nil), selection.Resources...)

	return result
}

// validateGlobPatterns rejects malformed shell-style globs before collection starts.
func validateGlobPatterns(field string, values []string) error {
	for index, value := range values {
		if value == "" {
			return fmt.Errorf("%s[%d] cannot be empty", field, index)
		}
		if _, err := path.Match(value, ""); err != nil {
			return fmt.Errorf("invalid %s pattern %q: %w", field, value, err)
		}
	}

	return nil
}

// globMatches applies a validated shell-style glob and treats invalid patterns as non-matches.
func globMatches(pattern, value string) bool {
	matched, err := path.Match(pattern, value)
	return err == nil && matched
}

// compileRule validates one normalization rule and prepares its matchers.
func compileRule(name string, spec contract.RuleSpec) (compiledRule, error) {
	resources, err := compileResourceExpressions(spec.Match.Resources)
	if err != nil {
		return compiledRule{}, fmt.Errorf("rule %q resources: %w", name, err)
	}

	for _, resource := range resources {
		if resource.Exclude {
			return compiledRule{}, fmt.Errorf("rule %q cannot invert a resource match", name)
		}
	}

	labelsSelector, err := compileSelector(spec.Match.LabelSelector, "labelSelector")
	if err != nil {
		return compiledRule{}, fmt.Errorf("rule %q: %w", name, err)
	}

	annotationSelector, err := compileAnnotationSelector(spec.Match.AnnotationSelector, "annotationSelector")
	if err != nil {
		return compiledRule{}, fmt.Errorf("rule %q: %w", name, err)
	}

	action, pathsSource, err := ruleAction(spec)
	if err != nil {
		return compiledRule{}, fmt.Errorf("rule %q: %w", name, err)
	}

	paths := make([][]string, 0, len(pathsSource))
	for _, expression := range pathsSource {
		path, err := parsePath(expression)
		if err != nil {
			return compiledRule{}, fmt.Errorf("rule %q: %w", name, err)
		}

		paths = append(paths, path)
	}

	return compiledRule{
		action:      action,
		name:        name,
		resources:   resources,
		namespaces:  newPatternMatcher(spec.Match.Namespaces),
		names:       newPatternMatcher(spec.Match.Names),
		labels:      labelsSelector,
		annotations: annotationSelector,
		paths:       paths,
	}, nil
}

// ruleAction validates the mutually exclusive rule action fields.
func ruleAction(spec contract.RuleSpec) (string, []string, error) {
	actions := 0
	if len(spec.Remove) > 0 {
		actions++
	}
	if len(spec.Encrypt) > 0 {
		actions++
	}

	if actions != 1 {
		return "", nil, errors.New("exactly one of remove or encrypt is required")
	}
	if len(spec.Remove) > 0 {
		return ruleRemove, spec.Remove, nil
	}

	return ruleEncrypt, spec.Encrypt, nil
}

// compileSelector converts the readable label selector into Kubernetes syntax.
func compileSelector(expression *contract.Selector, field string) (labels.Selector, error) {
	if expression == nil {
		return labels.Everything(), nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels:      expression.MatchLabels,
		MatchExpressions: toKubernetesRequirements(expression.MatchExpressions),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}

	return selector, nil
}

// compileAnnotationSelector converts annotation equality and expressions.
func compileAnnotationSelector(expression *contract.AnnotationSelector, field string) (labels.Selector, error) {
	if expression == nil {
		return labels.Everything(), nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels:      expression.MatchLabels,
		MatchExpressions: toKubernetesRequirements(expression.MatchExpressions),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}

	return selector, nil
}

// toKubernetesRequirements converts the public contract into the Kubernetes
// selector type only at the runtime boundary.
func toKubernetesRequirements(requirements []contract.SelectorRequirement) []metav1.LabelSelectorRequirement {
	converted := make([]metav1.LabelSelectorRequirement, len(requirements))
	for index, requirement := range requirements {
		converted[index] = metav1.LabelSelectorRequirement{
			Key:      requirement.Key,
			Operator: metav1.LabelSelectorOperator(requirement.Operator),
			Values:   append([]string(nil), requirement.Values...),
		}
	}

	return converted
}

// compileResourceExpressions parses resource selectors from a profile.
func compileResourceExpressions(values []string) ([]ResourceExpression, error) {
	result := make([]ResourceExpression, 0, len(values))
	for _, value := range values {
		expression, err := ParseResourceExpression(value)
		if err != nil {
			return nil, err
		}

		result = append(result, expression)
	}

	return result, nil
}

// matches reports whether a normalized object satisfies a compiled rule.
func (r compiledRule) matches(context matchContext) bool {
	if len(r.resources) > 0 && !resourceExpressionsMatch(
		r.resources,
		context.group,
		context.version,
		context.resource,
		context.name,
	) {
		return false
	}

	return r.namespaces.matches(context.namespace) &&
		r.names.matches(context.name) &&
		r.labels.Matches(labels.Set(context.labels)) &&
		r.annotations.Matches(labels.Set(context.annotations))
}

// resourceExpressionsMatch reports whether any expression matches an identity.
func resourceExpressionsMatch(expressions []ResourceExpression, group, version, resource, name string) bool {
	for _, expression := range expressions {
		if !expression.Exclude && expression.Matches(group, version, resource, name) {
			return true
		}
	}

	return false
}

// newMatchContext returns object metadata for read-only rule matching.
func newMatchContext(object state.Object) matchContext {
	return matchContext{
		group:       object.Identity.Group,
		version:     object.Identity.Version,
		resource:    object.Identity.Resource,
		namespace:   object.Identity.Namespace,
		name:        object.Identity.Name,
		labels:      object.Value.GetLabels(),
		annotations: object.Value.GetAnnotations(),
	}
}

// newPatternMatcher creates an ordered glob matcher from profile values.
func newPatternMatcher(values []string) patternMatcher {
	return patternMatcher{patterns: append([]string(nil), values...)}
}

// matches reports whether a value satisfies ordered glob patterns.
// If positive patterns exist, matching starts disabled; otherwise it starts enabled.
// The last matching pattern wins, so a later positive pattern can re-include a value.
func (m patternMatcher) matches(value string) bool {
	if len(m.patterns) == 0 {
		return true
	}

	selected := true
	for _, pattern := range m.patterns {
		if !strings.HasPrefix(pattern, "!") {
			selected = false
			break
		}
	}

	for _, pattern := range m.patterns {
		exclude := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		if globMatches(pattern, value) {
			selected = !exclude
		}
	}

	return selected
}

// parsePath accepts ordinary dot paths and quoted map keys used by annotations.
func parsePath(expression string) ([]string, error) {
	if expression == "" || expression[0] != '.' {
		return nil, fmt.Errorf("path %q must start with a dot", expression)
	}

	var path []string
	for rest := expression[1:]; rest != ""; {
		if rest[0] == '.' {
			rest = rest[1:]
			if rest == "" {
				return nil, fmt.Errorf("invalid path %q", expression)
			}
		}

		if rest[0] == '[' {
			end := strings.Index(rest, "]")
			if end < 4 || rest[1] != '"' || rest[end-1] != '"' {
				return nil, fmt.Errorf("invalid path %q", expression)
			}

			path = append(path, rest[2:end-1])
			rest = rest[end+1:]
			if rest != "" && rest[0] != '.' && rest[0] != '[' {
				return nil, fmt.Errorf("invalid path %q", expression)
			}

			continue
		}

		end := len(rest)
		if dot := strings.IndexByte(rest, '.'); dot >= 0 {
			end = dot
		}
		if bracket := strings.IndexByte(rest, '['); bracket >= 0 && bracket < end {
			end = bracket
		}
		if end == 0 {
			return nil, fmt.Errorf("invalid path %q", expression)
		}

		path = append(path, rest[:end])
		rest = rest[end:]
	}

	if len(path) == 0 {
		return nil, fmt.Errorf("invalid path %q", expression)
	}

	return path, nil
}
