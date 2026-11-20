package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// toAssessorOnFailingCheck runs #7 to its assessor round over a failing
// check.
func toAssessorOnFailingCheck(t *testing.T, be *bsEnv) *Babysit {
	t.Helper()
	be.runnerOnline()
	be.start(be.open())
	be.ci("7777777aaaa", CheckFailed)
	be.finish(be.babysit().JobID, false)
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if !lastRound(b).Assessor {
		t.Fatalf("no assessor round: %+v", b)
	}
	return b
}

// pastRest moves the clock past #7's rest and steps.
func (be *bsEnv) pastRest() {
	be.t.Helper()
	be.setClock(lastRound(be.babysit()).RestUntil.Add(time.Second))
	be.runnerOnline()
	be.step()
}

// rerunSettles moves the clock past the wait for a re-run's results, which
// the forge still shows failed, and steps.
func (be *bsEnv) rerunSettles() {
	be.t.Helper()
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.runnerOnline()
	be.step()
}

// A park nobody was asked about rests out of the merge line (WP-930): no
// request, no notice. A rest over failing checks alone re-runs them when it
// ends, and their failing again brings the assessor back as soon as that is
// known, told what happened and with every way to end still open to it.
func TestRestRerunsItsChecksThenTheAssessor(t *testing.T) {
	be := newBabysitEnv(t)
	b := toAssessorOnFailingCheck(t, be)
	// A question with no reason a person has to settle it reaches nobody.
	be.finishWith(b.JobID, false, "The test asserts the old order.\n\nQuestion: Should I change the test or the code?")
	b = be.babysit()
	r := lastRound(b)
	if !resting(r) || b.State != BabysitResting || !strings.HasPrefix(b.Wait, restWait) || len(r.ParkedChecks) == 0 {
		t.Fatalf("not resting: %s %q %+v", b.State, b.Wait, r)
	}
	if got := r.RestUntil.Sub(be.clockNow()); got != restFirst {
		t.Errorf("rest %v, want %v", got, restFirst)
	}
	if n := be.parkQuestions(); n != 0 {
		t.Fatalf("%d park questions", n)
	}
	if be.notified("can't merge until you answer") {
		t.Error("a person was notified about a rest")
	}
	if why := be.s.babysitParked(be.ctx, be.pr(7)); !strings.Contains(why, "it tries again at") {
		t.Errorf("babysitParked = %q", why)
	}
	// The rest ends: its checks are re-run, and it waits for their results,
	// not for a time.
	retried := len(be.retries())
	be.pastRest()
	b = be.babysit()
	r = lastRound(b)
	if len(be.retries()) != retried+1 || r.RerunAt.IsZero() || !resting(r) || len(be.jobs()) != 3 || !strings.Contains(b.Wait, "running again") {
		t.Fatalf("after the rest: retries %v, %q %+v, jobs %v", be.retries(), b.Wait, r, be.jobs())
	}
	be.step()
	if len(be.retries()) != retried+1 || len(be.jobs()) != 3 {
		t.Fatalf("before the results: retries %v, jobs %v", be.retries(), be.jobs())
	}
	// They fail again: the assessor, at once.
	be.rerunSettles()
	b = be.babysit()
	if len(be.retries()) != retried+1 || len(be.jobs()) != 4 || !lastRound(b).Assessor || b.Rounds[2].Reply == "" {
		t.Fatalf("after the re-run failed: retries %v, %+v, jobs %v", be.retries(), b, be.jobs())
	}
	in := be.jobInput(b.JobID)
	for _, want := range []string{"Nobody was asked", "read no `Because:` line", "Should I change the test or the code?", "was re-run and failed again",
		"`Wait: …`", "`Question: …` and `Because: …`", "a change you doubt is worse than none"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("assessor prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	// Nothing tells it to decide whatever it finds.
	for _, bad := range []string{"Make the call yourself", "safest default"} {
		if strings.Contains(in.Prompt, bad) {
			t.Errorf("assessor prompt says %q:\n%s", bad, in.Prompt)
		}
	}
	// Its round leaves the same check failing: nothing has changed, so the
	// rest is twice as long, and the next round is told how long that is.
	be.finish(b.JobID, false)
	r = lastRound(be.babysit())
	if !resting(r) {
		t.Fatalf("not resting again: %+v", r)
	}
	if got := r.RestUntil.Sub(be.clockNow()); got != 2*restFirst {
		t.Errorf("second rest over the same check %v, want %v", got, 2*restFirst)
	}
	be.pastRest()
	be.rerunSettles()
	b = be.babysit()
	if len(be.jobs()) != 5 || !strings.Contains(be.jobInput(b.JobID).Prompt, "rested 2 times in a row over the same things") {
		t.Fatalf("after the second rest: jobs %v\n%s", be.jobs(), be.jobInput(b.JobID).Prompt)
	}
	// Now it says a person has to settle it, and why: they are asked.
	be.finishWith(b.JobID, false, "Question: May the old order go?\nBecause: the task says to keep it and the test can't pass with it.")
	if b = be.babysit(); b.State != BabysitNeedsPerson || be.parkQuestions() != 1 {
		t.Fatalf("its question wasn't asked: %s %q, %d questions", b.State, b.Wait, be.parkQuestions())
	}
}

// A rest's length follows what it is over, not how many rests there were:
// it doubles while the same things stand and starts over when they change.
func TestRestLength(t *testing.T) {
	same := BabysitRound{Parked: "why", RestUntil: time.Now(), Checks: []string{"ci / test", "lint"}}
	reordered := same
	reordered.Checks = []string{"lint", "ci / test"}
	other := BabysitRound{Parked: "why", RestUntil: time.Now(), Checks: []string{"lint"}}
	person := BabysitRound{Parked: "why", Checks: same.Checks}
	worked := BabysitRound{Checks: []string{"lint"}} // a round that didn't park
	cases := []struct {
		name   string
		before []BabysitRound
		want   time.Duration
	}{
		{"the first rest", nil, restFirst},
		{"the same again", []BabysitRound{same}, 2 * restFirst},
		{"in any order, with rounds between", []BabysitRound{same, worked, reordered, worked}, 4 * restFirst},
		{"a long run of them", []BabysitRound{same, same, same, same, same, same, same}, restMax},
		{"one check fewer: progress", []BabysitRound{other, other, other}, restFirst},
		{"since it last changed", []BabysitRound{same, other, same}, 2 * restFirst},
		{"since a person answered", []BabysitRound{same, same, person, same}, 2 * restFirst},
	}
	for _, c := range cases {
		b := &Babysit{Rounds: append(slices.Clone(c.before), same)}
		if got := restLength(b); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// An assessor that says what it waits for outside the pull request rests,
// and the rest ends early when the check it left failing passes on another
// head: that may be what it waited for. That evidence counts once: when the
// re-run fails here all the same, the next rest runs its time.
func TestRestEndsEarlyWhenItsCheckPassesElsewhere(t *testing.T) {
	be := newBabysitEnv(t)
	b := toAssessorOnFailingCheck(t, be)
	be.finishWith(b.JobID, false, "The job dies pulling its image; nothing in the diff does that.\n\n**Wait:** the CI image has to be rebuilt")
	b = be.babysit()
	r := lastRound(b)
	if !resting(r) || r.Waiting != "the CI image has to be rebuilt" || !strings.Contains(r.Parked, "the CI image has to be rebuilt") || be.parkQuestions() != 0 {
		t.Fatalf("not resting on its wait: %s %q %+v", b.State, b.Wait, r)
	}
	passElsewhere := func(sha string) {
		t.Helper()
		be.setClock(be.clockNow().Add(10 * time.Minute))
		pr := be.pr(7)
		id, err := be.s.trackHead(be.ctx, pr.ForgeID, pr.Repo, sha)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := be.s.db.W.Exec(`INSERT INTO ci_checks(head_id, name, state, updated_at) VALUES (?, 'ci / test', ?, ?)`, id, CheckSuccess, be.clockNow().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		be.runnerOnline()
		be.step()
	}
	retried := len(be.retries())
	be.step()
	if len(be.retries()) != retried {
		t.Fatal("re-ran with nothing changed")
	}
	passElsewhere("9999999cccc")
	r = lastRound(be.babysit())
	if len(be.retries()) != retried+1 || r.RerunAt.IsZero() || !r.RerunAt.Before(r.RestUntil) || r.RestChange == "" {
		t.Fatalf("no early re-run: retries %v %+v", be.retries(), r)
	}
	be.rerunSettles()
	b = be.babysit()
	if len(be.jobs()) != 4 || !lastRound(b).Assessor {
		t.Fatalf("no assessor after the re-run failed: %+v jobs %v", b, be.jobs())
	}
	in := be.jobInput(b.JobID)
	for _, want := range []string{"Ci / test has passed on another head of the repository since the rest began, so ci / test was re-run here and failed again", "It ended waiting for: the CI image has to be rebuilt"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("assessor prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	be.finishWith(b.JobID, false, "Wait: the CI image has to be rebuilt")
	r = lastRound(be.babysit())
	if got := r.RestUntil.Sub(be.clockNow()); !resting(r) || got != 2*restFirst {
		t.Fatalf("second rest %v, want %v: %+v", got, 2*restFirst, r)
	}
	passElsewhere("aaaaaaadddd")
	if r = lastRound(be.babysit()); len(be.retries()) != retried+1 || !r.RerunAt.IsZero() {
		t.Fatalf("the same evidence ended a second rest early: retries %v %+v", be.retries(), r)
	}
}

// A park from before WP-930 whose question came with no reason: its request
// is withdrawn and it rests, due at once, so its checks are re-run.
func TestOldParkRests(t *testing.T) {
	be := newBabysitEnv(t)
	pr := assessorAsks(t, be, "Should the test change?")
	q := be.parkRequest(3)
	b := be.babysit()
	b.Rounds[2].Because = "" // as written before WP-930
	raw, _ := json.Marshal(b.Rounds)
	if err := be.s.setBabysit(be.ctx, b, map[string]any{"rounds": string(raw)}); err != nil {
		t.Fatal(err)
	}
	retried := len(be.retries())
	be.step()
	b = be.babysit()
	r := lastRound(b)
	if !parkedOn(b, pr.HeadSHA) || !resting(r) || r.RerunAt.IsZero() || b.State != BabysitResting || len(be.retries()) != retried+1 {
		t.Fatalf("old park: %s %q %+v", b.State, b.Wait, r)
	}
	if got := be.request(q.ID); got.State != RequestWithdrawn {
		t.Errorf("its request = %s", got.State)
	}
}

// A question a person was asked keeps its request and waits for them,
// however long.
func TestPersonQuestionDoesntRest(t *testing.T) {
	be := newBabysitEnv(t)
	assessorAsks(t, be, "May I rotate the deploy key?")
	be.parkRequest(3)
	be.setClock(be.clockNow().Add(restMax + time.Hour))
	be.runnerOnline()
	be.step()
	b := be.babysit()
	if r := lastRound(b); resting(r) || r.Reply != "" || b.State != BabysitNeedsPerson || len(be.jobs()) != 3 {
		t.Fatalf("a person's park moved on alone: %s %q %+v", b.State, b.Wait, r)
	}
}

// The request quotes the review comments the assessor named in its question,
// all of them and no others.
func TestParkRequestQuotesTheCommentsNamed(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Now()
	for _, c := range [][2]string{{"501", "Handle empty rows."}, {"502", "Rename the helper."}, {"503", "Drop the legacy column."}, {"504", "Purge the old exports too."}, {"505", "Typo."}} {
		be.sg.threads = append(be.sg.threads, ReviewThread{ID: "T" + c[0], Path: "app/export.go", Line: 4, Comments: []ThreadComment{
			{ID: c[0], Author: "alice", Body: c[1], SHA: "7777777aaaa", At: at}}})
	}
	b := toAssessorOnFailingCheck(t, be)
	refs := map[string]string{}
	for _, c := range be.comments() {
		refs[c.Body] = c.Ref
	}
	be.finishWith(b.JobID, false, "Question: May the legacy column and the old exports be deleted ("+refs["Drop the legacy column."]+", "+refs["Purge the old exports too."]+", "+refs["Handle empty rows."]+")?\n"+
		"Because: neither can be restored once gone.")
	body := be.parkRequest(3).Body
	for text := range refs {
		named := text == "Drop the legacy column." || text == "Purge the old exports too." || text == "Handle empty rows."
		if strings.Contains(body, text) != named {
			t.Errorf("%q quoted = %v, want %v:\n%s", text, !named, named, body)
		}
	}
	if !strings.Contains(body, "as the assessor put it:\n\n> neither can be restored once gone.") {
		t.Errorf("the request lacks the reason:\n%s", body)
	}
}

// A re-run whose results aren't in doesn't hold the rest open (WP-930
// review): once the forge has taken it up the head is an ordinary one with
// running checks, watched as any other is, however long the step was away.
// Its failing later brings the assessor, told nobody was asked.
func TestRestEndsWhileItsRerunStillRuns(t *testing.T) {
	for _, away := range []time.Duration{faultRerunSettle, 20 * time.Minute} {
		be := newBabysitEnv(t)
		b := toAssessorOnFailingCheck(t, be)
		be.finishWith(b.JobID, false, "Wait: the runner's database service has to come back")
		be.pastRest()
		if r := lastRound(be.babysit()); r.RerunAt.IsZero() || !resting(r) {
			t.Fatalf("no re-run: %+v", r)
		}
		be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckPending, URL: "https://ci.example.com/run/1"})
		be.setClock(be.clockNow().Add(away))
		be.runnerOnline()
		be.step()
		b = be.babysit()
		r := &b.Rounds[2]
		if r.Reply == "" || strings.Contains(r.Reply, "failed again") || b.State == BabysitResting || len(be.jobs()) != 3 || be.parkQuestions() != 0 {
			t.Fatalf("away %v, re-run still running: %s %q reply %q jobs %v", away, b.State, b.Wait, r.Reply, be.jobs())
		}
		be.ci("7777777aaaa", CheckFailed)
		be.runnerOnline()
		be.step()
		b = be.babysit()
		if len(be.jobs()) != 4 || !lastRound(b).Assessor || !strings.Contains(be.jobInput(b.JobID).Prompt, "Nobody was asked") {
			t.Fatalf("away %v, after the re-run failed: %+v jobs %v", away, b, be.jobs())
		}
	}
}

// An old park its owner answered, whose answer never reached the babysit
// (the event was lost in a restart), goes on with the answer: it isn't
// turned into a rest that drops it.
func TestOldParkKeepsItsAnswer(t *testing.T) {
	be := newBabysitEnv(t)
	assessorAsks(t, be, "Should the test change?")
	q := be.parkRequest(3)
	b := be.babysit()
	b.Rounds[2].Because = "" // as written before WP-930
	raw, _ := json.Marshal(b.Rounds)
	if err := be.s.setBabysit(be.ctx, b, map[string]any{"rounds": string(raw)}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`UPDATE requests SET state = ?, answer = ? WHERE id = ?`, RequestAnswered, "Change the test.", q.ID); err != nil {
		t.Fatal(err)
	}
	be.step()
	b = be.babysit()
	if r := &b.Rounds[2]; r.Reply != "Change the test." || !r.RestUntil.IsZero() || len(be.jobs()) != 4 {
		t.Fatalf("old answered park: reply %q %+v jobs %v", r.Reply, r, be.jobs())
	}
	if got := be.request(q.ID); got.State != RequestAnswered {
		t.Errorf("its request = %s", got.State)
	}
}
