package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A Stop that leaves a background run going names the outstanding work, once;
// a Stop with nothing running, or with nothing outstanding, says nothing. The
// wait hook naming it first doesn't stop the Stop naming it (audit #1344.1).
func TestStopNamesOutstandingWorkWhileARunGoes(t *testing.T) {
	a, box := auditAPEnv(t)
	_, audit, _ := handoffStart(t, a)
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp testsum -- make check"}
	// The round's own prompt is still to come: stops until it are quiet. A
	// session at work is not in that state.
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
		t.Fatal(err)
	}
	ag := *a.ag
	ag.Session = a.session
	f := func(n int) string { return itoa64(audit) + "." + itoa64(int64(n)) }

	if res := a.stopWith("Waiting.", run); res.Text != "" {
		t.Fatalf("before the audit's findings: %q", res.Text)
	}
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error")
	if res := a.stopWith("Done."); strings.Contains(res.Text, "wp audit mark") {
		t.Errorf("nothing runs, yet the stop named work: %q", res.Text)
	}
	// The wait hook named them when the run went to the background.
	if o, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !slices.Equal(o.Findings, []string{f(1), f(2)}) {
		t.Fatalf("at the wait: %+v %v", o, err)
	}
	// The agent didn't act on them (or never got the hook's answer): the Stop
	// still names them, with at most three of what runs.
	more := []PendingTask{run, {Type: "local_bash", Status: "running", Description: "run b"}, {Type: "local_bash", Status: "running", Description: "run c"}, {Type: "local_bash", Status: "running", Description: "run d"}}
	res := a.stopWith("Waiting.", more...)
	for _, want := range []string{"still running", "run c", "audit #" + itoa64(audit), f(1), f(2), "wp audit mark"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the stop's text lacks %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "run d") {
		t.Errorf("more than three runs named:\n%s", res.Text)
	}
	// Told once: the same findings at the next stop are not repeated, or an
	// agent holding them while a worker fixes them is re-prompted at every stop.
	if res := a.stopWith("Still waiting.", run); res.Text != "" {
		t.Errorf("a second stop repeated the findings: %q", res.Text)
	}
	// A new finding names all that are open, so the count is the whole
	// (audit #1344.10).
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 3, 'Late finding')`, audit); err != nil {
		t.Fatal(err)
	}
	res = a.stopWith("Waiting more.", run)
	for _, want := range []string{"3 findings still to do", f(1), f(2), f(3)} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("after a new finding the stop lacks %q:\n%s", want, res.Text)
		}
	}
	// The continued turn's own stop is not held again, even with a finding
	// it wasn't told of (audit #1344.33); the next stop names it.
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 4, 'Later finding')`, audit); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "Stop", func(h *HookEvent) {
		h.StopHookActive = true
		h.Pending = &HookPending{Tasks: []PendingTask{run}, Crons: []PendingCron{}}
	})})
	if err != nil || strings.Contains(res.Text, "wp audit mark") {
		t.Errorf("a stop the hook continued: %q %v", res.Text, err)
	}
	if res := a.stopWith("Waiting still.", run); !strings.Contains(res.Text, f(4)) {
		t.Errorf("the finding the continued stop let through is never named: %q", res.Text)
	}
}

// A follow-up with something to tell the agent goes first: the Stop that
// would name outstanding work waits for it instead (audit #1344.18).
func TestStopWithAFollowUpTalkingDoesntNameOutstandingWork(t *testing.T) {
	a, box := auditAPEnv(t)
	handoffStart(t, a)
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
		t.Fatal(err)
	}
	a.endAudit(box, "done", "Merge reads a nil map")
	if _, err := a.s.SaveFollowUp(a.ctx, a.admin, a.session, FollowUpInput{Steps: []Action{step("tell_agent", map[string]any{"message": "Now write the tests."})}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO automation_runs(automation_id, rule_id, status, created_at)
		SELECT r.automation_id, r.id, 'running', 0 FROM automation_rules r JOIN automations x ON x.id = r.automation_id WHERE x.session_id = ?`, a.session); err != nil {
		t.Fatal(err)
	}
	if !a.s.followUpTalks(a.ctx, a.session) {
		t.Fatal("the follow-up doesn't talk")
	}
	res := a.stopWith("Waiting.", PendingTask{Type: "local_bash", Status: "running", Description: "make check"})
	if strings.Contains(res.Text, "wp audit mark") || res.Await == 0 {
		t.Errorf("the stop named work instead of waiting for the follow-up: %+v", res)
	}
}

// A session's outstanding work is the unmarked findings of its open review
// round's audit and the answered requests it hasn't read (WP-1571); nothing
// else, and nothing for a session with none, nor for another session.
func TestSessionOutstanding(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	if _, err := a.s.SessionOutstanding(a.ctx, nil, OutstandingAtWait); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: %v", err)
	}
	if o, err := a.s.SessionOutstanding(a.ctx, a.admin, OutstandingAtWait); err != nil || !o.Empty() {
		t.Errorf("not a session: %+v %v", o, err)
	}
	empty, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if err != nil || !empty.Empty() {
		t.Fatalf("before any round: %+v %v", empty, err)
	}

	round, audit, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error", "Dead code")
	// one finding already marked: no longer outstanding
	if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET state = 'rejected' WHERE audit_id = ? AND ordinal = 2`, audit); err != nil {
		t.Fatal(err)
	}
	// Another session has none of them.
	other := ag
	other.Session = a.session + 1000
	if o, err := a.s.SessionOutstanding(a.ctx, &other, OutstandingAtWait); err != nil || !o.Empty() {
		t.Fatalf("another session's: %+v %v", o, err)
	}
	got, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{itoa64(audit) + ".1", itoa64(audit) + ".3"}
	if got.Audit != audit || !slices.Equal(got.Findings, want) {
		t.Fatalf("findings = %d %v, want %d %v (round %d)", got.Audit, got.Findings, audit, want, round)
	}

	// Told once at each way: asking again at the wait names nothing, the
	// Stop still names them once, and a finding that turns up later names all.
	if again, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !again.Empty() || again.Lines != "" {
		t.Fatalf("asked twice: %+v %v", again, err)
	}
	if atStop, _ := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtStop); !slices.Equal(atStop.Findings, want) {
		t.Fatalf("at the stop after the wait: %v", atStop.Findings)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 4, 'New one')`, audit); err != nil {
		t.Fatal(err)
	}
	if more, _ := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !slices.Equal(more.Findings, append(want, itoa64(audit)+".4")) || !strings.Contains(more.Lines, "3 findings") {
		t.Fatalf("new finding = %v %q", more.Findings, more.Lines)
	}
	// The record is bookkeeping, not something the session did: the
	// session's timeline doesn't list it.
	sum, err := a.s.GetAgentSession(a.ctx, &ag, a.session)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Timeline) == 0 {
		t.Fatal("the timeline is empty, so it can't show the record is left out") // audit #1372.26
	}
	for _, e := range sum.Timeline {
		if e.Kind == "outstanding" {
			t.Fatalf("the timeline lists an outstanding record: %+v", e)
		}
	}

	// A round that is over leaves nothing outstanding.
	a.s.setRoundNow(a.ctx, round, []string{RoundPending, RoundRunning}, RoundSkipped, "test")
	if got, err = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || len(got.Findings) != 0 {
		t.Fatalf("after the round ended: %+v %v", got, err)
	}

	// An answered request the session hasn't read, named once.
	req, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Squash the migrations?", Project: a.p.Key,
		Questions: []Question{{Question: "Squash?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); len(got.Requests) != 0 {
		t.Fatalf("an open request is not outstanding: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'answered', decided_at = 1, via = '' WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !slices.Equal(got.Requests, []string{req.Code}) || !strings.Contains(got.Lines, "1 answered request you haven't read ("+req.Code+")") {
		t.Fatalf("requests = %v %q, want [%s]", got.Requests, got.Lines, req.Code)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !got.Empty() {
		t.Fatalf("the request named twice: %+v", got)
	}
	// Another session has none of the session's requests (audit #1372.22).
	if o, err := a.s.SessionOutstanding(a.ctx, &other, OutstandingAtStop); err != nil || !o.Empty() {
		t.Fatalf("another session's, with a request: %+v %v", o, err)
	}
	// An answer the session was told of (in a wakeup, or by wp request show)
	// is not outstanding, at a Stop either.
	told, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Rename the table?", Project: a.p.Key,
		Questions: []Question{{Question: "Rename?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'answered', decided_at = 1, delivered_at = 2, via = '' WHERE id = ?`, told.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtStop); slices.Contains(got.Requests, told.Code) {
		t.Fatalf("a delivered answer is outstanding: %+v", got)
	}
}

// The newest open round with findings to do is the one named: a newer one
// whose audit hasn't ended is passed over (audit #1372.23).
func TestSessionOutstandingPassesOverARoundStillAuditing(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	round, audit, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map")
	if _, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, session_id, state, created_at, updated_at) SELECT issue_id, kind, session_id, ?, created_at, updated_at FROM work_log WHERE id = ?`, RoundPending, round); err != nil {
		t.Fatal(err)
	}
	got, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if err != nil || got.Audit != audit || !slices.Equal(got.Findings, []string{itoa64(audit) + ".1"}) {
		t.Fatalf("with a newer round still auditing: %+v %v", got, err)
	}
}

// A Stop the person paused, or in a session they left alone, names nothing,
// with a run going and findings to do (audit #1372.19).
func TestPausedOrLeftAloneStopNamesNoOutstandingWork(t *testing.T) {
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp testsum -- make check"}
	for _, how := range []string{"paused", "left alone"} {
		t.Run(how, func(t *testing.T) {
			a, box := auditAPEnv(t)
			handoffStart(t, a)
			if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
				t.Fatal(err)
			}
			a.endAudit(box, "done", "Merge reads a nil map")
			if how == "paused" {
				if _, err := a.s.PauseAgents(a.ctx, a.admin, "any", "quota jumped", true); err != nil {
					t.Fatal(err)
				}
			} else if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET left_alone_at = 1 WHERE id = ?`, a.session); err != nil {
				t.Fatal(err)
			}
			if res := a.stopWith("Waiting.", run); strings.Contains(res.Text, "wp audit mark") {
				t.Errorf("a %s stop named work: %q", how, res.Text)
			}
		})
	}
}

// The lines: one finding in the singular, more than twelve cut short.
func TestOutstandingLines(t *testing.T) {
	one := outstandingLines(&SessionOutstanding{Audit: 7, Findings: []string{"7.1"}})
	// A confirmed finding not filed is one too: it has a verdict, and the line
	// offers filing it (audit #1372.5).
	for _, want := range []string{"- 1 finding still to do in your review round's audit #7 (7.1): `wp audit show 7`", "`wp audit mark ID fixed -m", "`confirmed --issue KEY` for a real one you filed for later", "`rejected -m"} {
		if !strings.Contains(one, want) {
			t.Errorf("one finding lacks %q:\n%s", want, one)
		}
	}
	var many []string
	for i := 1; i <= 13; i++ {
		many = append(many, "7."+itoa64(int64(i)))
	}
	got := outstandingLines(&SessionOutstanding{Audit: 7, Findings: many, Requests: []string{"R-1", "R-2"}})
	// wp request show tells the session of them; ls doesn't (audit #1372.4).
	for _, want := range []string{"13 findings", "7.12, …", "2 answered requests you haven't read (R-1, R-2): `wp request show R-1 R-2`"} {
		if !strings.Contains(got, want) {
			t.Errorf("lines lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "7.13") {
		t.Errorf("more than twelve named:\n%s", got)
	}
	// More answered than the page holds: the count is all of them (audit #1372.7).
	var page []string
	for i := 1; i <= 20; i++ {
		page = append(page, "R-"+itoa64(int64(i)))
	}
	if got := outstandingLines(&SessionOutstanding{Requests: page, RequestsTotal: 25}); !strings.Contains(got, "25 answered requests you haven't read (R-1, ") || !strings.Contains(got, "R-20, …)") {
		t.Errorf("a cut page:\n%s", got)
	}
}

// A finding confirmed and still to do is outstanding; one confirmed and filed
// in an issue (findingFiledSQL) is handed off and isn't (audit #1357.1).
func TestSessionOutstandingNamesConfirmedButNotFiled(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	_, audit, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error", "Dead code")
	var issue int64
	if err := a.s.db.R.QueryRow(`SELECT id FROM issues ORDER BY id LIMIT 1`).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET severity = 'low', state = 'confirmed', issue_id = CASE ordinal WHEN 2 THEN ? END WHERE audit_id = ? AND ordinal IN (1, 2)`, issue, audit); err != nil {
		t.Fatal(err)
	}
	got, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{itoa64(audit) + ".1", itoa64(audit) + ".3"}; !slices.Equal(got.Findings, want) {
		t.Fatalf("findings = %v, want %v", got.Findings, want)
	}
}
