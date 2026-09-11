// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestValidatePVCNamePatterns(t *testing.T) {
	if err := validatePVCNamePatterns([]string{"postgres-*", "data-[0-9]"}); err != nil {
		t.Fatalf("validatePVCNamePatterns() error = %v", err)
	}
	if err := validatePVCNamePatterns([]string{"["}); err == nil {
		t.Fatal("validatePVCNamePatterns() accepted an invalid pattern")
	}
}

func TestListPVCsUsesProfileLabelSelectorAtTheAPIBoundary(t *testing.T) {
	compiled, err := profile.Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "pvc-labels",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{},
		PVC: &contract.PVCPolicy{Selection: contract.PVCSelectionSpec{
			ObjectScope: contract.ObjectScope{
				LabelSelector: &contract.Selector{
					MatchLabels: map[string]string{"backup": "true"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	pvcObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      "data",
			"namespace": "default",
			"labels":    map[string]any{"backup": "true"},
		},
	}}
	pvcObject.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"})
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), pvcObject)
	claims, err := listPVCs(context.Background(), client, []string{"default"}, nil, compiled)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Name != "data" {
		t.Fatalf("listPVCs() = %#v, want the selected PVC", claims)
	}

	actions := client.Actions()
	if len(actions) != 1 {
		t.Fatalf("dynamic client actions = %d, want one list request", len(actions))
	}
	listAction, ok := actions[0].(k8stesting.ListAction)
	if !ok {
		t.Fatalf("action type = %T, want ListAction", actions[0])
	}
	if got := listAction.GetListRestrictions().Labels.String(); got != "backup=true" {
		t.Fatalf("PVC list label selector = %q, want %q", got, "backup=true")
	}
}

func TestListPVCsProcessesPaginatedPages(t *testing.T) {
	t.Parallel()

	compiled, err := profile.Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "pvc-pages",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{},
		PVC:       &contract.PVCPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Version: "v1", Resource: "persistentvolumeclaims"}: "PersistentVolumeClaimList",
		},
	)
	page := 0
	client.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		page++
		if page == 1 {
			list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: map[string]any{
				"metadata": map[string]any{"name": "first", "namespace": "default"},
			}}}}
			list.SetContinue("next")
			return true, list, nil
		}

		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: map[string]any{
			"metadata": map[string]any{"name": "second", "namespace": "default"},
		}}}}, nil
	})

	claims, err := listPVCs(context.Background(), client, []string{"default"}, nil, compiled)
	if err != nil {
		t.Fatalf("listPVCs() error = %v", err)
	}
	if len(claims) != 2 || claims[0].Name != "first" || claims[1].Name != "second" {
		t.Fatalf("listPVCs() claims = %#v, want first and second", claims)
	}
}

func TestBackupPVCClaimsContinuesAfterFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		t.Fatalf("NewVolumeStore() error = %v", err)
	}

	strategy := &testPVCBackupStrategy{failed: map[string]bool{"failed": true}}
	claims := []pvc.Ref{
		{Namespace: "default", Name: "failed"},
		{Namespace: "default", Name: "saved-a"},
		{Namespace: "default", Name: "saved-b"},
	}

	_, err = backupPVCClaims(
		context.Background(),
		&store,
		claims,
		strategy,
		pvc.DefaultLifecycleConcurrency,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "PVC backup failed for 1 of 3 claims") {
		t.Fatalf("backupPVCClaims() error = %v, want one reported failure", err)
	}

	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifact count = %d, want 2", len(artifacts))
	}
	for _, artifact := range artifacts {
		if artifact.PVC.Name == "failed" {
			t.Fatal("failed PVC produced an artifact")
		}
	}
}

func TestBackupPVCClaimsBoundsFailureDetails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		t.Fatalf("NewVolumeStore() error = %v", err)
	}

	claims := make([]pvc.Ref, 10)
	failed := make(map[string]bool, len(claims))
	for index := range claims {
		claims[index] = pvc.Ref{Namespace: "default", Name: fmt.Sprintf("failed-%02d", index)}
		failed[claims[index].Name] = true
	}

	_, err = backupPVCClaims(
		context.Background(),
		&store,
		claims,
		&testPVCBackupStrategy{failed: failed},
		pvc.DefaultLifecycleConcurrency,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "2 additional PVC failure details omitted") {
		t.Fatalf("backupPVCClaims() error = %v, want bounded failure details", err)
	}
}

func TestBackupPVCClaimsStartsIndependentTasksConcurrently(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		t.Fatalf("NewVolumeStore() error = %v", err)
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	strategy := &testPVCBackupStrategy{started: started, release: release}
	claims := []pvc.Ref{
		{Namespace: "default", Name: "first"},
		{Namespace: "default", Name: "second"},
	}

	result := make(chan error, 1)
	go func() {
		_, err := backupPVCClaims(
			context.Background(),
			&store,
			claims,
			strategy,
			pvc.DefaultLifecycleConcurrency,
			nil,
		)
		result <- err
	}()

	seen := make(map[string]struct{}, len(claims))
	for range claims {
		select {
		case name := <-started:
			seen[name] = struct{}{}
		case <-time.After(time.Second):
			t.Fatal("backupPVCClaims() did not start all independent tasks")
		}
	}
	close(release)

	if err := <-result; err != nil {
		t.Fatalf("backupPVCClaims() error = %v", err)
	}
	if len(seen) != len(claims) {
		t.Fatalf("started claims = %#v, want %d distinct claims", seen, len(claims))
	}
}

func TestBackupPVCClaimsBoundsLifecycleWorkers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		t.Fatalf("NewVolumeStore() error = %v", err)
	}

	concurrency := 1
	started := make(chan string, concurrency+1)
	release := make(chan struct{})
	strategy := &testPVCBackupStrategy{started: started, release: release}
	claims := make([]pvc.Ref, concurrency+1)
	for index := range claims {
		claims[index] = pvc.Ref{Namespace: "default", Name: fmt.Sprintf("claim-%d", index)}
	}

	result := make(chan error, 1)
	go func() {
		_, backupErr := backupPVCClaims(
			context.Background(),
			&store,
			claims,
			strategy,
			concurrency,
			nil,
		)
		result <- backupErr
	}()

	for range concurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("backupPVCClaims() did not start the expected lifecycle workers")
		}
	}

	select {
	case claim := <-started:
		t.Fatalf("lifecycle worker limit exceeded; started %s", claim)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backupPVCClaims() did not start the queued claim")
	}
	if backupErr := <-result; backupErr != nil {
		t.Fatalf("backupPVCClaims() error = %v", backupErr)
	}
}

func TestBackupPVCClaimsKeepsArtifactWhenCleanupWarns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := pvc.NewVolumeStore(root)
	if err != nil {
		t.Fatalf("NewVolumeStore() error = %v", err)
	}

	claim := pvc.Ref{Namespace: "default", Name: "cleanup-warning"}
	_, err = backupPVCClaims(
		context.Background(),
		&store,
		[]pvc.Ref{claim},
		&testPVCBackupStrategy{
			warning: map[string]bool{claim.Name: true},
		},
		pvc.DefaultLifecycleConcurrency,
		nil,
	)
	if err != nil {
		t.Fatalf("backupPVCClaims() error = %v, want cleanup warning only", err)
	}
	artifacts, err := store.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(artifacts))
	}
}

// testPVCBackupStrategy writes a minimal valid artifact for orchestration tests.
type testPVCBackupStrategy struct {
	failed  map[string]bool
	warning map[string]bool
	started chan<- string
	release <-chan struct{}
}

// Backup simulates a PVC data mover with deterministic success and failure cases.
func (s *testPVCBackupStrategy) Backup(_ context.Context, claim pvc.Ref, dst io.Writer) (pvc.Metadata, error) {
	if s.failed[claim.Name] {
		return pvc.Metadata{}, io.ErrUnexpectedEOF
	}
	if s.started != nil {
		s.started <- claim.Name
		<-s.release
	}

	payload := "test-payload-" + claim.Name
	if _, err := io.WriteString(dst, payload); err != nil {
		return pvc.Metadata{}, err
	}

	metadata := pvc.NewMetadata(pvc.Pod, string(compress.Gzip))
	metadata.Portable = true
	metadata.SizeBytes = int64(len(payload))
	metadata.ContentSHA256 = strings.Repeat("0", 64)

	if s.warning[claim.Name] {
		return metadata, &pvc.CleanupWarning{
			Resource: "helper Pod default/helper",
			Err:      io.ErrClosedPipe,
		}
	}

	return metadata, nil
}
