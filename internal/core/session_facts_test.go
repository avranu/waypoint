package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/transcript"
)

// Fixtures are synthetic lines in the shape Claude Code writes.

type fm = map[string]any

func fj(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// fat is a time on 2026-10-05 at 11:MM:SS, in the transcript's format.
func fat(min, sec int) string { return fmt.Sprintf("2026-10-05T11:%02d:%02d.000Z", min, sec) }

func fatMS(min, sec int) int64 {
	t, _ := time.Parse(time.RFC3339Nano, fat(min, sec))
	return t.UnixMilli()
}

func fAssistant(id string, min int, out int, content ...fm) string {
	return fj(fm{"type": "assistant", "uuid": "u-" + id, "timestamp": fat(min, 0), "requestId": "q-" + id,
		"message": fm{"id": id, "model": "model-a", "content": content,
			"usage": fm{"input_tokens": 1, "output_tokens": out, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 0}}})
}

func fTool(id, name string, in fm) fm {
	return fm{"type": "tool_use", "id": id, "name": name, "input": in}
}

func fResult(id string, min int, isErr bool, tur any) string {
	r := fm{"type": "user", "uuid": "r-" + id, "timestamp": fat(min, 30),
		"message": fm{"content": []fm{{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": "ok"}}}}
	if tur != nil {
		r["toolUseResult"] = tur
	}
	return fj(r)
}

func fPrompt(uuid string, min int, origin string) string {
	r := fm{"type": "user", "uuid": uuid, "timestamp": fat(min, 1), "promptId": "p-" + uuid, "message": fm{"content": "do the thing"}}
	if origin != "" {
		r["turnOrigin"] = origin
	}
	return fj(r)
}

func (e *env) rowCount(table string) int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) derive() FactsRun {
	e.t.Helper()
	run, err := e.s.DeriveSessionFacts(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

// quiet moves the clock past the quiet period.
func (e *env) quiet() { e.setClock(e.clockNow().Add(factsQuiet + time.Minute)) }

func (e *env) putFile(a *Actor, session, file string, from int64, lines string) int64 {
	e.t.Helper()
	e.put(a, piece(session, file, from, lines), lines)
	return from + int64(len(lines))
}

// A file is derived once it has been quiet for the quiet period, not before;
// a second run finds nothing to do.
func TestFactsQuietPeriod(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fPrompt("a", 1, "human")+fAssistant("m1", 2, 5))
	if run := e.derive(); run.Waiting != 0 || run.Derived != 0 {
		t.Fatalf("a file stored just now was derived: %+v", run)
	}
	e.setClock(e.clockNow().Add(factsQuiet - time.Minute))
	if run := e.derive(); run.Derived != 0 {
		t.Fatalf("derived before the quiet period was over: %+v", run)
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	run := e.derive()
	if run.Derived != 1 || run.Failed != 0 || e.rowCount("fact_turns") != 1 || e.rowCount("fact_model_calls") != 1 {
		t.Fatalf("after the quiet period: %+v turns %d calls %d", run, e.rowCount("fact_turns"), e.rowCount("fact_model_calls"))
	}
	if run := e.derive(); run.Waiting != 0 || run.Derived != 0 {
		t.Errorf("an unchanged file was derived again: %+v", run)
	}
	var v int
	var derived, attempted int64
	if err := e.s.db.R.QueryRow(`SELECT parser_version, derived_to, attempted_to FROM session_fact_files WHERE session = 's1'`).Scan(&v, &derived, &attempted); err != nil {
		t.Fatal(err)
	}
	if v != transcript.ParserVersion || derived == 0 || derived != attempted {
		t.Errorf("progress: version %d derived %d attempted %d", v, derived, attempted)
	}
}

// A file that grew is derived whole again and its rows replaced, not added to.
func TestFactsGrowReplacesRows(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	end := e.putFile(jo, "s1", "main", 0, fPrompt("a", 1, "human")+fAssistant("m1", 2, 5, fTool("t1", "Read", fm{"file_path": "/r/a.go"})))
	e.quiet()
	e.derive()
	if e.rowCount("fact_tool_calls") != 1 || e.rowCount("fact_model_calls") != 1 {
		t.Fatal("first derivation")
	}
	end = e.putFile(jo, "s1", "main", end, fResult("t1", 3, false, nil)+fAssistant("m2", 4, 7))
	e.quiet()
	if run := e.derive(); run.Derived != 1 {
		t.Fatalf("grown file: %+v", run)
	}
	if e.rowCount("fact_model_calls") != 2 || e.rowCount("fact_tool_calls") != 1 || e.rowCount("fact_turns") != 1 {
		t.Errorf("rows were added instead of replaced: calls %d tools %d turns %d", e.rowCount("fact_model_calls"), e.rowCount("fact_tool_calls"), e.rowCount("fact_turns"))
	}
	if got := e.one(`SELECT is_error FROM fact_tool_calls`); got != "0" {
		t.Errorf("the result that arrived in the second piece is not in the replaced row: is_error %q", got)
	}
	_ = end
}

// A file derived by an older parser is derived again; its rows carry the new
// version and nothing is doubled.
func TestFactsParserVersionBump(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fPrompt("a", 1, "human")+fAssistant("m1", 2, 5))
	e.quiet()
	e.derive()
	e.exec(`UPDATE session_fact_files SET parser_version = 0`)
	e.exec(`UPDATE fact_model_calls SET parser_version = 0, output_tokens = -1`)
	if run := e.derive(); run.Derived != 1 {
		t.Fatalf("older parser's file was not derived again: %+v", run)
	}
	if got := e.one(`SELECT parser_version || ',' || output_tokens FROM fact_model_calls`); got != fmt.Sprint(transcript.ParserVersion)+",5" || e.rowCount("fact_model_calls") != 1 {
		t.Errorf("row after the bump: %q (rows %d)", got, e.rowCount("fact_model_calls"))
	}
}

// One event in the main file and a sub-agent's file, or in two generations, is
// one row per session.
func TestFactsDedupedAcrossFilesAndGenerations(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	call := fAssistant("m1", 2, 5, fTool("t1", "Bash", fm{"command": "ls"}))
	e.putFile(jo, "s1", "main", 0, call)
	e.putFile(jo, "s1", "subagents/agent-x.jsonl", 0, call+fAssistant("m9", 3, 1))
	in := piece("s1", "main", 0, call)
	in.FP = "fp0#1" // the file was rewritten: the next generation holds the same call
	e.put(jo, in, call)
	e.quiet()
	if run := e.derive(); run.Derived != 3 || run.Failed != 0 {
		t.Fatalf("run: %+v", run)
	}
	if e.rowCount("fact_model_calls") != 2 || e.rowCount("fact_tool_calls") != 1 {
		t.Errorf("calls %d tools %d, want 2 and 1", e.rowCount("fact_model_calls"), e.rowCount("fact_tool_calls"))
	}
	// deriving again keeps it that way
	e.exec(`UPDATE session_fact_files SET parser_version = 0`)
	e.derive()
	if e.rowCount("fact_model_calls") != 2 || e.rowCount("fact_tool_calls") != 1 {
		t.Errorf("after deriving again: calls %d tools %d", e.rowCount("fact_model_calls"), e.rowCount("fact_tool_calls"))
	}
}

// A run reads at most its byte budget (the first file whatever its size); the
// rest wait for the next run, the newest first.
func TestFactsRunByteBudget(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	for i := range 3 {
		e.putFile(jo, fmt.Sprintf("s%d", i), "main", 0, fAssistant(fmt.Sprintf("m%d", i), 2, 5))
		e.setClock(e.clockNow().Add(time.Second))
	}
	e.quiet()
	old := factsRunBytes
	factsRunBytes = 1
	t.Cleanup(func() { factsRunBytes = old })
	for i, want := range []int{1, 1, 1, 0} {
		run := e.derive()
		if run.Derived != want {
			t.Fatalf("run %d: derived %d, want %d (%+v)", i, run.Derived, want, run)
		}
	}
	if e.rowCount("fact_model_calls") != 3 {
		t.Errorf("calls %d", e.rowCount("fact_model_calls"))
	}
	// the newest session came first
	if got := e.one(`SELECT session FROM fact_model_calls ORDER BY id LIMIT 1`); got != "s2" {
		t.Errorf("first derived %q, want the newest, s2", got)
	}
}

// A file too big for what is left of the run's budget is skipped, not a stop:
// the smaller due files behind it still run, and the big one is next in line.
func TestFactsRunBudgetSkipsTheFileThatDoesNotFit(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	small := fAssistant("ma", 2, 5)
	big := fAssistant("mb", 2, 5, fm{"type": "text", "text": strings.Repeat("x", 4000)})
	e.putFile(jo, "c", "main", 0, small) // oldest
	e.setClock(e.clockNow().Add(time.Second))
	e.putFile(jo, "b", "main", 0, big)
	e.setClock(e.clockNow().Add(time.Second))
	e.putFile(jo, "a", "main", 0, small) // newest, so first
	e.quiet()
	old := factsRunBytes
	factsRunBytes = int64(2*len(small) + 10)
	t.Cleanup(func() { factsRunBytes = old })
	run := e.derive()
	if run.Derived != 2 {
		t.Fatalf("first run derived %d, want 2: the big file is skipped and the small one behind it runs (%+v)", run.Derived, run)
	}
	if got := e.rowCount("fact_model_calls"); got != 2 {
		t.Errorf("calls %d", got)
	}
	if run := e.derive(); run.Derived != 1 {
		t.Errorf("second run derived %d, want the skipped big file (%+v)", run.Derived, run)
	}
}

// A file that cannot be read is recorded, counted in the collector health and
// not retried until something changes or the retry time has passed.
func TestFactsFailureRecordedNotRetried(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 2, 5))
	e.putFile(jo, "s2", "main", 0, fAssistant("m2", 2, 5))
	var path string
	if err := e.s.db.R.QueryRow(`SELECT path FROM transcript_pieces WHERE session = 's1'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.s.cfg.TranscriptDir, filepath.FromSlash(path)), tgzBytes(t, "{\"damaged\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.quiet()
	run := e.derive()
	if run.Failed != 1 || run.Derived != 1 {
		t.Fatalf("run: %+v", run)
	}
	if got := e.one(`SELECT error FROM session_fact_files WHERE session = 's1'`); !strings.Contains(got, "hash") {
		t.Errorf("error recorded: %q", got)
	}
	if run := e.derive(); run.Waiting != 0 {
		t.Errorf("a failed file was retried at once: %+v", run)
	}
	e.s.CheckCollectorHealth(e.ctx)
	hs, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var h *CollectorHealth
	for i := range hs {
		if hs[i].Name == "session facts" {
			h = &hs[i]
		}
	}
	if h == nil || !strings.Contains(h.Problem, "failed") {
		t.Fatalf("collector health: %+v", h)
	}
	e.setClock(e.clockNow().Add(factsRetry + time.Minute))
	if run := e.derive(); run.Failed != 1 {
		t.Errorf("a failed file is retried after the retry time: %+v", run)
	}
}

func tgzBytes(t *testing.T, s string) []byte { return tgz(t, s) }

// Records the parser does not understand are counted by type and reported.
func TestFactsUnknownRecordsReported(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 2, 5)+fj(fm{"type": "brand-new"})+fj(fm{"type": "brand-new"}))
	e.quiet()
	e.derive()
	got, err := e.s.FactsUnknown(e.ctx)
	if err != nil || got["brand-new"] != 2 {
		t.Fatalf("unknown: %v %v", got, err)
	}
	e.s.CheckCollectorHealth(e.ctx)
	hs, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hs {
		if h.Name == "session facts" {
			found = true
			if !strings.Contains(h.Problem, "brand-new 2") {
				t.Errorf("problem: %q", h.Problem)
			}
		}
	}
	if !found {
		t.Error("no session facts entry in the collector health")
	}
}

func factsEnv(t *testing.T) (e *env, jo *Actor) {
	e = transcriptEnv(t)
	jo = e.user("jo", false)
	lines := fPrompt("a", 1, "human") +
		fAssistant("m1", 2, 10, fTool("t1", "Edit", fm{"file_path": "/r/a.go"}), fTool("t2", "Read", fm{"file_path": "/r/docs/guide.md"})) +
		fResult("t1", 2, false, fm{"type": "update", "structuredPatch": []fm{{"lines": []string{"+x", "+y", "-z"}}}}) +
		fResult("t2", 2, false, nil) +
		// second task's stretch: starts at minute 20
		fPrompt("b", 20, "") +
		fAssistant("m2", 21, 20, fTool("t3", "Write", fm{"file_path": "/r/a_test.go"}), fTool("t4", "Read", fm{"file_path": "/r/docs/guide.md"})) +
		fResult("t3", 21, false, fm{"type": "create", "content": "a\nb\n"}) +
		fResult("t4", 21, false, nil) +
		fAssistant("m3", 22, 30, fTool("t5", "Read", fm{"file_path": "/r/docs/guide.md"})) +
		fj(fm{"type": "permission-mode", "permissionMode": "plan", "sessionId": "s1"}) + // no timestamp; a setting, which the untimed count leaves out
		fj(fm{"type": "system", "subtype": "compact_boundary", "uuid": "cb1", "compactMetadata": fm{"trigger": "auto"}}) + // no timestamp
		fj(fm{"type": "attachment", "uuid": "i1", "timestamp": fat(1, 0), "attachment": fm{"type": "instructions",
			"files": []fm{{"path": "/r/AGENTS.md", "type": "Project", "content": "12345678"}}}})
	e.putFile(jo, "s1", "main", 0, lines)
	e.putFile(jo, "s1", "subagents/agent-x.jsonl", 0, fAssistant("sm1", 3, 4))
	e.quiet()
	if run := e.derive(); run.Derived != 2 || run.Failed != 0 {
		t.Fatalf("derive: %+v", run)
	}
	return e, jo
}

func TestFactsReadSession(t *testing.T) {
	e, jo := factsEnv(t)
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status.FilesArchived != 2 || f.Status.FilesDerived != 2 || f.Status.FilesWaiting != 0 {
		t.Errorf("status: %+v", f.Status)
	}
	if f.Turns.Total != 2 || f.Turns.ByOrigin["human"] != 1 || f.Turns.ByOrigin["unknown"] != 1 {
		t.Errorf("turns: %+v (a turn whose record has no origin counts as unknown)", f.Turns)
	}
	if f.ModelCalls.Total != 4 || f.ModelCalls.Sidechain != 1 || f.ModelCalls.Output.Sum != 10+20+30+4 {
		t.Errorf("calls: %+v", f.ModelCalls)
	}
	if f.ModelCalls.Thinking.Known != 0 || f.ModelCalls.Thinking.Unknown != 4 {
		t.Errorf("thinking tokens were not recorded: they are unknown, not 0: %+v", f.ModelCalls.Thinking)
	}
	c := f.Changes
	if c.Edits != 2 || c.FilesChanged != 2 || c.LinesAdded.Sum != 4 || c.LinesRemoved.Sum != 1 || c.TestEdits != 1 || c.TestFiles != 1 || c.TestLinesAdd.Sum != 2 {
		t.Errorf("changes: %+v", c)
	}
	if f.DocsRead.Calls != 3 || f.DocsRead.Distinct != 1 || f.Repeated.Repeats != 2 || len(f.Repeated.Top) != 1 || f.Repeated.Top[0].Times != 3 {
		t.Errorf("docs %+v repeated %+v", f.DocsRead, f.Repeated)
	}
	if f.Injected.Count != 1 || f.Injected.Bytes.Sum != 8 || f.Injected.Top[0].Name != "/r/AGENTS.md" || *f.Injected.Top[0].Bytes != 8 {
		t.Errorf("injected: %+v", f.Injected)
	}
	if !f.CacheExpiry.Inferred {
		t.Errorf("cache expiry must be labelled inferred")
	}
}

func TestFactsReadTaskByWindows(t *testing.T) {
	e, jo := factsEnv(t)
	p := e.project(jo, "Acme", "")
	i1 := e.issue(jo, IssueInput{Project: p.Key, Title: "first"})
	i2 := e.issue(jo, IssueInput{Project: p.Key, Title: "second"})
	uid := jo.User.ID
	// task 1 ran from minute 0 to minute 19; task 2 took over at minute 20
	e.exec(`INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, updated_at) VALUES (?, 'box', 's1', 1, ?, ?, ?, 'next', 1)`,
		uid, i1.ID, fatMS(0, 0), fatMS(19, 0))
	e.exec(`INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, updated_at) VALUES (?, 'box', 's1', 2, ?, ?, ?, '', 1)`,
		uid, i2.ID, fatMS(20, 0), fatMS(23, 0))

	f1, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: i1.Key})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: i2.Key})
	if err != nil {
		t.Fatal(err)
	}
	// task 1: the first prompt, its call, its edit, the sub-agent's call at minute 3, the instruction file
	if f1.Turns.Total != 1 || f1.ModelCalls.Total != 2 || f1.Changes.FilesChanged != 1 || f1.Changes.TestEdits != 0 || f1.Injected.Count != 1 {
		t.Errorf("task 1: turns %d calls %d changes %+v injected %d", f1.Turns.Total, f1.ModelCalls.Total, f1.Changes, f1.Injected.Count)
	}
	// task 2 (open-ended): the second prompt and the calls after it
	if f2.Turns.Total != 1 || f2.ModelCalls.Total != 2 || f2.Changes.TestEdits != 1 || f2.DocsRead.Calls != 2 || f2.Injected.Count != 0 {
		t.Errorf("task 2: turns %d calls %d changes %+v docs %d injected %d", f2.Turns.Total, f2.ModelCalls.Total, f2.Changes, f2.DocsRead.Calls, f2.Injected.Count)
	}
	if f1.Scope != "task" || len(f1.Sources) != 1 || f1.Sources[0].To == nil || f2.Sources[0].To != nil {
		t.Errorf("windows: %+v %+v", f1.Sources, f2.Sources)
	}
	// the status is the sessions' files, not the windows, and says so; the
	// row with no timestamp is in no window, and the answer says how many
	if !f1.Status.WholeSession || !f2.Status.WholeSession || f1.Status.FilesArchived != 2 {
		t.Errorf("a task's status must be named whole-session: %+v", f1.Status)
	}
	if f1.UntimedLeftOut != 1 || f2.UntimedLeftOut != 1 {
		t.Errorf("untimed rows left out: %d, %d, want 1", f1.UntimedLeftOut, f2.UntimedLeftOut)
	}
	if sf, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"}); err != nil || sf.Status.WholeSession || sf.UntimedLeftOut != 0 {
		t.Errorf("a session read is not whole-session-labelled and leaves nothing out: %v %+v", err, sf)
	}
	// nothing is counted twice across the two tasks
	if f1.ModelCalls.Total+f2.ModelCalls.Total != 4 {
		t.Errorf("the tasks' calls do not add up to the session's")
	}
}

func TestFactsReadAccess(t *testing.T) {
	e, jo := factsEnv(t)
	kim := e.user("kim", false)
	if _, err := e.s.SessionFacts(e.ctx, kim, FactsQuery{Session: "s1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another person's session id holds nothing of yours: %v", err)
	}
	if _, err := e.s.SessionFacts(e.ctx, kim, FactsQuery{Session: "s1", User: "jo"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("reading another person's facts: %v", err)
	}
	if f, err := e.s.SessionFacts(e.ctx, agentActor(jo, "anthropic/claude-code"), FactsQuery{Session: "s1"}); !errors.Is(err, ErrForbidden) || f != nil {
		t.Errorf("an agent's shell must be refused with forbidden and no facts: %v %v", err, f)
	}
	if f, err := e.s.SessionFacts(e.ctx, e.admin, FactsQuery{Session: "s1", User: "jo"}); err != nil || f.ModelCalls.Total != 4 {
		t.Errorf("admin: %v %+v", err, f)
	}
	if _, err := e.s.SessionFacts(e.ctx, nil, FactsQuery{Session: "s1"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: %v", err)
	}
	if _, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{}); !IsInvalid(err) {
		t.Errorf("neither session nor task: %v", err)
	}
	if _, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1", Task: "X-1"}); !IsInvalid(err) {
		t.Errorf("both: %v", err)
	}
	// a task whose traces are another person's is not readable by you
	p := e.project(e.admin, "Acme", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "t"})
	e.exec(`INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, updated_at) VALUES (?, 'box', 's1', 1, ?, ?, ?, '', 1)`,
		jo.User.ID, i.ID, fatMS(0, 0), fatMS(5, 0))
	if _, err := e.s.SessionFacts(e.ctx, e.admin, FactsQuery{Task: i.Key}); !errors.Is(err, ErrNotFound) {
		t.Errorf("admin without --user sees only their own sessions' share of a task: %v", err)
	}
	if f, err := e.s.SessionFacts(e.ctx, e.admin, FactsQuery{Task: i.Key, User: "jo"}); err != nil || f.ModelCalls.Total == 0 {
		t.Errorf("admin with --user: %v", err)
	}
	// a person whose own session traced a task in a project they cannot view
	// is told it is not found: the facts of a task are not a way around the
	// project's access
	priv := e.project(e.admin, "Priv", "")
	pi := e.issue(e.admin, IssueInput{Project: priv.Key, Title: "hidden"})
	e.exec(`INSERT INTO task_traces(user_id, machine, session, seq, issue_id, started_at, last_at, ended, updated_at) VALUES (?, 'box', 's1', 2, ?, ?, ?, '', 1)`,
		jo.User.ID, pi.ID, fatMS(10, 0), fatMS(15, 0))
	if f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Task: pi.Key}); !errors.Is(err, ErrNotFound) || f != nil {
		t.Errorf("a task in a project the person cannot view: %v %v", err, f)
	}
}

// A session archived but not yet derived says so rather than reporting zeros.
func TestFactsReadBeforeDerivation(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	e.putFile(jo, "s1", "main", 0, fAssistant("m1", 2, 5))
	f, err := e.s.SessionFacts(e.ctx, jo, FactsQuery{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status.FilesArchived != 1 || f.Status.FilesWaiting != 1 || f.Status.FilesDerived != 0 {
		t.Errorf("status: %+v", f.Status)
	}
}
