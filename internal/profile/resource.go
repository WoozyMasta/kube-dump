// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"errors"
	"fmt"
	"strings"
)

// ResourceExpression identifies an API resource and an optional object name.
//
// Accepted forms are a bare resource name, version/resource, or group/version/resource.
// A bare resource matches any API group and version;
// the two-component form addresses a core API resource.
type ResourceExpression struct {
	// Group is the API group, "core" for core resources, or "*" for any group.
	Group string
	// Version is the API version or "*" for any version.
	Version string
	// Resource is the plural API resource name or "*" for any resource.
	Resource string
	// Name optionally restricts the expression to one object name.
	Name string
	// Exclude makes a matching ordered selection entry remove an object.
	Exclude bool
}

// ParseResourceExpression parses a resource selection expression.
func ParseResourceExpression(value string) (ResourceExpression, error) {
	if value == "" {
		return ResourceExpression{}, errors.New("resource expression cannot be empty")
	}

	exclude := strings.HasPrefix(value, "!")
	if exclude {
		value = strings.TrimPrefix(value, "!")
		if value == "" {
			return ResourceExpression{}, errors.New("resource expression cannot be only '!'")
		}
	}

	if value == "*" {
		return ResourceExpression{
			Group:    "*",
			Version:  "*",
			Resource: "*",
			Exclude:  exclude,
		}, nil
	}

	parts := strings.Split(value, ":")
	if len(parts) > 2 || parts[0] == "" {
		return ResourceExpression{}, invalidResourceExpression(value)
	}

	name := ""
	if len(parts) == 2 {
		name = parts[1]
		if name == "" {
			return ResourceExpression{}, fmt.Errorf(
				"resource expression %q has an empty name", value)
		}
	}

	identity := strings.Split(parts[0], "/")
	expression := ResourceExpression{Name: name, Exclude: exclude}
	switch len(identity) {
	case 1:
		if looksLikeAPIVersion(identity[0]) {
			return ResourceExpression{}, invalidResourceExpression(value)
		}

		// Bare resource names are intentionally group/version agnostic.
		expression.Group = "*"
		expression.Version = "*"
		expression.Resource = identity[0]

	case 2:
		// The short two-component form addresses core resources.
		expression.Group = "core"
		expression.Version = identity[0]
		expression.Resource = identity[1]

	case 3:
		expression.Group = identity[0]
		expression.Version = identity[1]
		expression.Resource = identity[2]

	default:
		return ResourceExpression{}, invalidResourceExpression(value)
	}

	if expression.Group == "" || expression.Version == "" || expression.Resource == "" {
		return ResourceExpression{}, fmt.Errorf(
			"resource expression %q contains an empty GVR component", value)
	}

	return expression, nil
}

// Matches reports whether the expression matches an object identity.
func (e ResourceExpression) Matches(group, version, resource, name string) bool {
	if !e.MatchesResource(group, version, resource) {
		return false
	}

	return e.Name == "" || e.Name == "*" || e.Name == name
}

// MatchesResource reports whether the expression matches an API resource,
// ignoring its optional object name.
func (e ResourceExpression) MatchesResource(group, version, resource string) bool {
	actualGroup := group
	if actualGroup == "" {
		actualGroup = "core"
	}

	return (e.Group == "*" || e.Group == actualGroup) &&
		(e.Version == "*" || e.Version == version) &&
		(e.Resource == "*" || e.Resource == resource)
}

// invalidResourceExpression reports the accepted syntax for a resource expression.
func invalidResourceExpression(value string) error {
	return fmt.Errorf(
		"resource expression %q must use [group/]version/resource[:name] or resource[:name]",
		value,
	)
}

// looksLikeAPIVersion reports whether value is a Kubernetes v-prefixed numeric version.
func looksLikeAPIVersion(value string) bool {
	if len(value) < 2 || value[0] != 'v' {
		return false
	}

	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}

	return true
}
