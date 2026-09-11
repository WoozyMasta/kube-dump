//go:build windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

import "golang.org/x/sys/windows"

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

// replaceFile uses MoveFileEx so replacing an existing target
// does not expose a remove-then-rename gap where a failed publication could lose the old file.
func replaceFile(source, target string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}

	return windows.MoveFileEx(
		sourcePath,
		targetPath,
		moveFileReplaceExisting|moveFileWriteThrough,
	)
}
