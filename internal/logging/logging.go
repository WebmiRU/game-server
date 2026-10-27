package logging

import (
	"log"
	"os"
)

// Errorf writes errors to stderr in red, regardless of the destination of the
// normal logger. Ordinary log output remains on stdout.
func Errorf(logger *log.Logger, format string, args ...any) {
	if logger == nil {
		logger = log.Default()
	}

	errorLogger := log.New(os.Stderr, logger.Prefix(), logger.Flags())
	errorLogger.Printf("\x1b[31mERROR: "+format+"\x1b[0m", args...)
}
