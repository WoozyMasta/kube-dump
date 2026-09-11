//go:build windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"errors"
	"os"
)

// applyArchiveOwner reports the unsupported Windows ownership contract explicitly.
func applyArchiveOwner(_ *os.Root, _ string, _ tarEntryType, uid, gid int) error {
	if uid == 0 && gid == 0 {
		return nil
	}

	return errors.New("restore numeric ownership is unsupported on Windows")
}
