// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package keyring

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestOpenCreatesAndReopensEncryptedKeyring(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	created, err := Open(root, []age.Recipient{identity.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, cipher, err := created.Active()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt([]byte("secret"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 16 || len(ciphertext) == 0 {
		t.Fatalf("invalid active key result: id=%q ciphertext=%x", id, ciphertext)
	}

	keyFile := filepath.Join(root, ".kube-dump", "crypto", "keys", "siv-"+id+".age")
	data, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(data, []byte("KDSIV")) {
		t.Fatal("key envelope contains the plaintext key record")
	}

	reopened, err := Open(root, nil, []age.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	reopenedID, reopenedCipher, err := reopened.Active()
	if err != nil {
		t.Fatal(err)
	}
	if reopenedID != id {
		t.Fatalf("active key ID changed: got %q, want %q", reopenedID, id)
	}
	plain, err := reopenedCipher.Decrypt(ciphertext, []byte("aad"))
	if err != nil || string(plain) != "secret" {
		t.Fatalf("reopened cipher decrypt = %q, %v", plain, err)
	}
}

func TestOpenRejectsExistingKeyringWithoutIdentity(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := Open(root, []age.Recipient{identity.Recipient()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, []age.Recipient{identity.Recipient()}, nil); err == nil {
		t.Fatal("Open() accepted existing keyring without an age identity")
	}
}

func TestOpenExistingRejectsUninitializedDirectory(t *testing.T) {
	if _, err := OpenExisting(t.TempDir(), nil); err == nil {
		t.Fatal("OpenExisting() accepted an uninitialized directory")
	}
}

func TestOpenExistingLoadsInitializedKeyring(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	created, err := Open(root, []age.Recipient{identity.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _, err := created.Active()
	if err != nil {
		t.Fatal(err)
	}

	opened, err := OpenExisting(root, []age.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := opened.Active()
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("OpenExisting() active key ID = %q, want %q", gotID, wantID)
	}
}

func TestOpenRejectsOrphanedKeyEnvelope(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".kube-dump", "crypto", "keys")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "siv-orphan.age"), []byte("age-encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, []age.Recipient{identity.Recipient()}, nil); err == nil {
		t.Fatal("Open() regenerated a key with orphaned key material")
	}
}

func TestOpenDoesNotRegenerateWhenIdentityCannotDecrypt(t *testing.T) {
	owner, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	created, err := Open(root, []age.Recipient{owner.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _, err := created.Active()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, []age.Recipient{other.Recipient()}, []age.Identity{other}); err == nil {
		t.Fatal("Open() accepted an unauthorized age identity")
	}
	reopened, err := Open(root, nil, []age.Identity{owner})
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := reopened.Active()
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("key ID changed after failed open: got %q, want %q", gotID, wantID)
	}
}

func TestOpenRejectsBrokenHistoricalKey(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	created, err := Open(root, []age.Recipient{identity.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID, _, err := created.Active()
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Rotate([]age.Recipient{identity.Recipient()}); err != nil {
		t.Fatal(err)
	}

	oldPath := filepath.Join(root, ".kube-dump", "crypto", "keys", "siv-"+oldID+".age")
	if err := os.WriteFile(oldPath, []byte("broken envelope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, nil, []age.Identity{identity}); err == nil {
		t.Fatal("Open() accepted a broken historical key envelope")
	}
}

func TestRewrapPreservesActiveKeyAndRotateKeepsOldKey(t *testing.T) {
	firstIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	keyring, err := Open(root, []age.Recipient{firstIdentity.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldCipher, err := keyring.Active()
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Rewrap([]age.Recipient{secondIdentity.Recipient()}, []age.Identity{firstIdentity}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, nil, []age.Identity{secondIdentity})
	if err != nil {
		t.Fatal(err)
	}
	rewrappedID, rewrappedCipher, err := reopened.Active()
	if err != nil {
		t.Fatal(err)
	}
	if rewrappedID != oldID {
		t.Fatalf("rewrap changed key ID: got %q, want %q", rewrappedID, oldID)
	}
	oldPayload, err := oldCipher.Encrypt([]byte("old"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewrappedCipher.Decrypt(oldPayload, []byte("aad")); err != nil {
		t.Fatalf("rewrapped key cannot decrypt old payload: %v", err)
	}
	if err := reopened.Rotate([]age.Recipient{secondIdentity.Recipient()}); err != nil {
		t.Fatal(err)
	}
	newID, _, err := reopened.Active()
	if err != nil {
		t.Fatal(err)
	}
	if newID == oldID || len(reopened.IDs()) != 2 {
		t.Fatalf("rotation result: active=%q IDs=%v", newID, reopened.IDs())
	}
	if _, err := reopened.Cipher(oldID, []age.Identity{secondIdentity}); err != nil {
		t.Fatalf("historical key is not available after rotation: %v", err)
	}
	if err := reopened.Rewrap([]age.Recipient{secondIdentity.Recipient()}, []age.Identity{secondIdentity}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Cipher(oldID, []age.Identity{secondIdentity}); err != nil {
		t.Fatalf("rewrapped historical key cannot be opened: %v", err)
	}
}

func FuzzDecodeEnvelope(f *testing.F) {
	seed, err := encodeEnvelope("0011223344556677", make([]byte, 64))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		id, key, err := decodeEnvelope(data)
		if err != nil {
			return
		}

		encoded, err := encodeEnvelope(id, key)
		if err != nil {
			t.Fatalf("encodeEnvelope() error after successful decode: %v", err)
		}
		if !bytes.Equal(encoded, data) {
			t.Fatal("key envelope changed after decode/encode")
		}
	})
}

func FuzzEncodeEnvelopeRoundTrip(f *testing.F) {
	f.Add("0011223344556677", make([]byte, 64))
	f.Add("deadbeef", []byte("invalid key length"))

	f.Fuzz(func(t *testing.T, id string, key []byte) {
		encoded, err := encodeEnvelope(id, key)
		if err != nil {
			return
		}

		decodedID, decodedKey, err := decodeEnvelope(encoded)
		if err != nil {
			t.Fatalf("decodeEnvelope() rejected its own encoding: %v", err)
		}
		if decodedID != id || !bytes.Equal(decodedKey, key) {
			t.Fatalf("envelope round-trip changed data: id=%q key=%x", decodedID, decodedKey)
		}
	})
}
