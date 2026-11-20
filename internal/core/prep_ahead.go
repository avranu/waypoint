package core

import (
	"context"
	"time"
)

// Prep ahead (WP-906, WP-ADR89): while the gateway has spare capacity, PrepDue hands
// runners the tasks behind the front of each agent queue as well, and their
// preps are made at surplus priority. One that is still current when an agent
// takes the task spared a normal prep then; one made again first, or never
// taken, was capacity nobody else wanted.
//
// How deep to go is the evidence's call, by position behind the front: a
// position is dropped once at least prepAheadMinN of its preps are decided
// and the 95% upper bound of their use rate is under prepAheadUseBar. Until
// then it is tried (surplus costs nothing but spare capacity), and the
// window moves, so a position dropped comes back when old outcomes age out.
const (
	prepAheadMax    = 4
	prepAheadMinN   = 8
	prepAheadUseBar = 0.25
	// prepAheadWindow is how far back the outcomes that set the depth go,
	// and how long an open one waits before it counts as unused.
	prepAheadWindow = 30 * 24 * time.Hour
	// prepAheadMarkTTL is how long a runner has to upload a task PrepDue
	// handed out ahead before its prep counts as a front one.
	prepAheadMarkTTL = time.Hour
)

// prepAheadMark is where PrepDue last handed out a task.
type prepAheadMark struct {
	pos int // 1 = the first behind the front; 0 = at the front
	at  time.Time
}

// markPrepAhead notes that PrepDue handed out the task at pos behind the
// front (ahead) or at the front (not ahead, which replaces an older mark).
func (s *Service) markPrepAhead(issueID int64, pos int, ahead bool) {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	if !ahead {
		pos = 0
	}
	if s.prepAhead == nil {
		s.prepAhead = map[int64]prepAheadMark{}
	}
	now := s.now()
	for id, m := range s.prepAhead { // bounded: marks a runner never used
		if now.Sub(m.at) > prepAheadMarkTTL {
			delete(s.prepAhead, id)
		}
	}
	s.prepAhead[issueID] = prepAheadMark{pos: pos, at: now}
}

// takePrepAhead is where behind the front PrepDue handed the task out (0
// at the front), and whether it did lately at all; it uses the mark up.
func (s *Service) takePrepAhead(issueID int64) (pos int, marked bool) {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	m, ok := s.prepAhead[issueID]
	if !ok {
		return 0, false
	}
	delete(s.prepAhead, issueID)
	if s.now().Sub(m.at) > prepAheadMarkTTL {
		return 0, false
	}
	return m.pos, true
}

// prepAheadUse is one position's decided prep-ahead outcomes.
type prepAheadUse struct {
	Position int     `json:"position"`
	Used     int     `json:"used"`
	Redone   int     `json:"redone"`
	Unused   int     `json:"unused"`
	Refused  int     `json:"refused"` // no answer: not counted
	Failed   int     `json:"failed"`  // no answer: not counted
	Open     int     `json:"open"`
	UseLow   float64 `json:"use_low"`  // 95% interval of used/(used+redone+unused)
	UseHigh  float64 `json:"use_high"` //
	Dropped  bool    `json:"dropped"`  // the evidence says it doesn't pay
}

// prepAheadUses is each position's outcomes over the window.
func (s *Service) prepAheadUses(ctx context.Context) ([]prepAheadUse, error) {
	since := s.now().Add(-prepAheadWindow).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT position, outcome, COUNT(*) FROM prep_ahead
		WHERE (outcome = '' OR decided_at >= ?) AND position BETWEEN 1 AND ? GROUP BY position, outcome`, since, prepAheadMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]prepAheadUse, prepAheadMax)
	for n := range out {
		out[n].Position = n + 1
	}
	for rows.Next() {
		var pos, n int
		var outcome string
		if err := rows.Scan(&pos, &outcome, &n); err != nil {
			return nil, err
		}
		u := &out[pos-1]
		switch outcome {
		case "used":
			u.Used += n
		case "redone":
			u.Redone += n
		case "unused":
			u.Unused += n
		case "refused":
			u.Refused += n
		case "failed":
			u.Failed += n
		case "":
			u.Open += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for n := range out {
		u := &out[n]
		decided := u.Used + u.Redone + u.Unused
		u.UseLow, u.UseHigh = wilsonLower(u.Used, decided), wilsonUpper(u.Used, decided)
		u.Dropped = decided >= prepAheadMinN && u.UseHigh < prepAheadUseBar
	}
	return out, nil
}

// prepAheadDepth is how many tasks behind each queue's front to prepare:
// up to the first position the evidence drops.
func (s *Service) prepAheadDepth(ctx context.Context) int {
	uses, err := s.prepAheadUses(ctx)
	if err != nil {
		s.log.Info("prep ahead: outcomes unreadable", "err", err)
		return 0
	}
	return aheadDepth(uses)
}

func aheadDepth(uses []prepAheadUse) int {
	for _, u := range uses {
		if u.Dropped {
			return u.Position - 1
		}
	}
	return prepAheadMax
}

// notePrepAheadShown decides a task's open prep-ahead row when an agent is
// given its prep: used when it was ready and current, else unused.
func (s *Service) notePrepAheadShown(ctx context.Context, issueID int64, current bool) {
	outcome := "unused"
	if current {
		outcome = "used"
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE prep_ahead SET outcome = ?, decided_at = ? WHERE issue_id = ? AND outcome = ''`,
		outcome, s.nowMS(), issueID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// endPrepAhead decides a task's open prep-ahead row that got no answer
// (refused, failed).
func (s *Service) endPrepAhead(ctx context.Context, issueID int64, outcome string) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE prep_ahead SET outcome = ?, decided_at = ? WHERE issue_id = ? AND outcome = ''`,
		outcome, s.nowMS(), issueID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// sweepPrepAhead decides the open rows no agent will take: the task closed,
// or the window passed. Hourly.
func (s *Service) sweepPrepAhead(ctx context.Context) {
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE prep_ahead SET outcome = 'unused', decided_at = ?
		WHERE outcome = '' AND (created_at < ? OR issue_id IN (SELECT id FROM issues WHERE resolved_at IS NOT NULL))`,
		now, now-prepAheadWindow.Milliseconds()); err != nil {
		s.log.WarnContext(ctx, "prep ahead sweep", "err", err)
	}
}
