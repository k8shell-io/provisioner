package log

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// JsonLogger controls whether the logger outputs in JSON format or console format.
// Set this to false to use a human-readable console format.
var JsonLogger = true

// NewLogger creates a new zerolog logger with a console output format.
func NewLogger(component string) *zerolog.Logger {
	var output = io.Writer(os.Stdout)
	if !JsonLogger {
		output = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	} else {
		output = os.Stdout
	}
	log := zerolog.New(output).
		With().
		Timestamp().
		Str("component", component).
		Logger()
	return &log
}
