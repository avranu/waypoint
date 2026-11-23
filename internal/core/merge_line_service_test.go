package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// lineEnv is stackEnv with the line driven the way production does: a
// coalescing lineChanged per base (never lineStep directly), then the
// babysits' steps.
func (se *stackEnv) line() {
	se.t.Helper()
	se.s.lineChanged(se.ctx, lineRef{forge: se.pr(7).ForgeID, repo: se.pr(7).Repo, base: "main"})
	se.s.Wait()
}

// madeHeads lists the heads of each candidate branch made, as "7,8".
func (se *stackEnv) madeHeads() []string {
	var out []string
	for _, in := range se.made {
		var ns []string
		for _, l := range strings.Split(in.Files[0].Content, "\n") {
			if n, _, ok := strings.Cut(l, " "); ok && n != "" && !strings.HasPrefix(l, "#") {
				ns = append(ns, n)
			}
		}
		out = append(out, strings.Join(ns, ","))
	}
	return out
}

// Three settled armed pull requests get their candidates together: main + #7,
// main + #7 + #8, main + #7 + #8 + #9. None is brought up to date.
func TestLineStartsCandidatesTogether(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8 7,8,9" {
		t.Fatalf("candidates made: %q (%+v)", got, se.made)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v", u)
	}
	if w := se.babysitOf(8).Wait; !strings.Contains(w, "tested on a candidate (main + #7 + #8): its run is going") {
		t.Errorf("#8 waits %q", w)
	}
	// Another look changes nothing.
	se.line()
	if len(se.made) != 3 {
		t.Errorf("candidates made again: %d", len(se.made))
	}
}

// The candidates pass and all three merge in order, each on its own
// candidate: no update of a branch, no further run, no further candidate.
func TestLineMergesThreeOnTheirCandidates(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.line()
	if len(se.made) != 3 {
		t.Fatalf("candidates made: %v", se.madeHeads())
	}
	h7, h8, h9 := "7 "+head7s, "8 "+head8s, "9 "+head9s
	se.stackRun(0, CheckSuccess, treeA, h7)
	se.stackRun(1, CheckSuccess, treeB, h7, h8)
	se.stackRun(2, CheckSuccess, strings.Repeat("c", 40), h7, h8, h9)
	se.sg.trees = map[string]string{"3e7000000": treeA, "3e8000000": treeB, "3e9000000": strings.Repeat("c", 40)}
	se.line()
	if pr := se.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready on its candidate: %+v (%q)", pr, se.babysitOf(7).Wait)
	}
	se.editSkipped(head7s, 30)
	se.step()
	if m := se.sg.merged(); len(m) != 1 || m[0].HeadSHA != head7s {
		t.Fatalf("merges %+v (%q)", m, se.babysitOf(7).Wait)
	}
	se.line()
	se.editSkipped(head8s, 40)
	se.step()
	if m := se.sg.merged(); len(m) != 2 || m[1].HeadSHA != head8s {
		t.Fatalf("merges %+v (%q)", m, se.babysitOf(8).Wait)
	}
	se.line()
	se.editSkipped(head9s, 50)
	se.step()
	if m := se.sg.merged(); len(m) != 3 || m[2].HeadSHA != head9s {
		t.Fatalf("merges %+v (%q)", m, se.babysitOf(9).Wait)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v", u)
	}
	if len(se.made) != 3 {
		t.Errorf("candidates made: %v", se.madeHeads())
	}
	for _, st := range se.stacks() {
		if st.EndReason != endMerged {
			t.Errorf("candidate %s ended %q", st.name(), st.EndReason)
		}
	}
}

// The front's candidate fails, runs its failed jobs once more and fails
// again: #7 is ejected with a round for the failed check of its candidate's
// run, and the two behind it get candidates without it.
func TestLineFrontFailsTwiceIsEjected(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.line()
	h7 := "7 " + head7s
	se.stackRun(0, CheckFailed, treeA, h7)
	se.line()
	if sts := se.stacks(); sts[0].RerunAt == 0 || sts[0].Ended {
		t.Fatalf("not run again once: %+v", sts[0])
	}
	se.setClock(se.clockNow().Add(lineRerunSettle + time.Minute))
	se.stackRun(0, CheckFailed, treeA, h7)
	se.line()
	se.step()
	sts := se.stacks()
	if sts[0].State != stackFailed || sts[0].EndReason != endFailed {
		t.Fatalf("#7's candidate: %s %q", sts[0].State, sts[0].EndReason)
	}
	if st := se.sg.lastStatus(head7s); st.State != "failure" || !strings.Contains(st.Context, "backend") {
		t.Errorf("#7's head status: %+v", st)
	}
	// The flake rerun comes before a round for the ejected PR's failed check.
	if b := se.babysitOf(7); b.JobID == 0 && !strings.Contains(b.Wait, "re-ran") {
		t.Errorf("#7 got no round or rerun: %q", b.Wait)
	}
	// The round that follows is told the failure was seen on the candidate.
	if todo, err := se.s.PRWorkPacket(se.ctx, SystemActor(), se.pr(7).ID); err != nil ||
		!strings.Contains(todo.Text, "merged onto main at tip-1") || !strings.Contains(todo.Text, "Merge main into the branch first") {
		t.Errorf("packet of #7: %v %+v", err, todo)
	}
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8 7,8,9 8 8,9" {
		t.Errorf("candidates made: %q", got)
	}
	for _, st := range sts[1:3] {
		if !st.Ended || st.EndReason != endHeadMoved && st.EndReason != endLineChanged && st.EndReason != endBaseMoved {
			t.Errorf("%s ended %q", st.name(), st.EndReason)
		}
	}
}

// A PR whose checks haven't passed isn't an entry; it joins, and gets its
// candidate, when they pass.
func TestLineUnsettledJoinsWhenSettled(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8" {
		t.Fatalf("candidates made: %q", got)
	}
	if pr := se.pr(9); pr.QueuedAt != nil {
		t.Errorf("#9 holds a place before its checks passed: %v", pr.QueuedAt)
	}
	se.draftGreen(head9s, 9)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8 7,8,9" {
		t.Errorf("candidates made: %q", got)
	}
}

// A failed check on a PR far back starts a round for it, and leaves the line
// alone.
func TestLineRoundForAFailedCheckFarBack(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftChecks(head9s, CommitCheck{State: CheckFailed, ID: 9, Description: "Failing after 3m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Has been skipped"})
	se.step()
	se.line()
	if b := se.babysitOf(9); b.JobID == 0 && !strings.Contains(b.Wait, "re-ran") {
		t.Errorf("#9 got no round or rerun: %q", b.Wait)
	}
	if b := se.babysitOf(7); b.JobID != 0 {
		t.Errorf("#7 got a round: %q", b.Wait)
	}
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8" {
		t.Errorf("candidates made: %q", got)
	}
}

// With line_candidates off the line builds none: the front's branch is
// brought up to date and its own run is its test.
func TestLineCandidatesOffUpdatesTheFront(t *testing.T) {
	se := newStackEnv(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
		t.Fatal(err)
	}
	se.sg.behind = map[string]bool{head7s: true, head8s: true, head9s: true}
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.line()
	if len(se.made) != 0 {
		t.Errorf("candidates made: %v", se.madeHeads())
	}
	if u := se.sg.updatedPRs(); len(u) != 1 || u[0] != 7 {
		t.Errorf("branches updated %v: want only #7", u)
	}
}

func TestLineCandidatesOnAccessor(t *testing.T) {
	se := newStackEnv(t)
	ref := lineRef{forge: se.pr(7).ForgeID, repo: se.pr(7).Repo, base: "main"}
	if !se.s.lineCandidatesOn(se.ctx, ref) {
		t.Fatal("off by default")
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
		t.Fatal(err)
	}
	if se.s.lineCandidatesOn(se.ctx, ref) {
		t.Error("on though the column says off")
	}
	if !se.s.lineCandidatesOn(se.ctx, lineRef{forge: 99, repo: "x/y", base: "main"}) {
		t.Error("an unknown repo is not on")
	}
}

// F8: a repo attached to several projects is off when any of its rows says
// off, and a read that fails is off (own-branch testing is the safe mode).
func TestLineCandidatesOnWithSeveralProjects(t *testing.T) {
	se := newStackEnv(t)
	pr := se.pr(7)
	ref := lineRef{forge: pr.ForgeID, repo: pr.Repo, base: "main"}
	name := "Second"
	p2, err := se.s.CreateProject(se.ctx, se.admin, ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := se.s.AddProjectRepo(se.ctx, se.admin, p2.Key, pr.ForgeID, pr.Repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !se.s.lineCandidatesOn(se.ctx, ref) {
		t.Fatal("off with both on")
	}
	off := false
	if err := se.s.SetRepoMergePolicy(se.ctx, se.admin, p2.Key, r2.ID, MergePolicyInput{LineCandidates: &off}); err != nil {
		t.Fatal(err)
	}
	if se.s.lineCandidatesOn(se.ctx, ref) {
		t.Error("on though the second project's row says off")
	}
	defer se.breakTable("project_repos")()
	if se.s.lineCandidatesOn(se.ctx, ref) {
		t.Error("on though the read failed")
	}
}

// inLine has the line look at armed pull request id as production does once
// its babysit calls it settled: it joins, and the line acts (here, with no
// candidates possible: it updates the front's branch).
func (se *shipEnv) inLine(id int64) {
	se.t.Helper()
	pr, err := se.s.prByID(se.ctx, se.s.db.R, id)
	if err != nil {
		se.t.Fatal(err)
	}
	if b, _ := se.s.activeBabysit(se.ctx, se.s.db.R, id); b == nil {
		if _, err := se.s.StartBabysit(se.ctx, se.admin, id, BabysitInput{Via: "ship"}); err != nil {
			se.t.Fatal(err)
		}
		se.s.Wait()
	}
	se.s.joinLine(se.ctx, pr)
	se.s.lineChanged(se.ctx, lineOf(pr))
	se.s.Wait()
}

// An armed pull request that no babysit watches, behind its base, joins the
// line and is brought up to date there; then it merges as any other.
func TestLineArmedWithoutBabysitJoins(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	// Its babysit ends (a person stopped it) and none is started again.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET ended_at = ?, state = ? WHERE pr_id = ? AND ended_at IS NULL`, se.s.nowMS(), BabysitStopped, id); err != nil {
		t.Fatal(err)
	}
	if b, _ := se.s.activeBabysit(se.ctx, se.s.db.R, id); b != nil {
		t.Fatalf("a babysit is still active: %+v", b)
	}
	se.s.maybeAutoMerge(se.ctx, id)
	se.s.Wait()
	if pr := se.pr(5); pr.QueuedAt == nil {
		t.Fatalf("it didn't join the line: %+v", pr)
	}
	if se.sg.updated() != 1 || len(se.sg.merged()) != 0 {
		t.Fatalf("updates %d, merges %+v", se.sg.updated(), se.sg.merged())
	}
	se.sg.behind = nil
	se.s.maybeAutoMerge(se.ctx, id)
	se.s.Wait()
	if len(se.sg.merged()) != 1 {
		t.Fatalf("merges %+v", se.sg.merged())
	}
}

// A babysit that needs a person keeps its pull request out of the line.
func TestLineNeedsPersonStaysOut(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	se.s.Wait()
	if b, _ := se.s.activeBabysit(se.ctx, se.s.db.R, id); b == nil {
		if _, err := se.s.StartBabysit(se.ctx, se.admin, id, BabysitInput{Via: "ship"}); err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	pr, err := se.s.prByID(se.ctx, se.s.db.R, id)
	if err != nil {
		t.Fatal(err)
	}
	// A member of the line while it waits its turn; only the state below
	// takes it out.
	if members, err := se.s.lineMembers(se.ctx, lineOf(pr)); err != nil || len(members) != 1 || members[0].ID != id {
		t.Fatalf("members before it needs a person: %v, %v", members, err)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET state = ? WHERE pr_id = ? AND ended_at IS NULL`, BabysitNeedsPerson, id); err != nil {
		t.Fatal(err)
	}
	if b, _ := se.s.activeBabysit(se.ctx, se.s.db.R, id); b == nil || b.State != BabysitNeedsPerson {
		t.Fatalf("babysit %+v", b)
	}
	members, err := se.s.lineMembers(se.ctx, lineOf(pr))
	if err != nil || len(members) != 0 {
		t.Fatalf("members %v, %v", members, err)
	}
}

// A candidate whose branch gets no CI run is ended as no_run after the start
// wait, and the line builds the places again.
func TestLineCandidateWithNoRunIsEnded(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7 7,8" {
		t.Fatalf("candidates made: %q", got)
	}
	se.setClock(se.clockNow().Add(stackStartWait + time.Minute))
	se.line()
	sts := se.stacks()
	if sts[0].EndReason != endNoRun || sts[1].EndReason != endNoRun {
		t.Fatalf("ended %q and %q, want no_run", sts[0].EndReason, sts[1].EndReason)
	}
	// The two are the step's places 1 and 2: the end turned the fallback on,
	// so nothing replaced them in the same step.
	if len(sts) != 2 || len(se.made) != 2 {
		t.Errorf("candidates: %d stacks, %d made", len(sts), len(se.made))
	}
}

// The fallback after a candidate ended: the front is tested on its own
// branch, no candidate is built for lineFallbackFor, and then they are.
func TestLineFallbackAfterNoRun(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	se.setClock(se.clockNow().Add(stackStartWait + time.Minute))
	se.line()
	se.line()
	if len(se.made) != 2 {
		t.Fatalf("a candidate was built in the fallback: %v", se.madeHeads())
	}
	se.sg.behind = map[string]bool{head7s: true}
	se.line()
	if se.sg.updated() < 1 {
		t.Errorf("the front wasn't brought up to date on its own branch (updates %d)", se.sg.updated())
	}
	if len(se.made) != 2 {
		t.Errorf("candidates made in the fallback: %v", se.madeHeads())
	}
	se.setClock(se.clockNow().Add(lineFallbackFor + time.Minute))
	se.sg.behind = nil
	se.line()
	if len(se.made) <= 2 {
		t.Errorf("no candidate built once the fallback was over: %v", se.madeHeads())
	}
}

// The no_merge and refused ends turn the fallback on too, and a merge the
// forge refused ends its candidate as refused.
func TestLineFallbackAfterNoMergeAndRefused(t *testing.T) {
	for _, reason := range []string{endNoMerge, endRefused} {
		t.Run(reason, func(t *testing.T) {
			se := newStackEnv(t)
			pr := se.pr(7)
			now := se.clockNow().UnixMilli()
			if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, state, end_reason, created_at, ended_at)
				VALUES (?,?,?,?,'b','wp-queue/main/1','c','cancelled',?,?,?)`, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, reason, now, now); err != nil {
				t.Fatal(err)
			}
			se.draftGreen(head7s, 5)
			se.draftGreen(head8s, 7)
			se.step() // the sweep looks at the line too
			se.line()
			if len(se.made) != 0 {
				t.Fatalf("a candidate in the fallback: %v", se.madeHeads())
			}
		})
	}
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	st := se.stacks()[0]
	se.s.candRefused(se.ctx, se.pr(7), st, "protected branch")
	se.s.Wait()
	if got := se.stacks()[0]; got.EndReason != endRefused || !got.Ended {
		t.Errorf("refused candidate: %+v", got)
	}
}

// redBase gives main's tip a failed check for each name, as its push run
// recorded them.
func (se *stackEnv) redBase(names ...string) {
	se.t.Helper()
	pr := se.pr(7)
	tip := cmp(se.sg.baseTip, "tip-1")
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO branch_activity(forge_id, repo, branch, head_sha, pushed_at) VALUES (?, ?, 'main', ?, ?)
		ON CONFLICT(forge_id, repo, branch) DO UPDATE SET head_sha = excluded.head_sha`, pr.ForgeID, pr.Repo, tip, se.clockNow().UnixMilli()); err != nil {
		se.t.Fatal(err)
	}
	res, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO ci_heads(forge_id, repo, sha, state, seen_at) VALUES (?, ?, ?, 'failed', ?)`, pr.ForgeID, pr.Repo, tip, se.clockNow().UnixMilli())
	if err != nil {
		se.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for _, n := range names {
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO ci_checks(head_id, name, state, updated_at) VALUES (?, ?, 'failed', ?)`, id, n, se.clockNow().UnixMilli()); err != nil {
			se.t.Fatal(err)
		}
	}
}

// A job that runs only on main (advisories) fails on its tip: no head can
// pass it, so the line tests the front alone and merges it on a pass.
func TestLineBaseRedTestsTheFrontAlone(t *testing.T) {
	se := newStackEnv(t)
	se.redBase("ci / advisories (push)")
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7" {
		t.Fatalf("candidates made: %q, want the front alone", got)
	}
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	se.line()
	se.editSkipped(head7s, 30)
	se.step()
	if m := se.sg.merged(); len(m) != 1 || m[0].HeadSHA != head7s {
		t.Fatalf("merges %+v (%q)", m, se.babysitOf(7).Wait)
	}
}

// A candidate that fails only the jobs that fail on main ends base_red: no
// one is ejected, no round starts, the same heads aren't started again on
// that tip, and the wait names the job.
func TestLineBaseRedFailureOfTheBaseOnly(t *testing.T) {
	se := newStackEnv(t)
	se.redBase("ci / lint (push)") // no pull request job of that name: nothing fixes it
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7" {
		t.Fatalf("candidates made: %q", got)
	}
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	var checks []CommitCheck
	for j, name := range []string{"ci / backend (push)", "ci / lint (push)", "ci / e2e (push)"} {
		st := CheckSuccess
		if j == 1 {
			st = CheckFailed // the job that fails on main, and only it
		}
		checks = append(checks, CommitCheck{Name: name, State: st, ID: int64(700 + j), Description: "x"})
	}
	se.sg.setChecks(se.shas[0], checks...)
	se.line()
	se.step()
	sts := se.stacks()
	if sts[0].EndReason != endBaseRed || !sts[0].Ended {
		t.Fatalf("#7's candidate: ended=%v %q", sts[0].Ended, sts[0].EndReason)
	}
	if b := se.babysitOf(7); b.JobID != 0 {
		t.Errorf("#7 got a round: %q", b.Wait)
	}
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7" {
		t.Errorf("started again: %q", got)
	}
	if w := se.babysitOf(7).Wait; !strings.Contains(w, "is failing") || !strings.Contains(w, "lint") {
		t.Errorf("#7 waits %q", w)
	}
}

// F1: callers that hold the base's queue lock (readyCatchUp, the shared
// timeout) reach nudgeLine; it mustn't step the line in their goroutine, as
// lineStep takes the same lock.
func TestNudgeLineWhileHoldingTheQueueLock(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	unlock := se.s.queueLock(pr)
	done := make(chan struct{})
	go func() {
		se.s.nudgeLine(se.ctx, pr)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		unlock()
		<-done
		t.Fatal("nudgeLine waited on the queue lock its caller holds")
	}
	unlock()
	se.s.Wait()
}

// runsErrForge can't list a commit's runs.
type runsErrForge struct{ *runsAtForge }

func (runsErrForge) ActionRunsAt(context.Context, *Forge, string, string) ([]ActionRun, error) {
	return nil, errors.New("forge down")
}

// F6: green checks without a read of the commit's runs don't pass the
// candidate (a run may be unfinished); it is lost at the run limit.
func TestLineCandidateWithUnreadableRunsDoesNotPass(t *testing.T) {
	se := newStackEnv(t)
	se.s.Forges = runsErrForge{newRunsAtForge(se.sg)}
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	if len(se.made) == 0 {
		t.Fatal("no candidate")
	}
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	se.line()
	if st := se.stacks()[0]; st.State != stackRunning || st.Ended {
		t.Fatalf("candidate %s ended %v", st.State, st.Ended)
	}
	se.setClock(se.clockNow().Add(stackRunMax + time.Minute))
	se.line()
	if st := se.stacks()[0]; !st.Ended || st.EndReason != endLost {
		t.Errorf("after the run limit: %s %q", st.State, st.EndReason)
	}
}

// F7: a policy that can't be read leaves the candidate running, and lost at
// the run limit.
func TestLineCandidateWithUnreadablePolicyIsLostAtTheLimit(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	defer se.breakTable("project_repos")()
	st := se.stacks()[0]
	if got := se.s.pollCand(se.ctx, se.f, st); got.state != candRunning {
		t.Fatalf("poll %+v", got)
	}
	se.setClock(se.clockNow().Add(stackRunMax + time.Minute))
	if got := se.s.pollCand(se.ctx, se.f, st); got.state != candLost || got.reason != endLost {
		t.Errorf("after the run limit: %+v", got)
	}
}

// F11: the line reads one tip of its base, the forge's, for the fixer and the
// red test as for the candidates; Waypoint's stored tip may be older.
func TestLineGatherReadsTheForgesTip(t *testing.T) {
	gather := func(se *stackEnv) *lineState {
		t.Helper()
		pr := se.pr(7)
		ls, err := se.s.lineGather(se.ctx, lineRef{forge: pr.ForgeID, repo: pr.Repo, base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		return ls
	}
	t.Run("stored tip red, forge tip green", func(t *testing.T) {
		se := newStackEnv(t)
		se.redBase("ci / lint (push)") // on tip-1, which the database still holds
		se.draftGreen(head7s, 5)
		se.sg.baseTip = "tip-2" // green: no failed check stored for it
		if ls := gather(se); ls.snap.BaseRed || len(ls.failing) != 0 {
			t.Errorf("red on the old tip: %+v %v", ls.snap.BaseRed, ls.failing)
		}
	})
	t.Run("stored tip green, forge tip red", func(t *testing.T) {
		se := newStackEnv(t)
		se.sg.baseTip = "tip-2"
		se.redBase("ci / lint (push)")
		if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE branch_activity SET head_sha = 'tip-1'`); err != nil {
			t.Fatal(err)
		}
		se.draftGreen(head7s, 5)
		if ls := gather(se); !ls.snap.BaseRed || !ls.failing["ci / lint (push)"] && len(ls.failing) == 0 {
			t.Errorf("green on the old tip: %+v %v", ls.snap.BaseRed, ls.failing)
		}
	})
}

// The base moves between the read for the first pull request and the read
// for the one that fixes it, now the front: the failing jobs are the new
// tip's, the one the candidates are built on (audit 1507.1).
func TestLineGatherFailingIsOfTheTipItBuildsOn(t *testing.T) {
	se := newStackEnv(t)
	se.redBase("ci / lint (push)") // tip-1
	se.draftGreen(head7s, 5)
	se.sg.setChecks(head8s,
		CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess, ID: 7, Description: "Successful in 4m"},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess, ID: 8, Description: "Has been skipped"},
		CommitCheck{Name: "ci / lint (pull_request)", State: CheckSuccess, ID: 9, Description: "Successful in 1m"})
	se.poll()
	se.s.Wait()
	pr := se.pr(7)
	ref := lineRef{forge: pr.ForgeID, repo: pr.Repo, base: "main"}
	ls, err := se.s.lineGather(se.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.order) == 0 || ls.order[0].Number != 8 || !ls.snap.FrontFixes || !ls.snap.BaseRed {
		t.Fatalf("#8 fixes the red base and goes first: %+v %+v", ls.order, ls.snap)
	}
	se.sg.sm.Lock()
	se.sg.tipReads, se.sg.tipThen = 0, "tip-2" // green: no failed check stored for it
	se.sg.sm.Unlock()
	ls, err = se.s.lineGather(se.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if ls.tip != "tip-2" || ls.snap.BaseRed || len(ls.failing) != 0 || ls.snap.FrontFixes {
		t.Errorf("tip %q with tip-1's failures: red %v, failing %v, front fixes %v", ls.tip, ls.snap.BaseRed, ls.failing, ls.snap.FrontFixes)
	}
}

// F15: a line whose babysits can't be read acts on nothing: a live candidate
// isn't ended, nothing merges and no pull request is dropped.
func TestLineStepActsOnNothingWithUnreadableBabysits(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	states, waits := map[int]string{}, map[int]string{}
	for _, n := range []int{7, 8} {
		b := se.babysitOf(n)
		states[n], waits[n] = b.State, b.Wait
	}
	before := len(se.stacks())
	madeBefore := len(se.made)
	restore := se.breakTable("babysits")
	se.line()
	se.s.sweepLines(se.ctx)
	se.s.Wait()
	restore()
	sts := se.stacks()
	if len(sts) != before || len(se.made) != madeBefore {
		t.Errorf("candidates changed: %d -> %d rows, %d -> %d made", before, len(sts), madeBefore, len(se.made))
	}
	for _, st := range sts {
		if st.Ended {
			t.Errorf("candidate %d ended (%q)", st.ID, st.EndReason)
		}
	}
	if m := se.sg.merged(); len(m) != 0 {
		t.Errorf("merged %+v", m)
	}
	for _, n := range []int{7, 8} {
		if b := se.babysitOf(n); b == nil || b.State != states[n] || b.Wait != waits[n] {
			t.Errorf("#%d changed: %+v (was %q %q)", n, b, states[n], waits[n])
		}
	}
}

// F31: a commit's live candidate names its line, and a forge's webhook for
// that commit steps the line (it doesn't read the checks as a branch's).
func TestCandLineByCommit(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	st := se.stacks()[0]
	pr := se.pr(7)
	if ref, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID, strings.ToUpper(pr.Repo), strings.ToUpper(st.CommitSHA)); !ok || ref.repo != pr.Repo || ref.base != "main" || ref.forge != pr.ForgeID {
		t.Errorf("a live candidate's commit: %+v %v", ref, ok)
	}
	if _, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID+1, pr.Repo, st.CommitSHA); ok {
		t.Error("another forge's")
	}
	if _, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID, "other/repo", st.CommitSHA); ok {
		t.Error("another repo's")
	}
	if _, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID, pr.Repo, ""); ok {
		t.Error("no commit")
	}
	if _, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID, pr.Repo, head7s); ok {
		t.Error("a pull request's head isn't a candidate")
	}
	// The webhook for the commit: the run passed, so the line records it.
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	se.s.onForgeCI(se.ctx, se.f, pr.Repo, st.CommitSHA)
	se.s.Wait()
	if got := se.stacks()[0]; got.State == stackRunning {
		t.Errorf("the line didn't step on its candidate's webhook: %+v", got)
	}
	// An ended candidate is no longer one.
	se.s.candEnd(se.ctx, se.f, se.stacks()[0], stackFailed, endFailed, "x")
	if _, ok := se.s.candLineByCommit(se.ctx, pr.ForgeID, pr.Repo, st.CommitSHA); ok {
		t.Error("an ended candidate's commit")
	}
}

// F32: an open pull request marked ready on a candidate keeps its title and
// the time it was marked, and the candidate's run is its full run at this head.
func TestMarkReadyOnACandidateForAnOpenPR(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	st := se.stacks()[0]
	read := func() (readyAt int64, sha, checks string) {
		t.Helper()
		var at sql.NullInt64
		if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT ready_at, ready_sha, ready_checks FROM pull_requests WHERE number = 7`).Scan(&at, &sha, &checks); err != nil {
			t.Fatal(err)
		}
		return at.Int64, sha, checks
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'open', ready_at = NULL, ready_sha = '', ready_checks = '["x"]' WHERE number = 7`); err != nil {
		t.Fatal(err)
	}
	pr := se.pr(7)
	if err := se.s.markReadyOn(se.ctx, pr, st); err != nil {
		t.Fatal(err)
	}
	at, sha, checks := read()
	if at == 0 || !sameSHA(sha, head7s) || checks != "[]" {
		t.Fatalf("after marking: ready_at %d ready_sha %q ready_checks %q", at, sha, checks)
	}
	if got := se.pr(7).Title; got != pr.Title {
		t.Errorf("title changed: %q -> %q", pr.Title, got)
	}
	// Marked at an earlier head: the time stays, the head is this one's.
	se.setClock(se.clockNow().Add(time.Hour))
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET ready_sha = 'old' WHERE number = 7`); err != nil {
		t.Fatal(err)
	}
	if err := se.s.markReadyOn(se.ctx, se.pr(7), st); err != nil {
		t.Fatal(err)
	}
	if at2, sha2, _ := read(); at2 != at || !sameSHA(sha2, head7s) {
		t.Errorf("again: ready_at %d (was %d) ready_sha %q", at2, at, sha2)
	}
}

// F33: a head that failed on a candidate with heads ahead of it says so, and
// names them; the first place has none to name.
func TestCandEjectTextNamesTheHeadsAhead(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	sts := se.stacks()
	if len(sts) != 2 {
		t.Fatalf("candidates: %d", len(sts))
	}
	se.s.candEnd(se.ctx, se.f, sts[0], stackFailed, endFailed, "lint failed")
	se.s.candEnd(se.ctx, se.f, sts[1], stackFailed, endFailed, "lint failed")
	if got := se.s.candEjectText(se.ctx, se.pr(8)); !strings.Contains(got, "and the heads ahead of it (#7)") || !strings.Contains(got, "main at "+short(sts[1].BaseSHA)) {
		t.Errorf("#8: %q", got)
	}
	if got := se.s.candEjectText(se.ctx, se.pr(7)); got == "" || strings.Contains(got, "heads ahead") {
		t.Errorf("#7: %q", got)
	}
}

// What fails on the base can't be read: that is an error to the line's
// order and fixer, not a base with nothing failing, so lineGather decides
// nothing on it and a candidate's failure of the base's own job isn't taken
// for the pull request's (c6385 of #569).
func TestLineUnreadableBaseChecksIsAnError(t *testing.T) {
	se := newStackEnv(t)
	se.redBase("ci / lint (push)")
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	if _, failing, err := se.s.lineOrder(se.ctx, []*PullRequest{pr}, "tip-1"); err != nil || len(failing) != 1 {
		t.Fatalf("read: %v, %v", failing, err)
	}
	defer se.breakTable("ci_checks")()
	if failing, err := se.s.baseFailingAt(se.ctx, pr, "tip-1"); err == nil {
		t.Errorf("an unreadable base read as failing %v", failing)
	}
	if _, failing, err := se.s.baseFailing(se.ctx, pr); err == nil {
		t.Errorf("an unreadable base at its stored tip read as failing %v", failing)
	}
	if _, _, err := se.s.lineOrder(se.ctx, []*PullRequest{pr}, "tip-1"); err == nil {
		t.Error("the line was ordered without what fails on its base")
	}
}

// The forge doesn't take the request to run a failed candidate's jobs again:
// the candidate isn't marked run again, so its first failure isn't read as a
// second and the front stays; the next step asks again (c6393 of #569).
func TestLineRejectedRerunIsNotARerun(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	se.stackRun(0, CheckFailed, treeA, "7 "+head7s)
	se.sg.sm.Lock()
	se.sg.retryErr = errors.New("502 from the forge")
	se.sg.sm.Unlock()
	se.line()
	se.setClock(se.clockNow().Add(lineRerunSettle + time.Minute))
	se.line()
	if sts := se.stacks(); sts[0].RerunAt != 0 || sts[0].Ended {
		t.Fatalf("a rejected re-run counted: reran %d, ended %v %q", sts[0].RerunAt, sts[0].Ended, sts[0].EndReason)
	}
	se.sg.sm.Lock()
	se.sg.retryErr = nil
	se.sg.sm.Unlock()
	se.line()
	if sts := se.stacks(); sts[0].RerunAt == 0 || sts[0].Ended {
		t.Fatalf("not run again once the forge took it: %+v", sts[0])
	}
	// A forge that goes on refusing: after stackRunMax the failure counts.
	se2 := newStackEnv(t)
	se2.draftGreen(head7s, 5)
	se2.step()
	se2.line()
	se2.stackRun(0, CheckFailed, treeA, "7 "+head7s)
	se2.sg.sm.Lock()
	se2.sg.retryErr = errors.New("502 from the forge")
	se2.sg.sm.Unlock()
	se2.setClock(se2.clockNow().Add(stackRunMax + time.Minute))
	se2.line()
	if sts := se2.stacks(); sts[0].RerunAt == 0 {
		t.Fatalf("a forge that refuses for good holds the front forever: %+v", sts[0])
	}
	se2.setClock(se2.clockNow().Add(lineRerunSettle + time.Minute))
	se2.line()
	if sts := se2.stacks(); !sts[0].Ended || sts[0].EndReason != endFailed {
		t.Errorf("the failure didn't count after the run limit: %s ended %v %q", sts[0].State, sts[0].Ended, sts[0].EndReason)
	}
	if p := se2.pr(7); p.QueuedAt != nil {
		t.Errorf("#7 still in the line after its candidate failed for good")
	}
}

// checksErrForge can't read a commit's checks.
type checksErrForge struct{ *shipForge }

func (checksErrForge) CommitChecks(context.Context, *Forge, string, string) ([]CommitCheck, error) {
	return nil, errors.New("forge down")
}

// A candidate whose checks can't be read is still running, and lost at the
// run limit: a forge that never answers doesn't hold the line for ever.
func TestLineCandidateWithUnreadableChecksIsLostAtTheLimit(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	st := se.stacks()[0]
	se.s.Forges = checksErrForge{se.sg}
	if got := se.s.pollCand(se.ctx, se.f, st); got.state != candRunning {
		t.Fatalf("poll %+v", got)
	}
	se.setClock(se.clockNow().Add(stackRunMax + time.Minute))
	if got := se.s.pollCand(se.ctx, se.f, st); got.state != candLost || got.reason != endLost {
		t.Errorf("after the run limit: %+v", got)
	}
}

// The front left the line after the step read it: its branch isn't brought
// up to date and its draft isn't marked ready (as for a merge). Staying, it
// is updated, so a lineHolds that always said no would fail (WP-1700).
func TestLineUpdateActSkipsAFrontThatLeft(t *testing.T) {
	for _, leaves := range []bool{true, false} {
		t.Run(fmt.Sprintf("leaves=%v", leaves), func(t *testing.T) {
			se := newStackEnv(t)
			if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
				t.Fatal(err)
			}
			se.sg.behind = map[string]bool{head7s: true}
			se.draftGreen(head7s, 5)
			pr := se.pr(7)
			if pr.QueuedAt == nil {
				se.s.joinLine(se.ctx, pr)
				pr = se.pr(7)
			}
			ls, err := se.s.lineGather(se.ctx, lineOf(pr))
			if err != nil {
				t.Fatal(err)
			}
			acts := planLine(ls.snap)
			if len(acts) != 1 || acts[0].Kind != lineUpdate {
				t.Fatalf("plan: %+v", acts)
			}
			before := len(se.sg.updatedPRs()) // the step that read its checks updated it once
			// The base moved again, so an act that runs updates it (an update is
			// tried once per tip).
			se.sg.baseTip = "tip-9"
			se.sg.behind = map[string]bool{head7s: true}
			stateBefore := se.pr(7).State
			if leaves {
				se.s.leaveQueue(se.ctx, pr.ID)
			}
			se.s.lineAct(se.ctx, ls, acts[0])
			se.s.Wait()
			u := se.sg.updatedPRs()
			if leaves {
				if len(u) != before {
					t.Errorf("updated after it left the line: %v", u)
				}
				if p := se.pr(7); p.State != stateBefore {
					t.Errorf("state %s after it left the line, was %s", p.State, stateBefore)
				}
			} else if len(u) != before+1 {
				t.Errorf("not updated though it holds its place: %v (before %d)", u, before)
			}
		})
	}
}

// baseFailing with no stored tip of the base reads nothing failing; a tip
// that can't be read is an error.
func TestBaseFailingWithoutATipAndUnreadable(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	if _, err := se.s.db.W.ExecContext(se.ctx, `DELETE FROM branch_activity`); err != nil {
		t.Fatal(err)
	}
	if tip, failing, err := se.s.baseFailing(se.ctx, pr); err != nil || tip != "" || len(failing) != 0 {
		t.Errorf("no stored tip: %q %v %v", tip, failing, err)
	}
	defer se.breakTable("branch_activity")()
	if tip, failing, err := se.s.baseFailing(se.ctx, pr); err == nil {
		t.Errorf("an unreadable tip read as %q failing %v", tip, failing)
	}
}

// queueAhead orders as the line does: the pull request that fixes a red base
// isn't behind the one armed before it (c6396 of #569).
func TestQueueAheadPutsTheFixerFirst(t *testing.T) {
	se := newStackEnv(t)
	se.redBase("ci / lint (push)")
	se.draftGreen(head7s, 5)
	se.sg.setChecks(head8s,
		CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess, ID: 7, Description: "Successful in 4m"},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess, ID: 8, Description: "Has been skipped"},
		CommitCheck{Name: "ci / lint (pull_request)", State: CheckSuccess, ID: 9, Description: "Successful in 1m"})
	se.poll()
	se.s.Wait()
	if a, err := se.s.queueAhead(se.ctx, se.pr(8)); err != nil || len(a) != 0 {
		t.Errorf("ahead of #8, which fixes main: %v, %v", a, err)
	}
	if a, err := se.s.queueAhead(se.ctx, se.pr(7)); err != nil || len(a) != 1 || a[0].Number != 8 {
		t.Errorf("ahead of #7: %v, %v", a, err)
	}
	restore := se.breakTable("ci_checks")
	defer restore()
	if _, err := se.s.queueAhead(se.ctx, se.pr(7)); err == nil {
		t.Error("the order was given without what fails on the base")
	}
}

// The front left the line after the step read it (its babysit asked a person
// about a round's held change): the step doesn't mark it ready on its passed
// candidate or merge it (c6384 of #569). Staying, the same step acts on it
// (WP-1700).
func TestLineMergeActSkipsAFrontThatLeft(t *testing.T) {
	for _, leaves := range []bool{true, false} {
		t.Run(fmt.Sprintf("leaves=%v", leaves), func(t *testing.T) {
			se := newStackEnv(t)
			se.draftGreen(head7s, 5)
			se.step()
			se.line()
			se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
			se.sg.trees = map[string]string{"3e7000000": treeA}
			pr := se.pr(7)
			ls, err := se.s.lineGather(se.ctx, lineOf(pr))
			if err != nil {
				t.Fatal(err)
			}
			acts := planLine(ls.snap)
			if len(acts) != 1 || acts[0].Kind != lineMerge {
				t.Fatalf("plan: %+v", acts)
			}
			if leaves {
				se.s.leaveQueue(se.ctx, pr.ID) // as heldWait does, outside the line's lock
			}
			se.s.lineAct(se.ctx, ls, acts[0])
			se.s.Wait()
			if !leaves {
				if p := se.pr(7); p.State == "draft" {
					t.Errorf("not marked ready though it holds its place: %s", p.State)
				}
				if sts := se.stacks(); sts[0].State != stackReady {
					t.Errorf("its candidate was not recorded ready: %s", sts[0].State)
				}
				return
			}
			if m := se.sg.merged(); len(m) != 0 {
				t.Errorf("merged after it left the line: %+v", m)
			}
			if p := se.pr(7); p.State != "draft" {
				t.Errorf("marked ready after it left the line: %s", p.State)
			}
			if sts := se.stacks(); sts[0].State == stackReady {
				t.Errorf("its candidate was recorded ready")
			}
		})
	}
}

// A candidate for #7 and #8 ends base_red after #7 merged: it is recorded as
// the test of #8 on the tip that holds #7, which is what keeps #8 from being
// started again while that tip is red (c6389 of #569).
func TestLineBaseRedIsRecordedOnTheTipItEndedOn(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.line()
	pr := se.pr(7)
	ref := lineOf(pr)
	ls, err := se.s.lineGather(se.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var both *queueStack
	for _, st := range ls.rows {
		if len(st.Heads) == 2 {
			both = st
		}
	}
	if both == nil {
		t.Fatalf("no candidate for #7 and #8: %q", se.madeHeads())
	}
	both.Merged, ls.tip = 1, "3e7000000" // #7 merged on its own candidate
	se.s.lineAct(se.ctx, ls, lineAction{Kind: lineEnd, Cand: both.ID, End: endBaseRed, Why: "it failed only what fails on the base's tip"})
	rest := both.Heads[1:]
	if tried := se.s.lineTried(se.ctx, ref, "3e7000000", true); !tried[candKey(rest)] {
		t.Errorf("#8 alone isn't remembered on the tip that holds #7: %v", tried)
	}
	if tried := se.s.lineTried(se.ctx, ref, "tip-9", true); tried[candKey(rest)] {
		t.Error("remembered on another tip too")
	}
}

// Unread, the line's pause and a live candidate fail closed: no candidates
// are built, and a branch isn't updated under a candidate (WP-1700).
func TestLineFallbackAndInCandidateFailClosedWhenUnread(t *testing.T) {
	se := newStackEnv(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
		t.Fatal(err)
	}
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	ref := lineOf(pr)
	if se.s.lineFallback(se.ctx, ref) || se.s.inCandidate(se.ctx, pr.ID) {
		t.Fatal("no candidate rows, yet the line is paused or a candidate holds #7")
	}
	restore := se.breakTable("queue_stacks")
	defer restore()
	if !se.s.lineFallback(se.ctx, ref) {
		t.Error("an unread pause isn't taken as paused")
	}
	if !se.s.inCandidate(se.ctx, pr.ID) {
		t.Error("an unread candidate list isn't taken as holding #7")
	}
}

// A live candidate row that can't be read is left out of the line's reading,
// and the rest is read (WP-1700).
func TestLineGatherSkipsAnUnreadableLiveCandidate(t *testing.T) {
	se := newStackEnv(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
		t.Fatal(err)
	}
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, branch, created_at)
		VALUES (?, ?, ?, ?, 'x', '[]', 'wp-queue/main/bad', 1)`, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID); err != nil {
		t.Fatal(err)
	}
	ls, err := se.s.lineGather(se.ctx, lineOf(pr))
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.snap.Cands) != 0 || len(ls.rows) != 0 {
		t.Errorf("the unreadable row is in the snapshot: %+v", ls.snap.Cands)
	}
}
