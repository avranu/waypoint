package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// driftLLM answers the drift check's two prompts as told and counts them.
type driftLLM struct {
	mu          sync.Mutex
	answer      string
	week        string
	fail        error
	calls       int
	lastPrompts []string
	during      func() // runs once, while the next pull request's model call is out
}

func (f *driftLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (f *driftLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(prompt, "Check pull request"):
		if f.during != nil {
			f.during()
			f.during = nil
		}
		f.calls++
		f.lastPrompts = append(f.lastPrompts, prompt)
		if f.fail != nil {
			return nil, "", f.fail
		}
		return json.RawMessage(f.answer), "", nil
	case strings.HasPrefix(prompt, "Read this week's work"):
		f.calls++
		f.lastPrompts = append(f.lastPrompts, prompt)
		if f.fail != nil {
			return nil, "", f.fail
		}
		return json.RawMessage(f.week), "", nil
	}
	return json.RawMessage(`{}`), "", nil
}

func (f *driftLLM) set(answer string, fail error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer, f.fail = answer, fail
}

func (f *driftLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type driftEnv struct {
	*prEnv
	llm   *driftLLM
	agent *Actor
}

func newDriftEnv(t *testing.T) *driftEnv {
	pe := newPREnv(t, "forgejo")
	llm := &driftLLM{answer: `{"kind":"none","goal":0,"reason":""}`, week: `{"flags":[]}`}
	pe.s.LLM = llm
	pe.setClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	agent := *pe.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	return &driftEnv{prEnv: pe, llm: llm, agent: &agent}
}

// agentPR opens a pull request an agent made, carrying the task if given.
func (de *driftEnv) agentPR(number int, title string, task *IssueDetail) *PullRequest {
	de.t.Helper()
	sha := strings.Repeat(string(rune('a'+number)), 10)
	de.pullHook("opened", ForgePR{Number: number, Title: title, Author: "sam", HeadRef: "b" + sha, HeadSHA: sha, State: "open"})
	de.s.Wait()
	var id int64
	if err := de.s.db.R.QueryRow(`SELECT id FROM pull_requests WHERE number = ?`, number).Scan(&id); err != nil {
		de.t.Fatal(err)
	}
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET agent = ? WHERE id = ?`, de.agent.Agent, id); err != nil {
		de.t.Fatal(err)
	}
	pr := de.reread(id)
	if task != nil {
		if _, err := de.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			task.ID, "forgejo", "pull", pr.URL, pr.Repo, pr.Number, "open", 0); err != nil {
			de.t.Fatal(err)
		}
	}
	return pr
}

func (de *driftEnv) reread(id int64) *PullRequest {
	de.t.Helper()
	pr, err := de.s.prByID(de.ctx, de.s.db.R, id)
	if err != nil {
		de.t.Fatal(err)
	}
	return pr
}

// settleCheck starts a check and lets it finish.
func (de *driftEnv) settleCheck(pr *PullRequest) (string, *time.Time) {
	de.t.Helper()
	de.s.driftHold(de.ctx, pr, true)
	de.s.Wait()
	return de.s.driftHold(de.ctx, de.reread(pr.ID), true)
}

func (de *driftEnv) verdict(prID int64) string {
	de.t.Helper()
	c, err := de.s.prGoalCheckOf(de.ctx, prID)
	if err != nil {
		de.t.Fatal(err)
	}
	if c == nil {
		return ""
	}
	return c.verdict
}

func TestDriftCheckFlagsAndAPersonAnswers(t *testing.T) {
	de := newDriftEnv(t)

	// No approved goal: nothing is checked.
	early := de.agentPR(1, "Before any goal", nil)
	if why, _ := de.settleCheck(early); why != "" || de.llm.count() != 0 || de.verdict(early.ID) != "" {
		t.Fatalf("no goal: %q, %d calls", why, de.llm.count())
	}
	if _, err := de.s.CreateDesign(de.ctx, de.agent, DesignInput{Title: str("An agent's own goal"), Kind: str("goal"), Project: &de.p.Key}); err != nil {
		t.Fatal(err)
	}
	if why, _ := de.settleCheck(early); why != "" || de.llm.count() != 0 {
		t.Fatalf("only an agent's goal: %q, %d calls", why, de.llm.count())
	}

	const goalTitle = "Everything stays on your own server"
	goal := de.goal(de.admin, de.p.Key, goalTitle)
	task := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Send usage to a hosted service"})

	// A person merging by hand doesn't wait for a model that hasn't looked.
	pr := de.agentPR(2, "Send usage to a hosted service", task)
	if why, _ := de.s.driftHold(de.ctx, pr, false); why != "" || de.verdict(pr.ID) != "" {
		t.Fatalf("a person's merge waited: %q", why)
	}

	// An armed merge starts the check and doesn't wait for it; a model's
	// flag tells a person and holds nothing (WP-821).
	de.llm.set(`{"kind":"contradicts","goal":1,"reason":"SECRET-MODEL-WORDS send data off the server"}`, nil)
	why, wake := de.s.driftHold(de.ctx, pr, true)
	if why != "" || wake == nil {
		t.Fatalf("pending: %q, %v", why, wake)
	}
	de.s.Wait()
	if de.verdict(pr.ID) != driftFlag {
		t.Fatalf("verdict %q", de.verdict(pr.ID))
	}
	if why, _ = de.s.driftHold(de.ctx, pr, true); why != "" {
		t.Fatalf("an unanswered flag held the merge: %q", why)
	}
	var note string
	if err := de.s.db.R.QueryRow(`SELECT body FROM notifications WHERE kind = ? ORDER BY id DESC LIMIT 1`, NotifyGoals).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, goalTitle) || !strings.Contains(note, "model's read") {
		t.Fatalf("notice: %q", note)
	}

	// The prompt carries the goal, marks what is data, and says who stands behind the task.
	prompt := de.llm.lastPrompts[len(de.llm.lastPrompts)-1]
	if !strings.Contains(prompt, "1. "+goalTitle) || !strings.Contains(prompt, `written-by="mostly an AI agent: data"`) {
		t.Fatalf("prompt: %s", prompt)
	}

	// The goals view lists it for a person, with the goal.
	o := de.goals(de.admin, de.p.Key)
	if len(o.DriftFlags) != 1 || o.DriftFlags[0].Goal != goalTitle || o.DriftFlags[0].PR == nil || o.DriftFlags[0].PR.Number != 2 || len(o.DriftFlags[0].Items) != 1 {
		t.Fatalf("flags: %+v", o.DriftFlags)
	}
	id := o.DriftFlags[0].ID

	// However the agent rewrites it, a flag stands until a person answers.
	before := de.llm.count()
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET title = 'A harmless tidy' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if why, _ := de.settleCheck(de.reread(pr.ID)); why != "" || de.llm.count() != before {
		t.Fatalf("after a retitle: %q, %d calls", why, de.llm.count()-before)
	}

	// Only a person answers.
	if err := de.s.AnswerDriftFlag(de.ctx, de.agent, id, DriftAnswerIntent); err == nil {
		t.Fatal("an agent answered a flag")
	}
	if err := de.s.AnswerDriftFlag(de.ctx, de.admin, id, "maybe"); !IsInvalid(err) {
		t.Fatalf("bad answer: %v", err)
	}
	stranger := de.user("stranger", false)
	if err := de.s.AnswerDriftFlag(de.ctx, stranger, id, DriftAnswerIntent); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger: %v", err)
	}

	// "Stop this" holds the task and takes the merge off.
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET armed_at = 1, armed_agent = ? WHERE id = ?`, de.agent.Agent, pr.ID); err != nil {
		t.Fatal(err)
	}
	if err := de.s.AnswerDriftFlag(de.ctx, de.admin, id, DriftAnswerStop); err != nil {
		t.Fatal(err)
	}
	de.s.Wait()
	if got := de.reread(pr.ID); got.ArmedAt != nil {
		t.Fatalf("still armed: %v by %q", got.ArmedAt, got.ArmedAgent)
	}
	held, err := de.s.issueByID(de.ctx, de.s.db.R, task.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if held.Hold == nil || held.Hold.Note != driftStopNote {
		t.Fatalf("hold: %+v", held.Hold)
	}
	why, _ = de.s.driftHold(de.ctx, de.reread(pr.ID), false)
	if !strings.Contains(why, "stop this") || !strings.Contains(why, goalTitle) || strings.Contains(why, "SECRET-MODEL-WORDS") {
		t.Fatalf("the stop names the goal and no model words: %q", why)
	}

	// A person changing their mind lifts that hold and ends the merge's.
	if err := de.s.AnswerDriftFlag(de.ctx, de.admin, id, DriftAnswerIntent); err != nil {
		t.Fatal(err)
	}
	de.s.Wait()
	if held, _ = de.s.issueByID(de.ctx, de.s.db.R, task.ID, false); held.Hold != nil {
		t.Fatalf("hold stayed: %+v", held.Hold)
	}
	if why, _ := de.s.driftHold(de.ctx, de.reread(pr.ID), true); why != "" {
		t.Fatalf("after intent: %q", why)
	}
	if o := de.goals(de.admin, de.p.Key); len(o.DriftFlags) != 0 {
		t.Fatalf("answered still listed: %+v", o.DriftFlags)
	}

	// "Intent" makes the task a person's request in the trace to goals.
	if g := de.grounding(task.Key); g != GroundPersonRequest {
		t.Fatalf("grounding after intent: %q", g)
	}
	_ = goal
}

func TestDriftCheckClearLeavesNoTrace(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "People find any task in a second")
	pr := de.agentPR(3, "Fix a typo", nil)
	if why, _ := de.settleCheck(pr); why != "" || de.verdict(pr.ID) != driftClear {
		t.Fatalf("clear: %q, verdict %q", why, de.verdict(pr.ID))
	}
	// A clear is a guess: nothing shows it. No flag, nothing on the view.
	var n int
	if err := de.s.db.R.QueryRow(`SELECT count(*) FROM goal_drift_flags`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("flags %d, %v", n, err)
	}
	if o := de.goals(de.agent, de.p.Key); len(o.DriftFlags) != 0 {
		t.Fatalf("flags: %+v", o.DriftFlags)
	}
	// The same change isn't asked about twice, and another title is another change.
	calls := de.llm.count()
	if why, _ := de.settleCheck(de.reread(pr.ID)); why != "" || de.llm.count() != calls {
		t.Fatalf("recheck: %q, %d", why, de.llm.count()-calls)
	}
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET title = 'Add billing' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if de.settleCheck(de.reread(pr.ID)); de.llm.count() != calls+1 {
		t.Fatalf("a new title wasn't checked: %d", de.llm.count()-calls)
	}
}

// A person's own pull request, or a stranger's, is not the agents' drift.
func TestDriftCheckOnlyAgents(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "People find any task in a second")
	pr := de.agentPR(4, "Theirs", nil)
	// The repo's Merge automatically setting arms a person's too.
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET agent = '', armed_at = 1, armed_agent = ? WHERE id = ?`, mergeSettingAgent, pr.ID); err != nil {
		t.Fatal(err)
	}
	if why, _ := de.settleCheck(de.reread(pr.ID)); why != "" || de.llm.count() != 0 {
		t.Fatalf("a person's: %q, %d calls", why, de.llm.count())
	}
}

// A flag can come after the merge it no longer holds: it stays listed, says
// so, and a person can still answer it (WP-821).
func TestDriftFlagOnAMergedPullRequestStaysListed(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	pr := de.agentPR(9, "Send usage away", nil)
	de.llm.set(`{"kind":"contradicts","goal":1,"reason":"It sends data off the server."}`, nil)
	de.settleCheck(pr)
	if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	o := de.goals(de.admin, de.p.Key)
	if len(o.DriftFlags) != 1 || o.DriftFlags[0].PR == nil || !o.DriftFlags[0].PR.Merged {
		t.Fatalf("flags: %+v", o.DriftFlags)
	}
	if err := de.s.AnswerDriftFlag(de.ctx, de.admin, o.DriftFlags[0].ID, DriftAnswerIntent); err != nil {
		t.Fatal(err)
	}
	de.s.Wait()
	if o := de.goals(de.admin, de.p.Key); len(o.DriftFlags) != 0 {
		t.Fatalf("answered still listed: %+v", o.DriftFlags)
	}
}

// A model that can't answer lets the merge go: WP-ADR64's rule.
func TestDriftCheckFailsOpen(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "People find any task in a second")
	pr := de.agentPR(5, "Change", nil)
	de.llm.set("", errors.New("rate limited"))
	why, _ := de.s.driftHold(de.ctx, pr, true)
	if why != "" {
		t.Fatalf("waited for the first attempt: %q", why)
	}
	de.s.Wait()
	for range driftAttempts + 2 {
		if de.verdict(pr.ID) != driftPending {
			break
		}
		de.setClock(de.clockNow().Add(5 * time.Minute))
		de.s.driftHold(de.ctx, pr, true)
		de.s.Wait()
	}
	if v := de.verdict(pr.ID); v != driftError {
		t.Fatalf("verdict %q", v)
	}
	if why, _ := de.s.driftHold(de.ctx, pr, true); why != "" {
		t.Fatalf("after giving up: %q", why)
	}

	// A check that outlasts its window lets the merge go too.
	pr2 := de.agentPR(6, "Another change", nil)
	if err := de.s.startDriftCheck(de.ctx, pr2.ID, "x"); err != nil {
		t.Fatal(err)
	}
	de.setClock(de.clockNow().Add(driftWaitFor + time.Second))
	if why, _ := de.s.driftHold(de.ctx, pr2, true); why != "" || de.verdict(pr2.ID) != driftError {
		t.Fatalf("past the window: %q, %q", why, de.verdict(pr2.ID))
	}

	// Answering nonsense is a failure, never a hold.
	pr3 := de.agentPR(7, "A third change", nil)
	de.llm.set(`{"kind":"contradicts","goal":9,"reason":"no such goal"}`, nil)
	de.settleCheck(pr3)
	if c, _ := de.s.prGoalCheckOf(de.ctx, pr3.ID); c == nil || c.verdict == driftFlag || c.attempts == 0 {
		t.Fatalf("bad goal: %+v", c)
	}
}

// settle holds to Waypoint's record of who stands behind the work, not the
// model's say-so.
func TestDriftSettle(t *testing.T) {
	goals := []driftGoal{{id: 1, title: "A"}, {id: 2, title: "B"}}
	person := &driftInput{goals: goals, tasks: []driftTask{{grounding: GroundPersonRequest}}}
	agent := &driftInput{goals: goals, tasks: []driftTask{{grounding: GroundAgentGoal}}, recent: []string{"x"}}
	for _, c := range []struct {
		name string
		in   *driftInput
		a    driftAnswer
		want string // kind, or "err"
	}{
		{"untraced but a person asked", person, driftAnswer{Kind: DriftUntraced, Reason: "r"}, driftNone},
		{"technology but a person asked", person, driftAnswer{Kind: DriftTechnology, Goal: 1, Reason: "r"}, driftNone},
		{"contradicts even a person's", person, driftAnswer{Kind: DriftContradicts, Goal: 1, Reason: "r"}, DriftContradicts},
		{"untraced with no goal named", agent, driftAnswer{Kind: DriftUntraced, Reason: "r"}, DriftUntraced},
		{"untraced with a wild goal", agent, driftAnswer{Kind: DriftUntraced, Goal: 7, Reason: "r"}, DriftUntraced},
		{"contradicts a goal that isn't there", agent, driftAnswer{Kind: DriftContradicts, Goal: 3, Reason: "r"}, "err"},
		{"technology a goal that isn't there", agent, driftAnswer{Kind: DriftTechnology, Reason: "r"}, "err"},
		{"run with no recent work", &driftInput{goals: goals}, driftAnswer{Kind: DriftRun, Goal: 1, Reason: "r"}, "err"},
		{"run", agent, driftAnswer{Kind: DriftRun, Goal: 2, Reason: "r"}, DriftRun},
		{"a flag with no reason", agent, driftAnswer{Kind: DriftContradicts, Goal: 1}, "err"},
		{"an unknown kind", agent, driftAnswer{Kind: "wibble", Goal: 1, Reason: "r"}, "err"},
		{"none", agent, driftAnswer{Kind: driftNone, Goal: 9, Reason: "ignored"}, driftNone},
	} {
		got, err := c.in.settle(c.a)
		switch {
		case c.want == "err" && err == nil:
			t.Errorf("%s: no error, %+v", c.name, got)
		case c.want != "err" && (err != nil || got.Kind != c.want):
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
		if c.name == "untraced with a wild goal" && got.Goal != 0 {
			t.Errorf("%s: goal %d", c.name, got.Goal)
		}
	}
}

// The week's read raises patterns as one list that names the goal.
func TestDriftWeekly(t *testing.T) {
	de := newDriftEnv(t)
	const goalTitle = "Everything stays on your own server"
	de.goal(de.admin, de.p.Key, goalTitle)
	a := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Store files in S3"})
	b := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Send logs to CloudWatch"})
	asked := de.issue(de.admin, IssueInput{Project: de.p.Key, Title: "A person's own task"})
	de.llm.week = `{"flags":[
		{"kind":"run","goal":1,"reason":"Two changes move work onto AWS.","items":["` + a.Key + `","` + b.Key + `","NOPE-1"]},
		{"kind":"untraced","goal":0,"reason":"Only one real item.","items":["` + a.Key + `","GHOST-2"]},
		{"kind":"run","goal":8,"reason":"No such goal.","items":["` + a.Key + `","` + b.Key + `"]},
		{"kind":"untraced","goal":0,"reason":"A person asked for one of these.","items":["` + a.Key + `","` + asked.Key + `"]}
	]}`
	de.s.maybeGoalDrift(de.ctx)
	de.s.Wait()
	o := de.goals(de.admin, de.p.Key)
	if len(o.DriftFlags) != 1 || o.DriftFlags[0].Kind != DriftRun || o.DriftFlags[0].Goal != goalTitle || o.DriftFlags[0].PR != nil || len(o.DriftFlags[0].Items) != 2 {
		t.Fatalf("flags: %+v", o.DriftFlags)
	}
	var title, body string
	if err := de.s.db.R.QueryRow(`SELECT title, body FROM notifications WHERE kind = ? ORDER BY id DESC LIMIT 1`, NotifyGoals).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "drifting") || !strings.Contains(body, goalTitle) || !strings.Contains(body, "Two changes move work onto AWS.") {
		t.Fatalf("notice %q / %q", title, body)
	}

	// A week the model can't read says so.
	de.setClock(de.clockNow().Add(8 * 24 * time.Hour))
	de.llm.set(de.llm.answer, errors.New("down"))
	de.s.maybeGoalDrift(de.ctx)
	de.s.Wait()
	if err := de.s.db.R.QueryRow(`SELECT body FROM notifications WHERE kind = ? ORDER BY id DESC LIMIT 1`, NotifyGoals).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "couldn't run") {
		t.Fatalf("notice after a failed read: %q", body)
	}
}

func driftDiff(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-x\n+y\n", p, p, p, p)
	}
	return b.String()
}

// The check reads the pull request as it is now, and a clear lasts only
// while that stays the same change.
func TestDriftCheckReadsTheChangeAsItIsNow(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "People find any task in a second")
	task := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Tidy the importer"})
	pr := de.agentPR(5, "Tidy the importer", task)
	live := &ForgePR{Number: 5, Title: pr.Title, Author: pr.Author, Body: "LIVE-BODY", HeadSHA: pr.HeadSHA, State: "open"}
	de.fg.prs[mk(de.f, pr.Repo, pr.Number)] = live
	de.fg.diffs[pr.HeadSHA] = driftDiff("importer.go")
	// What the babysitter froze before its first round isn't what is read.
	de.s.saveFootprint(de.ctx, pr.ID, prFootprint{Ship: &footprintShip{footprintAt: footprintAt{SHA: pr.HeadSHA, Paths: []string{"frozen.go"}}, Body: "FROZEN-BODY"}})

	if why, _ := de.settleCheck(pr); why != "" || de.verdict(pr.ID) != driftClear {
		t.Fatalf("clear: %q, verdict %q", why, de.verdict(pr.ID))
	}
	prompt := de.llm.lastPrompts[len(de.llm.lastPrompts)-1]
	if !strings.Contains(prompt, "LIVE-BODY") || !strings.Contains(prompt, "importer.go") || strings.Contains(prompt, "FROZEN") || strings.Contains(prompt, "frozen.go") {
		t.Fatalf("prompt: %s", prompt)
	}

	push := func(sha string, paths ...string) {
		t.Helper()
		de.fg.diffs[sha] = driftDiff(paths...)
		if _, err := de.s.db.W.Exec(`UPDATE pull_requests SET head_sha = ? WHERE id = ?`, sha, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
	calls := de.llm.count()
	// A fix-up in the same files is the same change.
	push("fixup1", "importer.go")
	if why, _ := de.settleCheck(de.reread(pr.ID)); why != "" || de.llm.count() != calls {
		t.Fatalf("a fix-up: %q, %d calls", why, de.llm.count()-calls)
	}
	// A push that reaches a new file isn't.
	push("spread1", "importer.go", "billing.go")
	de.settleCheck(de.reread(pr.ID))
	if de.llm.count() != calls+1 || !strings.Contains(de.llm.lastPrompts[len(de.llm.lastPrompts)-1], "billing.go") {
		t.Fatalf("a new file: %d calls", de.llm.count()-calls)
	}
	// Nor is a rewritten description, or an edited task.
	de.fg.mu.Lock()
	live.Body = "REWRITTEN-BODY"
	de.fg.mu.Unlock()
	de.settleCheck(de.reread(pr.ID))
	if de.llm.count() != calls+2 || !strings.Contains(de.llm.lastPrompts[len(de.llm.lastPrompts)-1], "REWRITTEN-BODY") {
		t.Fatalf("a new description: %d calls", de.llm.count()-calls)
	}
	if _, err := de.s.db.W.Exec(`UPDATE issues SET description = 'Now it adds billing' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	de.settleCheck(de.reread(pr.ID))
	if de.llm.count() != calls+3 {
		t.Fatalf("an edited task: %d calls", de.llm.count()-calls)
	}
}

// An answer about an earlier change doesn't decide a newer check.
func TestDriftCheckDropsAStaleAnswer(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "People find any task in a second")
	pr := de.agentPR(6, "Fix a typo", nil)
	de.llm.during = func() {
		if _, err := de.s.db.W.Exec(`UPDATE pr_goal_checks SET input_hash = 'newer' WHERE pr_id = ?`, pr.ID); err != nil {
			t.Error(err)
		}
	}
	de.s.driftHold(de.ctx, pr, true)
	de.s.Wait()
	if v := de.verdict(pr.ID); v != driftPending {
		t.Fatalf("the newer check got the old answer: %q", v)
	}
}

// Not being able to read a hold isn't the same as there being none.
func TestDriftHoldStandsWhenItCantBeRead(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	pr := de.agentPR(7, "Send usage away", nil)
	de.llm.set(`{"kind":"contradicts","goal":1,"reason":"It sends data off the server."}`, nil)
	de.settleCheck(pr)
	if de.verdict(pr.ID) != driftFlag {
		t.Fatalf("verdict %q", de.verdict(pr.ID))
	}
	c, err := de.s.prGoalCheckOf(de.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(de.ctx)
	cancel()
	if why, wake := de.s.driftHold(gone, pr, false); why != driftUnread || wake == nil {
		t.Fatalf("unread check: %q, %v", why, wake)
	}
	if why, err := de.s.driftFlagHold(gone, c.flagID); why != "" || err == nil {
		t.Fatalf("unread flag: %q, %v", why, err)
	}
	if why, err := de.s.driftFlagHold(de.ctx, c.flagID+99); why != "" || err != nil {
		t.Fatalf("a flag that's gone: %q, %v", why, err)
	}
}

func (de *driftEnv) holdNote(id int64) string {
	de.t.Helper()
	i, err := de.s.issueByID(de.ctx, de.s.db.R, id, false)
	if err != nil {
		de.t.Fatal(err)
	}
	if i.Hold == nil {
		return ""
	}
	return i.Hold.Note
}

// A weekly "stop" stays answerable, and each flag's holds are its own.
func TestDriftStopHolds(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	a := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Store files in S3"})
	b := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Send logs to CloudWatch"})
	c := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Move queues to SQS"})
	if _, err := de.s.db.W.Exec(`UPDATE issues SET hold_at = 1, hold_note = 'Waiting on legal', hold_person = 1 WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	de.llm.week = `{"flags":[
		{"kind":"run","goal":1,"reason":"first","items":["` + a.Key + `","` + b.Key + `"]},
		{"kind":"run","goal":1,"reason":"second","items":["` + a.Key + `","` + b.Key + `"]},
		{"kind":"run","goal":1,"reason":"third","items":["` + a.Key + `","` + c.Key + `"]}
	]}`
	if note := de.s.driftWeekly(de.ctx, de.p.ID); note != "" {
		t.Fatalf("note %q", note)
	}
	listed := func() map[string]DriftFlag {
		out := map[string]DriftFlag{}
		for _, f := range de.goals(de.admin, de.p.Key).DriftFlags {
			out[f.Reason] = f
		}
		return out
	}
	flags := listed()
	answer := func(reason, to string) {
		t.Helper()
		if err := de.s.AnswerDriftFlag(de.ctx, de.admin, flags[reason].ID, to); err != nil {
			t.Fatal(err)
		}
		de.s.Wait()
	}
	answer("first", DriftAnswerStop)
	answer("second", DriftAnswerStop)
	if f, ok := listed()["first"]; !ok || f.Answer != DriftAnswerStop {
		t.Fatalf("a weekly stop left the view: %+v", listed())
	}

	// One flag changing its answer doesn't release what another still stops.
	answer("first", DriftAnswerIntent)
	if n := de.holdNote(a.ID); n != driftStopNote {
		t.Fatalf("after one intent: %q", n)
	}
	if _, ok := listed()["first"]; ok {
		t.Fatal("an answered flag is still listed")
	}
	answer("second", DriftAnswerIntent)
	if n := de.holdNote(a.ID); n != "" {
		t.Fatalf("after both: %q", n)
	}

	// A person's own hold is neither replaced nor lifted.
	answer("third", DriftAnswerStop)
	if n, m := de.holdNote(c.ID), de.holdNote(a.ID); n != "Waiting on legal" || m != driftStopNote {
		t.Fatalf("after stop: %q, %q", n, m)
	}
	answer("third", DriftAnswerIntent)
	if n, m := de.holdNote(c.ID), de.holdNote(a.ID); n != "Waiting on legal" || m != "" {
		t.Fatalf("after intent: %q, %q", n, m)
	}
}

// However many weekly flags pile up, one holding a merge can be answered.
func TestDriftFlagHoldingAMergeIsAlwaysListed(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	pr := de.agentPR(8, "Send usage away", nil)
	de.llm.set(`{"kind":"contradicts","goal":1,"reason":"It sends data off the server."}`, nil)
	de.settleCheck(pr)
	for n := range driftFlagsShown {
		if _, err := de.s.db.W.Exec(`INSERT INTO goal_drift_flags(project_id, kind, goal_title, reason, model, created_at) VALUES (?,?,?,?,?,?)`,
			de.p.ID, DriftRun, "g", fmt.Sprintf("weekly %d", n), "m", 1); err != nil {
			t.Fatal(err)
		}
	}
	o := de.goals(de.admin, de.p.Key)
	if len(o.DriftFlags) != driftFlagsShown || o.DriftFlags[0].PR == nil || o.DriftFlags[0].PR.Number != 8 {
		t.Fatalf("%d flags, first %+v", len(o.DriftFlags), o.DriftFlags[0])
	}
}

// Open tasks don't crowd the week's finished work out, and a week too big
// to read says so.
func TestDriftWeekReadsFinishedWorkFirst(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	done := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Finished this week"})
	if _, err := de.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE category = 'done' LIMIT 1), resolved_at = ? WHERE id = ?`, de.s.nowMS(), done.ID); err != nil {
		t.Fatal(err)
	}
	de.setClock(de.clockNow().Add(time.Hour))
	for n := range driftWeekItems {
		de.issue(de.agent, IssueInput{Project: de.p.Key, Title: fmt.Sprintf("Open %d", n)})
	}
	de.s.Wait()
	note := de.s.driftWeekly(de.ctx, de.p.ID)
	if !strings.Contains(note, "stopped at") {
		t.Fatalf("note %q", note)
	}
	if prompt := de.llm.lastPrompts[len(de.llm.lastPrompts)-1]; !strings.Contains(prompt, done.Key+" ") {
		t.Fatalf("the finished task wasn't read: %s", prompt)
	}
}

// A model that copies the whole line it was shown still names its tasks.
func TestDriftWeekTakesCopiedLines(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	a := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Store files in S3"})
	b := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Send logs to CloudWatch"})
	de.llm.week = `{"flags":[{"kind":"run","goal":1,"reason":"They move work onto AWS.","items":["` + a.Key + ` Store files in S3 (traces to no goal)","` + strings.ToLower(b.Key) + `"]}]}`
	de.s.driftWeekly(de.ctx, de.p.ID)
	if o := de.goals(de.admin, de.p.Key); len(o.DriftFlags) != 1 || len(o.DriftFlags[0].Items) != 2 {
		t.Fatalf("flags: %+v", o.DriftFlags)
	}
}

// "Record as intent" sticks on a task an agent had linked to a goal.
func TestDriftIntentSticksOnALinkedTask(t *testing.T) {
	de := newDriftEnv(t)
	goal := de.goal(de.admin, de.p.Key, "People find any task in a second")
	task := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Move search to a hosted engine"})
	de.serve(de.agent, task.Key, goal.ID)
	de.llm.set(`{"kind":"technology","goal":1,"reason":"It moves to a hosted engine for its own sake."}`, nil)
	first := de.agentPR(9, "Move search", task)
	de.settleCheck(first)
	if de.verdict(first.ID) != driftFlag {
		t.Fatalf("verdict %q", de.verdict(first.ID))
	}
	o := de.goals(de.admin, de.p.Key)
	if err := de.s.AnswerDriftFlag(de.ctx, de.admin, o.DriftFlags[0].ID, DriftAnswerIntent); err != nil {
		t.Fatal(err)
	}
	de.s.Wait()
	if g := de.grounding(task.Key); g != GroundAgentGoal {
		t.Fatalf("grounding %q", g)
	}
	second := de.agentPR(10, "Move search, part two", task)
	if why, _ := de.settleCheck(second); why != "" || de.verdict(second.ID) != driftClear {
		t.Fatalf("after intent: %q, verdict %q", why, de.verdict(second.ID))
	}
}

// brokenPRForge can't give a pull request's description.
type brokenPRForge struct{ *prForge }

func (brokenPRForge) GetPR(ctx context.Context, f *Forge, repo string, n int) (*ForgePR, error) {
	return nil, errors.New("the forge is down")
}

// A failed read is not "nothing to report": the week says it couldn't run,
// and a repo's lookup tells a failure from a repo attached to nothing.
func TestDriftFailedReadsAreNotNothing(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	gone, cancel := context.WithCancel(de.ctx)
	cancel()
	if note := de.s.driftWeekly(gone, de.p.ID); !strings.Contains(note, "couldn't run") {
		t.Fatalf("a failed read of the goals: %q", note)
	}
	if n, err := de.s.driftUnchecked(gone, de.p.ID); n != 0 || err == nil {
		t.Fatalf("a failed count: %d, %v", n, err)
	}
	if n, err := de.s.driftUnchecked(de.ctx, de.p.ID); n != 0 || err != nil {
		t.Fatalf("nothing unchecked: %d, %v", n, err)
	}
	if id, err := de.s.attachedProjectRead(gone, de.s.db.R, de.f.ID, de.repo.Repo); id != 0 || err == nil {
		t.Fatalf("a failed lookup: %d, %v", id, err)
	}
	if id, err := de.s.attachedProjectRead(de.ctx, de.s.db.R, de.f.ID, "nobody/nothing"); id != 0 || err != nil {
		t.Fatalf("a repo attached to nothing: %d, %v", id, err)
	}
	if id, err := de.s.attachedProjectRead(de.ctx, de.s.db.R, de.f.ID, de.repo.Repo); id != de.p.ID || err != nil {
		t.Fatalf("an attached repo: %d, %v", id, err)
	}
}

// The model doesn't rule on a change whose description couldn't be read: the
// check stays pending and the next look reads it.
func TestDriftWaitsForAnUnreadDescription(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	pr := de.agentPR(11, "Send usage away", nil)
	de.s.Forges = brokenPRForge{de.fg}
	de.s.driftHold(de.ctx, pr, true)
	de.s.Wait()
	if de.llm.count() != 0 || de.verdict(pr.ID) != driftPending {
		t.Fatalf("%d model calls, verdict %q", de.llm.count(), de.verdict(pr.ID))
	}
	de.s.Forges = de.fg
	if err := de.s.runDriftCheck(de.ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if de.llm.count() != 1 || de.verdict(pr.ID) != driftClear {
		t.Fatalf("%d model calls, verdict %q", de.llm.count(), de.verdict(pr.ID))
	}
}

// The week's limit counts flags recorded, so one dropped doesn't crowd out
// a good one.
func TestDriftWeekCapCountsRecordedFlags(t *testing.T) {
	de := newDriftEnv(t)
	de.goal(de.admin, de.p.Key, "Everything stays on your own server")
	a := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Store files in S3"})
	b := de.issue(de.agent, IssueInput{Project: de.p.Key, Title: "Send logs to CloudWatch"})
	flag := func(reason string, items ...string) string {
		return `{"kind":"run","goal":1,"reason":"` + reason + `","items":["` + strings.Join(items, `","`) + `"]}`
	}
	both := []string{a.Key, b.Key}
	de.llm.week = `{"flags":[` + strings.Join([]string{
		flag("dropped", a.Key), flag("one", both...), flag("two", both...), flag("three", both...), flag("four", both...),
	}, ",") + `]}`
	if note := de.s.driftWeekly(de.ctx, de.p.ID); note != "" {
		t.Fatalf("note %q", note)
	}
	got := map[string]bool{}
	for _, f := range de.goals(de.admin, de.p.Key).DriftFlags {
		got[f.Reason] = true
	}
	if len(got) != driftWeekFlags || !got["one"] || !got["two"] || !got["three"] {
		t.Fatalf("recorded: %v", got)
	}
}
