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

// checkQuotaJump runs after a seven_day reading was stored. It compares it
// with the lowest reading of the same week in the last hour and, when the
// rise reaches the person's threshold and none was reported in the last
// hour, emits EvQuotaJump. A drop (the week rolled over, resets_at changed)
// never fires.
func (s *Service) checkQuotaJump(ctx context.Context, t *tx, uid, at int64, pct float64, resetsAt int64) error {
	u, err := s.userByID(ctx, s.db.R, uid)
	if err != nil {
		return nil
	}
	thr := u.Prefs.quotaJumpThreshold()
	if thr <= 0 {
		return nil
	}
	var low float64
	var lowAt int64
	err = t.QueryRowContext(ctx, `SELECT percent, at FROM quota_samples
		WHERE user_id = ? AND span = 'seven_day' AND resets_at = ? AND at >= ? AND at <= ?
		ORDER BY percent ASC, at DESC LIMIT 1`, uid, resetsAt, at-quotaJumpWindow.Milliseconds(), at).Scan(&low, &lowAt)
	if err != nil {
		return nil
	}
	rise := pct - low
	if rise <= 0 || rise < float64(thr) {
		return nil
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
	// usage_hours keeps hourly rows, so the busiest machine and repo since the low reading is the nearest cheap answer, not an exact one.
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
