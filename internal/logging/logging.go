package logging

import (
	"log"
)

// Errorf writes an error in red while leaving ordinary log output untouched.
// GoLand's Run console understands ANSI escape sequences.
func Errorf(logger *log.Logger, format string, args ...any) {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("\x1b[31mERROR: "+format+"\x1b[0m", args...)
}
