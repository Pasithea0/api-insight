package logger

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
)

// Log is the package-level zerolog logger.
var Log zerolog.Logger

func init() {
	// Default: pretty console output in development
	output := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
	}

	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	Log = zerolog.New(output).
		Level(zerolog.InfoLevel).
		With().
		Timestamp().
		Logger()
}

// SetLevel adjusts the logging level at runtime.
func SetLevel(level string) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	Log = Log.Level(lvl)
}

// SetOutput changes the output writer (e.g., to a file).
func SetOutput(w io.Writer) {
	Log = Log.Output(w)
}

// SetPrettyOutput enables human-readable console output.
func SetPrettyOutput(w io.Writer) {
	Log = zerolog.New(zerolog.ConsoleWriter{
		Out:        w,
		TimeFormat: time.RFC3339,
	}).Level(Log.GetLevel()).With().Timestamp().Logger()
}