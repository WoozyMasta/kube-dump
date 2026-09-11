package pvc

import (
	"errors"
	"reflect"
	"testing"
)

func TestMetadataValidation(t *testing.T) {
	t.Parallel()

	metadata := NewMetadata(Pod, "zstd")
	metadata.SizeBytes = 0
	metadata.ContentSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if err := metadata.Validate(); err != nil {
		t.Fatalf("Metadata.Validate() error = %v", err)
	}
}

func TestPVCRefValidationRejectsPathComponents(t *testing.T) {
	t.Parallel()

	if err := (Ref{Namespace: "default", Name: "data/volume"}).Validate(); err == nil {
		t.Fatal("Ref.Validate() accepted a path separator")
	}
}

func TestStrategyValidation(t *testing.T) {
	t.Parallel()

	if err := Strategy("unknown").Validate(); err == nil {
		t.Fatal("Strategy.Validate() accepted an unknown strategy")
	}
}

func TestIsCleanupOnly(t *testing.T) {
	t.Parallel()

	warning := &CleanupWarning{Resource: "helper Pod default/helper", Err: errors.New("forbidden")}
	if !IsCleanupOnly(warning) {
		t.Fatal("IsCleanupOnly() = false, want true for cleanup warning")
	}
	if IsCleanupOnly(errors.Join(warning, errors.New("capture failed"))) {
		t.Fatal("IsCleanupOnly() = true, want false for mixed failures")
	}
}

func TestTemporaryResourceLabels(t *testing.T) {
	t.Parallel()

	labels := temporaryResourceLabels(Ref{Namespace: "prod", Name: "database"}, "")
	want := map[string]any{
		"app.kubernetes.io/managed-by": "kube-dump",
		"kube-dump/source-namespace":   "prod",
		"kube-dump/source-pvc":         "database",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("temporaryResourceLabels() = %#v, want %#v", labels, want)
	}
}
