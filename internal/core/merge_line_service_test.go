package core

import (
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
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET state = ? WHERE pr_id = ? AND ended_at IS NULL`, BabysitNeedsPerson, id); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET queued_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	pr, err := se.s.prByID(se.ctx, se.s.db.R, id)
	if err != nil {
		t.Fatal(err)
	}
	if se.s.joinUnbabysat(se.ctx, pr) || pr.QueuedAt != nil {
		t.Fatalf("a pull request that needs a person joined the line")
	}
	members, err := se.s.lineMembers(se.ctx, lineOf(pr))
	if err != nil || len(members) != 0 {
		t.Fatalf("members %v, %v", members, err)
	}
}
