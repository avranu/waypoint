package core

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a mail server that takes every message, without TLS or login.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []fakeMail
}

type fakeMail struct {
	from string
	to   []string
	data string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake")
	var m fakeMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			m = fakeMail{from: strings.Trim(strings.TrimSpace(line)[10:], "<>")}
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			m.to = append(m.to, strings.Trim(strings.TrimSpace(line)[8:], "<>"))
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.data = b.String()
			f.mu.Lock()
			f.mail = append(f.mail, m)
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) received() []fakeMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMail(nil), f.mail...)
}

func TestEmailSettings(t *testing.T) {
	e := newEnv(t)
	es, err := e.s.GetEmailSettings(e.ctx, e.admin)
	if err != nil || es.Configured || es.Port != 587 || es.Security != SMTPStartTLS {
		t.Fatalf("default: %+v %v", es, err)
	}
	if _, err := e.s.GetEmailSettings(e.ctx, e.user("sam", false)); err == nil {
		t.Error("a non-admin read the settings")
	}
	if _, err := e.s.SetEmailSettings(e.ctx, &Actor{User: e.admin.User, Agent: "anthropic/claude-code"}, EmailSettingsInput{Host: "smtp.example.com", From: "a@example.com"}); err == nil {
		t.Error("an agent set up email")
	}
	for _, bad := range []EmailSettingsInput{
		{Host: "smtp.example.com:587", From: "a@example.com"},
		{Host: "smtp.example.com", From: "not an address"},
		{Host: "smtp.example.com"},
		{Host: "smtp.example.com", From: "a@example.com", Security: "ssl"},
		{Host: "smtp.example.com", From: "a@example.com", Port: 70000},
		{Host: "smtp.example.com", From: "a@example.com", Password: "x\r\nRCPT TO:<b@example.com>"},
	} {
		if _, err := e.s.SetEmailSettings(e.ctx, e.admin, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	es, err = e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "smtp.example.com", Security: "tls", Username: "wp", Password: "s3cret", From: "Waypoint <wp@example.com>"})
	if err != nil || !es.Configured || es.Port != 465 || !es.HasPassword || es.Password != "" || es.From != "Waypoint <wp@example.com>" {
		t.Fatalf("set: %+v %v", es, err)
	}
	// An empty password keeps the one set; clearing removes it.
	if es, _ = e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "smtp.example.com", Port: 2525, Username: "wp", From: "wp@example.com"}); !es.HasPassword || es.Port != 2525 {
		t.Errorf("kept password: %+v", es)
	}
	if es, _ = e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "smtp.example.com", From: "wp@example.com", ClearPassword: true}); es.HasPassword {
		t.Errorf("cleared password: %+v", es)
	}
	if !e.s.EmailConfigured(e.ctx) {
		t.Error("not configured")
	}
	if es, _ = e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{}); es.Configured || e.s.EmailConfigured(e.ctx) {
		t.Errorf("turned off: %+v", es)
	}
}

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

	srv := newFakeSMTP(t)
	if _, err := e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "127.0.0.1", Port: srv.port(), Security: SMTPNone, From: "Waypoint <wp@example.com>"}); err != nil {
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
	got := srv.received()
	// robin is both an admin and in "to": one email each, to two addresses.
	if len(got) != 2 {
		t.Fatalf("sent %d emails: %+v", len(got), got)
	}
	to := map[string]string{}
	for _, m := range got {
		if m.from != "wp@example.com" || len(m.to) != 1 {
			t.Errorf("envelope: %+v", m)
		}
		to[m.to[0]] = m.data
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

	// Saving a rule with nobody to email, or a bad address, is refused.
	for _, act := range []Action{{Type: "email", Message: "x"}, {Type: "email", Extra: map[string]any{"to": "nope"}}} {
		if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: ptr("Bad " + strconv.Itoa(len(act.Extra))), Rule: &Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{act}}}); err == nil {
			t.Errorf("saved %+v", act)
		}
	}
}

func TestSendTestEmail(t *testing.T) {
	e := newEnv(t)
	if err := e.s.SendTestEmail(e.ctx, e.admin, ""); err == nil {
		t.Error("a test without settings")
	}
	srv := newFakeSMTP(t)
	if _, err := e.s.SetEmailSettings(e.ctx, e.admin, EmailSettingsInput{Host: "127.0.0.1", Port: srv.port(), Security: SMTPNone, From: "wp@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SendTestEmail(e.ctx, e.admin, ""); err != nil {
		t.Fatal(err)
	}
	if got := srv.received(); len(got) != 1 || got[0].to[0] != "robin@example.com" {
		t.Errorf("test email: %+v", got)
	}
	if err := e.s.SendTestEmail(e.ctx, e.user("sam", false), ""); err == nil {
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
