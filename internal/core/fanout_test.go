package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Fan-outs (WP-148).

type fanoutEnv struct {
	*taskEnv
	p *Project
}

func newFanoutEnv(t *testing.T) *fanoutEnv {
	te := newTaskEnv(t, "")
	return &fanoutEnv{taskEnv: te, p: te.project(te.admin, "Fan", "")}
}

func (fe *fanoutEnv) plan(n int, mod func(*FanoutInput)) FanoutInput {
	in := FanoutInput{Project: fe.p.Key, Machine: "box", Dir: "/work/app", Base: "origin/main", Parallel: 2}
	for i := range n {
		in.Packages = append(in.Packages, FanoutPackage{Title: "Package " + string(rune('A'+i)), Body: "Do part " + string(rune('A'+i)) + "."})
	}
	if mod != nil {
		mod(&in)
	}
	return in
}

func (fe *fanoutEnv) create(in FanoutInput) *Fanout {
	fe.t.Helper()
	f, err := fe.s.CreateFanout(fe.ctx, fe.ag, in)
	if err != nil {
		fe.t.Fatal(err)
	}
	fe.s.bg.Wait()
	return f
}

func (fe *fanoutEnv) claim(id int64) *FanoutClaim {
	fe.t.Helper()
	c, err := fe.s.ClaimFanout(fe.ctx, fe.ag, id, "box")
	if err != nil {
		fe.t.Fatal(err)
	}
	return c
}

func (fe *fanoutEnv) get(id int64) *Fanout {
	fe.t.Helper()
	f, err := fe.s.GetFanout(fe.ctx, fe.ag, id)
	if err != nil {
		fe.t.Fatal(err)
	}
	return f
}

func claimed(c *FanoutClaim) string {
	var keys []string
	for _, it := range c.Start {
		keys = append(keys, it.Key)
	}
	return strings.Join(keys, ",")
}

func (fe *fanoutEnv) messages() []string {
	fe.t.Helper()
	rows, err := fe.s.db.R.QueryContext(fe.ctx, `SELECT body FROM session_messages WHERE session_id = ? ORDER BY id`, fe.session)
	if err != nil {
		fe.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func TestFanoutStartsPackagesAFewAtATime(t *testing.T) {
	fe := newFanoutEnv(t)
	f := fe.create(fe.plan(3, func(in *FanoutInput) { in.Packages[1].Model, in.Packages[1].Budget = "opus", 12 }))
	if f.State != FanoutRunning || len(f.Items) != 3 || f.Parallel != 2 {
		t.Fatalf("fan-out: %+v", f)
	}
	// Defaults: sonnet at $0.06 a call, 150 calls; budget twice the estimate.
	a, b := f.Items[0], f.Items[1]
	if a.Model != "sonnet" || a.Estimate != 9 || a.Budget != 18 || b.Model != "opus" || b.Estimate != 15 || b.Budget != 12 {
		t.Errorf("items: %+v %+v", a, b)
	}
	if f.Estimate != 33 || f.WeekPct != 1.8 || !strings.Contains(f.Basis, "default cost per call") {
		t.Errorf("estimate %v, %v%%, %q", f.Estimate, f.WeekPct, f.Basis)
	}
	if a.Branch != strings.ToLower(a.Key)+"-package-a" {
		t.Errorf("branch %q", a.Branch)
	}
	d := fe.get(f.ID)
	for _, it := range d.Items {
		i := fe.issueKey(it.Key)
		if i.Status.Category != CatBacklog || !strings.Contains(i.Description, "fan-out #") {
			t.Errorf("%s: %s %q", it.Key, i.Status.Name, i.Description)
		}
	}
	// Queued packages are the fan-out's: nothing else hands them out.
	if fe.s.fanoutOf(fe.ctx, a.issueIDFor(fe)) == 0 {
		t.Error("a queued package isn't the fan-out's")
	}

	c := fe.claim(f.ID)
	if claimed(c) != a.Key+","+b.Key {
		t.Fatalf("first claim: %q (%s)", claimed(c), c.Waiting)
	}
	for _, it := range c.Start {
		if err := fe.s.FanoutItemStarted(fe.ctx, fe.ag, f.ID, it.Key, "a1b2c3d4"); err != nil {
			t.Fatal(err)
		}
	}
	// The package's session takes its task by name.
	if _, err := fe.s.AgentTake(fe.ctx, fe.ag, a.Key, ""); err != nil {
		t.Fatalf("take the package: %v", err)
	}
	if c = fe.claim(f.ID); len(c.Start) != 0 || !strings.Contains(c.Waiting, "All 2 slots") {
		t.Fatalf("full: %q %q", claimed(c), c.Waiting)
	}
	// Another machine can't start them: the worktrees are on box.
	if _, err := fe.s.ClaimFanout(fe.ctx, fe.ag, f.ID, "elsewhere"); !IsInvalid(err) {
		t.Errorf("claim elsewhere: %v", err)
	}

	if err := fe.s.EndFanoutItem(fe.ctx, fe.ag, f.ID, a.Key, OutcomeDone, 7.5, ""); err != nil {
		t.Fatal(err)
	}
	fe.s.bg.Wait()
	if c = fe.claim(f.ID); claimed(c) != f.Items[2].Key {
		t.Fatalf("after one ended: %q %q", claimed(c), c.Waiting)
	}
	_ = fe.s.FanoutItemStarted(fe.ctx, fe.ag, f.ID, f.Items[2].Key, "")
	// Ending twice (wp done, then the session's end) keeps the first.
	if err := fe.s.EndFanoutItem(fe.ctx, fe.ag, f.ID, a.Key, OutcomeEnded, 8, ""); err != nil {
		t.Fatal(err)
	}
	if err := fe.s.EndFanoutItem(fe.ctx, fe.ag, f.ID, b.Key, OutcomeBudget, 13, ""); err != nil {
		t.Fatal(err)
	}
	if err := fe.s.EndFanoutItem(fe.ctx, fe.ag, f.ID, f.Items[2].Key, OutcomeDone, 4, ""); err != nil {
		t.Fatal(err)
	}
	fe.s.bg.Wait()
	// The package stopped at its budget isn't retried: its task is let go.
	if i := fe.issueKey(b.Key); i.ClaimedBy != "" {
		t.Errorf("%s still claimed by %q after its budget stop", b.Key, i.ClaimedBy)
	}
	d = fe.get(f.ID)
	if d.State != FanoutFinished || d.Items[0].State != ItemDone || d.Items[0].Spent != 8 || d.Items[1].State != ItemStopped || d.Spent != 25 {
		t.Fatalf("finished: %s %+v %+v spent %v", d.State, d.Items[0], d.Items[1], d.Spent)
	}
	msgs := strings.Join(fe.messages(), "\n---\n")
	for _, want := range []string{a.Key + " done", b.Key + " stopped (stopped at its budget)", "Fan-out #" + itoa64(f.ID) + " finished: 2 of 3 done, 1 stopped"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("coordinator's news lacks %q:\n%s", want, msgs)
		}
	}
}

func (fe *fanoutEnv) issueKey(key string) *IssueDetail {
	fe.t.Helper()
	d, err := fe.s.GetIssue(fe.ctx, fe.admin, key)
	if err != nil {
		fe.t.Fatal(err)
	}
	return d
}

func (it *FanoutItem) issueIDFor(fe *fanoutEnv) int64 {
	return fe.issueKey(it.Key).ID
}

func TestFanoutOverTheLineAsksFirst(t *testing.T) {
	fe := newFanoutEnv(t)
	in := fe.plan(4, func(in *FanoutInput) { in.Model = "opus" })
	for n := range in.Packages {
		in.Packages[n].Calls = 200 // 4 × $20 = $80, 4.4% of a week
	}
	dry := in
	dry.DryRun = true
	g, err := fe.s.CreateFanout(fe.ctx, fe.ag, dry)
	if err != nil || g.ID != 0 || g.State != FanoutAsked || g.Estimate != 80 || g.WeekPct != 4.4 {
		t.Fatalf("dry run: %+v %v", g, err)
	}
	if n, _ := queryIDs(fe.ctx, fe.s.db.R, `SELECT id FROM fanouts`); len(n) != 0 {
		t.Fatal("a dry run filed a fan-out")
	}

	f := fe.create(in)
	if f.State != FanoutAsked || f.Request == "" {
		t.Fatalf("asked: %+v", f)
	}
	r, err := fe.s.GetRequest(fe.ctx, fe.admin, f.Request, 0)
	if err != nil || !strings.Contains(r.Title, "$80") || !strings.Contains(r.Body, "Choose Start") {
		t.Fatalf("the ask: %+v %v", r, err)
	}
	if c := fe.claim(f.ID); len(c.Start) != 0 || !strings.Contains(c.Waiting, f.Request) {
		t.Fatalf("claimed before the answer: %q %q", claimed(c), c.Waiting)
	}
	if _, err := fe.s.DecideRequest(fe.ctx, fe.admin, f.Request, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{fanoutStart}}}}); err != nil {
		t.Fatal(err)
	}
	fe.s.bg.Wait()
	if d := fe.get(f.ID); d.State != FanoutRunning {
		t.Fatalf("after Start: %s", d.State)
	}
	if msgs := strings.Join(fe.messages(), "\n"); !strings.Contains(msgs, "wp fanout start "+itoa64(f.ID)) {
		t.Errorf("coordinator not told to start: %q", msgs)
	}
	if c := fe.claim(f.ID); len(c.Start) != 2 {
		t.Fatalf("after Start: %q %q", claimed(c), c.Waiting)
	}

	// Don't start: nothing runs and the tasks stay where they are.
	f2 := fe.create(in)
	if _, err := fe.s.DecideRequest(fe.ctx, fe.admin, f2.Request, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{fanoutDontStart}}}}); err != nil {
		t.Fatal(err)
	}
	fe.s.bg.Wait()
	d := fe.get(f2.ID)
	if d.State != FanoutDeclined || d.Items[0].State != ItemCancelled {
		t.Fatalf("declined: %s %s", d.State, d.Items[0].State)
	}
	if fe.s.fanoutOf(fe.ctx, d.Items[0].issueIDFor(fe)) != 0 {
		t.Error("a declined fan-out still holds its tasks")
	}
}

func TestFanoutStopAndSweep(t *testing.T) {
	fe := newFanoutEnv(t)
	f := fe.create(fe.plan(3, nil))
	c := fe.claim(f.ID)
	a, b := c.Start[0], c.Start[1]
	_ = fe.s.FanoutItemStarted(fe.ctx, fe.ag, f.ID, a.Key, "aaaa1111")
	// b never says it started: after 10 minutes it failed.
	fe.setClock(fe.clockNow().Add(11 * time.Minute))
	fe.s.CheckFanouts(fe.ctx)
	fe.s.bg.Wait()
	d := fe.get(f.ID)
	if d.Items[1].State != ItemFailed {
		t.Fatalf("a launch that never reported: %s", d.Items[1].State)
	}
	// a's session was never seen for an hour: taken as ended.
	fe.setClock(fe.clockNow().Add(50 * time.Minute))
	fe.s.CheckFanouts(fe.ctx)
	fe.s.bg.Wait()
	if d = fe.get(f.ID); d.Items[0].State != ItemStopped {
		t.Fatalf("a lost session: %s", d.Items[0].State)
	}
	if i := fe.issueKey(a.Key); i.ClaimedBy != "" {
		t.Errorf("a lost package's task still claimed by %q", i.ClaimedBy)
	}
	// Nothing runs and the third could start: the coordinator is nudged.
	if msgs := strings.Join(fe.messages(), "\n"); !strings.Contains(msgs, "nothing running") {
		t.Errorf("no nudge: %q", msgs)
	}
	stopped, err := fe.s.StopFanout(fe.ctx, fe.ag, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != FanoutStopped || stopped.Items[2].State != ItemCancelled {
		t.Fatalf("stopped: %s %s", stopped.State, stopped.Items[2].State)
	}
	if c := fe.claim(f.ID); len(c.Start) != 0 {
		t.Fatalf("claimed after stop: %q", claimed(c))
	}
	_ = b
}

// A fan-out whose packages can't be saved after their tasks were filed
// cancels those tasks and ends, so nothing is left half-made (WP-579).
func TestFanoutHalfMadeCleansUp(t *testing.T) {
	fe := newFanoutEnv(t)
	fe.s.failFanoutItems = func() error { return errors.New("disk full") }
	if _, err := fe.s.CreateFanout(fe.ctx, fe.ag, fe.plan(2, nil)); err == nil {
		t.Fatal("no error")
	}
	fe.s.bg.Wait()
	rows, err := fe.s.db.R.QueryContext(fe.ctx, `SELECT st.category FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.project_id = ?`, fe.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var cat string
		_ = rows.Scan(&cat)
		n++
		if cat != CatCanceled {
			t.Errorf("a package task is %s, want canceled", cat)
		}
	}
	if n != 2 {
		t.Errorf("%d package tasks", n)
	}
	var ended int
	if err := fe.s.db.R.QueryRow(`SELECT count(*) FROM fanouts WHERE ended_at IS NULL`).Scan(&ended); err != nil || ended != 0 {
		t.Errorf("%d fan-outs left open (%v)", ended, err)
	}
}

func TestFanoutPlanChecks(t *testing.T) {
	fe := newFanoutEnv(t)
	for name, mod := range map[string]func(*FanoutInput){
		"no packages":  func(in *FanoutInput) { in.Packages = nil },
		"parallel":     func(in *FanoutInput) { in.Parallel = 9 },
		"relative dir": func(in *FanoutInput) { in.Dir = "app" },
		"model":        func(in *FanoutInput) { in.Packages[0].Model = "opus; rm -rf" },
		"branch":       func(in *FanoutInput) { in.Packages[0].Branch = "-x" },
		"same branch":  func(in *FanoutInput) { in.Packages[0].Branch, in.Packages[1].Branch = "x", "x" },
		"budget":       func(in *FanoutInput) { in.Budget = 1000 },
		"base":         func(in *FanoutInput) { in.Base = "--upload-pack=x" },
	} {
		if _, err := fe.s.CreateFanout(fe.ctx, fe.ag, fe.plan(2, mod)); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another person's fan-out is not found.
	f := fe.create(fe.plan(1, nil))
	other := fe.user("olga", false)
	if _, err := fe.s.GetFanout(fe.ctx, other, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone else's: %v", err)
	}
}

// The id a package's session is attached by is the shape claude --bg prints,
// so a stop or an attach can't be handed an option (WP-1370).
func TestFanoutItemStartedRefusesAnAttachIDThatIsNotOne(t *testing.T) {
	fe := newFanoutEnv(t)
	f := fe.create(fe.plan(3, nil))
	c := fe.claim(f.ID)
	for _, bad := range []string{"--bogus-x", "-abcdef", "abcde", strings.Repeat("a", 37), "ABCDEF12", "abcdef\n--x"} {
		if err := fe.s.FanoutItemStarted(fe.ctx, fe.ag, f.ID, c.Start[0].Key, bad); !IsInvalid(err) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if err := fe.s.FanoutItemStarted(fe.ctx, fe.ag, f.ID, c.Start[0].Key, "a1b2c3d4"); err != nil {
		t.Fatalf("a real id refused: %v", err)
	}
}
