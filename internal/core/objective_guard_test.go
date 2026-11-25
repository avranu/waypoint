package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The merge of a round's pull request ends the round, whoever merged it:
// Waypoint reads the merge itself, and a rule of the project that closes
// tasks on merge doesn't close this one past its objective.
func TestObjectiveRoundEndsWhenItsPullRequestMerges(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: passed(false)})
	o.take1(task.Key)
	pr := o.pull(task.Key, 7)
	o.setClock(o.clockNow().Add(time.Minute))
	o.linkState(pr, "merged")
	ob := o.obj(task.Key)
	if ob.Phase != PhaseVerdict || ob.Round != 1 || len(ob.Rounds) != 1 || ob.Rounds[0].Outcome != RoundReading || ob.Rounds[0].PR != pr {
		t.Fatalf("after the merge: phase %q round %d %+v", ob.Phase, ob.Round, ob.Rounds)
	}
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("status = %s while the round is read", got)
	}
	// The same merge, told again, ends nothing more.
	if err := o.s.objectiveMerged(o.ctx, o.issueID(task.Key), pr); err != nil {
		t.Fatal(err)
	}
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: passed(false)})
	ob = o.obj(task.Key)
	if ob.Round != 2 || len(ob.Rounds) != 1 || ob.Rounds[0].Outcome != RoundWorked || o.status(task.Key) != "To Do" {
		t.Fatalf("after the reading: round %d %+v %s", ob.Round, ob.Rounds, o.status(task.Key))
	}
	// Round 2 isn't out on round 1's merged pull request.
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("round 2 isn't handed out: %q", wait)
	}
	o.take1(task.Key)
	pr2 := o.pull(task.Key, 8)
	o.setClock(o.clockNow().Add(time.Minute))
	o.linkState(pr2, "merged")
	o.read(ObjectiveJobResult{Head: shaC, Base: shaB, Reading: passed(true)})
	if ob = o.obj(task.Key); ob.State != ObjectiveMet || o.status(task.Key) != "Done" || ob.Rounds[1].PR != pr2 {
		t.Fatalf("met by round 2's merge: %s %s %+v", ob.State, o.status(task.Key), ob.Rounds)
	}
}

// Pull requests that merge at other times than the end of the round being
// worked: one that lands while the round waits to be taken up moves where
// the round starts from, so that is read again; one from an earlier round
// that lands while somebody works the round doesn't cut their work short;
// one that lands while a round is being read has the round read again.
func TestObjectiveMergesOutOfTurn(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: passed(false)})
	o.take1(task.Key)
	prs := []string{o.pull(task.Key, 7), o.pull(task.Key, 8), o.pull(task.Key, 9), o.pull(task.Key, 10)}
	o.setClock(o.clockNow().Add(time.Minute))
	o.linkState(prs[0], "merged")
	// A second lands while round 1 is read: the reading starts over.
	_, first := o.job()
	o.linkState(prs[1], "merged")
	ob := o.obj(task.Key)
	if ob.Phase != PhaseVerdict || ob.Round != 1 || len(ob.Rounds) != 1 {
		t.Fatalf("a merge during the reading: %q round %d %+v", ob.Phase, ob.Round, ob.Rounds)
	}
	if j := o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: passed(false)}); j.Round != first.Round || j.Base != shaA {
		t.Fatalf("the second reading = %+v", j)
	}
	if ob = o.obj(task.Key); ob.Round != 2 || len(ob.Rounds) != 1 || ob.Phase != PhaseWork {
		t.Fatalf("after the reading: round %d %+v", ob.Round, ob.Rounds)
	}
	// A third lands while round 2 waits in To Do: no round ends, and where
	// round 2 starts from is read again.
	o.linkState(prs[2], "merged")
	if ob = o.obj(task.Key); ob.Phase != PhaseBaseline || ob.Round != 2 || len(ob.Rounds) != 1 {
		t.Fatalf("a merge while the round waits: %q round %d", ob.Phase, ob.Round)
	}
	if _, err := o.s.AgentTake(o.ctx, o.ag, task.Key, ""); !IsInvalid(err) {
		t.Errorf("taking it while its start is read: %v", err)
	}
	if j := o.read(ObjectiveJobResult{Head: shaC, Reading: passed(false)}); j.Base != "" {
		t.Fatalf("a starting point compared with something: %+v", j)
	}
	if ob = o.obj(task.Key); ob.Phase != PhaseWork || ob.BaseSHA != shaC || ob.Round != 2 {
		t.Fatalf("after reading the new start: %q %s", ob.Phase, ob.BaseSHA)
	}
	// The fourth lands while the agent works round 2: the round goes on.
	o.setClock(o.clockNow().Add(time.Minute))
	o.take1(task.Key)
	o.setClock(o.clockNow().Add(time.Minute))
	o.linkState(prs[3], "merged")
	ob = o.obj(task.Key)
	if ob.Phase != PhaseWork || ob.Round != 2 || len(ob.Rounds) != 1 || o.status(task.Key) != "In Progress" {
		t.Fatalf("an earlier round's merge ended the round being worked: %q round %d %+v %s", ob.Phase, ob.Round, ob.Rounds, o.status(task.Key))
	}
	if h := strings.Join(o.history(task.Key), "\n"); !strings.Contains(h, "doesn't end it") {
		t.Errorf("the task's history doesn't say so:\n%s", h)
	}
}

// Only a person ends an objective. An agent can't cancel the task, delete
// it, change what its rounds are read by, or steer it; a person's token in
// an agent's shell is the agent.
func TestObjectiveIsAPersonsToEnd(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Twice as fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("2x")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(1)})
	o.take1(task.Key)
	shell := &Actor{User: o.admin.User, AgentEnv: "claude"}
	st := "canceled"
	for who, actor := range map[string]*Actor{"an agent": o.ag, "a person's token in an agent's shell": shell} {
		if _, err := o.s.UpdateIssue(o.ctx, actor, task.Key, IssuePatch{Status: &st}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s cancelling the task: %v", who, err)
		}
		if err := o.s.DeleteIssue(o.ctx, actor, task.Key, false); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s deleting the task: %v", who, err)
		}
		if _, err := o.s.SetObjective(o.ctx, actor, task.Key, ObjectiveInput{Until: ptrOf("1.01x")}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s loosening the target: %v", who, err)
		}
		if err := o.s.ClearObjective(o.ctx, actor, task.Key); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s taking it off: %v", who, err)
		}
		for _, act := range []string{ObjectiveEnd, ObjectiveNext, ObjectiveRetry, ObjectiveAccept} {
			if _, err := o.s.ObjectiveGo(o.ctx, actor, task.Key, act); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s: objective %s: %v", who, act, err)
			}
		}
	}
	ob := o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Round != 1 || ob.Until != "2x" || o.status(task.Key) != "In Progress" {
		t.Fatalf("after all that: %s round %d %s %s", ob.State, ob.Round, ob.Until, o.status(task.Key))
	}
	// A person's cancel stands and ends it.
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Status: &st}); err != nil {
		t.Fatal(err)
	}
	if ob = o.obj(task.Key); ob.State != ObjectiveEnded || !strings.Contains(ob.EndedWhy, "by ") {
		t.Fatalf("after a person's cancel: %s %q", ob.State, ob.EndedWhy)
	}

	// A person ends one and the task stays an ordinary, open task.
	other := o.task("Other")
	o.set(other.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, other.Key, ObjectiveEnd); err != nil {
		t.Fatal(err)
	}
	if ob = o.obj(other.Key); ob.State != ObjectiveEnded || o.status(other.Key) == "Done" {
		t.Fatalf("ended: %s %s", ob.State, o.status(other.Key))
	}
	o.noJob() // its reading was called off
	if res := o.worked(other.Key); !res.Finished {
		t.Fatalf("an ended objective still holds the task open: %+v", res)
	}
	// A person deletes a task with one in effect.
	third := o.task("Third")
	o.set(third.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if err := o.s.DeleteIssue(o.ctx, o.admin, third.Key, false); err != nil {
		t.Fatal(err)
	}
	o.noJob()
}

// A task closed past its objective by something that asks nothing of it
// (its status renamed into a closed one, a bulk edit by a rule) doesn't
// leave an objective in effect on a closed task: the objective ends.
func TestObjectiveEndsWhenItsTaskIsClosedPastIt(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench.sh"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.worked(task.Key)
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = 'done' LIMIT 1) WHERE id = ?`, o.p.ID, o.issueID(task.Key)); err != nil {
		t.Fatal(err)
	}
	o.tick(time.Minute)
	ob := o.obj(task.Key)
	if ob.State != ObjectiveEnded || !strings.Contains(ob.EndedWhy, "closed") || ob.Rounds[0].Outcome != RoundUnmeasured {
		t.Fatalf("after the close: %s %q %+v", ob.State, ob.EndedWhy, ob.Rounds)
	}
	o.noJob()
}

// A round an agent took and left, with no pull request open, is handed out
// again; one whose pull request is closed unmerged is handed out at once.
func TestObjectiveRoundLeftIsHandedOutAgain(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench.sh"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.take1(task.Key)
	d := o.get(o.admin, task.Key)
	if d.ClaimedUntil == nil {
		t.Fatal("no claim")
	}
	// The claim runs out; not handed back before an hour has gone by.
	o.setClock(d.ClaimedUntil.Add(30 * time.Minute))
	o.s.CheckObjectives(o.ctx)
	if got := o.status(task.Key); got != "In Progress" {
		t.Fatalf("handed back after 30 minutes: %s", got)
	}
	o.tick(45 * time.Minute)
	ob := o.obj(task.Key)
	if got := o.status(task.Key); got != "To Do" || ob.Round != 1 || len(ob.Rounds) != 0 {
		t.Fatalf("after the hour: %s round %d %+v", got, ob.Round, ob.Rounds)
	}
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("the round isn't handed out again: %q", wait)
	}
	// Taken again, a pull request opened, and closed unmerged.
	o.take1(task.Key)
	pr := o.pull(task.Key, 7)
	d = o.get(o.admin, task.Key)
	o.setClock(d.ClaimedUntil.Add(2 * time.Hour))
	o.s.CheckObjectives(o.ctx)
	if got := o.status(task.Key); got != "In Progress" {
		t.Fatalf("handed back with its pull request open: %s", got)
	}
	o.linkState(pr, "closed")
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("after its pull request was closed: %s", got)
	}
	if ob = o.obj(task.Key); ob.Round != 1 || ob.State != ObjectiveActive || ob.Phase != PhaseWork {
		t.Fatalf("the round: %d %s %q", ob.Round, ob.State, ob.Phase)
	}
}

// A pull request from outside no person approved isn't the round's: a
// round left with only its link is handed out again (WP-1802).
func TestObjectiveRoundIgnoresAnUnapprovedOutsidePull(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench.sh"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.take1(task.Key)
	url := o.pull(task.Key, 7)
	if _, err := o.s.db.W.ExecContext(o.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, outside, created_at, updated_at)
		VALUES (?, 'me/app', 7, ?, 'open', 1, 1, 1) ON CONFLICT(forge_id, repo, number) DO UPDATE SET outside = 1, url = excluded.url`, o.f.ID, url); err != nil {
		t.Fatal(err)
	}
	d := o.get(o.admin, task.Key)
	o.setClock(d.ClaimedUntil.Add(2 * time.Hour))
	o.s.CheckObjectives(o.ctx)
	o.s.Wait()
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("with only an unapproved outside pull request: %s", got)
	}
}

// A round nobody takes up, and one out on a pull request nobody merges,
// don't wait in silence: the objective's person is told, once and then
// daily, and the list says it is stuck.
func TestObjectiveStuckIsTold(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench.sh"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.tick(23 * time.Hour)
	if ob := o.obj(task.Key); ob.Health == "stuck" || len(o.told(NotifyObjective)) != 0 {
		t.Fatalf("stuck after 23 hours: %s", ob.Health)
	}
	o.tick(2 * time.Hour)
	ob := o.obj(task.Key)
	if ob.Health != "stuck" || !strings.Contains(ob.HealthNote, "nobody has taken it up") {
		t.Fatalf("after 25 hours: %s %q", ob.Health, ob.HealthNote)
	}
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "is stuck") {
		t.Fatalf("told = %q", told)
	}
	o.tick(time.Hour)
	if told := o.told(NotifyObjective); len(told) != 1 {
		t.Fatalf("told again within the day: %q", told)
	}
	o.tick(24 * time.Hour)
	if told := o.told(NotifyObjective); len(told) != 2 || !strings.Contains(told[0]+told[1], "still stuck") {
		t.Fatalf("not told again after a day: %q", told)
	}
	// Taken up, it isn't stuck any more.
	o.take1(task.Key)
	o.tick(6 * time.Minute)
	if ob = o.obj(task.Key); ob.Health == "stuck" || ob.StuckSince != nil {
		t.Fatalf("taken up: %s", ob.Health)
	}
	// Out on a pull request for three days.
	o.pull(task.Key, 7)
	o.tick(73 * time.Hour)
	if ob = o.obj(task.Key); ob.Health != "stuck" || !strings.Contains(ob.HealthNote, "hasn't merged") {
		t.Fatalf("a pull request nobody merges: %s %q", ob.Health, ob.HealthNote)
	}
}
