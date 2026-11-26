package kernel

import (
	"context"
	"database/sql"
	"time"
)

// Queryer is what a read and a write transaction share: the database, a
// connection or a transaction, so a helper can run in either.
type Queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// MSTime is Unix milliseconds as a UTC time.
func MSTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// PtrTime is a nullable Unix-milliseconds column as a UTC time, nil for NULL.
func PtrTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := MSTime(n.Int64)
	return &t
}

// BoolInt is a bool as 0 or 1, for a column that holds one.
func BoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
