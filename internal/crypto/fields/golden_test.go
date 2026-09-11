// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fields_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	filippoage "filippo.io/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const goldenFieldKeyID = "0011223344556677"

func TestGoldenInlineFieldRoundTrip(t *testing.T) {
	plain := goldenObject(t, "plain.yaml")
	identity := goldenIdentity(t)
	key := goldenKey(t)
	cipher, err := siv.New(key)
	if err != nil {
		t.Fatalf("create golden AES-SIV cipher: %v", err)
	}

	tests := []struct {
		name         string
		fixture      string
		encryptedTag string
		decryptedTag string
		options      fieldcrypto.Options
		resolver     fieldcrypto.SIVKeyResolver
	}{
		{
			name:         "age",
			fixture:      "age.yaml",
			encryptedTag: "age",
			decryptedTag: "age-decrypted",
			options: fieldcrypto.Options{
				Mode:       fieldcrypto.Age,
				Recipients: []filippoage.Recipient{identity.Recipient()},
			},
		},
		{
			name:         "aes-siv",
			fixture:      "aes-siv.yaml",
			encryptedTag: "aes-siv",
			decryptedTag: "aes-siv-decrypted",
			options: fieldcrypto.Options{
				Mode:      fieldcrypto.AES256SIV,
				SIVKeyID:  goldenFieldKeyID,
				SIVCipher: cipher,
			},
			resolver: goldenSIVResolver{keyID: goldenFieldKeyID, cipher: cipher},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encryptedData, err := os.ReadFile(filepath.Join("testdata", "golden", test.fixture))
			if err != nil {
				t.Fatalf("read %s fixture: %v", test.name, err)
			}

			encrypted := goldenObjectData(t, encryptedData)
			restored, err := fieldcrypto.RestoreFields(encrypted, []filippoage.Identity{identity}, test.resolver)
			if err != nil {
				t.Fatalf("restore %s fixture: %v", test.name, err)
			}
			if !reflect.DeepEqual(restored.Value.Object, plain.Value.Object) {
				t.Fatal("restored fixture differs from the plaintext object")
			}

			fresh, err := fieldcrypto.EncryptFields(
				plain,
				[][]string{{"spec", "password"}},
				test.options,
			)
			if err != nil {
				t.Fatalf("encrypt plaintext with %s: %v", test.name, err)
			}

			freshValue, found, err := unstructured.NestedFieldNoCopy(
				fresh.Value.Object,
				"spec",
				"password",
			)
			if err != nil || !found {
				t.Fatalf("read freshly encrypted %s field: found=%v error=%v", test.name, found, err)
			}

			freshTag, _, ok := fieldcrypto.TaggedValueParts(freshValue)
			if !ok || freshTag != test.encryptedTag {
				t.Fatalf("fresh %s field marker = %q, want %q", test.name, freshTag, test.encryptedTag)
			}
			freshRestored, err := fieldcrypto.RestoreFields(
				fresh,
				[]filippoage.Identity{identity},
				test.resolver,
			)
			if err != nil {
				t.Fatalf("restore freshly encrypted %s field: %v", test.name, err)
			}
			if !reflect.DeepEqual(freshRestored.Value.Object, plain.Value.Object) {
				t.Fatal("fresh encryption did not preserve the plaintext object")
			}

			decrypted, err := fieldcrypto.DecryptFields(encrypted, []filippoage.Identity{identity}, test.resolver)
			if err != nil {
				t.Fatalf("decrypt %s fixture: %v", test.name, err)
			}
			decryptedValue, found, err := unstructured.NestedFieldNoCopy(
				decrypted.Value.Object,
				"spec",
				"password",
			)
			if err != nil || !found {
				t.Fatalf("read decrypted %s field: found=%v error=%v", test.name, found, err)
			}

			decryptedTag, _, ok := fieldcrypto.TaggedValueParts(decryptedValue)
			if !ok || decryptedTag != test.decryptedTag {
				t.Fatalf("decrypted %s field marker = %q, want %q", test.name, decryptedTag, test.decryptedTag)
			}

			reencrypted, err := fieldcrypto.EncryptDecryptedFields(decrypted, test.options)
			if err != nil {
				t.Fatalf("reencrypt %s fixture: %v", test.name, err)
			}

			reencryptedRestored, err := fieldcrypto.RestoreFields(
				reencrypted,
				[]filippoage.Identity{identity},
				test.resolver,
			)
			if err != nil {
				t.Fatalf("restore reencrypted %s fixture: %v", test.name, err)
			}
			if !reflect.DeepEqual(reencryptedRestored.Value.Object, plain.Value.Object) {
				t.Fatal("decrypt → encrypt cycle changed the plaintext object")
			}

			if test.name == "aes-siv" {
				canonical, err := codec.Marshal(reencrypted)
				if err != nil {
					t.Fatalf("marshal reencrypted AES-SIV fixture: %v", err)
				}
				if !bytes.Equal(canonical, encryptedData) {
					t.Fatal("AES-SIV fixture changed after decrypt → encrypt cycle")
				}
			}
		})
	}
}

func goldenObject(t *testing.T, name string) state.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatalf("read %s fixture: %v", name, err)
	}

	return goldenObjectData(t, data)
}

func goldenObjectData(t *testing.T, data []byte) state.Object {
	t.Helper()
	value, err := codec.Unmarshal(data)
	if err != nil {
		t.Fatalf("decode golden object: %v", err)
	}

	object, err := state.NewObject(state.Identity{
		Group:     "database.example.io",
		Version:   "v1",
		Resource:  "clusters",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "database",
	}, value)
	if err != nil {
		t.Fatalf("create golden object: %v", err)
	}

	return object
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

func goldenKey(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "golden", "aes-siv-key.hex"))
	if err != nil {
		t.Fatalf("read golden AES-SIV key: %v", err)
	}

	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("decode golden AES-SIV key: %v", err)
	}

	return key
}

type goldenSIVResolver struct {
	keyID  string
	cipher *siv.Cipher
}

func (r goldenSIVResolver) Cipher(id string, _ []filippoage.Identity) (*siv.Cipher, error) {
	if id != r.keyID {
		return nil, errors.New("unexpected golden AES-SIV key ID")
	}

	return r.cipher, nil
}

var _ fieldcrypto.SIVKeyResolver = goldenSIVResolver{}
