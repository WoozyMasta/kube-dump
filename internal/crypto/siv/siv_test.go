// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package siv

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAcceptsOnlyAES256SIVKey(t *testing.T) {
	for _, size := range []int{32, 48, 64} {
		cipher, err := New(make([]byte, size))
		if size == KeySize {
			if err != nil || cipher == nil {
				t.Fatalf("New(%d) = %v, %v", size, cipher, err)
			}
			continue
		}
		if err == nil || cipher != nil {
			t.Fatalf("New(%d) accepted unsupported key size", size)
		}
	}
}

func TestAES256SIVCompatibilityVector(t *testing.T) {
	key := readGoldenHex(t, "key.hex")
	aad := readGoldenHex(t, "aad.hex")
	plaintext := readGoldenFile(t, "plaintext.txt")
	want := readGoldenHex(t, "ciphertext.hex")
	wantPayload := strings.TrimSpace(string(readGoldenFile(t, "payload.txt")))

	cipher, err := New(key)
	if err != nil {
		t.Fatal(err)
	}

	got, err := cipher.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext = %x, want %x", got, want)
	}
	payload, err := EncodePayload("0011223344556677", got)
	if err != nil {
		t.Fatalf("EncodePayload() error: %v", err)
	}
	if payload != wantPayload {
		t.Fatalf("payload = %q, want %q", payload, wantPayload)
	}
	keyID, decoded, err := DecodePayload(payload)
	if err != nil || keyID != "0011223344556677" || !bytes.Equal(decoded, want) {
		t.Fatalf("DecodePayload() = %q, %x, %v", keyID, decoded, err)
	}

	plain, err := cipher.Decrypt(got, aad)
	if err != nil || !bytes.Equal(plain, plaintext) {
		t.Fatalf("Decrypt() = %q, %v", plain, err)
	}

	modifiedCiphertext := append([]byte(nil), got...)
	modifiedCiphertext[0] ^= 1
	if _, err := cipher.Decrypt(modifiedCiphertext, aad); err == nil {
		t.Fatal("Decrypt() accepted modified golden ciphertext")
	}
	modifiedAAD := append([]byte(nil), aad...)
	modifiedAAD[0] ^= 1
	if _, err := cipher.Decrypt(got, modifiedAAD); err == nil {
		t.Fatal("Decrypt() accepted modified golden AAD")
	}
}

func TestDecodePayloadRejectsUnsupportedVersion(t *testing.T) {
	unsupported := "v2:0011223344556677:eAXGe17QsG_q6E_jMlrmpzYaDSYHlftNseZeuyaC"
	if _, _, err := DecodePayload(unsupported); err == nil {
		t.Fatal("DecodePayload() accepted an unsupported payload version")
	}
}

func readGoldenFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}

	return data
}

func readGoldenHex(t *testing.T, name string) []byte {
	t.Helper()
	data := strings.TrimSpace(string(readGoldenFile(t, name)))
	decoded, err := hex.DecodeString(data)
	if err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}

	return decoded
}

func FuzzDecodePayload(f *testing.F) {
	cipher, err := New(make([]byte, KeySize))
	if err != nil {
		f.Fatal(err)
	}

	ciphertext, err := cipher.Encrypt([]byte("seed"), []byte("aad"))
	if err != nil {
		f.Fatal(err)
	}

	seed, err := EncodePayload("0011223344556677", ciphertext)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(seed)
	f.Fuzz(func(t *testing.T, payload string) {
		keyID, ciphertext, err := DecodePayload(payload)
		if err != nil {
			return
		}

		encoded, err := EncodePayload(keyID, ciphertext)
		if err != nil {
			t.Fatalf("EncodePayload() error after successful decode: %v", err)
		}

		redecodedKeyID, redecodedCiphertext, err := DecodePayload(encoded)
		if err != nil {
			t.Fatalf("DecodePayload() error after re-encoding: %v", err)
		}
		if redecodedKeyID != keyID || !bytes.Equal(redecodedCiphertext, ciphertext) {
			t.Fatal("payload bytes changed after decode/encode/decode")
		}
	})
}

func FuzzEncryptDecrypt(f *testing.F) {
	f.Add([]byte("seed plaintext"), []byte("seed aad"))
	f.Add([]byte{}, []byte{})

	cipher, err := New(make([]byte, KeySize))
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, plaintext, aad []byte) {
		const maxFuzzInput = 64 << 10
		if len(plaintext) > maxFuzzInput || len(aad) > maxFuzzInput {
			return
		}

		ciphertext, err := cipher.Encrypt(plaintext, aad)
		if err != nil {
			t.Fatalf("Encrypt() error: %v", err)
		}

		decrypted, err := cipher.Decrypt(ciphertext, aad)
		if err != nil {
			t.Fatalf("Decrypt() rejected authenticated ciphertext: %v", err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("Decrypt() = %x, want %x", decrypted, plaintext)
		}

		modifiedCiphertext := append([]byte(nil), ciphertext...)
		modifiedCiphertext[0] ^= 1
		if _, err := cipher.Decrypt(modifiedCiphertext, aad); err == nil {
			t.Fatal("Decrypt() accepted modified ciphertext")
		}

		modifiedAAD := append([]byte(nil), aad...)
		if len(modifiedAAD) == 0 {
			modifiedAAD = []byte{1}
		} else {
			modifiedAAD[0] ^= 1
		}
		if _, err := cipher.Decrypt(ciphertext, modifiedAAD); err == nil {
			t.Fatal("Decrypt() accepted modified associated data")
		}
	})
}
