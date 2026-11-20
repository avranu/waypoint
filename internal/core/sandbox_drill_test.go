package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/probe"
)

func drillResult(cases ...probe.Case) json.RawMessage {
	b, _ := json.Marshal(probe.Result{Cases: cases})
	return b
}

// drillReport is a whole report: cases and the ones every report has.
func drillReport(cases ...probe.Case) json.RawMessage {
	return drillResult(append(cases,
		probe.Case{Name: probe.CaseFork, Group: "resources", Want: probe.Refused, Got: probe.Refused},
		probe.Case{Name: probe.CaseTime, Group: "resources", Want: probe.Refused, Got: probe.Refused},
		probe.Case{Name: probe.CaseFinished, Group: "resources", Want: probe.Allowed, Got: probe.Allowed})...)
}

var (
	drillRefused = probe.Case{Name: "read /etc/shadow", Group: "files", Want: probe.Refused, Got: probe.Refused}
	drillControl = probe.Case{Name: "the forge through the broker", Group: "egress", Want: probe.Allowed, Got: probe.Allowed}
	drillEscaped = probe.Case{Name: "list /projects", Group: "files", Want: probe.Refused, Got: probe.Allowed, Detail: "3 entries"}
)

// A ready machine gets a drill when first ready, weekly and after its
// helper changes; its job carries signed Contained start specs as the
// newest repo user; how it ends is recorded and told (WP-473, PL7 §13).
func TestSandboxDrill(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	r := e.runner(bob, "box", func(r *RunnerRegistration) { r.Features = []string{FeatureSandboxDrill} })
	key, err := e.s.SandboxKey(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := sandbox.ParseKey(key.Key)
	report := func(version string, users ...sandbox.User) *sandbox.Report {
		return &sandbox.Report{Installed: true, Status: &sandbox.Status{Version: version, Key: key.Fingerprint,
			Store: sandbox.StoreStatus{Path: sandbox.StoreDir, OK: true}, Users: users}}
	}
	drills := func() int {
		var n int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM sandbox_drills WHERE runner_id = ?`, r.ID).Scan(&n)
		return n
	}
	told := func(a *Actor) int {
		var n int
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ?`, a.User.ID, NotifySandbox).Scan(&n)
		return n
	}
	last := func() *SandboxDrill {
		t.Helper()
		d, err := e.s.lastSandboxDrill(e.ctx, e.s.db.R, r.ID, true)
		if err != nil || d == nil {
			t.Fatalf("last drill %+v %v", d, err)
		}
		return d
	}

	// No helper, or a helper with no repo user: no drill.
	e.s.maybeSandboxDrills(e.ctx)
	e.poll(bob, r, RunnerPoll{Sandbox: report("v1", sandbox.User{Name: "wp-acme", Kind: sandbox.KindOrg, Key: "1/acme", Present: true})})
	e.s.maybeSandboxDrills(e.ctx)
	if n := drills(); n != 0 {
		t.Fatalf("%d drills before the machine could run one", n)
	}
	web := e.browser(bob)
	if _, err := e.s.StartSandboxDrill(e.ctx, web, r.ID); !IsInvalid(err) || !strings.Contains(err.Error(), "no repo user") {
		t.Errorf("drill with no repo user: %v", err)
	}

	users := []sandbox.User{
		{Name: "wp-old", Kind: sandbox.KindRepo, Key: "1/76", Slug: "Acme/Old", LastUsed: 100, Present: true},
		{Name: "wp-new", Kind: sandbox.KindRepo, Key: "1/77", Slug: "Acme/App", LastUsed: 200, Present: true},
		{Name: "wp-gone", Kind: sandbox.KindRepo, Key: "1/78", Slug: "Acme/Gone", LastUsed: 300},
	}
	e.poll(bob, r, RunnerPoll{Sandbox: report("v1", users...)})

	// Only the owner or an admin, in the browser.
	if _, err := e.s.StartSandboxDrill(e.ctx, bob, r.ID); !errors.Is(err, ErrSandboxUI) {
		t.Errorf("drill from the CLI: %v", err)
	}
	if _, err := e.s.StartSandboxDrill(e.ctx, e.browser(e.user("carol", false)), r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("drill on someone else's machine: %v", err)
	}

	// The first: queued once, as the owner's job for this runner.
	e.s.maybeSandboxDrills(e.ctx)
	e.s.maybeSandboxDrills(e.ctx)
	if n := drills(); n != 1 {
		t.Fatalf("%d drills, want 1", n)
	}
	if d := last(); d.Reason != "first" || d.State != DrillQueued || d.HelperVersion != "v1" || len(d.NotCovered) == 0 {
		t.Errorf("first drill %+v", d)
	}
	if _, err := e.s.StartSandboxDrill(e.ctx, web, r.ID); !IsInvalid(err) || !strings.Contains(err.Error(), "already queued") {
		t.Errorf("a second drill while one is queued: %v", err)
	}

	claim := func(rep *sandbox.Report) *RunnerClaim {
		t.Helper()
		w := e.poll(bob, r, RunnerPoll{Sandbox: rep})
		if w.Job == nil || w.Job.Kind != JobSandboxDrill {
			t.Fatalf("claim = %+v", w.Job)
		}
		return w.Job
	}
	finish := func(c *RunnerClaim, in RunnerFinish) {
		t.Helper()
		if _, err := e.s.FinishRunnerJob(e.ctx, bob, c.ID, in); err != nil {
			t.Fatal(err)
		}
	}

	c := claim(nil)
	if len(c.Sessions) != 3 || c.Sandbox != nil {
		t.Fatalf("drill claim: %d sessions, spec %v", len(c.Sessions), c.Sandbox)
	}
	for i, sd := range c.Sessions {
		spec, err := sandbox.Verify(pub, sd, e.clock)
		if err != nil {
			t.Fatal(err)
		}
		want := probe.Sessions(c.ID)[i]
		if spec.Op != sandbox.OpStart || spec.Level != sandbox.LevelContained || spec.Runner != r.ID || spec.Job != c.ID ||
			len(spec.Repos) != 1 || spec.Repos[0].ID != "1/77" || spec.Session == nil || spec.Session.ID != want.ID ||
			time.Duration(spec.Expiry-spec.Issued)*time.Second > sandbox.MaxSpecLife {
			t.Errorf("spec %d = %+v %+v", i, spec, spec.Session)
		}
	}
	if d := last(); d.State != DrillRunning || d.RepoUser != "wp-new" || d.JobID != c.ID {
		t.Errorf("running drill %+v", d)
	}

	// It passed: recorded, no one told.
	finish(c, RunnerFinish{State: "done", Result: drillReport(drillRefused, drillControl)})
	if d := last(); d.State != DrillPassed || d.Passed != 5 || len(d.Cases) != 5 || d.FinishedAt == nil {
		t.Errorf("passed drill %+v", d)
	}
	if told(bob)+told(e.admin) != 0 {
		t.Error("a passed drill was told")
	}
	e.s.maybeSandboxDrills(e.ctx)
	if n := drills(); n != 1 {
		t.Errorf("another drill right after the last: %d", n)
	}

	// A week on: something got out. The owner and the admins hear.
	e.clock = e.clock.Add(sandboxDrillEvery)
	e.s.maybeSandboxDrills(e.ctx)
	if d := last(); d.Reason != "weekly" {
		t.Fatalf("weekly drill %+v", d)
	}
	finish(claim(nil), RunnerFinish{State: "done", Result: drillReport(drillRefused, drillControl, drillEscaped)})
	if d := last(); d.State != DrillFailed || d.Escaped != 1 || d.Cases[2].Detail != "3 entries" {
		t.Errorf("failed drill %+v", d)
	}
	if told(bob) != 1 || told(e.admin) != 1 {
		t.Errorf("failed drill told bob %d, the admin %d", told(bob), told(e.admin))
	}

	// A new helper: a drill now. It couldn't run: recorded, told only the
	// second time in a row, and not the third.
	e.poll(bob, r, RunnerPoll{Sandbox: report("v2", users...)})
	e.s.maybeSandboxDrills(e.ctx)
	if d := last(); d.Reason != "helper" || d.HelperVersion != "v2" {
		t.Fatalf("helper drill %+v", d)
	}
	finish(claim(nil), RunnerFinish{State: "failed", Error: "the helper refused the spec"})
	if d := last(); d.State != DrillError || d.Error != "the helper refused the spec" {
		t.Errorf("errored drill %+v", d)
	}
	// A report that stops part way proves nothing either.
	for i, try := range []struct {
		result json.RawMessage
		why    string
		told   int
	}{
		{drillResult(), "the runner reported no tries", 2},
		{drillResult(drillRefused, drillControl), "it stopped part way: no " + strings.Join(probe.Required, ", "), 2},
	} {
		want := try.told
		e.clock = e.clock.Add(sandboxDrillRetry)
		e.s.maybeSandboxDrills(e.ctx)
		if d := last(); d.Reason != "retry" {
			t.Fatalf("retry %d: %+v", i, d)
		}
		finish(claim(nil), RunnerFinish{State: "done", Result: try.result})
		if d := last(); d.State != DrillError || d.Error != try.why {
			t.Errorf("retry %d: %+v", i, d)
		}
		if told(bob) != want || told(e.admin) != want {
			t.Errorf("error %d: told bob %d, the admin %d, want %d", i+2, told(bob), told(e.admin), want)
		}
	}

	// A person can start one; a paused machine can't run one.
	d, err := e.s.StartSandboxDrill(e.ctx, web, r.ID)
	if err != nil || d.Reason != "person" || d.StartedBy == nil || d.StartedBy.ID != bob.User.ID {
		t.Fatalf("person's drill %+v %v", d, err)
	}
	finish(claim(nil), RunnerFinish{State: "done", Result: drillReport(drillRefused, drillControl)})
	admin := e.browser(e.admin)
	if _, err := e.s.Reauth(e.ctx, admin, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetSandboxMachine(e.ctx, admin, r.ID, SandboxMachineInput{Paused: ptr(true), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.StartSandboxDrill(e.ctx, admin, r.ID); !IsInvalid(err) || !strings.Contains(err.Error(), "paused") {
		t.Errorf("drill on a paused machine: %v", err)
	}
	ms, _ := e.s.SandboxMachines(e.ctx, admin)
	if len(ms) != 1 || ms[0].Drill == nil || ms[0].Drill.ID != d.ID || ms[0].Drill.State != DrillPassed || ms[0].DrillBlocked != "its sandboxing is paused" {
		t.Errorf("machine drill %+v", ms)
	}
}

// A drill job's specs can't be had once the machine is paused between the
// queue and the claim: the runner fails the job, the drill says why.
func TestSandboxDrillPausedAtClaim(t *testing.T) {
	e := newEnv(t)
	r := e.runner(e.admin, "box", func(r *RunnerRegistration) { r.Features = []string{FeatureSandboxDrill} })
	key, _ := e.s.SandboxKey(e.ctx, e.admin)
	e.poll(e.admin, r, RunnerPoll{Sandbox: &sandbox.Report{Installed: true, Status: &sandbox.Status{Version: "v1", Key: key.Fingerprint,
		Store: sandbox.StoreStatus{Path: sandbox.StoreDir, OK: true},
		Users: []sandbox.User{{Name: "wp-app", Kind: sandbox.KindRepo, Key: "1/77", Present: true}}}}})
	e.s.maybeSandboxDrills(e.ctx)
	web := e.browser(e.admin)
	if _, err := e.s.Reauth(e.ctx, web, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetSandboxMachine(e.ctx, web, r.ID, SandboxMachineInput{Paused: ptr(true), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	w := e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobSandboxDrill || w.Job.Sessions != nil {
		t.Fatalf("claim on a paused machine = %+v", w.Job)
	}
	if _, err := e.s.FinishRunnerJob(e.ctx, e.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "no start specs"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.s.lastSandboxDrill(e.ctx, e.s.db.R, r.ID, false); d == nil || d.State != DrillError || d.Error != "no start specs" {
		t.Errorf("drill %+v", d)
	}
}

func TestDrillDue(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	at := func(state, version string, ago time.Duration) *SandboxDrill {
		return &SandboxDrill{State: state, HelperVersion: version, CreatedAt: now.Add(-ago)}
	}
	for _, c := range []struct {
		last *SandboxDrill
		want string
	}{
		{nil, "first"},
		{at(DrillQueued, "v0", 30*24*time.Hour), ""},
		{at(DrillRunning, "v1", 30*24*time.Hour), ""},
		{at(DrillPassed, "v0", time.Hour), "helper"},
		{at(DrillPassed, "v1", 6*24*time.Hour), ""},
		{at(DrillPassed, "v1", 7*24*time.Hour), "weekly"},
		{at(DrillFailed, "v1", 2*24*time.Hour), ""},
		{at(DrillError, "v1", 23*time.Hour), ""},
		{at(DrillError, "v1", 24*time.Hour), "retry"},
	} {
		if got := drillDue(c.last, "v1", now); got != c.want {
			t.Errorf("%+v: %q, want %q", c.last, got, c.want)
		}
	}
}
