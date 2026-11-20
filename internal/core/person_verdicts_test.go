package core

import (
	"errors"
	"testing"
)

// A person's verdicts on agents' work (deciding or snoozing a request,
// running its command, reviewing what agents wrote or did, reviewing task
// summaries, locking glossary terms, approving a sign-in) need the person
// directly: not an agent's token, and not the person's token from an agent's
// shell, as an agent there can say it is a person (WAYPOINT_AGENT=none)
// (WP-409, WP-ADR96).
func TestPersonVerdictsRefuseAgentShells(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	shell := *r.ana
	shell.AgentEnv = "anthropic/claude-code"
	p := r.project(r.admin, "Gamma", "")
	g, err := r.s.CreateGlossary(r.ctx, r.admin, GlossaryInput{Term: str("Lease"), Definition: str("A claim."), Project: str(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	adminShell := *r.admin
	adminShell.AgentEnv = "anthropic/claude-code"
	login, err := r.s.StartDeviceLogin(r.ctx, "box", "", "198.51.100.7", "wp/test")
	if err != nil {
		t.Fatal(err)
	}

	for _, who := range []struct {
		name string
		ana  *Actor
		adm  *Actor
	}{
		{"agent token", r.agent, agentActor(r.admin, "anthropic/claude-code")},
		{"person's token in an agent's shell", &shell, &adminShell},
	} {
		forbid := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("%s, %s: err = %v, want forbidden", who.name, what, err)
			}
		}
		_, err := r.s.DecideRequest(r.ctx, who.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}})
		forbid("deciding a request", err)
		_, err = r.s.SnoozeRequest(r.ctx, who.ana, q.Ref, nil)
		forbid("snoozing a request", err)
		_, err = r.s.RunCell(r.ctx, who.ana, q.Ref, 1, "true")
		forbid("running a request's command", err)
		_, err = r.s.Review(r.ctx, who.adm, ReviewIssue, "WP-1", "")
		forbid("reviewing what agents wrote", err)
		_, err = r.s.ReviewCloses(r.ctx, who.adm, []string{"WP-1"})
		forbid("reviewing what agents did", err)
		_, err = r.s.ReviewTaskSummaries(r.ctx, who.ana, nil, true, "", false)
		forbid("reviewing task summaries", err)
		_, err = r.s.UpdateGlossary(r.ctx, who.adm, g.ID, GlossaryInput{Locked: ptr(true)})
		forbid("locking a glossary entry", err)
		_, err = r.s.CreateGlossary(r.ctx, who.adm, GlossaryInput{Term: str("Claim"), Definition: str("x"), Project: str(p.Key), Locked: ptr(true)})
		forbid("creating a locked glossary entry", err)
		forbid("approving a sign-in", r.s.ApproveDeviceLogin(r.ctx, who.adm, login.UserCode))
	}

	// The person, directly, still can.
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}}); err != nil {
		t.Errorf("the person deciding: %v", err)
	}
	if _, err := r.s.UpdateGlossary(r.ctx, r.admin, g.ID, GlossaryInput{Locked: ptr(true)}); err != nil {
		t.Errorf("the person locking: %v", err)
	}
	if err := r.s.ApproveDeviceLogin(r.ctx, r.admin, login.UserCode); err != nil {
		t.Errorf("the person approving a sign-in: %v", err)
	}
}

// An agent's design revision is never recorded as approved, whatever path
// gets there: only a person's verdict approves (WP-409, WP-ADR96).
func TestAgentRevisionNeverApproved(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	n, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Idea"), Body: str("x"), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	err = e.s.write(e.ctx, agent, func(t *tx) error {
		return e.s.addRevision(e.ctx, t, n.ID, "addition", "", "sneaky", nil, RevApproved)
	})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("agent's approved revision: err = %v, want forbidden", err)
	}
}

// A person's token in an agent's shell writes like an agent: its note is a
// proposal, its edit to an approved note waits for a person, and nobody is
// recorded as approving either; the person's own write still approves
// itself (WP-409, WP-ADR96).
func TestAgentShellDesignWritesDontApprove(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "")
	shell := *e.admin
	shell.AgentEnv = "anthropic/claude-code"
	n, err := e.s.CreateDesign(e.ctx, &shell, DesignInput{Title: str("Idea"), Body: str("x"), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if n.Approved || n.ApprovedBy != nil || n.CreatedAgent != "agent-shell" || n.CanApprove {
		t.Errorf("shell note: approved=%v by=%v agent=%q can_approve=%v, want a proposal by agent-shell", n.Approved, n.ApprovedBy, n.CreatedAgent, n.CanApprove)
	}
	mine, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Mine"), Body: str("y"), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if !mine.Approved || mine.ApprovedBy == nil {
		t.Fatalf("the person's own note: approved=%v by=%v", mine.Approved, mine.ApprovedBy)
	}
	edited, err := e.s.UpdateDesign(e.ctx, &shell, mine.ID, DesignInput{Body: str("changed")})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Body != "y" {
		t.Errorf("shell edit to an approved note applied: body %q", edited.Body)
	}
	added, err := e.s.AddDesignAddition(e.ctx, &shell, mine.ID, "more")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range added.Additions {
		if a.State != RevPending {
			t.Errorf("shell addition state %q, want pending", a.State)
		}
	}
}
