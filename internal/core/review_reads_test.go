package core

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// readBreaks are the reads that show a pull request's reviews settled, each
// broken and mended (WP-748, audits #243 and #674).
var readBreaks = []struct{ name, breakSQL, mendSQL, logs string }{
	{"review runs", `ALTER TABLE bot_runs RENAME TO bot_runs_x`, `ALTER TABLE bot_runs_x RENAME TO bot_runs`, "read whether its reviews settled"},
	{"merge policy", `UPDATE project_repos SET quiet_minutes = 'soon'`, `UPDATE project_repos SET quiet_minutes = 0`, "read its merge policy"},
	{"audits", `ALTER TABLE code_audits RENAME TO code_audits_x`, `ALTER TABLE code_audits_x RENAME TO code_audits`, "read whether its reviews settled"},
}

func (be *bsEnv) exec(sql string) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, sql); err != nil {
		be.t.Fatal(err)
	}
}

// A babysit whose reads of the reviews fail starts no round and says why in
// the log, so a stall that lasts can be found (finding 674.3).
func TestBabysitRoundWaitsOnFailedReads(t *testing.T) {
	for _, tc := range readBreaks {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			logs := &lockedBuf{}
			be.s.log = slog.New(slog.NewTextHandler(logs, nil))
			be.runnerOnline()
			be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
				{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
			pr := be.open()
			// A reviewer still expected holds the round until the review wait
			// passes, so the comment is read before the read breaks.
			be.policy(MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}, ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)})
			be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
			be.poll()
			be.start(pr)
			be.exec(tc.breakSQL)
			be.step()
			if n := len(be.jobs()); n != 0 {
				t.Fatalf("%d rounds started on a read that failed while the checks run", n)
			}
			be.ci(bsPR.HeadSHA, CheckSuccess)
			be.step()
			if n := len(be.jobs()); n != 0 {
				t.Fatalf("%d rounds started on a read that failed", n)
			}
			if !strings.Contains(logs.String(), tc.logs) {
				t.Errorf("no %q in the log:\n%s", tc.logs, logs.String())
			}
			be.exec(tc.mendSQL)
			be.setClock(be.clockNow().Add(31 * time.Minute))
			be.runnerOnline()
			be.step()
			if n := len(be.jobs()); n != 1 {
				b := be.babysit()
				t.Fatalf("%d rounds once the read works, want 1: %s %q", n, b.State, b.Wait)
			}
		})
	}
}

// An urgent pull request's round that waits on a failed read says why in the
// log (finding 674.3).
func TestUrgentRoundLogsFailedReads(t *testing.T) {
	for _, tc := range readBreaks {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			logs := &lockedBuf{}
			be.s.log = slog.New(slog.NewTextHandler(logs, nil))
			be.runnerOnline()
			be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
				{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
			pr := be.open()
			be.labelUrgent(7)
			b := be.start(pr)
			be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
			be.poll()
			be.exec(tc.breakSQL)
			if be.sendEarlyRound(b) {
				t.Fatal("a round started on a read that failed")
			}
			if !strings.Contains(logs.String(), tc.logs) {
				t.Errorf("no %q in the log:\n%s", tc.logs, logs.String())
			}
		})
	}
}

// A failed read of the running audits holds the merge rather than counting
// as "no audit running" (finding 674.1).
func TestMergeHoldsOnFailedAuditRead(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	be.auditOn()
	pr := be.openMine()
	holds := func() []string {
		t.Helper()
		var rd PRReadiness
		reasons, err := be.s.reviewHolds(be.ctx, be.pr(pr.Number), true, &rd)
		if err != nil {
			t.Fatalf("reviewHolds: %v", err)
		}
		return reasons
	}
	if got := holds(); !slicesHas(got, "waiting for audit #") {
		t.Fatalf("a running audit doesn't hold: %q", got)
	}
	be.exec(`ALTER TABLE code_audits RENAME TO code_audits_x`)
	if got := holds(); !slicesHas(got, "the running audits couldn't be read") {
		t.Fatalf("a failed audit read doesn't hold the merge: %q", got)
	}
}

// A failed read of the review runs leaves a head unsettled, so no stack or
// batch is built on it (finding 674.2).
func TestUnsettledOnFailedReviewRead(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	before, _ := be.s.unsettled(be.ctx, be.pr(pr.Number))
	be.exec(`ALTER TABLE bot_runs RENAME TO bot_runs_x`)
	if why, unread := be.s.unsettled(be.ctx, be.pr(pr.Number)); why != "its reviews couldn't be read" || !unread {
		t.Fatalf("a failed review-runs read: %q (before it broke: %q)", why, before)
	}
}

func slicesHas(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}
