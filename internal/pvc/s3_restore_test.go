// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestS3RestorerHelperOptionsBuildDirectDownloadCommand(t *testing.T) {
	t.Parallel()

	restorer := &S3Restorer{options: S3RestorerOptions{
		Image:          "example/kube-dump:test",
		AgentBinary:    "/bin/kube-dump",
		URI:            "s3://bucket/backup",
		Endpoint:       "http://s3.example.test",
		Insecure:       true,
		Region:         "us-east-1",
		Object:         "backup/volumes/prod/data/20260904T010203Z/data.tar.gz",
		Compression:    compress.Gzip,
		Existing:       ExistingEmptyOnly,
		ExpectedSize:   1234,
		ExpectedSHA256: "digest",
		Encrypted:      true,
	}}
	options := restorer.helperOptions("credentials", "identity", "passphrase")
	object := helperPodObject(Ref{Namespace: "prod", Name: "data"}, "kube-dump-restorer-", options)

	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("read restorer containers: %#v, found=%t error=%v", containers, found, err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("restorer container has unexpected type %T", containers[0])
	}
	command, found, err := unstructured.NestedStringSlice(container, "command")
	if err != nil || !found {
		t.Fatalf("read restorer command: found=%t error=%v", found, err)
	}
	want := []string{
		"/bin/kube-dump",
		"volume",
		"download",
		"s3://bucket/backup",
		"backup/volumes/prod/data/20260904T010203Z/data.tar.gz",
		"/data",
		"empty-only",
		"--compression=gzip",
		"--expected-size=1234",
		"--expected-sha256=digest",
		"--encrypted",
		"--s3-endpoint=http://s3.example.test",
		"--s3-insecure",
		"--s3-region=us-east-1",
	}
	if len(command) != len(want) {
		t.Fatalf("restorer command = %#v, want %#v", command, want)
	}
	for index := range want {
		if command[index] != want[index] {
			t.Fatalf("restorer command = %#v, want %#v", command, want)
		}
	}

	envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
	if err != nil || !found || len(envFrom) != 2 {
		t.Fatalf("restorer envFrom = %#v, found=%t error=%v", envFrom, found, err)
	}
	volumes, found, err := unstructured.NestedSlice(object.Object, "spec", "volumes")
	if err != nil || !found || len(volumes) != 3 {
		t.Fatalf("restorer volumes = %#v, found=%t error=%v", volumes, found, err)
	}
}
