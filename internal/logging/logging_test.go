package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warning": slog.LevelWarn,
		"ERROR":   slog.LevelError,
	}
	for input, want := range tests {
		got, err := ParseLevel(input)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Fatal("ParseLevel(trace) unexpectedly succeeded")
	}
}

func TestNewProducesJSONAndFiltersByLevel(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger, err := New(&output, "warn")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("visible", "call_id", "example")

	got := output.String()
	if strings.Contains(got, "hidden") {
		t.Fatalf("info message was not filtered: %s", got)
	}
	if !strings.Contains(got, `"msg":"visible"`) || !strings.Contains(got, `"call_id":"example"`) {
		t.Fatalf("missing structured fields: %s", got)
	}
}
