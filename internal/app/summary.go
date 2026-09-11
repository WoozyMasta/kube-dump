// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import "fmt"

// formatBytes formats byte counters for human-readable completion summaries.
func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	amount := float64(value)
	for _, unit := range units {
		amount /= 1024
		if amount < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", amount, unit)
		}
	}

	return fmt.Sprintf("%d B", value)
}
