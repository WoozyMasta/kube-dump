package kube

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSelectionAllowsEmptyAndMetadataSelectors(t *testing.T) {
	object := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]any{"app": "api"},
			"annotations": map[string]any{"team": "platform"},
		},
	}}

	if !(Selection{}).allowsUnstructured(object) {
		t.Fatal("empty selection rejected object")
	}
	if !(Selection{LabelSelector: "app=api", AnnotationSelector: "team=platform"}).allowsUnstructured(object) {
		t.Fatal("include selectors rejected object")
	}
	if (Selection{LabelSelector: "app!=api"}).allowsUnstructured(object) {
		t.Fatal("negative selector accepted object")
	}
}

func TestEffectiveSelectionMergesProfileDefaultsAndCLIOverrides(t *testing.T) {
	defaults := contract.ResourceSelectionSpec{
		ObjectScope: contract.ObjectScope{
			Namespaces: []string{"status"},
			LabelSelector: &contract.Selector{
				MatchLabels: map[string]string{"app": "api"},
			},
		},
		Resources: []string{"*", "!core/v1/configmaps:kube-root-ca.crt"},
	}

	selection, err := EffectiveSelection(defaults, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Allows(metav1.APIResource{Name: "configmaps", Verbs: []string{"list"}}, "", "v1") {
		t.Fatal("profile wildcard did not select ConfigMaps for discovery")
	}

	rootCA := state.Identity{Version: "v1", Resource: "configmaps", Name: "kube-root-ca.crt", Namespace: "status"}
	if selection.AllowsIdentity(rootCA) {
		t.Fatal("profile resource exclusion did not reject kube-root-ca.crt")
	}

	overridden, err := EffectiveSelection(defaults, Selection{
		Namespaces: []string{"production"},
		Resources:  []string{"apps/v1/deployments"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.NamespaceSelected("status") || !overridden.NamespaceSelected("production") {
		t.Fatal("CLI namespace did not override profile namespaces")
	}
	if overridden.AllowsIdentity(rootCA) {
		t.Fatal("CLI resource override did not restrict resources")
	}
}

func TestEffectiveSelectionBroadensDiscoveryForNamespacePatterns(t *testing.T) {
	defaults := contract.ResourceSelectionSpec{
		ObjectScope: contract.ObjectScope{
			Namespaces: []string{"*", "!kube-*"},
		},
	}

	selection, err := EffectiveSelection(defaults, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.Validate(); err != nil {
		t.Fatalf("namespace patterns failed validation: %v", err)
	}
	if len(selection.Namespaces) != 2 {
		t.Fatalf("namespace patterns were not retained for effective selection: %#v", selection.Namespaces)
	}
	if selection.NamespaceSelected("kube-system") {
		t.Fatal("negative namespace pattern accepted an excluded namespace")
	}
	if !selection.NamespaceSelected("production") {
		t.Fatal("namespace pattern rejected an allowed namespace")
	}
}

func TestObjectNameFieldSelectorOnlyUsesOneExactPositiveName(t *testing.T) {
	tests := []struct {
		name      string
		resources []string
		want      string
		group     string
		version   string
		resource  string
	}{
		{
			name:      "exact name",
			resources: []string{"apps/v1/deployments:api"},
			want:      "metadata.name=api",
			group:     "apps",
			version:   "v1",
			resource:  "deployments",
		},
		{
			name:      "unrelated resource",
			resources: []string{"core/v1/configmaps:settings"},
			group:     "apps",
			version:   "v1",
			resource:  "deployments",
		},
		{
			name:      "broad resource",
			resources: []string{"apps/v1/deployments"},
			group:     "apps",
			version:   "v1",
			resource:  "deployments",
		},
		{
			name:      "multiple names",
			resources: []string{"apps/v1/deployments:api", "apps/v1/deployments:web"},
			group:     "apps",
			version:   "v1",
			resource:  "deployments",
		},
		{
			name:      "negative only",
			resources: []string{"!apps/v1/deployments:api"},
			group:     "apps",
			version:   "v1",
			resource:  "deployments",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection := Selection{Resources: test.resources}
			if got := selection.ObjectNameFieldSelector(test.group, test.version, test.resource); got != test.want {
				t.Fatalf("ObjectNameFieldSelector() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEffectiveSelectionAppliesProfileObjectScope(t *testing.T) {
	defaults := contract.ResourceSelectionSpec{
		ObjectScope: contract.ObjectScope{
			Namespaces: []string{"*", "!kube-*", "!production-*", "production-api"},
		},
	}
	selection, err := EffectiveSelection(defaults, Selection{})
	if err != nil {
		t.Fatal(err)
	}

	keep := state.Object{
		Identity: state.Identity{
			Version: "v1", Resource: "configmaps",
			Namespace: "production-api", Name: "generated-drop",
		},
		Value: &unstructured.Unstructured{Object: map[string]any{}},
	}
	if !selection.AllowsObject(keep) {
		t.Fatal("later positive namespace pattern did not re-include the object")
	}

	for _, identity := range []state.Identity{
		{Version: "v1", Resource: "configmaps", Namespace: "kube-system", Name: "config"},
	} {
		object := state.Object{Identity: identity, Value: &unstructured.Unstructured{Object: map[string]any{}}}
		if selection.AllowsObject(object) {
			t.Fatalf("negative glob matched excluded object: %#v", identity)
		}
	}
}
