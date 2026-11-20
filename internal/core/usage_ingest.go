package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Usage analytics (WP-D20): what `wp usage sync` and `wp statusline` report
// about a person's coding-agent sessions. The CLI reads Claude Code's own
// transcripts and sends compact, hour-bucketed counts, so the agent spends
// nothing and reports nothing about itself. All of it belongs to one
// person; they (and admins) read it back.

// UsageMessageRow counts assistant messages in one hour of one session.
// Counts may be negative: a message written again with more tokens (a
// stream's later line) is sent as the difference.
type UsageMessageRow struct {
	Hour    int64  `json:"h"` // ms
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model"`
	Kind    string `json:"kind,omitempty"` // main, or a subagent type
	N       int64  `json:"n"`
	Input   int64  `json:"in"`
	Output  int64  `json:"out"`
	CW5     int64  `json:"cw5"`
	CW1     int64  `json:"cw1"`
	CR      int64  `json:"cr"`
}

// UsageLedgerRow holds the per-call measures of one hour of one session
// (WP-35): context per call, cold cache starts, how subagents began.
type UsageLedgerRow struct {
	Hour     int64  `json:"h"`
	Session  string `json:"s"`
	Repo     string `json:"repo,omitempty"`
	Model    string `json:"model"`
	Kind     string `json:"kind,omitempty"`
	Calls    int64  `json:"n"`
	Ctx      int64  `json:"ctx"`
	Over200  int64  `json:"o200"`
	Over300  int64  `json:"o300"`
	CR300    int64  `json:"cr300"`
	Cold     int64  `json:"cold"`
	ColdW5   int64  `json:"cold_w5,omitempty"`
	ColdW1   int64  `json:"cold_w1,omitempty"`
	Forks    int64  `json:"forks,omitempty"`
	ForkCtx  int64  `json:"fork_ctx,omitempty"`
	Fresh    int64  `json:"fresh,omitempty"`
	FreshCtx int64  `json:"fresh_ctx,omitempty"`
}

// UsageToolRow counts calls of one tool (or skill, command, bash head…).
type UsageToolRow struct {
	Hour     int64  `json:"h"`
	Session  string `json:"s"`
	Repo     string `json:"repo,omitempty"`
	Model    string `json:"model,omitempty"`
	Kind     string `json:"kind"` // builtin, mcp, skill, command, subagent, bash
	Tool     string `json:"tool,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Calls    int64  `json:"calls"`
	Errors   int64  `json:"errors,omitempty"`
	Refusals int64  `json:"refusals,omitempty"`
	Prompts  int64  `json:"prompts,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

// UsageSessionRow is what a transcript says about its session.
type UsageSessionRow struct {
	Session     string `json:"s"`
	Cwd         string `json:"cwd,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Version     string `json:"version,omitempty"`
	Title       string `json:"title,omitempty"`
	First       int64  `json:"first,omitempty"` // ms
	Last        int64  `json:"last,omitempty"`
	Compactions int64  `json:"compactions,omitempty"`
}

// UsageRefusal is one tool call that was not run.
type UsageRefusal struct {
	At      int64  `json:"t"`
	Session string `json:"s"`
	Repo    string `json:"repo,omitempty"`
	Model   string `json:"model,omitempty"`
	Tool    string `json:"tool"`
	Detail  string `json:"detail,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// UsageChunk is what one range of one transcript file added. Its id names
// the range, so sending it twice counts it once.
type UsageChunk struct {
	ID       string            `json:"id"`
	Ledger   []UsageLedgerRow  `json:"ledger,omitempty"`
	Messages []UsageMessageRow `json:"messages,omitempty"`
	Tools    []UsageToolRow    `json:"tools,omitempty"`
	Sessions []UsageSessionRow `json:"sessions,omitempty"`
	Refusals []UsageRefusal    `json:"refusals,omitempty"`
	Risky    []UsageRisky      `json:"risky,omitempty"`
	CLI      []UsageCLIRow     `json:"cli,omitempty"`
	Traces   []UsageTrace      `json:"traces,omitempty"` // WP-250: sent again as a segment grows
}

// UsageInventoryItem is something configured on a machine, by name.
type UsageInventoryItem struct {
	Kind   string `json:"kind"` // mcp, skill, command, agent, plugin, hook
	Name   string `json:"name"`
	Scope  string `json:"scope,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// UsageIngest is one upload from one machine.
type UsageIngest struct {
	Machine string       `json:"machine"`
	Version string       `json:"version,omitempty"` // wp's
	Chunks  []UsageChunk `json:"chunks,omitempty"`
	// Inventory, when present, replaces what the machine reported before.
	Inventory *[]UsageInventoryItem `json:"inventory,omitempty"`
	// Parser health: lines understood, lines skipped, and skipped types.
	Parsed  int64            `json:"parsed,omitempty"`
	Skipped int64            `json:"skipped,omitempty"`
	Unknown map[string]int64 `json:"unknown,omitempty"`
}

// UsageIngestResult says what was new.
type UsageIngestResult struct {
	Accepted  int `json:"accepted"`  // chunks applied
	Duplicate int `json:"duplicate"` // chunks seen before
	// Traces stored or changed; TracesDropped: of a task that isn't there or
	// that the uploader can't read, or out of bounds (WP-250).
	Traces        int `json:"traces,omitempty"`
	TracesDropped int `json:"traces_dropped,omitempty"`
}

const (
	usageMaxChunks  = 2000
	usageMaxRows    = 60000
	usageChunkKeep  = 90 * 24 * time.Hour
	usageMinHour    = 1_700_000_000_000 // 2023-11: nothing earlier is a Claude Code session
	usageFutureSkew = 36 * time.Hour
)

// usageUser is the person the data belongs to; the system actor and
// anonymous callers have none.
func usageUser(actor *Actor) (int64, error) {
	if actor == nil || actor.User == nil {
		return 0, ErrUnauthorized
	}
	if actor.User.ID <= 0 {
		return 0, invalid("usage belongs to a person's account")
	}
	return actor.User.ID, nil
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func hourStart(ms int64) int64 { return ms - ms%3_600_000 }

// IngestUsage applies an upload. Agents may upload (the hooks run inside
// their sessions); read-only tokens may not.
func (s *Service) IngestUsage(ctx context.Context, actor *Actor, in UsageIngest) (*UsageIngestResult, error) {
	uid, err := usageUser(actor)
	if err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	in.Machine = clip(in.Machine, 100)
	if in.Machine == "" {
		return nil, invalid("machine is required")
	}
	if len(in.Chunks) > usageMaxChunks {
		return nil, invalid("at most %d chunks in one upload", usageMaxChunks)
	}
	rows := 0
	for _, c := range in.Chunks {
		rows += len(c.Messages) + len(c.Tools) + len(c.Sessions) + len(c.Refusals) + len(c.Ledger) + len(c.Risky) + len(c.CLI) + len(c.Traces)
		if c.ID == "" || len(c.ID) > 100 {
			return nil, invalid("each chunk needs an id")
		}
	}
	if inv := in.Inventory; inv != nil {
		rows += len(*inv)
	}
	if rows > usageMaxRows {
		return nil, invalid("at most %d rows in one upload", usageMaxRows)
	}
	now := s.nowMS()
	maxHour := now + usageFutureSkew.Milliseconds()
	res := &UsageIngestResult{}
	var landed []int64
	err = s.write(ctx, actor, func(t *tx) error {
		var notable []UsageRisky
		for _, c := range in.Chunks {
			r, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO usage_chunks(user_id, id, created_at) VALUES (?,?,?)`, uid, c.ID, now)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				res.Duplicate++
				continue
			}
			res.Accepted++
			if err := t.applyUsageChunk(ctx, uid, in.Machine, c, maxHour); err != nil {
				return err
			}
			if err := t.applyRisky(ctx, uid, in.Machine, c.Risky, maxHour, now, &notable); err != nil {
				return err
			}
			if err := t.applyCLI(ctx, uid, in.Machine, c.CLI, maxHour); err != nil {
				return err
			}
			if err := t.applyTraces(ctx, uid, in.Machine, c.Traces, maxHour, now, res, &landed); err != nil {
				return err
			}
		}
		t.emitRisky(uid, in.Machine, notable)
		if in.Inventory != nil {
			if err := t.replaceInventory(ctx, uid, in.Machine, *in.Inventory, now); err != nil {
				return err
			}
		}
		if len(in.Chunks) > 0 && res.Accepted == 0 {
			// A replayed upload: every chunk was counted before, and so were its parser stats.
			in.Parsed, in.Skipped, in.Unknown = 0, 0, nil
		}
		if err := t.touchMachine(ctx, uid, in, now); err != nil {
			return err
		}
		// Old chunk ids are only needed while a retry could still come.
		_, err := t.ExecContext(ctx, `DELETE FROM usage_chunks WHERE user_id = ? AND created_at < ?`, uid, now-usageChunkKeep.Milliseconds())
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(landed) > 0 {
		s.taskTracesLanded(ctx, landed)
	}
	return res, nil
}

func (t *tx) applyUsageChunk(ctx context.Context, uid int64, machine string, c UsageChunk, maxHour int64) error {
	// A session read from its transcript no longer counts its OTLP events.
	if err := t.dropOTLPFor(ctx, uid, c); err != nil {
		return err
	}
	for _, m := range c.Messages {
		if m.Hour < usageMinHour || m.Hour > maxHour || m.Session == "" || m.Model == "" {
			continue
		}
		kind := clip(m.Kind, 80)
		if kind == "" {
			kind = "main"
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, kind, messages, input, output, cache_write_5m, cache_write_1h, cache_read)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, hour, machine, session, repo, model, kind, via) DO UPDATE SET
				messages = messages + excluded.messages, input = input + excluded.input, output = output + excluded.output,
				cache_write_5m = cache_write_5m + excluded.cache_write_5m, cache_write_1h = cache_write_1h + excluded.cache_write_1h,
				cache_read = cache_read + excluded.cache_read`,
			uid, hourStart(m.Hour), machine, clip(m.Session, 80), clip(m.Repo, 200), clip(m.Model, 100), kind,
			m.N, m.Input, m.Output, m.CW5, m.CW1, m.CR); err != nil {
			return err
		}
	}
	for _, r := range c.Ledger {
		if r.Hour < usageMinHour || r.Hour > maxHour || r.Session == "" || r.Model == "" {
			continue
		}
		kind := clip(r.Kind, 80)
		if kind == "" {
			kind = "main"
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO ledger_hours(user_id, hour, machine, session, repo, model, kind, calls, ctx, over_200k, over_300k, cr_300k,
				cold, cold_w5, cold_w1, forks, fork_ctx, fresh, fresh_ctx)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, hour, machine, session, repo, model, kind) DO UPDATE SET
				calls = calls + excluded.calls, ctx = ctx + excluded.ctx, over_200k = over_200k + excluded.over_200k,
				over_300k = over_300k + excluded.over_300k, cr_300k = cr_300k + excluded.cr_300k, cold = cold + excluded.cold,
				cold_w5 = cold_w5 + excluded.cold_w5, cold_w1 = cold_w1 + excluded.cold_w1, forks = forks + excluded.forks,
				fork_ctx = fork_ctx + excluded.fork_ctx, fresh = fresh + excluded.fresh, fresh_ctx = fresh_ctx + excluded.fresh_ctx`,
			uid, hourStart(r.Hour), machine, clip(r.Session, 80), clip(r.Repo, 200), clip(r.Model, 100), kind,
			r.Calls, r.Ctx, r.Over200, r.Over300, r.CR300, r.Cold, r.ColdW5, r.ColdW1, r.Forks, r.ForkCtx, r.Fresh, r.FreshCtx); err != nil {
			return err
		}
	}
	for _, r := range c.Tools {
		if r.Hour < usageMinHour || r.Hour > maxHour || r.Session == "" || r.Kind == "" {
			continue
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO tool_hours(user_id, hour, machine, session, repo, model, kind, tool, detail, calls, errors, refusals, prompts, result_bytes)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, hour, machine, session, repo, model, kind, tool, detail, via) DO UPDATE SET
				calls = calls + excluded.calls, errors = errors + excluded.errors, refusals = refusals + excluded.refusals,
				prompts = prompts + excluded.prompts, result_bytes = result_bytes + excluded.result_bytes`,
			uid, hourStart(r.Hour), machine, clip(r.Session, 80), clip(r.Repo, 200), clip(r.Model, 100), clip(r.Kind, 20),
			clip(r.Tool, 120), clip(r.Detail, 200), r.Calls, r.Errors, r.Refusals, r.Prompts, r.Bytes); err != nil {
			return err
		}
	}
	for _, r := range c.Refusals {
		if r.At < usageMinHour || r.At > maxHour || r.Session == "" || r.Tool == "" {
			continue
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO permission_events(user_id, at, machine, session, repo, model, tool, detail, reason, source, excerpt)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			uid, r.At, machine, clip(r.Session, 80), clip(r.Repo, 200), clip(r.Model, 100), clip(r.Tool, 120), clip(r.Detail, 200),
			clip(r.Reason, 200), clip(r.Source, 40), clip(r.Excerpt, 300)); err != nil {
			return err
		}
	}
	for _, r := range c.Sessions {
		if r.Session == "" {
			continue
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO usage_sessions(user_id, machine, session, cwd, repo, branch, version, title, first_at, last_at, compactions)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine, session) DO UPDATE SET
				cwd = CASE WHEN usage_sessions.cwd = '' THEN excluded.cwd ELSE usage_sessions.cwd END,
				repo = CASE WHEN usage_sessions.repo = '' THEN excluded.repo ELSE usage_sessions.repo END,
				branch = CASE WHEN excluded.branch != '' THEN excluded.branch ELSE usage_sessions.branch END,
				version = CASE WHEN excluded.version != '' THEN excluded.version ELSE usage_sessions.version END,
				title = CASE WHEN excluded.title != '' THEN excluded.title ELSE usage_sessions.title END,
				first_at = CASE WHEN usage_sessions.first_at IS NULL OR (excluded.first_at IS NOT NULL AND excluded.first_at < usage_sessions.first_at) THEN excluded.first_at ELSE usage_sessions.first_at END,
				last_at = CASE WHEN usage_sessions.last_at IS NULL OR (excluded.last_at IS NOT NULL AND excluded.last_at > usage_sessions.last_at) THEN excluded.last_at ELSE usage_sessions.last_at END,
				compactions = usage_sessions.compactions + excluded.compactions`,
			uid, machine, clip(r.Session, 80), clip(r.Cwd, 400), clip(r.Repo, 200), clip(r.Branch, 200), clip(r.Version, 40), clip(r.Title, 300),
			nullPositive(r.First), nullPositive(r.Last), r.Compactions); err != nil {
			return err
		}
	}
	return nil
}

func nullPositive(n int64) any {
	if n <= 0 {
		return nil
	}
	return n
}

func (t *tx) replaceInventory(ctx context.Context, uid int64, machine string, items []UsageInventoryItem, now int64) error {
	if _, err := t.ExecContext(ctx, `DELETE FROM usage_inventory WHERE user_id = ? AND machine = ?`, uid, machine); err != nil {
		return err
	}
	for _, it := range items {
		kind := clip(it.Kind, 20)
		name := clip(it.Name, 200)
		if name == "" || !validInventoryKind(kind) {
			continue
		}
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO usage_inventory(user_id, machine, kind, name, scope, detail, seen_at) VALUES (?,?,?,?,?,?,?)`,
			uid, machine, kind, name, clip(it.Scope, 200), clip(it.Detail, 300), now); err != nil {
			return err
		}
	}
	return nil
}

func validInventoryKind(k string) bool {
	switch k {
	case "mcp", "skill", "command", "agent", "plugin", "hook":
		return true
	}
	return false
}

func (t *tx) touchMachine(ctx context.Context, uid int64, in UsageIngest, now int64) error {
	unknown, _ := json.Marshal(in.Unknown)
	if len(in.Unknown) == 0 {
		unknown = []byte("{}")
	}
	var old string
	err := t.QueryRowContext(ctx, `SELECT unknown FROM usage_machines WHERE user_id = ? AND machine = ?`, uid, in.Machine).Scan(&old)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return err
	default:
		merged := map[string]int64{}
		_ = json.Unmarshal([]byte(old), &merged)
		for k, v := range in.Unknown {
			merged[clip(k, 60)] += v
		}
		if len(merged) > 100 {
			var all int64
			for _, v := range merged {
				all += v
			}
			merged = map[string]int64{"(many kinds)": all}
		}
		unknown, _ = json.Marshal(merged)
	}
	_, err = t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, last_sync_at, wp_version, parsed, skipped, unknown)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_id, machine) DO UPDATE SET last_sync_at = excluded.last_sync_at,
			wp_version = CASE WHEN excluded.wp_version != '' THEN excluded.wp_version ELSE usage_machines.wp_version END,
			parsed = usage_machines.parsed + excluded.parsed, skipped = usage_machines.skipped + excluded.skipped, unknown = excluded.unknown`,
		uid, in.Machine, now, clip(in.Version, 40), in.Parsed, in.Skipped, string(unknown))
	return err
}

// ---- the status line ----

// QuotaWindow is one rate-limit window as the status line saw it.
type QuotaWindow struct {
	Percent  float64 `json:"percent"`
	ResetsAt int64   `json:"resets_at"` // ms
}

// UsageQuotaInput is one status line report.
type UsageQuotaInput struct {
	Machine    string       `json:"machine"`
	Session    string       `json:"session,omitempty"`
	Model      string       `json:"model,omitempty"`
	ContextPct float64      `json:"context_pct,omitempty"`
	FiveHour   *QuotaWindow `json:"five_hour,omitempty"`
	SevenDay   *QuotaWindow `json:"seven_day,omitempty"`
	At         int64        `json:"at,omitempty"` // ms; the server's clock when absent
}

const quotaKeepEvery = 15 * time.Minute

// RecordQuota stores the subscription's quota windows. A sample that says
// what the last one did is dropped unless it is a quarter hour newer, so
// months of history stay small.
func (s *Service) RecordQuota(ctx context.Context, actor *Actor, in UsageQuotaInput) error {
	uid, err := usageUser(actor)
	if err != nil {
		return err
	}
	if err := actor.canWrite(); err != nil {
		return err
	}
	in.Machine = clip(in.Machine, 100)
	if in.Machine == "" {
		return invalid("machine is required")
	}
	now := s.nowMS()
	at := now
	if in.At > 0 && in.At <= now+60_000 && in.At > now-int64(time.Hour/time.Millisecond) {
		at = in.At
	}
	return s.write(ctx, actor, func(t *tx) error {
		for name, w := range map[string]*QuotaWindow{"five_hour": in.FiveHour, "seven_day": in.SevenDay} {
			if w == nil || w.Percent < 0 || w.Percent > 1000 || w.ResetsAt <= 0 {
				continue
			}
			var lastAt, lastReset int64
			var lastPct float64
			err := t.QueryRowContext(ctx, `SELECT at, resets_at, percent FROM quota_samples WHERE user_id = ? AND span = ? ORDER BY at DESC LIMIT 1`, uid, name).
				Scan(&lastAt, &lastReset, &lastPct)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && lastReset == w.ResetsAt && lastPct == w.Percent && at-lastAt < quotaKeepEvery.Milliseconds() {
				continue
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO quota_samples(user_id, at, span, percent, resets_at, machine, session, model) VALUES (?,?,?,?,?,?,?,?)`,
				uid, at, name, w.Percent, w.ResetsAt, in.Machine, clip(in.Session, 80), clip(in.Model, 100)); err != nil {
				return err
			}
			if name == "seven_day" {
				if err := s.checkQuotaJump(ctx, t, uid, at, w.Percent, w.ResetsAt); err != nil {
					return err
				}
			}
		}
		_, err := t.ExecContext(ctx, `INSERT INTO usage_machines(user_id, machine, last_status_at) VALUES (?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET last_status_at = excluded.last_status_at`, uid, in.Machine, now)
		return err
	})
}
