// Package logging builds the process-wide slog.Logger.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options describes the requested log output.
type Options struct {
	// Level is one of debug|info|warn|error (already validated by config).
	Level string
	// Format is json or text (already validated by config).
	Format string
	// Writer overrides the destination; nil means os.Stdout.
	Writer io.Writer
}

// New returns a logger plus a function that flushes buffered output.
//
// JSON is the default because container logs are shipped to structured sinks;
// text is for local development. The handler is level-filtered at construction
// time so hot paths do not pay for disabled Debug calls beyond an atomic load.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	level := parseLevel(opts.Level)
	ho := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		h = slog.NewTextHandler(w, ho)
	} else {
		h = slog.NewJSONHandler(w, ho)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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
