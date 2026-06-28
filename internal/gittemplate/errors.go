package gittemplate

import (
	"errors"
	"fmt"
)

// resolveError wraps a deterministic git-template resolution failure that will
// never succeed on retry (malformed YAML, cycle, depth limit, name collision).
// Callers (internal/controller/scheduler.go) check IsResolveError and mark the
// run Failed instead of leaving it Pending for an infinite retry loop.
type resolveError struct {
	err error
}

func newResolveError(format string, args ...any) error {
	return &resolveError{err: fmt.Errorf(format, args...)}
}

func wrapResolveError(err error) error {
	return &resolveError{err: err}
}

func (e *resolveError) Error() string { return e.err.Error() }
func (e *resolveError) Unwrap() error { return e.err }

// IsResolveError reports whether err is a deterministic resolution error, as
// opposed to a transient fetch/network/credential error that should be retried.
func IsResolveError(err error) bool {
	if err == nil {
		return false
	}
	var re *resolveError
	return errors.As(err, &re)
}
