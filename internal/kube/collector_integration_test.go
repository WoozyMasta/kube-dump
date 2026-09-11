package kube

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestCollectorWritesNamespacedAndClusterResources(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(&runtime.Scheme{},
		unstructuredObject("v1", "Namespace", "", "default", nil),
		unstructuredObject(
			"v1",
			"ConfigMap",
			"default",
			"settings",
			map[string]any{
				"data":     map[string]any{"key": "value"},
				"metadata": map[string]any{"uid": "generated"},
			}),
	)

	resources := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{
				Name:  "namespaces",
				Kind:  "Namespace",
				Verbs: []string{"list"},
			},
			{
				Name:       "configmaps",
				Kind:       "ConfigMap",
				Namespaced: true,
				Verbs:      []string{"list"},
			},
		}},
	}

	discoveryClient := &staticDiscovery{
		DiscoveryInterface: &discoveryfake.FakeDiscovery{},
		resources:          resources,
	}

	root := t.TempDir()
	directory, err := export.NewDirectorySink(root)
	if err != nil {
		t.Fatalf("store.NewDirectory() error = %v", err)
	}

	normalizationProfile, err := profile.Load("backup")
	if err != nil {
		t.Fatalf("profile.Load() error = %v", err)
	}

	collector := Collector{
		Dynamic:    dynamicClient,
		Discovery:  discoveryClient,
		Sink:       directory,
		Profile:    normalizationProfile,
		Selection:  Selection{},
		Collection: CollectionOptions{Concurrency: 2},
	}
	observer := &collectionObserverRecorder{}
	collector.Observer = observer

	result, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collector.Collect() error = %v", err)
	}
	if result.Collected != 2 {
		t.Fatalf("Collector.Collect() collected %d objects, want 2", result.Collected)
	}
	if result.Listed != 2 || result.Selected != 2 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf(
			"Collector.Collect() counters = listed:%d selected:%d skipped:%d failed:%d",
			result.Listed, result.Selected, result.Skipped, result.Failed)
	}
	if result.Changed != 2 {
		t.Fatalf("Collector.Collect() changed %d objects, want 2", result.Changed)
	}
	if got := observer.count("plan"); got != 1 {
		t.Fatalf("collection observer plans = %d, want 1", got)
	}
	if got := observer.count("list"); got != 2 {
		t.Fatalf("collection observer lists = %d, want 2", got)
	}
	if got := observer.count("object"); got != 2 {
		t.Fatalf("collection observer objects = %d, want 2", got)
	}
	if got := observer.count("job"); got != 2 {
		t.Fatalf("collection observer jobs = %d, want 2", got)
	}

	data, err := os.ReadFile(filepath.Join(root, "core", "v1", "configmaps", "default", "settings.yaml"))
	if err != nil {
		t.Fatalf("read exported object: %v", err)
	}
	if bytes.Contains(data, []byte("uid:")) {
		t.Fatal("portable collector output still contains metadata.uid")
	}
}

func TestCollectorDiscoversNamespacesOutsideResourceAllowlist(t *testing.T) {
	t.Parallel()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(&runtime.Scheme{},
		unstructuredObject("v1", "Namespace", "", "production", nil),
	)
	discoveryClient := &staticDiscovery{
		DiscoveryInterface: &discoveryfake.FakeDiscovery{},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{
					{
						Name:  "namespaces",
						Kind:  "Namespace",
						Verbs: []string{"list"},
					},
				}},
			{
				GroupVersion: "apps/v1",
				APIResources: []metav1.APIResource{
					{
						Name:       "deployments",
						Kind:       "Deployment",
						Namespaced: true,
						Verbs:      []string{"list"},
					},
				}},
		},
	}

	collector := Collector{
		Dynamic:   dynamicClient,
		Discovery: discoveryClient,
		Selection: Selection{Resources: []string{"apps/v1/deployments"}},
	}

	resources, err := collector.discoverResources()
	if err != nil {
		t.Fatalf("Collector.discoverResources() error = %v", err)
	}
	if len(resources) != 1 || resources[0].API.Name != "deployments" {
		t.Fatalf("discovered resources = %#v, want deployments only", resources)
	}

	namespaces, err := collector.namespaces(context.Background(), resources)
	if err != nil {
		t.Fatalf("Collector.namespaces() error = %v", err)
	}
	if len(namespaces) != 1 || namespaces[0] != "production" {
		t.Fatalf("resolved namespaces = %#v, want production", namespaces)
	}
}

func TestCollectorRejectsSelectionWithoutCollectionJobs(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(&runtime.Scheme{})
	discoveryClient := &staticDiscovery{
		DiscoveryInterface: &discoveryfake.FakeDiscovery{},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{{
				Name:  "namespaces",
				Kind:  "Namespace",
				Verbs: []string{"list"},
			}},
		}},
	}
	directory, err := export.NewDirectorySink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	normalizationProfile, err := profile.Load("backup")
	if err != nil {
		t.Fatal(err)
	}

	collector := Collector{
		Dynamic:   dynamicClient,
		Discovery: discoveryClient,
		Sink:      directory,
		Profile:   normalizationProfile,
		Selection: Selection{
			Resources:  []string{"missing.example/v1/widgets"},
			Namespaces: []string{"default"},
		},
		Collection: CollectionOptions{Concurrency: 1},
	}
	if _, err := collector.Collect(context.Background()); err == nil {
		t.Fatal("Collector.Collect() accepted a selection without collection jobs")
	}
}

// collectionObserverRecorder verifies that optional progress events
// do not depend on worker completion order or require a single worker.
type collectionObserverRecorder struct {
	mutex  sync.Mutex
	events []string
}

// Plan records the collection plan event.
func (r *collectionObserverRecorder) Plan(CollectionPlan) {
	r.record("plan")
}

// List records one list result event.
func (r *collectionObserverRecorder) List(CollectionListEvent) {
	r.record("list")
}

// Object records one object outcome event.
func (r *collectionObserverRecorder) Object(CollectionObjectEvent) {
	r.record("object")
}

// JobFinished records one job completion event.
func (r *collectionObserverRecorder) JobFinished(CollectionJobEvent) {
	r.record("job")
}

// record appends an event under the recorder lock.
func (r *collectionObserverRecorder) record(kind string) {
	r.mutex.Lock()
	r.events = append(r.events, kind)
	r.mutex.Unlock()
}

// count returns the number of recorded events of one kind.
func (r *collectionObserverRecorder) count(kind string) int {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	count := 0
	for _, event := range r.events {
		if event == kind {
			count++
		}
	}

	return count
}

// staticDiscovery supplies preferred resources omitted by client-go's fake
// while delegating the remaining discovery interface methods.
type staticDiscovery struct {
	// DiscoveryInterface supplies the remaining discovery methods.
	discovery.DiscoveryInterface
	// resources are deterministic discovery fixtures used by the test.
	resources []*metav1.APIResourceList
}

// ServerPreferredResources returns deterministic discovery fixtures.
func (d *staticDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return d.resources, nil
}

// unstructuredObject builds test data with the metadata required by state.NewObject.
func unstructuredObject(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	object := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}
	if namespace != "" {
		object["metadata"].(map[string]any)["namespace"] = namespace
	}

	for key, value := range fields {
		if key == "metadata" {
			for metadataKey, metadataValue := range value.(map[string]any) {
				object["metadata"].(map[string]any)[metadataKey] = metadataValue
			}
			continue
		}
		object[key] = value
	}

	return &unstructured.Unstructured{Object: object}
}
