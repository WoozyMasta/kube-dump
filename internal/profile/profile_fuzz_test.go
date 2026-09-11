// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"reflect"
	"testing"
)

func FuzzLoadBytes(f *testing.F) {
	for _, name := range []string{"backup", "export", "raw"} {
		data, err := Builtin(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		compiled, err := LoadBytes(data)
		if err != nil {
			return
		}

		if compiled == nil || compiled.Name() == "" {
			t.Fatal("successful profile load returned an invalid compiled profile")
		}

		repeated, err := LoadBytes(data)
		if err != nil {
			t.Fatalf("second profile load failed: %v", err)
		}
		if repeated.Name() != compiled.Name() ||
			!reflect.DeepEqual(repeated.SelectionDefaults(), compiled.SelectionDefaults()) ||
			!reflect.DeepEqual(repeated.PVCSelectionDefaults(), compiled.PVCSelectionDefaults()) ||
			!reflect.DeepEqual(repeated.ImageSelectionDefaults(), compiled.ImageSelectionDefaults()) {
			t.Fatal("profile compilation is not deterministic")
		}
	})
}
