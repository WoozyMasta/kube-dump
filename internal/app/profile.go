// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/woozymasta/kube-dump/v2/internal/profile"
	profileschema "github.com/woozymasta/kube-dump/v2/pkg/profile/schema"
)

// Execute prints the embedded profile names and descriptions.
func (c *ProfileListCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("profile list output is required")
	}

	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	profiles, err := registry.List()
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(c.output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tSOURCE\tDESCRIPTION"); err != nil {
		return fmt.Errorf("write profile table header: %w", err)
	}

	for _, item := range profiles {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", item.Name, item.Source, item.Description); err != nil {
			return fmt.Errorf("write profile %q: %w", item.Name, err)
		}
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush profile table: %w", err)
	}

	return nil
}

// Execute prints the embedded JSON Schema for profile files unchanged.
func (c *ProfileSchemaCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("profile schema output is required")
	}

	data := profileschema.JSON()
	if _, err := c.output.Write(data); err != nil {
		return fmt.Errorf("write profile schema: %w", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		_, _ = io.WriteString(c.output, "\n")
	}

	return nil
}

// Execute prints a profile document resolved by built-in name first and then by filesystem path.
// The resolved document is emitted unchanged.
func (c *ProfileShowCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("profile show output is required")
	}
	if c.Positional.Name == "" {
		return errors.New("profile name or path is required")
	}

	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	data, err := registry.Resolve(c.Positional.Name)
	if err != nil {
		return err
	}

	if _, err := profile.LoadBytes(data); err != nil {
		return fmt.Errorf("validate profile %q: %w", c.Positional.Name, err)
	}
	if _, err := c.output.Write(data); err != nil {
		return fmt.Errorf("write profile %q: %w", c.Positional.Name, err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		_, _ = io.WriteString(c.output, "\n")
	}

	return nil
}

// Execute validates a built-in or filesystem profile and prints a concise result.
func (c *ProfileValidateCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("profile validation output is required")
	}
	if c.Positional.Path == "" {
		return errors.New("profile name or path is required")
	}

	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	data, err := registry.Resolve(c.Positional.Path)
	if err != nil {
		return err
	}
	if _, err := profile.LoadBytes(data); err != nil {
		return err
	}

	_, err = fmt.Fprintf(c.output, "profile %q is valid\n", c.Positional.Path)
	return err
}

// Execute prints the canonical source identifier for a profile.
func (c *ProfileWhichCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("profile which output is required")
	}

	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	path, err := registry.Which(c.Positional.Name)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(c.output, path)
	return err
}

// Execute copies a built-in profile into the global profile directory.
func (c *ProfileCopyCommand) Execute(_ []string) error {
	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	path, err := registry.Copy(c.Positional.Source, c.Positional.Name, c.Force)
	if err != nil {
		return err
	}

	return writeProfilePath(c.output, path)
}

// Execute validates and installs a profile into the global profile directory.
func (c *ProfileInstallCommand) Execute(_ []string) error {
	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	path, err := registry.Install(c.Positional.Source, c.Force)
	if err != nil {
		return err
	}

	return writeProfilePath(c.output, path)
}

// Execute edits a global profile and validates it after the editor exits.
func (c *ProfileEditCommand) Execute(_ []string) error {
	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	return registry.Edit(c.context(), c.Positional.Name)
}

// Execute removes a global profile by alias.
func (c *ProfileRemoveCommand) Execute(_ []string) error {
	registry, err := profile.NewRegistry(c.profileDirectory)
	if err != nil {
		return err
	}

	if err := registry.Remove(c.Positional.Name); err != nil {
		return err
	}

	return writeProfilePath(c.output, c.Positional.Name)
}

// writeProfilePath reports a successful profile file operation when output is configured.
func writeProfilePath(output io.Writer, path string) error {
	if output == nil {
		return nil
	}

	_, err := fmt.Fprintln(output, path)
	return err
}
