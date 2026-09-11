// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var podListResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// Usage describes current attachment and scheduling information for a PVC.
type Usage struct {
	// UID is the immutable Kubernetes identity of the PVC.
	UID string
	// ResourceVersion identifies the observed PVC version for a point-in-time recheck.
	ResourceVersion string
	// Phase is the current PVC lifecycle phase.
	Phase string
	// VolumeMode is the PVC volume mode, normally Filesystem or Block.
	VolumeMode string
	// AccessModes contains Kubernetes PVC access mode names.
	AccessModes []string
	// PodNames contains Pods currently referencing the PVC.
	PodNames []string
	// Nodes contains distinct nodes used by referencing Pods.
	Nodes []string
	// CapacityBytes is the target PVC capacity from status or requested storage.
	CapacityBytes int64
}

// TargetIdentity identifies the exact PVC observed during restore preflight.
type TargetIdentity struct {
	// UID prevents a deleted and recreated PVC with the same name from being accepted.
	UID string
	// ResourceVersion detects changes between preflight and helper creation.
	ResourceVersion string
}

// Identity returns the Kubernetes identity observed for the PVC.
func (u Usage) Identity() TargetIdentity {
	return TargetIdentity{UID: u.UID, ResourceVersion: u.ResourceVersion}
}

// InUse reports whether at least one Pod currently references the PVC.
func (u Usage) InUse() bool {
	return len(u.PodNames) > 0
}

// HasAccessMode reports whether the PVC declares the supplied access mode.
func (u Usage) HasAccessMode(mode string) bool {
	return slices.Contains(u.AccessModes, mode)
}

// InspectUsage reads PVC access modes and referencing Pods in its namespace.
//
// The result is a snapshot of scheduling state, not a locking guarantee:
// another Pod may be scheduled after this call.
// Strategies use it to avoid known multi-attach hazards while the helper Pod is being created.
func InspectUsage(ctx context.Context, client dynamic.Interface, pvc Ref) (Usage, error) {
	if client == nil {
		return Usage{}, errors.New("PVC usage dynamic client is required")
	}
	if ctx == nil {
		return Usage{}, errors.New("PVC usage context is required")
	}
	if err := pvc.Validate(); err != nil {
		return Usage{}, err
	}

	// Read the claim and Pod references as one best-effort scheduling snapshot.
	// The later restore operation still needs to handle a Pod appearing after this check.
	claim, err := client.
		Resource(pvcResource).
		Namespace(pvc.Namespace).
		Get(ctx, pvc.Name, metav1.GetOptions{})
	if err != nil {
		return Usage{}, fmt.Errorf("get PVC %q: %w", pvc.Name, err)
	}

	accessModes, found, err := unstructured.NestedStringSlice(claim.Object, "spec", "accessModes")
	if err != nil {
		return Usage{}, fmt.Errorf("read PVC %q access modes: %w", pvc.Name, err)
	}
	if !found {
		accessModes = nil
	}

	phase, _, err := unstructured.NestedString(claim.Object, "status", "phase")
	if err != nil {
		return Usage{}, fmt.Errorf("read PVC %q phase: %w", pvc.Name, err)
	}

	volumeMode, _, err := unstructured.NestedString(claim.Object, "spec", "volumeMode")
	if err != nil {
		return Usage{}, fmt.Errorf("read PVC %q volume mode: %w", pvc.Name, err)
	}

	capacity, err := pvcCapacity(claim)
	if err != nil {
		return Usage{}, fmt.Errorf("read PVC %q capacity: %w", pvc.Name, err)
	}

	// Kubernetes has no reverse index from PVC to Pods in the dynamic client,
	// so inspect the namespace's Pods and match their volume claim references.
	list, err := client.
		Resource(podListResource).
		Namespace(pvc.Namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return Usage{}, fmt.Errorf("list Pods for PVC %q: %w", pvc.Name, err)
	}

	usage := Usage{
		UID:             string(claim.GetUID()),
		ResourceVersion: claim.GetResourceVersion(),
		Phase:           phase,
		VolumeMode:      volumeMode,
		CapacityBytes:   capacity,
		AccessModes:     append([]string(nil), accessModes...),
	}

	// Keep Pod names for diagnostics and deduplicate nodes for RWO placement checks.
	nodes := make(map[string]struct{})
	for _, pod := range list.Items {
		if !podReferencesPVC(&pod, pvc.Name) {
			continue
		}
		if podIsTerminal(&pod) {
			continue
		}

		if name, found, _ := unstructured.NestedString(pod.Object, "metadata", "name"); found && name != "" {
			usage.PodNames = append(usage.PodNames, name)
		}
		if node, found, _ := unstructured.NestedString(pod.Object, "spec", "nodeName"); found && node != "" {
			nodes[node] = struct{}{}
		}
	}

	for node := range nodes {
		usage.Nodes = append(usage.Nodes, node)
	}

	sort.Strings(usage.PodNames)
	sort.Strings(usage.Nodes)

	return usage, nil
}

// ValidateRestoreTarget verifies the target PVC before any restore data is written.
// A restore refuses a non-Bound, block-mode, or currently referenced PVC,
// and rejects a payload larger than the target capacity when requiredBytes is set.
// This is a point-in-time safety check, not a Kubernetes locking guarantee.
func ValidateRestoreTarget(ctx context.Context, client dynamic.Interface, target Ref, requiredBytes int64) (TargetIdentity, error) {
	if client == nil {
		return TargetIdentity{}, errors.New("PVC restore target dynamic client is required")
	}
	if ctx == nil {
		return TargetIdentity{}, errors.New("PVC restore target context is required")
	}
	if err := target.Validate(); err != nil {
		return TargetIdentity{}, err
	}
	if requiredBytes < 0 {
		return TargetIdentity{}, errors.New("PVC restore target required size must not be negative")
	}

	usage, err := InspectUsage(ctx, client, target)
	if err != nil {
		return TargetIdentity{}, fmt.Errorf("inspect target PVC %s/%s: %w", target.Namespace, target.Name, err)
	}

	identity := usage.Identity()
	if identity.UID == "" || identity.ResourceVersion == "" {
		return TargetIdentity{}, fmt.Errorf(
			"target PVC %s/%s has incomplete Kubernetes identity",
			target.Namespace,
			target.Name,
		)
	}

	if usage.Phase != "Bound" {
		return TargetIdentity{}, fmt.Errorf(
			"target PVC %s/%s is not Bound: phase is %q",
			target.Namespace,
			target.Name,
			usage.Phase,
		)
	}

	if usage.VolumeMode == "Block" {
		return TargetIdentity{}, fmt.Errorf("target PVC %s/%s uses unsupported Block volume mode", target.Namespace, target.Name)
	}

	if requiredBytes > 0 && usage.CapacityBytes < requiredBytes {
		return TargetIdentity{}, fmt.Errorf(
			"target PVC %s/%s capacity %d bytes is smaller than restore payload %d bytes",
			target.Namespace,
			target.Name,
			usage.CapacityBytes,
			requiredBytes,
		)
	}

	if usage.InUse() {
		const displayedPods = 8
		podNames := usage.PodNames
		if len(podNames) > displayedPods {
			podNames = append(
				append([]string(nil), podNames[:displayedPods]...),
				fmt.Sprintf("... and %d more", len(podNames)-displayedPods),
			)
		}

		return TargetIdentity{}, fmt.Errorf(
			"target PVC %s/%s is currently mounted by Pod(s): %s",
			target.Namespace,
			target.Name,
			strings.Join(podNames, ", "),
		)
	}

	return identity, nil
}

// pvcCapacity returns effective capacity, preferring the bound capacity reported by Kubernetes.
func pvcCapacity(object *unstructured.Unstructured) (int64, error) {
	for _, path := range [][3]string{
		{"status", "capacity", ""},
		{"spec", "resources", "requests"},
	} {
		value, found, err := nestedStorageValue(object, path[0], path[1], path[2])
		if err != nil {
			return 0, err
		}
		if found {
			return ParseSize(value)
		}
	}

	return 0, nil
}

// SelectHelperNode determines whether a helper can safely mount an in-use PVC.
//
// A single RWO consumer node is safe to reuse.
// RWOP and ambiguous consumer placement are rejected
// because a helper on an arbitrary node could remain pending or cause a storage attach conflict.
func SelectHelperNode(usage Usage) (string, error) {
	if usage.HasAccessMode("ReadWriteOncePod") && usage.InUse() {
		return "", errors.New("RWOP PVC is already used by another Pod")
	}

	if !usage.HasAccessMode("ReadWriteOnce") || !usage.InUse() {
		return "", nil
	}

	if len(usage.Nodes) != 1 {
		return "", fmt.Errorf("in-use RWO PVC is attached to %d nodes", len(usage.Nodes))
	}

	return usage.Nodes[0], nil
}

// podIsTerminal reports whether a Pod has completed and can no longer consume the PVC.
// Missing or unknown phases remain active to keep restore safety conservative.
func podIsTerminal(pod *unstructured.Unstructured) bool {
	phase, found, err := unstructured.NestedString(pod.Object, "status", "phase")
	if err != nil || !found {
		return false
	}

	return phase == "Succeeded" || phase == "Failed"
}

// podReferencesPVC reports whether a Pod volume references the named claim.
func podReferencesPVC(pod *unstructured.Unstructured, claimName string) bool {
	volumes, found, err := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	if err != nil || !found {
		return false
	}

	for _, value := range volumes {
		volume, ok := value.(map[string]any)
		if !ok {
			continue
		}

		claim, found, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if found && claim == claimName {
			return true
		}
	}

	return false
}
