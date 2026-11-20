package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// ciStacked runs on a ready pull request's edit and on pushes to merge queue
// stacks: the repo opts in to stacks (WP-404).
const ciStacked = "on:\n  push:\n    branches: [main, 'wp-queue/**']\n  pull_request:\n    types: [opened, synchronize, reopened, edited]\njobs: {}\n"

func (g *shipForge) CommitParents(ctx context.Context, f *Forge, repo, sha string) ([]string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	ps, ok := g.parents[sha]
	if !ok {
		return nil, fmt.Errorf("no commit %s", sha)
	}
	return slices.Clone(ps), nil
}

func (g *shipForge) CancelRunsAt(ctx context.Context, f *Forge, repo, sha string) (int, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.cancelled = append(g.cancelled, sha)
	return 1, nil
}

func (g *shipForge) StackRules(ctx context.Context, f *Forge, repo, base string) (string, bool, error) {
	return "merge", false, nil
}

func (g *shipForge) DeleteBranch(ctx context.Context, f *Forge, repo, branch string) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.deleted = append(g.deleted, branch)
	return nil
}

// stackEnv: three armed drafts, #7 #8 #9, in a repo that opts in to stacks.
type stackEnv struct {
	*bsEnv
	made []CommitInput // stack branches made, in order
	shas []string      // their commits
}

const head7s, head8s, head9s = "7777777aaaa", "8888888cccc", "9999999dddd"

func newStackEnv(t *testing.T) *stackEnv {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciStacked
	be.runnerOnline()
	se := &stackEnv{bsEnv: be}
	be.sg.parents = map[string][]string{}
	be.sg.mergeSHAs = map[int]string{7: "3e7000000", 8: "3e8000000", 9: "3e9000000"}
	be.sg.commitFn = func(in CommitInput) (*ForgeCommit, error) {
		be.sg.sm.Lock()
		defer be.sg.sm.Unlock()
		sha := fmt.Sprintf("5ac%d%03d", len(se.made), len(se.made))
		se.made = append(se.made, in)
		se.shas = append(se.shas, sha)
		be.sg.parents[sha] = []string{cmp(be.sg.baseTip, "tip-1")}
		return &ForgeCommit{SHA: sha, Branch: in.Branch}, nil
	}
	// A merge moves main: its merge commit's parents are the old tip and
	// the head, and every head still open now lacks the tip.
	be.sg.onMergeN = func(n int) {
		m := be.sg.mergeSHAs[n]
		head := map[int]string{7: head7s, 8: head8s, 9: head9s}[n]
		be.sg.parents[m] = []string{cmp(be.sg.baseTip, "tip-1"), head}
		be.sg.baseTip = m
		be.sg.behind = map[string]bool{head7s: true, head8s: true, head9s: true}
		delete(be.sg.behind, head)
	}
	be.queuedDraft(7, "add-export", head7s)
	be.queuedDraft(8, "add-import", head8s)
	be.queuedDraft(9, "add-sync", head9s)
	return se
}

// draftGreen: the draft tier passed on sha.
func (se *stackEnv) draftGreen(sha string, id int64) {
	se.draftChecks(sha, CommitCheck{State: CheckSuccess, ID: id, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: id + 1, Description: "Has been skipped"})
}

// fullGreen: the full run passed on sha.
func (se *stackEnv) fullGreen(sha string, id int64) {
	se.draftChecks(sha, CommitCheck{State: CheckSuccess, ID: id, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: id + 1, Description: "Successful in 24m"})
}

// editSkipped: the run for marking it ready skipped its checks.
func (se *stackEnv) editSkipped(sha string, id int64) {
	se.draftChecks(sha, CommitCheck{State: CheckSuccess, ID: id, Description: "Has been skipped"}, CommitCheck{State: CheckSuccess, ID: id + 1, Description: "Has been skipped"})
}

// stackRun gives stack i's commit its push run's checks, and the lint job's
// log what the stack step printed.
func (se *stackEnv) stackRun(i int, state string, tree string, heads ...string) {
	se.t.Helper()
	sha := se.shas[i]
	var checks []CommitCheck
	for j, name := range []string{"ci / backend (push)", "ci / lint (push)", "ci / e2e (push)"} {
		checks = append(checks, CommitCheck{Name: name, State: state, ID: int64(500 + 10*i + j), Description: "Successful in 20m", URL: "https://forge/me/app/actions/runs/" + sha + "/jobs/" + fmt.Sprint(j)})
	}
	se.sg.setChecks(sha, checks...)
	var log strings.Builder
	log.WriteString("checkout\n")
	for _, h := range heads {
		n, s, _ := strings.Cut(h, " ")
		fmt.Fprintf(&log, "2026-11-02T10:00:00Z waypoint-stack #%s %s\n", n, s)
	}
	fmt.Fprintf(&log, "2026-11-02T10:00:01Z waypoint-stack-tree %s\nok  \tpkg\n", tree)
	se.sg.sm.Lock()
	se.sg.logs[sha+" ci / lint (push)"] = log.String()
	se.sg.sm.Unlock()
}

func (se *stackEnv) step() {
	se.s.CheckBabysits(se.ctx)
	se.s.Wait()
}

func (se *stackEnv) stacks() []*queueStack {
	se.t.Helper()
	rows, err := se.s.db.R.QueryContext(se.ctx, `SELECT `+stackCols+` FROM queue_stacks ORDER BY id`)
	if err != nil {
		se.t.Fatal(err)
	}
	defer rows.Close()
	var out []*queueStack
	for rows.Next() {
		st, err := scanStack(rows)
		if err != nil {
			se.t.Fatal(err)
		}
		out = append(out, st)
	}
	return out
}

func stackStates(list []*queueStack) string {
	var s []string
	for _, st := range list {
		s = append(s, st.name()+"="+st.State)
	}
	return strings.Join(s, ", ")
}

var treeA, treeB = strings.Repeat("a", 40), strings.Repeat("b", 40)

// The front's full run and the stacks behind it run at once; when the front
// merges, the next merges on its passed stack with no update and no full
// run of its own, and so does the one after it. Each stacked merge is
// marked tested from the stack's tree.
func TestStackedQueueMergesThreeOnOneRound(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	if pr := se.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready: %+v", pr)
	}
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made: %+v", se.made)
	}
	if !strings.HasPrefix(se.made[0].Branch, "wp-queue/main/8-") || se.made[0].Base != "main" || len(se.made[0].Files) != 1 || se.made[0].Files[0].Path != stackFile ||
		!strings.Contains(se.made[0].Files[0].Content, "\n7 "+head7s+"\n8 "+head8s+"\n") {
		t.Fatalf("#8's stack: %+v", se.made[0])
	}
	if !strings.Contains(se.made[1].Files[0].Content, "\n7 "+head7s+"\n8 "+head8s+"\n9 "+head9s+"\n") {
		t.Fatalf("#9's stack: %+v", se.made[1])
	}
	if w := se.babysitOf(8).Wait; !strings.Contains(w, "tested on a stack (main + #7 + #8): its run is going") {
		t.Errorf("#8 waits %q", w)
	}

	// The stacks pass while #7's full run goes on.
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.stackRun(1, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=passed, main + #7 + #8 + #9=passed" {
		t.Fatalf("stacks: %s", got)
	}
	if w := se.babysitOf(9).Wait; !strings.Contains(w, "passed; it merges without an update or a full run") {
		t.Errorf("#9 waits %q", w)
	}

	// #7 merges. #8, at the front, is marked ready on its stack.
	se.sg.trees = map[string]string{"3e8000000": treeA, "3e9000000": treeB}
	se.fullGreen(head7s, 20)
	se.step()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v", m)
	}
	if pr := se.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#8 not marked ready on its stack: %+v (babysit %q)", pr, se.babysitOf(8).Wait)
	}
	if st := se.sg.lastStatus(head8s); st.Context != stackContext || st.State != "success" || !strings.Contains(st.Description, "main + #7 + #8") {
		t.Errorf("#8's head status %+v", st)
	}
	// Its edit's run skips its checks: it merges, tested from the stack.
	se.editSkipped(head8s, 30)
	se.step()
	if m := se.sg.merged(); len(m) != 2 || m[1].HeadSHA != head8s {
		t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(8).Wait)
	}
	if st := se.sg.lastStatus("3e8000000"); st.Context != testedContext || !strings.Contains(st.Description, "main + #7 + #8") {
		t.Errorf("#8's merge status %+v", st)
	}

	// And #9 the same way.
	se.step()
	if pr := se.pr(9); pr.State != "open" {
		t.Fatalf("#9 not marked ready on its stack: %+v (babysit %q)", pr, se.babysitOf(9).Wait)
	}
	se.editSkipped(head9s, 40)
	se.step()
	if m := se.sg.merged(); len(m) != 3 || m[2].HeadSHA != head9s {
		t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(9).Wait)
	}
	if st := se.sg.lastStatus("3e9000000"); st.Context != testedContext || !strings.Contains(st.Description, "#9") {
		t.Errorf("#9's merge status %+v", st)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v: the stacks stood in for that", u)
	}
	if got := stackStates(se.stacks()); got != "main + #7 + #8=merged, main + #7 + #8 + #9=merged" {
		t.Errorf("stacks: %s", got)
	}
	if len(se.sg.deleted) != 2 {
		t.Errorf("branches deleted: %v", se.sg.deleted)
	}
}

// A step of the front's babysit that comes while its own merge on its stack
// is going (the forge's base already holds it, Waypoint's record doesn't
// yet) waits: the base's tip is the stack's chain with the front's head
// merged on it too, not a base that moved past the stack, so neither its
// stack nor the ones built on it are dropped (WP-576).
func TestStackedFrontStepDuringItsMerge(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.stackRun(1, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	se.sg.trees = map[string]string{"3e8000000": treeA, "3e9000000": treeB}
	se.fullGreen(head7s, 20)
	se.step()
	pr8 := se.pr(8)
	if pr8.State != "open" {
		t.Fatalf("#8 not marked ready on its stack: %+v (babysit %q)", pr8, se.babysitOf(8).Wait)
	}

	// The forge merges #8; its edit's run skipped its checks.
	f, err := se.s.forgeByID(se.ctx, pr8.ForgeID)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := se.sg.GetPR(se.ctx, f, pr8.Repo, 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := se.sg.MergePR(se.ctx, f, pr8.Repo, fp, MergeOptions{Style: "merge", HeadSHA: head8s}); err != nil {
		t.Fatal(err)
	}
	se.sg.sm.Lock()
	se.sg.behind[head8s] = true // the merge commit isn't in its head
	se.sg.sm.Unlock()
	se.sg.setChecks(head8s, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess, ID: 30, Description: "Has been skipped"},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess, ID: 31, Description: "Has been skipped"})
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET ci_state = ?, ci_sha = ? WHERE id = ?`, CIPassed, head8s, pr8.ID); err != nil {
		t.Fatal(err)
	}
	// A step of #8's babysit before Waypoint records the merge.
	se.s.Wait()
	se.s.babysitStep(se.ctx, pr8.ID, false)
	if got := stackStates(se.stacks()); got != "main + #7 + #8=ready, main + #7 + #8 + #9=passed" {
		t.Fatalf("stacks: %s (#8 %q)", got, se.babysitOf(8).Wait)
	}
	if pr := se.pr(8); pr.State == "draft" {
		t.Fatalf("#8 put back to draft: %+v", pr)
	}
	se.s.Wait()

	// Once the merge is recorded, #9 goes on its stack.
	merged, err := se.sg.GetPR(se.ctx, f, pr8.Repo, 8)
	if err != nil {
		t.Fatal(err)
	}
	se.pullHook("closed", *merged)
	se.s.Wait()
	se.step()
	se.step()
	if pr := se.pr(8); pr.State != "merged" {
		t.Fatalf("#8's merge not recorded: %+v (babysit %q)", pr, se.babysitOf(8).Wait)
	}
	if pr := se.pr(9); pr.State != "open" {
		t.Fatalf("#9 not marked ready on its stack: %+v (babysit %q; stacks %s)", pr, se.babysitOf(9).Wait, stackStates(se.stacks()))
	}
	se.editSkipped(head9s, 40)
	se.step()
	if m := se.sg.merged(); len(m) != 3 || m[2].HeadSHA != head9s {
		t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(9).Wait)
	}
	if got := stackStates(se.stacks()); got != "main + #7 + #8=merged, main + #7 + #8 + #9=merged" {
		t.Errorf("stacks: %s", got)
	}
}

// The front's full run fails: it keeps its place for its round (WP-455),
// but the stacks built on its head are cancelled with their runs and none is
// made on it again. Its round's push brings new stacks on its new head.
func TestStackedQueueFrontFails(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made: %+v", se.made)
	}
	se.draftChecks(head7s, CommitCheck{State: CheckSuccess, ID: 20, Description: "Successful in 16m"}, CommitCheck{State: CheckFailed, ID: 21, Description: "Failing after 20m"})
	se.rerunFails()
	se.step()
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=cancelled, main + #7 + #8 + #9=cancelled" {
		t.Fatalf("stacks: %s (#8 %q)", got, se.babysitOf(8).Wait)
	}
	if len(se.made) != 2 {
		t.Errorf("stacked on #7's failed head again: %+v", se.made[2:])
	}
	if !slices.Contains(se.sg.cancelled, se.shas[0]) || !slices.Contains(se.sg.cancelled, se.shas[1]) {
		t.Errorf("runs cancelled on %v", se.sg.cancelled)
	}
	if len(se.sg.deleted) != 2 {
		t.Errorf("branches deleted: %v", se.sg.deleted)
	}
	// The round's fix: stacked again, on the new head.
	se.finish(se.babysitOf(7).JobID, true)
	p7 := bsPR
	p7.Number, p7.HeadRef, p7.BaseRef, p7.Title = 7, "add-export", "main", "Change add-export"
	se.pushTo(p7, "7777777bbbb")
	se.step()
	if len(se.made) < 3 || !strings.Contains(se.made[2].Files[0].Content, "\n7 7777777bbbb\n8 "+head8s+"\n") {
		t.Fatalf("not stacked on #7's new head: %d made (#7 %+v, #8 %q)", len(se.made), se.pr(7), se.babysitOf(8).Wait)
	}
}

// A stack whose run fails ends, with those built on it; the pull request
// waits its turn the usual way, and isn't stacked on the same heads again.
func TestStackedQueueStackFails(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.stackRun(0, CheckFailed, treeA, "7 "+head7s, "8 "+head8s)
	se.step()
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=failed, main + #7 + #8 + #9=cancelled" {
		t.Fatalf("stacks: %s", got)
	}
	// #7 merges; #8 is brought up to date for its own full run.
	se.fullGreen(head7s, 20)
	se.step()
	if u := se.sg.updatedPRs(); !slices.Contains(u, 8) {
		t.Fatalf("#8 not updated at its turn: %v (babysit %q)", u, se.babysitOf(8).Wait)
	}
}

// main moves outside the queue before the next merges on its stack: the
// chain is broken, so it is updated and runs the full suite as before.
func TestStackedQueueBrokenChain(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.step()
	se.sg.onMergeN = func(n int) {
		// someone else's push landed on main after #7's merge
		se.sg.parents["3e7000000"] = []string{"tip-1", head7s}
		se.sg.parents["0ther000"] = []string{"3e7000000"}
		se.sg.baseTip = "0ther000"
		se.sg.behind = map[string]bool{head8s: true}
	}
	se.fullGreen(head7s, 20)
	se.step()
	if st := se.sg.lastStatus(head8s); st.Context == stackContext && st.State == "success" {
		t.Fatalf("#8 marked ready on a broken stack: %+v", st)
	}
	if u := se.sg.updatedPRs(); !slices.Contains(u, 8) {
		t.Fatalf("#8 not updated: %v (babysit %q)", u, se.babysitOf(8).Wait)
	}
	if got := stackStates(se.stacks()); got != "main + #7 + #8=cancelled" {
		t.Errorf("stacks: %s", got)
	}
}

// A push to the stacked pull request ends its stack.
func TestStackedQueueHeadMoves(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	p := bsPR
	p.Number, p.HeadRef, p.HeadSHA, p.BaseRef, p.Title = 8, "add-import", "8888888eeee", "main", "WIP: Change add-import"
	se.pullHook("synchronize", p)
	se.s.Wait()
	se.drain()
	se.step()
	if got := stackStates(se.stacks()); !strings.HasPrefix(got, "main + #7 + #8=cancelled") {
		t.Errorf("stacks: %s", got)
	}
}

// reviewOver finishes pull request n's review with no findings.
func (se *stackEnv) reviewOver(n int) {
	se.t.Helper()
	pr := se.pr(n)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE bot_runs SET status = 'done', finished_at = ? WHERE status = 'running' AND pr_id = ?`, se.s.nowMS(), pr.ID); err != nil {
		se.t.Fatal(err)
	}
	if err := se.s.evalGate(se.ctx, pr.ID); err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

// Drafts whose reviews are still going aren't stacked (WP-386, undoing
// WP-489): a review that leads to a round moves the head and throws the
// stack's run away. Once the reviews are over the stacks are made, and the
// next merges on its stack with no update or full run of its own.
func TestStackedQueueWhileReviewsGo(t *testing.T) {
	se := newStackEnv(t)
	se.reviewRunning(7)
	se.reviewRunning(8)
	se.reviewRunning(9)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if pr := se.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready with its review going: %+v (babysit %q)", pr, se.babysitOf(7).Wait)
	}
	if len(se.made) != 0 {
		t.Fatalf("stacks made with reviews going: %d (#8 %q, #9 %q)", len(se.made), se.babysitOf(8).Wait, se.babysitOf(9).Wait)
	}
	se.reviewOver(8)
	se.reviewOver(9)
	se.step()
	if len(se.made) != 0 {
		t.Fatalf("stacks made on a front whose review is going: %d", len(se.made))
	}

	se.reviewOver(7)
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made: %d (#8 %q, #9 %q)", len(se.made), se.babysitOf(8).Wait, se.babysitOf(9).Wait)
	}
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.stackRun(1, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=passed, main + #7 + #8 + #9=passed" {
		t.Fatalf("stacks: %s", got)
	}

	se.sg.trees = map[string]string{"3e8000000": treeA, "3e9000000": treeB}
	se.fullGreen(head7s, 20)
	se.step()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v", m)
	}
	se.step()
	if pr := se.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#8 not marked ready on its stack: %+v (babysit %q)", pr, se.babysitOf(8).Wait)
	}
	se.editSkipped(head8s, 30)
	se.step()
	if m := se.sg.merged(); len(m) != 2 || m[1].HeadSHA != head8s {
		t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(8).Wait)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v: the stacks stood in for that", u)
	}
}

// A repo whose workflow doesn't run on wp-queue/ pushes gets no stacks.
func TestStackedQueueNotOptedIn(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	made := 0
	be.sg.commitFn = func(in CommitInput) (*ForgeCommit, error) { made++; return &ForgeCommit{SHA: "5ac"}, nil }
	be.queuedDraft(7, "add-export", head7s)
	be.queuedDraft(8, "add-import", head8s)
	be.draftChecks(head7s, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8s, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
	if made != 0 {
		t.Errorf("%d stacks made in a repo that doesn't run them", made)
	}
}

func TestWorkflowPushesStacks(t *testing.T) {
	for body, want := range map[string]bool{
		ciStacked: true,
		"on:\n  push:\n    branches:\n      - main\n      - wp-queue/**\n": true,
		"on:\n  push:\n    branches: wp-queue/*\n":                         true,
		"on:\n  push:\n    branches: [main]\n":                             false,
		"on: [push]\n":                                                     false,
		"on:\n  pull_request:\n    branches: ['wp-queue/**']\n":            false,
		ciOnEdit:     false,
		"not: [yaml": false,
	} {
		if got := workflowPushesStacks(body); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

func TestParseStackLog(t *testing.T) {
	heads := []stackHead{{Number: 7, SHA: "7777777aaaa"}, {Number: 8, SHA: "8888888cccc"}}
	good := "x waypoint-stack #7 7777777aaaa\nx waypoint-stack #8 8888888cccc\nx waypoint-stack-tree " + treeA + "\nlater waypoint-stack-tree " + treeB + "\n"
	if tree, why := parseStackLog(good, heads); tree != treeA || why != "" {
		t.Errorf("good log: %q %q", tree, why)
	}
	if tree, why := parseStackLog("waypoint-stack #7 7777777aaaa\nwaypoint-stack-tree "+treeA+"\n", heads); tree != "" || !strings.Contains(why, "merged 1 heads") {
		t.Errorf("short log: %q %q", tree, why)
	}
	if tree, why := parseStackLog("waypoint-stack #8 8888888cccc\nwaypoint-stack #7 7777777aaaa\nwaypoint-stack-tree "+treeA+"\n", heads); tree != "" || !strings.Contains(why, "where the stack has #7") {
		t.Errorf("out of order: %q %q", tree, why)
	}
	if tree, why := parseStackLog("no marker here", heads); tree != "" || why != "" {
		t.Errorf("no marker: %q %q", tree, why)
	}
	if tree, why := parseStackLog("waypoint-stack #7 7777777aaaa\nwaypoint-stack #8 8888888cccc\nwaypoint-stack-tree $(git rev-parse)\n", heads); tree != "" || why == "" {
		t.Errorf("no tree id: %q %q", tree, why)
	}
}

func TestPushCheckName(t *testing.T) {
	if got := pushCheckName("ci / backend (pull_request)"); got != "ci / backend (push)" {
		t.Errorf("got %q", got)
	}
	if got := pushCheckName("lint"); got != "lint" {
		t.Errorf("got %q", got)
	}
}

// saturate sets the reading of the forge's runners (WP-509).
func (se *stackEnv) saturate(on bool) {
	se.s.poolSat.Store(se.pr(7).ForgeID, poolVerdict{saturated: on, at: se.s.now()})
}

// queueAll puts #7 #8 #9 in the queue in that order, as a backlog is: #7
// at the front with its checks still running.
func (se *stackEnv) queueAll() {
	se.t.Helper()
	for _, n := range []int{7, 8, 9} {
		if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET queued_at = ? WHERE id = ?`, int64(1_700_000_000_000+n), se.pr(n).ID); err != nil {
			se.t.Fatal(err)
		}
	}
}

const batch789 = "\n7 " + head7s + "\n8 " + head8s + "\n9 " + head9s + "\n"

// While the runners are saturated, the drafts behind the front wait for a
// batch instead of being brought up to date, and the front rides one with
// the two behind it: one run tests all three, and they merge in order on it
// with no update and no full run of their own. Each merge is marked tested
// from the batch, and a batch that merged whole earns a deeper next one.
func TestBatchedQueueMergesThreeOnOneRun(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	se.sg.behind = map[string]bool{head8s: true, head9s: true}
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 0 {
		t.Fatalf("made before the front could ride: %+v", se.made)
	}
	if w := se.babysitOf(8).Wait; !strings.Contains(w, "waits for a merge queue batch") {
		t.Errorf("#8 waits %q", w)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("drafts brought up to date while batching: %v", u)
	}
	se.draftGreen(head7s, 5)
	if pr := se.pr(7); pr.State != "draft" {
		t.Fatalf("#7 marked ready for its own run instead of riding a batch: %+v", pr)
	}
	if len(se.made) != 1 || !strings.HasPrefix(se.made[0].Branch, "wp-queue/main/9-") || !strings.Contains(se.made[0].Files[0].Content, batch789) {
		t.Fatalf("batch made: %+v", se.made)
	}
	sts := se.stacks()
	if got := stackStates(sts); got != "main + #7 + #8 + #9=running, main + #7 + #8 + #9=running, main + #7 + #8 + #9=running" {
		t.Fatalf("rows: %s", got)
	}
	for i, st := range sts {
		if st.Pos != i || st.CommitSHA != se.shas[0] || st.own().Number != 7+i {
			t.Errorf("row %d: pos %d, commit %s, own #%d", i, st.Pos, st.CommitSHA, st.own().Number)
		}
	}
	se.step()
	if w := se.babysitOf(7).Wait; !strings.Contains(w, "its batch (main + #7 + #8 + #9) is running the full suite") {
		t.Errorf("#7 waits %q", w)
	}
	if w := se.babysitOf(9).Wait; !strings.Contains(w, "rides a merge queue batch (main + #7 + #8 + #9): its run is going") {
		t.Errorf("#9 waits %q", w)
	}

	// The batch passes: #7 is marked ready on it and merges.
	se.sg.trees = map[string]string{"3e9000000": treeB}
	se.stackRun(0, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	if got := stackStates(se.stacks()); strings.Contains(got, "running") {
		t.Fatalf("rows after the run passed: %s", got)
	}
	if pr := se.pr(7); pr.State != "open" {
		t.Fatalf("#7 not marked ready on its batch: %+v (babysit %q)", pr, se.babysitOf(7).Wait)
	}
	if st := se.sg.lastStatus(head7s); st.Context != stackContext || st.State != "success" {
		t.Errorf("#7's head status %+v", st)
	}
	for i, n := range []int{7, 8, 9} {
		head := map[int]string{7: head7s, 8: head8s, 9: head9s}[n]
		if i > 0 {
			se.step()
			if pr := se.pr(n); pr.State != "open" {
				t.Fatalf("#%d not marked ready on its batch: %+v (babysit %q)", n, pr, se.babysitOf(n).Wait)
			}
		}
		se.editSkipped(head, int64(30+10*i))
		se.step()
		if m := se.sg.merged(); len(m) != i+1 || m[i].HeadSHA != head {
			t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(n).Wait)
		}
		st := se.sg.lastStatus(se.sg.mergeSHAs[n])
		if st.Context != testedContext || st.State != "success" {
			t.Fatalf("#%d's merge status %+v", n, st)
		}
		if n < 9 && !strings.Contains(st.Description, fmt.Sprintf("step %d of 3", i+1)) {
			t.Errorf("#%d's merge status %+v", n, st)
		}
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v: the batch stood in for that", u)
	}
	if got := stackStates(se.stacks()); got != "main + #7 + #8 + #9=merged, main + #7 + #8 + #9=merged, main + #7 + #8 + #9=merged" {
		t.Errorf("rows: %s", got)
	}
	if len(se.sg.deleted) != 1 {
		t.Errorf("branches deleted: %v", se.sg.deleted)
	}
	if d := se.s.batchDepthFor(se.pr(7)); d != 3 {
		t.Errorf("batch depth after a whole batch merged: %d", d)
	}
}

// A batch whose run fails fails for every rider: the front goes the usual
// way (marked ready for its own full run), the same heads aren't batched
// again, and the next batch is shallower.
func TestBatchedQueueRunFails(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.s.batchDepth.Store(baseKey(se.pr(7)), 3)
	se.queueAll()
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 || !strings.Contains(se.made[0].Files[0].Content, batch789) {
		t.Fatalf("batch made: %+v", se.made)
	}
	se.stackRun(0, CheckFailed, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	se.step()
	sts := se.stacks()
	for _, st := range sts[:3] {
		if st.State != stackFailed {
			t.Fatalf("rows: %s", stackStates(sts))
		}
	}
	if pr := se.pr(7); pr.State != "open" {
		t.Fatalf("#7 not marked ready for its own run: %+v (babysit %q)", pr, se.babysitOf(7).Wait)
	}
	for _, in := range se.made[1:] {
		if strings.Contains(in.Files[0].Content, batch789) {
			t.Errorf("the failed heads batched again: %+v", in)
		}
	}
	if !slices.Contains(se.sg.cancelled, se.shas[0]) {
		t.Errorf("runs cancelled on %v", se.sg.cancelled)
	}
	if !slices.Contains(se.sg.deleted, se.made[0].Branch) {
		t.Errorf("branches deleted: %v", se.sg.deleted)
	}
	if d := se.s.batchDepthFor(se.pr(7)); d != batchMinDepth {
		t.Errorf("batch depth after a failed batch: %d", d)
	}
}

// A batch's front merged by hand while the batch's run goes ends its own
// row as merged; the run and the branch are the other riders' still.
func TestBatchedQueueRiderMergedOutside(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 || !strings.Contains(se.made[0].Files[0].Content, batch789) {
		t.Fatalf("batch made: %+v", se.made)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged' WHERE number = 7`); err != nil {
		t.Fatal(err)
	}
	se.s.sweepStacks(se.ctx)
	if got := stackStates(se.stacks()); got != "main + #7 + #8 + #9=merged, main + #7 + #8 + #9=running, main + #7 + #8 + #9=running" {
		t.Fatalf("rows: %s", got)
	}
	if len(se.sg.cancelled) != 0 || len(se.sg.deleted) != 0 {
		t.Errorf("the riders' run cancelled on %v, branches deleted %v", se.sg.cancelled, se.sg.deleted)
	}
}

// A push to a pull request riding a batch ends the batch for all of them.
func TestBatchedQueueRiderMoves(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 {
		t.Fatalf("batch made: %+v", se.made)
	}
	p9 := bsPR
	p9.Number, p9.HeadRef, p9.BaseRef, p9.Title = 9, "add-sync", "main", "Change add-sync"
	se.pushTo(p9, "9999999eeee")
	se.step()
	sts := se.stacks()
	for _, st := range sts[:3] {
		if st.State != stackCancelled {
			t.Fatalf("rows: %s", stackStates(sts))
		}
	}
	if !slices.Contains(se.sg.deleted, se.made[0].Branch) {
		t.Errorf("branches deleted: %v", se.sg.deleted)
	}
}

// A batch takes riders only while their reviews are over (WP-386): one
// whose review is still going ends the batch at the one before it, and a
// front whose review is going gets no batch at all.
func TestBatchedQueueRidersSettled(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	for _, n := range []int{7, 8, 9} {
		se.reviewRunning(n)
	}
	se.reviewOver(7)
	se.reviewOver(8)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	se.step()
	if len(se.made) != 1 || !strings.HasPrefix(se.made[0].Branch, "wp-queue/main/8-") ||
		!strings.Contains(se.made[0].Files[0].Content, "\n7 "+head7s+"\n8 "+head8s+"\n") || strings.Contains(se.made[0].Files[0].Content, head9s) {
		t.Fatalf("batch made: %+v", se.made)
	}

	se = newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	for _, n := range []int{7, 8, 9} {
		se.reviewRunning(n)
	}
	se.reviewOver(8)
	se.reviewOver(9)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	se.step()
	if len(se.made) != 0 {
		t.Fatalf("batch made on a front whose review is going: %+v", se.made)
	}
	if pr := se.pr(7); pr.State != "open" {
		t.Errorf("#7 not marked ready for its own run: %+v", pr)
	}
}

// Ten stack or batch runs on a line in a row with no pass pause its stacks
// for stackPauseFor after the newest ended; then one is tried again.
func TestStacksPauseAfterNoPasses(t *testing.T) {
	se := newStackEnv(t)
	pr := se.pr(9)
	ended := se.s.now().Add(-30 * time.Minute)
	for i := range stackPauseRuns {
		state := stackFailed
		if i%2 == 0 {
			state = stackCancelled
		}
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, state, created_at, ended_at)
			VALUES (?, ?, 'main', ?, 'tip-0', ?, ?, ?, ?)`, pr.ForgeID, pr.Repo, pr.ID, fmt.Sprintf("wp-queue/main/9-old%d", i), state,
			ended.Add(-time.Hour).UnixMilli(), ended.Add(-time.Duration(i)*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if until, paused := se.s.stacksPaused(se.ctx, pr); !paused || !until.Equal(time.UnixMilli(ended.UnixMilli()).Add(stackPauseFor)) {
		t.Fatalf("paused %v until %v", paused, until)
	}
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 0 {
		t.Fatalf("stacks made while paused: %+v", se.made)
	}

	// The pause is over: stacks are tried again.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE queue_stacks SET ended_at = ended_at - ?`, (2 * stackPauseFor).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made after the pause: %+v", se.made)
	}

	// A pass among the last ten lifts a pause.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE queue_stacks SET ended_at = ? WHERE branch = 'wp-queue/main/9-old3'`, se.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE queue_stacks SET state = ? WHERE branch = 'wp-queue/main/9-old5'`, stackPassed); err != nil {
		t.Fatal(err)
	}
	if _, paused := se.s.stacksPaused(se.ctx, pr); paused {
		t.Error("paused with a pass among the last ten")
	}
}

// The runners are saturated when a job has waited poolSatWait for one.
func TestNotePoolSaturation(t *testing.T) {
	s := &Service{now: time.Now}
	now := time.Now()
	snap := func(waited time.Duration, status string) *ciSnapshot {
		return &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: &Forge{ID: 4}}, active: []ActionRun{{ID: 1, Index: 1, Created: now.Add(-waited)}},
			jobs: map[int64][]ActionJob{1: {{ID: 1, Status: status}}}}}}
	}
	for _, c := range []struct {
		waited time.Duration
		status string
		want   bool
	}{{3 * time.Minute, "waiting", true}, {time.Minute, "waiting", false}, {time.Hour, "running", false}, {time.Hour, "blocked", false}} {
		s.notePoolSaturation(snap(c.waited, c.status))
		v, _ := s.poolSat.Load(int64(4))
		if v.(poolVerdict).saturated != c.want {
			t.Errorf("%s for %s: saturated %v", c.status, c.waited, !c.want)
		}
	}
}

// A stack's job logs can't be read yet (a forge error, a log not there
// right after the job ends): the stack waits for them, and stacks stay on.
func TestStackedQueueLogNotReadYet(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.sg.sm.Lock()
	delete(se.sg.logs, se.shas[0]+" ci / lint (push)")
	se.sg.sm.Unlock()
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=running" {
		t.Fatalf("stacks: %s", got)
	}
	if _, off := se.s.stackOff.Load(baseKey(se.pr(8))); off {
		t.Fatal("stacks turned off over a log not read")
	}
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=passed" {
		t.Fatalf("stacks: %s", got)
	}
}

// A stack whose run is still going after stackRunMax fails, so the front
// isn't held on it; stacks stay on for the base.
func TestStackedQueueRunOverdue(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.stackRun(0, CheckPending, treeA, "7 "+head7s, "8 "+head8s)
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8=running" {
		t.Fatalf("stacks: %s", got)
	}
	// Only the stack's own poll: two hours on, the rest of the queue has
	// moved on in ways this test isn't about.
	se.setClock(se.clockNow().Add(stackRunMax - time.Minute))
	st := se.stacks()[0]
	f, err := se.s.forgeByID(se.ctx, st.ForgeID)
	if err != nil {
		t.Fatal(err)
	}
	se.s.pollStack(se.ctx, f, st)
	if st.State != stackRunning {
		t.Fatalf("ended before stackRunMax: %s (%s)", st.State, st.Note)
	}
	se.setClock(se.clockNow().Add(time.Minute))
	se.s.pollStack(se.ctx, f, st)
	if got := stackStates(se.stacks()); got != "main + #7 + #8=failed" {
		t.Fatalf("stacks: %s (%s)", got, se.stacks()[0].Note)
	}
	if _, off := se.s.stackOff.Load(baseKey(se.pr(8))); off {
		t.Fatal("stacks turned off over a slow run")
	}
}

// A stacked pull request merged at its stacked head outside the queue ends
// its stack as merged: the stacks built on it still hold.
func TestStackedQueueMergedOutside(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.stackRun(1, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged' WHERE number = 8`); err != nil {
		t.Fatal(err)
	}
	se.s.sweepStacks(se.ctx)
	if got := stackStates(se.stacks()); got != "main + #7 + #8=merged, main + #7 + #8 + #9=passed" {
		t.Fatalf("stacks: %s", got)
	}
}

func TestTouchesCI(t *testing.T) {
	for p, want := range map[string]bool{
		".forgejo/workflows/ci.yml": true, ".forgejo/scripts/queue-stack.sh": true,
		".gitea/workflows/ci.yml": true, ".github/workflows/ci.yml": true, ".github/actions/x/action.yml": true,
		"internal/core/merge.go": false, "docs/.github-notes.md": false, ".githubx/a": false,
	} {
		if got := touchesCI(p); got != want {
			t.Errorf("touchesCI(%q) = %v", p, got)
		}
	}
}

// stackConflict gives stack i's commit a failed push run whose lint log
// says head n doesn't merge onto the ones before it; heads are what the
// step merged before it stopped.
func (se *stackEnv) stackConflict(i int, n string, heads ...string) {
	se.t.Helper()
	se.stackConflictOn(i, n, "", heads...)
}

// stackConflictOn is stackConflict whose log also says head n doesn't merge
// onto base alone either, when base isn't "" (WP-457).
func (se *stackEnv) stackConflictOn(i int, n, base string, heads ...string) {
	se.t.Helper()
	sha := se.shas[i]
	var checks []CommitCheck
	for j, name := range []string{"ci / backend (push)", "ci / lint (push)", "ci / e2e (push)"} {
		st := CheckFailed
		if j != 1 {
			st = CheckSuccess // the jobs fail-fast cancelled: no status
		}
		checks = append(checks, CommitCheck{Name: name, State: st, ID: int64(500 + 10*i + j), Description: "Failing after 1m", URL: "https://forge/me/app/actions/runs/" + sha + "/jobs/" + fmt.Sprint(j)})
	}
	se.sg.setChecks(sha, checks...)
	var log strings.Builder
	for _, h := range heads {
		num, s, _ := strings.Cut(h, " ")
		fmt.Fprintf(&log, "2026-11-03T10:00:00Z waypoint-stack #%s %s\n", num, s)
	}
	num, s, _ := strings.Cut(n, " ")
	fmt.Fprintf(&log, "2026-11-03T10:00:01Z ::error title=Merge queue stack::#%s (%s) doesn't merge cleanly onto the heads before it\n", num, s)
	if base != "" {
		fmt.Fprintf(&log, "2026-11-03T10:00:02Z ::error title=Merge queue stack::#%s (%s) doesn't merge cleanly onto the base (%s) itself\n", num, s, base)
	}
	se.sg.sm.Lock()
	se.sg.logs[sha+" ci / lint (push)"] = log.String()
	se.sg.sm.Unlock()
}

// A stack's run finds the second place's head doesn't merge onto the
// front's: its row ends as a conflict, and the third place is stacked on
// the front alone, past it, taking its place. The third merges on that
// stack with no update; the conflicting one comes to the front after.
func TestStackedQueuePassesConflict(t *testing.T) {
	se := newStackEnv(t)
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made: %+v", se.made)
	}
	se.stackConflict(0, "8 "+head8s, "7 "+head7s)
	se.step()
	se.step()
	sts := se.stacks()
	if got := stackStates(sts); got != "main + #7 + #8=conflict, main + #7 + #8 + #9=cancelled, main + #7 + #9=running" {
		t.Fatalf("stacks: %s (#9 %q)", got, se.babysitOf(9).Wait)
	}
	if !strings.Contains(sts[0].Note, "#8 (8888888) doesn't merge cleanly onto the base + #7") {
		t.Errorf("conflict note %q", sts[0].Note)
	}
	if ahead, err := se.s.queueAhead(se.ctx, se.pr(9)); err != nil || len(ahead) != 1 || ahead[0].Number != 7 {
		t.Fatalf("ahead of #9: %v %v", ahead, err)
	}
	if n := se.pr(8).armedNote; !strings.Contains(n, "let #9 go first") {
		t.Errorf("#8's note %q", n)
	}
	// #8 is not passed over again on the same conflict, or stacked on it.
	se.step()
	if len(se.made) != 3 {
		t.Fatalf("stacks made after the conflict: %d (%+v)", len(se.made), se.made[3:])
	}

	se.stackRun(2, CheckSuccess, treeA, "7 "+head7s, "9 "+head9s)
	se.step()
	se.sg.trees = map[string]string{"3e9000000": treeA}
	se.fullGreen(head7s, 20)
	se.step()
	if pr := se.pr(9); pr.State != "open" {
		t.Fatalf("#9 not marked ready on its stack: %+v (babysit %q)", pr, se.babysitOf(9).Wait)
	}
	se.editSkipped(head9s, 40)
	se.step()
	if m := se.sg.merged(); len(m) != 2 || m[1].HeadSHA != head9s {
		t.Fatalf("merges %+v (babysit %q)", m, se.babysitOf(9).Wait)
	}
	if u := se.sg.updatedPRs(); slices.Contains(u, 9) {
		t.Errorf("#9 updated %v: its stack stood in for that", u)
	}
	if ahead, err := se.s.queueAhead(se.ctx, se.pr(8)); err != nil || len(ahead) != 0 {
		t.Errorf("#8 not at the front after the heads it conflicts with merged: %v %v", ahead, err)
	}
}

// A stack's run that finds the second place's head doesn't merge onto the
// base's tip alone either hands its conflict to a round at once, while the
// front's full run goes (WP-457): it keeps its place, and the round's packet
// has the conflict though Forgejo says nothing for a draft (WP-171).
func TestStackedQueueBaseConflictRoundsEarly(t *testing.T) {
	se := newStackEnv(t)
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if len(se.made) != 2 {
		t.Fatalf("stacks made: %+v", se.made)
	}
	base := se.stacks()[0].BaseSHA
	se.stackConflictOn(0, "8 "+head8s, base, "7 "+head7s)
	se.runnerOnline() // it kept checking in while the clock went on
	se.step()
	sts := se.stacks()
	if !strings.Contains(sts[0].Note, "#8 (8888888) doesn't merge cleanly onto the base") || strings.Contains(sts[0].Note, "+ #7") {
		t.Errorf("conflict note %q", sts[0].Note)
	}
	b := se.babysitOf(8)
	if b.Round != 1 || !b.Rounds[0].Conflict || b.JobID == 0 {
		t.Fatalf("#8 got no early conflict round: %+v (waits %q)", b, b.Wait)
	}
	if in := se.jobInput(b.JobID); !strings.Contains(in.Prompt, "git merge FETCH_HEAD") {
		t.Errorf("the round's packet has no conflict:\n%s", in.Prompt)
	}
	if pr := se.pr(8); pr.QueuedAt == nil || pr.State != "draft" {
		t.Errorf("#8 lost its place or went ready: %+v", pr)
	}
	if pr := se.pr(7); pr.ReadyAt == nil {
		t.Errorf("#7 lost the front: %+v", pr)
	}
	if u := se.sg.updatedPRs(); slices.Contains(u, 8) {
		t.Errorf("#8 updated %v behind the front", u)
	}
}

// An early conflict round doesn't start while its owner's usage limit is
// spent: the round would have to wait, and waiting costs the place in the
// queue that a conflict alone keeps (WP-457).
func TestStackedQueueBaseConflictWaitsOutALimit(t *testing.T) {
	se := newStackEnv(t)
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE babysits SET limited_until = ? WHERE id = ?`,
		se.s.nowMS()+time.Hour.Milliseconds(), se.babysitOf(8).ID); err != nil {
		t.Fatal(err)
	}
	se.stackConflictOn(0, "8 "+head8s, se.stacks()[0].BaseSHA, "7 "+head7s)
	se.runnerOnline()
	se.step()
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		t.Fatalf("#8 got a round with its owner's limit spent: %+v", b)
	}
	if pr := se.pr(8); pr.QueuedAt == nil {
		t.Errorf("#8 lost its place: %+v", pr)
	}
}

// A head with a live stack gets no early conflict round: the stack's run
// says whether it merges, and a push would throw that run away (WP-457).
func TestStackedQueueLiveStackGetsNoEarlyRound(t *testing.T) {
	se := newStackEnv(t)
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	pr := se.pr(8)
	se.s.baseUpdated.Store(fmt.Sprintf("%d@%s", pr.ID, se.stacks()[0].BaseSHA), baseUpdate{outcome: upConflict, at: se.s.now(), from: pr.HeadSHA, n: 1})
	se.runnerOnline()
	se.step()
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		t.Fatalf("#8 got a round on its live stack: %+v", b)
	}
	if st, err := se.s.liveStackOf(se.ctx, pr.ID); err != nil || st == nil {
		t.Errorf("#8's stack ended: %+v, %v", st, err)
	}
}

// stackBaseConflictNoRunner: #8's stack run failed with a conflict with the
// base itself while no runner could take a round, so it has none yet; it is
// #8's head at the base's tip that the line now knows about (WP-457).
func (se *stackEnv) stackBaseConflictNoRunner() {
	se.t.Helper()
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.runnersOffline()
	se.stackConflictOn(0, "8 "+head8s, se.stacks()[0].BaseSHA, "7 "+head7s)
	se.step()
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		se.t.Fatalf("#8 got a round with no runner: %+v", b)
	}
}

func (se *stackEnv) runnersOffline() {
	se.t.Helper()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE runners SET last_seen_at = 0`); err != nil {
		se.t.Fatal(err)
	}
}

// The ones behind a base conflict don't stack past it until its early round
// has gone: their stacks would move it out of the places that get one. With
// no round coming they go past it, as ever (WP-457, WP-535).
func TestEarlyConflictRoundGoesBeforeItIsPassed(t *testing.T) {
	se := newStackEnv(t)
	se.stackBaseConflictNoRunner()
	ahead := []*PullRequest{se.pr(7), se.pr(8)}
	if _, passed := se.s.passConflicts(se.ctx, ahead); len(passed) != 1 {
		t.Fatalf("no runner, so no round: #8 should be passed over, passed %+v", passed)
	}
	se.runnerOnline()
	if on, passed := se.s.passConflicts(se.ctx, ahead); len(passed) != 0 || len(on) != 2 {
		t.Fatalf("#8 passed over before its early round: on %d, passed %d", len(on), len(passed))
	}
	se.step()
	if b := se.babysitOf(8); b.Round != 1 || !b.Rounds[0].Conflict || b.JobID == 0 {
		t.Fatalf("#8 got no early round: %+v", b)
	}
	if _, passed := se.s.passConflicts(se.ctx, ahead); len(passed) != 1 {
		t.Fatalf("its round has gone: #8 should be passed over, passed %+v", passed)
	}
}

// An early conflict round that no runner took never happened: no round is
// counted, so its turn at the front still gets one; no person is called; it
// keeps its place in the queue (WP-457).
func TestEarlyConflictRoundNoRunnerTookIsNothing(t *testing.T) {
	se := newStackEnv(t)
	se.stackBaseConflictNoRunner()
	pr, b := se.pr(8), se.babysitOf(8)
	// Waiting through the first case; past the runner's grace in the second,
	// where a round is otherwise handed to a person.
	for _, since := range []time.Time{se.clockNow(), se.clockNow().Add(-babysitRunnerGrace)} {
		b.State, b.Wait, b.WaitSince = BabysitWatching, se.s.runnerWaitFor(b), since
		if !se.s.babysitRoundBy(se.ctx, b, pr, nil, true, nil, nil, nil, true) {
			t.Fatalf("a round no runner took says it started (waiting since %v)", since)
		}
		got := se.babysitOf(8)
		if got.Round != 0 || len(got.Rounds) != 0 || got.State == BabysitNeedsPerson {
			t.Fatalf("a round no runner took left a mark: %+v", got)
		}
		if p := se.pr(8); p.QueuedAt == nil {
			t.Fatalf("a round no runner took cost its place: %+v", p)
		}
	}
}

// A conflict found at a tip the base has since left is no reason for an
// early round: the next stack or update says about the tip it is at now.
func TestEarlyConflictIgnoresAnOldTip(t *testing.T) {
	se := newStackEnv(t)
	se.stackBaseConflictNoRunner()
	se.sg.sm.Lock()
	se.sg.baseTip = "tip-2"
	se.sg.sm.Unlock()
	se.runnerOnline()
	se.step()
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		t.Fatalf("#8 got a round for a tip the base left: %+v", b)
	}
}

// A base conflict only a stack's run found still shows in the wait of a
// draft that gets no early round: Forgejo has no flag to say so (WP-457).
func TestBaseConflictFromAStackShowsInTheWait(t *testing.T) {
	se := newStackEnv(t)
	se.stackBaseConflictNoRunner()
	se.step()
	if b := se.babysitOf(8); !strings.Contains(b.Wait, "has conflicts with main") {
		t.Fatalf("#8's wait doesn't say it conflicts: %q", b.Wait)
	}
}

// A refused update is about the head it was refused for: another head at
// the same tip hasn't been tried, so the forge is asked (WP-457).
func TestUpdateConflictIsForTheHeadItWasFoundOn(t *testing.T) {
	se := newStackEnv(t)
	se.sg.sm.Lock()
	se.sg.behind = map[string]bool{head7s: true, head8s: true}
	se.sg.sm.Unlock()
	refused := func(n int, from string) {
		se.s.baseUpdated.Store(fmt.Sprintf("%d@tip-1", se.pr(n).ID), baseUpdate{outcome: upConflict, at: se.s.now(), from: from, n: 1})
	}
	refused(7, "aaaaaaa1111")
	if got := se.s.updateIfBehind(se.ctx, se.pr(7)); got != upUpdated || !slices.Contains(se.sg.updatedPRs(), 7) {
		t.Errorf("a new head after a refused update: outcome %d, updates %v", got, se.sg.updatedPRs())
	}
	refused(8, head8s)
	if got := se.s.updateIfBehind(se.ctx, se.pr(8)); got != upConflict || slices.Contains(se.sg.updatedPRs(), 8) {
		t.Errorf("the refused head itself: outcome %d, updates %v", got, se.sg.updatedPRs())
	}
}

// A stack's run replaces what is known at a tip about an older head, so a
// newer conflicting head counts; what the line knows about this head stands.
func TestStackBaseConflictIsForTheLatestHead(t *testing.T) {
	se := newStackEnv(t)
	pr := se.pr(8)
	key := fmt.Sprintf("%d@tip-1", pr.ID)
	st := &queueStack{BaseSHA: "tip-1"}
	h := stackHead{PRID: pr.ID, Number: pr.Number, SHA: pr.HeadSHA}
	se.s.baseUpdated.Store(key, baseUpdate{outcome: upConflict, at: se.s.now(), from: "aaaaaaa1111", n: 1})
	se.s.stackBaseConflict(st, h)
	se.s.Wait()
	if !se.s.updateConflicted(pr) {
		t.Fatal("a newer conflicting head left out")
	}
	se.s.baseUpdated.Store(key, baseUpdate{outcome: upUpdated, at: se.s.now(), from: pr.HeadSHA, n: 2})
	se.s.stackBaseConflict(st, h)
	se.s.Wait()
	if v, _ := se.s.baseUpdated.Load(key); v.(baseUpdate).outcome != upUpdated {
		t.Errorf("the record for this head replaced: %+v", v)
	}
}

// Without the base-itself line, a stack conflict may be with the heads ahead
// only: it waits for its turn, as before.
func TestStackedQueueHeadConflictWaits(t *testing.T) {
	se := newStackEnv(t)
	se.queueAll()
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.stackConflict(0, "8 "+head8s, "7 "+head7s)
	se.runnerOnline()
	se.step()
	se.step()
	if b := se.babysitOf(8); b.Round != 0 {
		t.Fatalf("#8 got a round on a conflict with #7: %+v", b)
	}
}

// Under saturation, a batch goes past a draft known not to merge onto the
// front, which yields its place to the batch's last rider; the same
// conflict found again doesn't fail the riders.
func TestBatchedQueuePassesConflict(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.queueAll()
	hj := fmt.Sprintf(`[{"pr_id":%d,"number":7,"sha":%q},{"pr_id":%d,"number":8,"sha":%q}]`, se.pr(7).ID, head7s, se.pr(8).ID, head8s)
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, note, created_at, ended_at)
		VALUES (?, 'me/app', 'main', ?, 'tip-1', ?, 1, 'wp-queue/main/8-x', '5acold', ?, 'test', 1, 2)`, se.pr(8).ForgeID, se.pr(8).ID, hj, stackConflict); err != nil {
		t.Fatal(err)
	}
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 || !strings.Contains(se.made[0].Files[0].Content, "\n7 "+head7s+"\n9 "+head9s+"\n") || strings.Contains(se.made[0].Files[0].Content, head8s) {
		t.Fatalf("batch made: %+v (#9 %q)", se.made, se.babysitOf(9).Wait)
	}
	if ahead, err := se.s.queueAhead(se.ctx, se.pr(8)); err != nil || len(ahead) != 2 || ahead[1].Number != 9 {
		t.Errorf("ahead of #8: %v %v", ahead, err)
	}
	if got := stackStates(se.stacks()[1:]); got != "main + #7 + #9=running, main + #7 + #9=running" {
		t.Errorf("rows: %s", got)
	}
}

// A rider's conflict in a batch ends its row as a conflict and cancels the
// others' rows: one rider's conflict says nothing of theirs, or of how
// deep a batch's run can go.
func TestBatchedQueueRiderConflicts(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.s.batchDepth.Store(baseKey(se.pr(7)), 3)
	se.queueAll()
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 || !strings.Contains(se.made[0].Files[0].Content, batch789) {
		t.Fatalf("batch made: %+v", se.made)
	}
	se.stackConflict(0, "8 "+head8s, "7 "+head7s)
	se.step()
	if got := stackStates(se.stacks()); got != "main + #7 + #8 + #9=cancelled, main + #7 + #8 + #9=conflict, main + #7 + #8 + #9=cancelled" {
		t.Fatalf("rows: %s", got)
	}
	if d := se.s.batchDepthFor(se.pr(7)); d != 3 {
		t.Errorf("batch depth after a conflict: %d (a conflict says nothing of the runs)", d)
	}
}

// A draft past its limit of conflicts at one head isn't passed over any
// more, and the batch its conflict ended isn't run again on the same heads.
func TestBatchedQueueConflictNotRetried(t *testing.T) {
	se := newStackEnv(t)
	se.saturate(true)
	se.s.batchDepth.Store(baseKey(se.pr(7)), 3)
	se.queueAll()
	for i := range conflictYields {
		hj := fmt.Sprintf(`[{"pr_id":%d,"number":7,"sha":"70ld%d"},{"pr_id":%d,"number":8,"sha":%q}]`, se.pr(7).ID, i, se.pr(8).ID, head8s)
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, note, created_at, ended_at)
			VALUES (?, 'me/app', 'main', ?, 'tip-1', ?, 1, 'wp-queue/main/8-x', '5acold', ?, 'test', 1, 2)`, se.pr(8).ForgeID, se.pr(8).ID, hj, stackConflict); err != nil {
			t.Fatal(err)
		}
	}
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.draftGreen(head7s, 5)
	if len(se.made) != 1 || !strings.Contains(se.made[0].Files[0].Content, batch789) {
		t.Fatalf("batch made: %+v", se.made)
	}
	se.stackConflict(0, "8 "+head8s, "7 "+head7s)
	se.step()
	se.step()
	se.step()
	for _, in := range se.made[1:] {
		if strings.Contains(in.Files[0].Content, batch789) {
			t.Fatalf("the same batch run again: %d made", len(se.made))
		}
	}
}

func TestParseConflictLog(t *testing.T) {
	heads := []stackHead{{Number: 7, SHA: head7s}, {Number: 8, SHA: head8s + "00"}}
	log := "waypoint-stack #7 " + head7s + "\n::error title=Merge queue stack::#8 (" + head8s + "00) doesn't merge cleanly onto the heads before it\n"
	if k := parseConflictLog(log, heads); k != 1 {
		t.Errorf("k = %d", k)
	}
	if k := parseConflictLog("::error title=Merge queue stack::#9 (abcdef1) doesn't merge cleanly onto the heads before it", heads); k != -1 {
		t.Errorf("a head not in the stack: k = %d", k)
	}
	if k := parseConflictLog("FAIL pkg", heads); k != -1 {
		t.Errorf("no conflict: k = %d", k)
	}
	base := "4e7ced0ffee0000000000000000000000000000"
	onBase := log + "::error title=Merge queue stack::#8 (" + head8s + "00) doesn't merge cleanly onto the base (" + base + ") itself\n"
	if !onBaseLog(onBase, heads[1], base) || onBaseLog(log, heads[1], base) || onBaseLog(onBase, heads[1], "1111111") || onBaseLog(onBase, heads[0], base) {
		t.Error("onBaseLog")
	}
}

// A draft that joins the stacks after the front merged on its stack extends
// the stack behind it on the base's new tip, without the merged head: the
// branch is cut from that tip, so a stack recorded on the old one would be
// deleted as soon as it was made (its run cancelled), and made again on the
// next step, for as long as the stack ahead stands.
func TestStackOnMovedBaseRestacks(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	if len(se.made) != 1 {
		t.Fatalf("stacks made: %+v", se.made)
	}
	// #7 merges on its own full run; #8's stack run is still going.
	se.fullGreen(head7s, 20)
	se.step()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v", m)
	}
	// #9's draft tier passes now, with #8 at the front on its stack.
	se.draftGreen(head9s, 9)
	se.step()
	se.step()
	sts := se.stacks()
	if len(sts) != len(se.made) {
		t.Fatalf("stack branches made: %d, recorded: %d (deleted %v)", len(se.made), len(sts), se.sg.deleted)
	}
	last := sts[len(sts)-1]
	if last.own().Number != 9 || last.BaseSHA != "3e7000000" || len(last.Heads) != 2 || last.Heads[0].Number != 8 || last.Heads[1].Number != 9 {
		t.Fatalf("#9's stack: %+v", last)
	}
	if !strings.Contains(se.made[len(se.made)-1].Files[0].Content, "8 "+head8s+"\n9 "+head9s+"\n") {
		t.Errorf("stack file: %q", se.made[len(se.made)-1].Files[0].Content)
	}
}

// A stack ahead whose base the tip doesn't follow (the base moved some other
// way) can't be extended: no branch is made for it.
func TestStackOnForeignTipMakesNoBranch(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.step()
	se.sg.sm.Lock()
	se.sg.baseTip = "0ther000"
	se.sg.sm.Unlock()
	se.draftGreen(head9s, 9)
	se.step()
	se.step()
	if len(se.made) != 1 || len(se.sg.deleted) != 0 {
		t.Fatalf("stack branches made: %d, deleted %v", len(se.made), se.sg.deleted)
	}
}
