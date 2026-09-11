// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	restoreLockLeaseDuration = 30 * time.Second
	restoreLockRenewDeadline = 10 * time.Second
	restoreLockRetryPeriod   = 2 * time.Second
)

// WithRestoreLock runs one PVC restore while holding a Kubernetes Lease for the target claim.
// The callback context is cancelled if lease renewal fails.
func WithRestoreLock(
	ctx context.Context,
	config *rest.Config,
	target Ref,
	restore func(context.Context) error,
) error {
	if ctx == nil {
		return errors.New("PVC restore lock context is required")
	}
	if config == nil {
		return errors.New("PVC restore lock Kubernetes config is required")
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("validate PVC restore lock target: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create PVC restore lock client: %w", err)
	}

	claim, err := client.CoreV1().PersistentVolumeClaims(target.Namespace).Get(
		ctx,
		target.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get PVC for restore lock: %w", err)
	}

	targetIdentity := TargetIdentity{
		UID:             string(claim.GetUID()),
		ResourceVersion: claim.GetResourceVersion(),
	}
	if targetIdentity.UID == "" || targetIdentity.ResourceVersion == "" {
		return fmt.Errorf("PVC %s/%s has incomplete Kubernetes identity", target.Namespace, target.Name)
	}

	return withRestoreLockClient(ctx, client, target, targetIdentity, restore)
}

// withRestoreLockClient is separated from the REST constructor for fake-client tests.
func withRestoreLockClient(
	ctx context.Context,
	client kubernetes.Interface,
	target Ref,
	identity TargetIdentity,
	restore func(context.Context) error,
) error {
	if ctx == nil || client == nil || restore == nil {
		return errors.New("PVC restore lock requires context, client, and callback")
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("validate PVC restore lock target: %w", err)
	}
	if identity.UID == "" || identity.ResourceVersion == "" {
		return errors.New("PVC restore lock target identity is incomplete")
	}

	name := restoreLockName(target, identity.UID)
	lockIdentity := restoreLockIdentity()
	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		target.Namespace,
		name,
		client.CoreV1(),
		client.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: lockIdentity},
	)
	if err != nil {
		return fmt.Errorf("create PVC restore Lease: %w", err)
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()

	started := make(chan struct{})
	result := make(chan error, 1)
	restoreDone := make(chan struct{})
	runDone := make(chan struct{})
	var startedOnce sync.Once
	var restoreDoneOnce sync.Once
	var stoppedOnce sync.Once
	callbacks := leaderelection.LeaderCallbacks{
		OnStartedLeading: func(leaderContext context.Context) {
			startedOnce.Do(func() { close(started) })
			err := validateRestoreLockIdentity(leaderContext, client, target, identity.UID)
			if err == nil {
				err = restore(leaderContext)
			}
			result <- err
			restoreDoneOnce.Do(func() { close(restoreDone) })
			cancel()
		},
		OnStoppedLeading: func() {
			stoppedOnce.Do(func() { close(runDone) })
		},
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   restoreLockLeaseDuration,
		RenewDeadline:   restoreLockRenewDeadline,
		RetryPeriod:     restoreLockRetryPeriod,
		Callbacks:       callbacks,
		ReleaseOnCancel: true,
		Name:            name,
	})
	if err != nil {
		return fmt.Errorf("configure PVC restore Lease: %w", err)
	}
	go func() {
		elector.Run(runContext)
		stoppedOnce.Do(func() { close(runDone) })
	}()

	select {
	case <-started:
	case <-runDone:
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("PVC restore Lease stopped before acquisition: %s", lock.Describe())

	case <-ctx.Done():
		<-runDone
		return ctx.Err()
	}

	select {
	case err := <-result:
		<-restoreDone
		<-runDone
		return err

	case <-ctx.Done():
		<-restoreDone
		<-runDone
		return ctx.Err()
	}
}

// validateRestoreLockIdentity rejects a PVC recreated between the initial lookup
// and acquisition of the Lease derived from that PVC's UID.
func validateRestoreLockIdentity(
	ctx context.Context,
	client kubernetes.Interface,
	target Ref,
	expectedUID string,
) error {
	claim, err := client.CoreV1().PersistentVolumeClaims(target.Namespace).Get(
		ctx,
		target.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("recheck PVC after restore Lease acquisition: %w", err)
	}
	if string(claim.GetUID()) != expectedUID {
		return fmt.Errorf(
			"target PVC %s/%s changed while acquiring the restore Lease",
			target.Namespace,
			target.Name,
		)
	}

	return nil
}

// restoreLockName keeps one stable, DNS-safe Lease name per PVC UID.
func restoreLockName(target Ref, uid string) string {
	digest := sha256.Sum256([]byte(target.Namespace + "\x00" + target.Name + "\x00" + uid))
	return "kube-dump-pvc-" + hex.EncodeToString(digest[:])[:32]
}

// restoreLockIdentity creates a unique process identity for Lease ownership.
func restoreLockIdentity() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "unknown-host"
	}

	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return hostname
	}

	return hostname + "/" + hex.EncodeToString(random)
}
