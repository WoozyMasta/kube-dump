package fields

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFieldAADIsLengthPrefixedAndIdentityBound(t *testing.T) {
	identity := state.Identity{
		Version:   "v1",
		Resource:  "clusters",
		Kind:      "Cluster",
		Namespace: "prod",
		Name:      "db",
	}
	first, err := fieldAAD(identity, []string{"spec", "password"})
	if err != nil {
		t.Fatal(err)
	}

	second, err := fieldAAD(identity, []string{"spec", "user"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || len(first) < 4 {
		t.Fatalf("AAD values are not distinct or length-prefixed: %x %x", first, second)
	}
	if got := first[:4]; !bytes.Equal(got, []byte{0, 0, 0, 18}) {
		t.Fatalf("AAD domain prefix = %x", got)
	}
}

func TestFieldAADDistinguishesDottedKeysFromNestedPaths(t *testing.T) {
	t.Parallel()
	identity := state.Identity{
		Version:   "v1",
		Resource:  "clusters",
		Kind:      "Cluster",
		Namespace: "prod",
		Name:      "db",
	}

	dotted, err := fieldAAD(identity, []string{"data", "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := fieldAAD(identity, []string{"data", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(dotted, nested) {
		t.Fatal("field AAD aliases a dotted key with a nested path")
	}
}

func TestSecretDataRoundTripUsesDecodedBytes(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	object := newSecretObject(t, map[string]any{"password": "//79"})
	object.Value.Object["stringData"] = map[string]any{"password": "plain"}

	encrypted, err := EncryptFields(object, [][]string{{"data", "*"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err != nil {
		t.Fatalf("EncryptFields() error = %v", err)
	}
	if _, found, _ := unstructured.NestedString(encrypted.Value.Object, "data", "password"); found {
		t.Fatal("Secret.data.password remained plaintext")
	}
	if value, found, _ := unstructured.NestedString(encrypted.Value.Object, "stringData", "password"); !found || value != "plain" {
		t.Fatalf("Secret.stringData.password changed: %q, found=%v", value, found)
	}

	marker, _, _ := unstructured.NestedFieldNoCopy(encrypted.Value.Object, "data", "password")
	decryptedBytes, err := DecryptAgeValue(marker, []age.Identity{identity})
	if err != nil {
		t.Fatalf("DecryptAgeValue() error = %v", err)
	}
	if !bytes.Equal(decryptedBytes, []byte{0xff, 0xfe, 0xfd}) {
		t.Fatalf("encrypted Secret.data plaintext = %x, want fffefd", decryptedBytes)
	}

	restored, err := RestoreFields(encrypted, []age.Identity{identity})
	if err != nil {
		t.Fatalf("RestoreFields() error = %v", err)
	}

	value, found, err := unstructured.NestedString(restored.Value.Object, "data", "password")
	if err != nil || !found || value != "//79" {
		t.Fatalf("restored Secret.data.password = %q, found=%v, error=%v", value, found, err)
	}
}

func TestSecretDataDecryptEncryptRoundTripForBothAlgorithms(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, siv.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := siv.New(key)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		options    Options
		resolver   SIVKeyResolver
		identities []age.Identity
		tag        string
	}{
		{
			name: "age",
			options: Options{
				Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
			},
			identities: []age.Identity{identity},
			tag:        ageDecryptedTag,
		},
		{
			name: "aes-siv",
			options: Options{
				Mode:      AES256SIV,
				SIVKeyID:  "0123456789abcdef",
				SIVCipher: cipher,
			},
			resolver: testSIVResolver{keyID: "0123456789abcdef", cipher: cipher},
			tag:      sivDecryptedTag,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := newSecretObject(t, map[string]any{"password": "//79"})
			encrypted, err := EncryptFields(object, [][]string{{"data", "*"}}, test.options)
			if err != nil {
				t.Fatalf("EncryptFields() error = %v", err)
			}

			decrypted, err := DecryptFields(encrypted, test.identities, test.resolver)
			if err != nil {
				t.Fatalf("DecryptFields() error = %v", err)
			}
			value, found, err := unstructured.NestedFieldNoCopy(
				decrypted.Value.Object, "data", "password",
			)
			if err != nil || !found {
				t.Fatalf("read decrypted Secret.data.password: found=%v error=%v", found, err)
			}
			marker, payload, ok := TaggedValueParts(value)
			if !ok || marker != test.tag || payload != "//79" {
				t.Fatalf("decrypted Secret.data marker = %q %q, want %q //79", marker, payload, test.tag)
			}

			reencrypted, err := EncryptDecryptedFields(decrypted, test.options)
			if err != nil {
				t.Fatalf("EncryptDecryptedFields() error = %v", err)
			}
			restored, err := RestoreFields(reencrypted, test.identities, test.resolver)
			if err != nil {
				t.Fatalf("RestoreFields() error = %v", err)
			}
			value, found, err = unstructured.NestedString(
				restored.Value.Object, "data", "password",
			)
			if err != nil || !found || value != "//79" {
				t.Fatalf("restored Secret.data.password = %q, found=%v error=%v", value, found, err)
			}
		})
	}
}

func TestSecretDataRejectsInvalidBase64(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	object := newSecretObject(t, map[string]any{"password": "not-base64"})

	_, err = EncryptFields(object, [][]string{{"data", "password"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err == nil {
		t.Fatal("EncryptFields() accepted invalid Secret.data Base64")
	}
}

func TestNonSecretDataFieldKeepsGenericEncoding(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	object := newTestObject(t, map[string]any{
		"data": map[string]any{"value": "//79"},
	})

	encrypted, err := EncryptFields(object, [][]string{{"spec", "data", "value"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err != nil {
		t.Fatalf("EncryptFields() error = %v", err)
	}
	decrypted, err := DecryptFields(encrypted, []age.Identity{identity})
	if err != nil {
		t.Fatalf("DecryptFields() error = %v", err)
	}
	_, payload, ok := TaggedValueParts(decrypted.Value.Object["spec"].(map[string]any)["data"].(map[string]any)["value"])
	if !ok || payload != `"//79"` {
		t.Fatalf("generic data marker payload = %q, want %q", payload, `"//79"`)
	}
}

func TestAES256SIVWildcardRoundTrip(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, siv.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := siv.New(key)
	if err != nil {
		t.Fatal(err)
	}

	object := newTestObject(t, nil)
	object.Identity = state.Identity{
		Version:   "v1",
		Resource:  "secrets",
		Kind:      "Secret",
		Namespace: "default",
		Name:      "credentials",
	}
	object.Value.Object["apiVersion"] = "v1"
	object.Value.Object["kind"] = "Secret"
	object.Value.Object["metadata"].(map[string]any)["name"] = "credentials"
	object.Value.Object["data"] = map[string]any{"password": "c2VjcmV0"}

	encrypted, err := EncryptFields(object, [][]string{{"data", "*"}}, Options{
		Mode: AES256SIV, SIVKeyID: "0123456789abcdef", SIVCipher: cipher,
	})
	if err != nil {
		t.Fatalf("EncryptFields() error = %v", err)
	}

	restored, err := RestoreFields(encrypted, []age.Identity{identity}, testSIVResolver{
		keyID: "0123456789abcdef", cipher: cipher,
	})
	if err != nil {
		t.Fatalf("RestoreFields() error = %v", err)
	}
	value, found, err := unstructured.NestedString(restored.Value.Object, "data", "password")
	if err != nil || !found || value != "c2VjcmV0" {
		t.Fatalf("restored wildcard field = %q, found=%v, error=%v", value, found, err)
	}
}

func TestAES256SIVBindsCiphertextToFieldIdentity(t *testing.T) {
	key := make([]byte, siv.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	cipher, err := siv.New(key)
	if err != nil {
		t.Fatal(err)
	}

	options := Options{Mode: AES256SIV, SIVKeyID: "0123456789abcdef", SIVCipher: cipher}
	object := newTestObject(t, map[string]any{"password": "secret"})
	first, err := EncryptFields(object, [][]string{{"spec", "password"}}, options)
	if err != nil {
		t.Fatalf("first EncryptFields() error = %v", err)
	}

	second, err := EncryptFields(object, [][]string{{"spec", "password"}}, options)
	if err != nil {
		t.Fatalf("second EncryptFields() error = %v", err)
	}

	firstValue, _, _ := unstructured.NestedFieldNoCopy(first.Value.Object, "spec", "password")
	secondValue, _, _ := unstructured.NestedFieldNoCopy(second.Value.Object, "spec", "password")
	if !bytes.Equal(mustJSON(t, firstValue), mustJSON(t, secondValue)) {
		t.Fatal("AES-SIV encryption is not deterministic for the same field")
	}

	wrongIdentity := first
	wrongIdentity.Identity.Name = "other"
	_, err = RestoreFields(wrongIdentity, nil, testSIVResolver{keyID: options.SIVKeyID, cipher: cipher})
	if err == nil {
		t.Fatal("AES-SIV ciphertext restored with a different object identity")
	}
}

func TestEncryptFieldsDoesNotEncryptOverlappingPathTwice(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	object := newTestObject(t, map[string]any{"password": "secret"})

	encrypted, err := EncryptFields(object, [][]string{{"spec", "*"}, {"spec", "password"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err != nil {
		t.Fatalf("EncryptFields() error = %v", err)
	}

	restored, err := RestoreFields(encrypted, []age.Identity{identity})
	if err != nil {
		t.Fatalf("RestoreFields() error = %v", err)
	}

	password, found, err := unstructured.NestedString(restored.Value.Object, "spec", "password")
	if err != nil || !found || password != "secret" {
		t.Fatalf("restored password = %q, found=%v, error=%v", password, found, err)
	}
}

func TestEncryptFieldsHandlesDisabledAndMissingFields(t *testing.T) {
	object := newTestObject(t, map[string]any{"password": "secret"})

	_, err := EncryptFields(object, [][]string{{"spec", "password"}}, Options{Mode: Plain})
	if err != nil {
		t.Fatalf("plain EncryptFields() error = %v", err)
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, err = EncryptFields(object, [][]string{{"spec", "missing"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err != nil {
		t.Fatalf("missing field EncryptFields() error = %v", err)
	}
}

func TestRestoreFieldsDoesNotMutateInput(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	object := newTestObject(t, map[string]any{"password": "secret"})
	encrypted, err := EncryptFields(object, [][]string{{"spec", "password"}}, Options{
		Mode: Age, Recipients: []age.Recipient{identity.Recipient()},
	})
	if err != nil {
		t.Fatalf("EncryptFields() error = %v", err)
	}
	if _, err := RestoreFields(encrypted, []age.Identity{identity}); err != nil {
		t.Fatalf("RestoreFields() error = %v", err)
	}

	value, found, err := unstructured.NestedFieldNoCopy(encrypted.Value.Object, "spec", "password")
	if err != nil || !found {
		t.Fatalf("encrypted input field lookup error = %v, found=%v", err, found)
	}
	if tag, _, ok := TaggedValueParts(value); !ok || tag != ageTag {
		t.Fatalf("encrypted input field was mutated: %#v", value)
	}
}

func TestContainsSIVSearchesAllObjectFields(t *testing.T) {
	object := newTestObject(t, map[string]any{
		"credentials": map[string]any{
			"password": NewTaggedValue(sivTag, "key-id:ciphertext"),
		},
	})

	if !ContainsSIV(object) {
		t.Fatal("ContainsSIV() = false for an AES-SIV marker in a CRD field")
	}
}

// newSecretObject builds a Secret with the supplied Kubernetes data map.
func newSecretObject(t *testing.T, data map[string]any) state.Object {
	t.Helper()
	object, err := state.NewObject(state.Identity{
		Version:   "v1",
		Resource:  "secrets",
		Kind:      "Secret",
		Namespace: "default",
		Name:      "credentials",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "credentials", "namespace": "default"},
		"data":       data,
	}})
	if err != nil {
		t.Fatal(err)
	}

	return object
}

// newTestObject builds a CRD-shaped object containing the supplied test spec.
func newTestObject(t *testing.T, spec map[string]any) state.Object {
	t.Helper()
	object, err := state.NewObject(state.Identity{
		Group:     "database.example.io",
		Version:   "v1",
		Resource:  "clusters",
		Kind:      "Cluster",
		Namespace: "default",
		Name:      "database",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "database.example.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "database", "namespace": "default"},
		"spec":       spec,
	}})
	if err != nil {
		t.Fatal(err)
	}

	return object
}

// mustJSON marshals a test value and fails immediately when JSON encoding fails.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

type testSIVResolver struct {
	keyID  string
	cipher *siv.Cipher
}

func (r testSIVResolver) Cipher(id string, _ []age.Identity) (*siv.Cipher, error) {
	if id != r.keyID {
		return nil, errors.New("unexpected AES-SIV key ID")
	}

	return r.cipher, nil
}
