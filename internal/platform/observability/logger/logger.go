// Package logger builds the application-wide structured logger (zerolog).
package logger

import (
	"os"

	"github.com/rs/zerolog"
)

// New returns a zerolog logger. In the "local" environment it writes
// human-readable console output; otherwise it emits JSON.
func New(level, env string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	var l zerolog.Logger
	if env == "local" {
		l = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout})
	} else {
		l = zerolog.New(os.Stdout)
	}
	return l.Level(lvl).With().Timestamp().Logger()
}
