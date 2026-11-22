package core

// Per-event session facts (WP-1467, ADR145 point 2): a scheduled job derives
// one row per model call, tool call, turn, compaction, injected file, sub-agent
// spawn and settings change from each archived transcript file (the archive's
// pieces, internal/transcript parses them) and stores them in fact_* tables.
//
// A file is derived when its newest piece is at least factsQuiet old (a session
// still writing is not parsed on every piece) and either it grew since the last
// attempt or the parser that derived it is older than transcript.ParserVersion.
// Deriving reads the whole file and replaces that file's rows in one
// transaction, so a run twice or a bumped parser never duplicates a row. An
// event that appears in two generations of a file, or in a sub-agent's file and
// the main thread's, is stored once per session (a unique uid).
//
// A failed attempt is recorded on the file's progress row and not retried until
// the file grows, the parser changes or factsRetry has passed: a file the parser
// cannot read does not loop. Records it does not understand are counted by type
// on that row and reported through the collector health.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/transcript"
)

const (
	// factsQuiet is how long a file's newest piece must have been stored
	// before the file is derived.
	factsQuiet = 10 * time.Minute
	// factsRetry is how long a failed attempt waits when nothing changed.
	factsRetry = 6 * time.Hour
	// factsRunBytesDefault is the most raw transcript one run reads (the first file is
	// always read, whatever its size); the rest wait for the next run.
	factsRunBytesDefault = 128 << 20
	// factsRunFiles bounds the files one run lists.
	factsRunFiles = 500
)

// factsRunBytes is a variable so a test can lower it.
var factsRunBytes int64 = factsRunBytesDefault

// factsWaitingSQL selects the files due to be derived: one row per
// (user, machine, session, file, gen) with the end offset, raw bytes and newest
// piece time. ?1 is the newest piece time allowed (now - quiet), ?2 the parser
// version, ?3 the time before which a failed attempt may be retried.
const factsWaitingSQL = `SELECT p.user_id, p.machine, p.session, p.file, p.gen, MAX(p.to_off) AS end_off, SUM(p.raw_bytes) AS raw, MAX(p.created_at) AS newest
	FROM transcript_pieces p
	LEFT JOIN session_fact_files f ON f.user_id = p.user_id AND f.machine = p.machine AND f.session = p.session AND f.file = p.file AND f.gen = p.gen
	GROUP BY p.user_id, p.machine, p.session, p.file, p.gen
	HAVING MAX(p.created_at) <= ?1 AND (f.user_id IS NULL OR f.parser_version < ?2 OR f.attempted_to < MAX(p.to_off)
		OR (f.error IS NOT NULL AND f.error_at <= ?3))`

// FactsRun is what one run of the job did.
type FactsRun struct {
	Derived int   // files derived
	Failed  int   // files that failed (recorded, not returned)
	Waiting int   // files due, derived or not
	Bytes   int64 // raw transcript bytes read
	Rows    int   // fact rows stored
}

// RunSessionFacts is the scheduled job.
func (s *Service) RunSessionFacts(ctx context.Context) {
	run, err := s.DeriveSessionFacts(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		s.log.WarnContext(ctx, "session facts", "err", err)
	case run.Failed > 0:
		s.log.WarnContext(ctx, "session facts: files failed to derive (see session_fact_files.error)", "failed", run.Failed, "derived", run.Derived)
	case run.Derived > 0:
		s.log.InfoContext(ctx, "session facts derived", "files", run.Derived, "rows", run.Rows, "bytes", run.Bytes, "still_waiting", run.Waiting-run.Derived)
	}
}

type factFile struct {
	uid     int64
	machine string
	session string
	file    string
	gen     int
	end     int64
	raw     int64
	newest  int64
}

// DeriveSessionFacts derives the files that are due, up to the run's byte
// budget. The error is for a failure of the job itself (listing the files); one
// file's failure is recorded on its progress row and counted in the result.
func (s *Service) DeriveSessionFacts(ctx context.Context) (FactsRun, error) {
	var run FactsRun
	if s.cfg.TranscriptDir == "" {
		return run, nil
	}
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, factsWaitingSQL+`
		ORDER BY (f.user_id IS NULL) DESC, MAX(p.created_at) DESC LIMIT ?4`,
		now.Add(-factsQuiet).UnixMilli(), transcript.ParserVersion, now.Add(-factsRetry).UnixMilli(), factsRunFiles)
	if err != nil {
		return run, err
	}
	var due []factFile
	for rows.Next() {
		var f factFile
		if err := rows.Scan(&f.uid, &f.machine, &f.session, &f.file, &f.gen, &f.end, &f.raw, &f.newest); err != nil {
			_ = rows.Close()
			return run, err
		}
		due = append(due, f)
	}
	if err := rows.Err(); err != nil {
		return run, err
	}
	_ = rows.Close()
	run.Waiting = len(due)
	for _, f := range due {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		if run.Bytes > 0 && run.Bytes+f.raw > factsRunBytes {
			// Skip the file that does not fit and go on: a break would let
			// one large file defer every smaller due file behind it, run
			// after run. The first file is always taken (above), so a file
			// larger than the budget still gets its run when it leads the list; the order (never
			// derived first, then newest) keeps the skipped one due next time.
			continue
		}
		run.Bytes += f.raw
		n, err := s.deriveFactFile(ctx, f)
		if err != nil {
			run.Failed++
			if rerr := s.recordFactFailure(ctx, f, err); rerr != nil {
				return run, rerr
			}
			continue
		}
		run.Derived++
		run.Rows += n
	}
	return run, nil
}

// factPieces lists one file's pieces in order.
func (s *Service) factPieces(ctx context.Context, f factFile) ([]transcriptPiece, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, p.machine, p.session, p.file, p.kind, p.gen, p.from_off, p.to_off, p.raw_bytes, p.stored_bytes, p.sha256, p.path, p.created_at
		FROM transcript_pieces p WHERE p.user_id = ? AND p.machine = ? AND p.session = ? AND p.file = ? AND p.gen = ? ORDER BY p.from_off`,
		f.uid, f.machine, f.session, f.file, f.gen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []transcriptPiece
	for rows.Next() {
		p := transcriptPiece{}
		if err := rows.Scan(&p.id, &p.machine, &p.session, &p.file, &p.kind, &p.gen, &p.from, &p.to, &p.raw, &p.stored, &p.sha, &p.path, &p.at); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// deriveFactFile parses one archived file and replaces its rows. It returns the
// rows stored.
func (s *Service) deriveFactFile(ctx context.Context, f factFile) (int, error) {
	ps, err := s.factPieces(ctx, f)
	if err != nil {
		return 0, err
	}
	if len(ps) == 0 {
		return 0, fmt.Errorf("no pieces")
	}
	d := describeTranscript(ps)
	if !d.Complete {
		return 0, fmt.Errorf("the archive has a hole: its pieces do not join")
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(s.copyPieces(ps, pw)) }()
	start := time.Now()
	res, err := transcript.Parse(pr, f.file)
	_ = pr.CloseWithError(err)
	if err != nil {
		return 0, err
	}
	parseMS := time.Since(start).Milliseconds()
	var kept, produced int
	err = s.write(ctx, nil, func(t *tx) error {
		for _, tbl := range factTables {
			if _, err := t.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE user_id = ? AND machine = ? AND session = ? AND file = ? AND gen = ?`,
				f.uid, f.machine, f.session, f.file, f.gen); err != nil {
				return err
			}
		}
		w := &factWriter{t: t, ctx: ctx, f: f}
		defer w.close()
		produced, kept, err = w.store(res)
		if err != nil {
			return err
		}
		unknown, _ := json.Marshal(res.Unknown)
		ignored, _ := json.Marshal(res.Ignored)
		_, err = t.ExecContext(ctx, `INSERT INTO session_fact_files (user_id, machine, session, file, gen, parser_version, attempted_to, derived_to, lines, bytes, rows, kept,
				unknown, ignored, parse_ms, derived_at, error, error_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
			ON CONFLICT (user_id, machine, session, file, gen) DO UPDATE SET parser_version = excluded.parser_version, attempted_to = excluded.attempted_to,
				derived_to = excluded.derived_to, lines = excluded.lines, bytes = excluded.bytes, rows = excluded.rows, kept = excluded.kept,
				unknown = excluded.unknown, ignored = excluded.ignored, parse_ms = excluded.parse_ms, derived_at = excluded.derived_at, error = NULL, error_at = NULL`,
			f.uid, f.machine, f.session, f.file, f.gen, transcript.ParserVersion, f.end, f.end, res.Lines, res.Bytes, produced, kept,
			string(unknown), string(ignored), parseMS, s.nowMS())
		return err
	})
	if err != nil {
		return 0, err
	}
	return kept, nil
}

// recordFactFailure notes a failed attempt, keeping what the last success left.
func (s *Service) recordFactFailure(ctx context.Context, f factFile, cause error) error {
	msg := cause.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return s.write(ctx, nil, func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT INTO session_fact_files (user_id, machine, session, file, gen, parser_version, attempted_to, error, error_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, machine, session, file, gen) DO UPDATE SET parser_version = excluded.parser_version, attempted_to = excluded.attempted_to,
				error = excluded.error, error_at = excluded.error_at`,
			f.uid, f.machine, f.session, f.file, f.gen, transcript.ParserVersion, f.end, msg, s.nowMS())
		return err
	})
}

// factTables are the tables a file's rows live in.
var factTables = []string{"fact_model_calls", "fact_tool_calls", "fact_turns", "fact_compactions", "fact_injected_files", "fact_spawns", "fact_settings"}

// factWriter inserts a file's rows with one prepared statement per table.
type factWriter struct {
	t     *tx
	ctx   context.Context
	f     factFile
	stmts []*sql.Stmt
}

func (w *factWriter) close() {
	for _, st := range w.stmts {
		_ = st.Close()
	}
}

// ins prepares the insert of a table with the common columns first. A uid
// already stored for the session (from another file or generation) is kept: the
// insert does nothing.
func (w *factWriter) prepare(table string, cols ...string) (*sql.Stmt, error) {
	all := append([]string{"user_id", "machine", "session", "file", "gen", "parser_version", "uid", "seq", "at", "thread"}, cols...)
	q := `INSERT INTO ` + table + ` (` + strings.Join(all, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(all)-1) +
		`) ON CONFLICT (user_id, machine, session, uid) DO NOTHING`
	st, err := w.t.PrepareContext(w.ctx, q)
	if err != nil {
		return nil, err
	}
	w.stmts = append(w.stmts, st)
	return st, nil
}

// put runs one insert and says whether the row was stored (false: the session
// already had that event).
func (w *factWriter) put(st *sql.Stmt, r transcript.Row, vals ...any) (int, error) {
	args := append([]any{w.f.uid, w.f.machine, w.f.session, w.f.file, w.f.gen, transcript.ParserVersion, r.Uid, r.Seq, r.At, r.Thread}, vals...)
	res, err := st.ExecContext(w.ctx, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// store inserts every row of the result: how many it had and how many were new
// to the session.
func (w *factWriter) store(r *transcript.Result) (produced, kept int, err error) {
	add := func(n int, err error) error {
		produced++
		kept += n
		return err
	}
	st, err := w.prepare("fact_model_calls", "sidechain", "msg_id", "request_id", "model", "requested_model", "effort", "per_turn_effort", "query_source", "stop_reason",
		"input_tokens", "output_tokens", "thinking_tokens", "cache_read", "cache_write", "cache_write_5m", "cache_write_1h", "context_tokens", "end_at", "latency_ms",
		"thinking_ms", "prompt_id", "turn_uid", "gap_ms", "cold_inferred")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.ModelCalls {
		if err := add(w.put(st, c.Row, boolInt(c.Sidechain), c.MsgID, c.RequestID, c.Model, c.RequestedModel, c.Effort, c.PerTurnEffort, c.QuerySource, c.StopReason,
			c.Input, c.Output, c.Thinking, c.CacheRead, c.CacheWrite, c.CacheW5m, c.CacheW1h, c.Context, c.EndAt, c.LatencyMS,
			c.ThinkingMS, c.PromptID, c.TurnUID, c.GapMS, ptrBoolInt(c.ColdInferred))); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_tool_calls", "msg_id", "prompt_id", "tool", "target", "target_kind", "detail", "read_offset", "read_limit", "todo_items", "plan_chars",
		"background", "is_test", "result_at", "duration_ms", "result_bytes", "is_error", "error", "exit_code", "interrupted", "timed_out", "lines_added", "lines_removed")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.ToolCalls {
		if err := add(w.put(st, c.Row, c.MsgID, c.PromptID, c.Tool, c.Target, c.TargetKind, c.Detail, c.ReadOffset, c.ReadLimit, c.TodoItems, c.PlanChars,
			ptrBoolInt(c.Background), ptrBoolInt(c.IsTest), c.ResultAt, c.DurationMS, c.ResultBytes, ptrBoolInt(c.IsError), c.Error, c.ExitCode,
			ptrBoolInt(c.Interrupted), ptrBoolInt(c.TimedOut), c.LinesAdded, c.LinesRemoved)); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_turns", "prompt_id", "origin", "prompt_source", "slash_command", "prompt_index", "turn_index", "prompt_bytes", "permission_mode",
		"duration_ms", "messages", "ended_at")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.Turns {
		if err := add(w.put(st, c.Row, c.PromptID, c.Origin, c.PromptSource, c.SlashCommand, c.PromptIndex, c.TurnIndex, c.PromptBytes, c.PermMode,
			c.DurationMS, c.Messages, c.EndedAt)); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_compactions", "trigger", "pre_tokens", "post_tokens", "dropped_tokens", "duration_ms")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.Compactions {
		if err := add(w.put(st, c.Row, c.Trigger, c.PreTokens, c.PostTokens, c.Dropped, c.DurationMS)); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_injected_files", "kind", "file_type", "name", "bytes", "lines", "count")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.Injected {
		if err := add(w.put(st, c.Row, c.Kind, c.FileType, c.Name, c.Bytes, c.Lines, c.Count)); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_spawns", "prompt_id", "agent_type", "purpose", "requested_model", "background", "agent_id", "resolved_model", "status",
		"result_at", "duration_ms", "tokens")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.Spawns {
		if err := add(w.put(st, c.Row, c.PromptID, c.AgentType, c.Purpose, c.RequestedModel, ptrBoolInt(c.Background), c.AgentID, c.ResolvedModel, c.Status,
			c.ResultAt, c.DurationMS, c.Tokens)); err != nil {
			return 0, 0, err
		}
	}
	st, err = w.prepare("fact_settings", "name", "value", "prev", "near_at", "source")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.Settings {
		if err := add(w.put(st, c.Row, c.Name, c.Value, c.Prev, c.NearAt, c.Source)); err != nil {
			return 0, 0, err
		}
	}
	return produced, kept, nil
}

// ---- health ----

// factsProblem is the collector health's plain-words problem: records the
// parser did not understand, by type, and files that failed.
func (s *Service) factsProblem(ctx context.Context) (string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT j.key, SUM(j.value) FROM session_fact_files f, json_each(f.unknown) j
		WHERE f.parser_version = ? GROUP BY j.key ORDER BY SUM(j.value) DESC, j.key LIMIT 8`, transcript.ParserVersion)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var parts []string
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, n))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	var failed int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_fact_files WHERE error IS NOT NULL`).Scan(&failed); err != nil {
		return "", err
	}
	var out []string
	if failed > 0 {
		out = append(out, fmt.Sprintf("%d transcript files failed to derive (session_fact_files.error)", failed))
	}
	if len(parts) > 0 {
		out = append(out, "records the parser does not understand (type count): "+strings.Join(parts, ", ")+"; a new Claude Code record type needs the parser taught and ParserVersion bumped")
	}
	return strings.Join(out, "; "), nil
}

// FactsUnknown is the records the parser did not understand, by type, over the
// files derived by the current parser.
func (s *Service) FactsUnknown(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT j.key, SUM(j.value) FROM session_fact_files f, json_each(f.unknown) j
		WHERE f.parser_version = ? GROUP BY j.key`, transcript.ParserVersion)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
