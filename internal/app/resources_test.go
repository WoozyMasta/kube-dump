// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateCollectionResultRejectsEmptyWarningOnlyRun(t *testing.T) {
	warning := errors.New("Kubernetes API is unavailable")
	err := validateCollectionResult(kube.CollectionResult{Warnings: []error{warning}})
	if !errors.Is(err, warning) {
		t.Fatalf("validateCollectionResult() error = %v, want wrapped warning", err)
	}
}

func TestValidateCollectionResultAllowsPartialRun(t *testing.T) {
	warning := errors.New("one resource is forbidden")
	if err := validateCollectionResult(kube.CollectionResult{
		Warnings:  []error{warning},
		Collected: 1,
	}); err != nil {
		t.Fatalf("validateCollectionResult() rejected partial run: %v", err)
	}
}

func TestRequireCompleteResourceCollectionReportsOmittedWarnings(t *testing.T) {
	warnings := make([]error, 8)
	for index := range warnings {
		warnings[index] = errors.New("resource warning")
	}

	err := requireCompleteResourceCollection(kube.CollectionResult{
		Warnings:     warnings,
		WarningCount: 10,
	})
	if err == nil {
		t.Fatal("requireCompleteResourceCollection() accepted warnings")
	}
	if !strings.Contains(err.Error(), "warnings=10") {
		t.Fatalf("error = %v, want exact warning count", err)
	}
	if !strings.Contains(err.Error(), "2 warning details omitted") {
		t.Fatalf("error = %v, want omitted warning count", err)
	}
}

func TestResourceBackupCommitMessageContainsStableTrailers(t *testing.T) {
	message := resourceBackupCommitMessage(
		"backup",
		"prod\ncluster",
		kube.Selection{
			Namespaces:        []string{"zeta", "alpha"},
			ExcludeNamespaces: []string{"kube-system", "monitoring"},
		},
		fieldcrypto.AES256SIV,
		kube.CollectionResult{Collected: 12, Changed: 3},
		"v2.1.0",
	)

	want := "kube-dump: backup\n\n" + strings.Join([]string{
		"Kube-Dump-Profile: backup",
		"Kube-Dump-Context: prod cluster",
		"Kube-Dump-Namespaces: alpha,zeta",
		"Kube-Dump-Resource-Count: 12",
		"Kube-Dump-Changed: 3",
		"Kube-Dump-Field-Encryption: aes-siv",
		"Kube-Dump-Version: v2.1.0",
		"Kube-Dump-Excluded-Namespaces: kube-system,monitoring",
	}, "\n")
	if message != want {
		t.Fatalf("resourceBackupCommitMessage() = %q, want %q", message, want)
	}
}

func TestResourceBackupCommitMessageUsesAllNamespacesByDefault(t *testing.T) {
	message := resourceBackupCommitMessage(
		"backup",
		"",
		kube.Selection{},
		fieldcrypto.Plain,
		kube.CollectionResult{},
		"v2.1.0",
	)

	if !strings.Contains(message, "Kube-Dump-Namespaces: all") {
		t.Fatalf("resourceBackupCommitMessage() = %q, want all namespace scope", message)
	}
	if strings.Contains(message, "Kube-Dump-Context:") {
		t.Fatalf("resourceBackupCommitMessage() = %q, want no implicit context trailer", message)
	}
}

func TestResourceDirectorySinkWritesReadableObjectPath(t *testing.T) {
	root := t.TempDir()
	sink, err := export.NewDirectorySink(root)
	if err != nil {
		t.Fatal(err)
	}

	object, err := state.NewObject(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: "status", Name: "api",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "status",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := sink.Write(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write was reported unchanged")
	}

	path := filepath.Join(root, "apps", "v1", "deployments", "status", "api.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resource file was not written: %v", err)
	}

	changed, err = sink.Write(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical second write was reported changed")
	}
}

func TestFinalizeResourceTreeKeepsPreviousSnapshotAfterWarning(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "staging")
	destination := filepath.Join(root, "resources")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(destination, "old.yaml")
	if err := os.WriteFile(oldFile, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "new.yaml"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	warning := errors.New("one resource was forbidden")
	if err := finalizeResourceTree(source, destination, kube.CollectionResult{Warnings: []error{warning}}); err == nil {
		t.Fatal("finalizeResourceTree() accepted an incomplete collection")
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("previous resource snapshot was changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "new.yaml")); !os.IsNotExist(err) {
		t.Fatalf("incomplete resource snapshot was published, stat error = %v", err)
	}
}

func TestResourceFieldEncryptionDefaultsToPlainWithoutRecipients(t *testing.T) {
	options, err := (ResourceOptions{}).fieldEncryptionOptions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if options.Mode != fieldcrypto.Plain {
		t.Fatalf("field encryption mode = %q, want plain", options.Mode)
	}
}

func TestResourceFieldEncryptionAES256SIVCreatesKeyring(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	identityPath := filepath.Join(root, "identity")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	options, err := (ResourceOptions{
		FieldEncryption: fieldcrypto.AES256SIV,
		Recipients:      []string{identity.Recipient().String()},
		IdentityOptions: IdentityOptions{
			IdentityFiles: []string{identityPath},
		},
	}).fieldEncryptionOptions(root)
	if err != nil {
		t.Fatal(err)
	}
	if options.Mode != fieldcrypto.AES256SIV || options.SIVCipher == nil || options.SIVKeyID == "" {
		t.Fatalf("incomplete AES-SIV options: %#v", options)
	}
	if _, err := os.Stat(filepath.Join(root, ".kube-dump", "crypto", "metadata.yaml")); err != nil {
		t.Fatalf("AES-SIV keyring metadata was not created: %v", err)
	}
}

func TestResourceFieldEncryptionAES256SIVRejectsMismatchedIdentity(t *testing.T) {
	recipientIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	wrongIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	identityPath := filepath.Join(root, "identity")
	if err := os.WriteFile(identityPath, []byte(wrongIdentity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = (ResourceOptions{
		FieldEncryption: fieldcrypto.AES256SIV,
		Recipients:      []string{recipientIdentity.Recipient().String()},
		IdentityOptions: IdentityOptions{
			IdentityFiles: []string{identityPath},
		},
	}).fieldEncryptionOptions(root)
	if err == nil {
		t.Fatal("fieldEncryptionOptions() accepted mismatched recipient and identity")
	}
	if !strings.Contains(err.Error(), "no supplied identity matches") {
		t.Fatalf("fieldEncryptionOptions() error = %q, want mismatch guidance", err)
	}
}
