package kernel

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
	ErrConflict     = errors.New("conflict")
	// ErrUnavailable: a service Waypoint stands in front of (an assistant's
	// door) didn't answer; a 502.
	ErrUnavailable = errors.New("unavailable")
)

// InvalidError is a validation failure with a message safe to show users.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Invalid is a validation failure (an *InvalidError) built from a format.
func Invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// IsInvalid reports whether err is a validation error.
func IsInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie)
}

// NotFound wraps ErrNotFound with what was not found.
func NotFound(what string) error { return fmt.Errorf("%s %w", what, ErrNotFound) }

// Forbidden is a refusal (403) with a message people can act on.
func Forbidden(format string, args ...any) error {
	return &ForbiddenError{fmt.Sprintf(format, args...)}
}

// ForbiddenError is the refusal Forbidden builds; it matches ErrForbidden.
type ForbiddenError struct{ msg string }

func (e *ForbiddenError) Error() string { return e.msg }
func (e *ForbiddenError) Unwrap() error { return ErrForbidden }
