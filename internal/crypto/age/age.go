// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package age provides the Age stream and key-file helpers used by
// archives, PVC data, and encrypted resource fields.
package age

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"golang.org/x/crypto/ssh"
)

const (
	// maxKeyFileSize prevents accidentally loading an unbounded key file.
	maxKeyFileSize int64 = 16 << 20
)

// ErrIdentityPassphraseRequired indicates that an encrypted SSH identity
// needs a passphrase before it can be used.
var ErrIdentityPassphraseRequired = errors.New(
	"encrypted SSH identity requires a passphrase; use --identity-passphrase or --identity-passphrase-file")

// EncryptOptions controls the representation of an age payload.
type EncryptOptions struct {
	// Armor selects the human-readable ASCII age representation.
	Armor bool
}

// IdentityOptions selects how passphrase-protected SSH identity files are unlocked.
// Native age identities and unencrypted SSH keys do not use these fields.
type IdentityOptions struct {
	// Passphrase is the passphrase supplied directly by the caller.
	Passphrase string
	// PassphraseFile is the file containing the passphrase.
	PassphraseFile string
}

// encryptWriter closes the age stream before its optional armor stream.
type encryptWriter struct {
	// encrypted is the age stream that must be finalized first.
	encrypted io.WriteCloser
	// armored is the optional ASCII armor stream finalized after encrypted.
	armored io.WriteCloser
}

// NewEncryptWriter creates a streaming age encryptor.
//
// The returned writer must be closed to flush the age payload,
// and when enabled, the ASCII armor footer.
func NewEncryptWriter(dst io.Writer, recipients []age.Recipient, opts EncryptOptions) (io.WriteCloser, error) {
	if dst == nil {
		return nil, errors.New("age encryption destination is nil")
	}
	if len(recipients) == 0 {
		return nil, errors.New("age encryption requires at least one recipient")
	}

	if !opts.Armor {
		return age.Encrypt(dst, recipients...)
	}

	armored := armor.NewWriter(dst)
	encrypted, err := age.Encrypt(armored, recipients...)
	if err != nil {
		_ = armored.Close()
		return nil, fmt.Errorf("create age encryptor: %w", err)
	}

	// age must be closed before armor so the final encrypted bytes
	// are emitted before the armor footer is written.
	return &encryptWriter{encrypted: encrypted, armored: armored}, nil
}

// OpenDecryptReader opens a streaming age decryptor and detects ASCII armor.
// Detection consumes no payload bytes:
// the buffered reader is reused by the decoder,
// so callers can safely pass a network or archive stream.
func OpenDecryptReader(src io.Reader, identities []age.Identity) (io.Reader, error) {
	if src == nil {
		return nil, errors.New("age decryption source is nil")
	}
	if len(identities) == 0 {
		return nil, errors.New("age decryption requires at least one identity")
	}

	buffered := bufio.NewReader(src)
	peek, err := buffered.Peek(len(armor.Header))
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, fmt.Errorf("inspect age payload: %w", err)
	}
	if string(peek) == armor.Header {
		buffered = bufio.NewReader(armor.NewReader(buffered))
	}

	decrypted, err := age.Decrypt(buffered, identities...)
	if err != nil {
		return nil, fmt.Errorf("open age payload: %w", err)
	}

	return decrypted, nil
}

// ValidateRecipientsIdentities verifies that at least one identity
// can decrypt a payload encrypted for the supplied recipients.
// The probe contains no user data and exists only to reject mismatched key configuration
// before a backup keyring is created.
func ValidateRecipientsIdentities(recipients []age.Recipient, identities []age.Identity) error {
	if len(recipients) == 0 {
		return errors.New("recipient validation requires at least one recipient")
	}
	if len(identities) == 0 {
		return errors.New("recipient validation requires at least one identity")
	}

	const probe = "kube-dump recipient validation"

	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipients...)
	if err != nil {
		return fmt.Errorf("create validation payload: %w", err)
	}
	if _, err := io.WriteString(writer, probe); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write validation payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize validation payload: %w", err)
	}

	reader, err := age.Decrypt(bytes.NewReader(encrypted.Bytes()), identities...)
	if err != nil {
		return errors.New("no supplied identity matches the configured recipients")
	}

	data, err := io.ReadAll(io.LimitReader(reader, int64(len(probe)+1)))
	if err != nil {
		return fmt.Errorf("read validation payload: %w", err)
	}
	if string(data) != probe {
		return errors.New("recipient and identity validation payload mismatch")
	}

	return nil
}

// MaybeDecryptReader detects binary or armored age framing without treating
// the mere presence of identities as proof that a stream is encrypted.
func MaybeDecryptReader(source io.Reader, identities []age.Identity) (io.Reader, error) {
	if source == nil {
		return nil, errors.New("age source is nil")
	}

	buffered := bufio.NewReader(source)
	armorPeek, _ := buffered.Peek(len(armor.Header))
	binaryHeader := []byte("age-encryption.org/v1\n")
	binaryPeek, _ := buffered.Peek(len(binaryHeader))
	encrypted := string(armorPeek) == armor.Header || string(binaryPeek) == string(binaryHeader)
	if !encrypted {
		return buffered, nil
	}
	if len(identities) == 0 {
		return nil, errors.New("encrypted age stream requires identities")
	}

	return OpenDecryptReader(buffered, identities)
}

// LoadRecipients reads native age and SSH recipients from files.
// Files are size-limited before parsing
// and may contain either native age entries or one SSH recipient per non-comment line.
func LoadRecipients(paths []string) ([]age.Recipient, error) {
	if len(paths) == 0 {
		return nil, errors.New("no recipient files configured")
	}

	var recipients []age.Recipient
	for _, path := range paths {
		data, err := readKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("read recipient file %q: %w", path, err)
		}

		parsed, err := parseRecipients(data)
		if err != nil {
			return nil, fmt.Errorf("parse recipient file %q: %w", path, err)
		}

		recipients = append(recipients, parsed...)
	}

	return recipients, nil
}

// ParseRecipient parses one native age or SSH recipient.
func ParseRecipient(value string) (age.Recipient, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("recipient is empty")
	}
	if recipients, err := age.ParseRecipients(strings.NewReader(value)); err == nil && len(recipients) == 1 {
		return recipients[0], nil
	}

	recipient, err := agessh.ParseRecipient(value)
	if err != nil {
		return nil, fmt.Errorf("parse recipient: %w", err)
	}

	return recipient, nil
}

// ParseIdentity parses one native age identity or an SSH private key.
func ParseIdentity(value string) (age.Identity, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("identity is empty")
	}
	if identities, err := age.ParseIdentities(strings.NewReader(value)); err == nil && len(identities) == 1 {
		return identities[0], nil
	}

	identity, err := agessh.ParseIdentity([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}

	return identity, nil
}

// LoadIdentities reads native age and SSH identities from files.
//
// Encrypted SSH keys use the configured passphrase when one is provided.
// Native age identities and unencrypted SSH keys remain unaffected by the
// passphrase options.
func LoadIdentities(paths []string, opts IdentityOptions) ([]age.Identity, error) {
	if len(paths) == 0 {
		return nil, errors.New("no identity files configured")
	}
	if opts.Passphrase != "" && opts.PassphraseFile != "" {
		return nil, errors.New("identity passphrase and identity passphrase file are mutually exclusive")
	}

	passphrase, err := loadPassphrase(opts)
	if err != nil {
		return nil, err
	}

	var identities []age.Identity
	for _, path := range paths {
		data, err := readKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("read identity file %q: %w", path, err)
		}

		parsed, err := parseIdentities(data, passphrase)
		if err != nil {
			return nil, fmt.Errorf("parse identity file %q: %w", path, err)
		}

		identities = append(identities, parsed...)
	}

	return identities, nil
}

// parseRecipients accepts one recipient per line and keeps SSH parsing separate
// because the age parser intentionally excludes SSH recipients.
func parseRecipients(data []byte) ([]age.Recipient, error) {
	// Native age files can contain multiple entries and comments,
	// so let the/ upstream parser handle the complete file before trying SSH syntax.
	if recipients, err := age.ParseRecipients(bytes.NewReader(data)); err == nil {
		return recipients, nil
	}

	var recipients []age.Recipient
	// Fall back line by line only after the native parser rejects the file;
	// this keeps comments and multi-entry age files compatible with age itself.
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		recipient, err := ParseRecipient(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		recipients = append(recipients, recipient)
	}

	if len(recipients) == 0 {
		return nil, errors.New("no recipients found")
	}

	return recipients, nil
}

// parseIdentities accepts native age identity lines and PEM-encoded SSH keys.
func parseIdentities(data, passphrase []byte) ([]age.Identity, error) {
	// SSH private keys are multi-line PEM documents and must be parsed as a complete file.
	// Native age identities are attempted first
	// because they use the same one-entry-per-line file format as the age CLI.
	if identities, err := age.ParseIdentities(bytes.NewReader(data)); err == nil {
		return identities, nil
	}
	identity, err := agessh.ParseIdentity(data)
	if err == nil {
		return []age.Identity{identity}, nil
	}

	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		if len(passphrase) == 0 {
			return nil, ErrIdentityPassphraseRequired
		}

		identity, err := parseEncryptedSSHIdentity(data, passphrase)
		if err != nil {
			return nil, err
		}

		return []age.Identity{identity}, nil
	}

	var identities []age.Identity
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parsed, err := ParseIdentity(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}

		identities = append(identities, parsed)
	}

	if len(identities) == 0 {
		return nil, errors.New("no identities found")
	}

	return identities, nil
}

// parseEncryptedSSHIdentity decrypts an OpenSSH private key
// and converts its native key type to the corresponding age identity implementation.
func parseEncryptedSSHIdentity(data, passphrase []byte) (age.Identity, error) {
	key, err := ssh.ParseRawPrivateKeyWithPassphrase(data, passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypt SSH identity: %w", err)
	}

	var identity age.Identity
	switch key := key.(type) {
	case *ed25519.PrivateKey:
		identity, err = agessh.NewEd25519Identity(*key)

	case ed25519.PrivateKey:
		identity, err = agessh.NewEd25519Identity(key)

	case *rsa.PrivateKey:
		identity, err = agessh.NewRSAIdentity(key)

	default:
		return nil, fmt.Errorf("unsupported SSH identity type: %T", key)
	}
	if err != nil {
		return nil, fmt.Errorf("create age identity from SSH key: %w", err)
	}

	return identity, nil
}

// loadPassphrase resolves the mutually exclusive passphrase sources
// and preserves the exact passphrase except for a trailing line ending in a file.
func loadPassphrase(opts IdentityOptions) ([]byte, error) {
	if opts.Passphrase != "" {
		return []byte(opts.Passphrase), nil
	}
	if opts.PassphraseFile == "" {
		return nil, nil
	}

	data, err := readKeyFile(opts.PassphraseFile)
	if err != nil {
		return nil, fmt.Errorf("read identity passphrase file %q: %w", opts.PassphraseFile, err)
	}

	data = bytes.TrimSuffix(data, []byte("\n"))
	data = bytes.TrimSuffix(data, []byte("\r"))
	if len(data) == 0 {
		return nil, errors.New("identity passphrase file is empty")
	}

	return data, nil
}

// readKeyFile applies a size bound before returning key material to parsers.
func readKeyFile(path string) ([]byte, error) {
	expanded, err := fileutil.ExpandHome(path)
	if err != nil {
		return nil, fmt.Errorf("expand home directory: %w", err)
	}

	file, err := os.Open(expanded)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxKeyFileSize {
		return nil, fmt.Errorf("key file exceeds %d bytes", maxKeyFileSize)
	}

	return data, nil
}

// Write forwards plaintext to the age stream.
func (w *encryptWriter) Write(p []byte) (int, error) {
	return w.encrypted.Write(p)
}

// Close flushes encryption and then writes the armor footer.
func (w *encryptWriter) Close() error {
	if err := w.encrypted.Close(); err != nil {
		_ = w.armored.Close()
		return err
	}

	return w.armored.Close()
}
