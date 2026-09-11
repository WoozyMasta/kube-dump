// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package compress provides version-independent streaming compression codecs.
package compress

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultAlgorithm is the preferred format for new artifacts.
	DefaultAlgorithm Algorithm = "zstd"
	// Zstandard is the default high-throughput compression format.
	Zstandard Algorithm = "zstd"
	// Gzip is the secondary widely supported artifact format.
	Gzip Algorithm = "gzip"
)

// Algorithm identifies an on-disk compression format.
// The value is also a flags-compatible text type so invalid formats fail during CLI parsing.
type Algorithm string

// Config controls compression format and optional codec level.
// A zero algorithm selects DefaultAlgorithm; a zero level selects the codec default.
type Config struct {
	// Algorithm selects the codec used for the stream.
	Algorithm Algorithm
	// Level controls codec effort; zero delegates to the codec default.
	Level int
}

// decoder adapts codecs with a no-error Close method to io.ReadCloser.
type decoder struct {
	// Reader supplies decompressed bytes to callers.
	io.Reader
	// closeFunc releases codec-specific resources.
	closeFunc func()
}

// UnmarshalText parses a compression algorithm from CLI or configuration text.
func (a *Algorithm) UnmarshalText(value []byte) error {
	parsed := Algorithm(string(value))
	if err := (Config{Algorithm: parsed}).Validate(); err != nil {
		return err
	}

	*a = parsed
	return nil
}

// Validate checks the selected algorithm and its optional level.
// Validation is codec-specific because zstd and gzip expose different legal level ranges.
func (c Config) Validate() error {
	algorithm := c.Algorithm
	if algorithm == "" {
		algorithm = DefaultAlgorithm
	}

	switch algorithm {
	case Zstandard:
		// Zstd accepts negative fast modes as well as positive compression levels,
		// so the range is intentionally wider than gzip's range.
		if c.Level != 0 && (c.Level < -5 || c.Level > 22) {
			return fmt.Errorf("zstd compression level %d is outside -5..22", c.Level)
		}

	case Gzip:
		// gzip constants include HuffmanOnly (-2),
		// which is a valid explicit choice even though the default level remains codec-selected.
		if c.Level != 0 && (c.Level < gzip.HuffmanOnly || c.Level > gzip.BestCompression) {
			return fmt.Errorf("gzip compression level %d is outside -2..9", c.Level)
		}

	default:
		return fmt.Errorf("unsupported compression algorithm %q", algorithm)
	}

	return nil
}

// DetectReader identifies a supported compressed stream by magic bytes
// and returns a reader that still contains the inspected prefix.
func DetectReader(source io.Reader) (io.Reader, Algorithm, error) {
	if source == nil {
		return nil, "", errors.New("compression source is required")
	}

	buffered := bufio.NewReader(source)
	peek, err := buffered.Peek(4)
	if err != nil && err != io.EOF {
		return nil, "", fmt.Errorf("inspect compression header: %w", err)
	}
	if len(peek) >= 2 && peek[0] == 0x1f && peek[1] == 0x8b {
		return buffered, Gzip, nil
	}
	if len(peek) == 4 && peek[0] == 0x28 && peek[1] == 0xb5 && peek[2] == 0x2f && peek[3] == 0xfd {
		return buffered, Zstandard, nil
	}

	return nil, "", errors.New("unsupported or uncompressed archive stream")
}

// Close releases decoder resources.
func (d *decoder) Close() error {
	d.closeFunc()
	return nil
}

// NewWriter creates a streaming compressor writing to destination.
// The returned closer must be closed to flush codec trailers and buffered output.
func NewWriter(destination io.Writer, config Config) (io.WriteCloser, error) {
	if destination == nil {
		return nil, errors.New("compression destination is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}

	algorithm := config.Algorithm
	if algorithm == "" {
		algorithm = DefaultAlgorithm
	}

	switch algorithm {
	case Zstandard:
		// Do not pass a level option for zero:
		// the library default is better than duplicating codec-specific default assumptions here.
		options := make([]zstd.EOption, 0, 1)
		if config.Level != 0 {
			options = append(options, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(config.Level)))
		}
		return zstd.NewWriter(destination, options...)

	case Gzip:
		if config.Level == 0 {
			return gzip.NewWriter(destination), nil
		}
		return gzip.NewWriterLevel(destination, config.Level)

	default:
		return nil, fmt.Errorf("unsupported compression algorithm %q", algorithm)
	}
}

// NewReader creates a streaming decompressor reading from source.
// The caller must close the returned reader even when the source reaches EOF
// so codec resources are released consistently.
func NewReader(source io.Reader, algorithm Algorithm) (io.ReadCloser, error) {
	if source == nil {
		return nil, errors.New("compression source is required")
	}

	switch algorithm {
	case "", Zstandard:
		reader, err := zstd.NewReader(source)
		if err != nil {
			return nil, err
		}
		return &decoder{Reader: reader, closeFunc: reader.Close}, nil

	case Gzip:
		return gzip.NewReader(source)

	default:
		return nil, fmt.Errorf("unsupported compression algorithm %q", algorithm)
	}
}
