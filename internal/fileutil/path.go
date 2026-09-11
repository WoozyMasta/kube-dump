// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome expands a leading `~` or `~/`/`~\` to the current user's home directory.
// Tilde-user syntax is intentionally left unchanged because Go's standard library
// does not provide portable account lookup for it.
func ExpandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}

	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, filepath.FromSlash(path[2:])), nil
	}

	if filepath.Separator != '\\' {
		// On Unix, a backslash is a literal character rather than a path separator.
		// Preserve the explicitly written `~\` form instead of silently changing it.
		return home + path[1:], nil
	}

	return filepath.Join(home, path[2:]), nil
}
