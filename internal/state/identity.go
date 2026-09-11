// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package state defines the canonical Kubernetes backup model.
package state

import (
	"fmt"
	"strings"
)

// Identity uniquely identifies a Kubernetes object within an API resource.
// Resource and version address the dynamic API endpoint;
// kind and name guard the payload, while Namespace is empty for cluster-scoped resources.
type Identity struct {
	// Group is the API group; core resources use an empty group.
	Group string `json:"group" yaml:"group"`
	// Version is the API version containing the resource.
	Version string `json:"version" yaml:"version"`
	// Resource is the plural API resource name.
	Resource string `json:"resource" yaml:"resource"`
	// Kind is the Kubernetes object kind.
	Kind string `json:"kind" yaml:"kind"`
	// Namespace is the object namespace, or empty for cluster-scoped objects.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	// Name is the object name within its scope.
	Name string `json:"name" yaml:"name"`
}

// Validate checks that the identity contains enough information to address an
// object through the Kubernetes dynamic API.
func (i Identity) Validate() error {
	if i.Group != "" {
		if err := validateIdentityPart("group", i.Group); err != nil {
			return err
		}
	}
	if err := validateIdentityPart("version", i.Version); err != nil {
		return err
	}
	if err := validateIdentityPart("resource", i.Resource); err != nil {
		return err
	}
	if err := validateIdentityPart("kind", i.Kind); err != nil {
		return err
	}
	if err := validateIdentityPart("name", i.Name); err != nil {
		return err
	}
	if i.Namespace != "" {
		if err := validateIdentityPart("namespace", i.Namespace); err != nil {
			return err
		}
	}

	return nil
}

// Key returns the stable filesystem-independent key used to compare objects.
// It is an ordering and map key, not an API URL,
// so empty group or namespace segments are intentionally retained.
func (i Identity) Key() string {
	return strings.Join([]string{i.Group, i.Version, i.Resource, i.Namespace, i.Name}, "/")
}

// validateIdentityPart validates an identity component that cannot be empty.
func validateIdentityPart(field, value string) error {
	if value == "" {
		return fmt.Errorf("identity %s is required", field)
	}

	if strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("identity %s contains a path separator: %q", field, value)
	}

	return nil
}
