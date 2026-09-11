package kube

import (
	"context"
	"errors"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgotesting "k8s.io/client-go/testing"
)

func TestCollectionResultBoundsWarningDetails(t *testing.T) {
	var result CollectionResult
	for range maxCollectionWarningDetails + 4 {
		result.addWarning(errors.New("collection warning"))
	}

	if result.WarningTotal() != maxCollectionWarningDetails+4 {
		t.Fatalf("WarningTotal() = %d, want %d", result.WarningTotal(), maxCollectionWarningDetails+4)
	}
	if len(result.Warnings) != maxCollectionWarningDetails {
		t.Fatalf("len(Warnings) = %d, want %d", len(result.Warnings), maxCollectionWarningDetails)
	}
	if result.OmittedWarningDetails() != 4 {
		t.Fatalf("OmittedWarningDetails() = %d, want 4", result.OmittedWarningDetails())
	}
}

func TestSelectionAllowsCanonicalResource(t *testing.T) {
	t.Parallel()

	selection := Selection{
		Resources: []string{"apps/v1/deployments"},
	}

	resource := metav1.APIResource{
		Name:       "deployments",
		Namespaced: true,
		Verbs:      []string{"get", "list"},
	}
	if !selection.Allows(resource, "apps", "v1") {
		t.Fatal("Selection.Allows() rejected canonical GVR")
	}

	resource.Verbs = []string{"get"}
	if selection.Allows(resource, "apps", "v1") {
		t.Fatal("Selection.Allows() accepted a resource without list verb")
	}
}

func TestSelectionAllowsShortCoreResourceExpression(t *testing.T) {
	t.Parallel()

	selection := Selection{Resources: []string{"v1/configmaps"}}
	resource := metav1.APIResource{
		Name:  "configmaps",
		Verbs: []string{"list"},
	}
	if !selection.Allows(resource, "", "v1") {
		t.Fatal("Selection.Allows() rejected short core resource expression")
	}

	identity := state.Identity{Version: "v1", Resource: "configmaps", Name: "settings"}
	if !selection.AllowsIdentity(identity) {
		t.Fatal("Selection.AllowsIdentity() rejected short core resource expression")
	}
}

func TestSelectionAllowsShortCoreResource(t *testing.T) {
	t.Parallel()

	selection := Selection{Resources: []string{"v1/configmaps"}}
	resource := metav1.APIResource{
		Name:  "configmaps",
		Verbs: []string{"list"},
	}
	if !selection.Allows(resource, "", "v1") {
		t.Fatal("Selection.Allows() rejected short core GVR")
	}
	if !selection.AllowsIdentity(state.Identity{
		Version:  "v1",
		Resource: "configmaps",
		Name:     "settings",
	}) {
		t.Fatal("Selection.AllowsIdentity() rejected short core GVR")
	}
}

func TestSelectionRejectsNonCanonicalQualifiedResource(t *testing.T) {
	t.Parallel()

	resource := metav1.APIResource{
		Name:  "deployments",
		Verbs: []string{"list"},
	}
	for _, expression := range []string{"apps/deployments", "v1/deployments", "deployments.apps"} {
		if (Selection{Resources: []string{expression}}).Allows(resource, "apps", "v1") {
			t.Fatalf("Selection.Allows() accepted non-canonical expression %q", expression)
		}
	}
}

func TestResolveResourceAliases(t *testing.T) {
	t.Parallel()

	selection, err := resolveResourceAliases(Selection{
		Resources: []string{"deploy:api", "!svc"},
	}, []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "services", ShortNames: []string{"svc"}, Verbs: []string{"list"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", ShortNames: []string{"deploy"}, Verbs: []string{"list"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveResourceAliases() error = %v", err)
	}

	want := []string{"apps/v1/deployments:api", "!core/v1/services"}
	if len(selection.Resources) != len(want) {
		t.Fatalf("resolved resources = %#v, want %#v", selection.Resources, want)
	}
	for index := range want {
		if selection.Resources[index] != want[index] {
			t.Fatalf("resolved resources = %#v, want %#v", selection.Resources, want)
		}
	}
}

func TestResolveResourceAliasesRejectsUsedAmbiguousAlias(t *testing.T) {
	t.Parallel()

	_, err := resolveResourceAliases(Selection{Resources: []string{"thing"}}, []*metav1.APIResourceList{
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", ShortNames: []string{"thing"}, Verbs: []string{"list"}},
			},
		},
		{
			GroupVersion: "batch/v1",
			APIResources: []metav1.APIResource{
				{Name: "jobs", ShortNames: []string{"thing"}, Verbs: []string{"list"}},
			},
		},
	})
	if err == nil {
		t.Fatal("resolveResourceAliases() accepted an ambiguous alias")
	}
}

func TestResolveResourceAliasesPrefersExactPlural(t *testing.T) {
	t.Parallel()

	selection, err := resolveResourceAliases(Selection{Resources: []string{"svc"}}, []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "services",
					ShortNames: []string{"svc"},
					Verbs:      []string{"list"},
				},
			},
		},
		{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{
					Name:  "svc",
					Verbs: []string{"list"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveResourceAliases() error = %v", err)
	}
	if len(selection.Resources) != 1 || selection.Resources[0] != "svc" {
		t.Fatalf("resolved resources = %#v, want svc", selection.Resources)
	}
}

func TestSelectionExcludesNamespacesAndResources(t *testing.T) {
	t.Parallel()

	selection := Selection{
		Resources:         []string{"*", "!secrets"},
		ExcludeNamespaces: []string{"kube-system"},
	}
	if selection.Allows(metav1.APIResource{
		Name:       "secrets",
		Namespaced: true,
		Verbs:      []string{"list"},
	}, "", "v1") {
		t.Fatal("Selection.Allows() accepted an excluded resource")
	}
	if selection.NamespaceSelected("kube-system") {
		t.Fatal("Selection.NamespaceSelected() accepted an excluded namespace")
	}
}

func TestSelectionIncludeResourceOverridesProfileAllowlist(t *testing.T) {
	selection := Selection{Resources: []string{"metrics.k8s.io/v1beta1/pods"}}
	resource := metav1.APIResource{
		Name:       "pods",
		Namespaced: true,
		Verbs:      []string{"list"},
	}

	if !selection.Allows(resource, "metrics.k8s.io", "v1beta1") {
		t.Fatal("explicit resource selection rejected a selected resource")
	}
}

func TestBuildJobsIsDeterministic(t *testing.T) {
	t.Parallel()

	resources := []discoveredResource{
		{
			Group:   "apps",
			Version: "v1",
			API:     metav1.APIResource{Name: "deployments", Namespaced: true},
		},
		{Group: "",
			Version: "v1",
			API:     metav1.APIResource{Name: "nodes", Namespaced: false},
		},
	}

	jobs := buildJobs(resources, []string{"default", "production"}, false)
	if len(jobs) != 3 {
		t.Fatalf("buildJobs() returned %d jobs, want 3", len(jobs))
	}
	if jobs[0].Namespace != "default" || jobs[1].Namespace != "production" || jobs[2].Namespace != "" {
		t.Fatalf("buildJobs() order = %#v", jobs)
	}
}

func TestBuildJobsWithNamespacesSkipsClusterResources(t *testing.T) {
	t.Parallel()

	resources := []discoveredResource{
		{
			Group:   "",
			Version: "v1",
			API:     metav1.APIResource{Name: "namespaces", Namespaced: false},
		},
		{
			Group:   "apps",
			Version: "v1",
			API:     metav1.APIResource{Name: "deployments", Namespaced: true},
		},
	}

	jobs := buildJobs(resources, []string{"status"}, true)
	if len(jobs) != 1 || jobs[0].Resource.API.Name != "deployments" {
		t.Fatalf("buildJobs() with namespace filter = %#v, want only deployments", jobs)
	}
}

func TestSelectionAllowsIdentityMirrorsFilters(t *testing.T) {
	t.Parallel()

	selection := Selection{
		Resources:  []string{"apps/v1/deployments"},
		Namespaces: []string{"default"},
	}

	if !selection.AllowsIdentity(state.Identity{
		Group:     "apps",
		Version:   "v1",
		Resource:  "deployments",
		Namespace: "default",
		Name:      "api",
	}) {
		t.Fatal("Selection.AllowsIdentity() rejected selected object")
	}

	if selection.AllowsIdentity(state.Identity{
		Group:     "apps",
		Resource:  "deployments",
		Namespace: "other",
		Name:      "api",
	}) {
		t.Fatal("Selection.AllowsIdentity() accepted an excluded namespace")
	}

	if selection.AllowsIdentity(state.Identity{
		Group:    "",
		Resource: "nodes",
		Name:     "node",
	}) {
		t.Fatal("Selection.AllowsIdentity() accepted a cluster object without cluster scope")
	}
}

func TestCollectorListEachProcessesPaginatedObjects(t *testing.T) {
	t.Parallel()

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{
				Version:  "v1",
				Resource: "configmaps",
			}: "ConfigMapList",
		},
	)
	page := 0
	client.PrependReactor("list", "configmaps", func(clientgotesting.Action) (bool, runtime.Object, error) {
		page++
		if page == 1 {
			list := &unstructured.UnstructuredList{}
			list.SetContinue("next")
			list.Items = []unstructured.Unstructured{{Object: map[string]any{
				"metadata": map[string]any{"name": "first"},
			}}}
			return true, list, nil
		}

		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: map[string]any{
			"metadata": map[string]any{"name": "second"},
		}}}}, nil
	})

	collector := Collector{
		Dynamic: client,
		Collection: CollectionOptions{
			PageSize: 1,
		},
	}
	resource := discoveredResource{
		Version: "v1",
		API: metav1.APIResource{
			Name:       "configmaps",
			Namespaced: true,
		},
	}
	var names []string
	listed, err := collector.listEach(
		context.Background(), resource, "default", "", "",
		func(object unstructured.Unstructured) error {
			names = append(names, object.GetName())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("listEach() error = %v", err)
	}
	if listed != 2 {
		t.Fatalf("listEach() listed = %d, want 2", listed)
	}
	if len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatalf("listEach() names = %#v, want first and second", names)
	}
}
