package core

import (
	"context"
	"database/sql"
	"time"
)

// lineExec is what a write to merge_line_spans needs: a transaction or the
// writer connection.
type lineExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// openLineSpan records that a pull request took its place in the merge line
// at at (WP-1910). queued_at keeps only the present membership; the spans keep
// the history. A pull request already in the line keeps its span.
func openLineSpan(ctx context.Context, x lineExec, prID int64, at time.Time) error {
	_, err := x.ExecContext(ctx, `INSERT INTO merge_line_spans(pr_id, joined_at)
		SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM merge_line_spans WHERE pr_id = ? AND left_at IS NULL)`, prID, at.UnixMilli(), prID)
	return err
}

// closeLineSpan records that a pull request left the line, and why.
func closeLineSpan(ctx context.Context, x lineExec, prID int64, at time.Time, why string) error {
	_, err := x.ExecContext(ctx, `UPDATE merge_line_spans SET left_at = ?, why = ? WHERE pr_id = ? AND left_at IS NULL`, at.UnixMilli(), why, prID)
	return err
}
