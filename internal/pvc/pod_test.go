package pvc

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestHelperPodStartCreatesRestrictedPVCMount(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetName("kube-dump-helper-abc")
		return false, nil, nil
	})

	options := HelperPodOptions{
		Dynamic:  client,
		Image:    "registry.example/helper:v1",
		ReadOnly: true,
		RunID:    "20260902T120000.000000000Z",
	}

	pod, err := options.Start(
		context.Background(),
		Ref{Namespace: "default", Name: "data"},
		"kube-dump-helper",
	)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	created, err := client.Resource(podResource).
		Namespace(pod.Namespace).
		Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	automount, found, _ := unstructured.
		NestedBool(created.Object, "spec", "automountServiceAccountToken")
	if !found || automount {
		t.Fatal("helper Pod automounted a service account token")
	}

	labels, found, err := unstructured.NestedStringMap(created.Object, "metadata", "labels")
	if err != nil || !found {
		t.Fatalf("helper labels = %#v, found = %t, error = %v", labels, found, err)
	}
	for key, want := range map[string]string{
		"app.kubernetes.io/managed-by": "kube-dump",
		"kube-dump/source-namespace":   "default",
		"kube-dump/source-pvc":         "data",
		"kube-dump/run-id":             "20260902T120000.000000000Z",
	} {
		if labels[key] != want {
			t.Fatalf("helper label %q = %q, want %q", key, labels[key], want)
		}
	}

	containers, found, err := unstructured.NestedSlice(created.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("helper containers = %#v, found = %t, error = %v", containers, found, err)
	}

	containerObject, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("helper container type = %T", containers[0])
	}
	pullPolicy, found, err := unstructured.NestedString(containerObject, "imagePullPolicy")
	if err != nil || !found || pullPolicy != string(corev1.PullIfNotPresent) {
		t.Fatalf("helper image pull policy = %q, found = %t, error = %v; want %q", pullPolicy, found, err, corev1.PullIfNotPresent)
	}
	deadline, found, err := unstructured.NestedInt64(created.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found || deadline != defaultHelperActiveDeadlineSeconds {
		t.Fatalf("helper active deadline = %d, found = %t, error = %v; want %d", deadline, found, err, defaultHelperActiveDeadlineSeconds)
	}

	for _, path := range [][]string{
		{"securityContext", "runAsUser"},
		{"securityContext", "runAsGroup"},
	} {
		value, found, err := unstructured.NestedInt64(containerObject, path...)
		if err != nil || !found || value != 0 {
			t.Fatalf(
				"helper container %s = %d, found = %t, error = %v; want explicit root",
				strings.Join(path, "."), value, found, err)
		}
	}
	if runAsNonRoot, found, err := unstructured.
		NestedBool(containerObject, "securityContext", "runAsNonRoot"); err != nil || !found || runAsNonRoot {
		t.Fatalf("helper container runAsNonRoot = %t, found = %t, error = %v; want false", runAsNonRoot, found, err)
	}
	if readOnlyRoot, found, err := unstructured.NestedBool(
		containerObject,
		"securityContext",
		"readOnlyRootFilesystem",
	); err != nil || !found || !readOnlyRoot {
		t.Fatalf("helper readOnlyRootFilesystem = %t, found = %t, error = %v; want true", readOnlyRoot, found, err)
	}

	addCapabilities, found, err := unstructured.NestedStringSlice(
		containerObject,
		"securityContext",
		"capabilities",
		"add",
	)
	if err != nil || !found || !reflect.DeepEqual(addCapabilities, []string{"DAC_READ_SEARCH", "DAC_OVERRIDE"}) {
		t.Fatalf("read-only helper capabilities = %#v, found = %t, error = %v", addCapabilities, found, err)
	}

	dropCapabilities, found, err := unstructured.NestedStringSlice(
		containerObject,
		"securityContext",
		"capabilities",
		"drop",
	)
	if err != nil || !found || !reflect.DeepEqual(dropCapabilities, []string{"ALL"}) {
		t.Fatalf("read-only helper dropped capabilities = %#v, found = %t, error = %v", dropCapabilities, found, err)
	}

	command, found, err := unstructured.NestedSlice(containerObject, "command")
	if err != nil || !found || len(command) == 0 || command[0] != defaultVolumeAgentBinary {
		t.Fatalf("helper command = %#v, found = %t, error = %v", command, found, err)
	}

	volumes, found, err := unstructured.NestedSlice(created.Object, "spec", "volumes")
	if err != nil || !found || len(volumes) != 2 {
		t.Fatalf("PVC volume = %#v, found = %t, error = %v", volumes, found, err)
	}

	volume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatalf("PVC volume type = %T", volumes[0])
	}

	claim, found, err := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
	if err != nil || !found || claim != "data" {
		t.Fatalf("PVC claim = %q, found = %t, error = %v", claim, found, err)
	}

	readOnly, found, err := unstructured.NestedBool(volume, "persistentVolumeClaim", "readOnly")
	if err != nil || !found || !readOnly {
		t.Fatalf("PVC readOnly = %t, found = %t, error = %v", readOnly, found, err)
	}
	tmpVolume, ok := volumes[1].(map[string]any)
	if !ok {
		t.Fatalf("temporary volume type = %T", volumes[1])
	}
	if _, found, err := unstructured.NestedFieldNoCopy(tmpVolume, "emptyDir"); err != nil || !found {
		t.Fatalf("temporary volume = %#v, found = %t, error = %v", tmpVolume, found, err)
	}
	if err := options.Cleanup(context.Background(), pod); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if pod.AgentBinary != defaultVolumeAgentBinary {
		t.Fatalf("agent binary = %q, want %q", pod.AgentBinary, defaultVolumeAgentBinary)
	}

}

func TestHelperPodStartRejectsChangedTargetIdentity(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podListResource: "PodList"},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"metadata": map[string]any{
					"namespace":       "default",
					"name":            "data",
					"uid":             "current-uid",
					"resourceVersion": "2",
				},
			},
		},
	)
	created := false
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		created = true
		return false, nil, nil
	})

	options := HelperPodOptions{
		Dynamic: client,
		Image:   "registry.example/helper:v1",
		ExpectedTargetIdentity: &TargetIdentity{
			UID:             "original-uid",
			ResourceVersion: "1",
		},
	}
	if _, err := options.Start(context.Background(), Ref{Namespace: "default", Name: "data"}, "kube-dump-helper"); err == nil {
		t.Fatal("Start() unexpectedly accepted a changed target PVC")
	}
	if created {
		t.Fatal("Start() created a helper for a changed target PVC")
	}
}

func TestHelperPodRestoreAddsMetadataCapabilities(t *testing.T) {
	t.Parallel()
	object := helperPodObject(Ref{Namespace: "default", Name: "data"}, "kube-dump-helper-", HelperPodOptions{
		Image: "registry.example/helper:v1",
	})
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("helper containers = %#v, found = %t, error = %v", containers, found, err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("helper container type = %T", containers[0])
	}
	capabilities, found, err := unstructured.NestedStringSlice(
		container,
		"securityContext",
		"capabilities",
		"add",
	)
	if err != nil || !found {
		t.Fatalf("restore capabilities = %#v, found = %t, error = %v", capabilities, found, err)
	}
	want := []string{"DAC_READ_SEARCH", "DAC_OVERRIDE", "CHOWN", "FOWNER"}
	if !reflect.DeepEqual(capabilities, want) {
		t.Fatalf("restore capabilities = %#v, want %#v", capabilities, want)
	}
}

func TestNewPodStrategyConfiguresDefaultTransferLimiter(t *testing.T) {
	t.Parallel()

	strategy, err := NewPodStrategy(PodStrategyOptions{
		Dynamic: fake.NewSimpleDynamicClient(runtime.NewScheme()),
		Exec:    &captureExecTransport{},
		Image:   "registry.example/helper:v1",
	})
	if err != nil {
		t.Fatalf("NewPodStrategy() error = %v", err)
	}
	if strategy.options.TransferLimiter == nil {
		t.Fatal("NewPodStrategy() left the transfer limiter nil")
	}
	if got := cap(strategy.options.TransferLimiter.slots); got != DefaultTransferConcurrency {
		t.Fatalf("default transfer concurrency = %d, want %d", got, DefaultTransferConcurrency)
	}
}

func TestHelperPodWaitReadyIncludesPendingStatusOnTimeout(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "helper", "namespace": "default"},
			"status": map[string]any{
				"phase": "Pending",
				"conditions": []any{map[string]any{
					"type":    "PodScheduled",
					"status":  "False",
					"reason":  "Unschedulable",
					"message": "persistentvolumeclaim is not bound",
				}},
				"containerStatuses": []any{map[string]any{
					"name": "helper",
					"state": map[string]any{"waiting": map[string]any{
						"reason": "ContainerCreating",
					}},
				}},
			},
		}}, nil
	})

	err := (HelperPodOptions{Dynamic: client, ReadyTimeout: 10 * time.Millisecond}).WaitReady(
		context.Background(),
		HelperPod{Namespace: "default", Name: "helper"},
	)
	if err == nil || !strings.Contains(err.Error(), "persistentvolumeclaim is not bound") {
		t.Fatalf("WaitReady() error = %v, want pending Pod diagnostics", err)
	}
}

func TestHelperPodWaitReadyExplainsEarlyCompletion(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "helper", "namespace": "default"},
			"status": map[string]any{
				"phase": "Succeeded",
				"containerStatuses": []any{map[string]any{
					"name": "helper",
					"state": map[string]any{"terminated": map[string]any{
						"reason": "Completed",
					}},
				}},
			},
		}}, nil
	})

	err := (HelperPodOptions{Dynamic: client}).WaitReady(
		context.Background(),
		HelperPod{Namespace: "default", Name: "helper"},
	)
	if err == nil || !strings.Contains(err.Error(), "completed before becoming ready") {
		t.Fatalf("WaitReady() error = %v, want early completion diagnostics", err)
	}
}

func TestPodStrategyRetriesDefaultAgentWithRootFallback(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podListResource: "PodList"},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "PersistentVolumeClaim",
				"metadata": map[string]any{
					"name":      "data",
					"namespace": "default",
				},
				"spec": map[string]any{
					"accessModes": []any{"ReadWriteOnce"},
					"volumeMode":  "Filesystem",
					"resources": map[string]any{
						"requests": map[string]any{"storage": "1Gi"},
					},
				},
			},
		},
	)
	created := 0
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		created++
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetName(fmt.Sprintf("helper-%d", created))
		return false, nil, nil
	})

	gets := 0
	client.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		gets++
		phase := "Running"
		status := []any{map[string]any{
			"type":   "Ready",
			"status": "True",
		}}
		containers := []any(nil)
		if gets == 1 {
			phase = "Failed"
			containers = []any{map[string]any{
				"name": "helper",
				"state": map[string]any{"terminated": map[string]any{
					"reason":  "ContainerCannotRun",
					"message": `exec: "kube-dump": executable file not found in $PATH`,
				}},
			}}
		}

		name := "helper-1"
		if gets > 1 {
			name = "helper-2"
		}
		return true, &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"status": map[string]any{
				"phase":             phase,
				"conditions":        status,
				"containerStatuses": containers,
			},
		}}, nil
	})

	strategy, err := NewPodStrategy(PodStrategyOptions{
		Dynamic: client,
		Exec:    &captureExecTransport{},
		Image:   "registry.example/helper:v1",
	})
	if err != nil {
		t.Fatalf("NewPodStrategy() error = %v", err)
	}
	strategy.options.Helper.ReadyTimeout = time.Second

	pod, err := strategy.startHelper(context.Background(), Ref{Namespace: "default", Name: "data"}, true)
	if err != nil {
		t.Fatalf("startHelper() error = %v", err)
	}
	if created != 2 {
		t.Fatalf("helper Pods created = %d, want one retry", created)
	}
	if pod.AgentBinary != "/kube-dump" {
		t.Fatalf("selected agent binary = %q, want /kube-dump", pod.AgentBinary)
	}
}
