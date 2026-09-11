// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

const (
	snapshotGroupVersion = "snapshot.storage.k8s.io/v1"
	snapshotGroup        = "snapshot.storage.k8s.io"
	snapshotVersion      = "v1"
	defaultSnapshotWait  = 30 * time.Minute
)

var (
	volumeSnapshotResource = schema.GroupVersionResource{
		Group:    snapshotGroup,
		Version:  snapshotVersion,
		Resource: "volumesnapshots",
	}
)

// SnapshotClient provides dynamic CSI snapshot lifecycle operations.
type SnapshotClient struct {
	// Dynamic is the Kubernetes dynamic client used for storage resources.
	Dynamic dynamic.Interface
	// Discovery verifies that the CSI snapshot API is served by the cluster.
	Discovery discovery.DiscoveryInterface
	// ClassName explicitly selects a VolumeSnapshotClass when set.
	ClassName string
	// RunID identifies the command run that owns the temporary snapshot.
	RunID string
	// ReadyTimeout bounds waiting for status.readyToUse.
	ReadyTimeout time.Duration
}

// SnapshotHandle identifies a namespaced VolumeSnapshot.
type SnapshotHandle struct {
	// Namespace is the snapshot namespace.
	Namespace string
	// Name is the VolumeSnapshot name.
	Name string
	// ClassName is the explicitly requested VolumeSnapshotClass, if any.
	ClassName string
	// RunID identifies the command run that owns the temporary snapshot.
	RunID string
}

// Validate verifies CSI snapshot API availability and the source PVC reference.
func (c SnapshotClient) Validate(ctx context.Context, pvc Ref) error {
	if err := c.validate(ctx, pvc); err != nil {
		return err
	}
	if _, err := c.Discovery.ServerResourcesForGroupVersion(snapshotGroupVersion); err != nil {
		return fmt.Errorf("discover CSI snapshot API: %w", err)
	}

	return nil
}

// create submits a VolumeSnapshot request with caller-selected persistence semantics.
func (c SnapshotClient) create(
	ctx context.Context,
	pvc Ref,
	snapshotName string,
	options metav1.CreateOptions,
) (SnapshotHandle, error) {
	if err := c.validate(ctx, pvc); err != nil {
		return SnapshotHandle{}, err
	}
	if snapshotName == "" || strings.ContainsAny(snapshotName, "/\\") {
		return SnapshotHandle{}, errors.New("snapshot name is invalid")
	}

	spec := map[string]any{
		"source": map[string]any{"persistentVolumeClaimName": pvc.Name},
	}
	if c.ClassName != "" {
		spec["volumeSnapshotClassName"] = c.ClassName
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": snapshotGroupVersion,
		"kind":       "VolumeSnapshot",
		"metadata": map[string]any{
			"generateName": snapshotName,
			"labels":       temporaryResourceLabels(pvc, c.RunID),
		},
		"spec": spec,
	}}

	created, err := c.Dynamic.
		Resource(volumeSnapshotResource).
		Namespace(pvc.Namespace).
		Create(ctx, object, options)
	if err != nil {
		return SnapshotHandle{}, fmt.Errorf("create VolumeSnapshot %q: %w", snapshotName, err)
	}

	return SnapshotHandle{
		Namespace: pvc.Namespace,
		Name:      created.GetName(),
		ClassName: c.ClassName,
		RunID:     c.RunID,
	}, nil
}

// WaitReady waits until a VolumeSnapshot reports readyToUse=true.
//
// A missing readiness field is treated as pending
// because CSI drivers may publish status in several API updates.
// A status error is retained as diagnostic context because the CSI controller
// may recover and clear it on a later reconciliation attempt.
func (c SnapshotClient) WaitReady(ctx context.Context, ref SnapshotHandle) error {
	if c.Dynamic == nil || ctx == nil {
		return errors.New("snapshot client and context are required")
	}
	if ref.Namespace == "" || ref.Name == "" {
		return errors.New("snapshot reference is required")
	}

	timeout := c.ReadyTimeout
	if timeout == 0 {
		timeout = defaultSnapshotWait
	}
	if timeout < 0 {
		return errors.New("snapshot ready timeout must not be negative")
	}

	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resource := c.Dynamic.
		Resource(volumeSnapshotResource).
		Namespace(ref.Namespace)
	var (
		lastStatus   string
		lastAPIError string
		lastCSIError string
	)

	err := wait.PollUntilContextCancel(waitContext, time.Second, true, func(ctx context.Context) (bool, error) {
		object, err := resource.Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			lastStatus = "object not found"
			return false, nil
		}
		if err != nil {
			// A transient API connection failure should not discard a snapshot
			// that may already be progressing in the CSI controller.
			if retryableSnapshotGetError(err) {
				lastAPIError = err.Error()
				return false, nil
			}

			return false, err
		}

		ready, found, err := unstructured.NestedBool(object.Object, "status", "readyToUse")
		if err != nil {
			return false, fmt.Errorf("read VolumeSnapshot readiness: %w", err)
		}
		if found && ready {
			return true, nil
		}

		// Keep the latest controller state for timeout diagnostics.
		// CSI errors are not returned immediately because the driver may reconcile them.
		lastStatus = snapshotStatusMessage(object)
		failure, _, _ := unstructured.NestedString(object.Object, "status", "error", "message")
		if failure != "" {
			lastCSIError = failure
		}

		return false, nil
	})
	if err != nil {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			// Include every independently useful signal: the Kubernetes status,
			// the CSI-reported failure, and the last API transport error.
			details := make([]string, 0, 3)
			if lastStatus != "" {
				details = append(details, "last status: "+lastStatus)
			}
			if lastCSIError != "" {
				details = append(details, "last CSI error: "+lastCSIError)
			}
			if lastAPIError != "" {
				details = append(details, "last API error: "+lastAPIError)
			}

			if len(details) > 0 {
				return fmt.Errorf(
					"VolumeSnapshot %s/%s did not become ready within %s: %w; %s",
					ref.Namespace,
					ref.Name,
					timeout,
					err,
					strings.Join(details, "; "),
				)
			}

			return fmt.Errorf(
				"VolumeSnapshot %s/%s did not become ready within %s: %w",
				ref.Namespace,
				ref.Name,
				timeout,
				err,
			)
		}

		return fmt.Errorf("wait for VolumeSnapshot %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return nil
}

// snapshotStatusMessage returns the compact state needed to diagnose a stalled snapshot
// without dumping the complete Kubernetes object into the error.
func snapshotStatusMessage(object *unstructured.Unstructured) string {
	if object == nil {
		return "status not reported"
	}

	status, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil || !found || len(status) == 0 {
		return "status not reported"
	}

	details := make([]string, 0, 3)
	if ready, found := status["readyToUse"]; found {
		details = append(details, fmt.Sprintf("readyToUse=%v", ready))
	}
	if content, ok := status["boundVolumeSnapshotContentName"].(string); ok && content != "" {
		details = append(details, "boundContent="+content)
	}

	if len(details) == 0 {
		return "status reported without readiness details"
	}

	return strings.Join(details, ", ")
}

// retryableSnapshotGetError identifies transport failures
// that should not terminate readiness polling for an otherwise valid snapshot request.
func retryableSnapshotGetError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var networkError net.Error
	return errors.As(err, &networkError)
}

// Delete removes a VolumeSnapshot and treats an absent object as success.
func (c SnapshotClient) Delete(ctx context.Context, ref SnapshotHandle) error {
	if c.Dynamic == nil || ctx == nil {
		return errors.New("snapshot client and context are required")
	}
	if ref.Namespace == "" || ref.Name == "" {
		return errors.New("snapshot reference is required")
	}

	err := c.Dynamic.
		Resource(volumeSnapshotResource).
		Namespace(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete VolumeSnapshot %q: %w", ref.Name, err)
	}

	return nil
}

// validate checks the clients and source PVC before any snapshot API call.
func (c SnapshotClient) validate(ctx context.Context, pvc Ref) error {
	if c.Dynamic == nil || c.Discovery == nil {
		return errors.New("snapshot dynamic and discovery clients are required")
	}
	if ctx == nil {
		return errors.New("snapshot context is required")
	}

	return pvc.Validate()
}
