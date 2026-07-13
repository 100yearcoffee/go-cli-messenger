package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns the JSON logger shared by the client and signaling service.
// Protocol payloads must not be passed to it because they may contain private data.
func New(output io.Writer, level string) (*slog.Logger, error) {
	parsed, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}

	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: parsed})
	return slog.New(handler), nil
}

func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", value)
	}
}
