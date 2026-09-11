// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/export"
)

func TestInspectResourceObjectsUsesObjectKeysOnly(t *testing.T) {
	objects := []export.StoredObject{
		{Key: "backup/resources/apps/v1/deployments/prod/api.yaml", Size: 120},
		{Key: "backup/resources/core/v1/configmaps/prod/settings.yaml", Size: 80},
		{Key: "backup/.kube-dump/crypto/metadata.yaml", Size: 40},
		{Key: "backup/images/blob", Size: 500},
	}

	report := inspectResourceObjects("backup", objects)
	if report.Objects != 2 || report.Resources["apps/v1/deployments"] != 1 ||
		report.Resources["core/v1/configmaps"] != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestInspectPVCObjectsUsesS3ObjectSizes(t *testing.T) {
	objects := []export.StoredObject{
		{Key: "backup/volumes/prod/data/20260902T120000Z/data.tar.zst", Size: 1024},
		{Key: "backup/volumes/prod/encrypted/20260902T120001Z/data.tar.gz.age", Size: 512},
		{Key: "backup/volumes/prod/logs/20260902T120002Z/metadata.yaml", Size: 90},
		{Key: "backup/volumes/prod/logs/20260902T120002Z/snapshot.json", Size: 2048},
	}

	report := inspectPVCObjects("backup", objects)
	if report.PVCArtifacts != 3 || report.PVCBytes != 3584 {
		t.Fatalf("report = %#v", report)
	}
}
