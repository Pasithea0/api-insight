package logger

import (
	"io"
	"log"
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

// RouteStandardLog redirects the standard library log package output
// through zerolog so all existing log.Printf calls get structured output.
func RouteStandardLog() {
	log.SetFlags(0)
	log.SetOutput(&logWriter{logger: Log.With().Logger()})
}

// logWriter adapts zerolog.Logger as an io.Writer for the standard log package.
type logWriter struct {
	logger zerolog.Logger
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	// Trim trailing newline that log.Printf appends
	msg := string(p)
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	w.logger.Info().Msg(msg)
	return len(p), nil
}