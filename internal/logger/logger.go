// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package logger configures the application logger from CLI options.
package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/flags"
	"k8s.io/klog/v2"
)

const (
	// TextFormat renders human-readable console events.
	TextFormat Format = "text"
	// JSONFormat renders one structured event per line.
	JSONFormat Format = "json"
)

// Options holds CLI flags for log configuration.
type Options struct {
	Output          string          `long:"output" description:"Write logs to stderr or this file; stdout is reserved for command data" default:"stderr"`
	Format          Format          `long:"format" description:"Log encoding" default:"text" choices:"json;text"`
	Level           Level           `long:"level" description:"Minimum severity to emit; lower levels expose more pipeline context" default:"info" choices:"trace;debug;info;warn;error;off" short:"l"`
	TimestampFormat TimestampFormat `long:"timestamp" description:"Timestamp format in emitted events" default:"rfc3339" choices:"rfc3339;unix;unixms;none"`
	FileTruncate    bool            `long:"file-truncate" description:"Replace an existing log file instead of appending to it"`
}

// Format selects the logger output encoding.
type Format string

// TimestampFormat selects the timestamp representation in log events.
type TimestampFormat string

// Level selects the global zerolog threshold; off disables logging.
type Level string

// UnmarshalText parses a logger output format.
func (f *Format) UnmarshalText(value []byte) error {
	parsed := Format(string(value))
	if parsed != TextFormat && parsed != JSONFormat {
		return fmt.Errorf("unsupported log format %q", parsed)
	}

	*f = parsed
	return nil
}

// UnmarshalText validates a logger level.
func (l *Level) UnmarshalText(value []byte) error {
	parsed := Level(string(value))
	if _, err := parseLevel(parsed); err != nil {
		return err
	}

	*l = parsed
	return nil
}

// UnmarshalText validates a timestamp format.
func (f *TimestampFormat) UnmarshalText(value []byte) error {
	parsed := TimestampFormat(string(value))

	switch parsed {
	case "none", "rfc3339", "unix", "unixms":
		*f = parsed
		return nil

	default:
		return fmt.Errorf("unsupported timestamp format %q", parsed)
	}
}

// Setup configures the global zerolog logger from opts.
//
// The output sink is opened before global zerolog state changes.
// A failed configuration therefore does not silently replace a working logger,
// while successful setup installs either structured JSON or terminal-oriented text.
func Setup(opts Options) error {
	return setup(opts, nil)
}

// SetupWithWriter configures the logger with an already wrapped output sink.
// The application uses it when progress is active
// so terminal log records are printed above the bars
// and the renderer can redraw the current frame.
func SetupWithWriter(opts Options, output io.Writer) error {
	return setup(opts, output)
}

// setup applies logger options and optionally replaces the configured sink.
func setup(opts Options, outputOverride io.Writer) error {
	// Open the sink before changing global zerolog state
	// so a failed file path leaves the previous logger configuration untouched.
	writer := outputOverride
	if writer == nil {
		var err error
		writer, err = openOutput(opts.Output, opts.FileTruncate)
		if err != nil {
			return fmt.Errorf("open log output %q: %w", opts.Output, err)
		}
	}

	level, err := parseLevel(opts.Level)
	if err != nil {
		return err
	}

	zerolog.SetGlobalLevel(level)

	timeFormat, timestampEnabled := resolveTimestampFormat(opts.TimestampFormat)
	if opts.Format == JSONFormat {
		// JSON keeps one event per line and never adds terminal decoration.
		zerolog.TimeFieldFormat = timeFormat
		log.Logger = zerolog.New(writer)
		if timestampEnabled {
			log.Logger = log.Logger.With().Timestamp().Logger()
		}
		installKubernetesLogger()

		return nil
	}

	console := zerolog.ConsoleWriter{
		Out:                 writer,
		TimeFormat:          timeFormat,
		NoColor:             !shouldColor(writer),
		FormatErrFieldValue: formatConsoleError,
	}

	// ConsoleWriter otherwise reserves a timestamp column even
	// when the timestamp field is intentionally disabled.
	if !timestampEnabled {
		console.PartsExclude = append(console.PartsExclude, zerolog.TimestampFieldName)
	}

	log.Logger = zerolog.New(console)
	if timestampEnabled {
		log.Logger = log.Logger.With().Timestamp().Logger()
	}
	installKubernetesLogger()

	return nil
}

// formatConsoleError removes the quoting already added by ConsoleWriter
// before it formats an error value.
// This keeps Windows paths readable in text logs
// without changing the escaped representation used by JSON logs.
func formatConsoleError(value any) string {
	text := fmt.Sprint(value)
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}

	// Keep an error on one physical log line even when an underlying error
	// contains control characters.
	text = strings.ReplaceAll(text, "\r", "\\r")
	text = strings.ReplaceAll(text, "\n", "\\n")
	text = strings.ReplaceAll(text, "\t", "\\t")

	return text
}

// Default sets up a text/stderr/info logger before CLI parsing.
// It deliberately ignores setup errors because stderr
// is always available in the normal process environment and logging must not prevent help output.
func Default() {
	_ = Setup(Options{
		Format:          "text",
		Output:          "stderr",
		Level:           "info",
		TimestampFormat: "rfc3339",
	})
}

// openOutput resolves standard streams or opens a private file sink.
func openOutput(output string, truncate bool) (io.Writer, error) {
	switch output {
	case "", "stderr":
		// stderr is the default so stdout remains available
		// for future data streams and machine-readable command output.
		return os.Stderr, nil

	case "stdout":
		return nil, errors.New("stdout is reserved for command output; use stderr or a file for logs")

	default:
		// Log files are private by default;
		// append preserves history unless the operator explicitly requests truncation.
		openFlags := os.O_CREATE | os.O_WRONLY
		if truncate {
			openFlags |= os.O_TRUNC
		} else {
			openFlags |= os.O_APPEND
		}

		return os.OpenFile(output, openFlags, 0o600)
	}
}

// parseLevel handles the off level in addition to zerolog's named levels.
func parseLevel(value Level) (zerolog.Level, error) {
	if value == Level("off") {
		return zerolog.Disabled, nil
	}

	level, err := zerolog.ParseLevel(string(value))
	if err != nil {
		return zerolog.NoLevel, fmt.Errorf("parse log level %q: %w", value, err)
	}

	return level, nil
}

// resolveTimestampFormat maps a CLI choice to zerolog settings.
func resolveTimestampFormat(value TimestampFormat) (string, bool) {
	switch value {
	case "none":
		return "", false

	case "unix":
		return zerolog.TimeFormatUnix, true

	case "unixms":
		return zerolog.TimeFormatUnixMs, true

	case "", "rfc3339":
		return time.RFC3339, true

	default:
		return time.RFC3339, true
	}
}

// shouldColor reports whether the selected output supports terminal colors.
func shouldColor(writer io.Writer) bool {
	return flags.DetectColorSupport(writer)
}

// installKubernetesLogger routes client-go and klog events through zerolog.
// Kubernetes libraries write through the logr interface,
// so this keeps their warnings in the same sink and encoding as application events.
func installKubernetesLogger() {
	klog.SetLogger(zerologr.New(&log.Logger))
}
