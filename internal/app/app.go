// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package app contains the application boundary for the kube-dump CLI.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/woozymasta/flags"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/logger"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	"github.com/woozymasta/kube-dump/v2/internal/version"
)

// Run parses command-line arguments, configures command output streams,
// and executes the selected command through the flags parser.
//
// Help, version, completion, and documentation requests
// are successful parser control-flow results and are returned as nil.
// Other parser and command failures retain their original error
// so the process entry point can decide how to render the diagnostic and exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if ctx == nil {
		return errors.New("application context is required")
	}
	launchedFromExplorer := flags.LaunchedFromExplorer()
	if launchedFromExplorer && len(args) == 0 {
		args = []string{"--help"}
	}

	var options Options
	options.ctx = ctx
	options.setStreams(stdout, os.Stdin, stderr)
	options.environment = flags.DetectEnvironment()

	containerImage := version.ContainerImage()
	options.Pvc.Save.Dir.Image = containerImage
	options.Pvc.Save.S3.Image = containerImage
	options.Pvc.Restore.Dir.Image = containerImage
	options.Pvc.Restore.S3.Image = containerImage

	parser, err := newParser(&options)
	if err != nil {
		err = fmt.Errorf("create command parser: %w", err)
		if stderr != nil {
			_, _ = fmt.Fprintln(stderr, err)
		}

		return err
	}

	_, parseErr := parser.ParseArgs(args)
	if launchedFromExplorer {
		_ = flags.WaitForEnter(stdout, os.Stdin, "\nPress Enter to exit...")
	}
	if parseErr != nil {
		// flags writes help, version, and parser diagnostics itself.
		// Only the two successful control-flow results must be converted to nil here.
		var flagError *flags.Error
		if errors.As(parseErr, &flagError) &&
			(flagError.Type == flags.ErrHelp || flagError.Type == flags.ErrVersion) {
			return nil
		}

		return parseErr
	}

	return nil
}

// validateParsedOptions checks cross-field constraints that depend on a parsed option value
// and therefore cannot be expressed by a flags tag.
func validateParsedOptions(opts *Options) error {
	if opts == nil {
		return errors.New("application options are required")
	}

	if opts.Resource.Save.FieldEncryption == fieldcrypto.AES256SIV &&
		len(opts.Resource.Save.IdentityFiles) == 0 {
		return errors.New("--field-encryption=aes-siv requires --identity=PATH")
	}

	return nil
}

// simplifyCommandError keeps detailed wrapping for internal diagnostics while presenting stable,
// actionable messages for errors that users can fix in CLI options.
func simplifyCommandError(err error) error {
	if errors.Is(err, agecrypto.ErrIdentityPassphraseRequired) {
		return agecrypto.ErrIdentityPassphraseRequired
	}

	return err
}

// newParser builds the application parser and installs the single command dispatch boundary
// used by all user-facing and internal commands.
//
// Global flags are parsed before command-local flags.
// Environment and config provisioning are enabled centrally,
// while individual safety-sensitive options opt out through their own struct tags.
// Logger setup happens in the handler so every command receives the same final logging configuration.
func newParser(opts *Options) (*flags.Parser, error) {
	parserOptions := flags.Options(flags.Default |
		flags.DetectShellFlagStyle |
		flags.DetectShellEnvStyle |
		flags.PrintHelpOnInputErrors |
		flags.HelpCommand |
		flags.VersionCommand |
		flags.CompletionCommand |
		flags.DocsCommand |
		flags.VersionFlag |
		flags.StrictPositionalArgs |
		flags.CommandChain |
		flags.ShowRepeatableInHelp |
		flags.KeepDescriptionWhitespace |
		flags.SetTerminalTitle |
		flags.DotEnv |
		flags.DotEnvFlags |
		flags.EnvProvisioning)

	parser := flags.NewParser(opts, parserOptions)

	parser.NamespaceDelimiter = "-"
	parser.SetEnvPrefix("KUBE_DUMP")
	parser.TerminalTitle = "kube-dump"

	parser.SetShortDescription("Save, read, inspect, and encrypt Kubernetes backup artifacts.")
	if err := configureDescriptions(parser, opts); err != nil {
		return nil, err
	}

	parser.SetVersion(version.Version)
	parser.SetVersionCommit(version.Commit)
	parser.SetVersionURL(version.URL)

	parser.CommandHandler = func(command flags.Commander, args []string) error {
		if command == nil {
			return nil
		}

		if err := validateParsedOptions(opts); err != nil {
			return simplifyCommandError(err)
		}

		runContext := retry.WithAttempts(opts.ctx, opts.RetryAttempts)
		manager := progress.New(progress.Config{
			Context: runContext,
			Output:  opts.stderr,
			Columns: opts.environment.TerminalColumns,
			Enabled: !commandDisablesProgress(command) && progress.ShouldEnable(progress.Policy{
				NoProgress:      opts.NoProgress,
				Stderr:          opts.stderr,
				LogOutput:       opts.Log.Output,
				LogFormat:       string(opts.Log.Format),
				LogLevel:        string(opts.Log.Level),
				TerminalColumns: opts.environment.TerminalColumns,
			}),
		})

		var loggerErr error
		if manager.Enabled() && (opts.Log.Output == "" || opts.Log.Output == "stderr") {
			loggerErr = logger.SetupWithWriter(opts.Log, manager.LogWriter())
		} else {
			loggerErr = logger.Setup(opts.Log)
		}
		if loggerErr != nil {
			manager.Close(loggerErr)
			return fmt.Errorf("configure logger: %w", loggerErr)
		}

		if setter, ok := command.(interface {
			setContext(context.Context)
		}); ok {
			setter.setContext(runContext)
		}
		if setter, ok := command.(interface {
			setStreams(io.Writer, io.Reader, io.Writer)
		}); ok {
			setter.setStreams(opts.output, opts.input, opts.stderr)
		}
		if setter, ok := command.(interface {
			setProgress(*progress.Manager)
		}); ok {
			setter.setProgress(manager)
		}

		commandErr := simplifyCommandError(command.Execute(args))
		manager.Close(commandErr)

		return commandErr
	}

	return parser, nil
}

// commandDisablesProgress reports whether a command opts out of progress rendering
// because its output is intended for a data pipeline.
func commandDisablesProgress(command flags.Commander) bool {
	_, disabled := command.(interface{ disableProgress() })
	return disabled
}
