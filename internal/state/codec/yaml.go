// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package codec implements codecs for the canonical state representation.
package codec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strings"

	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	marshalContextNone marshalContext = iota
	marshalContextSecretRoot
	marshalContextSecretData
)

// marshalContext tracks Secret.data only because that field has special base64 handling.
// Tagged values themselves are valid at any scalar field.
type marshalContext uint8

// Marshal encodes a Kubernetes object as deterministic YAML ending in one newline.
func Marshal(object state.Object) ([]byte, error) {
	if err := object.Validate(); err != nil {
		return nil, fmt.Errorf("validate object before YAML encoding: %w", err)
	}

	context := marshalContextNone
	if object.Identity.Kind == "Secret" || object.Value.GetKind() == "Secret" {
		context = marshalContextSecretRoot
	}

	node, err := marshalNode(object.Value.Object, context)
	if err != nil {
		return nil, fmt.Errorf("marshal object as YAML: %w", err)
	}

	data, err := yaml.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("encode YAML node: %w", err)
	}

	return data, nil
}

// Unmarshal decodes one YAML document into an unstructured Kubernetes object.
// Multiple documents are rejected so one state file cannot contain ambiguous resource boundaries.
func Unmarshal(data []byte) (*unstructured.Unstructured, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode YAML object: %w", err)
	}
	if len(document.Content) == 0 {
		return nil, errors.New("decode YAML object: document is empty")
	}

	context := marshalContextNone
	if yamlObjectKind(document.Content[0]) == "Secret" {
		context = marshalContextSecretRoot
	}

	value, err := unmarshalNode(document.Content[0], context)
	if err != nil {
		return nil, fmt.Errorf("decode YAML values: %w", err)
	}

	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("decode YAML object: root must be a mapping")
	}

	value, err = normalizeJSONValue(value)
	if err != nil {
		return nil, fmt.Errorf("normalize YAML values: %w", err)
	}

	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("normalized YAML object: root must be a mapping")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode YAML object: multiple documents are not supported")
		}
		return nil, fmt.Errorf("check YAML document count: %w", err)
	}

	return &unstructured.Unstructured{Object: object}, nil
}

// normalizeJSONValue converts YAML-native numeric values into the scalar types
// accepted by Kubernetes unstructured objects.
// YAML decoders commonly produce int or uint values,
// while unstructured JSON-compatible data uses int64 and float64 for numbers.
func normalizeJSONValue(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, fmt.Errorf("normalize field %q: %w", key, err)
			}

			result[key] = normalized
		}
		return result, nil

	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, fmt.Errorf("normalize list item %d: %w", index, err)
			}

			result[index] = normalized
		}
		return result, nil
	}

	if value == nil {
		return nil, nil
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int(), nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		unsigned := reflected.Uint()
		if unsigned > math.MaxInt64 {
			return nil, fmt.Errorf("unsigned integer %d exceeds int64", unsigned)
		}
		return int64(unsigned), nil

	case reflect.Float32, reflect.Float64:
		return reflected.Float(), nil

	default:
		return value, nil
	}
}

// marshalNode converts values to deterministic YAML nodes.
// Tagged values are emitted as explicit scalar nodes wherever a profile placed them,
// including fields of custom resources.
func marshalNode(value any, context marshalContext) (*yaml.Node, error) {
	if tag, payload, ok := fieldcrypto.TaggedValueParts(value); ok {
		// Keep encryption markers as YAML tags rather than ordinary strings
		// so the decoder can restore them without the original profile.
		return &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!kube-dump/" + tag,
			Value: payload,
			Style: yaml.LiteralStyle,
		}, nil
	}

	switch typed := value.(type) {
	case map[string]any:
		// Go map iteration is randomized;
		// sorted keys keep generated manifests stable for review and Git diffs.
		node := &yaml.Node{Kind: yaml.MappingNode}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}

		sort.Strings(keys)
		for _, key := range keys {
			keyNode, err := marshalNode(key, marshalContextNone)
			if err != nil {
				return nil, err
			}

			childContext := marshalContextNone
			if context == marshalContextSecretRoot && key == "data" {
				childContext = marshalContextSecretData
			} else if context == marshalContextSecretData {
				childContext = marshalContextSecretData
			}

			valueNode, err := marshalNode(typed[key], childContext)
			if err != nil {
				return nil, fmt.Errorf("marshal field %q: %w", key, err)
			}

			node.Content = append(node.Content, keyNode, valueNode)
		}

		return node, nil

	case []any:
		// Sequence order is part of the object value, so recurse in source order.
		node := &yaml.Node{Kind: yaml.SequenceNode}
		for index, item := range typed {
			itemNode, err := marshalNode(item, context)
			if err != nil {
				return nil, fmt.Errorf("marshal list item %d: %w", index, err)
			}

			node.Content = append(node.Content, itemNode)
		}

		return node, nil

	default:
		// Let yaml.v3 preserve scalar typing uniformly,
		// then retain the resulting scalar node instead of maintaining a second conversion table here.
		data, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}

		var document yaml.Node
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, err
		}

		if len(document.Content) != 1 {
			return nil, errors.New("marshal scalar: unexpected YAML document")
		}

		return document.Content[0], nil
	}
}

// yamlObjectKind returns the root kind without decoding arbitrary object data.
func yamlObjectKind(node *yaml.Node) string {
	if node.Kind != yaml.MappingNode {
		return ""
	}

	for index := 0; index+1 < len(node.Content); index += 2 {
		var key string
		if err := node.Content[index].Decode(&key); err != nil || key != "kind" {
			continue
		}

		return node.Content[index+1].Value
	}

	return ""
}

// unmarshalNode converts tagged YAML scalars back to JSON-compatible markers.
// The tag is self-describing, so restoring it does not require the profile
// that originally selected the field.
func unmarshalNode(node *yaml.Node, context marshalContext) (any, error) {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, errors.New("YAML document must contain one value")
		}

		return unmarshalNode(node.Content[0], context)
	}

	if strings.HasPrefix(node.Tag, "!kube-dump/") {
		// Encrypted values are self-describing.
		// Unknown tags fail here instead of becoming ordinary strings
		// that could reach a later apply operation.
		if node.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("tagged value %q must be scalar", node.Tag)
		}

		tag := strings.TrimPrefix(node.Tag, "!kube-dump/")
		if !fieldcrypto.KnownTag(tag) {
			return nil, fmt.Errorf("unknown kube-dump tag %q", tag)
		}

		return fieldcrypto.NewTaggedValue(tag, node.Value), nil
	}

	switch node.Kind {
	case yaml.MappingNode:
		// Preserve the Secret.data context while descending;
		// only that known Kubernetes field receives Base64-specific handling.
		result := make(map[string]any, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			var key string
			if err := node.Content[index].Decode(&key); err != nil {
				return nil, fmt.Errorf("decode mapping key: %w", err)
			}

			childContext := marshalContextNone
			if context == marshalContextSecretRoot && key == "data" {
				childContext = marshalContextSecretData
			} else if context == marshalContextSecretData {
				childContext = marshalContextSecretData
			}

			value, err := unmarshalNode(node.Content[index+1], childContext)
			if err != nil {
				return nil, fmt.Errorf("decode mapping field %q: %w", key, err)
			}

			result[key] = value
		}

		return result, nil

	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for index, item := range node.Content {
			value, err := unmarshalNode(item, context)
			if err != nil {
				return nil, fmt.Errorf("decode list item %d: %w", index, err)
			}

			result = append(result, value)
		}

		return result, nil

	default:
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}

		return value, nil
	}
}
