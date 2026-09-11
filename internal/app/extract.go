// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
)

// Execute decrypts and extracts one local resource archive.
func (c *ResourceExtractArchiveCommand) Execute(_ []string) error {
	return extractArchive(
		c.context(),
		c.Positional.Source,
		c.Positional.Destination,
		c.IdentityOptions,
		c.progress,
	)
}

// Execute decrypts and extracts one archive object from S3.
func (c *ResourceExtractArchiveS3Command) Execute(_ []string) error {
	if c.URI == "" || c.Positional.Destination == "" {
		return errors.New("S3 archive source and destination are required")
	}

	if _, _, err := archiveAlgorithm(c.URI); err != nil {
		return err
	}

	destination := c.S3Destination
	store, err := newS3Store(c.context(), destination)
	if err != nil {
		return fmt.Errorf("open S3 archive source: %w", err)
	}

	body, err := store.OpenObject(c.context(), store.ObjectKey())
	if err != nil {
		return fmt.Errorf("open S3 archive: %w", err)
	}
	defer func() { _ = body.Close() }()

	return extractArchiveReader(
		c.context(),
		c.URI,
		body,
		c.Positional.Destination,
		c.IdentityOptions,
		c.progress,
		0,
	)
}

// Execute decrypts and extracts one local PVC archive.
func (c *PvcExtractCommand) Execute(_ []string) error {
	return extractArchive(
		c.context(),
		c.Positional.Source,
		c.Positional.Destination,
		c.IdentityOptions,
		c.progress,
	)
}

// extractArchive opens an archive and applies its optional age envelope.
func extractArchive(
	ctx context.Context,
	source, destination string,
	identityOptions IdentityOptions,
	manager *progress.Manager,
) error {
	if source == "" || destination == "" {
		return errors.New("archive source and destination are required")
	}

	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat archive: %w", err)
	}

	return extractArchiveReader(ctx, source, input, destination, identityOptions, manager, info.Size())
}

// extractArchiveReader detects archive framing, decrypts it when required,
// and delegates path validation and extraction to the archive package.
func extractArchiveReader(
	ctx context.Context,
	source string,
	input io.Reader,
	destination string,
	identityOptions IdentityOptions,
	manager *progress.Manager,
	total int64,
) error {
	algorithm, encrypted, err := archiveAlgorithm(source)
	if err != nil {
		return err
	}

	log.Debug().
		Str("component", "archive").
		Str("operation", "extract").
		Str("algorithm", string(algorithm)).
		Bool("encrypted", encrypted).
		Msg("archive framing detected")

	counter := manager.NewCounter("Archive bytes", total)
	defer counter.Complete()

	trackedInput := progress.TrackReader(input, counter)
	sourceReader := trackedInput
	if encrypted {
		identities, err := loadConfiguredIdentities(identityOptions)
		if err != nil {
			return fmt.Errorf("load archive identities: %w", err)
		}

		sourceReader, err = agecrypto.OpenDecryptReader(trackedInput, identities)
		if err != nil {
			return fmt.Errorf("decrypt archive: %w", err)
		}
	}

	if _, err := archive.Extract(sourceReader, algorithm, destination, archive.ExtractOptions{}); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}

	log.Info().
		Str("component", "archive").
		Str("operation", "extract").
		Str("destination", destination).
		Str("algorithm", string(algorithm)).
		Bool("encrypted", encrypted).
		Msg("archive extraction completed")

	return ctx.Err()
}
