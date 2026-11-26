package core

import (
	"database/sql"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

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

func placeholders(n int) string   { return kernel.Placeholders(n) }
func int64Args(ids []int64) []any { return kernel.Int64Args(ids) }

func (t *tx) audit(e AuditEntry)            { t.Audit(e) }
func auditable(a *Actor, e AuditEntry) bool { return kernel.Auditable(a, e) }

func msTime(ms int64) time.Time          { return kernel.MSTime(ms) }
func ptrTime(n sql.NullInt64) *time.Time { return kernel.PtrTime(n) }
func boolInt(b bool) int                 { return kernel.BoolInt(b) }
