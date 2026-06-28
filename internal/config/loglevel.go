package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// ParseLogLevel converts "debug"/"info"/"warn"/"error" (case-insensitive,
// surrounding whitespace trimmed) into a slog.Level. An empty string is
// treated as "info". Any other value returns an error.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want debug, info, warn, or error)", s)
	}
}
