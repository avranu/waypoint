package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// refuseProtected ends a round's job as the runner does when its agent
// changed a protected file.
func (be *bsEnv) refuseProtected(id int64, files string) {
	be.t.Helper()
	res, _ := json.Marshal(map[string]any{"files": []string{files}, "answer": "Fixed the temp dir leak.", "diff": "--- a/.github/scripts/run.py\n+++ b/.github/scripts/run.py\n+atexit.register(cleanup)\n+subprocess.run(['sudo', 'apt-get', 'install', '-y', 'jq'])\n"})
	msg := "The agent changed protected files (" + files + "), so nothing was pushed."
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "failed", nil, res, msg) }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// protectedTasks are the issues filed to push a refused fix, by key.
func (be *bsEnv) protectedTasks() map[string]string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT p.key || '-' || i.number, i.description FROM issues i JOIN projects p ON p.id = i.project_id WHERE (i.title LIKE 'Push the protected-file fix%' OR i.title LIKE 'Settle the protected-file conflict%')`)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, d string
		_ = rows.Scan(&k, &d)
		out[k] = d
	}
	return out
}

// issueRef is the issue key names, for the screen's reads.
func (be *bsEnv) issueRef(key string) *Issue {
	be.t.Helper()
	dash := strings.LastIndexByte(key, '-')
	var id int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.key = ? AND i.number = ?`, key[:dash], key[dash+1:]).Scan(&id); err != nil {
		be.t.Fatal(err)
	}
	return &Issue{ID: id, Key: key}
}

// A round refused for protected files files a task for a session to push
// the fix, once, names it in the babysit's wait and to its owner, and a
// resume leaves it be (Ops/infrastructure#144: a day in needs_person).
func TestBabysitProtectedFileHandoff(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET armed_at = 1, queued_at = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.refuseProtected(jobs[0], ".github/scripts/run.py")
	b := be.babysit()
	if be.pr(7).QueuedAt != nil {
		t.Error("waiting on a session's push, it keeps the front of its merge line (WP-439)")
	}
	if b.State != BabysitNeedsPerson || len(be.jobs()) != 1 {
		t.Fatalf("after a refused round: %+v, jobs %v", b, be.jobs())
	}
	if r := b.Rounds[0]; !strings.Contains(r.Diff, "atexit.register") {
		t.Errorf("the refused diff wasn't kept: %q", r.Diff)
	}
	tasks := be.protectedTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks %v", tasks)
	}
	var key, body string
	for k, d := range tasks {
		key, body = k, d
	}
	for _, want := range []string{"me/app#7", ".github/scripts/run.py", "git switch 'add-export'", "wp pr todo 7", "wp ship"} {
		if !strings.Contains(body, want) {
			t.Errorf("task body lacks %q:\n%s", want, body)
		}
	}
	// The round's agent words and diff are not on the task (they may edit protected files and run sudo); wp pr todo shows them as untrusted text.
	if strings.Contains(body, "atexit.register") {
		t.Errorf("the refused diff is in the template body:\n%s", body)
	}
	var notes int
	if err := be.s.db.R.QueryRow(`SELECT count(*) FROM comments c JOIN issues i ON i.id = c.issue_id WHERE i.title LIKE 'Push the protected-file fix%'`).Scan(&notes); err != nil || notes != 0 {
		t.Errorf("comments on the handoff: %d %v", notes, err)
	}
	if why, held := be.s.safetyWait(be.ctx, be.s.db.R, be.issueRef(key)); held {
		t.Errorf("the handoff is held: %s", why)
	}
	todo, err := be.s.PRWorkPacket(be.ctx, be.admin, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fixed the temp dir leak.", "+++ b/.github/scripts/run.py\n+atexit.register(cleanup)\n", "<untrusted>"} {
		if !strings.Contains(todo.Text, want) {
			t.Errorf("wp pr todo lacks %q:\n%s", want, todo.Text)
		}
	}
	var stamp string
	if err := be.s.db.R.QueryRow(`SELECT ts.template_hash FROM task_safety ts JOIN issues i ON i.id = ts.issue_id WHERE i.title LIKE 'Push the protected-file fix%'`).Scan(&stamp); err != nil || stamp == "" {
		t.Errorf("handoff not stamped as Waypoint's template: %q %v", stamp, err)
	}
	if !strings.Contains(b.Wait, "protected files (.github/scripts/run.py)") || !strings.Contains(b.Wait, key) {
		t.Errorf("wait %q", b.Wait)
	}
	if m := lastOf(be.told()); !strings.Contains(m, key) || !strings.Contains(m, "branch add-export") {
		t.Errorf("told %q", m)
	}
	var agent string
	if err := be.s.db.R.QueryRow(`SELECT coalesce(agent, '') FROM issues WHERE title LIKE 'Push the protected-file fix%'`).Scan(&agent); err != nil || agent != "claude" {
		t.Errorf("agent %q %v", agent, err)
	}

	be.step()
	if n := len(be.protectedTasks()); n != 1 || len(be.jobs()) != 1 {
		t.Errorf("after another step: %d tasks, jobs %v", n, be.jobs())
	}
	if _, err := be.s.UpdateIssue(be.ctx, be.admin, key, IssuePatch{Status: ptr("Done")}); err != nil {
		t.Fatal(err)
	}
	be.step()
	if n := len(be.protectedTasks()); n != 1 {
		t.Errorf("filed again for the same round: %d", n)
	}

	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET armed_at = ? WHERE id = ?`, be.s.nowMS(), pr.ID); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(7 * time.Hour))
	if _, _, ok := be.s.resumable(be.ctx, pr.ID); ok {
		t.Error("a protected-file refusal on the head is resumable")
	}

	be.push("8888888bbbb")
	if b = be.babysit(); b.State == BabysitNeedsPerson {
		t.Errorf("after a push: %+v", b)
	}
}

func TestProtectedRefusal(t *testing.T) {
	for _, c := range []struct {
		r     BabysitRound
		files string
		ok    bool
	}{
		{BabysitRound{To: "runner", Outcome: "failed: The agent changed protected files (.github/a.py, go.sum), so nothing was pushed."}, ".github/a.py, go.sum", true},
		{BabysitRound{To: "runner", Outcome: "failed: tests failed"}, "", false},
		{BabysitRound{To: "runner", Pushed: true, Outcome: "failed: The agent changed protected files (x)"}, "", false},
		{BabysitRound{To: "session", Outcome: "failed: The agent changed protected files (x)"}, "", false},
	} {
		files, ok := protectedRefusal(c.r)
		if files != c.files || ok != c.ok {
			t.Errorf("%q: %q %v", c.r.Outcome, files, ok)
		}
	}
}

// ciDiff is a pull request's change to its CI workflow.
const ciDiff = "diff --git a/.forgejo/workflows/ci.yml b/.forgejo/workflows/ci.yml\n--- a/.forgejo/workflows/ci.yml\n+++ b/.forgejo/workflows/ci.yml\n@@ -1,3 +1,3 @@\n on: push\n-run: make\n+run: make check\n"

// protectedConflictEnv: pull request 7 on Forgejo changes the CI workflow's
// run line; main changed it to baseCI since they parted, and the forge
// confirms a conflict.
func protectedConflictEnv(t *testing.T, baseCI string) *bsEnv {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	const ci = ".forgejo/workflows/ci.yml"
	be.sg.union = unionFake{mergeBase: "mb", changed: []string{ci, "app/main.go"}}
	be.sg.fileAt["mb:"+ci] = "on: push\nrun: make\njobs: 1\n"
	be.sg.fileAt["7777777aaaa:"+ci] = "on: push\nrun: make check\njobs: 1\n"
	be.sg.fileAt["main:"+ci] = baseCI
	pr := be.open()
	be.sg.mu.Lock()
	be.sg.diffs["7777777aaaa"] = ciDiff
	be.sg.mu.Unlock()
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET files_sha = '' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.forgeSays(7, ptr(false))
	be.fg.mu.Lock()
	be.fg.prs[mk(be.f, "me/app", 7)].BaseRef = "main"
	be.fg.mu.Unlock()
	be.readForge(7)
	be.ci("7777777aaaa", CheckSuccess)
	be.setClock(be.clockNow().Add(31 * time.Second))
	be.s.CheckMergeRechecks(be.ctx)
	be.setClock(be.clockNow().Add(2*time.Minute + time.Second))
	be.s.CheckMergeRechecks(be.ctx)
	be.s.Wait()
	be.runnerOnline()
	return be
}

// #212 changes the CI workflow, and so did main: no handoff task is filed
// and no wait is set before a round. The conflict round settles it, and the
// runner lands a resolution that adds no line of its own (WP-ADR138).
func TestBabysitProtectedConflictGetsARound(t *testing.T) {
	be := protectedConflictEnv(t, "on: push\nrun: make test\njobs: 1\n")
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 1 || b.Round != 1 || !b.Rounds[0].Conflict || len(be.protectedTasks()) != 0 {
		t.Fatalf("no round for a conflict in a protected file: %+v jobs %v tasks %v", b, be.jobs(), be.protectedTasks())
	}
}

// Where main changed another part of the workflow, it merges on its own and
// the runner takes that merge: the conflict is elsewhere, and a round
// settles it (WP-597).
func TestBabysitProtectedFileThatMergesGetsARound(t *testing.T) {
	be := protectedConflictEnv(t, "on: push\nrun: make\njobs: 1\nlint: yes\n")
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 1 || b.Round != 1 || !b.Rounds[0].Conflict || len(be.protectedTasks()) != 0 {
		t.Fatalf("no round for a conflict outside protected files: %+v jobs %v", b, be.jobs())
	}
}

func TestShSingleQuote(t *testing.T) {
	for in, want := range map[string]string{"main": "'main'", "a;$(x)`y`": "'a;$(x)`y`'", "it's": `'it'\''s'`} {
		if got := shSingleQuote(in); got != want {
			t.Errorf("shSingleQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
