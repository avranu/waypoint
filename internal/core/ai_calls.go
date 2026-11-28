package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// aiCallsKeep is how long AI request records are kept.
const aiCallsKeep = 30 * 24 * time.Hour

// aiCall is one request to record.
type aiCall struct {
	task      string
	pid       int64
	model     string
	fallbacks int
	paid      bool
	outcome   string
	priority  string
	err       error
	tokensIn  int64
	tokensOut int64
	ms        int64
	bar       string
	sent      int64
	status    int
}

// recordAICall keeps one request. Best effort: a failed write is logged, and
// the request's caller goes on.
func (s *Service) recordAICall(ctx context.Context, c aiCall) {
	status := c.status
	var le *llmError
	switch {
	case status != 0:
	case c.err == nil:
		status = 200
	case errors.As(c.err, &le):
		status = le.status
	}
	task := c.task
	if task == "" {
		task = "other"
	}
	if _, err := s.db.W.ExecContext(context.WithoutCancel(ctx), `INSERT INTO ai_calls(at, task, provider_id, model, fallbacks, paid, outcome, status, tokens_in, tokens_out, ms, sent, priority, bar)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.nowMS()-c.ms, task, c.pid, c.model, c.fallbacks, c.paid, c.outcome, status, c.tokensIn, c.tokensOut, c.ms, c.sent, c.priority, c.bar); err != nil {
		s.log.WarnContext(ctx, "AI request not recorded", "err", err)
	}
}

// pruneAICalls drops the records older than aiCallsKeep.
func (s *Service) pruneAICalls(ctx context.Context) {
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM ai_calls WHERE at < ?`, s.now().Add(-aiCallsKeep).UnixMilli()); err != nil {
		s.log.WarnContext(ctx, "prune AI request records", "err", err)
	}
}

// AIUsageQuery is what `wp ai usage` asks for.
type AIUsageQuery struct {
	Since, Until time.Time // zero Since: 24h ago; zero Until: now
	By           string    // task (default), model, outcome, status, hour, day, provider, priority, bar
	Task         string    // only this task's requests
}

// AIUsage is the AI requests of a window, grouped.
type AIUsage struct {
	Since time.Time    `json:"since"`
	Until time.Time    `json:"until"`
	By    string       `json:"by"`
	Total AIUsageRow   `json:"total"`
	Rows  []AIUsageRow `json:"rows"`
	// Keys is the AI gateway key's day now, as the gateway counts it.
	Keys []AIKeyDay `json:"keys"`
	// Kept is how far back the records go: older ones are pruned.
	KeptDays int `json:"kept_days"`
}

// AIUsageRow is one group's requests.
type AIUsageRow struct {
	Key       string `json:"key"`
	Requests  int    `json:"requests"`
	Sent      int    `json:"sent"` // HTTP requests, the client's retries included
	OK        int    `json:"ok"`
	Failed    int    `json:"failed"`
	RateLimit int    `json:"rate_limited"` // rate_limit and quota, and surplus requests refused
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
	AvgMS     int64  `json:"avg_ms"`
	MaxMS     int64  `json:"max_ms"`
}

// aiUsageGroups are the --by choices and the SQL key of each.
var aiUsageGroups = map[string]string{
	"task":     `task`,
	"model":    `model`,
	"outcome":  `outcome`,
	"status":   `CAST(status AS TEXT)`,
	"hour":     `strftime('%Y-%m-%d %H:00', at / 1000, 'unixepoch')`,
	"day":      `strftime('%Y-%m-%d', at / 1000, 'unixepoch')`,
	"provider": `coalesce((SELECT name FROM ai_providers p WHERE p.id = provider_id), CAST(provider_id AS TEXT))`,
	"priority": `CASE priority WHEN '' THEN 'normal' ELSE priority END`,
	"bar":      `CASE WHEN bar = '' THEN 'none' WHEN bar LIKE 'weakest=%' THEN substr(bar, 9, CASE instr(bar, ';') WHEN 0 THEN length(bar) ELSE instr(bar, ';') - 9 END) ELSE bar END`,
}

// AIUsageGroups are the --by choices, for usage messages.
func AIUsageGroups() []string {
	out := make([]string, 0, len(aiUsageGroups))
	for k := range aiUsageGroups {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AIUsage counts the AI requests of a window by task, model, outcome, HTTP
// status, hour, day or provider (admins; a permission token also needs
// server.read).
func (s *Service) AIUsage(ctx context.Context, actor *Actor, q AIUsageQuery) (*AIUsage, error) {
	if err := canReadServer(actor); err != nil {
		return nil, err
	}
	now := s.now()
	if q.Until.IsZero() || q.Until.After(now) {
		q.Until = now
	}
	if q.Since.IsZero() {
		q.Since = q.Until.Add(-24 * time.Hour)
	}
	if !q.Since.Before(q.Until) {
		return nil, invalid("since must be before until")
	}
	if q.By == "" {
		q.By = "task"
	}
	key, ok := aiUsageGroups[q.By]
	if !ok {
		return nil, invalid("by %q: say %s", q.By, strings.Join(AIUsageGroups(), ", "))
	}
	where := `at >= ? AND at <= ?`
	args := []any{q.Since.UnixMilli(), q.Until.UnixMilli()}
	if q.Task != "" {
		where += ` AND task = ?`
		args = append(args, q.Task)
	}
	cols := `count(*), coalesce(sum(sent), 0), coalesce(sum(outcome = 'ok'), 0), coalesce(sum(outcome != 'ok'), 0), coalesce(sum(outcome IN ('rate_limit', 'quota', 'refused')), 0),
		coalesce(sum(tokens_in), 0), coalesce(sum(tokens_out), 0), coalesce(CAST(avg(ms) AS INTEGER), 0), coalesce(max(ms), 0)`
	out := &AIUsage{Since: q.Since, Until: q.Until, By: q.By, Rows: []AIUsageRow{}, KeptDays: int(aiCallsKeep / (24 * time.Hour))}
	t := &out.Total
	t.Key = "total"
	if err := s.db.R.QueryRowContext(ctx, `SELECT `+cols+` FROM ai_calls WHERE `+where, args...).
		Scan(&t.Requests, &t.Sent, &t.OK, &t.Failed, &t.RateLimit, &t.TokensIn, &t.TokensOut, &t.AvgMS, &t.MaxMS); err != nil {
		return nil, err
	}
	order := `count(*) DESC, k`
	if q.By == "hour" || q.By == "day" {
		order = `k`
	}
	// #nosec G202 -- key and order are from aiUsageGroups and the two above, never from input
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+key+` AS k, `+cols+` FROM ai_calls WHERE `+where+` GROUP BY k ORDER BY `+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AIUsageRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.Sent, &r.OK, &r.Failed, &r.RateLimit, &r.TokensIn, &r.TokensOut, &r.AvgMS, &r.MaxMS); err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Keys = s.aiKeyDays(ctx)
	return out, nil
}

// String is the usage as a table, for `wp ai usage`.
func (u *AIUsage) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "AI requests %s to %s UTC, by %s\n", u.Since.UTC().Format("2006-01-02 15:04"), u.Until.UTC().Format("2006-01-02 15:04"), u.By)
	for _, k := range u.Keys {
		if k.Error != "" {
			fmt.Fprintf(&b, "%s key today: couldn't read it (%s)\n", k.Provider, k.Error)
			continue
		}
		limit := "no limit"
		if k.Limit > 0 {
			limit = fmt.Sprintf("of %d", k.Limit)
		}
		fmt.Fprintf(&b, "%s key today (UTC): %d requests %s", k.Provider, k.Used, limit)
		if k.ParkedUntil != nil {
			fmt.Fprintf(&b, "; used up, parked until %s", k.ParkedUntil.UTC().Format("15:04 UTC"))
		}
		b.WriteString("\n")
	}
	if u.Total.Requests == 0 {
		fmt.Fprintf(&b, "No AI requests in this window (records are kept %d days).\n", u.KeptDays)
		return b.String()
	}
	b.WriteString("\n")
	w := len(u.By)
	for _, r := range u.Rows {
		w = max(w, len(r.Key))
	}
	w = min(w, 48)
	fmt.Fprintf(&b, "%-*s %8s %6s %6s %7s %8s %10s %10s %8s %8s\n", w, u.By, "requests", "sent", "ok", "failed", "limited", "tokens in", "tokens out", "avg", "max")
	row := func(r AIUsageRow) {
		fmt.Fprintf(&b, "%-*s %8d %6d %6d %7d %8d %10d %10d %8s %8s\n", w, excerpt(r.Key, w), r.Requests, r.Sent, r.OK, r.Failed, r.RateLimit, r.TokensIn, r.TokensOut, msText(r.AvgMS), msText(r.MaxMS))
	}
	for _, r := range u.Rows {
		row(r)
	}
	if len(u.Rows) > 1 {
		row(u.Total)
	}
	return b.String()
}

// msText is a duration in ms, short: 850ms, 12.3s, 4m05s.
func msText(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%02ds", ms/60000, ms%60000/1000)
}
