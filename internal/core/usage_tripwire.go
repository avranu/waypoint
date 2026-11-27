package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EvQuotaJump: the week's quota rose fast (WP-146). It reaches only its user.
const EvQuotaJump = "usage.quota_jump"

// NotifyQuota is the notification kind of a quota jump.
const NotifyQuota = "quota"

const (
	// DefaultQuotaJump is the rise, in points of the week, that trips the alert.
	DefaultQuotaJump = 4
	quotaJumpWindow  = time.Hour
	quotaJumpEvery   = time.Hour
)

// quotaJumpThreshold is the person's setting: unset means the default, 0 off.
func (p UserPrefs) quotaJumpThreshold() int {
	if p.QuotaJump == nil {
		return DefaultQuotaJump
	}
	return *p.QuotaJump
}

// checkQuotaJump runs after a seven_day reading was stored. It adds up what
// the week rose over the last hour and, when that reaches the person's
// threshold and none was reported in the last hour, emits EvQuotaJump. A
// reset gives nothing back and a reading from before one is no rise.
func (s *Service) checkQuotaJump(ctx context.Context, t *tx, uid, at int64, resetsAt int64) error {
	u, err := s.userByID(ctx, s.db.R, uid)
	if err != nil {
		return nil
	}
	thr := u.Prefs.quotaJumpThreshold()
	if thr <= 0 {
		return nil
	}
	// A day back finds a rise cheaply, inside the writer; the whole window
	// then says whether it is one, or a reading from before an older reset.
	rise, lowAt := 0.0, at
	for _, back := range []time.Duration{24 * time.Hour, quotaWeek} {
		lines, err := quotaLines(ctx, t, uid, "seven_day", at-back.Milliseconds(), at+1)
		if err != nil || len(lines) == 0 {
			return err
		}
		l := lines[len(lines)-1]
		if max(l.Reset-resetsAt, resetsAt-l.Reset) > time.Hour.Milliseconds() {
			return nil
		}
		rise, lowAt = 0.0, at
		// Knot 0 is the window's start, not a reading to rise from.
		for i := len(l.Knots) - 1; i > 1 && l.Knots[i-1].At >= at-quotaJumpWindow.Milliseconds(); i-- {
			if r := l.Rise(i); r > 0 {
				rise, lowAt = rise+r, l.Knots[i-1].At
			}
		}
		if rise <= 0 || rise < float64(thr) {
			return nil
		}
	}
	var last int64
	_ = t.QueryRowContext(ctx, `SELECT at FROM quota_alerts WHERE user_id = ?`, uid).Scan(&last)
	if last != 0 && at-last < quotaJumpEvery.Milliseconds() {
		return nil
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO quota_alerts(user_id, at) VALUES (?,?)
		ON CONFLICT(user_id) DO UPDATE SET at = excluded.at`, uid, at); err != nil {
		return err
	}
	mins := int((at - lowAt + 30_000) / 60_000)
	if mins < 1 {
		mins = 1
	}
	text := fmt.Sprintf("+%.0f%% of your week in %d min", rise, mins)
	var machine, repo string
	hour := lowAt - lowAt%3_600_000
	if t.QueryRowContext(ctx, `SELECT machine, repo FROM usage_hours WHERE user_id = ? AND hour >= ?
		GROUP BY machine, repo ORDER BY SUM(input + output + cache_write_5m + cache_write_1h) DESC LIMIT 1`, uid, hour).Scan(&machine, &repo) == nil && machine != "" {
		busy := machine
		if repo != "" {
			busy += ", " + repo + " session"
		}
		text += ". Busiest: " + busy
	}
	text += "."
	t.emit(&Event{Kind: EvQuotaJump, UserID: uid, New: text, Data: map[string]any{"rise": rise, "minutes": mins}})
	return nil
}

func (s *Service) notifyQuotaJump(ctx context.Context, e *Event) {
	s.notifyURL(ctx, e.UserID, e, NotifyQuota, "Weekly quota is climbing", strings.TrimSpace(e.New), "/agents?tab=sessions")
}
