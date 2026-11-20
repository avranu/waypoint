package core

import (
	"strings"
	"testing"
	"time"
)

// armedBabysit: pull request 7, armed by the admin, babysat.
func armedBabysit(t *testing.T) (*bsEnv, *PullRequest) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, be.s.nowMS(), be.admin.User.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	return be, pr
}

func (be *bsEnv) setBabysitState(state, reason string, ended bool) {
	be.t.Helper()
	var at any
	if ended {
		at = be.s.nowMS()
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = ?, end_reason = ?, wait = ?, ended_at = ?, job_id = NULL, wait_at = ? WHERE id = (SELECT max(id) FROM babysits)`,
		state, reason, reason, at, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
}

func (be *bsEnv) babysitCount() int {
	be.t.Helper()
	be.s.Wait()
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM babysits`).Scan(&n); err != nil {
		be.t.Fatal(err)
	}
	return n
}

func TestResumeBabysitsAfterRestart(t *testing.T) {
	be, _ := armedBabysit(t)
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)

	// Waypoint starts: the babysit that gave up starts again, from round 0.
	be.s.ResumeBabysits(be.ctx)
	b := be.babysit()
	if !b.Active || b.Round != 0 || b.Via != viaResume || be.babysitCount() != 2 {
		t.Fatalf("after a start: %+v", b)
	}
	// It gives up again; another start soon after leaves it be.
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)
	be.s.ResumeBabysits(be.ctx)
	if n := be.babysitCount(); n != 2 {
		t.Fatalf("resumed again within %v: %d babysits", resumeEvery, n)
	}
	// Six hours on it may; a day after it gave up, no more.
	be.setClock(be.clockNow().Add(resumeEvery + time.Minute))
	be.s.ResumeBabysits(be.ctx)
	if n := be.babysitCount(); n != 3 {
		t.Fatalf("after %v: %d babysits", resumeEvery, n)
	}
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)
	be.setClock(be.clockNow().Add(resumeGaveUpWithin + time.Minute))
	be.s.ResumeBabysits(be.ctx)
	if n := be.babysitCount(); n != 3 {
		t.Fatalf("a give-up older than %v: %d babysits", resumeGaveUpWithin, n)
	}
}

func TestResumeBabysitsLeavesAPersonsCall(t *testing.T) {
	be, pr := armedBabysit(t)
	// Waiting on a person for Waypoint's own reason: started again.
	be.setBabysitState(BabysitNeedsPerson, "round 4 (done, nothing pushed) left conflicts on 0b8779d", false)
	be.s.ResumeBabysits(be.ctx)
	if b := be.babysit(); !b.Active || b.State == BabysitNeedsPerson || be.babysitCount() != 2 {
		t.Fatalf("a stuck round: %+v", b)
	}
	// A review comment waiting for a person: left for them.
	be.setClock(be.clockNow().Add(resumeEvery + time.Minute))
	be.setBabysitState(BabysitNeedsPerson, "1 review comment waits for a person: c1", false)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO review_comments(pr_id, kind, ext_id, body, verdict, created_at, updated_at, seen_at) VALUES (?, 'inline', 'x1', 'b', ?, 0, 0, 0)`,
		pr.ID, VerdictNeedsPerson); err != nil {
		t.Fatal(err)
	}
	be.s.ResumeBabysits(be.ctx)
	if n := be.babysitCount(); n != 2 {
		t.Fatalf("a person's call was started again: %d babysits", n)
	}
}

func TestResumeBabysitsRetriesAFailedStart(t *testing.T) {
	be, _ := armedBabysit(t)
	// Ended for a resume whose start then failed: the next start tries again.
	be.setBabysitState(BabysitStopped, resumeEnded, true)
	be.s.ResumeBabysits(be.ctx)
	if b := be.babysit(); !b.Active || be.babysitCount() != 2 {
		t.Fatalf("a failed resume: %+v", b)
	}
	// Stopped by a person: left stopped.
	be.setClock(be.clockNow().Add(resumeEvery + time.Minute))
	be.setBabysitState(BabysitStopped, "stopped by Robin", true)
	be.s.ResumeBabysits(be.ctx)
	if n := be.babysitCount(); n != 2 {
		t.Fatalf("a person's stop was started again: %d babysits", n)
	}
}

func TestStalledLineRequest(t *testing.T) {
	be, _ := armedBabysit(t)
	open := func() []string {
		t.Helper()
		be.s.Wait()
		rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT title || '|' || body FROM requests WHERE tool_use_id LIKE ? AND state = 'open'`, lineStalledKey+"%")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				out = append(out, s)
			}
		}
		return out
	}
	// Babysat and moving: nothing to say.
	be.s.watchLines(be.ctx)
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if r := open(); len(r) != 0 {
		t.Fatalf("a moving line: %q", r)
	}
	// It gave up: Waypoint starts it again by itself, and asks nobody.
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)
	be.s.watchLines(be.ctx)
	if n := be.babysitCount(); n != 1 {
		t.Fatalf("restarted at once: %d babysits", n)
	}
	be.setClock(be.clockNow().Add(lineSelfHealAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if b := be.babysit(); !b.Active || b.Via != viaResume || be.babysitCount() != 2 {
		t.Fatalf("not started again: %+v", b)
	}
	if r := open(); len(r) != 0 {
		t.Fatalf("asked while it could free itself: %q", r)
	}
	// It gives up again and may not restart yet: hours on, one request.
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)
	be.setClock(be.clockNow().Add(lineStalledAfter - time.Minute))
	be.s.watchLines(be.ctx)
	if r := open(); len(r) != 0 {
		t.Fatalf("asked before %v: %q", lineStalledAfter, r)
	}
	be.setClock(be.clockNow().Add(2 * time.Minute))
	be.s.watchLines(be.ctx)
	be.s.watchLines(be.ctx)
	r := open()
	if len(r) != 1 || !strings.Contains(r[0], "Nothing is merging into me/app main: its one armed pull request isn't moving") ||
		!strings.Contains(r[0], "or made progress since ") || !strings.Contains(r[0], "#7 Add export: Waypoint gave up on it after") ||
		!strings.Contains(r[0], "5 rounds and still 1 review comment") || !strings.Contains(r[0], "wp pr babysit N") {
		t.Fatalf("stalled: %q", r)
	}
	if strings.Contains(r[0], "when it next restarts") || strings.Contains(r[0], "A review comment") {
		t.Fatalf("says what doesn't apply: %q", r)
	}
	// Dismissed while the line still stands: not asked again.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE requests SET state = ? WHERE tool_use_id LIKE ?`, RequestDismissed, lineStalledKey+"%"); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(time.Minute))
	be.s.watchLines(be.ctx)
	if r := open(); len(r) != 0 {
		t.Fatalf("asked again after a dismissal: %q", r)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE requests SET state = ? WHERE tool_use_id LIKE ?`, RequestOpen, lineStalledKey+"%"); err != nil {
		t.Fatal(err)
	}
	// Started again: the request goes away.
	be.setClock(be.clockNow().Add(resumeEvery))
	be.s.ResumeBabysits(be.ctx)
	be.babysit()
	be.s.watchLines(be.ctx)
	if r := open(); len(r) != 0 {
		t.Fatalf("moving again: %q", r)
	}
	var reason string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT result FROM requests WHERE tool_use_id LIKE ?`, lineStalledKey+"%").Scan(&reason); err != nil || !strings.Contains(reason, lineMovingAgain) {
		t.Fatalf("expired with %q (%v)", reason, err)
	}
}

// How long a line has been quiet comes from the database: a deploy doesn't
// start the count again, and arming a pull request does.
func TestStalledLineCountsFromTheDatabase(t *testing.T) {
	be, pr := armedBabysit(t)
	be.setBabysitState(BabysitNeedsPerson, "waiting on something only a person can do", false)
	count := func() int {
		t.Helper()
		be.s.Wait()
		var n int
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, lineStalledKey+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Armed afresh after the wait began: the count starts there.
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ? WHERE id = ?`, be.s.nowMS(), pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.watchLines(be.ctx)
	if n := count(); n != 0 {
		t.Fatalf("asked at once about a line just armed: %d", n)
	}
	// No sweep saw the wait (Waypoint restarted); it counts all the same.
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if n := count(); n != 1 {
		t.Fatalf("a line quiet for %v across a restart: %d requests", lineStalledAfter, n)
	}
}

// repoArmed makes pull request 7 one the repo's Merge automatically armed,
// opened on the forge by login, with no babysit, and has the forge's token
// belong to waypointLogin.
func (be *bsEnv) repoArmed(pr *PullRequest, login, waypointLogin string) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM babysits`); err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_by = NULL, author = ?, forge_author = ? WHERE id = ?`, login, login, pr.ID); err != nil {
		be.t.Fatal(err)
	}
	f, err := be.s.forgeByID(be.ctx, pr.ForgeID)
	if err != nil {
		be.t.Fatal(err)
	}
	if f.token == "" {
		be.t.Fatal("the test forge has no token")
	}
	for _, bot := range []bool{true, false} {
		be.s.botLogins.Store(tokenLoginKey{f.ID, bot}, botLogin{token: f.token, login: waypointLogin, at: be.s.now()})
	}
}

// An agent's pull request the repo's Merge automatically armed, opened by
// Waypoint's own forge account, is babysat by the project's owner.
func TestStalledLineBabysitsARepoArmedPullRequest(t *testing.T) {
	be, pr := armedBabysit(t)
	be.repoArmed(pr, "waypoint-agent", "waypoint-agent")
	got, err := be.s.prByID(be.ctx, be.s.db.R, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if be.s.babysitOwnerFor(be.ctx, got) != 0 || be.s.armedOwnerFor(be.ctx, got) != be.admin.User.ID {
		t.Fatalf("owner: babysit %d, armed %d", be.s.babysitOwnerFor(be.ctx, got), be.s.armedOwnerFor(be.ctx, got))
	}
	be.setClock(be.clockNow().Add(lineSelfHealAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if n := be.babysitCount(); n != 1 {
		t.Fatalf("a line stuck on a pull request nobody babysits: %d babysits", n)
	}
}

// Text saying an agent wrote it is its author's to write: by itself it gets
// a non-member's pull request no owner's runner, and a person is asked.
func TestStalledLineKeepsAClaimedAgentOffAnOwnersRunner(t *testing.T) {
	be, pr := armedBabysit(t)
	be.repoArmed(pr, "stranger", "waypoint-agent")
	be.setClock(be.clockNow().Add(lineSelfHealAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if n := be.babysitCount(); n != 0 {
		t.Fatalf("a trailer alone reached a runner: %d babysits", n)
	}
	be.setClock(be.clockNow().Add(lineStalledAfter))
	be.s.watchLines(be.ctx)
	be.s.Wait()
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id LIKE ? AND state = 'open' AND user_id = ?`, lineStalledKey+"%", be.admin.User.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the project's owner wasn't asked: %d (%v)", n, err)
	}
}

// Not an agent's and not a member's: no runner may take it (D14), whoever
// owns the project, and the request says why instead of offering a babysit.
func TestStalledLineLeavesAnOutsidersPullRequestToAPerson(t *testing.T) {
	be, pr := armedBabysit(t)
	if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM babysits`); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_by = NULL, agent = '' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	be.s.watchLines(be.ctx)
	if n := be.babysitCount(); n != 0 {
		t.Fatalf("handed to a runner past the gate: %d babysits", n)
	}
	line := lineRef{forge: pr.ForgeID, repo: pr.Repo, base: pr.BaseRef}
	prs, err := be.s.armedOn(be.ctx, line)
	if err != nil || len(prs) != 1 {
		t.Fatalf("armed: %v %v", prs, err)
	}
	body := be.s.stalledLineBody(be.ctx, line, prs, be.clockNow())
	if !strings.Contains(body, "isn't a member of this project") || strings.Contains(body, "wp pr babysit N") {
		t.Fatalf("body:\n%s", body)
	}
}

// Only a pull request that really has a comment waiting says so, and one
// whose fix a session must push doesn't call for a person.
func TestStalledLineSaysWhatApplies(t *testing.T) {
	be, pr := armedBabysit(t)
	be.setBabysitState(BabysitNeedsPerson, "1 review comment waits for a person: c1", false)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO review_comments(pr_id, kind, ext_id, body, verdict, created_at, updated_at, seen_at) VALUES (?, 'inline', 'x1', 'b', ?, 0, 0, 0)`,
		pr.ID, VerdictNeedsPerson); err != nil {
		t.Fatal(err)
	}
	line := lineRef{forge: pr.ForgeID, repo: pr.Repo, base: pr.BaseRef}
	prs, err := be.s.armedOn(be.ctx, line)
	if err != nil || len(prs) != 1 {
		t.Fatalf("armed: %v %v", prs, err)
	}
	since := time.Date(2026, 11, 2, 4, 12, 0, 0, time.UTC)
	body := be.s.stalledLineBody(be.ctx, line, prs, since)
	if !strings.Contains(body, "since 2 Nov 04:12 UTC") || !strings.Contains(body, "It has one armed pull request") || !strings.Contains(body, "wp pr comments N") {
		t.Fatalf("body:\n%s", body)
	}
}

// A line stalled on a full run that hasn't finished doesn't suggest a
// babysit restart, which waits for the same run (WP-552).
func TestStalledLineOnAFullRun(t *testing.T) {
	be, _ := armedBabysit(t)
	be.setBabysitState(BabysitNeedsPerson, "full run hasn't finished since it was marked ready (6 of 7 checks) in 45m (default)", false)
	be.s.watchLines(be.ctx)
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	be.s.watchLines(be.ctx)
	be.s.watchLines(be.ctx)
	be.s.Wait()
	var body string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT body FROM requests WHERE tool_use_id LIKE ? AND state = 'open'`, lineStalledKey+"%").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "its CI hasn't finished the full run Waypoint waits for") || strings.Contains(body, "wp pr babysit N") || strings.Contains(body, "waits for a person") {
		t.Fatalf("body:\n%s", body)
	}
}

// A full run's wait rewritten as the repo's measured runs change is the same
// wait: the line stays quiet and its request stays open.
func TestStalledLineKeepsCountingARewrittenFullRunWait(t *testing.T) {
	be, pr := armedBabysit(t)
	wait := func(basis string) string {
		return "full run hasn't finished since it was marked ready (6 of 7 checks) in 45m (" + basis + ")"
	}
	be.setBabysitState(BabysitNeedsPerson, wait("the default"), false)
	be.setClock(be.clockNow().Add(lineStalledAfter + time.Minute))
	be.s.watchLines(be.ctx)
	open := func() int {
		t.Helper()
		be.s.Wait()
		var n int
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id LIKE ? AND state = 'open'`, lineStalledKey+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := open(); n != 1 {
		t.Fatalf("stalled on a full run: %d requests", n)
	}
	b := be.babysit()
	since := b.WaitSince
	be.setClock(be.clockNow().Add(time.Minute))
	be.s.setState(be.ctx, b, pr, BabysitNeedsPerson, wait("1.5 × the 30m p90 of its last 9 full runs"))
	be.s.watchLines(be.ctx)
	if n := open(); n != 1 {
		t.Fatalf("a rewritten wait closed the request: %d open", n)
	}
	if got := be.babysit(); got.Wait != wait("1.5 × the 30m p90 of its last 9 full runs") || !got.WaitSince.Equal(since) {
		t.Fatalf("wait %q since %s, want since %s", got.Wait, got.WaitSince, since)
	}
	// A different wait is a change, and starts the count again.
	be.s.setState(be.ctx, b, pr, BabysitNeedsPerson, "couldn't mark it ready: the forge refused")
	if got := be.babysit(); !got.WaitSince.Equal(be.clockNow()) {
		t.Fatalf("a new wait since %s, want %s", got.WaitSince, be.clockNow())
	}
}

// What a pull request waits on is its wait, not its last round: a round
// refused for protected files says nothing once the wait has moved on.
func TestStalledLineReadsTheCurrentWait(t *testing.T) {
	refused := []BabysitRound{{N: 1, To: "runner", Outcome: "The agent changed protected files (.forgejo/x.yml), so nothing was pushed."}}
	moved := &Babysit{State: BabysitNeedsPerson, Wait: "couldn't merge: the forge refused", Rounds: refused}
	if waitsForSession(moved) {
		t.Error("a wait that has moved on still reads as a session's")
	}
	parked := &Babysit{State: BabysitNeedsPerson, Wait: "round 1's fix changes protected files (.forgejo/x.yml), which a round may not push; failed checks still on abc", Rounds: refused}
	if !waitsForSession(parked) {
		t.Error("the protected-file wait doesn't read as a session's")
	}
}
