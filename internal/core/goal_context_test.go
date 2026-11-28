package core

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// uniqWords are n words no other title of the tests shares.
func uniqWords(seed, n int) []string {
	out := make([]string, n)
	for k := range out {
		v := seed*100 + k
		out[k] = fmt.Sprintf("zq%c%c%cvexillum", 'a'+v/676%26, 'a'+v/26%26, 'a'+v%26)
	}
	return out
}

// uniqTitle is a title of at least min runes of words only it has.
func uniqTitle(seed, min int) string {
	for n := 1; ; n++ {
		if t := strings.Join(uniqWords(seed, n), " "); utf8.RuneCountInString(t) >= min {
			return t
		}
	}
}

func steps(titles ...string) []GoalStep {
	out := make([]GoalStep, len(titles))
	for i, t := range titles {
		out[i] = GoalStep{ID: int64(i + 1), Title: t}
	}
	return out
}

func stepTitles(s []GoalStep) []string {
	out := make([]string, len(s))
	for i, g := range s {
		out[i] = g.Title
	}
	return out
}

// The chain arm shows every parent as written; the goal arm none.
func TestGoalChainArms(t *testing.T) {
	long := uniqTitle(1, 220)
	ps := steps("The interface is intuitive", long)
	got, above := goalChain(goalArmChain, ps, nil)
	if !slices.Equal(stepTitles(got), stepTitles(ps)) || above != 0 {
		t.Errorf("chain = %v above %d", got, above)
	}
	for _, g := range got {
		if g.Clipped {
			t.Errorf("chain clipped %q", g.Title)
		}
	}
	if got, above := goalChain(goalArmGoal, ps, nil); len(got) != 0 || above != 0 {
		t.Errorf("goal arm = %v %d", got, above)
	}
}

// The pruned arm leaves out exactly the parents the judgement names, never
// by their words, and never rewords one.
func TestGoalChainPruned(t *testing.T) {
	ps := steps("The interface is intuitive", "Storage stays reliable", "Alpha thinking")
	got, above := goalChain(goalArmPruned, ps, map[int64]bool{1: true})
	if !slices.Equal(stepTitles(got), []string{"Storage stays reliable", "Alpha thinking"}) || above != 1 {
		t.Errorf("covered = %v above %d", got, above)
	}
	got, above = goalChain(goalArmPruned, ps, nil)
	if !slices.Equal(stepTitles(got), stepTitles(ps)) || above != 0 {
		t.Errorf("no judgement = %v above %d", got, above)
	}
	twins := steps("Customers trust billing accuracy", "Billing accuracy and customers trust")
	got, above = goalChain(goalArmPruned, twins, map[int64]bool{2: true})
	if len(got) != 1 || got[0].ID != 1 || above != 1 {
		t.Errorf("twins = %v above %d", got, above)
	}
}

// Past the length budget the rest of the parents are counted as above.
func TestGoalChainBudget(t *testing.T) {
	var titles []string
	for k := range 4 {
		titles = append(titles, uniqTitle(10+k, 125))
	}
	got, above := goalChain(goalArmPruned, steps(titles...), nil)
	if len(got) != 3 || above != 1 {
		t.Fatalf("budget: %d shown, above %d", len(got), above)
	}
	if got[2].ID != 3 {
		t.Errorf("the nearest are kept: %+v", got)
	}
}

// A long title is cut at a word and says so; what is left is the title's own
// first words.
func TestGoalClip(t *testing.T) {
	long := uniqTitle(2, 240)
	got, above := goalChain(goalArmPruned, steps(long), nil)
	if len(got) != 1 || above != 0 || !got[0].Clipped {
		t.Fatalf("clip = %+v %d", got, above)
	}
	title := got[0].Title
	if !strings.HasSuffix(title, "…") || utf8.RuneCountInString(title) > goalCtxClip+1 {
		t.Errorf("clipped = %q", title)
	}
	kept := strings.TrimSuffix(title, "…")
	if !strings.HasPrefix(long, kept) || long[len(kept)] != ' ' {
		t.Errorf("%q is not a prefix of whole words of the title", kept)
	}
	if s, ok := goalClip("Short title"); ok || s != "Short title" {
		t.Errorf("short title = %q %v", s, ok)
	}
}

// goalCtxEnv is a project with goals three deep and tasks under the leaf.
type goalCtxEnv struct {
	*polEnv
	top, mid, leaf *DesignNote
}

func newGoalCtxEnv(t *testing.T) *goalCtxEnv {
	e := &goalCtxEnv{polEnv: newPolEnv(t)}
	e.top = e.goal(e.admin, e.p.Key, "Customers enjoy a polished interface")
	e.mid = e.child(e.admin, e.top, "Navigation feels predictable")
	e.leaf = e.child(e.admin, e.mid, "Buttons have a consistent style")
	return e
}

func (e *goalCtxEnv) child(a *Actor, parent *DesignNote, title string) *DesignNote {
	e.t.Helper()
	n, err := e.s.CreateDesign(e.ctx, a, DesignInput{Title: &title, Kind: str("goal"), Project: str(e.p.Key), Parent: Some(parent.ID)})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// serving is a task the person linked to the goal, claimed by the agent
// just now.
func (e *goalCtxEnv) serving(title string, goal *DesignNote) *IssueDetail {
	e.t.Helper()
	i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: title})
	e.serve(e.admin, i.Key, goal.ID)
	if _, err := e.s.RecomputeGoalTraces(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE issues SET claimed_by = ?, claimed_at = ? WHERE id = ?`, e.ag.AgentName(), e.s.nowMS(), i.ID)
	return i
}

func (e *goalCtxEnv) rows(id int64) int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM goal_context_assignments WHERE issue_id = ?`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *goalCtxEnv) task(a *Actor, id int64) *AgentTask {
	e.t.Helper()
	got, err := e.s.agentTask(e.ctx, a, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// read is the goal a's read of the task shows. A failed read, or a task with
// no goal, stops the test with the reason instead of a nil dereference.
func (e *goalCtxEnv) read(a *Actor, key string) *IssueGoal {
	e.t.Helper()
	d, err := e.s.GetIssue(e.ctx, a, key)
	if err != nil {
		e.t.Fatalf("read %s: %v", key, err)
	}
	if d.Goal == nil || d.Goal.Goal == nil {
		e.t.Fatalf("read %s: no goal: %+v", key, d.Goal)
	}
	return d.Goal
}

// The walk goes up from the goal, nearest first, over approved goals the
// actor can read, and stops at a cycle and at the step cap.
func TestGoalParents(t *testing.T) {
	e := newGoalCtxEnv(t)
	ids := func(s []GoalStep) []int64 {
		var out []int64
		for _, g := range s {
			out = append(out, g.ID)
		}
		return out
	}
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, e.leaf.ID); !slices.Equal(ids(got), []int64{e.mid.ID, e.top.ID}) {
		t.Errorf("parents = %+v", got)
	}
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, e.top.ID); len(got) != 0 {
		t.Errorf("a top goal has none: %+v", got)
	}

	other := agentActor(e.admin, "anthropic/claude-code")
	mine := e.child(other, e.top, "Pages load quickly")
	if mine.Approved {
		t.Fatal("an agent's goal waits for a person")
	}
	under := e.child(e.admin, mine, "Search answers at once")
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, under.ID); !slices.Equal(ids(got), []int64{e.top.ID}) {
		t.Errorf("unapproved parent = %+v", got)
	}

	hidden := e.project(e.admin, "Hidden", "")
	secret := e.goal(e.admin, hidden.Key, "Secret ambition")
	e.exec(`UPDATE design_notes SET parent_id = ? WHERE id = ?`, secret.ID, e.top.ID)
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, e.leaf.ID); !slices.Equal(ids(got), []int64{e.mid.ID, e.top.ID}) {
		t.Errorf("unreadable parent = %+v", got)
	}
	e.exec(`UPDATE design_notes SET parent_id = NULL WHERE id = ?`, e.top.ID)

	e.exec(`UPDATE design_notes SET parent_id = ? WHERE id = ?`, e.leaf.ID, e.top.ID)
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, e.leaf.ID); !slices.Equal(ids(got), []int64{e.mid.ID, e.top.ID}) {
		t.Errorf("cycle = %+v", got)
	}
	e.exec(`UPDATE design_notes SET parent_id = NULL WHERE id = ?`, e.top.ID)

	prev := e.top
	for k := range 11 {
		prev = e.child(e.admin, prev, fmt.Sprintf("Deeper level %s", uniqWords(50+k, 1)[0]))
	}
	if got := e.s.goalParents(e.ctx, e.s.db.R, e.ag, prev.ID); len(got) != goalCtxSteps {
		t.Errorf("%d steps, want %d", len(got), goalCtxSteps)
	}
}

// An agent that takes a task with a fresh claim is given an arm once and
// keeps it; others draw nothing, and a read draws nothing.
func TestGoalContextDraws(t *testing.T) {
	e := newGoalCtxEnv(t)

	first := e.serving("Restyle the save button", e.leaf)
	if g := e.read(e.ag, first.Key); len(g.Chain) != 0 || g.Arm != "" {
		t.Fatalf("a read shows %+v", g)
	}
	if e.rows(first.ID) != 0 {
		t.Error("a read drew an arm")
	}

	if got := e.task(e.admin, first.ID); len(got.Issue.Goal.Chain) != 0 || e.rows(first.ID) != 0 {
		t.Errorf("a person: %+v rows %d", got.Issue.Goal, e.rows(first.ID))
	}
	e.exec(`UPDATE issues SET claimed_at = ? WHERE id = ?`, e.s.now().Add(-time.Hour).UnixMilli(), first.ID)
	if got := e.task(e.ag, first.ID); len(got.Issue.Goal.Chain) != 0 || e.rows(first.ID) != 0 {
		t.Errorf("a stale claim: %+v rows %d", got.Issue.Goal, e.rows(first.ID))
	}
	e.exec(`UPDATE issues SET claimed_at = ? WHERE id = ?`, e.s.nowMS(), first.ID)
	if e.task(e.ag, first.ID); e.rows(first.ID) != 1 {
		t.Error("a fresh claim didn't draw")
	}
	flat := e.serving("Rename the page", e.top)
	e.task(e.ag, flat.ID)
	if e.rows(flat.ID) != 0 {
		t.Error("drew with nothing to choose between")
	}

	seen := map[string]int{}
	for k := range 16 {
		i := e.serving(fmt.Sprintf("Restyle button number %d", k), e.leaf)
		got := e.task(e.ag, i.ID)
		if e.rows(i.ID) != 1 {
			t.Fatalf("%s: %d rows", i.Key, e.rows(i.ID))
		}
		var arm, policy, cands, shares string
		var chainLen int
		if err := e.s.db.R.QueryRow(`SELECT arm, policy, candidates, shares, chain_len FROM goal_context_assignments WHERE issue_id = ?`, i.ID).Scan(&arm, &policy, &cands, &shares, &chainLen); err != nil {
			t.Fatal(err)
		}
		if policy != "thompson" || chainLen != 2 || !strings.HasPrefix(cands, "goal,chain") || shares == "" {
			t.Errorf("row = %s %s %s %s %d", arm, policy, cands, shares, chainLen)
		}
		seen[arm]++
		g := got.Issue.Goal
		if (arm == goalArmGoal) != (len(g.Chain) == 0) || (len(g.Chain) > 0 && g.Arm != arm) {
			t.Errorf("%s drawn %s shows %+v", i.Key, arm, g)
		}
		again := e.task(e.ag, i.ID)
		if again.Issue.Goal.Arm != g.Arm || len(again.Issue.Goal.Chain) != len(g.Chain) {
			t.Errorf("%s changed: %+v then %+v", i.Key, g, again.Issue.Goal)
		}
		if read := e.read(e.admin, i.Key); read.Arm != g.Arm {
			t.Errorf("%s read shows %q, take showed %q", i.Key, read.Arm, g.Arm)
		}
		if e.rows(i.ID) != 1 {
			t.Errorf("%s drawn again", i.Key)
		}
	}
	if seen[goalArmGoal] == 0 || seen[goalArmChain]+seen[goalArmPruned] == 0 {
		t.Errorf("with nothing known both ways are drawn: %v", seen)
	}
}

// A read shows the arm the cached estimate favours, never computing one.
func TestGoalContextReadBest(t *testing.T) {
	e := newGoalCtxEnv(t)
	now := e.s.now()
	var rows []*goalRow
	for k := range 80 {
		arm, drift := goalArmGoal, 1.0
		if k%2 == 1 {
			arm, drift = goalArmChain, 0
		}
		at := now.Add(-48 * time.Hour).UnixMilli()
		rows = append(rows, &goalRow{issue: int64(k + 1), arm: arm, policy: "thompson", chainLen: 2, textLen: 40 + k%9, assigned: at + int64(k)*1000,
			project: 1, typ: "task", resolved: at + 3600_000, done: true, prs: 1, checked: 1, drift: drift})
	}
	pol := goalPolicyFrom(rows, now)
	if got := pol.best(); got != goalArmChain {
		t.Fatalf("best = %s", got)
	}
	i := e.serving("Restyle the cancel button", e.leaf)
	if g := e.read(e.admin, i.Key); len(g.Chain) != 0 {
		t.Fatalf("no estimate yet: %+v", g)
	}
	e.s.goalPol.pol = pol
	if g := e.read(e.admin, i.Key); g.Arm != goalArmChain || len(g.Chain) != 2 || g.Above != 0 {
		t.Errorf("best arm shown: %+v", g)
	}
	if e.rows(i.ID) != 0 {
		t.Error("a read drew")
	}
	if goalPolicyFrom(nil, now).best() != goalArmGoal {
		t.Error("with no evidence the goal alone is shown")
	}
}

// The outcome: drift flags no person called fine, known once the task is
// done and every pull request has a check.
func TestGoalContextOutcome(t *testing.T) {
	now := time.Now()
	r := &goalRow{done: true, prs: 2, checked: 2, drift: 2, assigned: now.Add(-24 * time.Hour).UnixMilli(), resolved: now.UnixMilli()}
	if y, ok := goalValue(&goalPrimary, r, now); !ok || y < 1.09 || y > 1.1 {
		t.Errorf("two flags = %v %v", y, ok)
	}
	r.checked = 1
	if _, ok := goalValue(&goalPrimary, r, now); ok {
		t.Error("a pending check has no value yet")
	}
	r.checked, r.prs = 0, 0
	if _, ok := goalValue(&goalPrimary, r, now); ok {
		t.Error("no pull request, no value")
	}
	r.prs, r.checked, r.done = 1, 1, false
	if _, ok := goalValue(&goalPrimary, r, now); ok {
		t.Error("not done, no value")
	}
}

// goalRows reads flags from the checks, leaving out one a person answered
// "this is fine" and counting a clear check as known. Each task has its
// drift value or not for one reason, shown by the same task once that reason
// is gone, and the report counts each done task it leaves out under its own
// reason.
func TestGoalContextRows(t *testing.T) {
	e := newGoalCtxEnv(t)
	f := e.forge("forgejo", "https://git.example.com", false)
	mk := func(title string, checks ...[2]string) *IssueDetail {
		i := e.serving(title, e.leaf)
		e.task(e.ag, i.ID)
		if e.rows(i.ID) != 1 {
			t.Fatalf("%q wasn't drawn", title)
		}
		e.exec(`UPDATE goal_context_assignments SET arm = 'chain' WHERE issue_id = ?`, i.ID)
		for n, c := range checks {
			num := int(i.ID)*10 + n
			url := fmt.Sprintf("https://git.example.com/acme/app/pulls/%d", num)
			res, err := e.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'acme/app', ?, ?, 'open', 0, 0)`, f.ID, num, url)
			if err != nil {
				t.Fatal(err)
			}
			pr, _ := res.LastInsertId()
			e.exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/app', ?, 'open', 0)`, i.ID, url, num)
			if c[0] != "flag" {
				e.exec(`INSERT INTO pr_goal_checks(pr_id, input_hash, verdict, flag_id, created_at, updated_at) VALUES (?,'h',?,NULL,1,1)`, pr, c[0])
				continue
			}
			fr, err := e.s.db.W.Exec(`INSERT INTO goal_drift_flags(project_id, pr_id, kind, goal_title, reason, created_at, answer) VALUES (?,?,'contradicts','g','r',1,?)`, e.p.ID, pr, c[1])
			if err != nil {
				t.Fatal(err)
			}
			flag, _ := fr.LastInsertId()
			e.exec(`INSERT INTO pr_goal_checks(pr_id, input_hash, verdict, flag_id, created_at, updated_at) VALUES (?,'h','flag',?,1,1)`, pr, flag)
		}
		return i
	}
	flagged := mk("Flagged work", [2]string{"flag", ""}, [2]string{"clear", ""})
	fine := mk("Flag a person called fine", [2]string{"flag", DriftAnswerIntent})
	pending := mk("Check pending", [2]string{"pending", ""})
	errored := mk("Check errored", [2]string{"error", ""})
	bare := mk("No pull request")
	var noFlag, zeroFlag int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FILTER (WHERE flag_id IS NULL), count(*) FILTER (WHERE flag_id = 0) FROM pr_goal_checks`).Scan(&noFlag, &zeroFlag); err != nil {
		t.Fatal(err)
	}
	if noFlag != 3 || zeroFlag != 0 {
		t.Fatalf("checks without a flag: %d NULL, %d zero", noFlag, zeroFlag)
	}

	read := func() map[int64]*goalRow {
		t.Helper()
		rows, err := e.s.goalRows(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		by := map[int64]*goalRow{}
		for _, r := range rows {
			by[r.issue] = r
		}
		for _, i := range []*IssueDetail{flagged, fine, pending, errored, bare} {
			if by[i.ID] == nil {
				t.Fatalf("goalRows has no row for %s", i.Key)
			}
		}
		return by
	}
	by := read()
	if r := by[flagged.ID]; r.prs != 2 || r.checked != 2 || r.errored != 0 || r.drift != 1 {
		t.Errorf("flagged = %+v", r)
	}
	if r := by[fine.ID]; r.prs != 1 || r.checked != 1 || r.errored != 0 || r.drift != 0 {
		t.Errorf("answered intent = %+v", r)
	}
	if r := by[pending.ID]; r.prs != 1 || r.checked != 0 || r.errored != 0 || r.drift != 0 {
		t.Errorf("pending = %+v", r)
	}
	if r := by[errored.ID]; r.prs != 1 || r.checked != 0 || r.errored != 1 || r.drift != 0 {
		t.Errorf("errored = %+v", r)
	}
	if r := by[bare.ID]; r.prs != 0 || r.checked != 0 || r.errored != 0 || r.drift != 0 {
		t.Errorf("bare = %+v", r)
	}

	now := e.s.now().Add(time.Hour)
	done := func(r *goalRow) *goalRow {
		c := *r
		c.done, c.resolved = true, e.s.nowMS()
		return &c
	}
	if y, ok := goalValue(&goalPrimary, done(by[flagged.ID]), now); !ok || y != math.Log1p(1) {
		t.Errorf("flagged value = %v %v", y, ok)
	}
	if y, ok := goalValue(&goalPrimary, done(by[fine.ID]), now); !ok || y != 0 {
		t.Errorf("answered intent value = %v %v", y, ok)
	}
	for _, c := range []struct {
		name string
		i    *IssueDetail
		fix  func(*goalRow)
	}{
		{"a pending check", pending, func(r *goalRow) { r.checked = r.prs }},
		{"a check in error", errored, func(r *goalRow) { r.checked, r.errored = r.prs, 0 }},
		{"no pull request", bare, func(r *goalRow) { r.prs, r.checked = 1, 1 }},
	} {
		r := done(by[c.i.ID])
		if _, ok := goalValue(&goalPrimary, r, now); ok {
			t.Errorf("%s has a value", c.name)
		}
		c.fix(r)
		if y, ok := goalValue(&goalPrimary, r, now); !ok || y != 0 {
			t.Errorf("%s, that reason gone, has no value: %v %v", c.name, y, ok)
		}
	}
	for _, c := range []struct {
		i    *IssueDetail
		want GoalContextFigures
	}{
		{flagged, GoalContextFigures{Done: 1, PRs: 2, DriftFlags: 1}},
		{fine, GoalContextFigures{Done: 1, PRs: 1}},
		{pending, GoalContextFigures{Done: 1, PRs: 1, CheckPending: 1}},
		{errored, GoalContextFigures{Done: 1, PRs: 1, CheckError: 1}},
		{bare, GoalContextFigures{Done: 1, NoPR: 1}},
	} {
		if got := goalFigures([]*goalRow{done(by[c.i.ID])}); got != c.want {
			t.Errorf("%s figures = %+v, want %+v", c.i.Key, got, c.want)
		}
	}

	e.exec(`UPDATE issues SET status_id = (SELECT st.id FROM statuses st WHERE st.category = 'done' LIMIT 1) WHERE id IN (?,?,?,?,?)`,
		flagged.ID, fine.ID, pending.ID, errored.ID, bare.ID)
	e.s.goalPol.pol = nil
	if _, err := e.s.GoalContextPolicy(e.ctx, e.me); err == nil {
		t.Error("a member read the report")
	}
	rep, err := e.s.GoalContextPolicy(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Given != 5 || len(rep.Arms) != 3 || rep.Arms[0].Arm != goalArmGoal || !strings.Contains(rep.String(), "chain") {
		t.Errorf("report = %+v", rep)
	}
	var chain *GoalContextArmReport
	for k := range rep.Arms {
		if rep.Arms[k].Arm == goalArmChain {
			chain = &rep.Arms[k]
		}
	}
	if chain == nil {
		t.Fatalf("no chain arm: %+v", rep.Arms)
	}
	want := GoalContextFigures{Done: 5, PRs: 5, DriftFlags: 1, NoPR: 1, CheckPending: 1, CheckError: 1}
	if chain.Drawn != 5 || chain.GoalContextFigures != want {
		t.Errorf("chain: drawn %d, figures %+v, want %+v", chain.Drawn, chain.GoalContextFigures, want)
	}

	e.exec(`UPDATE goal_drift_flags SET answer = '' WHERE pr_id IN (SELECT p.id FROM pull_requests p JOIN external_links el ON el.url = p.url WHERE el.issue_id = ?)`, fine.ID)
	if r := read()[fine.ID]; r.drift != 1 {
		t.Errorf("unanswered, fine's flag = %+v", r)
	}
}
