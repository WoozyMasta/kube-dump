// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package siv provides the kube-dump AES-256-SIV boundary.
//
// The third-party implementation is intentionally imported only here.
// The package owns the key-size policy, canonical Secret AAD,
// and error boundary so the rest of kube-dump does not depend on a library-specific API.
package siv

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	aessiv "github.com/jedisct1/go-aes-siv"
)

const (
	// KeySize is the only supported AES-SIV key size: AES-256-SIV uses two independent 256-bit AES keys.
	KeySize = 64

	// payloadVersion identifies the current serialized AES-SIV payload contract.
	payloadVersion = "v1"
)

// Cipher encrypts and authenticates small field values with AES-256-SIV.
// The implementation never exposes the underlying third-party cipher type.
type Cipher struct {
	// cipher is the isolated AES-SIV implementation owned by this wrapper.
	cipher *aessiv.AESSIV
}

// EncodePayload serializes an authenticated ciphertext
// without exposing the underlying AES-SIV library format.
// Raw ciphertext is encoded with unpadded base64url
// so the value remains safe inside a YAML scalar.
func EncodePayload(keyID string, ciphertext []byte) (string, error) {
	if !validKeyID(keyID) {
		return "", fmt.Errorf("invalid AES-SIV key ID %q", keyID)
	}
	if len(ciphertext) < 16 {
		return "", errors.New("AES-SIV ciphertext is shorter than its authentication tag")
	}

	return payloadVersion + ":" + keyID + ":" + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// DecodePayload parses the stable kube-dump AES-SIV payload representation.
func DecodePayload(payload string) (string, []byte, error) {
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != payloadVersion || !validKeyID(parts[1]) || parts[2] == "" {
		return "", nil, errors.New("invalid AES-SIV payload")
	}

	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(ciphertext) < 16 {
		return "", nil, errors.New("invalid AES-SIV ciphertext payload")
	}

	return parts[1], ciphertext, nil
}

// New creates an AES-256-SIV cipher from exactly one 64-byte key.
// Key material is copied by the underlying implementation before returning.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("AES-256-SIV requires a %d-byte key", KeySize)
	}

	cipher, err := aessiv.New(key)
	if err != nil {
		return nil, fmt.Errorf("create AES-256-SIV cipher: %w", err)
	}

	return &Cipher{cipher: cipher}, nil
}

// Encrypt deterministically authenticates plaintext against one canonical AAD component.
// The returned value contains the RFC 5297 SIV tag followed by the encrypted plaintext
// and is independent of any caller-owned destination.
func (c *Cipher) Encrypt(plaintext, aad []byte) ([]byte, error) {
	if c == nil || c.cipher == nil {
		return nil, errors.New("AES-256-SIV cipher is nil")
	}

	return c.cipher.SealWithAssociatedDataList(nil, [][]byte{aad}, plaintext), nil
}

// Decrypt authenticates ciphertext against the canonical AAD component
// and returns plaintext only after the underlying implementation verifies it.
func (c *Cipher) Decrypt(ciphertext, aad []byte) ([]byte, error) {
	if c == nil || c.cipher == nil {
		return nil, errors.New("AES-256-SIV cipher is nil")
	}

	plaintext, err := c.cipher.OpenWithAssociatedDataList(nil, [][]byte{aad}, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("authenticate AES-256-SIV ciphertext: %w", err)
	}

	return plaintext, nil
}

// validKeyID reports whether id is a lowercase hexadecimal AES-SIV key ID.
func validKeyID(id string) bool {
	if len(id) == 0 || len(id) > 64 || strings.ToLower(id) != id {
		return false
	}

	for _, char := range id {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return true
}
