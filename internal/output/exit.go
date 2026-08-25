package output

import (
	"errors"
	"fmt"
)

// Exit codes, per DESIGN.md §9.1.
const (
	ExitOK       = 0 // success
	ExitGeneric  = 1 // anything not covered below
	ExitUsage    = 2 // bad flags or arguments
	ExitNotFound = 3 // the id, account or mailbox does not exist
	ExitOffline  = 4 // network needed but unreachable
	ExitProvider = 5 // the provider rejected the request
	ExitQueued   = 6 // write accepted locally and queued in the outbox
)

// Error codes used in the JSON error envelope. They are stable strings agents
// can branch on; the message is for humans.
const (
	CodeGeneric  = "error"
	CodeUsage    = "usage"
	CodeNotFound = "not_found"
	CodeOffline  = "offline"
	CodeProvider = "provider_error"
	CodeQueued   = "queued"
)

// CodeForExit maps an exit code to the JSON error code.
func CodeForExit(exit int) string {
	switch exit {
	case ExitUsage:
		return CodeUsage
	case ExitNotFound:
		return CodeNotFound
	case ExitOffline:
		return CodeOffline
	case ExitProvider:
		return CodeProvider
	case ExitQueued:
		return CodeQueued
	default:
		return CodeGeneric
	}
}

// ExitError carries the process exit code alongside the underlying error, so
// commands can `return &ExitError{...}` and main() can translate it.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Errorf builds an ExitError with a formatted message.
func Errorf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// ExitCodeOf returns the exit code an error should produce: the code from the
// first ExitError in the chain, ExitOK for nil, ExitGeneric otherwise.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitGeneric
}
