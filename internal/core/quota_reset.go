package core

import (
	"context"
	"time"
)

// A seven_day reading that falls a long way with resets_at unchanged is the
// allowance being reset mid-week (a free reset), not the week rolling over
// (resets_at moves then) and not jitter (the readings wobble a few points).
// It counts only when the readings after it stay low, so one low reading
// among high ones, and one high reading among low ones after a reset, are
// outliers: left out, and never fed to the week's running maximum.
//
// The values were set from the real readings in TestQuotaResetsInRealSeries
// (WP-1881): both resets there fell from 99 or 100 to 0, and single stray
// readings stood 12 and 53 points under their neighbours and one 99 above a
// reset. No replay over all stored readings has counted misses (WP-1902).
// A reset is confirmed by the one reading after it when that is all there
// is, so the pace is right from the second reading on; the answer is
// provisional, because the week is read again at every check and later
// readings that come back up undo it.
const (
	quotaResetDrop     = 30.0 // a fall of at least this many points
	quotaResetConfirm  = 3    // the readings after it looked at
	quotaResetNeedLow  = 2    // how many of them must stay low (fewer when fewer exist)
	quotaOutlierRise   = 30.0 // a rise of at least this many points is an outlier unless the readings after it stay up
	quotaResetStayBand = 30.0 // "low" is below the pre-drop value less this, or nearer where it fell to than where it fell from; "up" is above the jump less this
)

// quotaSample is one stored seven_day reading.
type quotaSample struct {
	at    int64
	pct   float64
	reset int64 // resets_at
}

// quotaReset is an allowance reset found in a week's readings.
type quotaReset struct {
	At  int64   // the first reading that fell
	Was float64 // the reading before it
}

// quotaResetsIn reads one week's readings (one resets_at, oldest first) and
// returns those that count (outliers left out, each reading's percent as
// stored) with the resets it found, each at the index in kept where the new
// segment starts. It is the one place that says what a reset is: the guard's
// week lines, the pace and the spend plan all read through it.
func quotaResetsIn(in []quotaSample) (kept []quotaSample, resets []quotaReset, startAt []int) {
	prev := -1.0
	for i, s := range in {
		if prev >= 0 {
			after := in[i+1:]
			switch {
			case prev-s.pct >= quotaResetDrop:
				// Use climbs straight after a reset, so a fall of just 30 could
				// never stay 30 under the old level: nearer the new level than
				// the old one is low too.
				low := max(prev-quotaResetStayBand, (prev+s.pct)/2)
				if stays(after, func(p float64) bool { return p < low }) {
					resets = append(resets, quotaReset{At: s.at, Was: prev})
					startAt = append(startAt, len(kept))
					kept = append(kept, s)
					prev = s.pct
				}
				continue
			case s.pct-prev >= quotaOutlierRise:
				if stays(after, func(p float64) bool { return p > s.pct-quotaResetStayBand }) {
					kept = append(kept, s)
					prev = s.pct
				}
				continue
			}
		}
		kept = append(kept, s)
		prev = s.pct
	}
	return kept, resets, startAt
}

// stays says whether the readings after a jump (the next few) mostly agree
// with it. With none after it, it is not confirmed yet.
func stays(after []quotaSample, ok func(float64) bool) bool {
	n := min(len(after), quotaResetConfirm)
	if n == 0 {
		return false
	}
	agree := 0
	for _, a := range after[:n] {
		if ok(a.pct) {
			agree++
		}
	}
	return agree >= min(quotaResetNeedLow, n)
}

// weekSamples reads the person's seven_day readings of the week resetting
// near reset, oldest first.
func (s *Service) weekSamples(ctx context.Context, uid, reset, since int64) ([]quotaSample, error) {
	hour := time.Hour.Milliseconds()
	rows, err := s.db.R.QueryContext(ctx, `SELECT at, percent, resets_at FROM quota_samples WHERE user_id = ? AND span = 'seven_day' AND resets_at BETWEEN ? AND ? AND at >= ? ORDER BY at, id`,
		uid, reset-hour, reset+hour, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotaSample
	for rows.Next() {
		var x quotaSample
		if err := rows.Scan(&x.at, &x.pct, &x.reset); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// lastQuotaReset is when the allowance of the week resetting near reset was
// last reset, 0 if it was not.
func (s *Service) lastQuotaReset(ctx context.Context, uid, reset int64) (int64, error) {
	r, err := s.lastQuotaResetInfo(ctx, uid, reset)
	if r == nil {
		return 0, err
	}
	return r.At, nil
}

// lastQuotaResetInfo is the week's last allowance reset (nil if none).
func (s *Service) lastQuotaResetInfo(ctx context.Context, uid, reset int64) (*quotaReset, error) {
	xs, err := s.weekSamples(ctx, uid, reset, reset-quotaWeek.Milliseconds())
	if err != nil {
		return nil, err
	}
	_, rs, _ := quotaResetsIn(xs)
	if len(rs) == 0 {
		return nil, nil
	}
	return &rs[len(rs)-1], nil
}

// quotaEven is an even pace's percent at t for a week that resets at
// resetsAt. The allowance restarted at evenFrom (the reset, or the week's
// start, where evenFrom is 0 or not inside the week): it is spread evenly
// from there to resetsAt. The guard's weekEvenAt and the autopilot gate's
// meter both read this, so they cannot disagree.
func quotaEven(evenFrom, resetsAt, t int64) float64 {
	start, length := resetsAt-quotaWeek.Milliseconds(), quotaWeek.Milliseconds()
	if evenFrom > start && resetsAt > evenFrom {
		start, length = evenFrom, resetsAt-evenFrom
	}
	return max(0, min(100, 100*float64(t-start)/float64(length)))
}

// guardResetsOnRead fills Resets of a check stored before resets were read:
// the resets in its window, and a note that its figures were worked out
// without them and are kept as stored.
func (s *Service) guardResetsOnRead(ctx context.Context, uid int64, c *QuotaGuardCheck) error {
	weeks, err := s.quotaWeekLines(ctx, uid, c.From.UnixMilli()-quotaWeek.Milliseconds(), c.To.UnixMilli())
	if err != nil {
		return err
	}
	c.Resets = []QuotaGuardReset{}
	for _, w := range weeks {
		for _, r := range w.resets {
			if r.At >= c.From.UnixMilli() && r.At < c.To.UnixMilli() {
				c.Resets = append(c.Resets, QuotaGuardReset{At: msTime(r.At).UTC(), WasPercent: round1(r.Was)})
			}
		}
	}
	if len(c.Resets) > 0 {
		c.ResetNote = "stored before allowance resets were read: its points and phases are kept as stored and may over-count the week; a new check handles the reset"
	}
	return nil
}
