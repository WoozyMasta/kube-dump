package state

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNewObjectCopiesValue(t *testing.T) {
	t.Parallel()

	value := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "api"},
	}}
	identity := Identity{Version: "v1", Resource: "pods", Kind: "Pod", Name: "api"}
	object, err := NewObject(identity, value)
	if err != nil {
		t.Fatalf("NewObject() error = %v", err)
	}
	value.Object["changed"] = true
	if _, found := object.Value.Object["changed"]; found {
		t.Fatal("NewObject() did not copy the input value")
	}
}

func TestObjectValidateRejectsMetadataMismatch(t *testing.T) {
	t.Parallel()

	object := Object{
		Identity: Identity{Version: "v1", Resource: "pods", Kind: "Pod", Name: "api"},
		Value: &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "other"},
		}},
	}
	if err := object.Validate(); err == nil {
		t.Fatal("Object.Validate() returned nil for mismatched metadata.name")
	}
}

func TestObjectValidateAllowsEncryptedMetadataIdentity(t *testing.T) {
	t.Parallel()

	object := Object{
		Identity: Identity{
			Version:   "v1",
			Resource:  "secrets",
			Kind:      "Secret",
			Namespace: "ci",
			Name:      "credentials",
		},
		Value: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      taggedIdentityValue("encrypted-name"),
				"namespace": taggedIdentityValue("encrypted-namespace"),
			},
		}},
	}
	if err := object.Validate(); err != nil {
		t.Fatalf("Object.Validate() rejected encrypted metadata identity: %v", err)
	}
}

func taggedIdentityValue(value string) map[string]any {
	return map[string]any{
		"__kube_dump_marker": "kube-dump/v2",
		"__kube_dump_tag":    "age",
		"value":              value,
	}
}

func TestObjectValidateRejectsAPIVersionAndKindMismatch(t *testing.T) {
	t.Parallel()

	object := Object{
		Identity: Identity{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Name: "api"},
		Value: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "api"},
		}},
	}
	if err := object.Validate(); err == nil {
		t.Fatal("Object.Validate() accepted mismatched API identity")
	}
}
