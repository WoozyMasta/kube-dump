package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
)

func TestArchiveAlgorithmDetectsUncompressedTar(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		path      string
		algorithm compress.Algorithm
		encrypted bool
	}{
		{path: "data.tar", algorithm: compress.None},
		{path: "data.tar.age", algorithm: compress.None, encrypted: true},
	} {
		algorithm, encrypted, err := archiveAlgorithm(test.path)
		if err != nil {
			t.Fatalf("archiveAlgorithm(%q) error = %v", test.path, err)
		}
		if algorithm != test.algorithm || encrypted != test.encrypted {
			t.Errorf("archiveAlgorithm(%q) = (%q, %t), want (%q, %t)",
				test.path, algorithm, encrypted, test.algorithm, test.encrypted)
		}
	}
}

func TestReadLimitedRejectsOversizedObject(t *testing.T) {
	t.Parallel()

	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("readLimited() accepted an oversized object")
	}
}

func TestCopyLimitedRejectsOversizedObject(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := copyLimited(&output, strings.NewReader("12345"), 4); err == nil {
		t.Fatal("copyLimited() accepted an oversized object")
	}
	if output.Len() != 5 {
		t.Fatalf("copyLimited() wrote %d bytes before rejecting the object", output.Len())
	}
}
