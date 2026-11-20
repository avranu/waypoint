package core

import (
	"strings"
	"testing"
	"time"
)

type escapeEnv struct {
	*env
	forge int64
	proj  *Project
}

func newEscapeEnv(t *testing.T) *escapeEnv {
	t.Helper()
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO forges(kind, name, base_url, token, created_at) VALUES ('forgejo','F','http://f.test','x',1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return &escapeEnv{env: e, forge: id, proj: p}
}

// mergedPR adds a merged pull request, returning its id.
func (e *escapeEnv) mergedPR(t *testing.T, n int, at time.Time, merge string) int64 {
	t.Helper()
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, merge_sha, merged_at, created_at, updated_at)
		VALUES (?, 'o/r', ?, ?, 'merged', ?, ?, ?, ?)`, e.forge, n, "http://f.test/o/r/pulls/"+itoa(n), merge, at.UnixMilli(), at.UnixMilli(), at.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *escapeEnv) bug(t *testing.T, title string, pr int) *IssueDetail {
	t.Helper()
	i := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: title, Type: "bug"})
	if pr > 0 {
		e.link(t, i.ID, pr)
	}
	return i
}

func (e *escapeEnv) link(t *testing.T, issue int64, pr int) {
	t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, created_at)
		VALUES (?, 'forgejo', 'pull', ?, 'o/r', ?, 't', 'merged', 1)`, issue, "http://f.test/o/r/pulls/"+itoa(pr), pr); err != nil {
		t.Fatal(err)
	}
}

func (e *escapeEnv) record(t *testing.T, pr int64, intro FixIntroducerInput, method string) {
	t.Helper()
	in := FixTraceInput{PR: pr, Machine: "m", Commit: "placed", Method: method, Lines: intro.Lines, Files: []FixFileInput{{Path: "internal/core/forge.go", Lines: 2}}}
	if intro.Merge != "" {
		in.Introducers = []FixIntroducerInput{intro}
	}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
}

func TestEscapesDueOnlyListsBugLinkedMergedFixes(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	bugPR := e.mergedPR(t, 1, now, "aaa")
	e.mergedPR(t, 2, now, "bbb")
	e.bug(t, "broken", 1)
	task := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: "Fix the wording", Type: "task"})
	e.link(t, task.ID, 2)

	due, err := e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].PR != bugPR || due[0].Merge != "aaa" {
		t.Fatalf("due = %+v", due)
	}
	e.record(t, bugPR, FixIntroducerInput{}, "none")
	if due, err = e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0); err != nil || len(due) != 0 {
		t.Fatalf("after the trace: %+v %v", due, err)
	}
}

func TestEscapeLedgerNumbers(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	e.mergedPR(t, 1, now.Add(-20*day), "m1")
	fix := e.mergedPR(t, 2, now.Add(-10*day), "m2")
	fix2 := e.mergedPR(t, 3, now.Add(-day), "m3")
	first := e.bug(t, "first", 2)
	e.bug(t, "second, found by grok review", 3)

	e.record(t, fix, FixIntroducerInput{Merge: "m1", PR: 1, Lines: 4, Share: 1, At: now.Add(-20 * day).UnixMilli()}, "szz")
	e.record(t, fix2, FixIntroducerInput{Merge: "m2", Lines: 1, Share: 0.8, At: now.Add(-10 * day).UnixMilli()}, "szz")

	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key, Days: 90})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Traced != 2 || st.Confident != 2 {
		t.Fatalf("stats = %+v", st)
	}
	if st.MedianDays == nil || *st.MedianDays < 9 || *st.MedianDays > 10 {
		t.Fatalf("median = %v", st.MedianDays)
	}
	if len(st.Tags) != 1 || st.Tags[0].Name != "forge" || st.Tags[0].Count != 2 {
		t.Fatalf("tags = %+v", st.Tags)
	}
	if st.FixOfFix != 1 || len(st.Chains) != 1 || len(st.Chains[0]) != 2 || !strings.HasSuffix(st.Chains[0][0], "-"+itoa64(first.Number)) {
		t.Fatalf("chains = %d %+v", st.FixOfFix, st.Chains)
	}
	if len(st.Detectors) != 1 || st.Detectors[0].Name != "person" {
		t.Fatalf("detectors = %+v", st.Detectors)
	}
	if len(st.Weeks) == 0 {
		t.Fatal("no weeks")
	}

	files, err := e.s.EscapeFilesFor(e.ctx, e.admin, "O/R")
	if err != nil {
		t.Fatal(err)
	}
	if files.Total != 2 || len(files.Files) != 1 || files.Files[0].Escapes != 2 {
		t.Fatalf("files = %+v", files)
	}

	shown, err := e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: first.Key})
	if err != nil || len(shown) != 1 || len(shown[0].Introducers) != 1 || shown[0].Introducers[0].Number != 1 {
		t.Fatalf("show = %+v %v", shown, err)
	}
}

func TestEscapeLateLinkAndContextConfidence(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	fix := e.mergedPR(t, 2, now.Add(-day), "m2")
	e.bug(t, "linked first", 2)
	e.record(t, fix, FixIntroducerInput{Merge: "m1", Lines: 2, Share: 1, At: now.Add(-3 * day).UnixMilli()}, "context")

	late := e.bug(t, "linked after the trace", 0)
	e.link(t, late.ID, 2)
	if _, err := e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0); err != nil {
		t.Fatal(err)
	}
	list, err := e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}
	for _, r := range list {
		if r.Confidence != contextWeight {
			t.Fatalf("confidence of %s = %v", r.Key, r.Confidence)
		}
	}
	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil || st.Confident != 0 || st.MedianDays != nil || st.MedianAllDays == nil {
		t.Fatalf("stats = %+v %v", st, err)
	}
}

func TestEscapesHideOtherProjects(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	fix := e.mergedPR(t, 2, now, "m2")
	e.bug(t, "private bug", 2)
	e.record(t, fix, FixIntroducerInput{}, "none")

	zed := e.user("zed", false)
	st, err := e.s.EscapeSummary(e.ctx, zed, EscapeQuery{})
	if err != nil || st.Total != 0 {
		t.Fatalf("stats = %+v %v", st, err)
	}
	if err := e.s.RecordFixTrace(e.ctx, zed, FixTraceInput{PR: fix, Method: "none"}); err == nil {
		t.Fatal("an outsider recorded a trace")
	}
}

// A fix the runner couldn't place (a squash whose merge commit the forge
// hadn't reported) comes back once the merge commit is known, and the
// corrected trace reaches the escape already made.
func TestEscapeUnplacedFixIsTracedAgainWhenTheMergeIsKnown(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	fix := e.mergedPR(t, 2, now, "")
	bug := e.bug(t, "squashed", 2)
	unplaced := FixTraceInput{PR: fix, Method: "none", Note: "the head isn't on the base branch"}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, unplaced); err != nil {
		t.Fatal(err)
	}
	if due, err := e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0); err != nil || len(due) != 0 {
		t.Fatalf("with the same merge commit: %+v %v", due, err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE pull_requests SET merge_sha = 'squash' WHERE id = ?`, fix); err != nil {
		t.Fatal(err)
	}
	due, err := e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0)
	if err != nil || len(due) != 1 || due[0].Merge != "squash" {
		t.Fatalf("once the merge commit is known: %+v %v", due, err)
	}
	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil || st.Pending != 1 {
		t.Fatalf("pending = %+v %v", st, err)
	}
	in := FixTraceInput{PR: fix, TriedMerge: "squash", Commit: "squash", Method: "szz", Lines: 1,
		Introducers: []FixIntroducerInput{{Merge: "m1", Lines: 1, Share: 1, At: now.Add(-48 * time.Hour).UnixMilli()}}}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
	list, err := e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: bug.Key})
	if err != nil || len(list) != 1 || list[0].IntroMerge != "m1" || list[0].Confidence != 1 {
		t.Fatalf("after the corrected trace: %+v %v", list, err)
	}
}

// A fix with no code files says nothing about any file, so it can't be what
// tips a repo over to the ledger.
func TestEscapeFilesTotalNeedsFiles(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	empty := e.mergedPR(t, 2, now, "m2")
	e.bug(t, "tests only", 2)
	if err := e.s.RecordFixTrace(e.ctx, e.admin, FixTraceInput{PR: empty, Commit: "m2", Method: "none", Note: "the fix changes only tests or docs"}); err != nil {
		t.Fatal(err)
	}
	withFile := e.mergedPR(t, 3, now, "m3")
	e.bug(t, "code", 3)
	e.record(t, withFile, FixIntroducerInput{}, "none")
	got, err := e.s.EscapeFilesFor(e.ctx, e.admin, "o/r")
	if err != nil || got.Total != 1 {
		t.Fatalf("files = %+v %v", got, err)
	}
}

// One fix, two bugs filed apart: a change between the two filings can only
// have caused the later bug.
func TestEscapeIntroducerMustPredateEachBug(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	fix := e.mergedPR(t, 2, now, "m2")
	early := e.bug(t, "early", 2)
	late := e.bug(t, "late", 2)
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE issues SET created_at = ? WHERE id = ?`, now.Add(-10*day).UnixMilli(), early.ID); err != nil {
		t.Fatal(err)
	}
	e.record(t, fix, FixIntroducerInput{Merge: "between", Lines: 1, Share: 1, At: now.Add(-5 * day).UnixMilli()}, "szz")
	for key, want := range map[string]string{early.Key: "", late.Key: "between"} {
		list, err := e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: key})
		if err != nil || len(list) != 1 || list[0].IntroMerge != want {
			t.Fatalf("%s: %+v %v", key, list, err)
		}
	}
}

// A trace becomes the ledger of every project with a bug on the fix, so an
// editor of only one of them can neither be asked for it nor upload it.
func TestEscapeTraceNeedsEveryLinkedProject(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	fix := e.mergedPR(t, 2, now, "m2")
	e.bug(t, "in alpha", 2)
	other := e.project(e.admin, "Beta", "kanban")
	b := e.issue(e.admin, IssueInput{Project: other.Key, Title: "in beta", Type: "bug"})
	e.link(t, b.ID, 2)
	ed := e.user("ed", false)
	if err := e.s.SetMember(e.ctx, e.admin, e.proj.Key, "ed", "editor"); err != nil {
		t.Fatal(err)
	}
	if due, err := e.s.EscapesDue(e.ctx, ed, "", "o/r", 0); err != nil || len(due) != 0 {
		t.Fatalf("due = %+v %v", due, err)
	}
	if err := e.s.RecordFixTrace(e.ctx, ed, FixTraceInput{PR: fix, Method: "none"}); err == nil {
		t.Fatal("an editor of one project wrote the other's ledger")
	}
	if due, err := e.s.EscapesDue(e.ctx, e.admin, "", "o/r", 0); err != nil || len(due) != 1 {
		t.Fatalf("admin due = %+v %v", due, err)
	}
}

// A fix closing several bugs is one fix of a fix, not several.
func TestEscapeFixOfFixCountsPullRequests(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	first := e.mergedPR(t, 1, now.Add(-20*day), "m1")
	e.bug(t, "first", 1)
	second := e.mergedPR(t, 2, now.Add(-day), "m2")
	for _, title := range []string{"a", "b", "c"} {
		e.bug(t, title, 2)
	}
	e.record(t, first, FixIntroducerInput{}, "none")
	e.record(t, second, FixIntroducerInput{Merge: "m1", PR: 1, Lines: 1, Share: 1, At: now.Add(-20 * day).UnixMilli()}, "szz")
	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil || st.FixOfFix != 1 || len(st.Chains) != 1 {
		t.Fatalf("fix of fix = %+v %v", st, err)
	}
}

func TestClassifyFinding(t *testing.T) {
	cases := []struct {
		name            string
		f               findingFacts
		stage, detector string
	}{
		{"audit label", findingFacts{labels: []string{"audit"}, textAgent: "grok"}, foundAudit, "audit"},
		{"flaky", findingFacts{labels: []string{"flaky"}, textAgent: "waypoint"}, foundPRCI, "ci_flake"},
		{"ci fault", findingFacts{labels: []string{"ci"}, textAgent: "waypoint"}, foundPRCI, "ci_fault"},
		{"ci label by a person isn't a fault", findingFacts{labels: []string{"ci"}}, foundPerson, "person"},
		{"automation", findingFacts{automation: "Watch logs", textAgent: "x", description: "seen in the server log"}, foundProdLog, "automation:Watch logs"},
		{"forge stranger", findingFacts{outside: true, textAgent: "forge"}, foundPerson, "forge"},
		{"person", findingFacts{title: "CodeRabbit is wrong"}, foundPerson, "person"},
		{"agent prod log", findingFacts{textAgent: "claude", description: "WARN forge sync failed"}, foundProdLog, "agent"},
		{"agent review", findingFacts{textAgent: "claude", title: "Grok found a nil deref"}, foundReview, "agent"},
		{"agent main ci", findingFacts{textAgent: "claude", title: "This broke main"}, foundMainCI, "agent"},
		{"words alone never say pr ci", findingFacts{textAgent: "claude", title: "PR CI failed"}, foundUse, "agent"},
		{"lowercase warn is a word", findingFacts{textAgent: "claude", title: "warn users"}, foundUse, "agent"},
		{"waypoint filer", findingFacts{textAgent: "waypoint", title: "Stuck"}, foundUse, "waypoint"},
	}
	for _, c := range cases {
		if s, d := classifyFinding(c.f); s != c.stage || d != c.detector {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, s, d, c.stage, c.detector)
		}
	}
}

func TestEscapeTags(t *testing.T) {
	got := escapeTags([]FixFileInput{{Path: "internal/db/migrations/1.sql"}, {Path: "cmd/wp/x.go"}, {Path: ".forgejo/workflows/ci.yml"}, {Path: "web/src/a.ts"}, {Path: "internal/forge/f.go"}})
	if strings.Join(got, ",") != "ci,cli,forge,migration,web" {
		t.Fatalf("tags = %v", got)
	}
}
