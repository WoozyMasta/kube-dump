//go:build !windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import "os"

// applyArchiveOwner restores numeric ownership without following symlinks.
func applyArchiveOwner(root *os.Root, name string, entryType tarEntryType, uid, gid int) error {
	if entryType == tarEntrySymlink {
		return root.Lchown(name, uid, gid)
	}

	return root.Chown(name, uid, gid)
}
