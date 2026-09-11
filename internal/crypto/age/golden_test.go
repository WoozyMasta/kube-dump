// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package age

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filippoage "filippo.io/age"
)

func TestGoldenFileCompatibility(t *testing.T) {
	identity := goldenIdentity(t)
	plain, err := os.ReadFile(filepath.Join("..", "testdata", "golden", "file", "plain.txt"))
	if err != nil {
		t.Fatalf("read golden plaintext: %v", err)
	}
	encrypted, err := os.ReadFile(filepath.Join("..", "testdata", "golden", "file", "encrypted.age"))
	if err != nil {
		t.Fatalf("read golden ciphertext: %v", err)
	}

	reader, err := OpenDecryptReader(bytes.NewReader(encrypted), []filippoage.Identity{identity})
	if err != nil {
		t.Fatalf("open golden age file: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read golden age file: %v", err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatalf("golden age plaintext = %q, want %q", decoded, plain)
	}

	var roundTrip bytes.Buffer
	writer, err := NewEncryptWriter(
		&roundTrip,
		[]filippoage.Recipient{identity.Recipient()},
		EncryptOptions{Armor: true},
	)
	if err != nil {
		t.Fatalf("create age encryptor: %v", err)
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatalf("write age plaintext: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close age encryptor: %v", err)
	}

	reader, err = OpenDecryptReader(bytes.NewReader(roundTrip.Bytes()), []filippoage.Identity{identity})
	if err != nil {
		t.Fatalf("open round-trip age file: %v", err)
	}
	decoded, err = io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read round-trip age file: %v", err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatalf("round-trip age plaintext = %q, want %q", decoded, plain)
	}
}

func goldenIdentity(t *testing.T) *filippoage.X25519Identity {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "golden", "identity.txt"))
	if err != nil {
		t.Fatalf("read golden identity: %v", err)
	}

	identity, err := filippoage.ParseX25519Identity(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse golden identity: %v", err)
	}

	return identity
}
