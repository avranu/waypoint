package core

import (
	"slices"
	"strings"
	"testing"
)

// A Stop that leaves a background run going names the outstanding work, once;
// a Stop with nothing running, or with nothing outstanding, says nothing.
func TestStopNamesOutstandingWorkWhileARunGoes(t *testing.T) {
	a, box := auditAPEnv(t)
	_, audit, _ := handoffStart(t, a)
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp testsum -- make check"}
	// The round's own prompt is still to come: stops until it are quiet. A
	// session at work is not in that state.
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
		t.Fatal(err)
	}

	if res := a.stopWith("Waiting.", run); res.Text != "" {
		t.Fatalf("before the audit's findings: %q", res.Text)
	}
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error")
	if res := a.stopWith("Done."); strings.Contains(res.Text, "wp audit mark") {
		t.Errorf("nothing runs, yet the stop named work: %q", res.Text)
	}
	res := a.stopWith("Waiting.", run)
	for _, want := range []string{"still running", "audit #" + itoa64(audit), itoa64(audit) + ".1", itoa64(audit) + ".2", "wp audit mark"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the stop's text lacks %q:\n%s", want, res.Text)
		}
	}
	// The continued turn's own stop is not held again.
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", func(h *HookEvent) {
		h.StopHookActive = true
		h.Pending = &HookPending{Tasks: []PendingTask{run}, Crons: []PendingCron{}}
	})})
	if err != nil || strings.Contains(res.Text, "wp audit mark") {
		t.Errorf("a stop the hook continued: %q %v", res.Text, err)
	}
}

// A session's outstanding work is the unmarked findings of its open review
// round's audit and the answered requests it hasn't read (WP-1571); nothing
// else, and nothing for a session with none.
func TestSessionOutstanding(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	empty, err := a.s.SessionOutstanding(a.ctx, &ag)
	if err != nil || !empty.Empty() {
		t.Fatalf("before any round: %+v %v", empty, err)
	}

	round, audit, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error", "Dead code")
	// one finding already marked: no longer outstanding
	if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET state = 'rejected' WHERE audit_id = ? AND ordinal = 2`, audit); err != nil {
		t.Fatal(err)
	}
	got, err := a.s.SessionOutstanding(a.ctx, &ag)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{itoa64(audit) + ".1", itoa64(audit) + ".3"}
	if got.Audit != audit || !slices.Equal(got.Findings, want) {
		t.Fatalf("findings = %d %v, want %d %v (round %d)", got.Audit, got.Findings, audit, want, round)
	}

	// A round that is over leaves nothing outstanding.
	a.s.setRoundNow(a.ctx, round, []string{RoundPending, RoundRunning}, RoundSkipped, "test")
	if got, err = a.s.SessionOutstanding(a.ctx, &ag); err != nil || len(got.Findings) != 0 {
		t.Fatalf("after the round ended: %+v %v", got, err)
	}

	// An answered request the session hasn't read.
	req, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Squash the migrations?", Project: a.p.Key,
		Questions: []Question{{Question: "Squash?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag); len(got.Requests) != 0 {
		t.Fatalf("an open request is not outstanding: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'answered', decided_at = 1, via = '' WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag); !slices.Equal(got.Requests, []string{req.Code}) {
		t.Fatalf("requests = %v, want [%s]", got.Requests, req.Code)
	}
}
