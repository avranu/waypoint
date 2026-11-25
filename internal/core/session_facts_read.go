package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/transcript"
)

// FactsQuery names one session, or one task (its key).
type FactsQuery struct {
	Session string
	Task    string // a task key: the facts of the stretches of sessions its traces cover
	User    string // "" = you; admins: a username, or "all"
	Machine string // "" = every machine that holds the session
}

// Tally is a sum over rows that may not all carry the value.
type Tally struct {
	Sum     int64 `json:"sum"`
	Known   int   `json:"known"`   // rows that carried it
	Unknown int   `json:"unknown"` // rows that did not
}

func (t *Tally) add(v sql.NullInt64) {
	if v.Valid {
		t.Sum += v.Int64
		t.Known++
	} else {
		t.Unknown++
	}
}

// FactsSource is one stretch of one session the facts were counted over.
type FactsSource struct {
	User    string `json:"user"`
	Machine string `json:"machine"`
	Session string `json:"session"`
	// From and To bound the stretch (ms); nil: the whole session, or no end
	// (the task's trace had not ended).
	From *int64 `json:"from,omitempty"`
	To   *int64 `json:"to,omitempty"`
}

// FactsStatus says how much of the archive the facts cover. Every rewrite
// (generation) of an archived file is counted as one file, and each is exactly
// one of derived, failed or waiting: the three add up to FilesArchived.
type FactsStatus struct {
	FilesArchived int `json:"files_archived"`
	// FilesDerived: derived by the current parser up to the archive's end, with
	// no error on the last attempt.
	FilesDerived int `json:"files_derived"`
	// FilesFailed: the last attempt failed (retried when the file grows, the
	// parser changes or after 6 hours).
	FilesFailed int `json:"files_failed"`
	// FilesWaiting: neither: not derived yet, or grown or re-parsed since.
	FilesWaiting int `json:"files_waiting"`
	// FilesEarlierRows: files, of the failed and waiting, whose stored rows
	// come from an earlier derive (the file grew since, the parser is older or
	// the last attempt failed). The counts include those rows.
	FilesEarlierRows int `json:"files_earlier_rows"`
	// Unknown is the records the parser did not understand, by type, in the
	// files whose last attempt succeeded with the current parser (those
	// files' rows are in the counts): facts may be missing for them.
	Unknown map[string]int64 `json:"unknown,omitempty"`
	// Undecoded is the fields of understood records the parser could not
	// decode (a malformed content array, say), by what was lost: facts are
	// missing for them too. Counted over the same files.
	Undecoded map[string]int64 `json:"undecoded,omitempty"`
	// WholeSession is true for a task: these file figures cover the task's
	// whole sessions, while its facts cover only the task's time windows.
	WholeSession bool `json:"whole_session,omitempty"`
}

// RepeatedCall is the same call made more than once.
type RepeatedCall struct {
	Tool   string `json:"tool"`
	Target string `json:"target"`
	Times  int    `json:"times"`
}

// InjectedSummary is one file put into the context, by name.
type InjectedSummary struct {
	Kind     string `json:"kind"`
	FileType string `json:"file_type,omitempty"`
	Name     string `json:"name"` // "unknown" when the record carried none
	Times    int    `json:"times"`
	Bytes    *int64 `json:"bytes"` // each time (the largest seen); nil: unknown
}

// DocRead is a documentation file read, by path.
type DocRead struct {
	Path  string `json:"path"`
	Times int    `json:"times"`
}

// DocFetch is a URL fetched, by address (no user, query or fragment).
type DocFetch struct {
	URL   string `json:"url"`
	Times int    `json:"times"`
}

// SessionFacts is the read path's answer.
type SessionFacts struct {
	Scope   string        `json:"scope"` // session or task
	Key     string        `json:"key"`
	Sources []FactsSource `json:"sources"`
	Status  FactsStatus   `json:"status"`
	// UntimedLeftOut is, for a task, the rows with no timestamp that its
	// time windows cannot place: they are in no task's counts.
	UntimedLeftOut int `json:"untimed_rows_left_out,omitempty"`

	Turns struct {
		Total int `json:"total"`
		// ByOrigin: what started each turn (human, task_notification, peer,
		// scheduled), "unknown" for a turn whose transcript does not say.
		ByOrigin map[string]int `json:"by_origin"`
		// BySlashCommand: turns that were a slash command, by command.
		BySlashCommand map[string]int `json:"by_slash_command"`
		// ByPromptSource: how the prompt was entered, "unknown" when the
		// transcript does not say.
		ByPromptSource map[string]int `json:"by_prompt_source"`
	} `json:"turns"`

	ModelCalls struct {
		Total      int            `json:"total"`
		Sidechain  int            `json:"sidechain"` // made by sub-agents
		ByModel    map[string]int `json:"by_model"`
		Output     Tally          `json:"output_tokens"`
		Thinking   Tally          `json:"thinking_tokens"` // part of the output
		CacheRead  Tally          `json:"cache_read_tokens"`
		CacheWrite Tally          `json:"cache_write_tokens"`
	} `json:"model_calls"`

	ToolCalls struct {
		Total  int            `json:"total"`
		Errors int            `json:"errors"`
		ByTool map[string]int `json:"by_tool"`
		// ByScript: tool calls a REPL script ran, not the model; they are in
		// none of the figures above.
		ByScript int `json:"by_script"`
	} `json:"tool_calls"`

	// Injected is what went into the context that the person did not type.
	Injected struct {
		Count int               `json:"count"`
		Bytes Tally             `json:"bytes"`
		Top   []InjectedSummary `json:"top"` // largest first
	} `json:"injected"`

	// DocsRead: documentation files read (Read of a doc path). DocsFetched:
	// URLs fetched (WebFetch), reported separately.
	DocsRead struct {
		Calls    int       `json:"calls"`
		Distinct int       `json:"distinct"`
		Top      []DocRead `json:"top"`
	} `json:"docs_read"`
	DocsFetched struct {
		Calls    int        `json:"calls"`
		Distinct int        `json:"distinct"`
		Top      []DocFetch `json:"top"`
	} `json:"docs_fetched"`

	// Changes: Edits counts the edit calls that did not fail; Failed is those
	// whose result was an error, including edits a REPL script ran. ShellEdits
	// counts Bash calls that wrote a file of the work and ran (a failed run
	// still counts, a refused one never ran); ShellUnknown is the Bash calls
	// with no answer (stored before it was recorded, or too long to read), so
	// ShellEdits is a floor when it is above 0. ScriptEdits and
	// ScriptShellEdits are the edit-tool and Bash calls a REPL script ran
	// that wrote; neither is in Edits or ShellEdits.
	Changes struct {
		Edits        int `json:"edits"`
		Failed       int `json:"failed"`
		ShellEdits   int `json:"shell_edits"`
		ShellUnknown int `json:"shell_unknown"`
		ScriptEdits  int `json:"script_edits"` // edit tools a REPL script ran; not in Edits
		// ScriptShellEdits: Bash calls a REPL script ran that wrote a file; not in ShellEdits.
		ScriptShellEdits int   `json:"script_shell_edits"`
		FilesChanged     int   `json:"files_changed"`
		LinesAdded       Tally `json:"lines_added"`
		LinesRemoved     Tally `json:"lines_removed"`
		TestEdits        int   `json:"test_edits"` // edits of test files (not failed)
		TestFiles        int   `json:"test_files"`
		TestLinesAdd     Tally `json:"test_lines_added"`
	} `json:"changes"`

	Repeated struct {
		Calls    int            `json:"calls"` // calls that name a target
		Distinct int            `json:"distinct"`
		Repeats  int            `json:"repeats"` // calls - distinct
		Top      []RepeatedCall `json:"top"`
	} `json:"repeated"`

	Compactions struct {
		Total      int            `json:"total"`
		ByTrigger  map[string]int `json:"by_trigger"`
		PreTokens  Tally          `json:"pre_tokens"`
		PostTokens Tally          `json:"post_tokens"`
	} `json:"compactions"`

	Subagents struct {
		Total      int            `json:"total"`
		ByType     map[string]int `json:"by_type"`
		DurationMS Tally          `json:"duration_ms"`
		Tokens     Tally          `json:"tokens"`
	} `json:"subagents"`

	// CacheExpiry is inferred from timing, not read from the transcript: a
	// cache write after a gap longer than the cache's life.
	CacheExpiry struct {
		Inferred       bool  `json:"inferred"`
		Determinable   int   `json:"determinable"` // calls with a previous call and a known write
		Cold           int   `json:"cold"`
		Undeterminable int   `json:"undeterminable"`
		ColdWrite      Tally `json:"cold_write_tokens"`
	} `json:"cache_expiry"`
}

// factScope is a stretch of one session; userID 0 is any person (an admin's "all").
type factScope struct {
	userID    int64
	machine   string
	session   string
	from, to  *int64
	userLabel string
}

func (sc factScope) where(pre string) (string, []any) {
	w := []string{pre + "session = ?"}
	a := []any{sc.session}
	if sc.userID != 0 {
		w, a = append(w, pre+"user_id = ?"), append(a, sc.userID)
	}
	if sc.machine != "" {
		w, a = append(w, pre+"machine = ?"), append(a, sc.machine)
	}
	if sc.from != nil {
		w, a = append(w, pre+"at >= ?"), append(a, *sc.from)
	}
	if sc.to != nil {
		w, a = append(w, pre+"at < ?"), append(a, *sc.to)
	}
	return "(" + strings.Join(w, " AND ") + ")", a
}

func scopesWhere(scs []factScope, pre string) (string, []any) {
	var ws []string
	var args []any
	for _, sc := range scs {
		w, a := sc.where(pre)
		ws, args = append(ws, w), append(args, a...)
	}
	return "(" + strings.Join(ws, " OR ") + ")", args
}

// SessionFacts counts a session's facts, or a task's.
func (s *Service) SessionFacts(ctx context.Context, actor *Actor, q FactsQuery) (*SessionFacts, error) {
	if actor == nil {
		return nil, ErrUnauthorized
	}
	// usageWho needs usage.read, refuses a runner job's scoped token and an
	// agent token reading another person's usage.
	uids, everyone, _, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		if actor.IsAgent() && errors.Is(err, ErrForbidden) && strings.TrimSpace(q.User) != "" {
			return nil, forbidden("%v; read your own person's sessions by leaving out --user", err)
		}
		return nil, err
	}
	// An agent, even in its person's shell, reads its own person's facts but
	// not anyone else's: decided on the resolved ids, so a differently-cased
	// name or an alias cannot dodge it.
	if everyone || len(uids) != 1 || uids[0] != actor.User.ID {
		if err := actor.requirePersonDirect("reading another person's session facts"); err != nil {
			return nil, forbidden("%v; read your own person's sessions by leaving out --user", err)
		}
	}
	q.Session, q.Task = strings.TrimSpace(q.Session), strings.TrimSpace(q.Task)
	if (q.Session == "") == (q.Task == "") {
		return nil, invalid("give a session or a task key, not both")
	}
	out := &SessionFacts{}
	var scopes []factScope
	if q.Session != "" {
		if !transcriptSessionRE.MatchString(q.Session) {
			return nil, invalid("a session id is required")
		}
		out.Scope, out.Key = "session", q.Session
		sc := factScope{machine: strings.TrimSpace(q.Machine), session: q.Session}
		if !everyone {
			sc.userID = uids[0]
		}
		scopes = []factScope{sc}
	} else {
		out.Scope, out.Key = "task", q.Task
		iss, _, err := s.getIssueFor(ctx, s.db.R, actor, strings.ToUpper(q.Task), RoleViewer, false)
		if err != nil {
			return nil, err
		}
		traces, err := s.taskTraces(ctx, iss.ID)
		if err != nil {
			return nil, err
		}
		allowed := map[int64]bool{}
		for _, u := range uids {
			allowed[u] = true
		}
		starts := map[string][]int64{}
		for _, t := range traces {
			if !everyone && !allowed[t.UserID] {
				continue
			}
			if m := strings.TrimSpace(q.Machine); m != "" && m != t.Machine {
				continue
			}
			k := fmt.Sprint(t.UserID, "\x00", t.Machine, "\x00", t.Session)
			if _, ok := starts[k]; !ok {
				if starts[k], err = s.sessionTraceStarts(ctx, t.UserID, t.Machine, t.Session); err != nil {
					return nil, err
				}
			}
			from := t.Started
			var to *int64
			for _, n := range starts[k] {
				if n > t.Started && (to == nil || n < *to) {
					v := n
					to = &v
				}
			}
			if to == nil && t.Ended != "" {
				v := t.Last + 1
				to = &v
			}
			scopes = append(scopes, factScope{userID: t.UserID, machine: t.Machine, session: t.Session, from: &from, to: to, userLabel: t.User})
		}
		if len(scopes) == 0 {
			return nil, notFound("traces of " + q.Task + " in your sessions")
		}
	}
	for _, sc := range scopes {
		out.Sources = append(out.Sources, FactsSource{User: sc.userLabel, Machine: sc.machine, Session: sc.session, From: sc.from, To: sc.to})
	}
	if err := s.factsStatus(ctx, scopes, &out.Status); err != nil {
		return nil, err
	}
	out.Status.WholeSession = out.Scope == "task"
	if out.Scope == "task" {
		n, err := s.factsUntimed(ctx, scopes)
		if err != nil {
			return nil, err
		}
		out.UntimedLeftOut = n
	}
	if out.Status.FilesArchived == 0 {
		return nil, notFound("archived transcript for " + out.Key)
	}
	if err := s.factsCount(ctx, scopes, out); err != nil {
		return nil, err
	}
	return out, nil
}

// sessionTraceStarts is when each trace of any task in one session began.
func (s *Service) sessionTraceStarts(ctx context.Context, userID int64, machine, session string) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT started_at FROM task_traces WHERE user_id = ? AND machine = ? AND session = ?`, userID, machine, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []int64{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// factsStatus counts the files of the scopes' sessions and where they stand:
// each is derived, failed or waiting, and the three add up to the files.
func (s *Service) factsStatus(ctx context.Context, scopes []factScope, st *FactsStatus) error {
	seen := map[string]bool{}
	var sessions []factScope
	for _, sc := range scopes {
		k := fmt.Sprint(sc.userID, "\x00", sc.machine, "\x00", sc.session)
		if !seen[k] {
			seen[k] = true
			sessions = append(sessions, factScope{userID: sc.userID, machine: sc.machine, session: sc.session})
		}
	}
	w, args := scopesWhere(sessions, "p.")
	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT MAX(p.to_off), f.user_id IS NOT NULL, f.parser_version, f.derived_to, f.error IS NOT NULL, f.unknown, f.ignored
		FROM transcript_pieces p
		LEFT JOIN session_fact_files f ON f.user_id = p.user_id AND f.machine = p.machine AND f.session = p.session AND f.file = p.file AND f.gen = p.gen
		WHERE `+w+`
		GROUP BY p.user_id, p.machine, p.session, p.file, p.gen`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	unknown := map[string]int64{}
	undecoded := map[string]int64{}
	for rows.Next() {
		var end int64
		var has, failed bool
		var version, derived sql.NullInt64
		var unk, ign sql.NullString
		if err := rows.Scan(&end, &has, &version, &derived, &failed, &unk, &ign); err != nil {
			return err
		}
		st.FilesArchived++
		current := has && version.Valid && version.Int64 == transcript.ParserVersion
		switch {
		case failed:
			st.FilesFailed++
		case current && derived.Valid && derived.Int64 >= end:
			st.FilesDerived++
		default:
			st.FilesWaiting++
		}
		if has && derived.Valid && (!current || failed || derived.Int64 < end) {
			st.FilesEarlierRows++
		}
		if !current || failed || !derived.Valid {
			continue
		}
		var m map[string]int64
		if json.Unmarshal([]byte(unk.String), &m) == nil {
			for t, n := range m {
				unknown[t] += n
			}
		}
		m = nil
		if json.Unmarshal([]byte(ign.String), &m) == nil {
			for t, n := range m {
				if what, ok := strings.CutPrefix(t, "undecoded/"); ok {
					undecoded[what] += n
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(unknown) > 0 {
		st.Unknown = unknown
	}
	if len(undecoded) > 0 {
		st.Undecoded = undecoded
	}
	return nil
}

// factsUntimed counts the rows of a task's sessions that carry no timestamp:
// a task's windows are cut by time, so they cannot say which task those
// belong to and the task's counts leave them out. Settings changes are not
// counted: no figure of the read path is made from them.
func (s *Service) factsUntimed(ctx context.Context, scopes []factScope) (int, error) {
	seen := map[string]bool{}
	total := 0
	for _, sc := range scopes {
		k := fmt.Sprint(sc.userID, "\x00", sc.machine, "\x00", sc.session)
		if seen[k] {
			continue
		}
		seen[k] = true
		w, args := factScope{userID: sc.userID, machine: sc.machine, session: sc.session}.where("")
		for _, tbl := range factTables {
			if tbl == "fact_settings" {
				continue
			}
			var n int
			// #nosec G202 -- tbl is a constant from factTables, w is built from fixed conditions; values are bound
			if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl+` WHERE `+w+` AND at IS NULL`, args...).Scan(&n); err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

func isDocPath(p string) bool {
	l := strings.ToLower(p)
	for _, ext := range []string{".md", ".mdx", ".rst", ".txt", ".adoc"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return strings.Contains(l, "/docs/")
}

func topN[K comparable](m map[K]int, n int, less func(a, b K) bool) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if m[ks[i]] != m[ks[j]] {
			return m[ks[i]] > m[ks[j]]
		}
		return less(ks[i], ks[j])
	})
	if len(ks) > n {
		ks = ks[:n]
	}
	return ks
}

// factsCount reads the rows of the scopes and counts them.
func (s *Service) factsCount(ctx context.Context, scopes []factScope, o *SessionFacts) error {
	w, args := scopesWhere(scopes, "")
	o.Turns.ByOrigin = map[string]int{}
	o.Turns.BySlashCommand = map[string]int{}
	o.Turns.ByPromptSource = map[string]int{}
	o.ModelCalls.ByModel = map[string]int{}
	o.ToolCalls.ByTool = map[string]int{}
	o.Compactions.ByTrigger = map[string]int{}
	o.Subagents.ByType = map[string]int{}
	o.CacheExpiry.Inferred = true

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT origin, prompt_source, slash_command FROM fact_turns WHERE `+w, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var origin, source, slash sql.NullString
		if err := rows.Scan(&origin, &source, &slash); err != nil {
			_ = rows.Close()
			return err
		}
		o.Turns.Total++
		o.Turns.ByOrigin[strOrUnknown(origin)]++
		o.Turns.ByPromptSource[strOrUnknown(source)]++
		if slash.Valid {
			o.Turns.BySlashCommand[slash.String]++
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT sidechain, model, output_tokens, thinking_tokens, cache_read, cache_write, cold_inferred
		FROM fact_model_calls WHERE `+w, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var side int
		var model sql.NullString
		var out, think, cr, cw, cold sql.NullInt64
		if err := rows.Scan(&side, &model, &out, &think, &cr, &cw, &cold); err != nil {
			_ = rows.Close()
			return err
		}
		m := &o.ModelCalls
		m.Total++
		if side == 1 {
			m.Sidechain++
		}
		m.ByModel[strOrUnknown(model)]++
		m.Output.add(out)
		m.Thinking.add(think)
		m.CacheRead.add(cr)
		m.CacheWrite.add(cw)
		if cold.Valid {
			o.CacheExpiry.Determinable++
			if cold.Int64 == 1 {
				o.CacheExpiry.Cold++
				o.CacheExpiry.ColdWrite.add(cw)
			}
		} else {
			o.CacheExpiry.Undeterminable++
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT tool, target, read_offset, read_limit, is_error, is_test, lines_added, lines_removed, by_script, shell_write
		FROM fact_tool_calls WHERE `+w, args...)
	if err != nil {
		return err
	}
	repeats := map[[2]string]int{}
	type rkey struct{ tool, target string }
	repeatShown := map[[2]string]rkey{}
	changed, testFiles := map[string]bool{}, map[string]bool{}
	docs, fetched := map[string]int{}, map[string]int{}
	for rows.Next() {
		var tool string
		var target sql.NullString
		var off, lim, isErr, isTest, add, rem sql.NullInt64
		var script int
		var shell sql.NullInt64
		if err := rows.Scan(&tool, &target, &off, &lim, &isErr, &isTest, &add, &rem, &script, &shell); err != nil {
			_ = rows.Close()
			return err
		}
		if script == 1 {
			o.ToolCalls.ByScript++
			failed := isErr.Valid && isErr.Int64 == 1
			switch {
			case transcript.IsEditTool(tool) && failed:
				o.Changes.Failed++
			case transcript.IsEditTool(tool):
				o.Changes.ScriptEdits++
			case tool == "Bash" && shell.Valid && shell.Int64 == 1:
				o.Changes.ScriptShellEdits++
			}
			continue
		}
		o.ToolCalls.Total++
		o.ToolCalls.ByTool[tool]++
		failed := isErr.Valid && isErr.Int64 == 1
		if failed {
			o.ToolCalls.Errors++
		}
		if target.Valid {
			o.Repeated.Calls++
			k := [2]string{tool + "\x00" + target.String, intOrUnknown(off) + "/" + intOrUnknown(lim)}
			repeats[k]++
			repeatShown[k] = rkey{tool, target.String}
		}
		if tool == "Read" && target.Valid && isDocPath(target.String) {
			o.DocsRead.Calls++
			docs[target.String]++
		}
		if tool == "WebFetch" && target.Valid {
			o.DocsFetched.Calls++
			fetched[target.String]++
		}
		if tool == "Bash" {
			// A refused call is recorded as no write (it never ran); one that
			// failed after running still wrote (sed -i && go test).
			switch {
			case !shell.Valid:
				o.Changes.ShellUnknown++
			case shell.Int64 == 1:
				o.Changes.ShellEdits++
			}
		}
		if transcript.IsEditTool(tool) {
			c := &o.Changes
			if failed {
				c.Failed++
				continue
			}
			c.Edits++
			if target.Valid {
				changed[target.String] = true
			}
			c.LinesAdded.add(add)
			c.LinesRemoved.add(rem)
			if isTest.Valid && isTest.Int64 == 1 {
				c.TestEdits++
				if target.Valid {
					testFiles[target.String] = true
				}
				c.TestLinesAdd.add(add)
			}
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	o.Changes.FilesChanged, o.Changes.TestFiles = len(changed), len(testFiles)
	o.Repeated.Distinct = len(repeats)
	o.Repeated.Repeats = o.Repeated.Calls - o.Repeated.Distinct
	times := map[[2]string]int{}
	for k, n := range repeats {
		if n > 1 {
			times[k] = n
		}
	}
	for _, k := range topN(times, 10, func(a, b [2]string) bool { return a[0]+a[1] < b[0]+b[1] }) {
		r := repeatShown[k]
		o.Repeated.Top = append(o.Repeated.Top, RepeatedCall{Tool: r.tool, Target: r.target, Times: times[k]})
	}
	o.DocsRead.Distinct = len(docs)
	for _, p := range topN(docs, 15, func(a, b string) bool { return a < b }) {
		o.DocsRead.Top = append(o.DocsRead.Top, DocRead{Path: p, Times: docs[p]})
	}
	o.DocsFetched.Distinct = len(fetched)
	for _, u := range topN(fetched, 15, func(a, b string) bool { return a < b }) {
		o.DocsFetched.Top = append(o.DocsFetched.Top, DocFetch{URL: u, Times: fetched[u]})
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT kind, file_type, name, bytes FROM fact_injected_files WHERE `+w, args...)
	if err != nil {
		return err
	}
	type ik struct {
		kind, ftype, name string
		noName            bool
	}
	inj := map[ik]*InjectedSummary{}
	for rows.Next() {
		var kind string
		var ftype, name sql.NullString
		var b sql.NullInt64
		if err := rows.Scan(&kind, &ftype, &name, &b); err != nil {
			_ = rows.Close()
			return err
		}
		o.Injected.Count++
		o.Injected.Bytes.add(b)
		k := ik{kind, ftype.String, name.String, !name.Valid}
		e := inj[k]
		if e == nil {
			e = &InjectedSummary{Kind: kind, FileType: ftype.String, Name: strOrUnknown(name)}
			inj[k] = e
		}
		e.Times++
		if b.Valid && (e.Bytes == nil || *e.Bytes < b.Int64) {
			v := b.Int64
			e.Bytes = &v
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	var list []*InjectedSummary
	for _, e := range inj {
		list = append(list, e)
	}
	total := func(e *InjectedSummary) int64 {
		if e.Bytes == nil {
			return 0
		}
		return *e.Bytes * int64(e.Times)
	}
	sort.Slice(list, func(i, j int) bool {
		if total(list[i]) != total(list[j]) {
			return total(list[i]) > total(list[j])
		}
		return list[i].Kind+list[i].Name < list[j].Kind+list[j].Name
	})
	if len(list) > 30 {
		list = list[:30]
	}
	o.Injected.Top = []InjectedSummary{}
	for _, e := range list {
		o.Injected.Top = append(o.Injected.Top, *e)
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT trigger, pre_tokens, post_tokens FROM fact_compactions WHERE `+w, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var trig sql.NullString
		var pre, post sql.NullInt64
		if err := rows.Scan(&trig, &pre, &post); err != nil {
			_ = rows.Close()
			return err
		}
		o.Compactions.Total++
		o.Compactions.ByTrigger[strOrUnknown(trig)]++
		o.Compactions.PreTokens.add(pre)
		o.Compactions.PostTokens.add(post)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT agent_type, duration_ms, tokens FROM fact_spawns WHERE `+w, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var typ sql.NullString
		var dur, tok sql.NullInt64
		if err := rows.Scan(&typ, &dur, &tok); err != nil {
			_ = rows.Close()
			return err
		}
		o.Subagents.Total++
		o.Subagents.ByType[strOrUnknown(typ)]++
		o.Subagents.DurationMS.add(dur)
		o.Subagents.Tokens.add(tok)
	}
	return closeRows(rows)
}

// strOrUnknown names a value, or says it is unknown.
func strOrUnknown(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return "unknown"
}

// intOrUnknown is the same for a number.
func intOrUnknown(v sql.NullInt64) string {
	if v.Valid {
		return fmt.Sprint(v.Int64)
	}
	return "unknown"
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
