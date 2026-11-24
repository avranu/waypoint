package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func (e *env) trace(a *Actor, issueID int64, seq int, startMin, lastMin int, ended string) {
	e.t.Helper()
	e.exec(`INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, updated_at) VALUES (?, 'box', 's1', ?, ?, ?, ?, ?, 1)`,
		a.User.ID, seq, issueID, fatMS(startMin, 0), fatMS(lastMin, 0), ended)
}

// The window of a trace ends where the session's next trace of ANY task
// begins: A, then B, then A again, shares the session between them with
// nothing counted twice.
func TestFactsWindowsAreBoundedByEveryTasksTraces(t *testing.T) {
	e, jo := factsEnv(t)
	p := e.project(jo, "Acme", "")
	a := e.issue(jo, IssueInput{Project: p.Key, Title: "a"})
	b := e.issue(jo, IssueInput{Project: p.Key, Title: "b"})
	e.trace(jo, a.ID, 1, 0, 19, "next")
	e.trace(jo, b.ID, 2, 20, 21, "next")
	e.trace(jo, a.ID, 3, 22, 23, "")
	fa, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: a.Key})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: b.Key})
	if err != nil {
		t.Fatal(err)
	}
	if fa.ModelCalls.Total != 3 || fb.ModelCalls.Total != 1 {
		t.Errorf("A-B-A: A has %d calls, B %d, want 3 and 1 (m1, sm1, m3 and m2)", fa.ModelCalls.Total, fb.ModelCalls.Total)
	}
	if len(fa.Sources) != 2 || fb.Sources[0].To == nil || *fb.Sources[0].To != fatMS(22, 0) {
		t.Errorf("B's window must end where A's second trace begins: %+v", fb.Sources)
	}
}

// An open trace followed by another task's trace ends at that trace's start.
func TestFactsOpenTraceEndsWhereAnotherTaskBegins(t *testing.T) {
	e, jo := factsEnv(t)
	p := e.project(jo, "Acme", "")
	a := e.issue(jo, IssueInput{Project: p.Key, Title: "a"})
	b := e.issue(jo, IssueInput{Project: p.Key, Title: "b"})
	e.trace(jo, a.ID, 1, 0, 19, "")
	e.trace(jo, b.ID, 2, 20, 23, "")
	fa, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: a.Key})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: b.Key})
	if err != nil {
		t.Fatal(err)
	}
	if fa.ModelCalls.Total != 2 || fb.ModelCalls.Total != 2 || fa.Sources[0].To == nil || *fa.Sources[0].To != fatMS(20, 0) {
		t.Errorf("open A: %d calls, to %v; B: %d calls", fa.ModelCalls.Total, fa.Sources[0].To, fb.ModelCalls.Total)
	}
}

// A setting taken back to an earlier value has its own row.
func TestFactsSettingCycleKeepsEveryChange(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	mode := func(v string) string {
		return fj(fm{"type": "permission-mode", "permissionMode": v, "sessionId": "s1"})
	}
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1)+mode("default")+mode("acceptEdits")+mode("plan")+mode("default")+mode("acceptEdits"))
	e.quiet()
	e.derive()
	if got := e.one(`SELECT COUNT(*) FROM fact_settings WHERE name = 'permission_mode'`); got != "5" {
		t.Errorf("permission-mode changes stored: %s, want 5", got)
	}
	e.exec(`UPDATE session_fact_files SET parser_version = 0`)
	e.derive()
	if got := e.one(`SELECT COUNT(*) FROM fact_settings WHERE name = 'permission_mode'`); got != "5" {
		t.Errorf("after deriving again: %s, want 5", got)
	}
}

func damagePiece(t *testing.T, e *env, session string) string {
	t.Helper()
	var path string
	if err := e.s.db.R.QueryRow(`SELECT path FROM transcript_pieces WHERE session = ? LIMIT 1`, session).Scan(&path); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(e.s.cfg.TranscriptDir, filepath.FromSlash(path))
	if err := os.WriteFile(full, tgzBytes(t, "{\"damaged\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return full
}

// Each file is derived, failed or waiting, and the three add up to the files.
func TestFactsStatusStatesAddUp(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1))
	e.putFile(jo, "s1", "subagents/agent-a.jsonl", 0, fAssistant("sa", 2, 1))
	e.quiet()
	damagePiece(t, e, "s1")
	if run := e.derive(); run.Derived != 1 || run.Failed != 1 {
		t.Fatalf("run: %+v", run)
	}
	e.putFile(jo, "s1", "subagents/agent-b.jsonl", 0, fAssistant("sb", 3, 1))
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	st := f.Status
	if st.FilesArchived != 3 || st.FilesDerived != 1 || st.FilesFailed != 1 || st.FilesWaiting != 1 || st.FilesDerived+st.FilesFailed+st.FilesWaiting != st.FilesArchived {
		t.Errorf("states do not add up: %+v", st)
	}
	if st.FilesEarlierRows != 0 {
		t.Errorf("no file has rows from an earlier derive: %+v", st)
	}
}

// A file that grew, an older parser, or a failed attempt after a success keeps
// the rows of the earlier derive: they are counted, and the status says how
// many files they come from. What the parser did not understand is read only
// from the last successful derive by the current parser.
func TestFactsStatusEarlierRowsAndFailedReDerive(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	end := e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1)+fj(fm{"type": "brand-new"}))
	e.quiet()
	e.derive()
	e.putFile(jo, "s1", "main", end, fAssistant("m2", 2, 1))
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status.FilesWaiting != 1 || f.Status.FilesEarlierRows != 1 || f.ModelCalls.Total != 1 || f.Status.Unknown["brand-new"] != 1 {
		t.Errorf("grown file: %+v calls %d", f.Status, f.ModelCalls.Total)
	}
	e.quiet()
	e.derive()
	e.exec(`UPDATE session_fact_files SET parser_version = 0, unknown = '{"old-type": 9}', ignored = '{"undecoded/old": 4}'`)
	e.exec(`UPDATE fact_model_calls SET parser_version = 0`)
	damagePiece(t, e, "s1")
	if run := e.derive(); run.Failed != 1 || run.Derived != 0 {
		t.Fatalf("run: %+v", run)
	}
	f, err = e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	st := f.Status
	if st.FilesFailed != 1 || st.FilesDerived != 0 || st.FilesWaiting != 0 || st.FilesEarlierRows != 1 || f.ModelCalls.Total != 2 {
		t.Errorf("failed re-derive: %+v calls %d (the earlier rows stay counted)", st, f.ModelCalls.Total)
	}
	if len(st.Unknown) != 0 || len(st.Undecoded) != 0 {
		t.Errorf("a failed file's old counts must not be reported: %+v %+v", st.Unknown, st.Undecoded)
	}
	if p, err := e.s.factsProblem(e.ctx); err != nil || strings.Contains(p, "old-type") || strings.Contains(p, "old ") || !strings.Contains(p, "1 transcript files failed") {
		t.Errorf("problem: %q %v", p, err)
	}
}

// An older parser's file waits, with its rows still counted.
func TestFactsStatusStaleParserWaits(t *testing.T) {
	e, jo := factsEnv(t)
	e.exec(`UPDATE session_fact_files SET parser_version = 0 WHERE file = 'main'`)
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status.FilesDerived != 1 || f.Status.FilesWaiting != 1 || f.Status.FilesEarlierRows != 1 {
		t.Errorf("status: %+v", f.Status)
	}
}

func TestFactsStatusReportsUnknownAndUndecoded(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1, fTool("w", "Write", fm{"file_path": "/a"}))+
		fResult("w", 1, false, fm{"type": "create", "content": 5})+fj(fm{"type": "brand-new"}))
	e.quiet()
	e.derive()
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status.Unknown["brand-new"] != 1 || f.Status.Undecoded["create_result_content"] != 1 {
		t.Errorf("status: unknown %v undecoded %v", f.Status.Unknown, f.Status.Undecoded)
	}
	p, err := e.s.factsProblem(e.ctx)
	if err != nil || !strings.Contains(p, "brand-new 1") || !strings.Contains(p, "create_result_content 1") {
		t.Errorf("problem line: %q %v", p, err)
	}
}

// A failure that says nothing about the file (a read error) is counted and
// logged, not recorded: the file is tried again next run.
func TestFactsReadErrorIsNotRecordedAgainstTheFile(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1))
	e.quiet()
	full := damagePiece(t, e, "s1")
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(full, 0o700); err != nil {
		t.Fatal(err)
	}
	run := e.derive()
	if run.Errored != 1 || run.Failed != 0 || run.Derived != 0 {
		t.Fatalf("run: %+v", run)
	}
	if n := e.rowCount("session_fact_files"); n != 0 {
		t.Errorf("a read error was recorded on the file (%d progress rows)", n)
	}
	if run := e.derive(); run.Errored != 1 || run.Waiting != 1 {
		t.Errorf("the file must be tried again at once: %+v", run)
	}
}

// A file the archive cannot give whole is recorded: no pieces, a hole, a
// piece gone from disk.
func TestFactsFileFailuresAreRecorded(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	_, err := e.s.deriveFactFile(e.ctx, factFile{userID: jo.User.ID, machine: "box", session: "none", file: "main"})
	var fault *factFault
	if !errors.As(err, &fault) || !strings.Contains(err.Error(), "no pieces") {
		t.Errorf("no pieces: %v", err)
	}
	first, second, third := fAssistant("m1", 1, 1), fAssistant("m2", 2, 1), fAssistant("m3", 3, 1)
	e.put(jo, piece("s1", "main", 0, first), first)
	e.put(jo, piece("s1", "main", int64(len(first)), second), second)
	e.put(jo, piece("s1", "main", int64(len(first)+len(second)), third), third)
	e.exec(`DELETE FROM transcript_pieces WHERE session = 's1' AND from_off = ?`, len(first))
	e.quiet()
	if run := e.derive(); run.Failed != 1 {
		t.Fatalf("run: %+v", run)
	}
	if got := e.one(`SELECT error FROM session_fact_files WHERE session = 's1'`); !strings.Contains(got, "hole") {
		t.Errorf("error recorded: %q", got)
	}
	e.putFile(jo, "s2", "main", 0, fAssistant("m3", 1, 1))
	e.quiet()
	var path string
	if err := e.s.db.R.QueryRow(`SELECT path FROM transcript_pieces WHERE session = 's2'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.s.cfg.TranscriptDir, filepath.FromSlash(path))); err != nil {
		t.Fatal(err)
	}
	if run := e.derive(); run.Failed != 1 || run.Errored != 0 {
		t.Fatalf("a missing piece: %+v", run)
	}
	if got := e.one(`SELECT error FROM session_fact_files WHERE session = 's2'`); !strings.Contains(got, "missing") {
		t.Errorf("error recorded: %q", got)
	}
}

func TestFactsFailureTextCutOnARuneBoundary(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	f := factFile{userID: jo.User.ID, machine: "box", session: "s1", file: "main", end: 5}
	// Truncated error text must not split a multi-byte rune.
	if err := e.s.recordFactFailure(e.ctx, f, errors.New("a"+strings.Repeat("€", 400))); err != nil {
		t.Fatal(err)
	}
	got := e.one(`SELECT error FROM session_fact_files`)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 300 || !strings.HasPrefix(got, "a€") {
		t.Errorf("error text: %d bytes, %d runes, valid %v", len(got), utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

// factTables is every fact_* table there is.
func TestFactTablesAreTheFactTables(t *testing.T) {
	e := transcriptEnv(t)
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'fact\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		have[n] = true
	}
	for _, n := range factTables {
		if !have[n] {
			t.Errorf("factTables names %s, which is not a table", n)
		}
		delete(have, n)
	}
	for n := range have {
		t.Errorf("table %s is not in factTables: a derive would leave its rows behind", n)
	}
}

// The derive's DELETE and the read paths use the unique index, not a scan.
func TestFactQueriesUseTheUidIndex(t *testing.T) {
	e := transcriptEnv(t)
	for _, tbl := range factTables {
		for _, q := range []string{
			`DELETE FROM ` + tbl + ` WHERE user_id = 1 AND machine = 'm' AND session = 's' AND file = 'f' AND gen = 0`,
			`SELECT * FROM ` + tbl + ` WHERE session = 's' AND user_id = 1 AND machine = 'm' AND at >= 1 AND at < 2`,
		} {
			plan := strings.Join(planOfQuery(t, e, q), " | ")
			if !strings.Contains(plan, "USING INDEX "+tbl+"_uid") && !strings.Contains(plan, "USING COVERING INDEX "+tbl+"_uid") {
				t.Errorf("%s: %s", q, plan)
			}
		}
	}
}

func planOfQuery(t *testing.T, e *env, q string) []string {
	t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, `EXPLAIN QUERY PLAN `+q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

func fCall(id string, min, write int, split string) string {
	usage := fm{"input_tokens": 1, "output_tokens": 1, "cache_read_input_tokens": 10, "cache_creation_input_tokens": write}
	switch split {
	case "5m":
		usage["cache_creation"] = fm{"ephemeral_5m_input_tokens": write, "ephemeral_1h_input_tokens": 0}
	case "1h":
		usage["cache_creation"] = fm{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": write}
	}
	return fj(fm{"type": "assistant", "uuid": "u-" + id, "timestamp": fat(min, 0), "requestId": "q-" + id,
		"message": fm{"id": id, "model": "model-a", "content": []fm{}, "usage": usage}})
}

// The cache expiry figures: the calls that could be told, the cold ones and
// the ones that could not be told (no previous call, or a split unknown in the
// one gap that needs it).
func TestFactsCacheExpiryCounts(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0,
		fCall("c1", 0, 100, "5m")+
			fCall("c2", 10, 50, "1h")+
			fCall("c3", 40, 20, "")+
			fCall("c4", 41, 0, "")+
			fCall("c5", 60, 10, ""))
	e.quiet()
	e.derive()
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.CacheExpiry
	if c.Determinable != 3 || c.Cold != 1 || c.Undeterminable != 2 || c.ColdWrite.Sum != 50 || c.ColdWrite.Known != 1 {
		t.Errorf("cache expiry: %+v", c)
	}
}

// Slash commands and prompt sources are counted; WebFetch is counted apart
// from the files read; a NULL injected name is not an empty one.
func TestFactsReadCountsSlashSourceFetchAndUnnamed(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0,
		fj(fm{"type": "user", "uuid": "p1", "timestamp": fat(1, 0), "promptId": "p-1", "turnOrigin": "human", "promptSource": "keyboard",
			"message": fm{"content": "<command-name>/review</command-name> now"}})+
			fj(fm{"type": "user", "uuid": "p2", "timestamp": fat(2, 0), "promptId": "p-2", "message": fm{"content": "plain"}})+
			fAssistant("m1", 3, 1, fTool("w1", "WebFetch", fm{"url": "https://user:pw@example.com/doc?token=1"}), fTool("w2", "WebFetch", fm{"url": "https://example.com/doc"}),
				fTool("r1", "Read", fm{"file_path": "/r/main.go"}), fTool("r2", "Read", fm{"file_path": "/r/README.md"}))+
			fj(fm{"type": "attachment", "uuid": "i1", "timestamp": fat(1, 0), "attachment": fm{"type": "instructions", "files": []fm{{"type": "Project", "content": "ab"}, {"path": "", "type": "Project", "content": "abc"}}}}))
	e.quiet()
	e.derive()
	e.exec(`UPDATE fact_injected_files SET name = '' WHERE bytes = 3`)
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Turns.BySlashCommand["/review"] != 1 || len(f.Turns.BySlashCommand) != 1 || f.Turns.ByPromptSource["keyboard"] != 1 || f.Turns.ByPromptSource["unknown"] != 1 {
		t.Errorf("turns: %+v", f.Turns)
	}
	if f.DocsFetched.Calls != 2 || f.DocsFetched.Distinct != 1 || len(f.DocsFetched.Top) != 1 || f.DocsFetched.Top[0].URL != "https://example.com/doc" {
		t.Errorf("fetched: %+v (the secret in the address is dropped, so both are the same page)", f.DocsFetched)
	}
	if f.DocsRead.Calls != 1 || f.DocsRead.Distinct != 1 {
		t.Errorf("docs read: %+v (a Read of a source file is not documentation, a fetch is counted apart)", f.DocsRead)
	}
	names := map[string]int{}
	for _, in := range f.Injected.Top {
		names[in.Name]++
	}
	if len(f.Injected.Top) != 2 || names["unknown"] != 1 || names[""] != 1 {
		t.Errorf("injected: %+v (a NULL name is its own entry)", f.Injected.Top)
	}
}

// A failed edit is counted as an edit and a failure, and nothing else.
func TestFactsReadFailedEditAndMachineFilter(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1, fTool("t1", "Edit", fm{"file_path": "/r/a_test.go"}), fTool("t2", "Edit", fm{"file_path": "/r/b.go"}))+
		fResult("t1", 1, true, nil)+fResult("t2", 1, false, fm{"type": "update", "structuredPatch": []fm{{"lines": []string{"+x"}}}}))
	in := piece("s1", "main", 0, "")
	in.Machine = "laptop"
	lines := fAssistant("lm", 2, 7)
	in.To = int64(len(lines))
	e.put(jo, in, lines)
	e.quiet()
	e.derive()
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.Changes
	if c.Edits != 2 || c.Failed != 1 || c.FilesChanged != 1 || c.TestEdits != 0 || c.LinesAdded.Sum != 1 || f.ModelCalls.Total != 2 {
		t.Errorf("changes %+v calls %d", c, f.ModelCalls.Total)
	}
	f, err = e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1", Machine: "laptop"})
	if err != nil || f.ModelCalls.Total != 1 || f.ModelCalls.Output.Sum != 7 || f.Status.FilesArchived != 1 {
		t.Errorf("machine filter: %v %+v", err, f)
	}
}

func TestFactsReadInputsAndAdmin(t *testing.T) {
	e, jo := factsEnv(t)
	for _, bad := range []string{"../x", "a b", strings.Repeat("a", 300)} {
		if _, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: bad}); !IsInvalid(err) {
			t.Errorf("session id %q: %v", bad, err)
		}
	}
	if _, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: "ZZ-999"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown task key: %v", err)
	}
	if f, err := e.s.SessionFacts(e.ctx, e.admin, FactsQuery{Session: "s1", User: "all"}); err != nil || f.ModelCalls.Total != 4 {
		t.Errorf("admin, user=all: %v %+v", err, f)
	}
}

// The health line names the derive's own files, and a growing backlog is not
// a drop: the collector has no dropped total for session facts.
func TestFactsCollectorHasNoDroppedTotal(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 1, 1))
	e.quiet()
	damagePiece(t, e, "s1")
	e.derive()
	e.s.CheckCollectorHealth(e.ctx)
	hs, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Name != "session facts" {
			continue
		}
		if h.Dropped != nil || !strings.Contains(h.Problem, "failed") {
			t.Errorf("collector health: dropped %v problem %q", h.Dropped, h.Problem)
		}
		return
	}
	t.Fatal("no session facts collector in the health list")
}
