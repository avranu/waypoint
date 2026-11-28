package email

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/email/emailtest"
	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/core/kernel/kerneltest"
)

func TestEmailSettings(t *testing.T) {
	e := kerneltest.New(t)
	svc := New(NewMailer(e.K))
	admin := e.User("robin", true)
	es, err := svc.GetEmailSettings(e.Ctx, admin)
	if err != nil || es.Configured || es.Port != 587 || es.Security != StartTLS {
		t.Fatalf("default: %+v %v", es, err)
	}
	if _, err := svc.GetEmailSettings(e.Ctx, e.User("sam", false)); err == nil {
		t.Error("a non-admin read the settings")
	}
	if _, err := svc.SetEmailSettings(e.Ctx, &kernel.Actor{User: admin.User, Agent: "anthropic/claude-code"}, SettingsInput{Host: "smtp.example.com", From: "a@example.com"}); err == nil {
		t.Error("an agent set up email")
	}
	for _, bad := range []SettingsInput{
		{Host: "smtp.example.com:587", From: "a@example.com"},
		{Host: "smtp.example.com", From: "not an address"},
		{Host: "smtp.example.com"},
		{Host: "smtp.example.com", From: "a@example.com", Security: "ssl"},
		{Host: "smtp.example.com", From: "a@example.com", Port: 70000},
		{Host: "smtp.example.com", From: "a@example.com", Password: "x\r\nRCPT TO:<b@example.com>"},
	} {
		if _, err := svc.SetEmailSettings(e.Ctx, admin, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	es, err = svc.SetEmailSettings(e.Ctx, admin, SettingsInput{Host: "smtp.example.com", Security: "tls", Username: "wp", Password: "s3cret", From: "Waypoint <wp@example.com>"})
	if err != nil || !es.Configured || es.Port != 465 || !es.HasPassword || es.Password != "" || es.From != "Waypoint <wp@example.com>" {
		t.Fatalf("set: %+v %v", es, err)
	}
	if es, _ = svc.SetEmailSettings(e.Ctx, admin, SettingsInput{Host: "smtp.example.com", Port: 2525, Username: "wp", From: "wp@example.com"}); !es.HasPassword || es.Port != 2525 {
		t.Errorf("kept password: %+v", es)
	}
	if es, _ = svc.SetEmailSettings(e.Ctx, admin, SettingsInput{Host: "smtp.example.com", From: "wp@example.com", ClearPassword: true}); es.HasPassword {
		t.Errorf("cleared password: %+v", es)
	}
	if !svc.m.Configured(e.Ctx) {
		t.Error("not configured")
	}
	var audited int
	if err := e.K.DB.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'settings.email' AND user_id = ?`, admin.User.ID).Scan(&audited); err != nil || audited != 3 {
		t.Errorf("audit rows after three sets = %d, %v", audited, err)
	}
	if es, _ = svc.SetEmailSettings(e.Ctx, admin, SettingsInput{}); es.Configured || svc.m.Configured(e.Ctx) {
		t.Errorf("turned off: %+v", es)
	}
}

func TestSendTestEmail(t *testing.T) {
	e := kerneltest.New(t)
	svc := New(NewMailer(e.K))
	admin := e.User("robin", true)
	if err := svc.SendTestEmail(e.Ctx, admin, ""); err == nil {
		t.Error("a test without settings")
	}
	srv := emailtest.NewSMTP(t)
	if _, err := svc.SetEmailSettings(e.Ctx, admin, SettingsInput{Host: "127.0.0.1", Port: srv.Port(), Security: None, From: "wp@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendTestEmail(e.Ctx, admin, ""); err != nil {
		t.Fatal(err)
	}
	if got := srv.Received(); len(got) != 1 || got[0].To[0] != "robin@example.com" {
		t.Errorf("test email: %+v", got)
	}
	if err := svc.SendTestEmail(e.Ctx, e.User("sam", false), ""); err == nil {
		t.Error("a non-admin sent a test")
	}
}

func TestEmailMessageHeaders(t *testing.T) {
	m, err := emailMessage("wp@example.com", "a@example.com", "Line\r\nBcc: x@example.com", "hi", "https://wp.test/issue/A-1", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if strings.Contains(s, "\r\nBcc:") || !strings.Contains(s, "Subject: Line Bcc: x@example.com\r\n") || !strings.Contains(s, "https://wp.test/issue/A-1") {
		t.Errorf("message:\n%s", s)
	}
	if _, err := emailMessage("wp@example.com", "a@example.com\r\nBcc: x@example.com", "s", "b", "", time.Now()); err == nil {
		t.Error("a recipient with a line break")
	}
}

func TestSendTestEmailToAGivenRecipientAndAsAnAdminAgent(t *testing.T) {
	e := kerneltest.New(t)
	svc := New(NewMailer(e.K))
	admin := e.User("robin", true)
	srv := emailtest.NewSMTP(t)
	if _, err := svc.SetEmailSettings(e.Ctx, admin, SettingsInput{Host: "127.0.0.1", Port: srv.Port(), Security: None, From: "wp@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendTestEmail(e.Ctx, admin, " other@example.com "); err != nil {
		t.Fatal(err)
	}
	if got := srv.Received(); len(got) != 1 || got[0].To[0] != "other@example.com" {
		t.Errorf("explicit recipient: %+v", got)
	}
	// Only changing settings needs a person: an admin's agent may send a test.
	agent := &kernel.Actor{User: admin.User, Agent: "anthropic/claude-code"}
	if err := svc.SendTestEmail(e.Ctx, agent, ""); err != nil {
		t.Errorf("an admin agent's test: %v", err)
	}
	if got := srv.Received(); len(got) != 2 || got[1].To[0] != "robin@example.com" {
		t.Errorf("agent's test email: %+v", got)
	}
}

func TestConfiguredIsFalseWhenTheReadFails(t *testing.T) {
	e := kerneltest.New(t)
	m := NewMailer(e.K)
	if err := e.K.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Configured(e.Ctx) {
		t.Error("configured with the database closed")
	}
}
