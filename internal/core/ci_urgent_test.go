package core

import (
	"slices"
	"strings"
	"testing"
)

// A task tagged ci-urgent passes the label to its new pull request, and says
// so; a label taken off later stays off (WP-697).
func TestTaskTagMakesPRUrgent(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	tagged := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix CI", Labels: []string{"CI-Urgent"}})
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Tidy", Labels: []string{"backend"}})

	res, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "fix-ci", Title: "Fix CI", Issue: tagged.Key})
	if err != nil {
		t.Fatal(err)
	}
	created, ops, _, _ := g.snapshot()
	if len(created) != 1 || !slices.Equal(created[0].Labels, []string{CIUrgentLabel}) || len(ops) != 0 {
		t.Errorf("first run isn't on the lane: created %+v label ops %v", created, ops)
	}
	if !res.Pull.Urgent || !strings.Contains(res.Notice, tagged.Key+" is tagged ci-urgent") || !strings.Contains(res.Notice, "--off") {
		t.Errorf("tagged task: urgent=%v notice %q", res.Pull.Urgent, res.Notice)
	}

	if _, err := e.s.SetPRUrgent(e.ctx, agent, res.Pull.ID, false); err != nil {
		t.Fatal(err)
	}
	again, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "fix-ci", Issue: tagged.Key})
	if err != nil || !again.Existing || again.Pull.Urgent || again.Notice != "" {
		t.Errorf("opened again after --off: %+v %v", again, err)
	}

	viaFor, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "fix-ci-2", Title: "Again", For: tagged.Key})
	if err != nil || !viaFor.Pull.Urgent {
		t.Errorf("tagged task named by For: %+v %v", viaFor, err)
	}
	other, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "tidy", Title: "Tidy", Issue: plain.Key})
	if err != nil || other.Pull.Urgent || other.Notice != "" {
		t.Errorf("task without the tag: %+v %v", other, err)
	}
}

// A pull request opened on the forge gets the label when its own text links
// it to a tagged task; one from outside, or naming another task, doesn't
// (WP-697).
func TestForgePRLinkedToTaggedTask(t *testing.T) {
	pe := newShipEnv(t, "github")
	g := pe.sg
	tagged := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Fix CI", Labels: []string{CIUrgentLabel}})
	plain := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Tidy"})

	pe.pullHook("opened", ForgePR{Number: 5, Title: "Fix CI", Author: "robin", HeadRef: strings.ToLower(tagged.Key) + "-fix", HeadSHA: "aaaa1111", State: "open"})
	pe.pullHook("opened", ForgePR{Number: 6, Title: "Tidy", Author: "robin", HeadRef: strings.ToLower(plain.Key) + "-tidy", HeadSHA: "bbbb2222", State: "open"})
	pe.pullHook("opened", ForgePR{Number: 7, Title: "Me first", Body: "Closes " + tagged.Key, Author: "mallory", HeadRef: strings.ToLower(tagged.Key) + "-me", HeadSHA: "dddd4444", State: "open", HeadRepo: "mallory/app"})
	pe.s.Wait()
	if !pe.pr(7).Outside {
		t.Fatal("number 7 should be from outside")
	}

	if _, ops, _, _ := g.snapshot(); !slices.Equal(ops, []string{"+" + CIUrgentLabel}) {
		t.Fatalf("label ops = %v, want one for the tagged task's pull request", ops)
	}
	pr, err := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 5)
	if err != nil || !pr.urgent() {
		t.Fatalf("the tagged task's pull request: %+v %v", pr, err)
	}

	if err := pe.s.labelPRUrgent(pe.ctx, SystemActor(), pr, false); err != nil {
		t.Fatal(err)
	}
	pe.pullHook("synchronize", ForgePR{Number: 5, Title: "Fix CI", Author: "robin", HeadRef: strings.ToLower(tagged.Key) + "-fix", HeadSHA: "cccc3333", State: "open"})
	pe.s.Wait()
	if _, ops, _, _ := g.snapshot(); !slices.Equal(ops, []string{"+" + CIUrgentLabel, "-" + CIUrgentLabel}) {
		t.Errorf("label ops after a push = %v", ops)
	}

	pe.s.urgentFromLink(pe.ctx, withHint(&Event{IssueKey: tagged.Key}, &pullRef{forgeID: pe.f.ID, repo: "me/app", number: 99}))
	if _, ops, _, _ := g.snapshot(); !slices.Equal(ops, []string{"+" + CIUrgentLabel, "-" + CIUrgentLabel}) {
		t.Errorf("label ops after a link to a missing pull request = %v", ops)
	}
	if pr, err := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 5); err != nil || pr.urgent() {
		t.Errorf("the labelled-off pull request changed: %+v %v", pr, err)
	}
}

func TestPRUrgent(t *testing.T) {
	e := newEnv(t)
	g := newShipForge()
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}

	res, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "fix-ci", Title: "Fix CI", Urgent: true})
	if err != nil {
		t.Fatal(err)
	}
	created, ops, _, _ := g.snapshot()
	if len(created) != 1 || !slices.Equal(created[0].Labels, []string{CIUrgentLabel}) || len(ops) != 0 {
		t.Errorf("opened urgent: created %+v label ops %v", created, ops)
	}
	if !res.Pull.Urgent || res.Warning != "" {
		t.Errorf("opened urgent: urgent=%v warning %q", res.Pull.Urgent, res.Warning)
	}

	d, err := e.s.SetPRUrgent(e.ctx, agent, res.Pull.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Urgent {
		t.Error("still urgent after --off")
	}
	if _, err := e.s.SetPRUrgent(e.ctx, agent, res.Pull.ID, false); err != nil {
		t.Fatal(err)
	}
	if d, err = e.s.SetPRUrgent(e.ctx, agent, res.Pull.ID, true); err != nil || !d.Urgent {
		t.Fatalf("on again: %+v %v", d, err)
	}
	if _, ops, _, _ = g.snapshot(); !slices.Equal(ops, []string{"-" + CIUrgentLabel, "+" + CIUrgentLabel}) {
		t.Errorf("label ops = %v (an unchanged state calls nothing)", ops)
	}

	plain, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "feature", Title: "Feature"})
	if err != nil || plain.Pull.Urgent {
		t.Fatalf("plain open: %+v %v", plain, err)
	}
	again, err := e.s.OpenPullRequest(e.ctx, agent, OpenPRInput{Project: p.Key, Head: "feature", Urgent: true})
	if err != nil || !again.Existing || !again.Pull.Urgent {
		t.Errorf("existing made urgent: %+v %v", again, err)
	}

	other := &Actor{User: e.admin.User, Agent: "openai/gpt-6"}
	if _, err := e.s.SetPRUrgent(e.ctx, other, res.Pull.ID, false); err == nil {
		t.Error("another agent took the label off")
	}
	if _, err := e.s.SetPRUrgent(e.ctx, e.admin, res.Pull.ID, false); err != nil {
		t.Errorf("a person took the label off: %v", err)
	}
	if _, err := e.s.SetPRUrgent(e.ctx, e.admin, res.Pull.ID, true); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'pr.urgent'`).Scan(&n); err != nil || n < 3 {
		t.Errorf("audit entries = %d %v", n, err)
	}
}
