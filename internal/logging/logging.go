package logging

import (
	"log"
	"os"
)

// Errorf writes error messages to stderr. Ordinary application logs belong on stdout;
// the console decides how to visually distinguish stderr from stdout.
func Errorf(logger *log.Logger, format string, args ...any) {
	if logger == nil {
		logger = log.Default()
	}

	errorLogger := log.New(os.Stderr, logger.Prefix(), logger.Flags())
	errorLogger.Printf("ERROR: "+format, args...)
}
