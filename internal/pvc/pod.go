// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const (
	defaultVolumeAgentBinary                 = "kube-dump"
	fallbackVolumeAgentBinary                = "/kube-dump"
	defaultHelperReadyWait                   = 10 * time.Minute
	defaultHelperActiveDeadlineSeconds int64 = 24 * 60 * 60
)

var (
	// errHelperAgentStart marks a helper failure caused by a missing executable.
	errHelperAgentStart = errors.New("helper agent failed to start")
	podResource         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

// HelperPodOptions configures temporary helper Pod creation and readiness.
type HelperPodOptions struct {
	// Dynamic is the Kubernetes dynamic client used for Pod lifecycle calls.
	Dynamic dynamic.Interface
	// ExpectedTargetIdentity rejects a changed or recreated restore target.
	ExpectedTargetIdentity *TargetIdentity
	// Image is the pinned container image; it must be supplied explicitly.
	Image string
	// ImagePullPolicy controls when Kubernetes pulls the helper image.
	ImagePullPolicy corev1.PullPolicy
	// AgentBinary is the executable path inside the helper container.
	AgentBinary string
	// Container is the helper container name.
	Container string
	// MountPath is the path where the PVC is mounted.
	MountPath string
	// NodeName optionally pins the helper to an existing PVC consumer node.
	NodeName string
	// RunID identifies the command run that owns the temporary Pod.
	RunID string
	// Arguments replaces the default volume hold command.
	Arguments []string
	// EnvironmentFromSecrets exposes temporary Secret keys to the helper.
	EnvironmentFromSecrets []string
	// SecretVolumes mounts temporary Secrets as read-only files in the helper.
	SecretVolumes []HelperSecretVolume
	// ExpectedTargetSize is the minimum restore payload size accepted by the target PVC.
	ExpectedTargetSize int64
	// ReadyTimeout bounds Pod readiness waiting.
	ReadyTimeout time.Duration
	// ActiveDeadlineSeconds bounds the lifetime of an abandoned helper Pod.
	ActiveDeadlineSeconds int64
	// ReadOnly requests a read-only PVC mount for backup operations.
	ReadOnly bool
	// AutomountServiceAccountToken enables workload identity for the helper.
	// Local volume helpers keep this disabled unless a remote mover needs it.
	AutomountServiceAccountToken bool
}

// HelperSecretVolume describes a temporary Secret mounted into a helper Pod.
type HelperSecretVolume struct {
	// SecretName is the Kubernetes Secret to mount.
	SecretName string
	// MountPath is the absolute path inside the helper container.
	MountPath string
}

// HelperPod identifies a created helper Pod and its mounted volume.
type HelperPod struct {
	// Namespace is the namespace containing the helper Pod.
	Namespace string
	// Name is the helper Pod name.
	Name string
	// AgentBinary is the executable path selected for volume-agent commands.
	AgentBinary string
	// Container is the container used for volume I/O.
	Container string
	// MountPath is the mounted PVC path inside the container.
	MountPath string
}

// Start creates a restricted helper Pod mounting the target PVC.
//
// The returned reference is usable only after WaitReady succeeds.
// Callers are responsible for invoking Cleanup on every successfully created Pod,
// including when a later stream operation fails.
func (o HelperPodOptions) Start(ctx context.Context, pvc Ref, podNamePrefix string) (HelperPod, error) {
	if err := o.normalize(ctx, pvc, podNamePrefix); err != nil {
		return HelperPod{}, err
	}

	if o.ExpectedTargetIdentity != nil {
		actual, err := ValidateRestoreTarget(ctx, o.Dynamic, pvc, o.ExpectedTargetSize)
		if err != nil {
			return HelperPod{}, fmt.Errorf("recheck restore target: %w", err)
		}

		// ResourceVersion changes during ordinary controller/status updates;
		// UID is the immutable identity that detects delete/recreate races.
		if actual.UID != o.ExpectedTargetIdentity.UID {
			return HelperPod{}, fmt.Errorf(
				"target PVC %s/%s changed after restore preflight",
				pvc.Namespace,
				pvc.Name,
			)
		}
	}

	object := helperPodObject(pvc, podNamePrefix, o)
	resource := o.Dynamic.Resource(podResource).Namespace(pvc.Namespace)
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return HelperPod{}, fmt.Errorf("create helper Pod with prefix %q: %w", podNamePrefix, err)
	}

	return HelperPod{
		Namespace:   pvc.Namespace,
		Name:        created.GetName(),
		AgentBinary: o.AgentBinary,
		Container:   o.Container,
		MountPath:   o.MountPath,
	}, nil
}

// normalize applies helper defaults and validates the complete Pod contract
// before a helper Pod request is sent to Kubernetes.
func (o *HelperPodOptions) normalize(ctx context.Context, pvc Ref, podNamePrefix string) error {
	if o.Dynamic == nil {
		return errors.New("helper Pod dynamic client is required")
	}
	if err := pvc.Validate(); err != nil {
		return err
	}
	if podNamePrefix == "" || strings.ContainsAny(podNamePrefix, "/\\") {
		return errors.New("helper Pod name is invalid")
	}
	if o.Image == "" {
		return errors.New("helper Pod image is required")
	}

	// Keep the default explicit in the generated Pod
	// so helper behavior is reproducible across clusters with different admission defaults.
	if o.ImagePullPolicy == "" {
		o.ImagePullPolicy = corev1.PullIfNotPresent
	}

	switch o.ImagePullPolicy {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return fmt.Errorf("helper Pod image pull policy %q is invalid", o.ImagePullPolicy)
	}

	if o.AgentBinary == "" {
		o.AgentBinary = defaultVolumeAgentBinary
	}

	// The binary is passed as an argv value, not interpolated into a shell command;
	// reject only bytes that cannot survive the Kubernetes API contract.
	if strings.IndexByte(o.AgentBinary, 0) >= 0 {
		return errors.New("helper Pod agent binary contains a null byte")
	}

	if o.Container == "" {
		o.Container = "helper"
	}
	if o.MountPath == "" {
		o.MountPath = "/data"
	}

	// Secret mounts must be distinct from the PVC mount because the agent receives credentials
	// and identity material through separate paths.
	if !strings.HasPrefix(o.MountPath, "/") ||
		strings.Contains(o.MountPath, "\x00") ||
		strings.Contains(o.MountPath, "..") {
		return errors.New("helper Pod mount path must be an absolute path without parent traversal")
	}

	for _, secret := range o.SecretVolumes {
		if secret.SecretName == "" {
			return errors.New("helper Pod Secret name is required")
		}
		if !strings.HasPrefix(secret.MountPath, "/") ||
			strings.Contains(secret.MountPath, "\x00") ||
			strings.Contains(secret.MountPath, "..") ||
			secret.MountPath == o.MountPath {
			return errors.New("helper Pod Secret mount path is invalid")
		}
	}

	// A zero timeout means the package default;
	// negative values are rejected instead of being interpreted as an immediately expired wait.
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = defaultHelperReadyWait
	}
	if o.ReadyTimeout < 0 {
		return errors.New("helper Pod ready timeout must not be negative")
	}
	if o.ActiveDeadlineSeconds == 0 {
		o.ActiveDeadlineSeconds = defaultHelperActiveDeadlineSeconds
	}
	if o.ActiveDeadlineSeconds < 1 {
		return errors.New("helper Pod active deadline must be positive")
	}
	if ctx == nil {
		return errors.New("helper Pod context is required")
	}

	return nil
}

// WaitReady waits for the helper Pod to be Running and Ready.
func (o HelperPodOptions) WaitReady(ctx context.Context, pod HelperPod) error {
	if o.Dynamic == nil {
		return errors.New("helper Pod dynamic client is required")
	}
	if pod.Namespace == "" || pod.Name == "" {
		return errors.New("helper Pod reference is required")
	}
	if ctx == nil {
		return errors.New("helper Pod context is required")
	}

	timeout := o.ReadyTimeout
	if timeout == 0 {
		timeout = defaultHelperReadyWait
	}
	if timeout < 0 {
		return errors.New("helper Pod ready timeout must not be negative")
	}

	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resource := o.Dynamic.Resource(podResource).Namespace(pod.Namespace)
	var lastStatus string

	// Polling is used instead of a watch because this helper is short-lived
	// and must also handle a Pod that disappears or finishes between API updates.
	// Polling keeps this lifecycle independent of informer setup
	// and treats a transiently absent Pod as not-ready rather than as success.
	err := wait.PollUntilContextCancel(
		waitContext,
		500*time.Millisecond,
		true,
		func(ctx context.Context) (bool, error) {
			object, err := resource.Get(ctx, pod.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				lastStatus = "object not found"
				return false, nil
			}
			if err != nil {
				return false, err
			}

			phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
			lastStatus = helperPodStatusMessage(object)

			if phase == "Failed" || phase == "Succeeded" {
				// A completed helper cannot become ready.
				// Preserve the terminal reason so image or agent startup failures remain actionable.
				if phase == "Succeeded" {
					return false, fmt.Errorf("helper Pod completed before becoming ready: %s", helperPodFailureMessage(object))
				}

				failure := fmt.Errorf("helper Pod ended with phase %s: %s", phase, helperPodFailureMessage(object))
				if helperPodHasStartError(object) {
					return false, fmt.Errorf("%w: %w", errHelperAgentStart, failure)
				}

				return false, failure
			}
			if phase != "Running" {
				return false, nil
			}

			// Running is not sufficient:
			// the exec endpoint is usable only after the Pod's Ready condition becomes True.
			conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
			if err != nil {
				return false, fmt.Errorf("read helper Pod conditions: %w", err)
			}

			for _, value := range conditions {
				condition, ok := value.(map[string]any)
				if !ok {
					continue
				}

				conditionType, _ := condition["type"].(string)
				status, _ := condition["status"].(string)
				if conditionType == "Ready" && status == "True" {
					return true, nil
				}
			}

			return false, nil
		})
	if err != nil && errors.Is(waitContext.Err(), context.DeadlineExceeded) && lastStatus != "" {
		return fmt.Errorf(
			"helper Pod %s/%s did not become ready within %s: %w; last status: %s",
			pod.Namespace,
			pod.Name,
			timeout,
			err,
			lastStatus,
		)
	}
	if err != nil && errors.Is(waitContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf(
			"helper Pod %s/%s did not become ready within %s: %w",
			pod.Namespace,
			pod.Name,
			timeout,
			err,
		)
	}

	return err
}

// WaitCompleted waits for a one-shot helper Pod to finish
// and returns its termination message.
// The message is the bounded result channel used by helper commands
// that perform work outside Kubernetes exec.
func (o HelperPodOptions) WaitCompleted(ctx context.Context, pod HelperPod, timeout time.Duration) (string, error) {
	if o.Dynamic == nil {
		return "", errors.New("helper Pod dynamic client is required")
	}
	if pod.Namespace == "" || pod.Name == "" {
		return "", errors.New("helper Pod reference is required")
	}
	if ctx == nil {
		return "", errors.New("helper Pod context is required")
	}
	if timeout == 0 {
		timeout = defaultHelperReadyWait
	}
	if timeout < 0 {
		return "", errors.New("helper Pod completion timeout must not be negative")
	}

	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resource := o.Dynamic.Resource(podResource).Namespace(pod.Namespace)
	var lastStatus string
	var result string

	// Completion is different from readiness:
	// a successful helper communicates its result through the termination message,
	// while a failed helper must be classified before the caller decides whether retry is safe.
	err := wait.PollUntilContextCancel(
		waitContext,
		500*time.Millisecond,
		true,
		func(ctx context.Context) (bool, error) {
			object, err := resource.Get(ctx, pod.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				lastStatus = "object not found"
				return false, nil
			}
			if err != nil {
				return false, err
			}

			phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
			lastStatus = helperPodStatusMessage(object)

			switch phase {
			case "Succeeded":
				result = helperPodTerminationMessage(object)
				return true, nil

			case "Failed":
				failure := fmt.Errorf(
					"helper Pod %s/%s failed: %s",
					pod.Namespace,
					pod.Name,
					helperPodFailureMessage(object),
				)
				if helperPodHasStartError(object) {
					return false, fmt.Errorf("%w: %w", errHelperAgentStart, failure)
				}
				return false, failure

			default:
				return false, nil
			}
		},
	)
	if err != nil && errors.Is(waitContext.Err(), context.DeadlineExceeded) {
		if lastStatus != "" {
			return "", fmt.Errorf(
				"helper Pod %s/%s did not complete within %s: %w; last status: %s",
				pod.Namespace,
				pod.Name,
				timeout,
				err,
				lastStatus,
			)
		}

		return "", fmt.Errorf(
			"helper Pod %s/%s did not complete within %s: %w",
			pod.Namespace,
			pod.Name,
			timeout,
			err,
		)
	}
	if err != nil {
		return "", err
	}

	return result, nil
}

// helperPodHasStartError reports whether a failed helper Pod could not execute its configured entrypoint.
// Application failures must not be retried.
func helperPodHasStartError(object *unstructured.Unstructured) bool {
	if object == nil {
		return false
	}

	statuses, _, _ := unstructured.NestedSlice(object.Object, "status", "containerStatuses")
	for _, value := range statuses {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}

		state, _, _ := unstructured.NestedMap(container, "state")
		for _, stateName := range []string{"waiting", "terminated"} {
			reason, _, _ := unstructured.NestedString(state, stateName, "reason")
			message, _, _ := unstructured.NestedString(state, stateName, "message")
			text := strings.ToLower(reason + " " + message)

			if strings.Contains(text, "executable file not found") ||
				(strings.Contains(text, "exec:") && strings.Contains(text, "no such file or directory")) ||
				(strings.Contains(text, "stat ") && strings.Contains(text, "no such file or directory")) {
				return true
			}
		}
	}

	return false
}

// helperAgentBinaries returns the configured executable or the two supported defaults:
// a PATH lookup followed by the binary location in the base image.
func helperAgentBinaries(configured string) []string {
	if configured != "" {
		return []string{configured}
	}

	return []string{defaultVolumeAgentBinary, fallbackVolumeAgentBinary}
}

// helperPodStatusMessage extracts the useful scheduling and container state
// for a Pod that is not ready without exposing the complete Kubernetes object.
func helperPodStatusMessage(object *unstructured.Unstructured) string {
	if object == nil {
		return "status not reported"
	}

	details := make([]string, 0, 5)
	// Start with Pod-level state, then append scheduling and container details;
	// this keeps the error useful even when one of the nested status sections is absent or malformed.
	if phase, found, _ := unstructured.NestedString(object.Object, "status", "phase"); found && phase != "" {
		details = append(details, "phase="+phase)
	}
	if reason, found, _ := unstructured.NestedString(object.Object, "status", "reason"); found && reason != "" {
		details = append(details, "reason="+reason)
	}
	if message, found, _ := unstructured.NestedString(object.Object, "status", "message"); found && message != "" {
		details = append(details, "message="+message)
	}

	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, value := range conditions {
		condition, ok := value.(map[string]any)
		if !ok {
			continue
		}

		conditionType, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		if status != "False" || (conditionType != "Ready" && conditionType != "PodScheduled") {
			continue
		}

		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		detail := conditionType + "=False"
		if reason != "" {
			detail += " reason=" + reason
		}
		if message != "" {
			detail += " message=" + message
		}
		details = append(details, detail)
	}

	// Container state usually explains why a helper never becomes ready,
	// for example ImagePullBackOff or a process that exits immediately.
	statuses, _, _ := unstructured.NestedSlice(object.Object, "status", "containerStatuses")
	for _, value := range statuses {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}

		name, _, _ := unstructured.NestedString(container, "name")
		state, _, _ := unstructured.NestedMap(container, "state")
		for _, stateName := range []string{"waiting", "terminated"} {
			reason, _, _ := unstructured.NestedString(state, stateName, "reason")
			message, _, _ := unstructured.NestedString(state, stateName, "message")
			if reason == "" && message == "" {
				continue
			}

			detail := name + " " + stateName
			if reason != "" {
				detail += " reason=" + reason
			}
			if message != "" {
				detail += " message=" + message
			}
			details = append(details, detail)

			break
		}
	}

	if len(details) == 0 {
		return "status reported without scheduling or container details"
	}

	return strings.Join(details, "; ")
}

// helperPodFailureMessage extracts the most useful API
// and container details from a failed helper Pod without exposing its full object in the error.
func helperPodFailureMessage(object *unstructured.Unstructured) string {
	if object == nil {
		return "no failure details"
	}

	details := make([]string, 0, 3)
	if reason, found, _ := unstructured.NestedString(object.Object, "status", "reason"); found && reason != "" {
		details = append(details, "reason="+reason)
	}
	if message, found, _ := unstructured.NestedString(object.Object, "status", "message"); found && message != "" {
		details = append(details, "message="+message)
	}

	statuses, _, _ := unstructured.NestedSlice(object.Object, "status", "containerStatuses")
	for _, value := range statuses {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}

		name, _, _ := unstructured.NestedString(container, "name")
		state, _, _ := unstructured.NestedMap(container, "state")
		for _, stateName := range []string{"waiting", "terminated"} {
			reason, _, _ := unstructured.NestedString(state, stateName, "reason")
			message, _, _ := unstructured.NestedString(state, stateName, "message")
			if reason == "" && message == "" {
				continue
			}

			detail := name + " " + stateName
			if reason != "" {
				detail += " reason=" + reason
			}
			if message != "" {
				detail += " message=" + message
			}
			details = append(details, detail)
			break
		}
	}

	if len(details) == 0 {
		return "no failure details"
	}

	return strings.Join(details, "; ")
}

// helperPodTerminationMessage returns the message from the terminated helper container
// while keeping the lifecycle polling code independent of its schema.
func helperPodTerminationMessage(object *unstructured.Unstructured) string {
	if object == nil {
		return ""
	}

	statuses, _, _ := unstructured.NestedSlice(object.Object, "status", "containerStatuses")
	for _, value := range statuses {
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}

		state, _, _ := unstructured.NestedMap(container, "state")
		message, _, _ := unstructured.NestedString(state, "terminated", "message")
		if message != "" {
			return message
		}
	}

	return ""
}

// Cleanup deletes a helper Pod and treats an already absent Pod as success.
func (o HelperPodOptions) Cleanup(ctx context.Context, pod HelperPod) error {
	if o.Dynamic == nil {
		return errors.New("helper Pod dynamic client is required")
	}
	if ctx == nil {
		return errors.New("helper Pod context is required")
	}
	if pod.Namespace == "" || pod.Name == "" {
		return errors.New("helper Pod reference is required")
	}
	if err := o.Dynamic.Resource(podResource).
		Namespace(pod.Namespace).
		Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete helper Pod %q: %w", pod.Name, err)
	}

	return nil
}

// helperPodObject builds a helper Pod with one PVC mount.
// The agent runs as root because a backup must be able to read files owned by arbitrary UIDs,
// including private database directories such as mode 0700.
// The helper has no added capabilities beyond its explicit file-access contract
// and cannot escalate privileges inside the container. Its external mounts are
// the PVC, an ephemeral /tmp workspace used by restore streaming, and optional Secrets.
// Service-account token mounting is controlled separately for ambient S3 identity.
func helperPodObject(pvc Ref, podName string, options HelperPodOptions) *unstructured.Unstructured {
	capabilities := map[string]any{
		"drop": []any{"ALL"},
		"add":  []any{"DAC_READ_SEARCH", "DAC_OVERRIDE"},
	}
	if !options.ReadOnly {
		// Restore must apply numeric ownership and metadata after writing entries.
		// CHOWN changes the owner;
		// FOWNER permits chmod and timestamp updates after that change.
		capabilities["add"] = []any{
			"DAC_READ_SEARCH",
			"DAC_OVERRIDE",
			"CHOWN",
			"FOWNER",
		}
	}

	command := []any{options.AgentBinary, "volume", "hold"}
	if len(options.Arguments) > 0 {
		command = make([]any, 1, 1+len(options.Arguments))
		command[0] = options.AgentBinary
		for _, argument := range options.Arguments {
			command = append(command, argument)
		}
	}

	container := map[string]any{
		"name":                     options.Container,
		"image":                    options.Image,
		"imagePullPolicy":          string(options.ImagePullPolicy),
		"command":                  command,
		"terminationMessagePolicy": "File",
		"securityContext": map[string]any{
			"runAsUser":                int64(0),
			"runAsGroup":               int64(0),
			"runAsNonRoot":             false,
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities":             capabilities,
		},
		"volumeMounts": []any{map[string]any{
			"name":      "data",
			"mountPath": options.MountPath,
		}},
	}
	if len(options.EnvironmentFromSecrets) > 0 {
		envFrom := make([]any, 0, len(options.EnvironmentFromSecrets))
		for _, secret := range options.EnvironmentFromSecrets {
			envFrom = append(envFrom, map[string]any{"secretRef": map[string]any{"name": secret}})
		}
		container["envFrom"] = envFrom
	}

	volumeMounts := container["volumeMounts"].([]any)
	volumeMounts = append(volumeMounts, map[string]any{
		"name":      "tmp",
		"mountPath": "/tmp",
	})
	volumes := make([]any, 0, 2+len(options.SecretVolumes))
	volumes = append(volumes, map[string]any{
		"name": "data",
		"persistentVolumeClaim": map[string]any{
			"claimName": pvc.Name,
			"readOnly":  options.ReadOnly,
		},
	})
	volumes = append(volumes, map[string]any{
		"name":     "tmp",
		"emptyDir": map[string]any{},
	})

	for index, secret := range options.SecretVolumes {
		name := fmt.Sprintf("secret-%d", index)
		volumeMounts = append(volumeMounts, map[string]any{
			"name":      name,
			"mountPath": secret.MountPath,
			"readOnly":  true,
		})
		volumes = append(volumes, map[string]any{
			"name": name,
			"secret": map[string]any{
				"secretName":  secret.SecretName,
				"defaultMode": int64(0o400),
			},
		})
	}

	container["volumeMounts"] = volumeMounts

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"generateName": podName,
			"labels":       temporaryResourceLabels(pvc, options.RunID),
		},
		"spec": map[string]any{
			"activeDeadlineSeconds":        options.ActiveDeadlineSeconds,
			"restartPolicy":                "Never",
			"automountServiceAccountToken": options.AutomountServiceAccountToken,
			"nodeName":                     options.NodeName,
			"securityContext": map[string]any{
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{container},
			"volumes":    volumes,
		},
	}}
}

// temporaryResourceLabels identifies resources created for one source PVC and, when supplied, one command run.
//
// The run label makes orphaned resources from concurrent or interrupted captures easy
// to distinguish without inspecting names.
// Values are bounded by Kubernetes namespace and PVC name limits,
// so they are safe to use as label values and remain directly searchable with kubectl.
func temporaryResourceLabels(pvc Ref, runID string) map[string]any {
	labels := map[string]any{
		"app.kubernetes.io/managed-by": "kube-dump",
		"kube-dump/source-namespace":   pvc.Namespace,
		"kube-dump/source-pvc":         pvc.Name,
	}
	if runID != "" {
		labels["kube-dump/run-id"] = runID
	}

	return labels
}
