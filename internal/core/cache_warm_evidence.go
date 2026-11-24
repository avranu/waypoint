package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// Keep-warm evidence (WP-1000, WP-ADR95). Each keep-warm decision is kept
// in warm_decisions with the kind of wait it named (its class), and the
// hooks fill in what became of the session: its next real prompt, or its
// end (a /clear, an exit) before one. Whether a person or a session's own
// work comes back to it doesn't depend on whether it was pinged, so every
// decision of a class, cooled ones too, says how often a ping or a
// compaction would have paid for it.
//
// A ping pays when the session's next prompt comes within the hour it buys:
// it reads the context once (cache read) and saves rewriting it cold (1-hour
// cache write). A compaction pays when the session is used again at all: it
// reads the context and writes a summary (output), and saves rewriting the
// context less what the compaction keeps. So a ping needs a share of read /
// (write − read) of its class's sessions used within the hour, and a
// compaction of C tokens (read·C + output·warmCompactOut) / (write·(C −
// warmCompactKeeps)) used again at all. With enough decisions of the class
// in the person's last warmEvidenceDays days, a ping below its share becomes
// a compaction, and a compaction below its share lets the cache cool.
// Fewer than warmEvidenceMin, and the rules in decideWarm stand.
const (
	warmEvidenceDays = 28
	warmEvidenceMin  = 10
	// warmOpenAfter: a decision this old with no prompt or end since counts
	// as one the session wasn't used again after.
	warmOpenAfter = 3 * 24 * time.Hour
	// warmCompactKeeps is the context a compaction leaves (the first cache
	// write after one, 32–40k tokens, measured in X16 on 2026-11-12), and
	// warmCompactOut the summary it writes (untested: the summary's share
	// of what's left, the rest being the system prompt and tools).
	warmCompactKeeps = 36_000
	warmCompactOut   = 8_000
)

// Warm classes: the kind of wait a keep-warm decision named.
const (
	warmClassPaused       = "paused"
	warmClassClearable    = "clearable"
	warmClassWork         = "work"
	warmClassWorkCap      = "work_cap"
	warmClassUnattended   = "unattended"
	warmClassRequests     = "requests"
	warmClassRequestsAway = "requests_away"
	warmClassRequestsCap  = "requests_cap"
	warmClassChat         = "chat"
	warmClassChatCap      = "chat_cap"
	warmClassFinished     = "finished"
	warmClassUnmerged     = "unmerged"
	warmClassOther        = "other"
)

// warmAction is how warm_decisions names a decision's action.
func warmAction(d warmRecord) string {
	switch {
	case d.News:
		return "news"
	case d.State == warmPinged:
		return "ping"
	case d.State == warmCompacting:
		return "compact"
	}
	return "cool"
}

// noteWarmDecision keeps a decision as evidence (in recordWarm's write).
func (s *Service) noteWarmDecision(ctx context.Context, t *tx, sessionID, userID, at int64, d warmRecord) error {
	if d.Class == "" {
		return nil
	}
	rule := ""
	if d.Rule != "" {
		rule = warmAction(warmRecord{State: d.Rule})
	}
	_, err := t.ExecContext(ctx, `INSERT INTO warm_decisions (session_id, user_id, decided_at, action, rule, class, model, context_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, userID, at, warmAction(d), rule, d.Class, d.Model, d.Context)
	return err
}

// warmUsed: a real prompt started a turn; the session's open decisions were
// followed by a use.
func warmUsed(ctx context.Context, t *tx, sessionID, at int64) error {
	_, err := t.ExecContext(ctx, `UPDATE warm_decisions SET next_at = ? WHERE session_id = ? AND next_at IS NULL AND ended_at IS NULL`, at, sessionID)
	return err
}

// warmEnded: the session ended (a /clear, an exit) with decisions open: it
// wasn't used again after them.
func warmEnded(ctx context.Context, t *tx, sessionID, at int64, reason string) error {
	_, err := t.ExecContext(ctx, `UPDATE warm_decisions SET ended_at = ?, end_reason = ? WHERE session_id = ? AND next_at IS NULL AND ended_at IS NULL`,
		at, excerpt(reason, 40), sessionID)
	return err
}

// warmWeights are a model's prices as multiples of its input price.
type warmWeights struct {
	Read, Write, Output float64 // cache read, 1-hour cache write, output
}

// defaultWarmWeights: Anthropic's ratios, for a model with no price.
var defaultWarmWeights = warmWeights{Read: 0.1, Write: 2, Output: 5}

func weightsOf(p ModelPrice, ok bool) warmWeights {
	if !ok || p.Input <= 0 || p.CacheRead <= 0 || p.CacheWrite1h <= p.CacheRead || p.Output <= 0 {
		return defaultWarmWeights
	}
	return warmWeights{Read: p.CacheRead / p.Input, Write: p.CacheWrite1h / p.Input, Output: p.Output / p.Input}
}

func (s *Service) warmWeightsFor(ctx context.Context, model string) warmWeights {
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "prices for keep-warm", "err", err)
		return defaultWarmWeights
	}
	return weightsOf(book.find(model))
}

// pingNeeds is the share of sessions used within the hour at which a ping
// pays for itself.
func (w warmWeights) pingNeeds() float64 {
	return w.Read / (w.Write - w.Read)
}

// compactNeeds is the share of sessions used again at which compacting
// ctxTokens of context pays for itself (above 1: never).
func (w warmWeights) compactNeeds(ctxTokens int64) float64 {
	saved := float64(ctxTokens - warmCompactKeeps)
	if saved <= 0 {
		return math.Inf(1)
	}
	return (w.Read*float64(ctxTokens) + w.Output*warmCompactOut) / (w.Write * saved)
}

// compactFrom is the context from which a compaction pays at a use rate
// (0: at no size).
func (w warmWeights) compactFrom(rate float64) int64 {
	d := rate*w.Write - w.Read
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil((rate*w.Write*warmCompactKeeps + w.Output*warmCompactOut) / d))
}

// warmEvidence is what a class's past decisions say.
type warmEvidence struct {
	// Soon of SoonOf: used again within the hour a ping buys, of the
	// decisions at least that old.
	Soon, SoonOf int
	// Later of LaterOf: used again at all, of the decisions whose outcome
	// is known (or that have waited warmOpenAfter).
	Later, LaterOf int
}

func warmShare(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// warmEvidenceSQL counts a set of decisions; ? are now − cacheWarm, the
// hour, and now − warmOpenAfter. News released early doesn't count: it
// starts the next turn itself.
const warmEvidenceSQL = `coalesce(sum(decided_at <= ?), 0),
	coalesce(sum(decided_at <= ? AND next_at IS NOT NULL AND next_at - decided_at <= ?), 0),
	coalesce(sum(next_at IS NOT NULL OR ended_at IS NOT NULL OR decided_at <= ?), 0),
	coalesce(sum(next_at IS NOT NULL), 0)`

func (s *Service) warmEvidenceArgs() []any {
	now := s.nowMS()
	soon := now - cacheWarm.Milliseconds()
	return []any{soon, soon, cacheWarm.Milliseconds(), now - warmOpenAfter.Milliseconds()}
}

// warmEvidenceFor counts the person's decisions of a class.
func (s *Service) warmEvidenceFor(ctx context.Context, userID int64, class string) (warmEvidence, error) {
	var ev warmEvidence
	args := append(s.warmEvidenceArgs(), userID, class, s.nowMS()-warmEvidenceDays*24*time.Hour.Milliseconds())
	err := s.db.R.QueryRowContext(ctx, `SELECT `+warmEvidenceSQL+` FROM warm_decisions WHERE user_id = ? AND class = ? AND decided_at >= ? AND action != 'news'`,
		args...).Scan(&ev.SoonOf, &ev.Soon, &ev.LaterOf, &ev.Later)
	return ev, err
}

// weighWarm checks the rule's ping or compaction against its class's
// evidence: a ping that doesn't pay becomes a compaction, and a compaction
// that doesn't pay lets the cache cool.
func (s *Service) weighWarm(ctx context.Context, userID int64, model string, ctxTokens int64, c warmChoice) warmChoice {
	if c.Action == warmCool || c.Class == "" {
		return c
	}
	ev, err := s.warmEvidenceFor(ctx, userID, c.Class)
	if err != nil {
		s.log.WarnContext(ctx, "keep-warm evidence", "user", userID, "err", err)
		return c
	}
	w := s.warmWeightsFor(ctx, model)
	if c.Action == warmPinged {
		if ev.SoonOf < warmEvidenceMin || warmShare(ev.Soon, ev.SoonOf) >= w.pingNeeds() {
			return c
		}
		c.Action = warmCompacting
		c.Why += fmt.Sprintf("; compacted instead of a ping: in %d days %d of %d such waits were used within the hour (a ping pays from %s)",
			warmEvidenceDays, ev.Soon, ev.SoonOf, pctText(w.pingNeeds()))
	}
	if ev.LaterOf < warmEvidenceMin {
		return c
	}
	if need := w.compactNeeds(ctxTokens); warmShare(ev.Later, ev.LaterOf) < need {
		c.Action, c.Warm = warmCool, false
		from := "at no size"
		if n := w.compactFrom(warmShare(ev.Later, ev.LaterOf)); n > 0 {
			from = fmt.Sprintf("from %dk of context", n/1000)
		}
		c.Why += fmt.Sprintf("; let cool, not compacted: in %d days %d of %d such waits were used again (a compaction pays %s)",
			warmEvidenceDays, ev.Later, ev.LaterOf, from)
	}
	return c
}

// pctText: 2.6%, 35%.
func pctText(x float64) string {
	if x < 0.1 {
		return fmt.Sprintf("%.1f%%", x*100)
	}
	return fmt.Sprintf("%.0f%%", x*100)
}

// WarmReport is what keep-warm decided and what became of the sessions,
// per class, with the shares a ping and a compaction need (WP-1000).
type WarmReport struct {
	Who   string `json:"who"`
	Days  int    `json:"days"`
	Model string `json:"model"` // whose prices the shares use: the most decided-for model
	// PingNeeds: a ping pays when at least this share of a class's
	// sessions is used within the hour.
	PingNeeds float64 `json:"ping_needs"`
	// CompactKeeps and CompactOut are the compaction's assumed figures.
	CompactKeeps int               `json:"compact_keeps"`
	CompactOut   int               `json:"compact_out"`
	MinSample    int               `json:"min_sample"`
	Classes      []WarmClassFigure `json:"classes"`
}

// WarmClassFigure is one class's decisions and outcomes.
type WarmClassFigure struct {
	Class     string `json:"class"`
	Decisions int    `json:"decisions"`
	Pings     int    `json:"pings"`
	Compacts  int    `json:"compactions"`
	Cools     int    `json:"cools"`
	News      int    `json:"news"`
	// Changed: decisions where the evidence changed the rule's action.
	Changed int `json:"changed"`
	// The evidence, as weighWarm counts it (news aside): used within the
	// hour of decisions at least that old, used again at all of those
	// whose outcome is known.
	Soon    int `json:"used_within_hour"`
	SoonOf  int `json:"used_within_hour_of"`
	Later   int `json:"used_again"`
	LaterOf int `json:"used_again_of"`
	Cleared int `json:"cleared"` // ended by /clear before being used again
	Ended   int `json:"ended"`   // ended otherwise
	Open    int `json:"open"`
	// PingPays, CompactFrom: what the evidence makes of this class now
	// (nil, 0 with fewer than MinSample decisions: the rules stand).
	// CompactFrom -1 means at no size.
	PingPays    *bool `json:"ping_pays,omitempty"`
	CompactFrom int64 `json:"compact_from,omitempty"`
}

// WarmDecisions reports keep-warm decisions and outcomes.
func (s *Service) WarmDecisions(ctx context.Context, actor *Actor, q UsageQuery) (*WarmReport, error) {
	uids, everyone, label, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, err
	}
	if q.Days <= 0 {
		q.Days = warmEvidenceDays
	}
	q.Days = min(q.Days, 365)
	who, whoArgs := "1", []any{}
	if !everyone {
		who, whoArgs = "user_id = ?", []any{uids[0]}
	}
	since := s.nowMS() - int64(q.Days)*24*time.Hour.Milliseconds()
	where := who + " AND decided_at >= ?"
	args := append(whoArgs, since)
	rep := &WarmReport{Who: label, Days: q.Days, CompactKeeps: warmCompactKeeps, CompactOut: warmCompactOut, MinSample: warmEvidenceMin, Classes: []WarmClassFigure{}}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	_ = s.db.R.QueryRowContext(ctx, `SELECT model FROM warm_decisions WHERE `+where+` GROUP BY model ORDER BY count(*) DESC LIMIT 1`, args...).Scan(&rep.Model)
	w := s.warmWeightsFor(ctx, rep.Model)
	rep.PingNeeds = w.pingNeeds()
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT class, count(*), coalesce(sum(action = 'ping'), 0), coalesce(sum(action = 'compact'), 0), coalesce(sum(action = 'cool'), 0),
		coalesce(sum(action = 'news'), 0), coalesce(sum(rule != ''), 0),
		coalesce(sum(action != 'news' AND next_at IS NULL AND end_reason = 'clear'), 0), coalesce(sum(action != 'news' AND next_at IS NULL AND ended_at IS NOT NULL AND end_reason != 'clear'), 0),
		coalesce(sum(action != 'news' AND next_at IS NULL AND ended_at IS NULL), 0)
		FROM warm_decisions WHERE `+where+` GROUP BY class`, args...)
	if err != nil {
		return nil, err
	}
	byClass := map[string]*WarmClassFigure{}
	for rows.Next() {
		f := &WarmClassFigure{}
		if err := rows.Scan(&f.Class, &f.Decisions, &f.Pings, &f.Compacts, &f.Cools, &f.News, &f.Changed, &f.Cleared, &f.Ended, &f.Open); err != nil {
			rows.Close()
			return nil, err
		}
		byClass[f.Class] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The evidence, as weighWarm counts it (news aside).
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT class, `+warmEvidenceSQL+` FROM warm_decisions WHERE `+where+` AND action != 'news' GROUP BY class`,
		append(s.warmEvidenceArgs(), args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var class string
		var ev warmEvidence
		if err := rows.Scan(&class, &ev.SoonOf, &ev.Soon, &ev.LaterOf, &ev.Later); err != nil {
			rows.Close()
			return nil, err
		}
		if f := byClass[class]; f != nil {
			f.Soon, f.SoonOf, f.Later, f.LaterOf = ev.Soon, ev.SoonOf, ev.Later, ev.LaterOf
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range byClass {
		if f.SoonOf >= warmEvidenceMin {
			pays := warmShare(f.Soon, f.SoonOf) >= rep.PingNeeds
			f.PingPays = &pays
		}
		if f.LaterOf >= warmEvidenceMin {
			if f.CompactFrom = w.compactFrom(warmShare(f.Later, f.LaterOf)); f.CompactFrom == 0 {
				f.CompactFrom = -1
			}
		}
		rep.Classes = append(rep.Classes, *f)
	}
	sort.Slice(rep.Classes, func(i, j int) bool {
		a, b := rep.Classes[i], rep.Classes[j]
		if a.Decisions != b.Decisions {
			return a.Decisions > b.Decisions
		}
		return a.Class < b.Class
	})
	return rep, nil
}
