package compress

import (
	"bytes"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte("kube-dump canonical state\n"), 128)
	for _, algorithm := range []Algorithm{Zstandard, Gzip} {
		t.Run(string(algorithm), func(t *testing.T) {
			compressed, err := encode(data, Config{Algorithm: algorithm})
			if err != nil {
				t.Fatalf("encode() error = %v", err)
			}
			reader, err := NewReader(bytes.NewReader(compressed), algorithm)
			if err != nil {
				t.Fatalf("NewReader() error = %v", err)
			}
			decoded, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				t.Fatalf("io.ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("reader.Close() error = %v", closeErr)
			}
			if !bytes.Equal(decoded, data) {
				t.Fatal("round-trip data differs")
			}
		})
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	t.Parallel()

	data := []byte("stable backup bytes")
	for _, algorithm := range []Algorithm{Zstandard, Gzip} {
		first, err := encode(data, Config{Algorithm: algorithm})
		if err != nil {
			t.Fatalf("first encode() error = %v", err)
		}
		second, err := encode(data, Config{Algorithm: algorithm})
		if err != nil {
			t.Fatalf("second encode() error = %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("%s output is not deterministic", algorithm)
		}
	}
}

func TestConfigRejectsUnsupportedCompression(t *testing.T) {
	t.Parallel()

	if err := (Config{Algorithm: "xz"}).Validate(); err == nil {
		t.Fatal("Config.Validate() accepted xz")
	}
	if err := (Config{Algorithm: Gzip, Level: 10}).Validate(); err == nil {
		t.Fatal("Config.Validate() accepted invalid gzip level")
	}
}

func TestNewWriterRejectsNilDestination(t *testing.T) {
	t.Parallel()

	if _, err := NewWriter(nil, Config{}); err == nil {
		t.Fatal("NewWriter() accepted nil destination")
	}
}

func TestDetectReaderPreservesPayload(t *testing.T) {
	t.Parallel()

	data := []byte("detect me")
	for _, algorithm := range []Algorithm{Zstandard, Gzip} {
		encoded, err := encode(data, Config{Algorithm: algorithm})
		if err != nil {
			t.Fatalf("encode() error = %v", err)
		}
		reader, detected, err := DetectReader(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("DetectReader() error = %v", err)
		}
		if detected != algorithm {
			t.Fatalf("DetectReader() algorithm = %q, want %q", detected, algorithm)
		}
		decoded, err := NewReader(reader, detected)
		if err != nil {
			t.Fatalf("NewReader() error = %v", err)
		}
		actual, err := io.ReadAll(decoded)
		_ = decoded.Close()
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatalf("detected payload = %q, error = %v", actual, err)
		}
	}
}

// encode compresses data and closes the stream so the final frame is flushed.
func encode(data []byte, config Config) ([]byte, error) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, config)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
