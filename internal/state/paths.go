// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package state

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// EncodePathSegment escapes a Kubernetes identity segment
// for a portable filesystem path while preserving readable ASCII names whenever possible.
// The encoding also avoids Windows device names and is reversible with DecodePathSegment.
func EncodePathSegment(value string) string {
	if value == "" {
		return ""
	}

	var encoded strings.Builder
	encoded.Grow(len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		if isSafePathCharacter(character, index, len(value)) {
			encoded.WriteByte(character)
			continue
		}

		_, _ = fmt.Fprintf(&encoded, "%%%02X", character)
	}

	result := encoded.String()
	if isWindowsDeviceName(value) {
		return fmt.Sprintf("%%%02X%s", value[0], result[1:])
	}

	return result
}

// DecodePathSegment reverses EncodePathSegment and rejects malformed escapes.
func DecodePathSegment(value string) (string, error) {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return "", fmt.Errorf("decode path segment %q: %w", value, err)
	}

	return decoded, nil
}

// isSafePathCharacter keeps ordinary DNS-like identity names readable while escaping separators,
// Windows-reserved punctuation, and non-ASCII bytes.
func isSafePathCharacter(character byte, index, length int) bool {
	if character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '_' || character == '-' {
		return true
	}

	return character == '.' && index != 0 && index != length-1
}

// isWindowsDeviceName identifies names that Windows reserves even with a file extension.
func isWindowsDeviceName(value string) bool {
	value = strings.TrimRight(value, ". ")
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		value = value[:dot]
	}
	value = strings.ToUpper(value)

	if value == "CON" || value == "PRN" || value == "AUX" || value == "NUL" {
		return true
	}
	if len(value) != 4 || (value[:3] != "COM" && value[:3] != "LPT") {
		return false
	}

	return value[3] >= '1' && value[3] <= '9'
}

// IsCanonicalObjectPath reports whether name has the current resource-file layout.
// Canonical object files use group/version/resource/namespace/name.yaml,
// which lets Git stage kube-dump output without claiming unrelated files in the same repository.
func IsCanonicalObjectPath(name string) bool {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	parts := strings.Split(name, "/")

	if len(parts) != 5 || parts[0] == "." || parts[0] == "" {
		return false
	}

	for _, part := range parts {
		if part == "" ||
			part == "." ||
			part == ".." ||
			strings.HasPrefix(part, ".") ||
			strings.ContainsRune(part, '\x00') {
			return false
		}
	}

	for index, part := range parts {
		// These two names are reserved sentinels for the core API group
		// and cluster-scoped objects, so they are not ordinary encoded segments.
		if index == 0 && part == "core" || index == 3 && part == "_cluster" {
			continue
		}
		if index == len(parts)-1 {
			part = strings.TrimSuffix(part, ".yaml")
		}

		decoded, err := DecodePathSegment(part)
		if err != nil || EncodePathSegment(decoded) != part {
			return false
		}
	}

	return path.Ext(parts[4]) == ".yaml"
}
