package observability

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger. Output is JSON so container logs are
// machine-readable; level comes from config (KILN_LOG_LEVEL).
func NewLogger(level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: ParseLevel(level),
	}))
}

// ParseLevel maps a config string to a slog level, defaulting to info for
// anything unrecognized rather than failing startup over a typo.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
