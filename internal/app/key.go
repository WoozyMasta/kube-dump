// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"filippo.io/age"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/keyring"
	"github.com/woozymasta/kube-dump/v2/internal/filelock"
)

// Execute generates a native age X25519 identity and writes it to the selected output.
// The private identity is intentionally emitted in age's standard text format
// so it can be consumed directly by --identity and by other age-compatible tools.
func (c *KeyGenerateCommand) Execute(_ []string) error {
	if c.output == nil && c.Output == "" {
		return errors.New("command output is not configured")
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate age identity: %w", err)
	}

	if c.Output == "" {
		if _, err := fmt.Fprintf(c.output, "# public key: %s\n%s\n", identity.Recipient(), identity); err != nil {
			return fmt.Errorf("write age identity: %w", err)
		}

		return nil
	}

	file, err := os.OpenFile(c.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create age identity file %q: %w", c.Output, err)
	}

	remove := true
	defer func() {
		if remove {
			_ = os.Remove(c.Output)
		}
	}()

	if _, err := fmt.Fprintf(file, "# public key: %s\n%s\n", identity.Recipient(), identity); err != nil {
		_ = file.Close()
		return fmt.Errorf("write age identity file %q: %w", c.Output, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect age identity file %q: %w", c.Output, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync age identity file %q: %w", c.Output, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close age identity file %q: %w", c.Output, err)
	}

	remove = false
	log.Info().
		Str("component", "keyring").
		Str("operation", "generate").
		Str("destination", c.Output).
		Msg("age identity generated")

	return nil
}

// Execute rotates the active AES-SIV key in an existing local keyring.
// Rotation changes only the key used for future values;
// historical keys remain available so existing encrypted fields can still be decrypted.
func (c *KeyRotateCommand) Execute(_ []string) error {
	return updateKeyring(c.context(), c.Positional.Directory, c.KeyringMaintenanceOptions, "rotate", func(
		keys *keyring.Keyring,
		recipients []age.Recipient,
		_ []age.Identity,
	) error {
		return keys.Rotate(recipients)
	})
}

// Execute re-encrypts all AES-SIV key envelopes for new age recipients.
// Rewrapping changes key protection only and does not rewrite resource files.
func (c *KeyRewrapCommand) Execute(_ []string) error {
	return updateKeyring(c.context(), c.Positional.Directory, c.KeyringMaintenanceOptions, "rewrap", func(
		keys *keyring.Keyring,
		recipients []age.Recipient,
		identities []age.Identity,
	) error {
		return keys.Rewrap(recipients, identities)
	})
}

// updateKeyring centralizes credential loading and prevents maintenance commands
// from accidentally initializing a keyring at a wrong or empty path.
func updateKeyring(
	ctx context.Context,
	directory string,
	options KeyringMaintenanceOptions,
	operation string,
	update func(*keyring.Keyring, []age.Recipient, []age.Identity) error,
) error {
	if ctx == nil {
		return errors.New("keyring context is required")
	}
	if directory == "" {
		return errors.New("keyring directory is required")
	}
	if update == nil {
		return errors.New("keyring update operation is required")
	}

	lockPath, err := resourceWriterLockPath(directory)
	if err != nil {
		return err
	}
	writerLock, err := filelock.Acquire(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("acquire keyring writer lock: %w", err)
	}
	defer func() { _ = writerLock.Close() }()
	if err := recoverResourceWriterState(directory); err != nil {
		return fmt.Errorf("recover resource writer state: %w", err)
	}

	identities, err := loadConfiguredIdentities(options.IdentityOptions)
	if err != nil {
		return fmt.Errorf("load keyring identities: %w", err)
	}
	if len(identities) == 0 {
		return errors.New("keyring maintenance requires --identity=PATH")
	}

	recipients, err := parseRecipients(
		options.Recipients,
		options.RecipientsFiles,
		options.RecipientsURLs,
		options.RecipientGitHubUsers,
	)
	if err != nil {
		return fmt.Errorf("load keyring recipients: %w", err)
	}
	if len(recipients) == 0 {
		return errors.New("keyring maintenance requires --recipient=RECIPIENT or --recipients-file=PATH")
	}

	keys, err := keyring.OpenExisting(directory, identities)
	if err != nil {
		return fmt.Errorf("open keyring: %w", err)
	}
	if err := update(keys, recipients, identities); err != nil {
		return fmt.Errorf("%s AES-SIV keyring: %w", operation, err)
	}

	log.Info().
		Str("component", "keyring").
		Str("operation", operation).
		Str("directory", directory).
		Msg("AES-SIV keyring maintenance completed")

	return nil
}
