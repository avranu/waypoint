package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// sha is a distinct 40-hex commit id for round n.
func sha(n int) string { return fmt.Sprintf("%040x", n+0x1000) }

// noBetter is a round of work by the agent that the reading finds no nearer:
// the check keeps failing.
func (o *objEnv) noBetter(key string, n int) {
	o.t.Helper()
	o.worked(key)
	o.read(ObjectiveJobResult{Head: sha(n), Base: sha(n - 1), Reading: passed(false)})
}

// toDiagnose sets a pass/fail objective and runs rounds that come no nearer
// until the next round is a diagnose round (or n rounds, if after is 0).
func (o *objEnv) toDiagnose(after, rounds int, in ObjectiveInput) *IssueDetail {
	o.t.Helper()
	task := o.task("Lint clean")
	in.Check, in.Until = ptrOf("make lint"), ptrOf("pass")
	if after >= 0 && in.DiagnoseAfter == nil {
		in.DiagnoseAfter = ptrOf(after)
	}
	o.set(task.Key, in)
	o.read(ObjectiveJobResult{Head: sha(0), Reading: passed(false)})
	for i := 1; i <= rounds; i++ {
		o.noBetter(task.Key, i)
	}
	return task
}

func (o *objEnv) diagnose(key string, in ObjectiveDiagnoseInput) (*Objective, error) {
	o.t.Helper()
	ob, err := o.s.ObjectiveDiagnose(o.ctx, o.ag, key, in)
	o.s.Wait()
	return ob, err
}

func TestObjectiveDiagnoseCounter(t *testing.T) {
	for _, c := range []struct {
		name  string
		after int
		steps []string // outcomes of the rounds, in order
		kind  string   // round_kind of the round that opens after them
		count int      // no_better after them
	}{
		{"default two no better", 2, []string{"none", "none"}, RoundKindDiagnose, 2},
		{"one no better is not enough", 2, []string{"none"}, "", 1},
		{"after 1 diagnoses at once", 1, []string{"none"}, RoundKindDiagnose, 1},
		{"after 3 needs three", 3, []string{"none", "none"}, "", 2},
		{"after 3 diagnoses at three", 3, []string{"none", "none", "none"}, RoundKindDiagnose, 3},
		{"progress zeroes it", 2, []string{"none", "progress", "none"}, "", 1},
		{"empty round neither counts nor clears", 2, []string{"none", "empty", "none"}, RoundKindDiagnose, 2},
		{"never at 0", 0, []string{"none", "none", "none"}, "", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := newObjEnv(t)
			task := o.task("Coverage")
			o.set(task.Key, ObjectiveInput{Check: ptrOf("./coverage.sh"), Until: ptrOf(">= 99%"), DiagnoseAfter: ptrOf(c.after)})
			val := func(v float64) *ObjectiveReading { return &ObjectiveReading{OK: true, Value: ptrOf(v), Runs: 3} }
			o.read(ObjectiveJobResult{Head: sha(0), Reading: val(50)})
			at, base := 50.0, 0
			for i, step := range c.steps {
				n := i + 1
				o.setClock(o.clockNow().Add(100 * time.Hour))
				switch step {
				case "empty":
					// Taken, finished with nothing landed: owed again, counted by no one.
					if _, err := o.s.AgentTake(o.ctx, o.ag, task.Key, ""); err != nil {
						t.Fatal(err)
					}
					if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); err != nil {
						t.Fatal(err)
					}
					o.s.Wait()
					o.read(ObjectiveJobResult{Head: sha(base), Base: sha(base), Reading: val(at), BaseReading: val(at), Exact: true})
					continue
				case "progress":
					o.worked(task.Key)
					o.read(ObjectiveJobResult{Head: sha(n), Base: sha(base), Reading: val(at + 10), BaseReading: val(at), Exact: true})
					at += 10
				default:
					o.worked(task.Key)
					o.read(ObjectiveJobResult{Head: sha(n), Base: sha(base), Reading: val(at), BaseReading: val(at), Exact: true})
				}
				base = n
			}
			ob := o.obj(task.Key)
			if ob.RoundKind != c.kind || ob.NoBetter != c.count {
				t.Fatalf("after %v: kind %q no_better %d, want %q %d (%s)", c.steps, ob.RoundKind, ob.NoBetter, c.kind, c.count, ob.Summary)
			}
		})
	}
}

func TestObjectiveDiagnoseValidatesTheSetting(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	if ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")}); ob.DiagnoseAfter != 2 {
		t.Fatalf("default = %d", ob.DiagnoseAfter)
	}
	for _, bad := range []int{-1, maxObjectiveDiagnose + 1} {
		if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(bad)}); !IsInvalid(err) {
			t.Errorf("diagnose_after %d: %v", bad, err)
		}
	}
	if ob := o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(maxObjectiveDiagnose)}); ob.DiagnoseAfter != maxObjectiveDiagnose {
		t.Errorf("at the cap: %d", ob.DiagnoseAfter)
	}
}

func TestObjectiveDiagnoseRoundBriefAndRecording(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	ob := o.obj(task.Key)
	if ob.RoundKind != RoundKindDiagnose || ob.Round != 3 || ob.Phase != PhaseWork {
		t.Fatalf("round %d kind %q phase %q", ob.Round, ob.RoundKind, ob.Phase)
	}
	brief := ObjectiveText(ob, true, o.clockNow())
	hasAll(t, "the diagnose brief", brief, "ships no code", "wp objective diagnose "+task.Key, "the check or the judge itself", "noise in the reading", "out of reach", "next round")
	if people := ObjectiveText(ob, false, o.clockNow()); !strings.Contains(people, "diagnose round") {
		t.Errorf("person's text lacks the diagnose round:\n%s", people)
	}

	// Not taken up: an agent can't record it yet.
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "x"}); !IsInvalid(err) || !strings.Contains(err.Error(), "hasn't been taken up by you") {
		t.Fatalf("not taken: %v", err)
	}
	o.take1(task.Key)
	// The round's text is validated, not truncated.
	for name, text := range map[string]string{"empty": "  \n ", "long": strings.Repeat("x", maxObjectiveDiagnosis+1)} {
		if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: text}); !IsInvalid(err) {
			t.Errorf("%s text: %v", name, err)
		}
	}
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: strings.Repeat("x", maxObjectiveDiagnosis)}); err != nil {
		t.Fatalf("text at the cap: %v", err)
	}
}

func TestObjectiveDiagnoseRecordsAndReopensTheSameRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	text := "  The check times the build, not the search.\n\nTry: --check ./bench-search.  "
	ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if ob.Round != 3 || ob.RoundKind != "" || ob.NoBetter != 0 || ob.Phase != PhaseWork || ob.Stalled != 2 {
		t.Fatalf("after diagnosing: round %d kind %q no_better %d phase %q stalled %d", ob.Round, ob.RoundKind, ob.NoBetter, ob.Phase, ob.Stalled)
	}
	if ob.Diagnosis == nil || ob.Diagnosis.Round != 3 || ob.Diagnosis.By != "anthropic/claude-opus-5-5" || ob.Diagnosis.Text != strings.TrimSpace(text) {
		t.Fatalf("diagnosis = %+v", ob.Diagnosis)
	}
	last := ob.Rounds[len(ob.Rounds)-1]
	if last.Round != 3 || last.Outcome != RoundDiagnosed || last.Diagnosis != strings.TrimSpace(text) || last.DiagnosisBy == "" {
		t.Fatalf("round row = %+v", last)
	}
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("status = %s", got)
	}
	// The claim is released: it can be taken again, and by anyone (no backoff:
	// waits only the objective's own pace, which for a target is none).
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("the reopened round isn't handed out: %q", wait)
	}
	// Every later round's brief reads it.
	hasAll(t, "the next brief", ObjectiveText(ob, true, o.clockNow()), "latest diagnosis", "times the build", "anthropic/claude-opus-5-5")
	// It can't be recorded twice: the round it reopened is a work round, and
	// the refusal says so. The first diagnosis is left as it was.
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "again"}); !IsInvalid(err) || !strings.Contains(err.Error(), "is a work round, not a diagnose round") {
		t.Fatalf("recording twice: %v", err)
	}
	if again := o.obj(task.Key); again.Diagnosis == nil || again.Diagnosis.Text != strings.TrimSpace(text) || len(again.Rounds) != len(ob.Rounds) {
		t.Fatalf("a refused second diagnosis changed it: %+v (%d rounds, was %d)", again.Diagnosis, len(again.Rounds), len(ob.Rounds))
	}
	// A met reading clears it.
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	o.read(ObjectiveJobResult{Head: sha(3), Base: sha(2), Reading: passed(true)})
	if ob = o.obj(task.Key); ob.State != ObjectiveMet || ob.Diagnosis != nil {
		t.Fatalf("state %s diagnosis %+v", ob.State, ob.Diagnosis)
	}
}

// A round that reads nearer, though not met, clears the diagnosis: later
// briefs no longer read it (WP-1449).
func TestObjectiveDiagnoseProgressClearsIt(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Coverage")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./coverage.sh"), Until: ptrOf(">= 99%"), DiagnoseAfter: ptrOf(1)})
	val := func(v float64) *ObjectiveReading { return &ObjectiveReading{OK: true, Value: ptrOf(v), Runs: 3} }
	o.read(ObjectiveJobResult{Head: sha(0), Reading: val(50)})
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: sha(1), Base: sha(0), Reading: val(50), BaseReading: val(50), Exact: true})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil {
		t.Fatal(err)
	}
	ob := o.obj(task.Key)
	if ob.Diagnosis == nil || !strings.Contains(ObjectiveText(ob, true, o.clockNow()), "times the build") {
		t.Fatalf("setup: diagnosis %+v", ob.Diagnosis)
	}
	o.setClock(o.clockNow().Add(100 * time.Hour))
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: sha(2), Base: sha(1), Reading: val(60), BaseReading: val(50), Exact: true})
	ob = o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Diagnosis != nil || ob.NoBetter != 0 {
		t.Fatalf("after progress: state %s diagnosis %+v no_better %d", ob.State, ob.Diagnosis, ob.NoBetter)
	}
	if brief := ObjectiveText(ob, true, o.clockNow()); strings.Contains(brief, "times the build") || strings.Contains(brief, "latest diagnosis") {
		t.Errorf("the brief still reads the diagnosis after progress:\n%s", brief)
	}
}

// A diagnosis ends its run, but not as "done": a workflow step's
// same/different-agent rule reads the latest done run as whoever did the
// step before, and a diagnosis finishes no step.
func TestObjectiveDiagnoseRunIsNotAStepDone(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	// Another agent finished the step before.
	if _, err := o.s.db.W.Exec(`INSERT INTO agent_runs(issue_id, agent, started_at, ended_at, outcome) VALUES (?, 'anthropic/claude-sonnet-5-5', ?, ?, 'done')`,
		task.ID, o.clockNow().UnixMilli(), o.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check is wrong"}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := o.s.db.R.QueryRow(`SELECT outcome FROM agent_runs WHERE issue_id = ? AND agent = 'anthropic/claude-opus-5-5' AND ended_at IS NOT NULL ORDER BY id DESC LIMIT 1`, task.ID).Scan(&outcome); err != nil || outcome != RunDiagnosed {
		t.Fatalf("the diagnosis run ended %q (%v), want %q", outcome, err, RunDiagnosed)
	}
	var prev string
	if err := o.s.db.R.QueryRow(`SELECT agent FROM agent_runs WHERE issue_id = ? AND outcome = 'done' ORDER BY id DESC LIMIT 1`, task.ID).Scan(&prev); err != nil || prev != "anthropic/claude-sonnet-5-5" {
		t.Fatalf("the step before was done by %q (%v), want the agent that did it", prev, err)
	}
}

func TestObjectiveDiagnoseDoesNotUseUpRounds(t *testing.T) {
	// MaxRounds 3, diagnose after 2: rounds 1 and 2 come no nearer, round 3 is a
	// diagnose round, which reopens round 3 as a work round, with the last round still to use.
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{MaxRounds: ptrOf(3)})
	o.take1(task.Key)
	ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check is wrong"})
	if err != nil || ob.Round != 3 || ob.RoundKind != "" || ob.Phase != PhaseWork {
		t.Fatalf("diagnosed at MaxRounds-0: %+v, %v", ob, err)
	}
	// A work round 3 that comes no nearer uses the last round: out of rounds, no diagnose opens.
	o.noBetter(task.Key, 3)
	ob = o.obj(task.Key)
	if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedRounds || ob.RoundKind != "" {
		t.Fatalf("out of rounds: phase %q why %q kind %q", ob.Phase, ob.PhaseWhy, ob.RoundKind)
	}
}

func TestObjectiveDiagnoseNeverOpensOutOfRounds(t *testing.T) {
	// MaxRounds 2 and diagnose after 2: after round 2 it is out of rounds, not a diagnose round.
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{MaxRounds: ptrOf(2)})
	ob := o.obj(task.Key)
	if ob.PhaseWhy != BlockedRounds || ob.RoundKind != "" {
		t.Fatalf("phase %q why %q kind %q", ob.Phase, ob.PhaseWhy, ob.RoundKind)
	}
}

func TestObjectiveDiagnoseAccess(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)

	// Another agent, with no claim.
	tok, _, err := o.s.CreateToken(o.ctx, o.admin, TokenSpec{Name: "other", Agent: "anthropic/claude-sonnet-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := o.s.TokenActor(o.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.ObjectiveDiagnose(o.ctx, other, task.Key, ObjectiveDiagnoseInput{Text: "mine"}); err == nil || !strings.Contains(err.Error(), "is claimed by anthropic/claude-opus-5-5") {
		t.Fatalf("another agent recorded a diagnosis, or was refused for another reason: %v", err)
	}
	// A viewer.
	viewer := o.user("vera", false)
	if err := o.s.SetMember(o.ctx, o.admin, o.p.Key, "vera", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.ObjectiveDiagnose(o.ctx, viewer, task.Key, ObjectiveDiagnoseInput{Text: "mine"}); err == nil || err.Error() != ErrForbidden.Error() {
		t.Fatalf("a viewer's refusal: %v, want %v", err, ErrForbidden)
	}
	if ob := o.obj(task.Key); ob.Diagnosis != nil || ob.RoundKind != RoundKindDiagnose {
		t.Fatalf("a refused diagnosis changed it: %+v", ob)
	}
	// A person who may edit the task can, without taking it.
	if _, err := o.s.ObjectiveDiagnose(o.ctx, o.admin, task.Key, ObjectiveDiagnoseInput{Text: "the person's own"}); err != nil {
		t.Fatalf("person: %v", err)
	}
}

func TestObjectiveDiagnoseWorkRoundRefuses(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 1, ObjectiveInput{})
	o.take1(task.Key)
	_, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "x"})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "work round") {
		t.Fatalf("work round: %v", err)
	}
}

// The other ways a diagnose round ends (WP-1449 item 6).

func TestObjectiveDiagnoseRefusesWpDone(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	_, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "wp objective diagnose") {
		t.Fatalf("wp done in a diagnose round: %v", err)
	}
	if ob := o.obj(task.Key); ob.RoundKind != RoundKindDiagnose || ob.Phase != PhaseWork {
		t.Fatalf("the round moved: %q %q", ob.RoundKind, ob.Phase)
	}
}

func TestObjectiveDiagnoseMergedPullRequestIsANormalRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	url := o.pull(task.Key, 7)
	o.linkState(url, "merged")
	o.read(ObjectiveJobResult{Head: sha(3), Base: sha(2), Reading: passed(false)})
	ob := o.obj(task.Key)
	// Read as a normal round, no better: a diagnosis is still owed.
	if ob.Round != 4 || ob.RoundKind != RoundKindDiagnose || ob.NoBetter != 3 {
		t.Fatalf("after the merge: round %d kind %q no_better %d", ob.Round, ob.RoundKind, ob.NoBetter)
	}
	found := false
	for _, h := range o.history(task.Key) {
		if strings.Contains(h, "a diagnose round, but a pull request landed in it") {
			found = true
		}
	}
	if !found {
		t.Errorf("no event notes that a diagnose round was read as a normal one: %q", o.history(task.Key))
	}
}

func TestObjectiveDiagnoseGoActions(t *testing.T) {
	// A person's next skips the diagnosis; retry and end keep their meaning.
	t.Run("next skips it", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext)
		if err != nil || ob.RoundKind != "" || ob.NoBetter != 0 || ob.Round != 3 || ob.NextAt != nil {
			t.Fatalf("next: %+v, %v", ob, err)
		}
	})
	t.Run("retry has nothing to read", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry); !IsInvalid(err) {
			t.Fatalf("retry: %v", err)
		}
		if ob := o.obj(task.Key); ob.RoundKind != RoundKindDiagnose {
			t.Fatal("retry changed the round")
		}
	})
	t.Run("end ends it", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveEnd)
		if err != nil || ob.State != ObjectiveEnded {
			t.Fatalf("end: %+v, %v", ob, err)
		}
	})
	t.Run("a person closing the task ends the objective", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		done := "Done"
		if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Status: &done}); err != nil {
			t.Fatal(err)
		}
		o.s.Wait()
		if ob := o.obj(task.Key); ob.State != ObjectiveEnded {
			t.Fatalf("state %s", ob.State)
		}
	})
}

func TestObjectiveDiagnoseResetAndSettingZero(t *testing.T) {
	t.Run("a re-set that changes the check makes a work round", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all")})
		if ob.RoundKind != "" || ob.NoBetter != 0 || ob.Stalled != 0 {
			t.Fatalf("after re-set: kind %q no_better %d stalled %d", ob.RoundKind, ob.NoBetter, ob.Stalled)
		}
	})
	t.Run("diagnose_after 0 makes it a work round", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		ob := o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(0)})
		if ob.RoundKind != "" {
			t.Fatalf("kind %q", ob.RoundKind)
		}
	})
	t.Run("a larger diagnose_after makes it a work round", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		if ob := o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(5)}); ob.RoundKind != "" {
			t.Fatalf("kind %q", ob.RoundKind)
		}
	})
}

func TestObjectiveDiagnoseHandBackKeepsTheRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.s.AgentRelease(o.ctx, o.ag, task.Key, ReleaseInput{}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	ob := o.obj(task.Key)
	if ob.RoundKind != RoundKindDiagnose || ob.Round != 3 || ob.Phase != PhaseWork {
		t.Fatalf("after a release: round %d kind %q phase %q", ob.Round, ob.RoundKind, ob.Phase)
	}
	if err := o.s.objectiveHandBack(o.ctx, o.issueID(task.Key), "test", 0); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	if ob = o.obj(task.Key); ob.RoundKind != RoundKindDiagnose {
		t.Fatalf("after a hand-back: kind %q", ob.RoundKind)
	}
	// A release backs the round off like any other; taken by name, it is still the diagnose round.
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the next taker's"}); err != nil {
		t.Fatalf("the next taker can't record it: %v", err)
	}
}

func TestObjectiveDiagnoseAsk(t *testing.T) {
	ask := func(t *testing.T) (*objEnv, *IssueDetail) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the target is out of reach", Ask: true})
		if err != nil || ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedDiagnosis || ob.Diagnosis == nil {
			t.Fatalf("ask: %+v, %v", ob, err)
		}
		if told := o.told(NotifyObjective); len(told) == 0 || !strings.Contains(told[0], "needs you") {
			t.Errorf("told = %q", told)
		}
		return o, task
	}
	t.Run("accept goes on with the diagnosis", func(t *testing.T) {
		o, task := ask(t)
		ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveAccept)
		if err != nil || ob.Phase != PhaseWork || ob.Round != 3 || ob.Diagnosis == nil || ob.RoundKind != "" {
			t.Fatalf("accept: %+v, %v", ob, err)
		}
	})
	t.Run("next hands the round out now", func(t *testing.T) {
		o, task := ask(t)
		ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext)
		if err != nil || ob.Phase != PhaseWork || ob.NextAt != nil || ob.RoundKind != "" {
			t.Fatalf("next: %+v, %v", ob, err)
		}
		if next, wait := o.queued(task.Key); !next {
			t.Fatalf("not handed out: %q", wait)
		}
	})
	t.Run("retry has nothing to read", func(t *testing.T) {
		o, task := ask(t)
		if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry); !IsInvalid(err) {
			t.Fatalf("retry: %v", err)
		}
	})
	t.Run("end ends it", func(t *testing.T) {
		o, task := ask(t)
		if ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveEnd); err != nil || ob.State != ObjectiveEnded {
			t.Fatalf("end: %+v, %v", ob, err)
		}
	})
	t.Run("set that changes the check unblocks it", func(t *testing.T) {
		o, task := ask(t)
		ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all")})
		if ob.Phase == PhaseBlocked {
			t.Fatalf("still blocked: %q", ob.PhaseWhy)
		}
	})
	t.Run("set of the rounds alone leaves it waiting", func(t *testing.T) {
		o, task := ask(t)
		ob := o.set(task.Key, ObjectiveInput{MaxRounds: ptrOf(9)})
		if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedDiagnosis {
			t.Fatalf("phase %q why %q", ob.Phase, ob.PhaseWhy)
		}
	})
}

// Who may not record a diagnosis, and when no round can be diagnosed
// (WP-1449): each refusal says why.

func TestObjectiveDiagnoseRefusals(t *testing.T) {
	refused := func(t *testing.T, o *objEnv, key, want string) {
		t.Helper()
		_, err := o.diagnose(key, ObjectiveDiagnoseInput{Text: "what I found"})
		if !IsInvalid(err) || !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal = %v, want it to say %q", err, want)
		}
	}
	t.Run("an agent with no claim at all", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		refused(t, o, task.Key, "hasn't been taken up by you")
	})
	t.Run("a task no one has taken refuses even a second agent", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		tok, _, err := o.s.CreateToken(o.ctx, o.admin, TokenSpec{Name: "other", Agent: "anthropic/claude-sonnet-5-5"})
		if err != nil {
			t.Fatal(err)
		}
		other, err := o.s.TokenActor(o.ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.s.ObjectiveDiagnose(o.ctx, other, task.Key, ObjectiveDiagnoseInput{Text: "x"}); !IsInvalid(err) || !strings.Contains(err.Error(), "hasn't been taken up by you") {
			t.Fatalf("second agent: %v", err)
		}
	})
	t.Run("an actor from another project", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		bob := o.user("bob", false)
		o.project(bob, "Elsewhere", "")
		_, err := o.s.ObjectiveDiagnose(o.ctx, bob, task.Key, ObjectiveDiagnoseInput{Text: "x"})
		if !IsForbiddenOrNotFound(err) {
			t.Fatalf("another project's actor: %v", err)
		}
		if ob := o.obj(task.Key); ob.Diagnosis != nil || ob.RoundKind != RoundKindDiagnose {
			t.Fatalf("a refused diagnosis changed it: %+v", ob)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "first", Ask: true}); err != nil {
			t.Fatal(err)
		}
		refused(t, o, task.Key, "needs a person")
	})
	t.Run("in the baseline phase", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.task("Lint clean")
		o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass"), DiagnoseAfter: ptrOf(1)})
		if ob := o.obj(task.Key); ob.Phase != PhaseBaseline && ob.Phase != PhaseVerdict {
			t.Fatalf("phase %q, want a reading under way", ob.Phase)
		}
		refused(t, o, task.Key, "Waypoint is reading it")
	})
	t.Run("in the verdict phase", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 1, ObjectiveInput{})
		o.take1(task.Key)
		o.setClock(o.clockNow().Add(time.Minute))
		if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); err != nil {
			t.Fatal(err)
		}
		o.s.Wait()
		if ob := o.obj(task.Key); ob.Phase == PhaseWork || ob.Phase == PhaseBlocked {
			t.Fatalf("phase %q, want a reading under way", ob.Phase)
		}
		refused(t, o, task.Key, "Waypoint is reading it")
	})
	t.Run("after the objective ended", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveEnd); err != nil {
			t.Fatal(err)
		}
		refused(t, o, task.Key, "there is no round to diagnose")
	})
	t.Run("a workflow with no To Do status ends the objective", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE statuses SET category = 'backlog' WHERE project_id = ? AND category = 'todo'`, o.p.ID); err != nil {
			t.Fatal(err)
		}
		ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the target is out of reach"})
		if err != nil {
			t.Fatal(err)
		}
		last := ob.Rounds[len(ob.Rounds)-1]
		if ob.State != ObjectiveEnded || !strings.Contains(ob.EndedWhy, "no To Do status") || last.Outcome != RoundDiagnosed || last.Diagnosis != "the target is out of reach" {
			t.Fatalf("state %q why %q last round %+v", ob.State, ob.EndedWhy, last)
		}
		if d := o.get(o.admin, task.Key); !d.Closed() {
			t.Fatalf("the task is %s, want it closed", d.Status.Name)
		}
	})
}

// --ask is told to the person whoever's token the agent is on, once.
func TestObjectiveDiagnoseAskTellsThePersonOnce(t *testing.T) {
	t.Run("by a person", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		before := len(o.told(NotifyObjective))
		ob, err := o.s.ObjectiveDiagnose(o.ctx, o.admin, task.Key, ObjectiveDiagnoseInput{Text: "mine", Ask: true})
		o.s.Wait()
		if err != nil || ob.PhaseWhy != BlockedDiagnosis {
			t.Fatalf("ask by a person: %+v, %v", ob, err)
		}
		if told := o.told(NotifyObjective); len(told) != before+1 {
			t.Fatalf("notifications %d -> %d: %q", before, len(told), told)
		}
	})
	t.Run("an agent on the person's own token", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		before := len(o.told(NotifyObjective))
		if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "out of reach", Ask: true}); err != nil {
			t.Fatal(err)
		}
		told := o.told(NotifyObjective)
		if len(told) != before+1 || !strings.Contains(told[0], "needs you") {
			t.Fatalf("notifications %d -> %d: %q", before, len(told), told)
		}
	})
	t.Run("an agent on a different token", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		eve := o.user("eve", false)
		if err := o.s.SetMember(o.ctx, o.admin, o.p.Key, "eve", "editor"); err != nil {
			t.Fatal(err)
		}
		tok, _, err := o.s.CreateToken(o.ctx, eve, TokenSpec{Name: "eves", Agent: "anthropic/claude-sonnet-5-5"})
		if err != nil {
			t.Fatal(err)
		}
		ag, err := o.s.TokenActor(o.ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.s.AgentTake(o.ctx, ag, task.Key, ""); err != nil {
			t.Fatal(err)
		}
		before := len(o.told(NotifyObjective))
		if _, err := o.s.ObjectiveDiagnose(o.ctx, ag, task.Key, ObjectiveDiagnoseInput{Text: "out of reach", Ask: true}); err != nil {
			t.Fatal(err)
		}
		told := o.told(NotifyObjective)
		if len(told) != before+1 || !strings.Contains(told[0], "needs you") {
			t.Fatalf("notifications %d -> %d: %q", before, len(told), told)
		}
	})
}

// A person's re-set that changes what is measured starts afresh even when it
// also raises the rounds of an objective that ran out of them (WP-1449): the
// diagnosis and the count belong to the old check.
func TestObjectiveDiagnoseOutOfRoundsThenReSet(t *testing.T) {
	outOfRounds := func(t *testing.T) (*objEnv, *IssueDetail) {
		o := newObjEnv(t)
		task := o.toDiagnose(1, 0, ObjectiveInput{MaxRounds: ptrOf(3)})
		// Work, diagnose, work, diagnose, work: three counted rounds.
		for i := 1; i <= 3; i++ {
			if i > 1 {
				o.take1(task.Key)
				if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: fmt.Sprintf("diagnosis %d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			o.noBetter(task.Key, i)
		}
		// Round 1's no-better made round 2 a diagnose round, and so on; the
		// last work round is round 3, and round 4 is over the limit.
		ob := o.obj(task.Key)
		if ob.PhaseWhy != BlockedRounds || ob.Diagnosis == nil || ob.NoBetter != 1 {
			t.Fatalf("setup: phase %q why %q no_better %d diagnosis %v", ob.Phase, ob.PhaseWhy, ob.NoBetter, ob.Diagnosis)
		}
		return o, task
	}
	t.Run("a new check and more rounds opens a work round", func(t *testing.T) {
		o, task := outOfRounds(t)
		ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all"), MaxRounds: ptrOf(9)})
		if ob.Round != 4 || ob.RoundKind != "" || ob.NoBetter != 0 || ob.Diagnosis != nil {
			t.Fatalf("round %d kind %q no_better %d diagnosis %v", ob.Round, ob.RoundKind, ob.NoBetter, ob.Diagnosis)
		}
	})
	t.Run("a new check alone stays out of rounds, with nothing stale", func(t *testing.T) {
		o, task := outOfRounds(t)
		ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all")})
		if ob.NoBetter != 0 || ob.Diagnosis != nil || ob.RoundKind != "" {
			t.Fatalf("no_better %d diagnosis %v kind %q", ob.NoBetter, ob.Diagnosis, ob.RoundKind)
		}
		ob = o.set(task.Key, ObjectiveInput{MaxRounds: ptrOf(9)})
		if ob.RoundKind != "" {
			t.Fatalf("raising the rounds after it opens a %q round for a check never tried", ob.RoundKind)
		}
	})
	t.Run("more rounds alone keeps the history, so a diagnosis is owed", func(t *testing.T) {
		o, task := outOfRounds(t)
		ob := o.set(task.Key, ObjectiveInput{MaxRounds: ptrOf(9)})
		if ob.Round != 4 || ob.RoundKind != RoundKindDiagnose || ob.Diagnosis == nil {
			t.Fatalf("round %d kind %q diagnosis %v", ob.Round, ob.RoundKind, ob.Diagnosis)
		}
	})
}

// Every round's row has an id of its own: the diagnosed round and the work
// round that reopens it share a round number, and the web keys by the id.
func TestObjectiveRoundRowsHaveDistinctIDs(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "found"}); err != nil {
		t.Fatal(err)
	}
	o.noBetter(task.Key, 3)
	seen := map[int64]bool{}
	rounds := map[int]int{}
	for _, r := range o.obj(task.Key).Rounds {
		if r.ID == 0 || seen[r.ID] {
			t.Fatalf("round row %d has id %d (seen: %v)", r.Round, r.ID, seen)
		}
		seen[r.ID] = true
		rounds[r.Round]++
	}
	if rounds[3] != 2 {
		t.Fatalf("round 3 has %d rows, want the diagnosed one and the reopened work round: %v", rounds[3], rounds)
	}
}

// The brief says what is refused (wp done, wp ship) and what is not: a pull
// request opened on the forge directly, which is read as any round.
func TestObjectiveDiagnoseBriefIsTrueOfShip(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	brief := ObjectiveText(o.obj(task.Key), true, o.clockNow())
	hasAll(t, "the diagnose brief", brief, "wp done, wp ship and wp pr merge of the task's pull request are refused", "opened on the forge directly can't be refused", "read as any round", "a diagnosis is still owed", "wp objective next "+task.Key)
}

func TestObjectiveDiagnoseWordsOneRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(1, 1, ObjectiveInput{})
	ob := o.obj(task.Key)
	for who, text := range map[string]string{"agent": ObjectiveText(ob, true, o.clockNow()), "person": ObjectiveText(ob, false, o.clockNow())} {
		if strings.Contains(text, "1 rounds") || strings.Contains(text, "last 1 rounds") {
			t.Errorf("%s text says '1 rounds':\n%s", who, text)
		}
		hasAll(t, who+" text", text, "last 1 round ")
	}
	for _, h := range o.history(task.Key) {
		if strings.Contains(h, "1 rounds") {
			t.Errorf("history says '1 rounds': %q", h)
		}
	}
}

// A diagnosis ships no code, so its run isn't the step before that a workflow
// step marked "different" compares against: the diagnoser can take the
// reopened round exactly as it could have before diagnosing (WP-1449).
func TestObjectiveDiagnoseRunIsNotAStepCompleted(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	id := "anthropic/claude-opus-5-5"
	allowed := func() bool {
		t.Helper()
		d, err := o.s.GetIssue(o.ctx, o.admin, task.Key)
		if err != nil {
			t.Fatal(err)
		}
		ok, _ := o.s.stepAllows(o.ctx, d.Issue, "any", "different", id)
		return ok
	}
	// Whatever earlier rounds left, the diagnosis must not change the answer
	// for the agent that recorded it: make that answer "allowed" by dropping
	// the earlier completions.
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE agent_runs SET outcome = 'released' WHERE issue_id = ? AND outcome = 'done'`, task.ID); err != nil {
		t.Fatal(err)
	}
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "The check times the build."}); err != nil {
		t.Fatal(err)
	}
	if !allowed() {
		t.Error("after diagnosing, the diagnoser is barred from a step that needs a different agent than the one who did the step before")
	}
}

// stallNotice is the latest stall notice ("" for none). Notices come newest
// first.
func (o *objEnv) stallNotice() string {
	o.t.Helper()
	for _, n := range o.toldBodies(NotifyObjective) {
		if strings.Contains(n, "without getting nearer") {
			return n
		}
	}
	return ""
}

// A reopened round that lands nothing has no reading after the diagnosis, so
// the stall notice says the diagnosis stands, not that rounds since came no
// nearer (WP-1449).
func TestObjectiveDiagnoseStallNoticeWithoutLaterReadings(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil {
		t.Fatal(err)
	}
	o.landsNothing(task.Key, 2)
	got := o.stallNotice()
	if got == "" {
		t.Fatalf("no stall notice: %q", o.toldBodies(NotifyObjective))
	}
	hasAll(t, "the stall notice", got, "diagnosis was recorded in round 3, and it stands")
	if strings.Contains(got, "came no nearer") {
		t.Errorf("claims rounds since the diagnosis were read: %s", got)
	}
}

// landsNothing takes the round and finishes it with nothing landed: the head
// is still sha(at), where the round began.
func (o *objEnv) landsNothing(key string, at int) {
	o.t.Helper()
	o.setClock(o.clockNow().Add(100 * time.Hour))
	o.take1(key)
	if _, err := o.s.AgentDone(o.ctx, o.ag, key, AgentDoneInput{}); err != nil {
		o.t.Fatal(err)
	}
	o.s.Wait()
	o.read(ObjectiveJobResult{Head: sha(at), Base: sha(at), Reading: passed(false)})
}

// Rounds that land nothing after a read one count as stalled, but only the
// read one is evidence against the diagnosis: the stall notice counts that
// one alone, and doesn't say every round since came no nearer (WP-1449).
func TestObjectiveDiagnoseStallNoticeCountsOnlyReadRounds(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil {
		t.Fatal(err)
	}
	o.noBetter(task.Key, 3) // read, no nearer: stalled 3, told once
	for range 3 {
		o.landsNothing(task.Key, 3) // stalled 4, 5, 6: told again at 6
	}
	ob := o.obj(task.Key)
	if ob.Stalled != 6 || ob.NoBetter != 1 || ob.Round != 4 {
		t.Fatalf("setup: stalled %d no_better %d round %d", ob.Stalled, ob.NoBetter, ob.Round)
	}
	got := o.stallNotice()
	hasAll(t, "the stall notice", got, "6 rounds without getting nearer", "recorded in round 3, and since then Waypoint has read 1 round that came no nearer")
	if strings.Contains(got, "rounds since") {
		t.Errorf("says every round since the diagnosis came no nearer: %s", got)
	}
}

// The notice cites the diagnosis that stood when its event was emitted, not
// one recorded before it was delivered.
func TestObjectiveDiagnoseStallNoticeCitesTheDiagnosisOfItsEvent(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := o.s.db.R.QueryRowContext(o.ctx, `SELECT diagnosis_id FROM objectives WHERE issue_id = ?`, task.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	o.noBetter(task.Key, 3)
	o.noBetter(task.Key, 4)
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	// Round 4's stall event is delivered only now.
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE objectives SET stalled_told = 0 WHERE issue_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	before := len(o.toldBodies(NotifyObjective))
	ev := &Event{IssueID: task.ID, New: "round 4: no nearer", Data: map[string]any{"objective": "round", "diagnosis": first, "no_better": 1}}
	o.s.objectiveTell(o.ctx, ev, "round")
	all := o.toldBodies(NotifyObjective)
	if len(all) != before+1 {
		t.Fatalf("notices %d -> %d: %q", before, len(all), all)
	}
	got := all[0] // newest first
	if !strings.Contains(got, "recorded in round 3,") {
		t.Errorf("the notice cites a later diagnosis than its event's: %s", got)
	}
}

// An agent can't merge or arm an existing pull request of a task in a diagnose
// round: the merge would end the round as an ordinary reading (WP-1449).
func TestObjectiveDiagnoseAgentMergeIsRefused(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.pull(task.Key, 7)
	rec, err := o.s.recordPR(o.ctx, o.f, "me/app", &ForgePR{Number: 7, Title: "Round work", HeadRef: "round", HeadSHA: "abc1234def", Author: o.admin.User.Username, State: "open"}, false)
	if err != nil {
		t.Fatal(err)
	}
	prID := rec.ID
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE pull_requests SET agent = ? WHERE id = ?`, o.ag.Agent, prID); err != nil {
		t.Fatal(err)
	}
	for _, when := range []bool{false, true} {
		_, err := o.s.MergePR(o.ctx, o.ag, prID, MergeInput{Style: "squash", WhenReady: when})
		if err == nil || !strings.Contains(err.Error(), "diagnose round") || !strings.Contains(err.Error(), "wp objective diagnose "+task.Key) {
			t.Errorf("agent merge (when ready %v): %v", when, err)
		}
	}
	// A person's call: not refused for the round.
	if _, err := o.s.MergePR(o.ctx, o.admin, prID, MergeInput{Style: "squash", WhenReady: true}); err != nil && strings.Contains(err.Error(), "diagnose round") {
		t.Errorf("a person's arm was refused for the round: %v", err)
	}
}

// A pull request linked before a diagnose round began that merges while a
// worker holds it ends the round as an ordinary reading: the holder's claim
// and run are closed, not orphaned, and the history says so (WP-1449).
func TestObjectiveDiagnoseHeldRoundReadsAnEarlierPullRequest(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass"), DiagnoseAfter: ptrOf(2)})
	o.read(ObjectiveJobResult{Head: sha(0), Reading: passed(false)})
	o.noBetter(task.Key, 1)
	o.noBetter(task.Key, 2)
	if ob := o.obj(task.Key); ob.RoundKind != RoundKindDiagnose || ob.Round != 3 {
		t.Fatalf("setup: round %d kind %q", ob.Round, ob.RoundKind)
	}
	o.take1(task.Key)
	url := o.pull(task.Key, 7)
	// Linked before the round began.
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE external_links SET created_at = 1 WHERE issue_id = ? AND kind = 'pull'`, task.ID); err != nil {
		t.Fatal(err)
	}
	o.linkState(url, "merged")
	ob := o.obj(task.Key)
	if ob.Round == 3 && ob.Phase == PhaseWork {
		t.Fatalf("the merge left the diagnose round open: %+v", ob)
	}
	d, err := o.s.GetIssue(o.ctx, o.admin, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if d.ClaimedBy != "" {
		t.Errorf("the holder's claim is orphaned: %q", d.ClaimedBy)
	}
	var open int
	if err := o.s.db.R.QueryRowContext(o.ctx, `SELECT count(*) FROM agent_runs WHERE issue_id = ? AND ended_at IS NULL`, task.ID).Scan(&open); err != nil || open != 0 {
		t.Errorf("open runs after the merge: %d, %v", open, err)
	}
	found := false
	for _, h := range o.history(task.Key) {
		if strings.Contains(h, "a diagnose round, but a pull request landed in it") {
			found = true
		}
	}
	if !found {
		t.Errorf("history doesn't say the round was read as any: %q", o.history(task.Key))
	}
}
