package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// A claim carries a signed spec only where the machine's helper is ready
// and the machine isn't paused (WP-418, PL7 §5.3).
func TestSandboxSpecs(t *testing.T) {
	e := newEnv(t)
	web := e.browser(e.admin)
	p := e.project(e.admin, "Alpha", "")
	res, err := e.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	forge, _ := res.LastInsertId()
	if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, forge_repo_id, created_at) VALUES (?,?, 'Acme/App', 77, 0)`, p.ID, forge); err != nil {
		t.Fatal(err)
	}
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://x.test/a/b.git", Permission: AgentEditRun})
	claim := func(rep *sandbox.Report, project int64) *RunnerClaim {
		t.Helper()
		e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, ProjectID: project})
		w := e.poll(e.admin, r, RunnerPoll{Sandbox: rep})
		if w.Job == nil {
			t.Fatal("no job")
		}
		return w.Job
	}

	// No report (an older wp), or no helper: no spec.
	if c := claim(nil, p.ID); c.Sandbox != nil {
		t.Errorf("spec with no report: %+v", c.Sandbox)
	}
	if c := claim(&sandbox.Report{Installed: false}, p.ID); c.Sandbox != nil {
		t.Errorf("spec with no helper: %+v", c.Sandbox)
	}
	ms, _ := e.s.SandboxMachines(e.ctx, web)
	if h := ms[0].Helper; h == nil || h.Ready || !strings.Contains(h.Problem, "isn't installed") {
		t.Errorf("machine helper = %+v", h)
	}

	key, err := e.s.SandboxKey(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := sandbox.ParseKey(key.Key)
	if err != nil || sandbox.Fingerprint(pub) != key.Fingerprint {
		t.Fatalf("key %+v %v", key, err)
	}
	if again, _ := e.s.SandboxKey(e.ctx, e.admin); again.Key != key.Key {
		t.Error("the key changed")
	}
	status := func(fp string, storeOK bool) *sandbox.Report {
		return &sandbox.Report{Installed: true, Status: &sandbox.Status{Version: "v1", Key: fp,
			Store: sandbox.StoreStatus{Path: sandbox.StoreDir, OK: storeOK, Problem: "gone"}, Drift: []string{strings.Repeat("x", 5000)}}}
	}
	// Another key, or a broken store: no spec.
	if c := claim(status("SHA256:other", true), p.ID); c.Sandbox != nil {
		t.Error("spec for a helper pinning another key")
	}
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	if h := ms[0].Helper; h.Ready || !strings.Contains(h.Problem, "another key") || len(h.Status.Drift[0]) > maxReportText {
		t.Errorf("machine helper = %+v", h)
	}
	if c := claim(status(key.Fingerprint, false), p.ID); c.Sandbox != nil {
		t.Error("spec for a broken store")
	}

	// Ready: a spec for the project's repo, signed with the key.
	c := claim(status(key.Fingerprint, true), p.ID)
	if c.Sandbox == nil {
		t.Fatal("no spec for a ready helper")
	}
	spec, err := sandbox.Verify(pub, c.Sandbox, e.clockNow())
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.Repo{ID: itoa64(forge) + "/77", Slug: "Acme/App", Owner: itoa64(forge) + "/acme"}
	if spec.Op != sandbox.OpEnsureUsers || spec.Runner != r.ID || spec.Job != c.ID || len(spec.Repos) != 1 || spec.Repos[0] != want ||
		time.Duration(spec.Expiry-spec.Issued)*time.Second != sandboxSpecLife {
		t.Errorf("spec = %+v %+v", spec, spec.Repos)
	}

	// A project with more repos: a user for each, the job's own first
	// (WP-448).
	if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, forge_repo_id, created_at) VALUES (?,?, 'Other/Lib', 78, 0)`, p.ID, forge); err != nil {
		t.Fatal(err)
	}
	lib, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "hi", Repo: "git@git.example.com:other/lib.git", Permission: AgentEditRun})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: lib, ProjectID: p.ID})
	w := e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.Sandbox == nil {
		t.Fatal("no spec for the two-repo job")
	}
	spec, err = sandbox.Verify(pub, w.Job.Sandbox, e.clockNow())
	if err != nil || len(spec.Repos) != 2 || spec.Repos[0].Slug != "Other/Lib" || spec.Repos[1] != want {
		t.Errorf("two-repo spec = %+v %v", spec, err)
	}
	var lastJob int64
	if err := e.s.db.R.QueryRow(`SELECT job_id FROM sandbox_user_jobs WHERE runner_id = ? AND kind = 'org' AND user_key = ?`, r.ID, itoa64(forge)+"/other").Scan(&lastJob); err != nil || lastJob != w.Job.ID {
		t.Errorf("last job of the org user = %d %v, want %d", lastJob, err, w.Job.ID)
	}
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	if h := ms[0].Helper; !h.Ready || h.Problem != "" || h.Status.Version != "v1" {
		t.Errorf("ready machine = %+v", h)
	}

	// A later poll without a report keeps the last one.
	if c := claim(nil, p.ID); c.Sandbox == nil {
		t.Error("the report was forgotten")
	}
	// A job for no project, or a project with no known repo: no spec.
	if c := claim(nil, 0); c.Sandbox != nil {
		t.Error("spec for a job with no project")
	}
	bare := e.project(e.admin, "Bare", "")
	if c := claim(nil, bare.ID); c.Sandbox != nil {
		t.Error("spec for a project with no repo")
	}

	// The kill switch: a paused machine gets none.
	if _, err := e.s.Reauth(e.ctx, web, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetSandboxMachine(e.ctx, web, r.ID, SandboxMachineInput{Paused: ptr(true), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if c := claim(nil, p.ID); c.Sandbox != nil {
		t.Error("spec for a paused machine")
	}
}

// A person asks a machine's helper to remove a user or finish what a
// crash left half done; the runner gets it signed on its next poll and
// says how it went on the one after (WP-448).
func TestSandboxOps(t *testing.T) {
	e := newEnv(t)
	web := e.browser(e.admin)
	p := e.project(e.admin, "Alpha", "")
	res, err := e.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	forge, _ := res.LastInsertId()
	f := itoa64(forge)
	if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, forge_repo_id, created_at) VALUES (?,?, 'Acme/App', 77, 0)`, p.ID, forge); err != nil {
		t.Fatal(err)
	}
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	key, err := e.s.SandboxKey(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := sandbox.ParseKey(key.Key)
	users := []sandbox.User{
		{Name: "wp-app", UID: 61001, Kind: sandbox.KindRepo, Key: f + "/77", Slug: "Acme/App"},
		{Name: "wp-acme", UID: 61002, Kind: sandbox.KindOrg, Key: f + "/acme", Slug: "Acme"},
		{Name: "wp-gone", UID: 61003, Kind: sandbox.KindRepo, Key: f + "/99", Slug: "Acme/Gone"},
	}
	report := &sandbox.Report{Installed: true, Status: &sandbox.Status{Version: "v1", Key: key.Fingerprint,
		Store: sandbox.StoreStatus{Path: sandbox.StoreDir, OK: true}, Users: users}}

	// Not before the helper answers.
	if _, err := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxReconcile}); !IsInvalid(err) {
		t.Errorf("op with no helper: %v", err)
	}
	// A job makes the users; its id is their last job.
	in, _ := json.Marshal(AgentJob{CLI: "claude", Prompt: "hi", Repo: "https://git.example.com/acme/app.git", Permission: AgentEditRun})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobAgent, Input: in, ProjectID: p.ID})
	job := e.poll(e.admin, r, RunnerPoll{Sandbox: report}).Job
	if job == nil || job.Sandbox == nil {
		t.Fatal("no job with a spec")
	}
	ms, _ := e.s.SandboxMachines(e.ctx, web)
	info := ms[0].Users
	if u := info["wp-app"]; u == nil || !u.Linked || u.LastJob == nil || u.LastJob.ID != job.ID {
		t.Errorf("wp-app info = %+v", u)
	}
	if u := info["wp-acme"]; u == nil || !u.Linked || u.LastJob == nil || u.LastJob.ID != job.ID {
		t.Errorf("wp-acme info = %+v", u)
	}
	if u := info["wp-gone"]; u == nil || u.Linked || u.LastJob != nil {
		t.Errorf("wp-gone info = %+v", u)
	}

	// Only a person in the browser, the owner or an admin, for a user the
	// helper has.
	if _, err := e.s.RequestSandboxOp(e.ctx, e.admin, r.ID, SandboxOpInput{Op: SandboxReconcile}); !errors.Is(err, ErrSandboxUI) {
		t.Errorf("op from the CLI: %v", err)
	}
	bob := e.browser(e.user("bob", false))
	if _, err := e.s.RequestSandboxOp(e.ctx, bob, r.ID, SandboxOpInput{Op: SandboxReconcile}); !errors.Is(err, ErrNotFound) {
		t.Errorf("op on someone else's runner: %v", err)
	}
	if _, err := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxRemoveUser, User: "wp-nobody"}); !IsInvalid(err) {
		t.Errorf("removing an unknown user: %v", err)
	}
	if _, err := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: "format_disk"}); !IsInvalid(err) {
		t.Errorf("unknown op: %v", err)
	}

	op, err := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxRemoveUser, User: "wp-gone"})
	if err != nil || op.State != "waiting" || op.User != "wp-gone" || op.RequestedBy == nil {
		t.Fatalf("remove = %+v %v", op, err)
	}
	if again, _ := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxRemoveUser, User: "wp-gone"}); again == nil || again.ID != op.ID {
		t.Errorf("asking twice made another: %+v", again)
	}
	rec, err := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxReconcile, User: "ignored"})
	if err != nil || rec.User != "" {
		t.Fatalf("reconcile = %+v %v", rec, err)
	}

	// The next poll carries both, signed for this runner.
	w := e.poll(e.admin, r, RunnerPoll{Running: []int64{job.ID}, Free: ptr(0)})
	if len(w.Sandbox) != 2 || w.Sandbox[0].ID != op.ID || w.Sandbox[1].ID != rec.ID {
		t.Fatalf("poll ops = %+v", w.Sandbox)
	}
	spec, err := sandbox.Verify(pub, w.Sandbox[0].Spec, e.clockNow())
	if err != nil || spec.Op != sandbox.OpRemoveUser || spec.Runner != r.ID || spec.User == nil ||
		*spec.User != (sandbox.Target{Name: "wp-gone", Kind: sandbox.KindRepo, Key: f + "/99"}) {
		t.Errorf("remove spec = %+v %v", spec, err)
	}
	if spec, err := sandbox.Verify(pub, w.Sandbox[1].Spec, e.clockNow()); err != nil || spec.Op != sandbox.OpReconcile || spec.User != nil {
		t.Errorf("reconcile spec = %+v %v", spec, err)
	}
	// Not again while the runner works on them; again if it never says.
	if w := e.poll(e.admin, r, RunnerPoll{Running: []int64{job.ID}, Free: ptr(0)}); len(w.Sandbox) != 0 {
		t.Errorf("resent at once: %+v", w.Sandbox)
	}
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	if o := ms[0].Ops; len(o) != 2 || o[0].State != "sent" || o[1].State != "sent" {
		t.Errorf("ops = %+v", o)
	}
	e.setClock(e.clockNow().Add(sandboxOpResend + time.Second))
	w = e.poll(e.admin, r, RunnerPoll{Running: []int64{job.ID}, Free: ptr(0)})
	if len(w.Sandbox) != 2 {
		t.Fatalf("not resent: %+v", w.Sandbox)
	}
	// The runner says how they went.
	e.poll(e.admin, r, RunnerPoll{Running: []int64{job.ID}, Free: ptr(0), SandboxDone: []RunnerSandboxResult{
		{ID: op.ID}, {ID: rec.ID, Error: "  the store is gone  "}, {ID: 9999}}})
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	if o := ms[0].Ops; len(o) != 2 || o[1].State != "done" || o[0].State != "failed" || o[0].Error != "the store is gone" || o[1].DoneAt == nil {
		t.Errorf("finished ops = %+v %+v", o[0], o[1])
	}
	// Done ones aren't the same action: asking again queues another.
	if again, _ := e.s.RequestSandboxOp(e.ctx, web, r.ID, SandboxOpInput{Op: SandboxRemoveUser, User: "wp-gone"}); again == nil || again.ID == op.ID {
		t.Errorf("a finished action was reused: %+v", again)
	}
	// One no runner took in an hour failed.
	e.setClock(e.clockNow().Add(sandboxOpExpiry + time.Minute))
	if w := e.poll(e.admin, r, RunnerPoll{Running: []int64{job.ID}, Free: ptr(0)}); len(w.Sandbox) != 0 {
		t.Errorf("expired op sent: %+v", w.Sandbox)
	}
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	if o := ms[0].Ops[0]; o.State != "failed" || !strings.Contains(o.Error, "within an hour") {
		t.Errorf("expired op = %+v", o)
	}
}
