// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package fileutil contains small filesystem helpers shared by persistence
// implementations.
package fileutil

import (
	"os"
)

// ReplaceFile publishes source at target.
//
// Unix replaces the destination in one rename operation.
// Windows uses the platform replacement primitive in replace_windows.go.
// The source file is flushed before publication where the platform supports it.
// Callers must keep source and target in the same directory
// when they need the usual atomic publication guarantees.
func ReplaceFile(source, target string) error {
	if source == "" || target == "" {
		return os.ErrInvalid
	}

	return replaceFile(source, target)
}
