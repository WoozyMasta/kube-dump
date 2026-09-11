// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"filippo.io/age"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
)

// loadConfiguredIdentities translates CLI identity settings into the crypto package contract.
// Keeping this boundary in app prevents command handlers from duplicating passphrase-source handling.
func loadConfiguredIdentities(options IdentityOptions) ([]age.Identity, error) {
	return agecrypto.LoadIdentities(options.IdentityFiles, agecrypto.IdentityOptions{
		Passphrase:     options.IdentityPassphrase,
		PassphraseFile: options.IdentityPassphraseFile,
	})
}
