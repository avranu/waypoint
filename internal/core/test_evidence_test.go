package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSummarizeEvidence(t *testing.T) {
	const now = int64(10 * time.Hour / time.Millisecond)
	wide := TestRun{At: 5, Command: "go test ./...", Outcome: EvidencePass, Passed: 1664, Skipped: 2, Units: 31, UnitName: "packages"}
	narrow := TestRun{At: 6, Command: "go test ./cmd/wp", Outcome: EvidencePass, Passed: 40, Units: 1, UnitName: "packages"}
	for _, tc := range []struct {
		name    string
		runs    []TestRun
		near    *TestRunNear
		outcome string
		want    string
	}{
		{"widest leads, never a sum", []TestRun{narrow, wide}, nil, EvidencePass, "Tests on this tree: 1,664 passed, 2 skipped in 31 packages (go test ./...); 1 other run"},
		{"uncounted", []TestRun{{Command: "make check", Outcome: EvidencePass, Units: 31, UnitName: "packages"}}, nil, EvidencePass, "Tests on this tree: 31 packages passed (make check)"},
		{"failure leads", []TestRun{wide, {At: 7, Command: "go test ./x", Outcome: EvidenceFail, Failed: 2, Passed: 1, Units: 1, UnitName: "packages", Failures: []string{"TestA", "TestB"}}}, nil, EvidenceFail,
			"Tests on this tree: 2 failed, 1 passed in 1 package (go test ./x): TestA, TestB; 1 other run"},
		{"newest of a command counts", []TestRun{{At: 1, Command: "c", Outcome: EvidenceFail, Failed: 1}, {At: 2, Command: "c", Outcome: EvidencePass, Passed: 3}}, nil, EvidencePass, "Tests on this tree: 3 passed (c)"},
		{"a wider pass clears a timeout", []TestRun{
			{At: 1, Command: "go test ./x ./y", Outcome: EvidenceFail, Failed: 0, Passed: 1529, Skipped: 2, Units: 2, Packages: []string{"x", "y"}, Failures: []string{"x (timed out after 10m0s)"}},
			{At: 2, Command: "go test ./x ./y -timeout 40m", Outcome: EvidencePass, Passed: 2247, Skipped: 2, Units: 2, Packages: []string{"x", "y"}},
		}, nil, EvidencePass, "Tests on this tree: 2,247 passed, 2 skipped in 2 packages (go test ./x ./y -timeout 40m); an earlier run failed (x (timed out after 10m0s)) and a later one passed"},
		{"a narrower pass doesn't", []TestRun{
			{At: 1, Command: "go test ./x ./y", Outcome: EvidenceFail, Passed: 1529, Units: 2, Packages: []string{"x", "y"}, Failures: []string{"x (timed out after 10m0s)"}},
			{At: 2, Command: "go test ./y", Outcome: EvidencePass, Passed: 2000, Units: 1, Packages: []string{"y"}},
		}, nil, EvidenceFail, "Tests on this tree: 1,529 passed in 2 packages (go test ./x ./y): x (timed out after 10m0s); 1 other run"},
		{"nor does a pass clear a timeout that recorded no packages", []TestRun{
			{At: 1, Command: "go test ./x", Outcome: EvidenceFail, Passed: 9, Units: 1, Failures: []string{"x (timed out after 10m0s)"}},
			{At: 2, Command: "go test ./z", Outcome: EvidencePass, Passed: 20, Units: 1, Packages: []string{"z"}},
		}, nil, EvidenceFail, "Tests on this tree: 9 passed in 1 package (go test ./x): x (timed out after 10m0s); 1 other run"},
		{"nor does a pass clear a failing test", []TestRun{
			{At: 1, Command: "go test ./x", Outcome: EvidenceFail, Failed: 1, Passed: 9, Units: 1, Packages: []string{"x"}, Failures: []string{"x.TestA", "x (timed out after 10m0s)"}},
			{At: 2, Command: "go test -timeout 40m ./x", Outcome: EvidencePass, Passed: 20, Units: 1, Packages: []string{"x"}},
		}, nil, EvidenceFail, "Tests on this tree: 1 failed, 9 passed in 1 package (go test ./x): x.TestA, x (timed out after 10m0s); 1 other run"},
		{"failed with no counts", []TestRun{{Command: "c", Outcome: EvidenceFail, Failures: []string{"the command failed"}}}, nil, EvidenceFail, "Tests on this tree: the command failed (c): the command failed"},
		{"matched nothing", []TestRun{{Command: "go test -run X", Outcome: EvidenceEmpty, Units: 1}}, nil, EvidenceEmpty, "No test ran on this tree (go test -run X): nothing matched"},
		{"a pass beats an empty run", []TestRun{{Command: "e", Outcome: EvidenceEmpty, Units: 90}, narrow}, nil, EvidencePass, "Tests on this tree: 40 passed in 1 package (go test ./cmd/wp); 1 other run"},
		{"none at all", nil, nil, EvidenceNone, "No test run recorded on this tree"},
		{"near", nil, &TestRunNear{TestRun: TestRun{At: now - int64(2*time.Hour/time.Millisecond), Outcome: EvidencePass, Passed: 118}, FilesAway: 3}, EvidenceNone, "No test run on this tree; the last was before 3 files changed (118 passed, 2 hours ago)"},
		{"near moved", nil, &TestRunNear{TestRun: TestRun{Outcome: EvidenceEmpty, Moved: true}, FilesAway: -1}, EvidenceNone, "No test run on this tree; the last ran while its files were changing (no test ran)"},
		{"near pruned", nil, &TestRunNear{TestRun: TestRun{Outcome: EvidencePass, Passed: 1}, FilesAway: -1}, EvidenceNone, "No test run on this tree; the last ran on another tree (1 passed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, line := summarizeEvidence(tc.runs, tc.near, now)
			if outcome != tc.outcome || line != tc.want {
				t.Errorf("got %s %q\nwant %s %q", outcome, line, tc.outcome, tc.want)
			}
		})
	}
}

func TestFindStatements(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"All tests pass.", "tests:"},
		{"Tests are green.", "tests:"},
		{"make check passes.", "tests:"},
		{"CI tests pass.", ""},
		{"Tests pass once this lands.", ""},
		{"Tests don't pass.", ""},
		{"PR #12 merged.", "merged:12"},
		{"Merged PR #12.", "merged:12"},
		{"Merged #12 into main.", "merged:12"},
		{"This was merged.", "merged:"},
		{"It will be merged when CI is green.", ""},
		{"It's now deployed.", "deployed:"},
		{"The fix is deployed.", "deployed:"},
		{"Live in production: the fix is now live in prod.", "deployed:"},
		{"Deploy this when ready.", ""},
		{"I applied R-214.", "applied:214"},
		{"R-214 was applied.", "applied:214"},
		{"R-214 isn't applied.", "applied:214!"},
		{"R-214 is not applied yet.", "applied:214!"},
		{"Should I apply R-214?", ""},
		{"> PR #12 merged.", ""},
		{"`PR #12 merged`", ""},
		{"```\nPR #12 merged.\n```", ""},
		{"Search is 30% faster now.", "faster:"},
		{"This speeds up the import.", "faster:"},
		{"CI is faster.", ""},
		{"It should be faster.", ""},
	} {
		var got []string
		for _, c := range findStatements(tc.text) {
			s := c.kind + ":" + c.ref
			if c.negative {
				s += "!"
			}
			got = append(got, s)
		}
		if g := strings.Join(got, " "); g != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, g, tc.want)
		}
	}
}

func (a *apEnv) evidence(key, sha, summary string, runs ...TestRun) *TestEvidence {
	a.t.Helper()
	e, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: sha, Tree: "tree" + sha, Issue: key, Summary: summary, Runs: runs})
	if err != nil {
		a.t.Fatal(err)
	}
	return e
}

func (a *apEnv) mismatches(key string) []*WorkLogEntry {
	a.t.Helper()
	wl, err := a.s.GetWorkLog(a.ctx, a.admin, key)
	if err != nil {
		a.t.Fatal(err)
	}
	var out []*WorkLogEntry
	for _, e := range wl.Entries {
		if e.Kind == WorkMismatch {
			out = append(out, e)
		}
	}
	return out
}

func TestRecordTestEvidence(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	pass := TestRun{At: 1, Command: "go test ./...", Outcome: EvidencePass, Passed: 9, Units: 2, UnitName: "packages"}

	if e := a.evidence(i.Key, "abc1234", "Done.", pass); e.Outcome != EvidencePass || !strings.HasPrefix(e.Line, "Tests on this tree: 9 passed") {
		t.Fatalf("recorded: %+v", e)
	}
	fail := TestRun{At: 2, Command: "go test ./...", Outcome: EvidenceFail, Failed: 1, Units: 1, Failures: []string{"TestX"}}
	if e := a.evidence(i.Key, "abc1234", "All tests pass.", fail); e.Outcome != EvidenceFail || len(e.Notices) != 1 || !strings.Contains(e.Notices[0], "TestX") {
		t.Fatalf("replaced: %+v", e)
	}
	var rows int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM test_evidence`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows %d %v", rows, err)
	}
	d, err := a.s.GetIssue(a.ctx, a.admin, i.Key)
	if err != nil || d.Tested == nil || d.Tested.Outcome != EvidenceFail {
		t.Fatalf("issue: %+v %v", d.Tested, err)
	}

	if e := a.evidence(i.Key, "abc1234", "All tests pass.", fail); len(e.Notices) != 0 {
		t.Errorf("noted again: %v", e.Notices)
	}
	cs := a.mismatches(i.Key)
	if len(cs) != 1 || cs[0].Tag != "tests" || cs[0].Title != "Said: All tests pass." || !strings.Contains(cs[0].Reason, "TestX") {
		t.Fatalf("mismatches: %+v", cs)
	}
	wl, err := a.s.GetWorkLog(a.ctx, a.admin, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	tested := 0
	for _, e := range wl.Entries {
		if e.Kind == WorkTested {
			tested++
		}
	}
	if tested != 1 || wl.Summary.Mismatches != 1 {
		t.Errorf("tested %d mismatches %d", tested, wl.Summary.Mismatches)
	}
	st, err := a.s.GetWorkLogStats(a.ctx, a.admin, a.p.Key, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Evidence) != 1 || st.Evidence[0].Ships != 1 || st.Evidence[0].OnTree != 1 || st.Evidence[0].Mismatches != 1 {
		t.Errorf("weeks: %+v", st.Evidence)
	}

	if e := a.evidence(i.Key, "def5678", "Done."); e.Outcome != EvidenceNone {
		t.Errorf("none: %+v", e)
	}
	st, _ = a.s.GetWorkLogStats(a.ctx, a.admin, a.p.Key, 30)
	if st.Evidence[0].Ships != 2 || st.Evidence[0].OnTree != 1 {
		t.Errorf("weeks: %+v", st.Evidence)
	}

	if e := a.evidence("APP-9999", "fff0000", "All tests pass.", fail); e.Outcome != EvidenceFail || len(e.Notices) != 0 {
		t.Errorf("no task: %+v", e)
	}
	if _, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: "1", Tree: "2", Runs: []TestRun{{Outcome: "bogus"}}}); err == nil {
		t.Error("a bad outcome was taken")
	}
	if _, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: "1", Tree: "2", Runs: []TestRun{{Outcome: EvidencePass, Tree: strings.Repeat("a", maxEvidenceHash+1)}}}); err == nil {
		t.Error("an overlong tree on a run was taken")
	}
}

func TestCheckStatements(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	check := func(text string) []StatementMismatch {
		t.Helper()
		var got []StatementMismatch
		err := a.s.write(a.ctx, a.admin, func(tx *tx) error {
			var err error
			got, err = a.s.checkStatements(a.ctx, tx, statementScope{issue: i.ID, project: i.ProjectID, key: i.Key, agent: "claude", source: "stop"}, text, a.s.nowMS())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := check("PR #7 merged. The fix is deployed. All tests pass."); len(got) != 0 {
		t.Errorf("no records: %+v", got)
	}

	a.linkPR(i.ID, 7, "abc")
	if got := check("PR #7 merged."); len(got) != 1 || got[0].Fact != "pull request #7 is open" {
		t.Errorf("merged: %+v", got)
	}
	if got := check("PR #8 merged."); len(got) != 0 {
		t.Errorf("a PR of another task: %+v", got)
	}
	if got := check("The fix is deployed."); len(got) != 1 || !strings.Contains(got[0].Fact, "can't be deployed") {
		t.Errorf("deployed: %+v", got)
	}
	if got := check("WP-2 is deployed too."); len(got) != 0 {
		t.Errorf("another task's deployment: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged'`); err != nil {
		t.Fatal(err)
	}
	if got := check("This was merged. The change is now deployed to production."); len(got) != 0 {
		t.Errorf("merged PR: %+v", got)
	}

	var n int
	r, _, err := a.s.CreateRequest(a.ctx, a.ag, RequestInput{Project: a.p.Key, Kind: RequestAction, Title: "Do a thing"})
	if err != nil {
		t.Fatal(err)
	}
	n = r.Number
	ref := fmt.Sprintf("R-%d", n)
	if got := check("I applied " + ref + "."); len(got) != 1 || got[0].Fact != ref+" is open" {
		t.Errorf("applied open: %+v", got)
	}
	if got := check(ref + " is not applied."); len(got) != 0 {
		t.Errorf("true denial: %+v", got)
	}
	setRequest := func(state string) {
		t.Helper()
		if _, err := a.s.db.W.Exec(`UPDATE requests SET state = ? WHERE number = ?`, state, n); err != nil {
			t.Fatal(err)
		}
	}
	setRequest(RequestApplying)
	if got := check("I applied " + ref + "."); len(got) != 1 || got[0].Fact != ref+" is applying" {
		t.Errorf("applied while applying: %+v", got)
	}
	setRequest(RequestApproved)
	if got := check("I applied " + ref + "."); len(got) != 0 {
		t.Errorf("applied once approved: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'applied' WHERE number = ?`, n); err != nil {
		t.Fatal(err)
	}
	if got := check("Now " + ref + " isn't applied."); len(got) != 1 || got[0].Fact != ref+" is applied" {
		t.Errorf("false denial: %+v", got)
	}
	if got := check("Second, " + ref + " was applied."); len(got) != 0 {
		t.Errorf("true statement: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET result = '{"no_change":true}' WHERE number = ?`, n); err != nil {
		t.Fatal(err)
	}
	if got := check("Second, " + ref + " was applied."); len(got) != 1 || !strings.Contains(got[0].Fact, "changed nothing") {
		t.Errorf("applied, no change: %+v", got)
	}
	if got := check(ref + " isn't applied."); len(got) != 0 {
		t.Errorf("true denial of a no-op: %+v", got)
	}
	if got := check("R-99999 was applied."); len(got) != 0 {
		t.Errorf("unknown request: %+v", got)
	}

	a.evidence(i.Key, "abc1234", "", TestRun{At: 1, Command: "c", Outcome: EvidenceFail, Failed: 1, Units: 1})
	if got := check("All tests pass."); len(got) != 1 {
		t.Errorf("fresh failure: %+v", got)
	}
	a.tick(time.Hour)
	if got := check("Tests are all green."); len(got) != 0 {
		t.Errorf("stale failure: %+v", got)
	}
}

func TestNoRecordedRunContradictsNothing(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	if e := a.evidence(i.Key, "abc1234", "All tests pass."); e.Outcome != EvidenceNone || len(e.Notices) != 0 {
		t.Errorf("no runs: %+v", e)
	}
	if got := a.mismatches(i.Key); len(got) != 0 {
		t.Errorf("mismatches: %+v", got)
	}
}

// The same sentence against a different record is a new mismatch.
func TestSameStatementNewFact(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	for _, name := range []string{"TestA", "TestB"} {
		run := TestRun{At: 1, Command: "c", Outcome: EvidenceFail, Failed: 1, Failures: []string{name}}
		if e := a.evidence(i.Key, "abc1234", "All tests pass.", run); len(e.Notices) != 1 || !strings.Contains(e.Notices[0], name) {
			t.Errorf("%s: %+v", name, e.Notices)
		}
	}
	if got := a.mismatches(i.Key); len(got) != 2 {
		t.Errorf("mismatches: %+v", got)
	}
}

// Editing the route's project doesn't reach a task elsewhere the actor can
// only see.
func TestEvidenceLinksOnlyATaskTheActorEdits(t *testing.T) {
	a := newAPEnv(t)
	other := a.project(a.admin, "Other", "")
	i := a.issue(a.admin, IssueInput{Project: other.Key, Title: "Theirs"})
	u := a.user("vera", false)
	for key, role := range map[string]string{a.p.Key: "editor", other.Key: "viewer"} {
		if err := a.s.SetMember(a.ctx, a.admin, key, "vera", role); err != nil {
			t.Fatal(err)
		}
	}
	e, err := a.s.RecordTestEvidence(a.ctx, u, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: "abc1234", Tree: "t", Issue: i.Key, Summary: "All tests pass.",
		Runs: []TestRun{{At: 1, Command: "c", Outcome: EvidenceFail, Failed: 1}}})
	if err != nil || len(e.Notices) != 0 {
		t.Fatalf("%+v %v", e, err)
	}
	if got := a.mismatches(i.Key); len(got) != 0 {
		t.Errorf("mismatches: %+v", got)
	}
	var linked int
	if err := a.s.db.R.QueryRow(`SELECT count(*) FROM test_evidence WHERE issue_id IS NOT NULL`).Scan(&linked); err != nil || linked != 0 {
		t.Errorf("linked %d %v", linked, err)
	}
}

func TestWorkLogEndsRound(t *testing.T) {
	ev := func(newState, state string) *Event {
		return &Event{Kind: EvIssueWorkLog, New: newState, Data: map[string]any{"state": state}}
	}
	for _, e := range []*Event{ev(RoundPending, ""), ev(RoundRunning, ""), ev("", ""), ev("x", "counted"), ev("x", "note"), ev(WorkTested, WorkTested), ev(WorkMismatch, WorkMismatch)} {
		if workLogEndsRound(e) {
			t.Errorf("ends a round: %+v", e)
		}
	}
	if !workLogEndsRound(ev("x", "done")) {
		t.Error("a finished round doesn't end one")
	}
}

// A pull request shows the record of its own repo's project, not another
// project's record for the same name and commit.
func TestPREvidenceIsItsOwnProjects(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	a.linkPR(i.ID, 7, "abc1234")
	var forge, pr int64
	if err := a.s.db.R.QueryRow(`SELECT forge_id, id FROM pull_requests`).Scan(&forge, &pr); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 1)`, a.p.ID, forge); err != nil {
		t.Fatal(err)
	}
	a.evidence(i.Key, "abc1234", "", TestRun{At: 1, Command: "mine", Outcome: EvidencePass, Passed: 1})
	a.tick(time.Minute)
	other := a.project(a.admin, "Other", "")
	if _, err := a.s.RecordTestEvidence(a.ctx, a.admin, other.Key, TestEvidenceInput{Repo: "acme/app", SHA: "abc1234", Tree: "t",
		Runs: []TestRun{{At: 2, Command: "theirs", Outcome: EvidenceFail, Failed: 1}}}); err != nil {
		t.Fatal(err)
	}
	d, err := a.s.GetPullRequest(a.ctx, a.admin, pr)
	if err != nil || !strings.Contains(d.Tested, "mine") {
		t.Fatalf("tested: %v %v", d, err)
	}
}

func TestBenchEvidence(t *testing.T) {
	a := newAPEnv(t)
	i := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Task", Agent: "claude"})
	pf := func(v float64) *float64 { return &v }
	noise := BenchRun{At: 1, Tree: "treeb1", Command: "go test -run TestSearch ./internal/core", Dir: "web", Runs: 5, Wall: 1.02, CPU: 2, BaseRef: "origin/main", BaseWall: 1, BaseCPU: 2, WallP: pf(0.6), CPUP: pf(0.9)}
	rec := func(sha, summary string, b ...BenchRun) *TestEvidence {
		t.Helper()
		e, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: sha, Tree: "tree" + sha, Issue: i.Key, Summary: summary, Bench: b})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	e := rec("b1", "Search is 30% faster now.", noise)
	if len(e.BenchLines) != 1 || !strings.Contains(e.BenchLines[0], "no clear change from origin/main (wall +2%, within noise)") || !strings.Contains(e.BenchLines[0], "(in web)") {
		t.Fatalf("lines: %q", e.BenchLines)
	}
	if len(e.Notices) != 1 || !strings.Contains(e.Notices[0], "wp bench at b1") {
		t.Fatalf("notices: %q", e.Notices)
	}
	if cs := a.mismatches(i.Key); len(cs) != 1 || cs[0].Tag != "faster" {
		t.Fatalf("mismatches: %+v", cs)
	}

	fast := noise
	fast.Wall, fast.WallP = 0.7, pf(0.008)
	if e := rec("b2", "Now 30% faster.", fast); len(e.Notices) != 0 || !strings.Contains(e.BenchLines[0], "30% faster than origin/main by wall (p=0.008)") {
		t.Errorf("supported: %q %q", e.Notices, e.BenchLines)
	}
	if e := rec("b3", "It is quicker."); len(e.Notices) != 0 || len(e.BenchLines) != 0 {
		t.Errorf("unmeasured: %+v", e)
	}
	few := noise
	few.Runs, few.Wall, few.WallP, few.CPUP = 3, 0.1, pf(0.0809), pf(0.0809)
	if e := rec("b5", "Search is 10x faster.", few); len(e.Notices) != 0 || !strings.Contains(e.BenchLines[0], "wall -90% against origin/main, too few runs to tell a change from noise (needs 4 a side)") {
		t.Errorf("few runs: %q %q", e.Notices, e.BenchLines)
	}

	rec("b4", "Done.", fast)
	wl, err := a.s.GetWorkLog(a.ctx, a.admin, i.Key)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range wl.Entries {
		found = found || (w.Kind == WorkTested && strings.Contains(w.Body, "Timed on this tree: go test -run TestSearch"))
	}
	if !found {
		t.Error("no bench line in the work log")
	}
	if d, err := a.s.GetIssue(a.ctx, a.admin, i.Key); err != nil || d.Tested == nil || len(d.Tested.BenchLines) != 1 {
		t.Errorf("issue: %+v %v", d.Tested, err)
	}

	for _, bad := range []BenchRun{{Runs: 0}, {Runs: 1, Wall: -1}, {Runs: 1, WallP: pf(2)}} {
		if _, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: "1", Tree: "2", Bench: []BenchRun{bad}}); err == nil {
			t.Errorf("taken: %+v", bad)
		}
	}
}

// wp ship says no test ran on the tree only where CI runs none either
// (WP-1080): the answer says whether the repo's CI ran tests lately.
func TestEvidenceSaysWhetherCIRunsTests(t *testing.T) {
	a := newAPEnv(t)
	rec := func(sha string, runs ...TestRun) *TestEvidence {
		t.Helper()
		e, err := a.s.RecordTestEvidence(a.ctx, a.ag, a.p.Key, TestEvidenceInput{Repo: "acme/app", SHA: sha, Tree: "tree" + sha, Runs: runs})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.s.db.W.ExecContext(a.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if rec("c1").CITests {
		t.Error("no CI history, yet CI runs tests")
	}
	res, err := a.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	forge, _ := res.LastInsertId()
	exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, a.p.ID, forge)
	now := a.clockNow().UnixMilli()
	job := func(task int64, repo, workflow, name, status string, at int64) {
		exec(`INSERT INTO ci_jobs(forge_id, repo, task_id, workflow, name, status, started_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, forge, repo, task, workflow, name, status, at, at+1)
	}
	old := now - int64(2*ciTestsLookback/time.Millisecond)
	job(1, "acme/app", "ci.yml", "lint", "success", now)
	job(2, "acme/app", "ci.yml", "pytest", "cancelled", now)
	job(3, "acme/app", "ci.yml", "unit tests", "success", old)
	job(4, "acme/other", "test.yml", "build", "success", now)
	job(6, "acme/app", "ci.yml", "latest", "success", now)
	job(7, "acme/app", "ci.yml", "build-attestation", "success", now)
	job(10, "acme/app", "majestic.yml", "build", "success", now)
	job(11, "acme/app", "ci.yml", "subjest", "success", now)
	if rec("c2").CITests {
		t.Error("CI runs tests on what no job shows")
	}
	res, err = a.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Other','https://other.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	exec(`INSERT INTO ci_jobs(forge_id, repo, task_id, workflow, name, status, started_at, ended_at) VALUES (?, 'acme/app', 50, 'ci.yml', 'unit tests', 'success', ?, ?)`, other, now, now+1)
	if rec("c2b").CITests {
		t.Error("a test job on a forge the project doesn't use counted")
	}
	job(8, "acme/app", "ci.yml", "unit tests", "success", now)
	if !rec("c2c").CITests {
		t.Error("a test job named for tests didn't count")
	}
	exec(`DELETE FROM ci_jobs WHERE task_id = 8`)
	job(12, "acme/app", "ci.yml", "jest", "success", now)
	if !rec("c2d").CITests {
		t.Error("a jest job didn't count")
	}
	exec(`DELETE FROM ci_jobs WHERE task_id = 12`)
	job(5, "Acme/App", "test.yml", "build", "failure", now)
	if !rec("c3").CITests {
		t.Error("a test workflow's job didn't count")
	}
	if rec("c4", TestRun{Command: "go test ./...", Outcome: EvidencePass, Passed: 1, Tree: "treec4"}).CITests {
		t.Error("said where tests ran on the tree: nothing to leave out")
	}
	exec(`DELETE FROM ci_jobs`)
	exec(`INSERT INTO ci_job_packages(forge_id, repo, task_id, package, outcome, ms, at) VALUES (?, 'acme/app', 9, 'internal/core', 'ok', 1, ?)`, forge, old)
	if rec("c4b").CITests {
		t.Error("go test packages from long ago counted")
	}
	exec(`UPDATE ci_job_packages SET at = ?`, now)
	if !rec("c5").CITests {
		t.Error("go test packages from CI's logs didn't count")
	}
	exec(`DELETE FROM ci_job_packages`)
	exec(`INSERT INTO ci_tests(id, forge_id, repo, package, name) VALUES (1, ?, 'acme/app', 'p', 'TestX')`, forge)
	exec(`INSERT INTO ci_test_runs(test_id, task_id, sha, outcome, ms, at) VALUES (1, 9, 'abc', 'pass', 1, ?)`, old)
	if rec("c5b").CITests {
		t.Error("test results from long ago counted")
	}
	exec(`UPDATE ci_test_runs SET at = ?`, now)
	if !rec("c6").CITests {
		t.Error("test results CI printed didn't count")
	}
}
