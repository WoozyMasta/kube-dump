// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package fields applies configured encryption markers to Kubernetes resource fields
// without exposing plaintext values in logs or metadata.
package fields

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"filippo.io/age"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Mode controls whether profile-selected fields are encrypted.
type Mode string

// Options controls generic field encryption.
type Options struct {
	// SIVCipher encrypts fields with the active AES-SIV key.
	SIVCipher *siv.Cipher
	// Mode selects whether field values are kept plain or encrypted.
	Mode Mode
	// SIVKeyID identifies the active AES-SIV key in the keyring.
	SIVKeyID string
	// Recipients are the Age recipients used for field encryption.
	Recipients []age.Recipient
}

// SIVKeyResolver loads an AES-SIV key by ID using authorized age identities.
// It keeps keyring storage out of the field-encryption package.
type SIVKeyResolver interface {
	Cipher(id string, identities []age.Identity) (*siv.Cipher, error)
}

// TaggedValue is the JSON-compatible intermediate form for a tagged YAML scalar.
// The codec renders it as !kube-dump/<tag> when writing YAML.
type TaggedValue = map[string]any

const (
	// Plain preserves selected field values.
	Plain Mode = "plain"
	// Age encrypts each selected field independently.
	Age Mode = "age"
	// AES256SIV encrypts each selected field deterministically with AES-256-SIV.
	AES256SIV Mode = "aes-siv"

	tagKey          = "__kube_dump_tag"
	valueKey        = "value"
	markerKey       = "__kube_dump_marker"
	markerValue     = "kube-dump/v2"
	ageTag          = "age"
	ageDecryptedTag = "age-decrypted"
	metaTag         = "metadata"
	sivTag          = "aes-siv"
	sivDecryptedTag = "aes-siv-decrypted"
)

// UnmarshalText parses a field-encryption mode from CLI or configuration.
func (m *Mode) UnmarshalText(value []byte) error {
	parsed := Mode(string(value))
	if err := parsed.Validate(); err != nil {
		return err
	}

	*m = parsed
	return nil
}

// Validate checks whether the field-encryption mode is supported.
func (m Mode) Validate() error {
	switch m {
	case Plain, Age, AES256SIV:
		return nil
	default:
		return fmt.Errorf("unsupported field-encryption mode %q", m)
	}
}

// NewTaggedValue creates a tagged scalar intermediate value.
func NewTaggedValue(tag, value string) TaggedValue {
	return TaggedValue{markerKey: markerValue, tagKey: tag, valueKey: value}
}

// KnownTag reports whether tag is part of the canonical field representation.
// The codec uses this allowlist so arbitrary YAML tags
// cannot become secret ciphertext markers by accident.
func KnownTag(tag string) bool {
	switch tag {
	case ageTag, ageDecryptedTag, sivTag, sivDecryptedTag, metaTag:
		return true
	default:
		return false
	}
}

// TaggedValueParts reports the tag and scalar payload of a tagged value.
func TaggedValueParts(value any) (tag, payload string, ok bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return "", "", false
	}

	marker, markerOK := fields[markerKey].(string)
	tag, tagOK := fields[tagKey].(string)
	payload, valueOK := fields[valueKey].(string)

	if !markerOK ||
		marker != markerValue ||
		!tagOK ||
		!valueOK ||
		!KnownTag(tag) ||
		len(fields) != 3 {
		return "", "", false
	}

	return tag, payload, true
}

// ContainsSIV reports whether an object contains at least one AES-SIV marker.
// Callers use this to open the repository keyring lazily before restoration.
func ContainsSIV(object state.Object) bool {
	return containsSIVValue(object.Value.Object)
}

// ContainsDecryptedSIV reports whether an object contains a plaintext marker
// that must be re-encrypted with the AES-SIV keyring.
func ContainsDecryptedSIV(object state.Object) bool {
	return containsTagValue(object.Value.Object, sivDecryptedTag)
}

// ContainsTaggedData reports whether an object contains any canonical kube-dump marker
// that must be restored before applying the object.
func ContainsTaggedData(object state.Object) bool {
	return containsTaggedValue(object.Value.Object)
}

// EncryptFields encrypts matching fields on any Kubernetes object.
// Values are serialized as JSON before encryption so restore can preserve their original type.
func EncryptFields(object state.Object, paths [][]string, opts Options) (state.Object, error) {
	if err := object.Validate(); err != nil {
		return state.Object{}, fmt.Errorf("validate object before field encryption: %w", err)
	}
	if err := opts.Mode.Validate(); err != nil {
		return state.Object{}, err
	}

	if len(paths) == 0 || opts.Mode == Plain {
		return object, nil
	}

	if opts.Mode == Age && len(opts.Recipients) == 0 {
		return state.Object{}, errors.New("age recipients are required for generic field encryption")
	}
	if opts.Mode == AES256SIV && (opts.SIVCipher == nil || opts.SIVKeyID == "") {
		return state.Object{}, errors.New("AES-SIV keyring is required for generic field encryption")
	}

	value := object.Value.DeepCopy().Object
	for _, path := range paths {
		if err := validateEncryptionPath(path); err != nil {
			return state.Object{}, err
		}

		_, err := encryptFieldPath(value, path, object.Identity, opts)
		if err != nil {
			return state.Object{}, err
		}
	}

	result, err := state.NewObject(object.Identity, &unstructured.Unstructured{Object: value})
	if err != nil {
		return state.Object{}, err
	}

	return result, nil
}

// RestoreFields decrypts every tagged value recursively without consulting a profile.
// An optional AES-SIV resolver is used only when the input contains AES-SIV markers.
func RestoreFields(
	object state.Object,
	identities []age.Identity,
	resolvers ...SIVKeyResolver,
) (state.Object, error) {
	if err := object.Validate(); err != nil {
		return state.Object{}, fmt.Errorf("validate object before field restore: %w", err)
	}

	var resolver SIVKeyResolver
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}

	value, err := restoreGenericValue(object.Value.DeepCopy().Object, nil, object.Identity, identities, resolver)
	if err != nil {
		return state.Object{}, err
	}

	result, ok := value.(map[string]any)
	if !ok {
		return state.Object{}, errors.New("restored object is not a map")
	}

	return state.NewObject(object.Identity, &unstructured.Unstructured{Object: result})
}

// DecryptFields replaces encrypted markers with plaintext markers that retain the original algorithm.
// The resulting object is intentionally not plain Kubernetes YAML:
// encrypting it later must remain unambiguous.
func DecryptFields(
	object state.Object,
	identities []age.Identity,
	resolvers ...SIVKeyResolver,
) (state.Object, error) {
	if err := object.Validate(); err != nil {
		return state.Object{}, fmt.Errorf("validate object before field decryption: %w", err)
	}

	var resolver SIVKeyResolver
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}

	value, err := decryptValue(object.Value.DeepCopy().Object, nil, object.Identity, identities, resolver)
	if err != nil {
		return state.Object{}, err
	}

	result, ok := value.(map[string]any)
	if !ok {
		return state.Object{}, errors.New("decrypted object is not a map")
	}

	return state.NewObject(object.Identity, &unstructured.Unstructured{Object: result})
}

// EncryptDecryptedFields re-encrypts only decrypted markers
// and preserves the algorithm recorded by each marker.
// Plain values are never modified.
func EncryptDecryptedFields(object state.Object, opts Options) (state.Object, error) {
	if err := object.Validate(); err != nil {
		return state.Object{}, fmt.Errorf("validate object before field encryption: %w", err)
	}

	value, err := encryptDecryptedValue(
		object.Value.DeepCopy().Object,
		nil,
		object.Identity,
		opts,
	)
	if err != nil {
		return state.Object{}, err
	}

	result, ok := value.(map[string]any)
	if !ok {
		return state.Object{}, errors.New("encrypted object is not a map")
	}

	return state.NewObject(object.Identity, &unstructured.Unstructured{Object: result})
}

// decryptValue walks an object while retaining the source algorithm
// in each marker after its payload has been authenticated and decoded.
func decryptValue(
	value any,
	path []string,
	identity state.Identity,
	identities []age.Identity,
	keys SIVKeyResolver,
) (any, error) {
	if tag, payload, ok := TaggedValueParts(value); ok {
		if tag != ageTag && tag != sivTag {
			return value, nil
		}

		plain, err := restoreGenericTaggedValue(tag, payload, path, identity, identities, keys)
		if err != nil {
			return nil, err
		}

		decryptedPayload := string(plain)
		if isSecretDataPath(identity, path) {
			// A decrypted marker must retain a safe textual form for arbitrary Secret.data bytes
			// and must be reversible by EncryptDecryptedFields.
			decryptedPayload = base64.StdEncoding.EncodeToString(plain)
		}

		return NewTaggedValue(decryptedTag(tag), decryptedPayload), nil
	}

	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			decrypted, err := decryptValue(
				child,
				append(path, key),
				identity,
				identities,
				keys,
			)
			if err != nil {
				return nil, err
			}

			typed[key] = decrypted
		}

	case []any:
		for index, child := range typed {
			decrypted, err := decryptValue(
				child,
				append(path, strconv.Itoa(index)),
				identity,
				identities,
				keys,
			)
			if err != nil {
				return nil, err
			}

			typed[index] = decrypted
		}
	}

	return value, nil
}

// encryptDecryptedValue walks decrypted markers and reuses their algorithm.
func encryptDecryptedValue(value any, path []string, identity state.Identity, opts Options) (any, error) {
	if tag, payload, ok := TaggedValueParts(value); ok {
		if tag != ageDecryptedTag && tag != sivDecryptedTag {
			return value, nil
		}

		// Decrypted markers retain the algorithm that produced them.
		// Re-encrypt each marker with that algorithm instead of silently changing its mode.
		secretData := isSecretDataPath(identity, path)
		var plain any
		if secretData {
			// Keep decrypted Secret.data bytes in Base64
			// so the intermediate marker remains valid YAML even when the original value is not UTF-8.
			plain = payload
		} else if err := json.Unmarshal([]byte(payload), &plain); err != nil {
			return nil, fmt.Errorf("decode decrypted field value: %w", err)
		}

		mode := Age
		if tag == sivDecryptedTag {
			mode = AES256SIV
		}
		options := opts
		options.Mode = mode

		encrypted, err := encryptJSONValue(
			plain,
			path,
			identity,
			options,
			secretData,
		)
		if err != nil {
			return nil, err
		}

		return encrypted, nil
	}

	// Markers may occur at arbitrary depth,
	// so preserve the complete object shape while recursively replacing only decrypted markers.
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			encrypted, err := encryptDecryptedValue(
				child,
				append(path, key),
				identity,
				opts,
			)
			if err != nil {
				return nil, err
			}

			typed[key] = encrypted
		}

	case []any:
		for index, child := range typed {
			encrypted, err := encryptDecryptedValue(
				child,
				append(path, strconv.Itoa(index)),
				identity,
				opts,
			)
			if err != nil {
				return nil, err
			}

			typed[index] = encrypted
		}
	}

	return value, nil
}

// decryptedTag maps an encrypted algorithm tag to its reversible plaintext tag.
func decryptedTag(tag string) string {
	if tag == sivTag {
		return sivDecryptedTag
	}

	return ageDecryptedTag
}

// encryptFieldPath applies one dot path, where `*` expands map keys.
func encryptFieldPath(
	value map[string]any,
	path []string,
	identity state.Identity,
	opts Options,
) (bool, error) {
	if len(path) == 0 {
		return false, nil
	}

	return encryptNestedValue(value, path, nil, identity, opts)
}

// encryptNestedValue walks maps and replaces the selected terminal value.
func encryptNestedValue(
	value map[string]any,
	path []string,
	fieldPath []string,
	identity state.Identity,
	opts Options,
) (bool, error) {
	key := path[0]

	if key == "*" {
		// Expand wildcards against concrete map keys.
		// The concrete path is also the path authenticated by AES-SIV.
		applied := false
		for childKey := range value {
			childPath := joinFieldPath(fieldPath, childKey)
			if len(path) == 1 {
				if _, _, tagged := TaggedValueParts(value[childKey]); tagged {
					continue
				}

				encrypted, err := encryptJSONValue(
					value[childKey],
					childPath,
					identity,
					opts,
					isSecretDataValue(identity, fieldPath),
				)
				if err != nil {
					return false, fmt.Errorf("encrypt field %s: %w", strings.Join(childPath, "."), err)
				}

				value[childKey] = encrypted
				applied = true
				continue
			}

			childMap, ok := value[childKey].(map[string]any)
			if !ok {
				return false, fmt.Errorf("field %s is not an object", strings.Join(childPath, "."))
			}

			childApplied, err := encryptNestedValue(childMap, path[1:], childPath, identity, opts)
			if err != nil {
				return false, err
			}

			applied = applied || childApplied
		}

		return applied, nil
	}

	child, found := value[key]
	if !found {
		return false, nil
	}
	childPath := joinFieldPath(fieldPath, key)

	if len(path) == 1 {
		// The terminal segment is the value to encrypt.
		// Existing markers are already protected and must not be wrapped a second time.
		if _, _, tagged := TaggedValueParts(child); tagged {
			return false, nil
		}

		encrypted, err := encryptJSONValue(
			child,
			childPath,
			identity,
			opts,
			isSecretDataValue(identity, fieldPath),
		)
		if err != nil {
			return false, fmt.Errorf("encrypt field %s: %w", strings.Join(childPath, "."), err)
		}

		value[key] = encrypted
		return true, nil
	}

	childMap, ok := child.(map[string]any)
	if !ok {
		return false, fmt.Errorf("field %s is not an object", strings.Join(fieldPath, "."))
	}

	return encryptNestedValue(childMap, path[1:], childPath, identity, opts)
}

// joinFieldPath appends one concrete field name to the path used for AAD.
// Wildcard selectors are expanded before this helper is called,
// so the authenticated path never contains the selector syntax itself.
func joinFieldPath(prefix []string, key string) []string {
	result := make([]string, len(prefix), len(prefix)+1)
	copy(result, prefix)

	return append(result, key)
}

// validateEncryptionPath rejects paths that cannot describe a JSON field.
func validateEncryptionPath(path []string) error {
	if len(path) == 0 {
		return errors.New("encryption path must not be empty")
	}

	if slices.Contains(path, "") {
		return errors.New("encryption path contains an empty segment")
	}

	return nil
}

// encryptJSONValue creates a self-describing marker for one field value.
// Secret.data values use their decoded bytes as plaintext;
// every other field keeps the JSON representation so its original type survives restoration.
func encryptJSONValue(
	value any,
	fieldPath []string,
	identity state.Identity,
	opts Options,
	secretData bool,
) (TaggedValue, error) {
	var plain []byte
	if secretData {
		// Secret.data is a Base64 transport field.
		// Encrypt its decoded bytes and let restoration encode the original bytes again.
		encoded, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("secret.data field %s must be a base64 string", strings.Join(fieldPath, "."))
		}

		var err error
		plain, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode secret.data field %s as base64: %w", strings.Join(fieldPath, "."), err)
		}
	} else {
		var err error
		plain, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}

	if opts.Mode == AES256SIV {
		// AES-SIV authenticates the resource identity and concrete field path,
		// making unchanged values stable without allowing relocation to another object or field.
		aad, err := fieldAAD(identity, fieldPath)
		if err != nil {
			return nil, err
		}

		ciphertext, err := opts.SIVCipher.Encrypt(plain, aad)
		if err != nil {
			return nil, fmt.Errorf("encrypt AES-SIV field %s: %w", strings.Join(fieldPath, "."), err)
		}

		payload, err := siv.EncodePayload(opts.SIVKeyID, ciphertext)
		if err != nil {
			return nil, err
		}

		return NewTaggedValue(sivTag, payload), nil
	}

	var encrypted bytes.Buffer
	writer, err := agecrypto.NewEncryptWriter(
		&encrypted,
		opts.Recipients,
		agecrypto.EncryptOptions{},
	)
	if err != nil {
		return nil, err
	}

	if _, err := writer.Write(plain); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return NewTaggedValue(ageTag, encodeInlineAgePayload(encrypted.Bytes())), nil
}

// encodeInlineAgePayload stores the binary age stream compactly
// while keeping long encrypted values readable as a YAML block scalar.
func encodeInlineAgePayload(value []byte) string {
	const lineLength = 76
	encoded := base64.RawURLEncoding.EncodeToString(value)
	var result strings.Builder
	result.Grow(len(encoded) + len(encoded)/lineLength)

	for start := 0; start < len(encoded); start += lineLength {
		if start > 0 {
			result.WriteByte('\n')
		}

		end := min(start+lineLength, len(encoded))
		result.WriteString(encoded[start:end])
	}

	return result.String()
}

// restoreGenericValue recursively replaces markers while preserving the input path.
// The path is needed to reproduce the AES-SIV associated data used at encryption time.
func restoreGenericValue(
	value any,
	path []string,
	identity state.Identity,
	identities []age.Identity,
	keys SIVKeyResolver,
) (any, error) {
	if tag, payload, ok := TaggedValueParts(value); ok {
		// Markers are terminal values.
		// Restore them before descending into maps and slices
		// so encrypted scalar types are not mistaken for containers.
		plain, err := restoreGenericTaggedValue(tag, payload, path, identity, identities, keys)
		if err != nil {
			return nil, err
		}
		if isSecretDataPath(identity, path) {
			return restoreSecretDataValue(tag, payload, plain)
		}

		var restored any
		if err := json.Unmarshal(plain, &restored); err != nil {
			return nil, fmt.Errorf("decode restored field value: %w", err)
		}

		return restored, nil
	}

	switch typed := value.(type) {
	case map[string]any:
		// Keep the path synchronized with the recursive walk;
		// AES-SIV uses it as associated data and changing it would make existing fields undecryptable
		for key, child := range typed {
			restored, err := restoreGenericValue(
				child,
				append(path, key),
				identity,
				identities,
				keys,
			)
			if err != nil {
				return nil, err
			}

			typed[key] = restored
		}

	case []any:
		for index, child := range typed {
			restored, err := restoreGenericValue(
				child,
				append(path, strconv.Itoa(index)),
				identity,
				identities,
				keys,
			)
			if err != nil {
				return nil, err
			}

			typed[index] = restored
		}
	}

	return value, nil
}

// isSecretDataValue reports whether a terminal value is a direct Secret.data entry.
// The surrounding field path is "data" immediately before the map key is visited.
func isSecretDataValue(identity state.Identity, fieldPath []string) bool {
	return identity.Kind == "Secret" && len(fieldPath) == 1 && fieldPath[0] == "data"
}

// isSecretDataPath reports whether a marker belongs to one direct Secret.data entry.
func isSecretDataPath(identity state.Identity, path []string) bool {
	return identity.Kind == "Secret" && len(path) == 2 && path[0] == "data"
}

// restoreSecretDataValue returns the canonical Kubernetes Base64 representation.
// Decrypted markers already carry Base64 because they must survive a later decrypt-to-encrypt cycle
// without exposing arbitrary bytes in YAML.
func restoreSecretDataValue(tag, payload string, plain []byte) (string, error) {
	if tag == ageDecryptedTag || tag == sivDecryptedTag {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", fmt.Errorf("decode decrypted Secret.data value as base64: %w", err)
		}

		return base64.StdEncoding.EncodeToString(decoded), nil
	}

	return base64.StdEncoding.EncodeToString(plain), nil
}

// restoreGenericTaggedValue dispatches a marker to its algorithm-specific decryptor.
func restoreGenericTaggedValue(
	tag, payload string,
	path []string,
	identity state.Identity,
	identities []age.Identity,
	keys SIVKeyResolver,
) ([]byte, error) {
	switch tag {
	case ageTag:
		return DecryptAgeValue(NewTaggedValue(tag, payload), identities)

	case ageDecryptedTag, sivDecryptedTag:
		return []byte(payload), nil

	case sivTag:
		if keys == nil {
			return nil, errors.New("AES-SIV keyring is required")
		}

		if len(path) < 2 {
			return nil, errors.New("AES-SIV marker has no field path")
		}

		keyID, ciphertext, err := siv.DecodePayload(payload)
		if err != nil {
			return nil, fmt.Errorf("decode AES-SIV payload: %w", err)
		}

		cipher, err := keys.Cipher(keyID, identities)
		if err != nil {
			return nil, fmt.Errorf("load AES-SIV key %q: %w", keyID, err)
		}

		aad, err := fieldAAD(identity, path)
		if err != nil {
			return nil, err
		}

		return cipher.Decrypt(ciphertext, aad)

	case metaTag:
		return nil, errors.New("metadata-only field cannot be restored")

	default:
		return nil, fmt.Errorf("unsupported kube-dump tag %q", tag)
	}
}

// fieldAAD builds associated data from the exact resource field path.
// The source identity must be used before namespace/name mapping
// so portable restores authenticate against the original object identity.
func fieldAAD(identity state.Identity, path []string) ([]byte, error) {
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("validate resource identity for AAD: %w", err)
	}

	if len(path) < 2 || slices.Contains(path, "") {
		return nil, errors.New("field AAD path must contain at least two non-empty segments")
	}

	apiVersion := identity.Version
	if identity.Group != "" {
		apiVersion = identity.Group + "/" + identity.Version
	}

	values := make([]string, 0, 4+len(path))
	values = append(values,
		apiVersion,
		identity.Kind,
		identity.Namespace,
		identity.Name,
	)
	values = append(values, path...)
	return encodeAAD(append([]string{"kube-dump:field:v1"}, values...)...)
}

// encodeAAD length-prefixes each component so concatenated values remain unambiguous.
func encodeAAD(values ...string) ([]byte, error) {
	var result bytes.Buffer
	for _, value := range values {
		if uint64(len(value)) > math.MaxUint32 {
			return nil, errors.New("field AAD component is too large")
		}

		// The preceding bound check makes this conversion safe.
		//
		//nolint:gosec // value length is bounded above
		if err := binary.Write(&result, binary.BigEndian, uint32(len(value))); err != nil {
			return nil, fmt.Errorf("encode field AAD length: %w", err)
		}
		if _, err := result.WriteString(value); err != nil {
			return nil, fmt.Errorf("encode field AAD component: %w", err)
		}
	}

	return result.Bytes(), nil
}

// containsTaggedValue searches an object without interpreting encrypted payloads.
func containsTaggedValue(value any) bool {
	if _, _, ok := TaggedValueParts(value); ok {
		return true
	}

	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if containsTaggedValue(child) {
				return true
			}
		}

	case []any:
		return slices.ContainsFunc(typed, containsTaggedValue)
	}

	return false
}

// containsSIVValue searches nested maps and slices for AES-SIV markers.
func containsSIVValue(value any) bool {
	if tag, _, ok := TaggedValueParts(value); ok {
		return tag == sivTag
	}

	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if containsSIVValue(child) {
				return true
			}
		}

	case []any:
		return slices.ContainsFunc(typed, containsSIVValue)
	}

	return false
}

// containsTagValue searches nested maps and slices for one exact marker tag.
func containsTagValue(value any, wanted string) bool {
	if tag, _, ok := TaggedValueParts(value); ok {
		return tag == wanted
	}

	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if containsTagValue(child, wanted) {
				return true
			}
		}

	case []any:
		return slices.ContainsFunc(typed, func(child any) bool {
			return containsTagValue(child, wanted)
		})
	}

	return false
}

// DecryptAgeValue decrypts one age-tagged scalar and returns its encrypted plaintext bytes.
// Generic fields contain JSON bytes; Secret.data fields contain the decoded secret bytes.
func DecryptAgeValue(value any, identities []age.Identity) ([]byte, error) {
	tag, payload, ok := TaggedValueParts(value)
	if !ok || tag != ageTag {
		return nil, errors.New("value is not an age-tagged field")
	}

	encoded := strings.Join(strings.Fields(payload), "")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode inline age payload: %w", err)
	}

	reader, err := agecrypto.OpenDecryptReader(bytes.NewReader(data), identities)
	if err != nil {
		return nil, fmt.Errorf("open encrypted resource field: %w", err)
	}

	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decrypt resource field: %w", err)
	}

	return plain, nil
}
