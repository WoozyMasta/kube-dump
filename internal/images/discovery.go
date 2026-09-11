// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package images discovers container image references used by Kubernetes Pods.
package images

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"oras.land/oras-go/v2/registry"
)

var podsResource = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

const defaultPodListPageSize int64 = 500

// Platform identifies the operating system and CPU architecture of an image.
// Variant is used for platforms such as linux/arm64/v8.
type Platform struct {
	// OS is the operating system name, for example linux or windows.
	OS string
	// Architecture is the CPU architecture name, for example amd64 or arm64.
	Architecture string
	// Variant is an optional architecture variant.
	Variant string
}

// Reference describes one image use discovered in a Pod.
type Reference struct {
	// Owners contains the Pod's owner chain from the direct owner to its root.
	Owners []Owner
	// PodLabels contains labels from the owning Pod for profile selection.
	PodLabels map[string]string
	// PodAnnotations contains annotations from the owning Pod for profile selection.
	PodAnnotations map[string]string
	// Platform is the best-known platform of the running Pod.
	Platform Platform
	// Source is the image reference declared in the Pod spec.
	Source string
	// Runtime is the image identity reported by the container runtime.
	Runtime string
	// Pull is the exact reference selected for registry access.
	Pull string
	// Namespace is the Pod namespace.
	Namespace string
	// Pod is the name of the Pod using the image.
	Pod string
	// Container is the container name within the Pod.
	Container string
	// PullSecrets contains Pod imagePullSecret names in the Pod namespace.
	PullSecrets []string
	// Init reports whether the reference belongs to an init container.
	Init bool
}

// ReferenceFilter decides whether a discovered Pod reference needs owner resolution.
// Returning an error aborts discovery before any owner API requests are made.
type ReferenceFilter func(Reference) (bool, error)

// Owner describes a Kubernetes object that owns a discovered Pod.
type Owner struct {
	// Labels contains the object's labels.
	Labels map[string]string
	// Annotations contains the object's annotations.
	Annotations map[string]string
	// APIVersion is the object's API version as declared by Kubernetes.
	APIVersion string
	// Kind is the object's Kubernetes kind.
	Kind string
	// Group is the object's API group; it is empty for core resources.
	Group string
	// Version is the object's API version component.
	Version string
	// Resource is the object's plural API resource name.
	Resource string
	// Namespace is the object's namespace.
	Namespace string
	// Name is the object's metadata.name.
	Name string
	// UID is the immutable Kubernetes identity from ownerReferences.
	UID string
}

// ownerResolver resolves owner references while reusing discovery and object lookups.
type ownerResolver struct {
	client      dynamic.Interface
	discovery   discovery.DiscoveryInterface
	resources   map[string]schema.GroupVersionResource
	namespaced  map[string]bool
	objects     map[string]*unstructured.Unstructured
	ownerChains map[string][]Owner
}

// Discover lists selected Pods and returns their normal and init-container image references.
// Container status is matched by name rather than position,
// because Kubernetes does not require spec and status arrays to have equal ordering.
func Discover(ctx context.Context, client dynamic.Interface, namespaces []string) ([]Reference, error) {
	if ctx == nil {
		return nil, errors.New("image discovery context is required")
	}
	if client == nil {
		return nil, errors.New("image discovery client is required")
	}

	namespaces = sortedUnique(namespaces)
	result := make([]Reference, 0)
	process := func(pod *unstructured.Unstructured) error {
		references, err := referencesFromPod(pod)
		if err != nil {
			return err
		}

		result = append(result, references...)
		return nil
	}

	if len(namespaces) == 0 {
		if err := forEachPod(ctx, client, "", process); err != nil {
			return nil, fmt.Errorf("list Pods: %w", err)
		}
	} else {
		for _, namespace := range namespaces {
			if err := forEachPod(ctx, client, namespace, process); err != nil {
				return nil, fmt.Errorf("list Pods in namespace %q: %w", namespace, err)
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return referenceKey(result[i]) < referenceKey(result[j])
	})

	return result, nil
}

// forEachPod reads all Pod pages for one namespace
// and processes each page before requesting the next one,
// keeping API response memory bounded.
func forEachPod(
	ctx context.Context,
	client dynamic.Interface,
	namespace string,
	process func(*unstructured.Unstructured) error,
) error {
	if process == nil {
		return errors.New("pod list callback is required")
	}

	resourceClient := client.Resource(podsResource)
	if namespace != "" {
		return forEachPodPages(ctx, resourceClient.Namespace(namespace), process)
	}

	return forEachPodPages(ctx, resourceClient, process)
}

// forEachPodPages reads all Pod pages from a selected dynamic resource.
func forEachPodPages(
	ctx context.Context,
	resourceClient dynamic.ResourceInterface,
	process func(*unstructured.Unstructured) error,
) error {
	continueToken := ""
	for {
		list, err := resourceClient.List(ctx, metav1.ListOptions{
			Continue: continueToken,
			Limit:    defaultPodListPageSize,
		})
		if err != nil {
			return err
		}

		for index := range list.Items {
			if err := process(&list.Items[index]); err != nil {
				return err
			}
		}

		continueToken = list.GetContinue()
		if continueToken == "" {
			return nil
		}
	}
}

// DiscoverWithOwners discovers image references and resolves their Pod owner chains.
// Owner objects are fetched only for this explicit owner-aware path.
func DiscoverWithOwners(
	ctx context.Context,
	client dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	namespaces []string,
	filter ReferenceFilter,
) ([]Reference, error) {
	if discoveryClient == nil {
		return nil, errors.New("image discovery Kubernetes discovery client is required")
	}

	result, err := Discover(ctx, client, namespaces)
	if err != nil {
		return nil, err
	}

	if filter != nil {
		// Apply Pod-level predicates before resolving owners.
		// Owner resolution is API-heavy, so this preserves the cheap-first filtering order.
		filtered := result[:0]
		for _, reference := range result {
			selected, filterErr := filter(reference)
			if filterErr != nil {
				return nil, fmt.Errorf("filter image reference %q: %w", reference.Source, filterErr)
			}

			if selected {
				filtered = append(filtered, reference)
			}
		}

		result = filtered
	}

	resolver := ownerResolver{
		client:      client,
		discovery:   discoveryClient,
		resources:   make(map[string]schema.GroupVersionResource),
		namespaced:  make(map[string]bool),
		objects:     make(map[string]*unstructured.Unstructured),
		ownerChains: make(map[string][]Owner),
	}

	// Resolve each remaining Pod independently
	// while the resolver caches shared owner objects and chains across references.
	for index := range result {
		owners, err := resolver.resolve(ctx, result[index].Namespace, result[index].Owners)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve owners for Pod %s/%s: %w",
				result[index].Namespace,
				result[index].Pod,
				err,
			)
		}

		result[index].Owners = owners
	}

	return result, nil
}

// resolve expands direct Pod owners into a deterministic owner chain.
func (r *ownerResolver) resolve(
	ctx context.Context,
	namespace string,
	owners []Owner,
) ([]Owner, error) {
	result := make([]Owner, 0, len(owners))
	for _, owner := range owners {
		chain, err := r.resolveOwner(ctx, namespace, owner, 0, make(map[string]struct{}))
		if err != nil {
			return nil, err
		}

		result = append(result, chain...)
	}

	return result, nil
}

// resolveOwner fetches one owner object and recursively resolves its owners.
func (r *ownerResolver) resolveOwner(
	ctx context.Context,
	namespace string,
	owner Owner,
	depth int,
	visiting map[string]struct{},
) ([]Owner, error) {
	if owner.APIVersion == "" || owner.Kind == "" || owner.Name == "" {
		return nil, fmt.Errorf("owner reference is incomplete: %#v", owner)
	}
	if depth > 16 {
		return nil, fmt.Errorf("owner chain exceeds 16 objects at %s/%s", owner.Kind, owner.Name)
	}

	resource, namespaced, err := r.resourceFor(owner.APIVersion, owner.Kind)
	if err != nil {
		return nil, err
	}

	lookupNamespace := namespace
	if !namespaced {
		lookupNamespace = ""
	}

	key := resource.String() + "/" + lookupNamespace + "/" + owner.Name + "/" + owner.UID
	if _, exists := visiting[key]; exists {
		return nil, fmt.Errorf("owner chain contains a cycle at %s/%s", owner.Kind, owner.Name)
	}
	if chain, exists := r.ownerChains[key]; exists {
		return append([]Owner(nil), chain...), nil
	}

	object, exists := r.objects[key]
	if !exists {
		resourceClient := r.client.Resource(resource)
		var getter dynamic.ResourceInterface = resourceClient
		if namespaced {
			getter = resourceClient.Namespace(namespace)
		}

		object, err = getter.Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get owner %s/%s: %w", owner.Kind, owner.Name, err)
		}

		if owner.UID != "" && string(object.GetUID()) != owner.UID {
			return nil, fmt.Errorf(
				"owner %s/%s UID is %q, want %q",
				owner.Kind, owner.Name, object.GetUID(), owner.UID,
			)
		}

		r.objects[key] = object
	}

	groupVersion, err := schema.ParseGroupVersion(object.GetAPIVersion())
	if err != nil {
		return nil, fmt.Errorf("parse owner %s/%s apiVersion: %w", owner.Kind, owner.Name, err)
	}

	resolved := Owner{
		APIVersion:  object.GetAPIVersion(),
		Kind:        object.GetKind(),
		Group:       groupVersion.Group,
		Version:     groupVersion.Version,
		Resource:    resource.Resource,
		Namespace:   object.GetNamespace(),
		Name:        object.GetName(),
		UID:         string(object.GetUID()),
		Labels:      maps.Clone(object.GetLabels()),
		Annotations: maps.Clone(object.GetAnnotations()),
	}
	visiting[key] = struct{}{}
	chain := []Owner{resolved}

	for _, reference := range object.GetOwnerReferences() {
		parent := ownerFromReference(reference, object.GetNamespace())
		parents, err := r.resolveOwner(ctx, object.GetNamespace(), parent, depth+1, visiting)
		if err != nil {
			return nil, err
		}

		chain = append(chain, parents...)
	}
	delete(visiting, key)
	r.ownerChains[key] = append([]Owner(nil), chain...)

	return chain, nil
}

// resourceFor maps an owner apiVersion/kind pair to its plural API resource.
func (r *ownerResolver) resourceFor(apiVersion, kind string) (schema.GroupVersionResource, bool, error) {
	key := apiVersion + "\x00" + kind
	if resource, exists := r.resources[key]; exists {
		return resource, r.namespaced[key], nil
	}

	list, err := r.discovery.ServerResourcesForGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf(
			"discover resource for %s/%s: %w", apiVersion, kind, err,
		)
	}

	for _, apiResource := range list.APIResources {
		if apiResource.Kind != kind || strings.ContainsRune(apiResource.Name, '/') {
			continue
		}

		groupVersion, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return schema.GroupVersionResource{}, false, fmt.Errorf("parse owner apiVersion %q: %w", apiVersion, err)
		}

		resource := groupVersion.WithResource(apiResource.Name)
		r.resources[key] = resource
		r.namespaced[key] = apiResource.Namespaced
		return resource, apiResource.Namespaced, nil
	}

	return schema.GroupVersionResource{}, false, fmt.Errorf(
		"resource for %s/%s was not found in Kubernetes discovery", apiVersion, kind,
	)
}

// referencesFromPod extracts both desired and runtime image identities from a Pod.
func referencesFromPod(pod *unstructured.Unstructured) ([]Reference, error) {
	if pod == nil {
		return nil, errors.New("image discovery received a nil Pod")
	}

	status := statusByName(pod, "status", "containerStatuses")
	initStatus := statusByName(pod, "status", "initContainerStatuses")
	platform := Platform{}

	if nodeName, found, err := unstructured.NestedString(pod.Object, "spec", "nodeName"); err != nil {
		return nil, fmt.Errorf("read Pod node name: %w", err)
	} else if found && nodeName != "" {
		// Node lookup is intentionally deferred to the capture layer. Discovery
		// remains useful with a fake client and does not require node privileges.
		platform = Platform{}
	}

	pullSecrets := podPullSecrets(pod)
	podLabels := pod.GetLabels()
	podAnnotations := pod.GetAnnotations()
	owners := make([]Owner, 0, len(pod.GetOwnerReferences()))
	for _, reference := range pod.GetOwnerReferences() {
		owners = append(owners, ownerFromReference(reference, pod.GetNamespace()))
	}

	result := make([]Reference, 0)
	for _, field := range []struct {
		name   string
		init   bool
		status map[string]map[string]any
	}{
		{
			name:   "containers",
			status: status,
		},
		{
			name:   "initContainers",
			init:   true,
			status: initStatus,
		},
	} {
		values, found, err := unstructured.NestedSlice(pod.Object, "spec", field.name)
		if err != nil {
			return nil, fmt.Errorf("read Pod %s: %w", field.name, err)
		}

		if !found {
			continue
		}

		for _, value := range values {
			container, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("pod %s contains an invalid container entry", field.name)
			}

			name, _, _ := unstructured.NestedString(container, "name")
			image, _, err := unstructured.NestedString(container, "image")
			if err != nil {
				return nil, fmt.Errorf("read image for container %q: %w", name, err)
			}

			if image == "" {
				continue
			}

			runtime := ""
			if current, ok := field.status[name]; ok {
				runtime, _, _ = unstructured.NestedString(current, "imageID")
			}

			pull := image
			if normalized, ok := normalizeRuntimeReference(runtime); ok {
				pull = normalized
			}

			result = append(result, Reference{
				Owners:         append([]Owner(nil), owners...),
				Source:         image,
				Runtime:        runtime,
				Pull:           pull,
				Platform:       platform,
				Namespace:      pod.GetNamespace(),
				Pod:            pod.GetName(),
				Container:      name,
				Init:           field.init,
				PullSecrets:    pullSecrets,
				PodLabels:      maps.Clone(podLabels),
				PodAnnotations: maps.Clone(podAnnotations),
			})
		}
	}

	return result, nil
}

// ownerFromReference converts Kubernetes owner metadata into a resolver input.
func ownerFromReference(reference metav1.OwnerReference, namespace string) Owner {
	return Owner{
		APIVersion: reference.APIVersion,
		Kind:       reference.Kind,
		Namespace:  namespace,
		Name:       reference.Name,
		UID:        string(reference.UID),
	}
}

// podPullSecrets returns the names of credentials explicitly attached to a Pod.
func podPullSecrets(pod *unstructured.Unstructured) []string {
	values, found, err := unstructured.NestedSlice(pod.Object, "spec", "imagePullSecrets")
	if err != nil || !found {
		return nil
	}

	result := make([]string, 0, len(values))
	for _, value := range values {
		secret, ok := value.(map[string]any)
		if !ok {
			continue
		}

		name, found, _ := unstructured.NestedString(secret, "name")
		if found && name != "" {
			result = append(result, name)
		}
	}

	return result
}

// statusByName indexes container statuses by name for order-independent matching.
func statusByName(pod *unstructured.Unstructured, parent, field string) map[string]map[string]any {
	result := make(map[string]map[string]any)
	values, found, err := unstructured.NestedSlice(pod.Object, parent, field)
	if err != nil || !found {
		return result
	}

	for _, value := range values {
		status, ok := value.(map[string]any)
		if !ok {
			continue
		}

		name, found, _ := unstructured.NestedString(status, "name")
		if found && name != "" {
			result[name] = status
		}
	}

	return result
}

// normalizeRuntimeReference removes CRI transport prefixes from imageID values.
func normalizeRuntimeReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"docker-pullable://", "docker://", "containerd://", "cri-o://"} {
		value = strings.TrimPrefix(value, prefix)
	}
	if value == "" {
		return "", false
	}

	if _, err := registry.ParseReference(value); err != nil {
		return "", false
	}
	if strings.Contains(value, "@sha256:") {
		return value, true
	}

	return "", false
}

// referenceKey provides deterministic ordering without exposing runtime details.
func referenceKey(reference Reference) string {
	return strings.Join([]string{
		reference.Namespace,
		reference.Pod,
		reference.Container,
		reference.Source,
	}, "\x00")
}

// sortedUnique removes repeated namespace arguments while preserving deterministic order.
func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	result = slicesUnique(result)

	return result
}

// slicesUnique removes adjacent duplicates from an already sorted slice
// and reuses the input backing array for the compacted result.
func slicesUnique(values []string) []string {
	if len(values) < 2 {
		return values
	}

	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}

		values[write] = values[read]
		write++
	}

	return values[:write]
}
