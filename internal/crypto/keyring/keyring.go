// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package keyring stores AES-SIV keys in age-encrypted envelopes.
package keyring

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/siv"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"go.yaml.in/yaml/v3"
)

const (
	metadataVersion = 1
	keyAlgorithm    = "aes-256-siv"
	keyIDBytes      = 8
	maxEnvelopeSize = 1 << 20
)

// Metadata describes the active and historical SIV keys without containing plaintext key material.
type Metadata struct {
	// ActiveKey identifies the key used for newly encrypted values.
	ActiveKey string `yaml:"activeSivKey"`
	// Keys lists active and historical encrypted key envelopes.
	Keys []KeyRecord `yaml:"keys"`
	// Version identifies the keyring metadata schema.
	Version int `yaml:"version"`
}

// KeyRecord identifies one age-encrypted SIV key envelope.
type KeyRecord struct {
	// ID is the stable hexadecimal key identifier.
	ID string `yaml:"id"`
	// Algorithm identifies the cipher used by the envelope.
	Algorithm string `yaml:"algorithm"`
	// File is the keyring-relative path of the age envelope.
	File string `yaml:"file"`
}

// rewrapEntry holds all data required to replace one envelope during a rewrap transaction.
// Original bytes allow rollback if a later replacement fails after earlier files were already installed.
type rewrapEntry struct {
	id        string // id identifies the SIV key record being rewrapped.
	target    string // target is the validated filesystem path of the envelope.
	original  []byte // original contains the old envelope for rollback.
	rewrapped []byte // rewrapped contains the newly encrypted envelope.
}

// Keyring provides active and historical AES-SIV keys loaded from a backup.
type Keyring struct {
	keys      map[string]*siv.Cipher // keys caches decrypted ciphers by stable key ID.
	envelopes map[string][]byte      // envelopes contains archive-loaded key files by relative path.
	root      string                 // root is the backup root containing the keyring directory.
	metadata  Metadata               // metadata is the validated keyring descriptor.
}

// Open loads an existing keyring or creates the first key when the crypto metadata is absent.
// Existing keyrings always require an authorized age identity;
// recipients alone can never regenerate a deterministic key.
func Open(root string, recipients []age.Recipient, identities []age.Identity) (*Keyring, error) {
	return open(root, recipients, identities, true)
}

// OpenExisting opens an initialized local keyring without allowing creation.
// Maintenance commands use this boundary
// so a misspelled backup path cannot silently create a new unrelated AES-SIV keyring.
func OpenExisting(root string, identities []age.Identity) (*Keyring, error) {
	return open(root, nil, identities, false)
}

// open loads a keyring and optionally permits initialization when metadata is absent.
func open(root string, recipients []age.Recipient, identities []age.Identity, allowCreate bool) (*Keyring, error) {
	if root == "" {
		return nil, errors.New("AES-SIV keyring root is required")
	}

	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve AES-SIV keyring root: %w", err)
	}

	keyring := &Keyring{root: absolute, keys: make(map[string]*siv.Cipher)}
	metadataPath := keyring.metadataPath()
	data, err := os.ReadFile(metadataPath)
	if os.IsNotExist(err) {
		if !allowCreate {
			return nil, errors.New("AES-SIV keyring metadata is missing")
		}
		if err := rejectOrphanedKeyring(keyring.cryptoPath()); err != nil {
			return nil, err
		}
		return create(keyring, recipients)
	}
	if err != nil {
		return nil, fmt.Errorf("read AES-SIV keyring metadata: %w", err)
	}
	if err := yaml.Unmarshal(data, &keyring.metadata); err != nil {
		return nil, fmt.Errorf("decode AES-SIV keyring metadata: %w", err)
	}
	if err := keyring.validateMetadata(); err != nil {
		return nil, err
	}

	if len(identities) == 0 {
		return nil, errors.New("existing AES-SIV keyring requires an authorized private age identity")
	}

	active, err := keyring.loadKey(keyring.metadata.ActiveKey, identities)
	if err != nil {
		return nil, fmt.Errorf("open active AES-SIV key: %w", err)
	}

	keyring.keys[keyring.metadata.ActiveKey] = active
	if err := keyring.loadAllKeys(identities); err != nil {
		return nil, err
	}

	return keyring, nil
}

// OpenData opens a keyring whose metadata and encrypted envelopes were read from an archive stream.
// The encrypted key files remain in memory because they are small
// and must be available when historical field keys are needed.
func OpenData(metadataData []byte, envelopes map[string][]byte, identities []age.Identity) (*Keyring, error) {
	if len(metadataData) == 0 {
		return nil, errors.New("AES-SIV keyring metadata is required")
	}

	keyring := &Keyring{
		keys:      make(map[string]*siv.Cipher),
		envelopes: envelopes,
	}

	if err := yaml.Unmarshal(metadataData, &keyring.metadata); err != nil {
		return nil, fmt.Errorf("decode AES-SIV keyring metadata: %w", err)
	}
	if err := keyring.validateMetadata(); err != nil {
		return nil, err
	}

	if len(identities) == 0 {
		return nil, errors.New("existing AES-SIV keyring requires an authorized private age identity")
	}

	active, err := keyring.loadKey(keyring.metadata.ActiveKey, identities)
	if err != nil {
		return nil, fmt.Errorf("open active AES-SIV key: %w", err)
	}

	keyring.keys[keyring.metadata.ActiveKey] = active
	if err := keyring.loadAllKeys(identities); err != nil {
		return nil, err
	}

	return keyring, nil
}

// loadAllKeys verifies every recorded envelope before the keyring is returned.
// Historical keys can still be referenced by older deterministic field values,
// so validating only the active key would allow a broken backup to be published.
func (k *Keyring) loadAllKeys(identities []age.Identity) error {
	for _, record := range k.metadata.Keys {
		if _, loaded := k.keys[record.ID]; loaded {
			continue
		}

		cipher, err := k.loadKey(record.ID, identities)
		if err != nil {
			return fmt.Errorf("open historical AES-SIV key %q: %w", record.ID, err)
		}

		k.keys[record.ID] = cipher
	}

	return nil
}

// rejectOrphanedKeyring prevents a missing metadata file from being treated
// as a fresh repository when encrypted key envelopes still exist.
func rejectOrphanedKeyring(path string) error {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect AES-SIV keyring directory: %w", err)
	}
	if len(entries) > 0 {
		return errors.New("AES-SIV keyring metadata is missing; refusing to generate a replacement key")
	}

	return nil
}

// Active returns the stable active key ID and cipher used for new values.
func (k *Keyring) Active() (string, *siv.Cipher, error) {
	if k == nil {
		return "", nil, errors.New("AES-SIV keyring is nil")
	}

	cipher, ok := k.keys[k.metadata.ActiveKey]
	if !ok {
		return "", nil, errors.New("active AES-SIV key is not loaded")
	}

	return k.metadata.ActiveKey, cipher, nil
}

// Rewrap encrypts every recorded SIV key for a new age recipient
// set without changing key IDs or encrypted field payloads.
//
// Historical keys must be included because unchanged resources can still reference them after rotation.
// An authorized identity is mandatory so recipient-only callers cannot replace key material.
func (k *Keyring) Rewrap(recipients []age.Recipient, identities []age.Identity) error {
	if k == nil {
		return errors.New("AES-SIV keyring is nil")
	}
	if len(recipients) == 0 {
		return errors.New("AES-SIV rewrap requires age recipients")
	}
	if len(identities) == 0 {
		return errors.New("AES-SIV rewrap requires an authorized age identity")
	}

	entries := make([]rewrapEntry, 0, len(k.metadata.Keys))
	for _, record := range k.metadata.Keys {
		key, err := k.readKey(record.ID, identities)
		if err != nil {
			return fmt.Errorf("read key %q for rewrap: %w", record.ID, err)
		}

		target := filepath.Join(k.cryptoPath(), filepath.FromSlash(record.File))
		original, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read original envelope %q for rewrap: %w", record.ID, err)
		}

		rewrapped, err := encryptKeyEnvelope(record.ID, key, recipients)
		if err != nil {
			return fmt.Errorf("encrypt key %q for rewrap: %w", record.ID, err)
		}

		entries = append(entries, rewrapEntry{record.ID, target, original, rewrapped})
	}

	installed := 0
	for _, entry := range entries {
		if err := k.writeAtomic(entry.target, entry.rewrapped, 0o600); err != nil {
			for index := installed - 1; index >= 0; index-- {
				rollback := entries[index]
				_ = k.writeAtomic(rollback.target, rollback.original, 0o600)
			}

			return fmt.Errorf("install rewrapped key %q: %w", entry.id, err)
		}

		installed++
	}

	return nil
}

// Rotate generates a new active AES-SIV key and keeps all old key records,
// so existing deterministic payloads remain decryptable.
// Re-encrypting resource fields is intentionally a separate operation.
func (k *Keyring) Rotate(recipients []age.Recipient) error {
	if k == nil {
		return errors.New("AES-SIV keyring is nil")
	}
	if len(recipients) == 0 {
		return errors.New("AES-SIV rotation requires age recipients")
	}

	key, id, err := generateKey()
	if err != nil {
		return err
	}
	if err := k.writeKey(id, key, recipients); err != nil {
		return err
	}

	k.metadata.ActiveKey = id
	k.metadata.Keys = append(k.metadata.Keys, KeyRecord{
		ID:        id,
		Algorithm: keyAlgorithm,
		File:      "keys/siv-" + id + ".age",
	})
	if err := k.writeMetadata(); err != nil {
		// Metadata is the index of the envelope.
		// Remove the newly written envelope when the index cannot be published, avoiding an orphan key.
		_ = os.Remove(filepath.Join(k.cryptoPath(), "keys", "siv-"+id+".age"))
		return err
	}

	cipher, err := siv.New(key)
	if err != nil {
		return err
	}

	k.keys[id] = cipher
	return nil
}

// Cipher returns a historical key by ID, loading and decrypting its age envelope on demand.
// The caller must provide an authorized identity.
func (k *Keyring) Cipher(id string, identities []age.Identity) (*siv.Cipher, error) {
	if k == nil {
		return nil, errors.New("AES-SIV keyring is nil")
	}

	if cipher, ok := k.keys[id]; ok {
		return cipher, nil
	}
	if len(identities) == 0 {
		return nil, errors.New("AES-SIV key requires an age identity")
	}

	cipher, err := k.loadKey(id, identities)
	if err != nil {
		return nil, err
	}

	k.keys[id] = cipher
	return cipher, nil
}

// create generates the first repository key, persists its encrypted envelope,
// and installs the corresponding active cipher in keyring.
func create(keyring *Keyring, recipients []age.Recipient) (*Keyring, error) {
	if len(recipients) == 0 {
		return nil, errors.New("AES-SIV mode requires age recipients to create a keyring")
	}

	key, id, err := generateKey()
	if err != nil {
		return nil, err
	}

	keyring.metadata = Metadata{
		Version:   metadataVersion,
		ActiveKey: id,
		Keys: []KeyRecord{{
			ID:        id,
			Algorithm: keyAlgorithm,
			File:      "keys/siv-" + id + ".age",
		}},
	}

	if err := keyring.writeKey(id, key, recipients); err != nil {
		return nil, err
	}
	if err := keyring.writeMetadata(); err != nil {
		_ = os.Remove(filepath.Join(keyring.cryptoPath(), "keys", "siv-"+id+".age"))
		return nil, err
	}

	cipher, err := siv.New(key)
	if err != nil {
		return nil, err
	}

	keyring.keys[id] = cipher
	return keyring, nil
}

// generateKey creates validated AES-SIV key material and its random stable ID.
func generateKey() ([]byte, string, error) {
	key := make([]byte, siv.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", fmt.Errorf("generate AES-SIV key: %w", err)
	}

	idBytes := make([]byte, keyIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, "", fmt.Errorf("generate AES-SIV key ID: %w", err)
	}

	id := hex.EncodeToString(idBytes)
	if _, err := siv.New(key); err != nil {
		return nil, "", err
	}

	return key, id, nil
}

// validateMetadata checks schema, active-key membership, record uniqueness,
// and the constrained file layout before any envelope is opened.
func (k *Keyring) validateMetadata() error {
	if k.metadata.Version != metadataVersion {
		return fmt.Errorf("unsupported AES-SIV keyring version %d", k.metadata.Version)
	}
	if k.metadata.ActiveKey == "" {
		return errors.New("AES-SIV keyring active key is required")
	}

	seen := make(map[string]struct{}, len(k.metadata.Keys))
	for _, record := range k.metadata.Keys {
		// Reject malformed records before using their paths or loading ciphertext.
		if record.ID == "" || record.Algorithm != keyAlgorithm || record.File == "" {
			return fmt.Errorf("invalid AES-SIV key record %q", record.ID)
		}
		if _, ok := seen[record.ID]; ok {
			return fmt.Errorf("duplicate AES-SIV key ID %q", record.ID)
		}

		seen[record.ID] = struct{}{}
		if !isKeyFile(record.File, record.ID) {
			return fmt.Errorf("invalid AES-SIV key file for %q", record.ID)
		}
	}

	if _, ok := seen[k.metadata.ActiveKey]; !ok {
		return fmt.Errorf("active AES-SIV key %q is not listed", k.metadata.ActiveKey)
	}

	return nil
}

// loadKey decrypts one key envelope and wraps the raw key in the SIV cipher.
func (k *Keyring) loadKey(id string, identities []age.Identity) (*siv.Cipher, error) {
	key, err := k.readKey(id, identities)
	if err != nil {
		return nil, err
	}

	return siv.New(key)
}

// readKey locates, decrypts, bounds, and validates one key envelope.
func (k *Keyring) readKey(id string, identities []age.Identity) ([]byte, error) {
	var record *KeyRecord
	for index := range k.metadata.Keys {
		if k.metadata.Keys[index].ID == id {
			record = &k.metadata.Keys[index]
			break
		}
	}

	if record == nil {
		return nil, fmt.Errorf("AES-SIV key %q is not listed", id)
	}

	var encrypted []byte
	if k.envelopes != nil {
		encrypted = k.envelopes[record.File]
		if len(encrypted) == 0 {
			return nil, fmt.Errorf("AES-SIV key envelope %q is not present in archive", id)
		}
	} else {
		// The metadata validator restricts record.File to the keyring layout;
		// filepath.Join is therefore safe from path traversal through metadata.
		file, err := os.Open(filepath.Join(k.cryptoPath(), filepath.FromSlash(record.File)))
		if err != nil {
			return nil, fmt.Errorf("open AES-SIV key envelope %q: %w", id, err)
		}

		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read AES-SIV key envelope %q: %w", id, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close AES-SIV key envelope %q: %w", id, closeErr)
		}

		encrypted = data
	}

	decrypted, err := agecrypto.OpenDecryptReader(bytes.NewReader(encrypted), identities)
	if err != nil {
		return nil, fmt.Errorf("decrypt AES-SIV key envelope %q: %w", id, err)
	}

	// Read one byte beyond the limit so oversized envelopes are detected.
	data, err := io.ReadAll(io.LimitReader(decrypted, maxEnvelopeSize+1))
	if err != nil {
		return nil, fmt.Errorf("read AES-SIV key envelope %q: %w", id, err)
	}
	if len(data) > maxEnvelopeSize {
		return nil, fmt.Errorf("AES-SIV key envelope %q is too large", id)
	}

	keyID, key, err := decodeEnvelope(data)
	if err != nil {
		return nil, fmt.Errorf("decode AES-SIV key envelope %q: %w", id, err)
	}
	if keyID != id {
		return nil, fmt.Errorf("AES-SIV key envelope ID %q does not match %q", keyID, id)
	}

	return key, nil
}

// writeKey encodes and age-encrypts one SIV key into its fixed repository path.
func (k *Keyring) writeKey(id string, key []byte, recipients []age.Recipient) error {
	encrypted, err := encryptKeyEnvelope(id, key, recipients)
	if err != nil {
		return err
	}

	return k.writeAtomic(filepath.Join(k.cryptoPath(), "keys", "siv-"+id+".age"), encrypted, 0o600)
}

// encryptKeyEnvelope serializes and age-encrypts one raw SIV key without touching the filesystem.
// Rewrap uses this preparation step before any envelope is replaced.
func encryptKeyEnvelope(id string, key []byte, recipients []age.Recipient) ([]byte, error) {
	data, err := encodeEnvelope(id, key)
	if err != nil {
		return nil, err
	}

	var encrypted bytes.Buffer
	writer, err := agecrypto.NewEncryptWriter(&encrypted, recipients, agecrypto.EncryptOptions{})
	if err != nil {
		return nil, fmt.Errorf("create AES-SIV key envelope: %w", err)
	}

	if _, err := writer.Write(data); err != nil {
		// Close best-effort here; the original write error is more actionable.
		_ = writer.Close()
		return nil, fmt.Errorf("write AES-SIV key envelope: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize AES-SIV key envelope: %w", err)
	}

	return encrypted.Bytes(), nil
}

// writeMetadata serializes the validated keyring descriptor atomically.
func (k *Keyring) writeMetadata() error {
	data, err := yaml.Marshal(k.metadata)
	if err != nil {
		return fmt.Errorf("encode AES-SIV keyring metadata: %w", err)
	}

	return k.writeAtomic(k.metadataPath(), data, 0o640)
}

// metadataPath returns the stable path of the keyring descriptor.
func (k *Keyring) metadataPath() string {
	return filepath.Join(k.cryptoPath(), "metadata.yaml")
}

// cryptoPath returns the directory containing metadata and encrypted keys.
func (k *Keyring) cryptoPath() string {
	return filepath.Join(k.root, ".kube-dump", "crypto")
}

// writeAtomic publishes data through a same-directory temporary file.
// Sync and rename ensure readers observe either the previous complete file
// or the new complete file, never a partially written keyring record.
func (k *Keyring) writeAtomic(target string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create AES-SIV keyring directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(target), ".kube-dump-siv-*.tmp")
	if err != nil {
		return fmt.Errorf("create AES-SIV keyring temporary file: %w", err)
	}

	temporaryName := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set AES-SIV keyring file permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write AES-SIV keyring file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync AES-SIV keyring file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close AES-SIV keyring file: %w", err)
	}

	if err := fileutil.ReplaceFile(temporaryName, target); err != nil {
		return fmt.Errorf("publish AES-SIV keyring file: %w", err)
	}

	remove = false
	return nil
}

// encodeEnvelope serializes the versioned binary key envelope before age encryption.
func encodeEnvelope(id string, key []byte) ([]byte, error) {
	if !validKeyID(id) {
		return nil, fmt.Errorf("invalid AES-SIV key ID %q", id)
	}
	if len(key) != siv.KeySize {
		return nil, fmt.Errorf("AES-SIV key must be %d bytes", siv.KeySize)
	}

	var result bytes.Buffer
	result.WriteString("KDSIV\x00")
	result.WriteByte(1)
	result.WriteByte(byte(keyIDBytes * 2))
	result.WriteString(id)

	if err := binary.Write(&result, binary.BigEndian, uint32(siv.KeySize)); err != nil {
		return nil, fmt.Errorf("encode AES-SIV key envelope: %w", err)
	}

	result.Write(key)
	return result.Bytes(), nil
}

// decodeEnvelope validates and parses one complete binary key envelope.
func decodeEnvelope(data []byte) (string, []byte, error) {
	if len(data) < 7 || string(data[:6]) != "KDSIV\x00" || data[6] != 1 {
		return "", nil, errors.New("invalid AES-SIV key envelope header")
	}

	// The seven-byte prefix contains the magic, envelope version, and ID length.
	reader := bytes.NewReader(data[7:])
	idLength, err := reader.ReadByte()
	if err != nil || idLength == 0 {
		return "", nil, errors.New("invalid AES-SIV key envelope ID")
	}

	idBytes := make([]byte, idLength)
	if _, err := io.ReadFull(reader, idBytes); err != nil {
		return "", nil, errors.New("truncated AES-SIV key envelope ID")
	}

	var keyLength uint32
	if err := binary.Read(reader, binary.BigEndian, &keyLength); err != nil || keyLength != siv.KeySize {
		return "", nil, errors.New("invalid AES-SIV key envelope length")
	}

	key := make([]byte, keyLength)
	if _, err := io.ReadFull(reader, key); err != nil {
		return "", nil, errors.New("truncated AES-SIV key envelope key")
	}
	if reader.Len() != 0 {
		return "", nil, errors.New("trailing data in AES-SIV key envelope")
	}

	id := string(idBytes)
	if !validKeyID(id) {
		return "", nil, errors.New("invalid AES-SIV key envelope ID")
	}

	return id, key, nil
}

// isKeyFile reports whether file is the canonical path for id.
func isKeyFile(file, id string) bool {
	return file == "keys/siv-"+id+".age"
}

// validKeyID reports whether id is a lowercase hexadecimal key identifier.
func validKeyID(id string) bool {
	if len(id) != keyIDBytes*2 {
		return false
	}

	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// IDs returns all known key IDs in deterministic order.
func (k *Keyring) IDs() []string {
	if k == nil {
		return nil
	}

	ids := make([]string, 0, len(k.metadata.Keys))
	for _, record := range k.metadata.Keys {
		ids = append(ids, record.ID)
	}

	sort.Strings(ids)
	return ids
}
