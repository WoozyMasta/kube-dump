// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}
	backslashPath := home + `\keys\recipient`
	if filepath.Separator == '\\' {
		backslashPath = filepath.Join(home, `keys\recipient`)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "home", path: "~", want: home},
		{name: "slash", path: "~/keys/recipient", want: filepath.Join(home, "keys", "recipient")},
		{name: "backslash", path: `~\keys\recipient`, want: backslashPath},
		{name: "relative", path: "keys/recipient", want: "keys/recipient"},
		{name: "other user", path: "~alice/keys/recipient", want: "~alice/keys/recipient"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExpandHome(test.path)
			if err != nil {
				t.Fatalf("ExpandHome() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ExpandHome(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}
