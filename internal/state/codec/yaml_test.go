package codec

import (
	"bytes"
	"testing"

	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMarshalIsDeterministic(t *testing.T) {
	t.Parallel()

	object, err := state.NewObject(
		state.Identity{Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Name: "config"},
		&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "config"},
			"data":     map[string]any{"z": "last", "a": "first"},
		}},
	)
	if err != nil {
		t.Fatalf("state.NewObject() error = %v", err)
	}

	first, err := Marshal(object)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	second, err := Marshal(object)
	if err != nil {
		t.Fatalf("Marshal() second call error = %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("Marshal() is not deterministic:\n%s\n%s", first, second)
	}
	if len(first) == 0 || first[len(first)-1] != '\n' {
		t.Fatal("Marshal() output does not end with a newline")
	}
}

func TestUnmarshalRejectsMultipleDocuments(t *testing.T) {
	t.Parallel()

	_, err := Unmarshal(
		[]byte("apiVersion: v1\nkind: ConfigMap\n---\napiVersion: v1\nkind: Secret\n"),
	)
	if err == nil {
		t.Fatal("Unmarshal() accepted multiple YAML documents")
	}
}

func TestUnmarshalRejectsEmptyDocument(t *testing.T) {
	t.Parallel()

	_, err := Unmarshal([]byte("---\n"))
	if err == nil {
		t.Fatal("Unmarshal() accepted an empty YAML document")
	}
}

func TestUnmarshalNormalizesNumericValues(t *testing.T) {
	t.Parallel()

	object, err := Unmarshal(
		[]byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: test\nspec:\n  replicas: 3\n  ratio: 1.5\n"),
	)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := object.Object["spec"].(map[string]any); !ok {
		t.Fatal("spec is not a JSON-compatible map")
	}

	replicas, ok, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
	if err != nil || !ok || replicas != 3 {
		t.Fatalf("replicas = %d, found=%t, error=%v", replicas, ok, err)
	}
	if _, ok, err := unstructured.NestedFloat64(object.Object, "spec", "ratio"); err != nil || !ok {
		t.Fatalf("ratio is not float64: found=%t, error=%v", ok, err)
	}

	_ = object.DeepCopy()
}

func TestTaggedValueRoundTrip(t *testing.T) {
	t.Parallel()

	object, err := state.NewObject(
		state.Identity{Version: "v1", Resource: "secrets", Kind: "Secret", Name: "credentials"},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "credentials"},
			"data": map[string]any{
				"password": fieldcrypto.NewTaggedValue(
					"age",
					"-----BEGIN AGE ENCRYPTED FILE-----\nsecret\n-----END AGE ENCRYPTED FILE-----\n",
				),
			},
		}},
	)
	if err != nil {
		t.Fatalf("state.NewObject() error = %v", err)
	}

	encoded, err := Marshal(object)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !bytes.Contains(encoded, []byte("!kube-dump/age")) {
		t.Fatalf("encoded YAML does not contain age tag:\n%s", encoded)
	}

	decoded, err := Unmarshal(encoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	tag, payload, ok := fieldcrypto.TaggedValueParts(decoded.Object["data"].(map[string]any)["password"])
	if !ok || tag != "age" || payload == "" {
		t.Fatalf("unexpected decoded tagged value: tag=%q payload=%q ok=%v", tag, payload, ok)
	}
}

func TestTaggedValueRoundTripInCustomResourceField(t *testing.T) {
	t.Parallel()

	object, err := state.NewObject(
		state.Identity{
			Group: "example.io", Version: "v1", Resource: "widgets",
			Kind: "Widget", Name: "sample",
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.io/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "sample"},
			"spec": map[string]any{
				"password": fieldcrypto.NewTaggedValue("age", "encrypted-value"),
			},
		}},
	)
	if err != nil {
		t.Fatalf("state.NewObject() error = %v", err)
	}

	encoded, err := Marshal(object)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !bytes.Contains(encoded, []byte("!kube-dump/age")) {
		t.Fatalf("encoded YAML does not contain age tag:\n%s", encoded)
	}

	decoded, err := Unmarshal(encoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	value, found, err := unstructured.NestedFieldNoCopy(decoded.Object, "spec", "password")
	if err != nil || !found {
		t.Fatalf("read tagged custom field: found=%t error=%v", found, err)
	}

	tag, payload, ok := fieldcrypto.TaggedValueParts(value)
	if !ok || tag != "age" || payload != "encrypted-value" {
		t.Fatalf("unexpected tagged custom field: tag=%q payload=%q ok=%v", tag, payload, ok)
	}
}

func TestMarkerLikeCustomResourceMapIsPreserved(t *testing.T) {
	t.Parallel()

	object, err := state.NewObject(
		state.Identity{
			Group:    "example.io",
			Version:  "v1",
			Resource: "widgets",
			Kind:     "Widget",
			Name:     "sample",
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.io/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "sample"},
			"spec": map[string]any{
				"payload": map[string]any{
					"__kube_dump_tag": "application-defined-value",
					"value":           "hello",
				},
			},
		}},
	)
	if err != nil {
		t.Fatalf("state.NewObject() error = %v", err)
	}

	encoded, err := Marshal(object)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	decoded, err := Unmarshal(encoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	payload, found, err := unstructured.NestedMap(decoded.Object, "spec", "payload")
	if err != nil || !found {
		t.Fatalf("read custom payload: found=%t error=%v", found, err)
	}

	if len(payload) != 2 || payload["__kube_dump_tag"] != "application-defined-value" || payload["value"] != "hello" {
		t.Fatalf("custom marker-like map was changed: %#v", payload)
	}
}
