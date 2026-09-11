// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

import "errors"

// SyncDirectory flushes directory metadata after a rename or removal.
// The platform implementation documents whether directory sync is available.
func SyncDirectory(path string) error {
	if path == "" {
		return errors.New("directory path is required")
	}

	return syncDirectory(path)
}
