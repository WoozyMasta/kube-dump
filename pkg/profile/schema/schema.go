// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package schema exposes the generated JSON Schema for kube-dump profiles.
package schema

import _ "embed"

// profileSchema contains the generated profile contract.
//
//go:embed profile.schema.json
var profileSchema []byte

// JSON returns a copy of the generated profile JSON Schema.
func JSON() []byte {
	return append([]byte(nil), profileSchema...)
}
