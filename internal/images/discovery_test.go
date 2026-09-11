package images

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryapi "k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic/fake"
	clientgotesting "k8s.io/client-go/testing"
)

func TestDiscoverMatchesStatusesByContainerName(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name": "api-0", "namespace": "production",
		},
		"spec": map[string]interface{}{
			"imagePullSecrets": []interface{}{map[string]interface{}{"name": "regcred"}},
			"containers": []interface{}{
				map[string]interface{}{"name": "api", "image": "ghcr.io/acme/api:pr-1"},
				map[string]interface{}{"name": "sidecar", "image": "ghcr.io/acme/sidecar:v2"},
			},
			"initContainers": []interface{}{
				map[string]interface{}{"name": "migration", "image": "ghcr.io/acme/migrate:v1"},
			},
		},
		"status": map[string]interface{}{
			"containerStatuses": []interface{}{
				map[string]interface{}{
					"name":    "sidecar",
					"imageID": "docker-pullable://ghcr.io/acme/sidecar@sha256:" + digest('b'),
				},
				map[string]interface{}{
					"name":    "api",
					"imageID": "docker-pullable://ghcr.io/acme/api@sha256:" + digest('a'),
				},
			},
			"initContainerStatuses": []interface{}{
				map[string]interface{}{
					"name":    "migration",
					"imageID": "containerd://ghcr.io/acme/migrate@sha256:" + digest('c'),
				},
			},
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod)
	result, err := Discover(context.Background(), client, []string{"production"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 {
		t.Fatalf("discovered references = %d, want 3", len(result))
	}

	for _, reference := range result {
		if reference.Pull == reference.Source && reference.Runtime != "" {
			t.Errorf("runtime digest was not selected for %s", reference.Container)
		}
		if len(reference.PullSecrets) != 1 || reference.PullSecrets[0] != "regcred" {
			t.Errorf("pull secrets = %#v", reference.PullSecrets)
		}
	}

	if result[0].Container != "api" || result[1].Container != "migration" || result[2].Container != "sidecar" {
		t.Fatalf("references are not deterministic: %#v", result)
	}
}

func TestDiscoverFiltersNamespaces(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": "api", "namespace": "other"},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{"name": "api", "image": "nginx:latest"}},
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod)
	result, err := Discover(context.Background(), client, []string{"production"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("discovered references = %d, want 0", len(result))
	}
}

func TestForEachPodProcessesPaginatedPages(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(testScheme())
	page := 0
	client.PrependReactor("list", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		page++
		if page == 1 {
			list := &unstructured.UnstructuredList{
				Items: []unstructured.Unstructured{{
					Object: map[string]any{
						"metadata": map[string]any{"name": "first"},
					}},
				},
			}
			list.SetContinue("next")
			return true, list, nil
		}

		return true, &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{
				{Object: map[string]any{
					"metadata": map[string]any{"name": "second"},
				}},
			},
		}, nil
	})

	var names []string
	err := forEachPod(context.Background(), client, "production", func(pod *unstructured.Unstructured) error {
		names = append(names, pod.GetName())
		return nil
	})
	if err != nil {
		t.Fatalf("forEachPod() error = %v", err)
	}
	if len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatalf("forEachPod() names = %#v, want first and second", names)
	}
}

func TestDiscoverWithOwnersResolvesOwnerChain(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{
			"name": "api-abc", "namespace": "production",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "api-abc",
			}},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name": "api", "image": "ghcr.io/acme/api:v1",
			}},
		},
	}}
	replicaSet := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"metadata": map[string]interface{}{
			"name": "api-abc", "namespace": "production",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "apps/v1", "kind": "Deployment", "name": "api",
			}},
		},
	}}
	deployment := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{
			"name": "api", "namespace": "production",
			"labels": map[string]interface{}{"team": "platform"},
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod, replicaSet, deployment)
	result, err := DiscoverWithOwners(
		context.Background(), client, &testDiscovery{}, []string{"production"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result[0].Owners) != 2 {
		t.Fatalf("resolved owners = %#v", result)
	}
	if result[0].Owners[0].Resource != "replicasets" || result[0].Owners[1].Resource != "deployments" {
		t.Fatalf("resolved owner resources = %#v", result[0].Owners)
	}
	if result[0].Owners[1].Labels["team"] != "platform" {
		t.Fatalf("resolved deployment labels = %#v", result[0].Owners[1].Labels)
	}
}

func TestDiscoverWithOwnersFiltersBeforeResolvingOwners(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "api",
			"namespace": "production",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       "api",
			}},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "api",
				"image": "ghcr.io/acme/api:v1",
			}},
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod)
	result, err := DiscoverWithOwners(
		context.Background(), client, &failingOwnerDiscovery{}, []string{"production"},
		func(Reference) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("filtered references = %#v, want none", result)
	}
}

func TestDiscoverWithOwnersUsesClusterScopeAndOwnerUID(t *testing.T) {
	t.Parallel()

	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "api",
			"namespace": "production",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"name":       "production",
				"uid":        "namespace-uid",
			}},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "api",
				"image": "ghcr.io/acme/api:v1",
			}},
		},
	}}
	namespace := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": "production",
			"uid":  "namespace-uid",
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod, namespace)
	client.PrependReactor("get", "namespaces", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.(clientgotesting.GetAction).GetNamespace() != "" {
			return true, nil, errors.New("cluster-scoped owner was requested with a namespace")
		}
		return false, nil, nil
	})

	result, err := DiscoverWithOwners(
		context.Background(), client, &testDiscovery{}, []string{"production"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result[0].Owners) != 1 {
		t.Fatalf("resolved owners = %#v", result)
	}
	if result[0].Owners[0].UID != "namespace-uid" {
		t.Fatalf("owner UID = %q, want namespace-uid", result[0].Owners[0].UID)
	}
}

func TestDiscoverWithOwnersRejectsRecreatedOwner(t *testing.T) {
	t.Parallel()

	pod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "api",
			"namespace": "production",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       "api",
				"uid":        "original",
			}},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "api",
				"image": "ghcr.io/acme/api:v1",
			}},
		},
	}}
	deployment := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "api",
			"namespace": "production",
			"uid":       "recreated",
		},
	}}

	client := fake.NewSimpleDynamicClient(testScheme(), pod, deployment)
	_, err := DiscoverWithOwners(
		context.Background(), client, &testDiscovery{}, []string{"production"}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "UID") {
		t.Fatalf("recreated owner error = %v, want UID mismatch", err)
	}
}

// failingOwnerDiscovery fails if owner resolution unexpectedly reaches discovery.
type failingOwnerDiscovery struct {
	discoveryapi.DiscoveryInterface
}

// ServerResourcesForGroupVersion reports an unexpected owner discovery request.
func (*failingOwnerDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return nil, errors.New("owner discovery should not be called for filtered references")
}

// digest returns a deterministic hexadecimal-looking test value of digest length.
func digest(char byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = char
	}

	return string(result)
}

// testScheme creates the minimal dynamic scheme required by discovery tests.
func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "PodList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "Namespace"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"},
		&unstructured.UnstructuredList{},
	)
	for _, kind := range []string{"ReplicaSet", "Deployment"} {
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind},
			&unstructured.Unstructured{},
		)
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind + "List"},
			&unstructured.UnstructuredList{},
		)
	}

	return scheme
}

// testDiscovery provides the API resources required by the owner resolver.
type testDiscovery struct {
	discoveryapi.DiscoveryInterface
}

// ServerResourcesForGroupVersion returns the plural resources used by the fixture owners.
func (*testDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	if groupVersion == "v1" {
		return &metav1.APIResourceList{
			GroupVersion: groupVersion,
			APIResources: []metav1.APIResource{{Name: "namespaces", Kind: "Namespace"}},
		}, nil
	}

	return &metav1.APIResourceList{
		GroupVersion: groupVersion,
		APIResources: []metav1.APIResource{
			{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true},
			{Name: "deployments", Kind: "Deployment", Namespaced: true},
		},
	}, nil
}
