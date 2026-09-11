//go:build !windows

// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"io/fs"
	"reflect"
)

// hasMultipleHardLinks reads the link-count field exposed by Unix FileInfo implementations.
func hasMultipleHardLinks(_ string, info fs.FileInfo) (bool, error) {
	systemInfo := info.Sys()
	if systemInfo == nil {
		return false, nil
	}

	value := reflect.ValueOf(systemInfo)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false, nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false, nil
	}

	field := value.FieldByName("Nlink")
	return field.IsValid() && field.CanUint() && field.Uint() > 1, nil
}
