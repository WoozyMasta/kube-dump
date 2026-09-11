// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
)

// Execute encrypts an existing archive as a streaming age payload.
func (c *EncryptArchiveCommand) Execute(_ []string) error {
	if c.Positional.Input == "" || c.Positional.Output == "" {
		return errors.New("input and output archive paths are required")
	}
	if filepath.Clean(c.Positional.Input) == filepath.Clean(c.Positional.Output) {
		return errors.New("input and output archive paths must differ")
	}

	recipients, err := parseRecipients(
		c.Recipients,
		c.RecipientsFiles,
		c.RecipientsURLs,
		c.RecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("load archive recipients: %w", err)
	}

	input, err := os.Open(c.Positional.Input)
	if err != nil {
		return fmt.Errorf("open input archive: %w", err)
	}
	defer func() { _ = input.Close() }()

	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat input archive: %w", err)
	}
	counter := c.progress.NewCounter("Archive bytes", info.Size())
	defer counter.Complete()

	err = writeAtomic(c.Positional.Output, func(output io.Writer) error {
		writer, err := agecrypto.NewEncryptWriter(output, recipients, agecrypto.EncryptOptions{})
		if err != nil {
			return err
		}

		if _, err := io.Copy(writer, progress.TrackReader(input, counter)); err != nil {
			_ = writer.Close()
			return fmt.Errorf("encrypt archive: %w", err)
		}

		return writer.Close()
	})
	if err != nil {
		return fmt.Errorf("write encrypted archive: %w", err)
	}

	log.Info().
		Str("component", "archive").
		Str("operation", "encrypt").
		Str("destination", c.Positional.Output).
		Int64("bytes", info.Size()).
		Msg("archive encryption completed")

	return nil
}

// Execute decrypts an age-encrypted archive as a streaming payload.
func (c *DecryptArchiveCommand) Execute(_ []string) error {
	if c.Positional.Input == "" || c.Positional.Output == "" {
		return errors.New("input and output archive paths are required")
	}
	if filepath.Clean(c.Positional.Input) == filepath.Clean(c.Positional.Output) {
		return errors.New("input and output archive paths must differ")
	}

	identities, err := loadConfiguredIdentities(c.IdentityOptions)
	if err != nil {
		return fmt.Errorf("load archive identities: %w", err)
	}

	input, err := os.Open(c.Positional.Input)
	if err != nil {
		return fmt.Errorf("open encrypted archive: %w", err)
	}
	defer func() { _ = input.Close() }()

	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat encrypted archive: %w", err)
	}

	counter := c.progress.NewCounter("Archive bytes", info.Size())
	defer counter.Complete()

	reader, err := agecrypto.OpenDecryptReader(progress.TrackReader(input, counter), identities)
	if err != nil {
		return fmt.Errorf("open encrypted archive: %w", err)
	}

	if err := writeAtomic(c.Positional.Output, func(output io.Writer) error {
		_, err := io.Copy(output, reader)
		return err
	}); err != nil {
		return fmt.Errorf("write decrypted archive: %w", err)
	}

	log.Info().
		Str("component", "archive").
		Str("operation", "decrypt").
		Str("destination", c.Positional.Output).
		Int64("encrypted_bytes", info.Size()).
		Msg("archive decryption completed")

	return nil
}

// loadRecipientValues combines inline and external recipient sources.
func loadRecipientValues(values, files, urls, githubUsers []string) ([]string, error) {
	result := append([]string(nil), values...)
	loaded, err := agecrypto.LoadRecipientValues(files, urls, githubUsers)
	if err != nil {
		return nil, err
	}

	return append(result, loaded...), nil
}

// parseRecipients loads and parses all configured age recipient sources.
func parseRecipients(values, files, urls, githubUsers []string) ([]age.Recipient, error) {
	values, err := loadRecipientValues(values, files, urls, githubUsers)
	if err != nil {
		return nil, err
	}

	return parseRecipientValues(values)
}

// parseRecipientValues parses already loaded recipient text
// without fetching external sources a second time.
func parseRecipientValues(values []string) ([]age.Recipient, error) {
	recipients := make([]age.Recipient, 0, len(values))
	for _, value := range values {
		recipient, err := agecrypto.ParseRecipient(value)
		if err != nil {
			return nil, err
		}

		recipients = append(recipients, recipient)
	}

	return recipients, nil
}

// writeAtomic writes a result beside its destination and renames it on success.
func writeAtomic(path string, write func(io.Writer) error) error {
	if write == nil {
		return errors.New("atomic writer callback is required")
	}

	file, err := os.CreateTemp(filepath.Dir(path), ".kube-dump-output-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}

	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()

	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		return fmt.Errorf("set temporary output permissions: %w", err)
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary output: %w", err)
	}
	if err := fileutil.ReplaceFile(temporary, path); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}

	return nil
}
