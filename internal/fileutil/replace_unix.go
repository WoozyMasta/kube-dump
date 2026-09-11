//go:build !windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

import (
	"os"
	"path/filepath"
)

// replaceFile publishes source with the atomic same-filesystem rename guaranteed by Unix filesystems.
func replaceFile(source, target string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	if err := os.Rename(source, target); err != nil {
		return err
	}

	return syncDirectory(filepath.Dir(target))
}
