package core

import (
	"context"
	"math"
	"time"
)

// AutopilotDay is one day of what autopilot and the person's agents did.
type AutopilotDay struct {
	Day      string  `json:"day"`       // YYYY-MM-DD in the viewer's time zone
	Finished int     `json:"finished"`  // tasks agents finished (task summaries left)
	MovedOn  int     `json:"moved_on"`  // of those, the ones autopilot moved the session on after
	Started  int     `json:"started"`   // sessions autopilot started a next task in
	SpendUSD float64 `json:"spend_usd"` // list-price spend, all sessions
}

// AutopilotActivity is the last days, oldest first, for the Autopilot page's charts.
type AutopilotActivity struct {
	Days []AutopilotDay `json:"days"`
}

// AutopilotActivity counts, per day over the last `days` (1-60), the tasks
// the person's agents finished, how many autopilot carried on, the next tasks
// it started and the spend at list price. `offsetMin` is the viewer's UTC
// offset in minutes, so days are theirs.
func (s *Service) AutopilotActivity(ctx context.Context, a *Actor, days, offsetMin int) (*AutopilotActivity, error) {
	if err := a.need(PermAgentsRead); err != nil {
		return nil, err
	}
	if days < 1 || days > 60 {
		days = 14
	}
	if offsetMin < -14*60 || offsetMin > 14*60 {
		offsetMin = 0
	}
	uid := a.id()
	off := int64(offsetMin) * 60_000
	const dayMS = int64(24 * time.Hour / time.Millisecond)
	now := s.nowMS()
	today := (now + off) / dayMS * dayMS
	first := today - int64(days-1)*dayMS
	out := &AutopilotActivity{Days: make([]AutopilotDay, 0, 60)}
	out.Days = out.Days[:min(max(days, 1), 60)]
	for i := range out.Days {
		out.Days[i].Day = time.UnixMilli(first + int64(i)*dayMS).UTC().Format("2006-01-02")
	}
	idx := func(ms int64) int {
		i := int((ms + off - first) / dayMS)
		if ms+off < first || i < 0 || i >= days {
			return -1
		}
		return i
	}
	since := first - off

	where, args := taskSummariesFor(a)
	// #nosec G202 -- where is built from constant clauses; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT ts.created_at, ts.autopilot FROM task_summaries ts WHERE ts.user_id = ? AND ts.created_at >= ?`+where, append([]any{uid, since}, args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at int64
		var auto int
		if err := rows.Scan(&at, &auto); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if i := idx(at); i >= 0 {
			out.Days[i].Finished++
			if auto != 0 {
				out.Days[i].MovedOn++
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	srows, err := s.db.R.QueryContext(ctx, `SELECT updated_at FROM autopilot_steps WHERE user_id = ? AND state = ? AND updated_at >= ?`, uid, StepStarted, since)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var at int64
		if err := srows.Scan(&at); err != nil {
			_ = srows.Close()
			return nil, err
		}
		if i := idx(at); i >= 0 {
			out.Days[i].Started++
		}
	}
	if err := srows.Err(); err != nil {
		_ = srows.Close()
		return nil, err
	}
	if err := srows.Close(); err != nil {
		return nil, err
	}

	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	hrows, err := s.db.R.QueryContext(ctx, `SELECT hour, model, sum(input), sum(output), sum(cache_write_5m), sum(cache_write_1h), sum(cache_read)
		FROM usage_hours WHERE user_id = ? AND hour >= ? GROUP BY hour, model`, uid, since)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var hour, in, outT, cw5, cw1, cr int64
		var model string
		if err := hrows.Scan(&hour, &model, &in, &outT, &cw5, &cw1, &cr); err != nil {
			return nil, err
		}
		p, ok := book.find(model)
		if !ok {
			continue
		}
		if i := idx(hour); i >= 0 {
			out.Days[i].SpendUSD += p.Cost(in, outT, cw5, cw1, cr)
		}
	}
	if err := hrows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Days {
		out.Days[i].SpendUSD = math.Round(out.Days[i].SpendUSD*100) / 100
	}
	return out, nil
}
