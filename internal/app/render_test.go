package app

import (
	"bytes"
	"strings"
	"testing"
)

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
