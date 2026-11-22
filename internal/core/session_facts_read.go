package core

// Reading the session facts (WP-1467): what a session, or a task's share of
// one, did, counted from the fact_* rows. The same access as the raw
// transcript: a person reads their own sessions, an admin anyone's, and an
// agent's token reads none (the facts hold paths and commands). A value the
// transcript did not carry is counted as unknown, never as 0; what is worked out
// from timing rather than read is labelled inferred.

import (
	"context"
	"database/sql"
	"encoding/json"
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

// FactsStatus says how much of the archive the facts cover.
type FactsStatus struct {
	FilesArchived int `json:"files_archived"`
	FilesDerived  int `json:"files_derived"` // by the current parser
	FilesWaiting  int `json:"files_waiting"` // not derived yet, or by an older parser
	FilesFailed   int `json:"files_failed"`
	// Unknown is the records the parser did not understand in those files, by
	// type: facts may be missing for them.
	Unknown map[string]int64 `json:"unknown,omitempty"`
	// Undecoded is the fields of understood records the parser could not
	// decode (a malformed content array, say), by what was lost: facts are
	// missing for them too.
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
	Name     string `json:"name"`
	Times    int    `json:"times"`
	Bytes    *int64 `json:"bytes"` // each time (the largest seen); nil: unknown
}

// DocRead is a documentation file read, by path.
type DocRead struct {
	Path  string `json:"path"`
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
	} `json:"turns"`

	ModelCalls struct {
		Total     int            `json:"total"`
		Sidechain int            `json:"sidechain"` // made by sub-agents
		ByModel   map[string]int `json:"by_model"`
		Output    Tally          `json:"output_tokens"`
		Thinking  Tally          `json:"thinking_tokens"` // part of the output
		CacheRead Tally          `json:"cache_read_tokens"`
		CacheWrit Tally          `json:"cache_write_tokens"`
	} `json:"model_calls"`

	ToolCalls struct {
		Total  int            `json:"total"`
		Errors int            `json:"errors"`
		ByTool map[string]int `json:"by_tool"`
	} `json:"tool_calls"`

	// Injected is what went into the context that the person did not type.
	Injected struct {
		Count int               `json:"count"`
		Bytes Tally             `json:"bytes"`
		Top   []InjectedSummary `json:"top"` // largest first
	} `json:"injected"`

	DocsRead struct {
		Calls    int       `json:"calls"`
		Distinct int       `json:"distinct"`
		Top      []DocRead `json:"top"`
	} `json:"docs_read"`

	// Changes: edits whose result was not an error.
	Changes struct {
		Edits        int   `json:"edits"`
		Failed       int   `json:"failed"`
		FilesChanged int   `json:"files_changed"`
		LinesAdded   Tally `json:"lines_added"`
		LinesRemoved Tally `json:"lines_removed"`
		TestEdits    int   `json:"test_edits"` // edits of test files
		TestFiles    int   `json:"test_files"`
		TestLinesAdd Tally `json:"test_lines_added"`
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
		Inferred      bool  `json:"inferred"`
		Determinable  int   `json:"determinable"` // calls with a previous call and a known write
		Cold          int   `json:"cold"`
		Undeterminate int   `json:"undeterminable"`
		ColdWrite     Tally `json:"cold_write_tokens"`
	} `json:"cache_expiry"`
}

// factScope is a stretch of one session; uid 0 is any person (an admin's "all").
type factScope struct {
	uid       int64
	machine   string
	session   string
	from, to  *int64
	userLabel string
}

func (sc factScope) where(pre string) (string, []any) {
	w := []string{pre + "session = ?"}
	a := []any{sc.session}
	if sc.uid != 0 {
		w, a = append(w, pre+"user_id = ?"), append(a, sc.uid)
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
	if err := actor.requirePersonDirect("reading session facts"); err != nil {
		return nil, err
	}
	uids, everyone, _, err := s.usageWho(ctx, actor, q.User)
	if err != nil {
		return nil, err
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
			sc.uid = uids[0]
		}
		scopes = []factScope{sc}
	} else {
		out.Scope, out.Key = "task", q.Task
		// the task must be one the actor may read, whoever's traces it has
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
		for i, t := range traces {
			if !everyone && !allowed[t.UserID] {
				continue
			}
			if m := strings.TrimSpace(q.Machine); m != "" && m != t.Machine {
				continue
			}
			from := t.Started
			var to *int64
			// The window ends where the session's next trace begins; the last
			// one at its end when it has one, else it is open.
			for _, n := range traces[i+1:] {
				if n.UserID == t.UserID && n.Machine == t.Machine && n.Session == t.Session && n.Started > t.Started {
					v := n.Started
					to = &v
					break
				}
			}
			if to == nil && t.Ended != "" {
				v := t.Last + 1
				to = &v
			}
			scopes = append(scopes, factScope{uid: t.UserID, machine: t.Machine, session: t.Session, from: &from, to: to, userLabel: t.User})
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

// factsStatus counts the files of the scopes' sessions and where they stand.
func (s *Service) factsStatus(ctx context.Context, scopes []factScope, st *FactsStatus) error {
	seen := map[string]bool{}
	var sessions []factScope
	for _, sc := range scopes {
		k := fmt.Sprint(sc.uid, "\x00", sc.machine, "\x00", sc.session)
		if !seen[k] {
			seen[k] = true
			sessions = append(sessions, factScope{uid: sc.uid, machine: sc.machine, session: sc.session})
		}
	}
	w, args := scopesWhere(sessions, "p.")
	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.user_id, p.machine, p.session, p.file, p.gen, MAX(p.to_off)
		FROM transcript_pieces p WHERE `+w+`
		GROUP BY p.user_id, p.machine, p.session, p.file, p.gen`, args...)
	if err != nil {
		return err
	}
	type key struct {
		uid              int64
		machine, session string
		file             string
		gen              int
		end              int64
	}
	var files []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.uid, &k.machine, &k.session, &k.file, &k.gen, &k.end); err != nil {
			_ = rows.Close()
			return err
		}
		files = append(files, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	st.FilesArchived = len(files)
	unknown := map[string]int64{}
	undecoded := map[string]int64{}
	for _, k := range files {
		var version int
		var derived sql.NullInt64
		var errText sql.NullString
		var unk, ign string
		err := s.db.R.QueryRowContext(ctx, `SELECT parser_version, derived_to, error, unknown, ignored FROM session_fact_files
			WHERE user_id = ? AND machine = ? AND session = ? AND file = ? AND gen = ?`, k.uid, k.machine, k.session, k.file, k.gen).
			Scan(&version, &derived, &errText, &unk, &ign)
		switch {
		case err == sql.ErrNoRows:
			st.FilesWaiting++
			continue
		case err != nil:
			return err
		}
		if errText.Valid {
			st.FilesFailed++
		}
		if derived.Valid && version == transcript.ParserVersion && derived.Int64 >= k.end {
			st.FilesDerived++
			var m map[string]int64
			if json.Unmarshal([]byte(unk), &m) == nil {
				for t, n := range m {
					unknown[t] += n
				}
			}
			if json.Unmarshal([]byte(ign), &m) == nil {
				for t, n := range m {
					if what, ok := strings.CutPrefix(t, "undecoded/"); ok {
						undecoded[what] += n
					}
				}
			}
		} else {
			st.FilesWaiting++
		}
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
// belong to and the task's counts leave them out.
func (s *Service) factsUntimed(ctx context.Context, scopes []factScope) (int, error) {
	seen := map[string]bool{}
	total := 0
	for _, sc := range scopes {
		k := fmt.Sprint(sc.uid, "\x00", sc.machine, "\x00", sc.session)
		if seen[k] {
			continue
		}
		seen[k] = true
		w, args := factScope{uid: sc.uid, machine: sc.machine, session: sc.session}.where("")
		for _, tbl := range factTables {
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

func isEditTool(t string) bool {
	switch t {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
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
	o.ModelCalls.ByModel = map[string]int{}
	o.ToolCalls.ByTool = map[string]int{}
	o.Compactions.ByTrigger = map[string]int{}
	o.Subagents.ByType = map[string]int{}
	o.CacheExpiry.Inferred = true

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT origin FROM fact_turns WHERE `+w, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var origin sql.NullString
		if err := rows.Scan(&origin); err != nil {
			_ = rows.Close()
			return err
		}
		o.Turns.Total++
		o.Turns.ByOrigin[orUnknown(origin)]++
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
		m.ByModel[orUnknown(model)]++
		m.Output.add(out)
		m.Thinking.add(think)
		m.CacheRead.add(cr)
		m.CacheWrit.add(cw)
		if cold.Valid {
			o.CacheExpiry.Determinable++
			if cold.Int64 == 1 {
				o.CacheExpiry.Cold++
				o.CacheExpiry.ColdWrite.add(cw)
			}
		} else {
			o.CacheExpiry.Undeterminate++
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT tool, target, read_offset, read_limit, is_error, is_test, lines_added, lines_removed
		FROM fact_tool_calls WHERE `+w, args...)
	if err != nil {
		return err
	}
	repeats := map[[2]string]int{}
	type rkey struct{ tool, target string }
	repeatShown := map[[2]string]rkey{}
	changed, testFiles := map[string]bool{}, map[string]bool{}
	docs := map[string]int{}
	for rows.Next() {
		var tool string
		var target sql.NullString
		var off, lim, isErr, isTest, add, rem sql.NullInt64
		if err := rows.Scan(&tool, &target, &off, &lim, &isErr, &isTest, &add, &rem); err != nil {
			_ = rows.Close()
			return err
		}
		o.ToolCalls.Total++
		o.ToolCalls.ByTool[tool]++
		failed := isErr.Valid && isErr.Int64 == 1
		if failed {
			o.ToolCalls.Errors++
		}
		if target.Valid {
			o.Repeated.Calls++
			k := [2]string{tool + "\x00" + target.String, orUnknown(off) + "/" + orUnknown(lim)}
			repeats[k]++
			repeatShown[k] = rkey{tool, target.String}
		}
		if tool == "Read" && target.Valid && isDocPath(target.String) {
			o.DocsRead.Calls++
			docs[target.String]++
		}
		if isEditTool(tool) {
			c := &o.Changes
			c.Edits++
			if failed {
				c.Failed++
				continue
			}
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

	// #nosec G202 -- w is built from fixed conditions; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT kind, file_type, name, bytes FROM fact_injected_files WHERE `+w, args...)
	if err != nil {
		return err
	}
	type ik struct{ kind, ftype, name string }
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
		k := ik{kind, ftype.String, name.String}
		e := inj[k]
		if e == nil {
			e = &InjectedSummary{Kind: kind, FileType: ftype.String, Name: name.String}
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
		o.Compactions.ByTrigger[orUnknown(trig)]++
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
		o.Subagents.ByType[orUnknown(typ)]++
		o.Subagents.DurationMS.add(dur)
		o.Subagents.Tokens.add(tok)
	}
	return closeRows(rows)
}

// orUnknown names a value, or says it is unknown.
func orUnknown(v any) string {
	switch x := v.(type) {
	case sql.NullString:
		if x.Valid {
			return x.String
		}
	case sql.NullInt64:
		if x.Valid {
			return fmt.Sprint(x.Int64)
		}
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
