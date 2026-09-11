// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package state

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Object is the canonical representation of one Kubernetes resource object.
// Identity is stored separately from the unstructured payload
// so the state path and the object metadata can be checked against
// each other at every persistence boundary.
type Object struct {
	// Value contains the unstructured Kubernetes object.
	Value *unstructured.Unstructured
	// Identity is the stable resource identity used for paths and comparisons.
	Identity Identity
}

// NewObject constructs an object and stores a deep copy of value.
// The copy prevents callers from mutating the canonical model after it has passed identity validation.
func NewObject(identity Identity, value *unstructured.Unstructured) (Object, error) {
	if err := identity.Validate(); err != nil {
		return Object{}, fmt.Errorf("validate object identity: %w", err)
	}
	if value == nil {
		return Object{}, errors.New("object value is required")
	}

	return Object{Identity: identity, Value: value.DeepCopy()}, nil
}

// Validate checks the object identity and verifies that its metadata agrees
// with the identity used by the state store.
func (o Object) Validate() error {
	if err := o.Identity.Validate(); err != nil {
		return err
	}
	if o.Value == nil {
		return errors.New("object value is required")
	}

	// API identity checks prevent a correctly named state path
	// from carrying a payload for another kind or API version into restore.
	expectedAPIVersion := o.Identity.Version
	if o.Identity.Group != "" {
		expectedAPIVersion = o.Identity.Group + "/" + o.Identity.Version
	}
	if apiVersion := o.Value.GetAPIVersion(); apiVersion != "" && apiVersion != expectedAPIVersion {
		return fmt.Errorf("apiVersion %q does not match identity apiVersion %q", apiVersion, expectedAPIVersion)
	}
	if kind := o.Value.GetKind(); kind != "" && kind != o.Identity.Kind {
		return fmt.Errorf("kind %q does not match identity kind %q", kind, o.Identity.Kind)
	}

	// Metadata checks catch mismatches between discovery identity
	// and API data before an object reaches normalization or persistent storage.
	if err := validateIdentityField(o.Value.Object, "name", o.Identity.Name); err != nil {
		return err
	}
	if err := validateIdentityField(o.Value.Object, "namespace", o.Identity.Namespace); err != nil {
		return err
	}

	return nil
}

// validateIdentityField checks metadata identity while allowing an encrypted
// tagged value used by the field-encryption pipeline before restoration.
func validateIdentityField(object map[string]any, field, expected string) error {
	value, found, err := unstructured.NestedFieldNoCopy(object, "metadata", field)
	if err != nil {
		return fmt.Errorf("read metadata.%s: %w", field, err)
	}
	if !found || isTaggedValue(value) {
		return nil
	}

	actual, ok := value.(string)
	if !ok {
		return fmt.Errorf("metadata.%s must be a string", field)
	}
	if actual != expected {
		return fmt.Errorf("metadata.%s %q does not match identity %s %q", field, actual, field, expected)
	}

	return nil
}

// isTaggedValue recognizes the serialized marker envelope
// without importing the field-encryption package, which already depends on state.Object.
func isTaggedValue(value any) bool {
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 3 {
		return false
	}
	if fields["__kube_dump_marker"] != "kube-dump/v2" {
		return false
	}
	if _, ok := fields["value"].(string); !ok {
		return false
	}

	tag, ok := fields["__kube_dump_tag"].(string)
	if !ok {
		return false
	}

	switch tag {
	case "age", "age-decrypted", "aes-siv", "aes-siv-decrypted", "metadata":
		return true
	default:
		return false
	}
}

// ValidateCanonical verifies that a persisted Kubernetes object retains
// the identity fields required to serialize, compare, and restore it safely.
//
// Validate intentionally permits absent metadata for intermediate objects;
// canonical state has a stricter contract after profile transformations.
func (o Object) ValidateCanonical() error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Value.GetAPIVersion() == "" {
		return errors.New("canonical object apiVersion is required")
	}
	if o.Value.GetKind() == "" {
		return errors.New("canonical object kind is required")
	}

	metadataName, found, err := unstructured.NestedString(o.Value.Object, "metadata", "name")
	if err != nil {
		return fmt.Errorf("read canonical metadata.name: %w", err)
	}
	if !found || metadataName == "" {
		return errors.New("canonical object metadata.name is required")
	}

	if o.Identity.Namespace != "" {
		metadataNamespace, found, err := unstructured.NestedString(o.Value.Object, "metadata", "namespace")
		if err != nil {
			return fmt.Errorf("read canonical metadata.namespace: %w", err)
		}

		if !found || metadataNamespace == "" {
			return errors.New("canonical namespaced object metadata.namespace is required")
		}
	}

	return nil
}
