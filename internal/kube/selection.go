// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package kube

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/woozymasta/kube-dump/v2/internal/profile"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// EffectiveSelection merges profile defaults with explicit command-line overrides before discovery starts.
// A non-empty CLI dimension replaces the corresponding profile dimension.
func EffectiveSelection(defaults contract.ResourceSelectionSpec, overrides Selection) (Selection, error) {
	selection := overrides
	if len(selection.Namespaces) == 0 {
		selection.Namespaces = append([]string(nil), defaults.Namespaces...)
	}

	if len(overrides.Resources) == 0 {
		selection.Resources = append([]string(nil), defaults.Resources...)
	}

	profileLabels, err := selectorText(defaults.LabelSelector)
	if err != nil {
		return Selection{}, fmt.Errorf("compile profile label selector: %w", err)
	}
	if selection.LabelSelector == "" {
		selection.LabelSelector = profileLabels
	}

	profileAnnotations, err := annotationSelectorText(defaults.AnnotationSelector)
	if err != nil {
		return Selection{}, fmt.Errorf("compile profile annotation selector: %w", err)
	}
	if selection.AnnotationSelector == "" {
		selection.AnnotationSelector = profileAnnotations
	}

	return selection, nil
}

// ObjectNameFieldSelector returns a server-side name selector
// when resource expressions identify exactly one positive object name for the given GVR.
// Ordered exclusions remain client-side because they can change the final decision
// without making the selected resource set any broader.
func (s Selection) ObjectNameFieldSelector(group, version, resource string) string {
	var name string
	for _, value := range s.Resources {
		expression, err := profile.ParseResourceExpression(value)
		if err != nil || expression.Exclude {
			continue
		}

		if !expression.MatchesResource(group, version, resource) {
			continue
		}
		if expression.Name == "" || expression.Name == "*" ||
			strings.ContainsAny(expression.Name, "*?\\[") {
			return ""
		}

		if name != "" && name != expression.Name {
			return ""
		}
		name = expression.Name
	}

	if name == "" {
		return ""
	}

	return fields.OneTermEqualSelector("metadata.name", name).String()
}

// exactNamespacePatterns reports whether profile namespaces can be passed directly
// to the Kubernetes namespace-scoped list operations.
func exactNamespacePatterns(values []string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, "!") || strings.ContainsAny(value, "*?\\[") {
			return false
		}
	}

	return true
}

// resolveResourceAliases expands kubectl-style short names in CLI selectors.
// Short names come from Kubernetes discovery,
// so built-in resources and CRDs use the aliases advertised by the connected cluster
// instead of a hard-coded list that could become incomplete or stale.
func resolveResourceAliases(selection Selection, lists []*metav1.APIResourceList) (Selection, error) {
	if len(selection.Resources) == 0 {
		return selection, nil
	}

	aliases := resourceAliases(lists)
	exactResources := make(map[string]struct{})
	for _, list := range lists {
		for _, resource := range list.APIResources {
			if resource.Name == "" || strings.ContainsRune(resource.Name, '/') ||
				!slices.Contains(resource.Verbs, "list") {
				continue
			}

			exactResources[resource.Name] = struct{}{}
		}
	}

	resolved := make([]string, 0, len(selection.Resources))
	for _, value := range selection.Resources {
		exclude := strings.HasPrefix(value, "!")
		expression := strings.TrimPrefix(value, "!")
		resource, suffix := splitResourceSelector(expression)

		// A qualified selector is already unambiguous and must retain its exact matching semantics.
		// Only a bare resource token can be an alias.
		if strings.ContainsAny(resource, "/.") {
			resolved = append(resolved, value)
			continue
		}

		if _, ok := exactResources[resource]; ok {
			// A discovered plural is more specific than a short name.
			// This also lets a CRD named like a kubectl alias remain selectable by name.
			resolved = append(resolved, value)
			continue
		}

		targets, ok := aliases[resource]
		if !ok {
			// Unknown bare names are valid plural resource names,
			// including custom resources that do not advertise a short name.
			resolved = append(resolved, value)
			continue
		}
		if len(targets) > 1 {
			sort.Strings(targets)
			return Selection{}, fmt.Errorf(
				"resource short name %q is ambiguous (%s); use full group/version/resource",
				resource, strings.Join(targets, ", "))
		}

		target := targets[0]
		if exclude {
			target = "!" + target
		}
		resolved = append(resolved, target+suffix)
	}

	selection.Resources = resolved
	return selection, nil
}

// resourceAliases returns canonical selector candidates keyed by their short names.
// Ambiguity is checked only when a selector actually uses it.
func resourceAliases(lists []*metav1.APIResourceList) map[string][]string {
	candidates := make(map[string][]string)

	for _, list := range lists {
		groupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}

		for _, resource := range list.APIResources {
			if resource.Name == "" || strings.ContainsRune(resource.Name, '/') ||
				!slices.Contains(resource.Verbs, "list") {
				continue
			}

			group := groupVersion.Group
			if group == "" {
				group = "core"
			}
			canonical := group + "/" + groupVersion.Version + "/" + resource.Name

			for _, alias := range resource.ShortNames {
				if alias == "" {
					continue
				}
				if !slices.Contains(candidates[alias], canonical) {
					candidates[alias] = append(candidates[alias], canonical)
				}
			}
		}
	}

	return candidates
}

// splitResourceSelector separates a resource token from its optional object name.
func splitResourceSelector(value string) (resource, suffix string) {
	separator := strings.IndexByte(value, ':')
	if separator < 0 {
		return value, ""
	}

	return value[:separator], value[separator:]
}

// selectorText converts the structured profile selector to Kubernetes syntax.
func selectorText(value *contract.Selector) (string, error) {
	if value == nil {
		return "", nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels:      value.MatchLabels,
		MatchExpressions: toRequirements(value.MatchExpressions),
	})
	if err != nil {
		return "", err
	}

	return selector.String(), nil
}

// annotationSelectorText converts the annotation selector to the same Kubernetes selector syntax
// because annotation values use identical matching.
func annotationSelectorText(value *contract.AnnotationSelector) (string, error) {
	if value == nil {
		return "", nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels:      value.MatchLabels,
		MatchExpressions: toRequirements(value.MatchExpressions),
	})
	if err != nil {
		return "", err
	}

	return selector.String(), nil
}

// toRequirements converts the public selector contract at the Kubernetes API boundary.
func toRequirements(values []contract.SelectorRequirement) []metav1.LabelSelectorRequirement {
	result := make([]metav1.LabelSelectorRequirement, len(values))
	for index, value := range values {
		result[index] = metav1.LabelSelectorRequirement{
			Key:      value.Key,
			Operator: metav1.LabelSelectorOperator(value.Operator),
			Values:   append([]string(nil), value.Values...),
		}
	}

	return result
}
