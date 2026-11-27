package core

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A Stop that leaves a background run going names the outstanding work, once;
// a Stop with nothing running, or with nothing outstanding, says nothing. The
// wait hook merely answering doesn't stop the Stop naming it (audit #1344.1).
func TestStopNamesOutstandingWorkWhileARunGoes(t *testing.T) {
	a, box := auditAPEnv(t)
	round, audit, _ := handoffStart(t, a)
	holdRoundOpen(t, a, round)
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp testsum -- make check"}
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
	promptIn(t, a, round)
	if o, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !slices.Equal(o.Findings, []string{f(1), f(2)}) {
		t.Fatalf("at the wait: %+v %v", o, err)
	}
	more := []PendingTask{run, {Type: "local_bash", Status: "running", Description: "run b"}, {Type: "local_bash", Status: "running", Description: "run c"}, {Type: "local_bash", Status: "running", Description: "run d"}}
	res := a.stopWith("Waiting.", more...)
	for _, want := range []string{"Still running in the background:\n- wp testsum -- make check\n- run b\n- run c\n- and 1 more\nWork of yours is waiting; do what doesn't need those results now", "run c", "audit #" + itoa64(audit), f(1), f(2), "wp audit mark"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the stop's text lacks %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "run d") {
		t.Errorf("more than three runs named:\n%s", res.Text)
	}
	if res := a.stopWith("Still waiting.", run); res.Text != "" {
		t.Errorf("a second stop repeated the findings: %q", res.Text)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 3, 'Late finding')`, audit); err != nil {
		t.Fatal(err)
	}
	res = a.stopWith("Waiting more.", run)
	for _, want := range []string{"3 findings still to do", f(1), f(2), f(3)} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("after a new finding the stop lacks %q:\n%s", want, res.Text)
		}
	}
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
	round, _, _ := handoffStart(t, a)
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
		t.Fatal(err)
	}
	a.endAudit(box, "done", "Merge reads a nil map")
	promptIn(t, a, round)
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
	if o, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !o.Empty() {
		t.Fatalf("before the round's prompt went in: %+v %v", o, err)
	}
	promptIn(t, a, round)
	if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET state = 'rejected' WHERE audit_id = ? AND ordinal = 2`, audit); err != nil {
		t.Fatal(err)
	}
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
	if strings.Contains(got.Lines, "What goes wrong") || !strings.Contains(got.Lines, "Merge reads a nil map") {
		t.Fatalf("lines:\n%s", got.Lines)
	}

	if again, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !slices.Equal(again.Findings, want) {
		t.Fatalf("asked again before the print: %+v %v", again, err)
	}
	if err := a.s.SessionFindingsNamed(a.ctx, &ag, want); err != nil {
		t.Fatal(err)
	}
	if again, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || !again.Empty() || again.Lines != "" {
		t.Fatalf("asked twice: %+v %v", again, err)
	}
	if atStop, _ := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtStop); !atStop.Empty() {
		t.Fatalf("at the stop after the wait printed them: %v", atStop.Findings)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 4, 'New one')`, audit); err != nil {
		t.Fatal(err)
	}
	if more, _ := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !slices.Equal(more.Findings, append(want, itoa64(audit)+".4")) || !strings.Contains(more.Lines, "3 findings") {
		t.Fatalf("new finding = %v %q", more.Findings, more.Lines)
	}
	sum, err := a.s.GetAgentSession(a.ctx, &ag, a.session)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Timeline) == 0 {
		t.Fatal("the timeline is empty, so it can't show the record is left out")
	}
	for _, e := range sum.Timeline {
		if e.Kind == "outstanding" {
			t.Fatalf("the timeline lists an outstanding record: %+v", e)
		}
	}

	a.s.setRoundNow(a.ctx, round, []string{RoundPending, RoundRunning}, RoundSkipped, "test")
	if got, err = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || len(got.Findings) != 0 {
		t.Fatalf("after the round ended: %+v %v", got, err)
	}

	req, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Squash the migrations?", Project: a.p.Key,
		Questions: []Question{{Question: "Squash?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); len(got.Requests) != 0 {
		t.Fatalf("an open request is not outstanding: %+v", got)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'answered', decided_at = 1, via = '', answer = 'No, keep them apart.' WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtStop); !got.Empty() {
		t.Fatalf("an answer alone carried a stop on: %+v", got)
	}
	got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if !slices.Equal(got.Requests, []string{req.Code}) || !slices.Equal(got.RequestIDs, []int64{req.ID}) || !strings.Contains(got.Lines, "No, keep them apart.") || strings.Contains(got.Lines, "wp request") {
		t.Fatalf("requests = %v %v %q, want [%s] with its answer", got.Requests, got.RequestIDs, got.Lines, req.Code)
	}
	delivered := func() bool {
		var at sql.NullInt64
		if err := a.s.db.R.QueryRow(`SELECT delivered_at FROM requests WHERE id = ?`, req.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at.Valid
	}
	if delivered() {
		t.Fatal("the answer is marked told before the hook said it gave it")
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !slices.Equal(got.RequestIDs, []int64{req.ID}) {
		t.Fatalf("an answer whose giving wasn't confirmed isn't given again: %+v", got)
	}
	if err := a.s.SessionDecisionsTold(a.ctx, &other, []int64{req.ID}); err != nil || delivered() {
		t.Fatalf("another session marked the answer told: %v", err)
	}
	if err := a.s.SessionDecisionsTold(a.ctx, &ag, []int64{req.ID}); err != nil || !delivered() {
		t.Fatalf("the hook's word doesn't mark the answer told: %v", err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !got.Empty() {
		t.Fatalf("the request named twice: %+v", got)
	}
	open, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Drop the column?", Project: a.p.Key,
		Questions: []Question{{Question: "Drop?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.s.SessionDecisionsTold(a.ctx, &ag, []int64{open.ID}); err != nil {
		t.Fatal(err)
	}
	var openAt sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT delivered_at FROM requests WHERE id = ?`, open.ID).Scan(&openAt); err != nil || openAt.Valid {
		t.Fatalf("an open request was marked told: %v %v", openAt, err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = ? WHERE id = ?`, RequestWithdrawn, open.ID); err != nil {
		t.Fatal(err)
	}
	hush, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Vendor the module?", Project: a.p.Key,
		Questions: []Question{{Question: "Vendor?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = ?, decision = 'dismiss', decided_at = 1, via = '' WHERE id = ?`, RequestDismissed, hush.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); !got.Empty() {
		t.Fatalf("a quiet dismissal was given: %+v", got)
	}
	var hushAt sql.NullInt64
	if err := a.s.db.R.QueryRow(`SELECT delivered_at FROM requests WHERE id = ?`, hush.ID).Scan(&hushAt); err != nil || !hushAt.Valid {
		t.Fatalf("a quiet dismissal was left untold at the wait: %v %v", hushAt, err)
	}
	if o, err := a.s.SessionOutstanding(a.ctx, &other, OutstandingAtStop); err != nil || !o.Empty() {
		t.Fatalf("another session's, with a request: %+v %v", o, err)
	}
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

// promptIn marks the round's prompt typed in (RoundRunning): the findings
// outstanding work names are those of a round the session holds.
func promptIn(t *testing.T, a *apEnv, round int64) {
	t.Helper()
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ? WHERE id = ?`, RoundRunning, round); err != nil {
		t.Fatal(err)
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
	promptIn(t, a, round)
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
			round, _, _ := handoffStart(t, a)
			if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
				t.Fatal(err)
			}
			a.endAudit(box, "done", "Merge reads a nil map")
			promptIn(t, a, round)
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

// The lines name each finding, which the session has in full from its
// round's prompt, and give each answer in full, so the agent needn't run a
// command to read either (WP-1571).
func TestOutstandingLines(t *testing.T) {
	one := outstandingLines(&SessionOutstanding{Audit: 7, todo: []CodeAuditFinding{{ID: "7.1", Severity: "medium", Title: "Merge reads a nil map", Path: "a.go", Line: 3, Problem: "It panics on a fresh map."}}})
	for _, want := range []string{"- 1 finding still to do in your review round's audit #7, in full in the round's prompt", "7.1 [medium] Merge reads a nil map (a.go:3)",
		"`wp audit mark ID fixed -m", "`wp audit mark ID confirmed --issue KEY` for a real one you filed for later", "rejected -m"} {
		if !strings.Contains(one, want) {
			t.Errorf("one finding lacks %q:\n%s", want, one)
		}
	}
	if strings.Contains(one, "wp audit show") || strings.Contains(one, "It panics") {
		t.Errorf("the line sends the agent to read the finding, or gives it again:\n%s", one)
	}
	var many []CodeAuditFinding
	for i := 1; i <= 40; i++ {
		many = append(many, CodeAuditFinding{ID: fmt.Sprintf("7.%d", i), Severity: "low", Title: fmt.Sprintf("Finding %d", i)})
	}
	got := outstandingLines(&SessionOutstanding{Audit: 7, todo: many})
	for _, want := range []string{"40 findings", "7.30 [low] Finding 30", "and 10 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("many findings lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "7.31 ") {
		t.Errorf("more than thirty named:\n%s", got)
	}
	r := &Request{Ref: "WP:R-4", Kind: "question", Title: "Squash the migrations?", State: RequestAnswered, Answer: "No, keep them apart.", DecidedBy: &UserRef{DisplayName: "Sam"}}
	got = outstandingLines(&SessionOutstanding{decisions: []*Request{r}})
	for _, want := range []string{"WP:R-4", "Sam answered:\nNo, keep them apart."} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "wp request") {
		t.Errorf("the answer sends the agent to read it:\n%s", got)
	}
	small2 := *r
	small2.Ref = "WP:R-5"
	huge := *r
	huge.Answer = strings.Repeat("y", 20000)
	if fit := fitDecisions([]*Request{r, &huge, &small2}, 5000); len(fit) != 2 || fit[0] != r || fit[1] != &small2 {
		t.Errorf("fit = %v", fit)
	}
	long := make([]CodeAuditFinding, 30)
	for i := range long {
		long[i] = CodeAuditFinding{ID: fmt.Sprintf("7.%d", i+1), Severity: "low", Title: strings.Repeat("t", 200), Path: strings.Repeat("p/", 60) + "a.go", Line: 3}
	}
	o := &SessionOutstanding{Audit: 7, todo: long}
	o.decisions = fitDecisions([]*Request{r, &small2}, outstandingRoom-len(outstandingLines(o)))
	got = outstandingLines(o)
	if len(got) > outstandingRoom || len(o.decisions) != 2 || !strings.Contains(got, "more.") || !strings.Contains(got, "7.1 [low]") {
		t.Errorf("30 long findings and two answers: %d bytes, %d answers:\n%s", len(got), len(o.decisions), got)
	}
}

// At a Stop with findings to name, the answers it hasn't been told of go
// with them, in full, and are marked told.
func TestStopGivesAnswersWithTheFindings(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	round, _, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map")
	promptIn(t, a, round)
	req, _, err := a.s.CreateRequest(a.ctx, &ag, RequestInput{Kind: "question", Title: "Squash the migrations?", Project: a.p.Key,
		Questions: []Question{{Question: "Squash?", Choices: []Choice{{Label: "yes"}, {Label: "no"}}}}, Followup: "never"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE requests SET state = 'answered', decided_at = 1, via = '', answer = 'No, keep them apart.' WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	got, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtStop)
	if err != nil || len(got.Findings) != 1 || !slices.Equal(got.Requests, []string{req.Code}) || !strings.Contains(got.Lines, "No, keep them apart.") || !strings.Contains(got.Lines, "Merge reads a nil map") {
		t.Fatalf("at the stop: %+v %v", got, err)
	}
}

// A finding confirmed and still to do is outstanding; one confirmed and filed
// in an issue (findingFiledSQL) is handed off and isn't (audit #1357.1).
func TestSessionOutstandingNamesConfirmedButNotFiled(t *testing.T) {
	a, box := auditAPEnv(t)
	ag := *a.ag
	ag.Session = a.session
	round, audit, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error", "Dead code")
	promptIn(t, a, round)
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

// holdRoundOpen runs the check of the round's marks, so a Stop with only a shell
// going isn't the round's report and the round stays running.
func holdRoundOpen(t *testing.T, a *apEnv, round int64) {
	t.Helper()
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, status, request_key) VALUES (?, (SELECT id FROM forges LIMIT 1), 'acme/app', 'branch', ?, ?, 'running', ?)`,
		a.p.ID, a.admin.User.ID, a.clockNow().UnixMilli(), fmt.Sprintf(checkAuditKey, round)); err != nil {
		t.Fatal(err)
	}
}

func stopOutstandingEnv(t *testing.T) (*apEnv, string, int64) {
	t.Helper()
	a, box := auditAPEnv(t)
	round, audit, _ := handoffStart(t, a)
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET warm_state = ''`); err != nil {
		t.Fatal(err)
	}
	a.endAudit(box, "done", "Merge reads a nil map", "Unchecked error")
	promptIn(t, a, round)
	holdRoundOpen(t, a, round)
	return a, itoa64(audit), audit
}

func outstandingRows(t *testing.T, a *apEnv) int {
	t.Helper()
	var n int
	if err := a.s.db.W.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind = ?`, outstandingKind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A running subagent of the session wakes it when it reports, and the
// findings wait on its work: a Stop then names nothing and records nothing.
func TestStopWithASubagentRunningNamesNothing(t *testing.T) {
	a, _, _ := stopOutstandingEnv(t)
	agent := PendingTask{Type: "local_agent", Status: "running", Description: "Fix audit findings"}
	shell := PendingTask{Type: "local_bash", Status: "running", Description: "wp ship"}
	if res := a.stopWith("Waiting.", agent, shell); res.Text != "" {
		t.Fatalf("a subagent runs, yet the stop said %q", res.Text)
	}
	if n := outstandingRows(t, a); n != 0 {
		t.Fatalf("%d outstanding rows written", n)
	}
	done := PendingTask{Type: "local_agent", Status: "completed", Description: "Fix audit findings"}
	if res := a.stopWith("Waiting.", done, shell); !strings.Contains(res.Text, "wp audit mark") {
		t.Fatalf("a finished subagent hid the findings from a stop with a shell running: %q", res.Text)
	}
}

func TestStopTextShape(t *testing.T) {
	a, _, audit := stopOutstandingEnv(t)
	long := "L=/tmp/x/ship9.log wp ship --summary \"Audit 1809 (check of marks on 1798): all 17 objections had one cause. " + strings.Repeat("word ", 30) + "\""
	var tasks []PendingTask
	names := []string{long, "Fix audit 1811 findings", "run c"}
	for i := range 12 {
		names = append(names, fmt.Sprintf("run %d", i))
	}
	for _, d := range names {
		tasks = append(tasks, PendingTask{Type: "local_bash", Status: "running", Description: d})
	}
	res := a.stopWith("Waiting.", tasks...)
	lines := strings.Split(res.Text, "\n")
	if lines[0] != "Still running in the background:" {
		t.Fatalf("header %q", lines[0])
	}
	if want := "- " + excerpt(long, 100); lines[1] != want || !strings.HasSuffix(lines[1], "…") || len([]rune(lines[1])) != 102 {
		t.Errorf("first entry %q", lines[1])
	}
	if lines[2] != "- Fix audit 1811 findings" || lines[3] != "- run c" || lines[4] != "- and 12 more" {
		t.Errorf("entries: %q", lines[2:5])
	}
	if lines[5] != "Work of yours is waiting; do what doesn't need those results now instead of ending the turn idle:" {
		t.Errorf("work line %q", lines[5])
	}
	if !strings.Contains(res.Text, "audit #"+itoa64(audit)) {
		t.Errorf("findings missing:\n%s", res.Text)
	}
}

func TestRunningTasksDedupesAgainstTheCut(t *testing.T) {
	long := strings.Repeat("x", 300)
	p := &HookPending{Tasks: []PendingTask{{Type: "local_bash", Description: long}, {Type: "local_bash", Description: long}}}
	if got := runningTasks(p); len(got) != 1 {
		t.Errorf("one long command listed %d times", len(got))
	}
	// Two commands that differ only past the cut read the same once cut.
	p.Tasks[1].Description = long + " --other"
	if got := runningTasks(p); len(got) != 1 {
		t.Errorf("two commands alike up to the cut listed as %d", len(got))
	}
	p.Tasks[1].Description = "y" + long
	if got := runningTasks(p); len(got) != 2 {
		t.Errorf("two commands that differ before the cut listed as %d", len(got))
	}
}

// A subagent counts while it runs, whatever the case of its type; a finished
// one, a shell and no pending at all don't.
func TestPendingSubagent(t *testing.T) {
	for _, tc := range []struct {
		p    *HookPending
		want bool
	}{
		{nil, false},
		{&HookPending{}, false},
		{&HookPending{Tasks: []PendingTask{{Type: "local_agent", Status: "running"}}}, true},
		{&HookPending{Tasks: []PendingTask{{Type: "Local_Agent", Status: "running"}}}, true},
		{&HookPending{Tasks: []PendingTask{{Type: "local_agent", Status: "completed"}}}, false},
		{&HookPending{Tasks: []PendingTask{{Type: "local_bash", Status: "running"}}}, false},
		{&HookPending{Tasks: []PendingTask{{Type: "local_bash", Status: "running"}, {Type: "local_agent", Status: "pending"}}}, true},
	} {
		if got := pendingSubagent(tc.p); got != tc.want {
			t.Errorf("pendingSubagent(%+v) = %v", tc.p, got)
		}
	}
}

// An answer to the wait the hook never says it printed leaves the findings
// for the Stop to name.
func TestStopNamesFindingsTheWaitAnsweredButDidntPrint(t *testing.T) {
	a, _, _ := stopOutstandingEnv(t)
	ag := *a.ag
	ag.Session = a.session
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp ship"}
	o, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait)
	if err != nil || len(o.Findings) != 2 {
		t.Fatalf("at the wait: %+v %v", o, err)
	}
	if res := a.stopWith("Waiting.", run); !strings.Contains(res.Text, "wp audit mark") {
		t.Fatalf("answered but not printed, the stop must still name them: %q", res.Text)
	}
}

// Findings the wait hook printed (and said so) are not fresh at a Stop; a
// new one makes the Stop name them all, and the next wait still names what
// only a Stop did.
func TestStopDoesntRepeatFindingsTheWaitPrinted(t *testing.T) {
	a, _, audit := stopOutstandingEnv(t)
	ag := *a.ag
	ag.Session = a.session
	run := PendingTask{Type: "local_bash", Status: "running", Description: "wp ship"}
	if err := a.s.SessionFindingsNamed(a.ctx, &ag, []string{itoa64(audit) + ".1", itoa64(audit) + ".2"}); err != nil {
		t.Fatal(err)
	}
	if res := a.stopWith("Waiting.", run); res.Text != "" {
		t.Fatalf("named at the wait, yet the stop said %q", res.Text)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title) VALUES (?, 3, 'Late finding')`, audit); err != nil {
		t.Fatal(err)
	}
	res := a.stopWith("Waiting.", run)
	for _, want := range []string{"3 findings still to do", itoa64(audit) + ".1", itoa64(audit) + ".3"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("after a new finding the stop lacks %q:\n%s", want, res.Text)
		}
	}
	if o, err := a.s.SessionOutstanding(a.ctx, &ag, OutstandingAtWait); err != nil || len(o.Findings) != 3 {
		t.Errorf("the wait after a stop named the late finding: %+v %v", o, err)
	}
}
