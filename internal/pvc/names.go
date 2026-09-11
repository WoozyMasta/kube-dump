// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

// snapshotName returns a DNS-compatible prefix for a temporary snapshot.
func snapshotName(prefix string, _ Ref) string {
	if prefix == "" {
		prefix = "kube-dump"
	}

	return prefix + "-snapshot-"
}
