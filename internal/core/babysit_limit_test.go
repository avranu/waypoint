package core

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// failWith ends a round's job as failed with the runner's message.
func (be *bsEnv) failWith(id int64, msg string) {
	be.t.Helper()
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "failed", nil, nil, msg) }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// toAssessor fails the check and lets rounds 1 and 2 end without a fix, so
// the assessor's round 3 is running; it returns that babysit.
func (be *bsEnv) toAssessor() *Babysit {
	be.t.Helper()
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "--- FAIL: TestExport (0.01s)\nFAIL\tapp\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	for i := 0; i < 2; i++ {
		b := be.babysit()
		if b.JobID == 0 {
			be.t.Fatalf("round %d has no job: %+v", i+1, b)
		}
		be.finish(b.JobID, false)
	}
	b := be.babysit()
	if b.Round != 3 || b.JobID == 0 || !b.Rounds[2].Assessor {
		be.t.Fatalf("not at the assessor: %+v", b)
	}
	return b
}

// An assessor round that hits its owner's weekly limit isn't a round: the
// pull request isn't parked for a person, leaves the merge line, and the
// same rung runs again once the limit lifts (WP-683, 2026-11-07).
func TestBabysitRoundAtUsageLimit(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	b := be.toAssessor()
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET armed_at = 1, queued_at = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	msg := "claude reported an error: You've hit your weekly limit · resets 5pm (UTC)"
	reset, ok := limitResetIn(msg, be.clockNow())
	if !ok {
		t.Fatal("no reset read")
	}
	before := map[string]int{"lint": 2, "test": 1}
	if _, err := be.s.db.W.Exec(`UPDATE babysits SET prev_streaks = '{"lint":2,"test":1}' WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	be.failWith(b.JobID, msg)
	b = be.babysit()
	if !maps.Equal(b.streaks, before) {
		t.Errorf("streaks %v, want those before the round %v", b.streaks, before)
	}
	if b.Round != 2 || len(b.Rounds) != 2 || b.JobID != 0 || b.limitedUntil != reset.UnixMilli() {
		t.Fatalf("after the limit: round %d, %d rounds, job %d, until %v (want %v)", b.Round, len(b.Rounds), b.JobID, msTime(b.limitedUntil), reset)
	}
	if b.State != BabysitWatching || !strings.Contains(b.Wait, "usage limit stopped the last round") || !strings.Contains(b.Wait, "17:00 UTC") {
		t.Fatalf("state %s, wait %q", b.State, b.Wait)
	}
	if parkedOn(b, pr.HeadSHA) {
		t.Fatal("parked for a person")
	}
	var asked int
	_ = be.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, parkRequestKey+"%").Scan(&asked)
	if asked != 0 {
		t.Fatalf("%d park questions", asked)
	}
	if p := be.pr(7); p.QueuedAt != nil {
		t.Error("it holds its place in the merge line while it waits")
	}
	be.setClock(reset.Add(-time.Minute))
	be.runnerOnline()
	be.step()
	if n := len(be.jobs()); n != 3 {
		t.Fatalf("%d jobs before the reset", n)
	}
	be.setClock(reset.Add(time.Minute))
	be.runnerOnline()
	be.step()
	b = be.babysit()
	if len(be.jobs()) != 4 || b.Round != 3 || b.JobID == 0 || !b.Rounds[2].Assessor {
		t.Fatalf("after the reset: %d jobs, %+v", len(be.jobs()), b)
	}
}

// A limit with no reset in its message waits for the quota window the
// owner's status line last read as full, else limitRetry.
func TestLimitLifts(t *testing.T) {
	be := newBabysitEnv(t)
	uid := be.admin.User.ID
	now := be.clockNow()
	if got, known := be.s.limitLifts(be.ctx, uid, "rate limit"); known || !got.Equal(now.Add(limitRetry)) {
		t.Errorf("no reset known: %v, %v", got, known)
	}
	week := now.Add(30 * time.Hour).Truncate(time.Millisecond)
	if _, err := be.s.db.W.Exec(`INSERT INTO quota_samples(user_id, at, span, percent, resets_at) VALUES (?, ?, 'seven_day', 100, ?), (?, ?, 'five_hour', 40, ?)`,
		uid, now.UnixMilli(), week.UnixMilli(), uid, now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if got, known := be.s.limitLifts(be.ctx, uid, "usage limit"); !known || !got.Equal(week) {
		t.Errorf("the full week: %v, want %v", got, week)
	}
	want, _ := limitResetIn("You've hit your weekly limit · resets 5pm (UTC)", now)
	if got, known := be.s.limitLifts(be.ctx, uid, "You've hit your weekly limit · resets 5pm (UTC)"); !known || !got.Equal(want) {
		t.Error("the message's reset doesn't come first")
	}
}

func TestLimitText(t *testing.T) {
	for text, want := range map[string]bool{
		"claude reported an error: You've hit your weekly limit · resets 5pm (UTC)": true,
		"You've hit your limit · resets 3am":                                        true,
		"5-hour limit reached ∙ resets 3pm":                                         true,
		"Claude AI usage limit reached|1762534800":                                  true,
		"Session limit reached":                                                     true,
		"API Error: 429 Too Many Requests":                                          true,
		`API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`:       true,
		"fatal: The requested URL returned error: 429":                              true,
		"API Error: 500 Internal server error (request_id req_01A429bc)":            false,
		"push of 3f4290ab rejected":                                                 false,
		"the round for PR #429 ended":                                               false,
		"Context limit reached · /compact or /clear to continue":                    false,
		"claude reported an error: tests failed":                                    false,
		"pushing the branch failed: protected files":                                false,
	} {
		if got := limitText(text); got != want {
			t.Errorf("limitText(%q) = %v", text, got)
		}
	}
}

func TestLimitResetIn(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 34, 0, 0, time.UTC)
	for text, want := range map[string]time.Time{
		"You've hit your weekly limit · resets 5pm (UTC)": time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC),
		"resets 1pm (UTC)":                                  time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC),
		"resets 12am":                                       time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		"resets 5:30pm":                                     time.Date(2026, 11, 7, 17, 30, 0, 0, time.UTC),
		"resets 18:15 (UTC)":                                time.Date(2026, 11, 7, 18, 15, 0, 0, time.UTC),
		"resets Nov 9, 5pm (UTC)":                           time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC),
		"resets Jan 2 at 9am (UTC)":                         time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC),
		"resets 5pm (America/New_York)":                     time.Date(2026, 11, 7, 22, 0, 0, 0, time.UTC),
		"Claude AI usage limit reached|1794100000":          time.Unix(1794100000, 0).UTC(),
		"5-hour limit reached ∙ resets 3pm (Europe/London)": time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC),
	} {
		got, ok := limitResetIn(text, now)
		if !ok || !got.Equal(want) {
			t.Errorf("limitResetIn(%q) = %v, %v; want %v", text, got, ok, want)
		}
	}
	for _, text := range []string{"usage limit", "resets 5", "resets 25:00", "resets 5pm (Nowhere/Land)", "resets Nov 7, 1pm (UTC)"} {
		if got, ok := limitResetIn(text, now); ok {
			t.Errorf("limitResetIn(%q) = %v", text, got)
		}
	}
}

// Only the agent's own failure can be a limit: the runner's (a push, a
// protected file) names branches and paths that a limit's words turn up in.
func TestRoundLimited(t *testing.T) {
	for msg, want := range map[string]bool{
		"claude reported an error: You've hit your weekly limit · resets 5pm (UTC)":            true,
		"claude exited with an error: exit status 1. API Error: 429 Too Many Requests":         true,
		"pushing wp-412-quota-rate-limit failed: exit status 1":                                false,
		"The agent changed protected files (internal/quota/limits.go), so nothing was pushed.": false,
		"claude reported an error: tests failed":                                               false,
	} {
		if got := roundLimited(&RunnerJob{State: "failed", Error: msg}); got != want {
			t.Errorf("roundLimited(%q) = %v", msg, got)
		}
	}
	if roundLimited(&RunnerJob{State: "done", Error: "claude reported an error: rate limit"}) {
		t.Error("a done job")
	}
}

// A limit with no reset anywhere is retried limitTries times, then counted:
// words that only looked like a limit can't hold the pull request off the
// ladder for good, and the assessor's counted failure parks it for a person.
func TestBabysitLimitWithoutResetCounts(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	b := be.toAssessor()
	msg := "claude reported an error: rate limit"
	for i := 1; i <= limitTries; i++ {
		be.failWith(b.JobID, msg)
		b = be.babysit()
		if b.JobID != 0 || b.limitTries != i || len(b.Rounds) != 2 {
			t.Fatalf("try %d: job %d, tries %d, %d rounds", i, b.JobID, b.limitTries, len(b.Rounds))
		}
		be.setClock(be.clockNow().Add(limitRetry + time.Minute))
		be.runnerOnline()
		be.step()
		if b = be.babysit(); b.JobID == 0 || !b.Rounds[len(b.Rounds)-1].Assessor {
			t.Fatalf("try %d: the assessor didn't go again: %+v", i, b)
		}
	}
	be.failWith(b.JobID, msg)
	b = be.babysit()
	if len(b.Rounds) != 3 || b.limitTries != 0 {
		t.Fatalf("the last try wasn't counted: %d rounds, tries %d", len(b.Rounds), b.limitTries)
	}
	be.step()
	if !parkedOn(be.babysit(), pr.HeadSHA) {
		t.Fatal("not parked for a person")
	}
}

// A transient HTTP 429 is still "busy" for audits; only a spent plan is "spent".
func TestAuditFailureKindKeeps429Busy(t *testing.T) {
	for text, want := range map[string]string{
		"API Error: 429 too many requests":                                                                       "busy",
		"You've hit your weekly limit · resets 5pm (UTC)":                                                        "spent",
		"opencode's model refused: Free usage limit reached. Free models work in OpenCode (https://opencode.ai)": "spent",
	} {
		if got := auditFailureKind(&stepJob{Error: text}); got != want {
			t.Errorf("auditFailureKind(%q) = %q, want %q", text, got, want)
		}
	}
}
