// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/woozymasta/kube-dump

package pvc

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRestoreLockNameIsStablePerPVC(t *testing.T) {
	target := Ref{Namespace: "ci", Name: "database"}
	first := restoreLockName(target, "uid-1")
	second := restoreLockName(target, "uid-1")
	otherNamespace := restoreLockName(Ref{Namespace: "production", Name: "database"}, "uid-1")
	otherName := restoreLockName(Ref{Namespace: "ci", Name: "cache"}, "uid-1")
	recreated := restoreLockName(target, "uid-2")

	if first != second {
		t.Fatalf("restoreLockName() is not stable: %q != %q", first, second)
	}
	if first == otherNamespace || first == otherName {
		t.Fatalf("restoreLockName() collided for distinct PVCs: %q", first)
	}
	if first == recreated {
		t.Fatalf("restoreLockName() reused a name for a recreated PVC: %q", first)
	}
	if len(first) > 63 {
		t.Fatalf("restoreLockName() returned an invalid Lease name: %q", first)
	}
}

func TestWithRestoreLockRunsCallback(t *testing.T) {
	target := Ref{Namespace: "ci", Name: "database"}
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		Namespace:       target.Namespace,
		Name:            target.Name,
		UID:             "database-uid",
		ResourceVersion: "1",
	})
	callbackCalled := make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := withRestoreLockClient(ctx, client, target, TargetIdentity{
		UID:             "database-uid",
		ResourceVersion: "1",
	}, func(callbackContext context.Context) error {
		if callbackContext == nil {
			t.Fatal("restore callback received a nil context")
		}
		close(callbackCalled)
		return nil
	})
	if err != nil {
		t.Fatalf("withRestoreLockClient() error = %v", err)
	}

	select {
	case <-callbackCalled:
	default:
		t.Fatal("restore callback was not called")
	}

	lease, err := client.CoordinationV1().Leases(target.Namespace).Get(
		context.Background(),
		restoreLockName(target, "database-uid"),
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get restore Lease: %v", err)
	}
	if lease.Name != restoreLockName(target, "database-uid") {
		t.Fatalf("Lease name = %q, want %q", lease.Name, restoreLockName(target, "database-uid"))
	}
}

func TestWithRestoreLockRejectsRecreatedPVCBeforeCallback(t *testing.T) {
	target := Ref{Namespace: "ci", Name: "database"}
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       target.Namespace,
			Name:            target.Name,
			UID:             "new-database-uid",
			ResourceVersion: "2",
		},
	})
	callbackCalled := false

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := withRestoreLockClient(ctx, client, target, TargetIdentity{
		UID:             "old-database-uid",
		ResourceVersion: "1",
	}, func(context.Context) error {
		callbackCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("withRestoreLockClient() accepted a recreated PVC")
	}
	if callbackCalled {
		t.Fatal("restore callback ran for a recreated PVC")
	}
}
