package core

import (
	"strings"
	"testing"
	"time"
)

func (be *bsEnv) labelUrgent(n int) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET labels = '["bug","CI-Urgent"]' WHERE id = ?`, be.pr(n).ID); err != nil {
		be.t.Fatal(err)
	}
}

// An urgent pull request's review comments get their round while its
// checks still run, and a runner takes that round before older jobs; one
// that isn't urgent waits for its checks (WP-686).
func TestUrgentRoundDoesntWaitForChecks(t *testing.T) {
	for _, urgent := range []bool{false, true} {
		t.Run(map[bool]string{false: "not urgent", true: "urgent"}[urgent], func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			at := be.clockNow().Add(-time.Minute)
			be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
				{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: at}}}}
			pr := be.open()
			if urgent {
				be.labelUrgent(7)
			}
			older := be.enqueue(be.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("make check", "/work")})
			be.start(pr)
			be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
			be.poll()
			be.runnerOnline()
			be.step()
			b := be.babysit()
			jobs := be.jobs()
			if !urgent {
				if len(jobs) != 0 || !strings.Contains(b.Wait, "checks running") {
					t.Fatalf("a round before the checks finished: %d jobs, %s %q", len(jobs), b.State, b.Wait)
				}
				return
			}
			if len(jobs) != 1 || b.State != BabysitWorking {
				t.Fatalf("no round while the checks run: %d jobs, %s %q", len(jobs), b.State, b.Wait)
			}
			if c := be.comment("501"); c.Verdict != VerdictSeen {
				t.Errorf("the comment is %s, want seen", c.Verdict)
			}
			runners, err := be.s.ownerRunners(be.ctx, be.admin.User.ID)
			if err != nil || len(runners) != 1 {
				t.Fatalf("runners: %v %v", runners, err)
			}
			r := runners[0]
			if w := be.env.poll(be.admin, r, RunnerPoll{}); w.Job == nil || w.Job.ID != jobs[0] {
				t.Errorf("the runner took %+v first, want the urgent round %d", w.Job, jobs[0])
			}
			if w := be.env.poll(be.admin, r, RunnerPoll{}); w.Job == nil || w.Job.ID != older.ID {
				t.Errorf("then %+v, want the older command %d", w.Job, older.ID)
			}
		})
	}
}

// An urgent pull request's round waits when the reads that show its reviews
// settled fail: the review runs or audits (finding 243.1) or the repo's
// merge policy (243.2). Before WP-748 a failed read counted as settled.
func TestUrgentRoundWaitsOnFailedReads(t *testing.T) {
	for _, tc := range []struct{ name, breakSQL, mendSQL string }{
		{"review runs", `ALTER TABLE bot_runs RENAME TO bot_runs_x`, `ALTER TABLE bot_runs_x RENAME TO bot_runs`},
		{"merge policy", `UPDATE project_repos SET quiet_minutes = 'soon'`, `UPDATE project_repos SET quiet_minutes = 0`},
		{"audits", `ALTER TABLE code_audits RENAME TO code_audits_x`, `ALTER TABLE code_audits_x RENAME TO code_audits`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
				{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
			pr := be.open()
			be.labelUrgent(7)
			b := be.start(pr)
			be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
			be.poll()
			if _, err := be.s.db.W.ExecContext(be.ctx, tc.breakSQL); err != nil {
				t.Fatal(err)
			}
			if be.s.urgentRound(be.ctx, b, be.pr(7)) {
				t.Fatal("a round started on a read that failed")
			}
			if _, err := be.s.db.W.ExecContext(be.ctx, tc.mendSQL); err != nil {
				t.Fatal(err)
			}
			if !be.s.urgentRound(be.ctx, b, be.pr(7)) {
				t.Fatal("no round once the read works")
			}
		})
	}
}
