package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const mistakeDay = int64(24 * 3600 * 1000)

func bugAt(id, pr string, day int64, title string, files ...string) *mistake {
	return &mistake{source: "bug", id: id, unit: "pr:" + pr, at: day * mistakeDay, keys: files, words: mistakeWords(title), text: title}
}

func groupKeys(p mistakePlan) []string {
	out := []string{}
	for _, g := range p.fresh {
		out = append(out, fmt.Sprintf("%s:%d", g.key, len(g.members)))
	}
	return out
}

func TestPlanMistakesNeedsThreeIndependentAlike(t *testing.T) {
	const file = "internal/core/babysit.go"
	alike := []*mistake{
		bugAt("1", "1", 1, "merge queue front loses its place", file),
		bugAt("2", "2", 5, "merge queue front stuck after a rebase", file),
		bugAt("3", "3", 9, "front of the merge queue is skipped", file),
	}
	cases := []struct {
		name  string
		items []*mistake
		want  []string
	}{
		{"three alike", alike, []string{file + ":3"}},
		{"two are not enough", alike[:2], []string{}},
		{"one pull request is one unit", []*mistake{alike[0], alike[1], bugAt("3", "2", 9, "front of the merge queue is skipped", file)}, []string{}},
		{"too far apart", []*mistake{alike[0], alike[1], bugAt("3", "3", 20, "front of the merge queue is skipped", file)}, []string{}},
		{"same file, unrelated words", []*mistake{alike[0], alike[1], bugAt("3", "3", 9, "assessor opus prompt truncated", file)}, []string{}},
		{"different files", []*mistake{alike[0], alike[1], bugAt("3", "3", 9, "front of the merge queue is skipped", "other.go")}, []string{}},
		{"each like the first, not each other", []*mistake{
			bugAt("1", "1", 1, "session token refresh races the login handler", file),
			bugAt("2", "2", 5, "session token missing on login page", file),
			bugAt("3", "3", 9, "refresh races the handler under load", file),
		}, []string{}},
	}
	for _, c := range cases {
		if got := groupKeys(planMistakes(c.items, nil)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// One set of bugs across four files is one class, not four.
func TestPlanMistakesOneClassPerInstance(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go", "d.go"}
	p := planMistakes([]*mistake{
		bugAt("1", "1", 1, "draft run loses its branch", files...),
		bugAt("2", "2", 2, "draft run branch deleted early", files...),
		bugAt("3", "3", 3, "draft run ignores the branch", files...),
	}, nil)
	if len(p.fresh) != 1 || len(p.fresh[0].members) != 3 {
		t.Fatalf("one class of three: %v", groupKeys(p))
	}
}

// Evidence that matches a class filed before is counted there, and doesn't
// start another.
func TestPlanMistakesAttachesToClass(t *testing.T) {
	c := &mistakeClassRow{id: 7, key: "a.go", words: mistakeWords("merge queue front loses its place")}
	in := bugAt("9", "9", 40, "merge queue front lost again", "a.go")
	other := bugAt("10", "10", 40, "assessor prompt truncated", "a.go")
	p := planMistakes([]*mistake{in, other}, []*mistakeClassRow{c})
	if !slices.Equal(p.attach[7], []*mistake{in}) || len(p.fresh) != 0 {
		t.Fatalf("attach %v fresh %v", p.attach, groupKeys(p))
	}
}

// A signature is the class; its instances need no words in common.
func TestPlanMistakesSignature(t *testing.T) {
	var items []*mistake
	for i := range 3 {
		id := fmt.Sprint(i)
		items = append(items, &mistake{source: "flake", id: id, unit: "issue:" + id, at: int64(i) * mistakeDay, keys: []string{"ci:data-race"}})
	}
	if got := groupKeys(planMistakes(items, nil)); !slices.Equal(got, []string{"ci:data-race:3"}) {
		t.Fatalf("%v", got)
	}
}

// mistakeBug files a bug fixed by a merged pull request on files.
func (pe *prEnv) mistakeBug(number int, title string, files ...string) *IssueDetail {
	pe.t.Helper()
	bug := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Type: "bug", Title: title})
	pe.pullHook("opened", ForgePR{Number: number, Title: title, HeadRef: fmt.Sprintf("b%d", number), HeadSHA: fmt.Sprintf("%040d", number), Author: "someone"})
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = `"` + f + `"`
	}
	url := fmt.Sprintf("https://git.example.com/me/app/pulls/%d", number)
	pe.mexec(`UPDATE pull_requests SET state = 'merged', merged_at = ?, files = ?, files_sha = head_sha WHERE number = ?`,
		pe.s.nowMS(), `{"paths":[`+strings.Join(quoted, ",")+`]}`, number)
	pe.mexec(`INSERT OR IGNORE INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'me/app', ?, 'merged', 0)`, bug.ID, url, number)
	return bug
}

func (pe *prEnv) mexec(q string, args ...any) int64 {
	pe.t.Helper()
	res, err := pe.s.db.W.ExecContext(pe.ctx, q, args...)
	if err != nil {
		pe.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (pe *prEnv) recurring() []*IssueDetail {
	pe.t.Helper()
	rows, err := pe.s.db.R.QueryContext(pe.ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.title LIKE 'Recurring mistake%' ORDER BY i.id`)
	if err != nil {
		pe.t.Fatal(err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			pe.t.Fatal(err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	var out []*IssueDetail
	for _, k := range keys {
		out = append(out, pe.get(pe.admin, k))
	}
	return out
}

func (pe *prEnv) mistakeClasses() []*MistakeClass {
	pe.t.Helper()
	list, err := pe.s.MistakeClasses(pe.ctx, pe.admin, pe.p.Key)
	if err != nil {
		pe.t.Fatal(err)
	}
	return list
}

// Three alike bugs file one task with the instances in it; another sweep
// files nothing more; what comes after the task is done counts as recurring.
func TestSweepFilesOnceAndMeasuresRecurrence(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	const file = "internal/core/queue.go"
	first := pe.mistakeBug(1, "merge queue front loses its place", file, "internal/core/queue_test.go")
	pe.mistakeBug(2, "merge queue front stuck after rebase", file)
	pe.s.SweepMistakes(pe.ctx)
	if got := pe.recurring(); len(got) != 0 {
		t.Fatalf("two bugs filed %v", got)
	}
	pe.mistakeBug(3, "front of the merge queue skipped", file)
	pe.s.SweepMistakes(pe.ctx)
	pe.s.SweepMistakes(pe.ctx)
	got := pe.recurring()
	if len(got) != 1 {
		t.Fatalf("one task, got %d", len(got))
	}
	task := got[0]
	if !strings.Contains(task.Title, "merge queue") || !strings.Contains(task.Description, file) || !strings.Contains(task.Description, first.Key) || !strings.Contains(task.Description, "came up 3 times") || !strings.Contains(task.Description, "Won't Do") {
		t.Fatalf("the task: %q\n%s", task.Title, task.Description)
	}
	if PriorityName(task.Priority) != "low" {
		t.Fatalf("behind planned work: %s", PriorityName(task.Priority))
	}
	if task.Agent != "claude" || !slices.ContainsFunc(task.Labels, func(l Label) bool { return l.Name == "recurring" }) {
		t.Fatalf("for an agent, labelled: %+v", task)
	}
	list := pe.mistakeClasses()
	if len(list) != 1 || list[0].State != MistakeOpen || list[0].Instances != 3 || list[0].Recurred != 0 || list[0].Issue != task.Key {
		t.Fatalf("open: %+v", list[0])
	}

	done := "Done"
	if _, err := pe.s.UpdateIssue(pe.ctx, pe.admin, task.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	pe.setClock(pe.clockNow().Add(time.Hour))
	pe.mistakeBug(4, "merge queue front lost again", file)
	pe.s.SweepMistakes(pe.ctx)
	list = pe.mistakeClasses()
	if len(pe.recurring()) != 1 || list[0].State != MistakeLanded || list[0].Instances != 4 || list[0].Recurred != 1 || list[0].LandedAt == nil {
		t.Fatalf("landed, one since: %+v", list[0])
	}
}

// High-churn open mistake classes sort ahead of smaller/closed classes and
// escalate priority from low -> medium (>=10) -> high (>=20) without ever
// downgrading a human-set higher priority.
func TestMistakeOrderingAndPriorityEscalation(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	const fileA, fileB = "internal/core/hot.go", "internal/core/warm.go"
	for i := 1; i <= 3; i++ {
		pe.mistakeBug(i, fmt.Sprintf("merge queue front loses place %d", i), fileA)
		pe.mistakeBug(100+i, fmt.Sprintf("draft run loses branch %d", i), fileB)
	}
	pe.s.SweepMistakes(pe.ctx)
	got := pe.recurring()
	if len(got) != 2 {
		t.Fatalf("expected 2 recurring tasks, got %d", len(got))
	}
	// Add 7 more instances to fileA (total 10) -> escalates to medium and sorts first.
	for i := 4; i <= 10; i++ {
		pe.mistakeBug(i, fmt.Sprintf("merge queue front loses place %d", i), fileA)
	}
	pe.s.SweepMistakes(pe.ctx)
	list := pe.mistakeClasses()
	if len(list) != 2 || list[0].Key != fileA || list[0].Instances != 10 {
		t.Fatalf("expected %s (10 instances) sorted first, got %+v", fileA, list)
	}
	if prio := PriorityName(pe.get(pe.admin, list[0].Issue).Priority); prio != "medium" {
		t.Fatalf("at 10 instances priority = %q, want medium", prio)
	}
	// Add 10 more instances to fileA (total 20) -> escalates to high.
	for i := 11; i <= 20; i++ {
		pe.mistakeBug(i, fmt.Sprintf("merge queue front loses place %d", i), fileA)
	}
	pe.s.SweepMistakes(pe.ctx)
	if prio := PriorityName(pe.get(pe.admin, list[0].Issue).Priority); prio != "high" {
		t.Fatalf("at 20 instances priority = %q, want high", prio)
	}
	// Human raising priority to urgent must never be downgraded by subsequent sweeps.
	if _, err := pe.s.UpdateIssue(pe.ctx, pe.admin, list[0].Issue, IssuePatch{Priority: "urgent"}); err != nil {
		t.Fatal(err)
	}
	pe.mistakeBug(21, "merge queue front loses place 21", fileA)
	pe.s.SweepMistakes(pe.ctx)
	if prio := PriorityName(pe.get(pe.admin, list[0].Issue).Priority); prio != "urgent" {
		t.Fatalf("after human raised to urgent, priority = %q, want urgent", prio)
	}
}

// Won't Do declines a class for good, and the setting stops filing.
func TestSweepDeclinedAndTurnedOff(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	for i, title := range []string{"draft run loses branch", "draft run branch deleted", "draft run ignores branch"} {
		pe.mistakeBug(i+1, title, "draft.go")
	}
	pe.mexec(`UPDATE projects SET settings = json_set(settings, '$.no_recurring_checks', json('true')) WHERE id = ?`, pe.p.ID)
	pe.s.SweepMistakes(pe.ctx)
	if got := pe.recurring(); len(got) != 0 {
		t.Fatalf("turned off, filed %v", got)
	}
	pe.mexec(`UPDATE projects SET settings = json_remove(settings, '$.no_recurring_checks') WHERE id = ?`, pe.p.ID)
	pe.s.SweepMistakes(pe.ctx)
	got := pe.recurring()
	if len(got) != 1 {
		t.Fatalf("filed %d", len(got))
	}
	wont := "Won't Do"
	if _, err := pe.s.UpdateIssue(pe.ctx, pe.admin, got[0].Key, IssuePatch{Status: &wont}); err != nil {
		t.Fatal(err)
	}
	pe.mistakeBug(9, "draft run branch lost again", "draft.go")
	pe.mistakeBug(10, "draft run branch gone twice", "draft.go")
	pe.mistakeBug(11, "draft run branch missing thrice", "draft.go")
	pe.s.SweepMistakes(pe.ctx)
	list := pe.mistakeClasses()
	if len(pe.recurring()) != 1 || len(list) != 1 || list[0].State != MistakeDeclined || list[0].Instances != 6 {
		t.Fatalf("declined stays one class, still counted: %+v", list[0])
	}
}

// One project gets at most two tasks a day; the rest wait for a later sweep.
func TestSweepCapsTasksPerDay(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	n := 0
	for _, file := range []string{"a.go", "b.go", "c.go"} {
		for _, title := range []string{"draft run loses branch", "draft run branch deleted", "draft run ignores branch"} {
			n++
			pe.mistakeBug(n, title, file)
		}
	}
	pe.s.SweepMistakes(pe.ctx)
	if got := len(pe.recurring()); got != 2 {
		t.Fatalf("filed %d in a day", got)
	}
	pe.setClock(pe.clockNow().Add(25 * time.Hour))
	pe.s.SweepMistakes(pe.ctx)
	if got := len(pe.recurring()); got != 3 {
		t.Fatalf("the rest a day later: %d", got)
	}
}

// A flake whose log shows a data race is counted as a race; others stay flakes.
func TestFlakeMistakesRaceIsItsOwnSignature(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	file := func(title, desc string) {
		pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Type: "bug", Title: title, Description: desc, Labels: []string{"flaky"}})
	}
	file("Flaky in CI: TestA", "WARNING: DATA RACE\nread at")
	file("Flaky in CI: TestB", "timeout")
	commented := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Type: "bug", Title: "Flaky in CI: TestC", Description: "timeout", Labels: []string{"flaky"}})
	if _, err := pe.s.AddComment(pe.ctx, pe.admin, commented.Key, "again:\nWARNING: DATA RACE"); err != nil {
		t.Fatal(err)
	}
	file("Flaky in CI: e2e/board.spec.ts", "timed out")
	denied := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Type: "bug", Title: "Flaky in CI: TestD", Description: "timeout", Labels: []string{"flaky"}})
	if _, err := pe.s.AddComment(pe.ctx, pe.admin, denied.Key, "looked at this: not a data race"); err != nil {
		t.Fatal(err)
	}
	file("Flaky in CI: test_login", "timed out")
	file("Flaky in CI: tests/test_a.py::test_b", "timed out")
	file("Flaky in CI: ci / backend", "timed out")
	list, err := pe.s.flakeMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, m := range list {
		keys = append(keys, m.keys...)
	}
	want := []string{"ci:data-race", "ci:flake:TestB", "ci:data-race", "ci:flake:e2e/board.spec.ts", "ci:flake:TestD", "ci:flake:test_login", "ci:flake:tests/test_a.py::test_b"}
	if !slices.Equal(keys, want) {
		t.Fatalf("%v, want %v", keys, want)
	}
}

// A test left red over several commits went red once.
func TestTestMistakesCountsEpisodes(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	id := pe.mexec(`INSERT INTO ci_tests(forge_id, repo, package, name) VALUES (?, 'me/app', 'internal/x', 'TestY')`, pe.f.ID)
	now := pe.s.nowMS()
	for i, outcome := range []string{"pass", "fail", "fail", "fail", "pass", "fail", "pass"} {
		pe.mexec(`INSERT INTO ci_test_runs(test_id, task_id, sha, on_default, outcome, ms, at) VALUES (?, ?, ?, 1, ?, 5, ?)`, id, i, fmt.Sprintf("sha%d", i), outcome, now+int64(i))
	}
	list, err := pe.s.testMistakes(context.Background(), pe.p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].id != fmt.Sprintf("%d:%d:sha1", pe.p.ID, id) || list[1].id != fmt.Sprintf("%d:%d:sha5", pe.p.ID, id) || list[0].keys[0] != "test:internal/x.TestY" {
		t.Fatalf("%+v", list)
	}
}

// Findings count when they held up and aren't on tests; audits when confirmed.
func TestFindingAndAuditMistakes(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.pullHook("opened", ForgePR{Number: 4, Title: "Export", HeadRef: "export", HeadSHA: "abc1234def", Author: "someone"})
	var prID int64
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT id FROM pull_requests WHERE number = 4`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	now := pe.s.nowMS()
	run := pe.mexec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, ?, 'review', 'abc', 'done', ?)`, pe.p.ID, prID, now)
	finding := func(path, state string, ord int) {
		pe.mexec(`INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, state, placement, ordinal, created_at, updated_at) VALUES (?, ?, ?, 1, 'high', 'nil map write panics', 'm', ?, 'inline', ?, ?, ?)`, run, prID, path, state, ord, now, now)
	}
	finding("x.go", FindingFixed, 1)
	finding("x_test.go", FindingFixed, 2)
	finding("y.go", FindingDismissed, 3)
	found, err := pe.s.findingMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].keys[0] != "x.go" || !strings.Contains(found[0].text, "nil map write") {
		t.Fatalf("findings: %+v", found)
	}
	audit := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at) VALUES (?, ?, 'me/app', 'repo', ?, ?)`, pe.p.ID, pe.f.ID, pe.admin.User.ID, now)
	for i, state := range []string{"confirmed", "fixed", "rejected", "open"} {
		pe.mexec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, state) VALUES (?, ?, 'stale cursor', 'z.go', ?)`, audit, i+1, state)
	}
	audits, err := pe.s.auditMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 || audits[0].unit != "audit:"+itoa64(audit) {
		t.Fatalf("audits: %+v", audits)
	}

	unitOf := func(audit int64) string {
		t.Helper()
		pe.mexec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, state) VALUES (?, 1, 'stale cursor', 'z.go', 'fixed')`, audit)
		list, err := pe.s.auditMistakes(pe.ctx, pe.p.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		return list[len(list)-1].unit
	}
	onPR := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, owner_id, created_at) VALUES (?, ?, 'me/app', 'pr', ?, ?, ?)`, pe.p.ID, pe.f.ID, prID, pe.admin.User.ID, now)
	if got := unitOf(onPR); got != "pr:"+itoa64(prID) {
		t.Errorf("pull request audit: %s", got)
	}
	onBranch := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, ref, owner_id, created_at) VALUES (?, ?, 'ME/APP', 'branch', 'export', ?, ?)`, pe.p.ID, pe.f.ID, pe.admin.User.ID, now)
	if got := unitOf(onBranch); got != "pr:"+itoa64(prID) {
		t.Errorf("its branch: %s", got)
	}
	noPR := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, ref, owner_id, created_at) VALUES (?, ?, 'me/app', 'branch', 'never-opened', ?, ?)`, pe.p.ID, pe.f.ID, pe.admin.User.ID, now)
	if got := unitOf(noPR); got != "audit:"+itoa64(noPR) {
		t.Errorf("a branch with no pull request: %s", got)
	}
}

// The "nobody judged these" audit of a pull request repeats its review.
func TestDropFindingCopies(t *testing.T) {
	item := func(source, id, unit, file, text string) *mistake {
		return &mistake{source: source, id: id, unit: unit, keys: []string{file}, words: mistakeWords(text)}
	}
	const said = "client add silently replaces the entry and its token hash"
	items := []*mistake{
		item("finding", "1", "pr:10", "main.go", said),
		item("audit", "copy", "pr:10", "main.go", said),
		item("audit", "same PR, other claim", "pr:10", "main.go", "webhook bearer stays after an empty flag value"),
		item("audit", "same words, other PR", "pr:11", "main.go", said),
		item("audit", "same words, other file", "pr:10", "other.go", said),
		{source: "flake", id: "9", unit: "issue:9"},
	}
	var left []string
	for _, m := range dropFindingCopies(items) {
		left = append(left, m.id)
	}
	if want := []string{"1", "same PR, other claim", "same words, other PR", "same words, other file", "9"}; !slices.Equal(left, want) {
		t.Fatalf("%v, want %v", left, want)
	}
}

// A flake's class comes from the first check that failed for itself.
func TestFlakeCause(t *testing.T) {
	section := func(name, tests, log string) string {
		s := "**" + name + "** (/r/actions/runs/1/jobs/1)"
		if tests != "" {
			s += ": " + tests
		}
		s += "\n"
		if log != "" {
			s += "\n````text\n" + log + "\n````\n"
		}
		return s + "\n"
	}
	cancelled := section("CI / images (pull_request)", "", "[runner]: context canceled")
	report := section("CI / deploy (pull_request)", "", "")
	cases := []struct {
		name, title, description, want string
	}{
		{"cancelled and report-only jobs are skipped", "Flaky in CI: CI / deploy, CI / images and CI / go",
			report + cancelled + section("CI / go (pull_request)", "TestA, TestB and TestC", "--- FAIL: TestA"), flakeKeyPrefix + "TestA"},
		{"a log with no test names", "Flaky in CI: CI / a and CI / lint", cancelled + section("CI / lint (pull_request)", "", "ok  fine\nmain.go:1: compile: signal: killed\nmake: *** [lint] Error 1"),
			flakeKeyPrefix + "CI / lint: " + strings.Join(strings.Fields(gistOf("compile: signal: killed")), " ")},
		{"everything cancelled", "Flaky in CI: CI / a and CI / images", cancelled, ""},
		{"only report jobs", "Flaky in CI: CI / deploy", report, ""},
		{"a check with no log, named like a test", "Flaky in CI: codecov/patch", section("codecov/patch", "", ""), ""},
		{"no sighting, a test in the title", "Flaky in CI: TestB", "timeout", flakeKeyPrefix + "TestB"},
		{"no sighting, a job in the title", "Flaky in CI: ci / backend", "timed out", ""},
	}
	for _, c := range cases {
		if got, _ := flakeCause(c.title, c.description); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// One cause keeps one key whatever file, version or temporary directory the
// run was on, and two causes on the same path don't share one.
func TestFlakeCauseKeysTheFailureNotThePath(t *testing.T) {
	key := func(line string) string {
		k, _ := flakeCause("Flaky in CI: CI / go", "**CI / go (pull_request)** (/r/1)\n\n````text\n"+line+"\n````\n")
		return k
	}
	const compiler = "/opt/hostedtoolcache/go/1.27.3/x64/pkg/tool/linux_amd64/compile"
	same := [][2]string{
		{"cmd/juno/main.go:16:2: could not import git.example.net/robin/juno/internal/api (-: " + compiler + ": signal: killed) (typecheck)",
			"internal/db/x.go:3:9: could not import git.example.net/robin/juno/internal/core (-: " + compiler + ": signal: killed) (typecheck)"},
		{"dial unix /tmp/TestFoo3482/001/sock: connect failed", "dial unix /tmp/TestFoo9911/001/sock: connect failed"},
		{"panic at 0xc000123456: bad state", "panic at 0xc000999999: bad state"},
	}
	for _, c := range same {
		if a, b := key(c[0]), key(c[1]); a == "" || a != b {
			t.Errorf("one cause, two keys: %q and %q", a, b)
		}
	}
	const lint = "/home/runner/go/pkg/mod/github.com/golangci/golangci-lint/v2@v2.15.0/pkg/golinters/"
	if a, b := key(lint+"interfacebloat/interfacebloat.go:4:2: Get https://proxy.golang.org/x: i/o timeout"), key(lint+"nilnil/nilnil.go:9:1: undefined: foo (typecheck) error"); a == b {
		t.Errorf("two causes share %q", a)
	}
	if k := key("cmd/juno/main.go:16:2: could not import x/y (-: " + compiler + ": signal: killed) (typecheck)"); !strings.Contains(k, "killed") {
		t.Errorf("the key lost the cause: %q", k)
	}
	if _, name := flakeCause("Flaky in CI: CI / go", "**CI / go (pull_request)** (/r/1)\n\n````text\ncmd/a.go:1:1: compile: signal: killed\n````\n"); name != "CI / go keeps failing: compile: signal: killed" {
		t.Errorf("name %q", name)
	}
}

// A test named in free text mustn't make a title the title model rewrites.
func TestFreeTextTestNameStaysOneSentence(t *testing.T) {
	const test = "the verifier checked a very long list of rows and got 3. Then it stopped"
	key, name := flakeNamed(test)
	if key != flakeKeyPrefix+test {
		t.Errorf("key %q lost the name", key)
	}
	if title := mistakeTitle(name); longForTitle(title) {
		t.Errorf("title goes to the model: %q", title)
	}
	if title := mistakeTitle(mistakeLabel(&mistakeGroup{key: "test:" + test})); longForTitle(title) {
		t.Errorf("title goes to the model: %q", title)
	}
}

// Seen again before its task closed is the same instance; after, a new one.
func TestJoinOpenFlakes(t *testing.T) {
	flake := func(id string, at, closed int64, key string) *mistake {
		m := &mistake{source: "flake", id: id, unit: "issue:" + id, at: at, closed: closed}
		if key != "" {
			m.keys = []string{flakeKeyPrefix + key}
		}
		return m
	}
	list := []*mistake{
		flake("1", 1, 10, "oom"),
		flake("2", 5, 20, "oom"),
		flake("3", 15, 25, "oom"),
		flake("4", 30, 0, "oom"),
		flake("5", 40, 50, "oom"),
		flake("6", 2, 3, "other"),
		flake("7", 2, 3, ""),
	}
	joinOpenFlakes(list)
	var units []string
	for _, m := range list {
		units = append(units, m.unit)
	}
	want := []string{"issue:1", "issue:1", "issue:1", "issue:4", "issue:4", "issue:6", "issue:7"}
	if !slices.Equal(units, want) {
		t.Fatalf("%v, want %v", units, want)
	}
}

// A class is titled with what one of its instances says went wrong.
func TestMistakeLabelSaysWhatWentWrong(t *testing.T) {
	named := func(source, id, name string) *mistake {
		return &mistake{source: source, id: id, unit: id, keys: []string{"api.py"}, name: name, words: mistakeWords(name)}
	}
	long := strings.Repeat("removed skill wording ", 10)
	g := &mistakeGroup{key: "api.py", members: []*mistake{
		named("finding", "1", "Removing a skill keeps its saved years under another wording of the skill"),
		named("audit", "2", "Removing one wording of a skill deletes years a skill still uses"),
		named("audit", "3", long),
		named("audit", "4", "Unrelated heading"),
	}}
	if got := mistakeLabel(g); got != "Removing one wording of a skill deletes years a skill still uses" {
		t.Fatalf("label: %q", got)
	}
	if got := mistakeLabel(&mistakeGroup{key: "ci:other", members: []*mistake{{source: "flake"}}}); got != "ci:other" {
		t.Fatalf("nothing to name it with: %q", got)
	}
	if got := mistakeSentence("It failed.  The next  one too."); got != "It failed" {
		t.Fatalf("sentence: %q", got)
	}
}

// A longer title would be rewritten by the title model, and an interrupted
// sweep finds its task by title.
func TestMistakeTitleStaysAsFiled(t *testing.T) {
	long := "`internal/some/quite/long/package.TestAVeryLongNameThatGoesOnAndOn` keeps going red on the default branch"
	if title := mistakeTitle(long); longForTitle(title) {
		t.Fatalf("%d runes: %q", len([]rune(title)), title)
	}
}

// A repo on two projects: each counts the same failures as its own.
func TestSweepTestFailuresInEveryProjectOfARepo(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	other := pe.project(pe.admin, "Other", "")
	pe.mexec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'me/app', 0)`, other.ID, pe.f.ID)
	id := pe.mexec(`INSERT INTO ci_tests(forge_id, repo, package, name) VALUES (?, 'me/app', 'x', 'TestY')`, pe.f.ID)
	now := pe.s.nowMS()
	for i, outcome := range []string{"fail", "pass", "fail", "pass", "fail"} {
		pe.mexec(`INSERT INTO ci_test_runs(test_id, task_id, sha, on_default, outcome, ms, at) VALUES (?, ?, ?, 1, ?, 5, ?)`, id, i, fmt.Sprintf("sha%d", i), outcome, now+int64(i))
	}
	pe.s.SweepMistakes(pe.ctx)
	for _, p := range []*Project{pe.p, other} {
		list, err := pe.s.MistakeClasses(pe.ctx, pe.admin, p.Key)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Instances != 3 {
			t.Fatalf("%s: %+v", p.Key, list)
		}
	}
}

// The same repo and number on a forge this project isn't on is another change.
func TestBugMistakesOnlyThisProjectsForge(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.mistakeBug(1, "draft run loses branch", "draft.go")
	other := pe.forge("forgejo", "https://other.example.com", false)
	pe.mexec(`INSERT INTO pull_requests(forge_id, repo, number, title, author, url, state, files, files_sha, head_sha, base_ref, head_ref, created_at, updated_at, opened_at)
		SELECT ?, repo, number, title, author, 'https://other.example.com/me/app/pulls/1', 'merged', '{"paths":["elsewhere.go"]}', 'x', 'x', base_ref, head_ref, created_at, updated_at, opened_at FROM pull_requests WHERE number = 1`, other.ID)
	list, err := pe.s.bugMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !slices.Equal(list[0].keys, []string{"draft.go"}) {
		t.Fatalf("%+v", list)
	}
}

// A fix nothing has read the files of is read by the sweep.
func TestSweepReadsAnUnreadFixsFiles(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.mistakeBug(1, "draft run loses branch", "draft.go")
	pe.mexec(`UPDATE pull_requests SET files = '', files_sha = '' WHERE number = 1`)
	before, err := pe.s.bugMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil || len(before) != 0 {
		t.Fatalf("unread: %+v, %v", before, err)
	}
	pe.s.SweepMistakes(pe.ctx)
	after, err := pe.s.bugMistakes(pe.ctx, pe.p.ID, 0)
	if err != nil || len(after) != 1 || len(after[0].keys) == 0 {
		t.Fatalf("read: %+v, %v", after, err)
	}
}

// Turning filing off stops new tasks, not the count for classes already filed.
func TestSweepTurnedOffKeepsCounting(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	for i, title := range []string{"draft run loses branch", "draft run branch deleted", "draft run ignores branch"} {
		pe.mistakeBug(i+1, title, "draft.go")
	}
	pe.s.SweepMistakes(pe.ctx)
	pe.mexec(`UPDATE projects SET settings = json_set(settings, '$.no_recurring_checks', json('true')) WHERE id = ?`, pe.p.ID)
	pe.mistakeBug(4, "draft run branch lost again", "draft.go")
	for i, title := range []string{"assessor prompt truncated", "assessor prompt doubled", "assessor prompt dropped"} {
		pe.mistakeBug(5+i, title, "assess.go")
	}
	pe.s.SweepMistakes(pe.ctx)
	list := pe.mistakeClasses()
	if len(pe.recurring()) != 1 || len(list) != 1 || list[0].Instances != 4 {
		t.Fatalf("%d tasks: %+v", len(pe.recurring()), list)
	}
}
