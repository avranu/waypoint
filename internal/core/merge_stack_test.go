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

var treeA, treeB = strings.Repeat("a", 40), strings.Repeat("b", 40)

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

// candEndingAt is the index of the candidate whose last head is pull request
// n (stackConflict puts a log on a candidate by index: it has to be the one
// that names that head, or no conflict is read from it).
func (se *stackEnv) candEndingAt(n int) int {
	se.t.Helper()
	for i, st := range se.stacks() {
		if !st.Ended && len(st.Heads) > 0 && st.Heads[len(st.Heads)-1].Number == n && len(st.Heads) > 1 {
			return i
		}
	}
	se.t.Fatalf("no live candidate ends at #%d", n)
	return -1
}

// conflictEnded checks that candidate i ended as a conflict about #n, and
// whether it conflicts with the heads ahead only.
func (se *stackEnv) conflictEnded(i, n int, ahead bool) {
	se.t.Helper()
	st := se.stacks()[i]
	if !st.Ended || st.EndReason != endConflict || st.ConflictPR != se.pr(n).ID || st.ConflictAhead != ahead {
		se.t.Fatalf("candidate %d: ended %v %q conflict_pr %d ahead %v; want a conflict about #%d (ahead %v)", i, st.Ended, st.EndReason, st.ConflictPR, st.ConflictAhead, n, ahead)
	}
}

// A conflict round doesn't start while its owner's usage limit is spent: it
// waits for the limit, with the pull request still armed (WP-457).
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
	i := se.candEndingAt(8)
	se.stackConflictOn(i, "8 "+head8s, se.stacks()[i].BaseSHA, "7 "+head7s)
	se.runnerOnline()
	se.step()
	se.conflictEnded(i, 8, false)
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		t.Fatalf("#8 got a round with its owner's limit spent: %+v", b)
	}
	// The line drops a head that conflicts with the base from its places (a
	// round resolves it); with the limit spent the round waits, and the pull
	// request stays armed.
	if pr := se.pr(8); pr.ArmedAt == nil {
		t.Errorf("#8 was disarmed: %+v", pr)
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
	i := se.candEndingAt(8)
	se.stackConflictOn(i, "8 "+head8s, se.stacks()[i].BaseSHA, "7 "+head7s)
	se.step()
	se.conflictEnded(i, 8, false)
	if !se.s.conflictOnTip(se.ctx, se.pr(8)) {
		se.t.Fatal("the line didn't record #8's conflict with the base's tip")
	}
	if b := se.babysitOf(8); b.Round != 0 || b.JobID != 0 {
		se.t.Fatalf("#8 got a round with no runner: %+v", b)
	}
}

// With a runner, a conflict with the base's tip gets #8 its round at once,
// whatever its place in the line (stackBaseConflict).
func TestStackBaseConflictStartsARound(t *testing.T) {
	se := newStackEnv(t)
	se.stackBaseConflictNoRunner()
	se.runnerOnline()
	se.step()
	if b := se.babysitOf(8); b.JobID == 0 {
		t.Fatalf("#8 got no round for its conflict with the base: %+v", b)
	}
}

func (se *stackEnv) runnersOffline() {
	se.t.Helper()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE runners SET last_seen_at = 0`); err != nil {
		se.t.Fatal(err)
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
	i := se.candEndingAt(8)
	se.stackConflict(i, "8 "+head8s, "7 "+head7s)
	se.runnerOnline()
	se.step()
	se.step()
	se.conflictEnded(i, 8, true)
	if se.s.conflictOnTip(se.ctx, se.pr(8)) {
		t.Error("a conflict with #7 alone was taken for one with the base")
	}
	if b := se.babysitOf(8); b.Round != 0 {
		t.Fatalf("#8 got a round on a conflict with #7: %+v", b)
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

// F30: a refused update (or candidate) on the base's current tip, from the
// pull request's head, is a conflict round for it; one on a tip the base has
// left, or for another head, is none.
func TestConflictOnTipStartsARoundThroughTheBabysit(t *testing.T) {
	for _, tc := range []struct {
		name, tip, from string
		round           bool
	}{
		{"current tip and head", "tip-1", head8s, true},
		{"an old tip", "tip-0", head8s, false},
		{"another head", "tip-1", "aaaaaaa1111", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newStackEnv(t)
			se.queueAll()
			se.draftGreen(head7s, 5)
			se.draftGreen(head8s, 7)
			se.step()
			se.runnerOnline()
			se.s.baseUpdated.Store(fmt.Sprintf("%d@%s", se.pr(8).ID, tc.tip), baseUpdate{outcome: upConflict, at: se.s.now(), from: tc.from, n: 1})
			se.step()
			se.step()
			if b := se.babysitOf(8); (b.JobID != 0) != tc.round {
				t.Errorf("#8 round started %v, want %v: %+v", b.JobID != 0, tc.round, b)
			}
		})
	}
}
