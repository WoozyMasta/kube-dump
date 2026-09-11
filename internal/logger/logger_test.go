package logger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestParseLevelSupportsOff(t *testing.T) {
	t.Parallel()

	level, err := parseLevel("off")
	if err != nil {
		t.Fatalf("parseLevel() error = %v", err)
	}
	if level != zerolog.Disabled {
		t.Fatalf("parseLevel() = %v, want off", level)
	}
}

func TestResolveTimestampFormat(t *testing.T) {
	t.Parallel()

	if format, enabled := resolveTimestampFormat("none"); format != "" || enabled {
		t.Fatalf("resolveTimestampFormat(none) = %q, %v", format, enabled)
	}
	if format, enabled := resolveTimestampFormat("unixms"); format != zerolog.TimeFormatUnixMs || !enabled {
		t.Fatalf("resolveTimestampFormat(unixms) = %q, %v", format, enabled)
	}
}

func TestSetupCanTruncateFileOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kube-dump.log")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	writer, err := openOutput(path, true)
	if err != nil {
		t.Fatalf("openOutput() error = %v", err)
	}
	file, ok := writer.(*os.File)
	if !ok {
		t.Fatalf("openOutput() returned %T, want *os.File", writer)
	}
	defer file.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("Setup() did not truncate file: %q", data)
	}
}

func TestOpenOutputRejectsStdout(t *testing.T) {
	t.Parallel()

	if _, err := openOutput("stdout", false); err == nil {
		t.Fatal("openOutput(stdout) accepted a log sink that can corrupt command output")
	}
}

func TestFormatConsoleErrorUnquotesEscapedWindowsPath(t *testing.T) {
	raw := `read existing object ".tmp\test\metrics-server:system:auth-delegator.yaml":`
	if got := formatConsoleError(strconv.Quote(raw)); got != raw {
		t.Fatalf("formatConsoleError() = %q, want %q", got, raw)
	}
}

func TestFormatConsoleErrorEscapesControlCharacters(t *testing.T) {
	if got := formatConsoleError(strconv.Quote("line one\nline two")); got != `line one\nline two` {
		t.Fatalf("formatConsoleError() = %q, want %q", got, `line one\nline two`)
	}
}

func TestSetupTextFormatsWindowsErrorsWithoutDoubleEscaping(t *testing.T) {
	var output bytes.Buffer
	if err := SetupWithWriter(Options{
		Format:          TextFormat,
		Level:           Level("error"),
		TimestampFormat: TimestampFormat("none"),
	}, &output); err != nil {
		t.Fatalf("SetupWithWriter() error = %v", err)
	}

	raw := `read existing object ".tmp\test\metrics-server:system:auth-delegator.yaml":`
	log.Error().Err(errors.New(raw)).Msg("operation failed")
	got := output.String()
	if !strings.Contains(got, raw) {
		t.Fatalf("text log = %q, want unescaped error %q", got, raw)
	}
	if strings.Contains(got, `\"`) || strings.Contains(got, `\\`) {
		t.Fatalf("text log still contains JSON escaping: %q", got)
	}
}
