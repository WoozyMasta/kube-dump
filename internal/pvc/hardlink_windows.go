//go:build windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// hasMultipleHardLinks queries Windows' link count directly from the file handle.
func hasMultipleHardLinks(path string, _ fs.FileInfo) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &information,
	); err != nil {
		return false, err
	}

	return information.NumberOfLinks > 1, nil
}
