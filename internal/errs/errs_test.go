package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestQuietKeepsTheExitCode(t *testing.T) {
	err := Quiet(fmt.Errorf("%w: details", ErrRolledBack))
	if !IsQuiet(err) || !errors.Is(err, ErrRolledBack) || ExitCode(err) != 3 {
		t.Errorf("quiet=%v is=%v code=%d", IsQuiet(err), errors.Is(err, ErrRolledBack), ExitCode(err))
	}
	if IsQuiet(ErrBlocked) || Quiet(nil) != nil {
		t.Error("plain errors are not quiet; Quiet(nil) is nil")
	}
	if got := ExitCode(Quiet(ErrRollbackFailed)); got != 4 {
		t.Errorf("code = %d", got)
	}
}
