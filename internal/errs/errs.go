// Package errs holds the sentinel errors that the CLI maps to exit codes.
package errs

import "errors"

var (
	// ErrBlocked: a BLOCK-level risk or precondition stopped the command (exit 2).
	ErrBlocked = errors.New("blocked by risk rule")
	// ErrRolledBack: the upgrade failed and was rolled back successfully (exit 3).
	ErrRolledBack = errors.New("upgrade failed and was rolled back")
	// ErrRollbackFailed: the upgrade failed and the rollback failed too (exit 4).
	ErrRollbackFailed = errors.New("upgrade failed and rollback failed")
	// ErrNotImplemented marks commands scheduled for a later milestone.
	ErrNotImplemented = errors.New("not implemented yet")
)

// ExitCode maps an error to the process exit code defined in PRD section 5.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrBlocked):
		return 2
	case errors.Is(err, ErrRolledBack):
		return 3
	case errors.Is(err, ErrRollbackFailed):
		return 4
	default:
		return 1
	}
}

// quiet wraps an error whose explanation was already printed for the user in
// a friendlier form; main sets the exit code but does not print it again.
type quiet struct{ error }

func (q quiet) Unwrap() error { return q.error }

// Quiet marks err as already reported.
func Quiet(err error) error {
	if err == nil {
		return nil
	}
	return quiet{err}
}

// IsQuiet reports whether err (or an error it wraps) was marked with Quiet.
func IsQuiet(err error) bool {
	var q quiet
	return errors.As(err, &q)
}
