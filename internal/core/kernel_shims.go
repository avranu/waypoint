package core

import "git.jmann.me/claude/waypoint/internal/core/kernel"

// These are the old names of what moved to package kernel. A file that
// leaves package core uses kernel directly, new code in core should too,
// and this file is deleted when its last user is gone.

var (
	ErrNotFound     = kernel.ErrNotFound
	ErrForbidden    = kernel.ErrForbidden
	ErrUnauthorized = kernel.ErrUnauthorized
	ErrConflict     = kernel.ErrConflict
	ErrUnavailable  = kernel.ErrUnavailable
)

type (
	InvalidError   = kernel.InvalidError
	forbiddenError = kernel.ForbiddenError
)

// IsInvalid reports whether err is a validation error.
func IsInvalid(err error) bool { return kernel.IsInvalid(err) }

func invalid(format string, args ...any) error   { return kernel.Invalid(format, args...) }
func forbidden(format string, args ...any) error { return kernel.Forbidden(format, args...) }
func notFound(what string) error                 { return kernel.NotFound(what) }
func readFault(err error) bool                   { return kernel.ReadFault(err) }
func missingOrDenied(err error) bool             { return kernel.MissingOrDenied(err) }
func excerpt(s string, n int) string             { return kernel.Excerpt(s, n) }
