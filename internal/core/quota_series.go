package core

import (
	"context"
	"math"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/quota"
)

// quotaLines reads the person's readings of a span taken in [since, before).
func quotaLines(ctx context.Context, q queryer, uid int64, span string, since, before int64) ([]*quota.Line, error) {
	rows, err := q.QueryContext(ctx, `SELECT at, percent, resets_at, session FROM quota_samples WHERE user_id = ? AND span = ? AND at >= ? AND at < ? ORDER BY at, id`,
		uid, span, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var readings []quota.Reading
	for rows.Next() {
		var r quota.Reading
		if err := rows.Scan(&r.At, &r.Pct, &r.Reset, &r.Session); err != nil {
			return nil, err
		}
		readings = append(readings, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return quota.LinesFrom(readings, quotaLength(span)), nil
}

func quotaLength(span string) time.Duration {
	for _, sp := range quotaSpans {
		if sp.span == span {
			return sp.length
		}
	}
	return quotaWeek
}

// currentQuota is the person's latest window of a span as of now (nil: no
// reading in a window's length), whether or not it has ended.
func (s *Service) currentQuota(ctx context.Context, uid int64, span string, now time.Time) (*quota.Line, error) {
	lines, err := quotaLines(ctx, s.db.R, uid, span, now.Add(-quotaLength(span)).UnixMilli(), math.MaxInt64)
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	return lines[len(lines)-1], nil
}
