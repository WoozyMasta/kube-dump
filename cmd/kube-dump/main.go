// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package main is the kube-dump process entry point
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/woozymasta/kube-dump/v2/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if err := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		stop()
		os.Exit(2)
	}

	stop()
}
