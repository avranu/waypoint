package core

import (
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core/email/emailtest"
)

func TestEmailCatalogAndAction(t *testing.T) {
	e := newEnv(t)
	hasEmail := func() bool {
		for _, a := range e.s.AutomationCatalog(e.ctx, e.admin).Actions {
			if a.Name == "email" {
				return true
			}
		}
		return false
	}
	if hasEmail() {
		t.Error("the editor offers email before it is set up")
	}
	p := e.project(e.admin, "Mail", "")
	a := e.addAuto(p, "Mailer", Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		{Type: "email", User: "admins", Title: "Hello", Message: "Line one\nLine two é", Extra: map[string]any{"to": "ops@example.com, robin@example.com"}},
	}})
	r := e.run(e.manual(a, nil).ID)
	if r.Status != RunFailed || !strings.Contains(r.Error, "Admin → Email") {
		t.Errorf("unconfigured run: %s %q", r.Status, r.Error)
	}

	srv := emailtest.NewSMTP(t)
	if _, err := e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "127.0.0.1", Port: srv.Port(), Security: SMTPNone, From: "Waypoint <wp@example.com>"}); err != nil {
		t.Fatal(err)
	}
	if !hasEmail() {
		t.Error("the editor doesn't offer email once it is set up")
	}
	r = e.run(e.manual(a, nil).ID)
	e.s.Wait()
	if r.Status == RunFailed {
		t.Fatalf("run failed: %q", r.Error)
	}
	got := srv.Received()
	if len(got) != 2 {
		t.Fatalf("sent %d emails: %+v", len(got), got)
	}
	to := map[string]string{}
	for _, m := range got {
		if m.From != "wp@example.com" || len(m.To) != 1 {
			t.Errorf("envelope: %+v", m)
		}
		to[m.To[0]] = m.Data
	}
	body, ok := to["robin@example.com"]
	if !ok || to["ops@example.com"] == "" {
		t.Fatalf("recipients: %v", to)
	}
	for _, want := range []string{"Subject: Hello\r\n", "To: robin@example.com\r\n", "Content-Type: text/plain; charset=utf-8", "Line two =C3=A9"} {
		if !strings.Contains(body, want) {
			t.Errorf("message lacks %q:\n%s", want, body)
		}
	}

	for _, act := range []Action{{Type: "email", Message: "x"}, {Type: "email", Extra: map[string]any{"to": "nope"}}} {
		if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: ptr("Bad " + strconv.Itoa(len(act.Extra))), Rule: &Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{act}}}); err == nil {
			t.Errorf("saved %+v", act)
		}
	}
}
