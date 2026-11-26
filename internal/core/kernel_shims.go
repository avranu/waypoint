package core

import (
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
func cmp(a, b string) string               { return kernel.FirstNonEmpty(a, b) }
func msTime(ms int64) time.Time            { return kernel.MillisToTime(ms) }
func boolInt(b bool) int                   { return kernel.BoolInt(b) }
func nullInt(v *int64) any                 { return kernel.NullInt(v) }
func itoa64(n int64) string                { return kernel.Itoa64(n) }
func deref(p *string) string               { return kernel.TrimDeref(p) }
func short(sha string) string              { return kernel.ShortSHA(sha) }
func plural(n int) string                  { return kernel.PluralS(n) }
func plural1(n int, one string) string     { return kernel.CountNoun(n, one) }
func truncateBytes(s string, n int) string { return kernel.TruncateBytes(s, n) }
func sameSHA(a, b string) bool             { return kernel.SameSHA(a, b) }
func clipRunes(s string, n int) string     { return kernel.ClipRunes(s, n) }
func ptr[T any](v T) *T                    { return kernel.Ptr(v) }
