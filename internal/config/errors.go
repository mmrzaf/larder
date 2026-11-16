package config

import (
	"fmt"
	"strings"
)

// Errors is the aggregate of every configuration problem found in one load.
type Errors []error

// Error joins all messages, one per line.
func (e Errors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "\n")
}

// Unwrap exposes the individual errors to errors.Is and errors.As.
func (e Errors) Unwrap() []error { return e }

func (e *Errors) add(format string, args ...any) {
	*e = append(*e, fmt.Errorf(format, args...))
}
