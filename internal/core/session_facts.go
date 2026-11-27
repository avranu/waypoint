package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
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
	Failed  int   // files that failed (recorded on the file, not returned)
	Errored int   // files not derived for a reason outside the file (disk, database); not recorded, tried again next run
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
	case run.Failed > 0 || run.Errored > 0:
		s.log.WarnContext(ctx, "session facts: files failed to derive (a failed file: see session_fact_files.error; an errored one is tried again)", "failed", run.Failed, "errored", run.Errored, "derived", run.Derived)
	case run.Derived > 0:
		s.log.InfoContext(ctx, "session facts derived", "files", run.Derived, "rows", run.Rows, "bytes", run.Bytes, "still_waiting", run.Waiting-run.Derived)
	}
}

type factFile struct {
	userID  int64
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
		if err := rows.Scan(&f.userID, &f.machine, &f.session, &f.file, &f.gen, &f.end, &f.raw, &f.newest); err != nil {
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
			continue
		}
		run.Bytes += f.raw
		n, err := s.deriveFactFile(ctx, f)
		var fault *factFault
		switch {
		case err == nil:
			run.Derived++
			run.Rows += n
		case errors.As(err, &fault):
			run.Failed++
			if rerr := s.recordFactFailure(ctx, f, err); rerr != nil {
				return run, rerr
			}
		default:
			run.Errored++
			if ctx.Err() == nil {
				s.log.WarnContext(ctx, "session facts: a file could not be read or stored; tried again next run", "session", f.session, "file", f.file, "err", err)
			}
		}
	}
	return run, nil
}

// factFault is a failure about the file itself: what is recorded on its
// progress row. Any other error from deriving it says nothing about the file.
type factFault struct{ error }

func (e *factFault) Unwrap() error { return e.error }

func faultf(format string, a ...any) error { return &factFault{fmt.Errorf(format, a...)} }

// factPieces lists one file's pieces in order.
func (s *Service) factPieces(ctx context.Context, f factFile) ([]transcriptPiece, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, p.machine, p.session, p.file, p.kind, p.gen, p.from_off, p.to_off, p.raw_bytes, p.stored_bytes, p.sha256, p.path, p.created_at
		FROM transcript_pieces p WHERE p.user_id = ? AND p.machine = ? AND p.session = ? AND p.file = ? AND p.gen = ? ORDER BY p.from_off`,
		f.userID, f.machine, f.session, f.file, f.gen)
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
		return 0, faultf("no pieces")
	}
	d := describeTranscript(ps)
	if !d.Complete {
		return 0, faultf("the archive has a hole: its pieces do not join")
	}
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := s.copyPieces(ps, pw)
		pw.CloseWithError(err)
		copied <- err
	}()
	start := time.Now()
	res, err := transcript.Parse(pr, f.file)
	_ = pr.CloseWithError(err)
	if cerr := <-copied; cerr != nil {
		var pf *pieceFault
		if errors.As(cerr, &pf) {
			return 0, &factFault{cerr}
		}
		return 0, cerr
	}
	if err != nil {
		return 0, err
	}
	parseMS := time.Since(start).Milliseconds()
	var kept, produced int
	err = s.write(ctx, nil, func(t *tx) error {
		for _, tbl := range factTables {
			if _, err := t.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE user_id = ? AND machine = ? AND session = ? AND file = ? AND gen = ?`,
				f.userID, f.machine, f.session, f.file, f.gen); err != nil {
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
			f.userID, f.machine, f.session, f.file, f.gen, transcript.ParserVersion, f.end, f.end, res.Lines, res.Bytes, produced, kept,
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
	msg := clip(cause.Error(), 300)
	return s.write(ctx, nil, func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT INTO session_fact_files (user_id, machine, session, file, gen, parser_version, attempted_to, error, error_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, machine, session, file, gen) DO UPDATE SET parser_version = excluded.parser_version, attempted_to = excluded.attempted_to,
				error = excluded.error, error_at = excluded.error_at`,
			f.userID, f.machine, f.session, f.file, f.gen, transcript.ParserVersion, f.end, msg, s.nowMS())
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

// prepare prepares the insert of a table with the common columns first. A uid
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
	args := append([]any{w.f.userID, w.f.machine, w.f.session, w.f.file, w.f.gen, transcript.ParserVersion, r.Uid, r.Seq, r.At, r.Thread}, vals...)
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
		"background", "is_test", "result_at", "duration_ms", "result_bytes", "is_error", "error", "exit_code", "interrupted", "timed_out", "lines_added", "lines_removed", "by_script", "shell_write", "shell_files")
	if err != nil {
		return 0, 0, err
	}
	for _, c := range r.ToolCalls {
		if err := add(w.put(st, c.Row, c.MsgID, c.PromptID, c.Tool, c.Target, c.TargetKind, c.Detail, c.ReadOffset, c.ReadLimit, c.TodoItems, c.PlanChars,
			ptrBoolInt(c.Background), ptrBoolInt(c.IsTest), c.ResultAt, c.DurationMS, c.ResultBytes, ptrBoolInt(c.IsError), c.Error, c.ExitCode,
			ptrBoolInt(c.Interrupted), ptrBoolInt(c.TimedOut), c.LinesAdded, c.LinesRemoved, boolInt(c.ByScript), ptrBoolInt(c.ShellWrite), shellFilesCol(c.ShellFiles))); err != nil {
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

// factsProblem is the collector health's plain-words problem: files that
// failed, and records and fields the parser did not understand or decode, by
// type. It reads the rows of the current parser's successful derives only.
func (s *Service) factsProblem(ctx context.Context) (string, error) {
	var failed int64
	if err := s.db.R.QueryRowContext(ctx, factsFailedSQL).Scan(&failed); err != nil {
		return "", err
	}
	unknown, err := s.FactsUnknown(ctx)
	if err != nil {
		return "", err
	}
	undecoded, err := s.factsUndecoded(ctx)
	if err != nil {
		return "", err
	}
	var out []string
	if failed > 0 {
		out = append(out, fmt.Sprintf("%d transcript files failed to derive (session_fact_files.error)", failed))
	}
	if len(unknown) > 0 {
		out = append(out, "records the parser does not understand (type count): "+topCounts(unknown, 8)+"; a new Claude Code record type needs the parser taught and ParserVersion bumped")
	}
	if len(undecoded) > 0 {
		out = append(out, "fields the parser could not decode (what count): "+topCounts(undecoded, 8)+"; the facts are missing those values")
	}
	return strings.Join(out, "; "), nil
}

// topCounts is "k n, k n" for the n largest, ties by key.
func topCounts(m map[string]int64, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// FactsUnknown is the records the parser did not understand, by type, over the
// files whose last attempt succeeded with the current parser. It is the one
// query for this: the collector's problem line and the tests read it.
func (s *Service) FactsUnknown(ctx context.Context) (map[string]int64, error) {
	return s.factsCountsOf(ctx, factsUnknownSQL, "")
}

// factsUndecoded is the fields of understood records that could not be
// decoded, by what was lost (the "undecoded/" keys of the ignored counts).
func (s *Service) factsUndecoded(ctx context.Context) (map[string]int64, error) {
	m, err := s.factsCountsOf(ctx, factsIgnoredSQL, "undecoded/")
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[strings.TrimPrefix(k, "undecoded/")] = v
	}
	return out, err
}

// The health queries: the files that failed, and the JSON counts (unknown,
// ignored) by key over the files whose last attempt succeeded with the current
// parser, with the keys starting with a prefix (?2 and ?3, the same value).
const factsFailedSQL = `SELECT COUNT(*) FROM session_fact_files f WHERE f.error IS NOT NULL`

var (
	factsUnknownSQL = factsCountsSQL("unknown")
	factsIgnoredSQL = factsCountsSQL("ignored")
)

func factsCountsSQL(column string) string {
	return `SELECT j.key, SUM(j.value) FROM session_fact_files f, json_each(f.` + column + `) j
		WHERE f.parser_version = ?1 AND f.error IS NULL AND substr(j.key, 1, length(?2)) = ?3 GROUP BY j.key`
}

// factsCountsOf runs one of them.
func (s *Service) factsCountsOf(ctx context.Context, query, prefix string) (map[string]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, query, transcript.ParserVersion, prefix, prefix)
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

// shellFilesCol is fact_tool_calls.shell_files: the files one per line, NULL
// for none.
func shellFilesCol(files []string) *string {
	if len(files) == 0 {
		return nil
	}
	v := strings.Join(files, "\n")
	return &v
}
