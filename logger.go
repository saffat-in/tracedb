package tracedb

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// Logger is the logger to use in application.
var logger = zerolog.New(os.Stderr).With().Timestamp().Logger()

// Info logs the conn or sub/unsub action with a tag.
func Info(context, action string) {
	logger.Info().Str("context", context).Msg(action)
}

// // Error logs the error messages.
// func Error(context, err string) {
// 	logger.Error().Str("context", context).Msg(err)
// }

// Fatal logs the fatal error messages.
func Fatal(context, msg string, err error) {
	logger.Fatal().
		Err(err).
		Str("context", context).Msg(msg)
}

// Debug logs the debug message with tag if it is turned on.
func Debug(context, msg string) {
	logger.Debug().Str("context", context).Msg(msg)
}

// ParseLevel parses a string which represents a log level and returns
// a zerolog.Level.
func ParseLevel(level string, defaultLevel zerolog.Level) zerolog.Level {
	l := defaultLevel
	switch strings.ToLower(level) {
	case "0", "debug":
		l = zerolog.DebugLevel
	case "1", "info":
		l = zerolog.InfoLevel
	case "2", "warn":
		l = zerolog.WarnLevel
	case "3", "error":
		l = zerolog.ErrorLevel
	case "4", "fatal":
		l = zerolog.FatalLevel
	}
	return l
}
