//go:build windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

// Windows has no portable directory fsync operation.
// File replacement uses MoveFileEx with MOVEFILE_WRITE_THROUGH,
// so directory sync is handled by the platform replacement primitive where it is available.
func syncDirectory(string) error {
	return nil
}
