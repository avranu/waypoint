package core

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Audit 1218.2 ruled that no agent reads raw transcripts; WP-1587 replaced it
// on the owner's decision (an agent in the person's shell or session must be
// able to run `wp usage transcript` and `wp usage facts`). What guards it now
// (audit 1907.4): the transcripts.read permission for a token, which no
// preset grants; an automation never; another person's stay person-only.
func TestTranscriptsNeedTheirOwnPermission(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	kim := e.user("kim", false)
	l := `{"cmd":"x"}` + "\n"
	e.put(jo, piece("s1", "main", 0, l), l)
	e.put(kim, piece("s1", "main", 0, l), l)
	read := func(a *Actor, user string) (string, error) {
		var b bytes.Buffer
		err := e.s.ReadTranscript(e.ctx, a, TranscriptQuery{Session: "s1", File: "main", User: user}, &b)
		return b.String(), err
	}
	token := func(ps ...Permission) *Actor {
		c := agentActor(jo, "anthropic/claude-code")
		c.Permissions = permissionSet(ps)
		return c
	}
	refused := func(name string, a *Actor) {
		t.Helper()
		got, err := read(a, "")
		var pe *PermissionError
		if !errors.Is(err, ErrForbidden) || got != "" || !errors.As(err, &pe) || pe.Permission != PermTranscriptsRead ||
			!strings.Contains(err.Error(), "transcripts.read") {
			t.Errorf("%s reading: %q %v", name, got, err)
		}
		if _, err := e.s.TranscriptFiles(e.ctx, a, TranscriptQuery{Session: "s1"}); !errors.As(err, &pe) {
			t.Errorf("%s listing: %v", name, err)
		}
	}
	refused("usage.read only", token(PermUsageRead))
	refused("read only preset", token(permissionPresets[1].Permissions...))

	ok := token(PermUsageRead, PermTranscriptsRead)
	if got, err := read(ok, ""); err != nil || got != l {
		t.Errorf("token with transcripts.read, own person's: %q %v", got, err)
	}
	if got, err := read(ok, "kim"); !errors.Is(err, ErrForbidden) || got != "" {
		t.Errorf("token with transcripts.read, another person's: %q %v", got, err)
	}
	// The person's unrestricted login reads it in an agent's shell and session.
	shell, sess := *jo, *jo
	shell.AgentEnv, sess.Session = "claude", 7
	for name, a := range map[string]*Actor{"agent env": &shell, "session": &sess, "unrestricted agent": agentActor(jo, "anthropic/claude-code")} {
		if got, err := read(a, ""); err != nil || got != l {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	// An automation never reads them, own person's or not.
	auto := *jo
	auto.Automation = &Automation{}
	autoTok := *ok
	autoTok.Automation = &Automation{}
	for name, a := range map[string]*Actor{"automation": &auto, "automation with the permission": &autoTok} {
		got, err := read(a, "")
		if !errors.Is(err, ErrForbidden) || got != "" || !strings.Contains(err.Error(), "automation") {
			t.Errorf("%s: %q %v", name, got, err)
		}
		if _, err := e.s.TranscriptFiles(e.ctx, a, TranscriptQuery{Session: "s1"}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s listing: %v", name, err)
		}
	}
}

// Session facts hold command heads (the Repeated list's targets), so they need
// the same permission and are never an automation's.
func TestFactsNeedTranscriptsPermission(t *testing.T) {
	e, jo := factsEnv(t)
	token := func(ps ...Permission) *Actor {
		c := agentActor(jo, "anthropic/claude-code")
		c.Permissions = permissionSet(ps)
		return c
	}
	for name, a := range map[string]*Actor{"usage.read only": token(PermUsageRead), "read preset": token(permissionPresets[1].Permissions...)} {
		f, err := e.s.SessionFacts(e.ctx, a, FactsQuery{Session: "s1"})
		var pe *PermissionError
		if !errors.As(err, &pe) || pe.Permission != PermTranscriptsRead || f != nil {
			t.Errorf("%s: %v %v", name, err, f)
		}
	}
	if f, err := e.s.SessionFacts(e.ctx, token(PermUsageRead, PermTranscriptsRead), FactsQuery{Session: "s1"}); err != nil || f.ModelCalls.Total != 4 {
		t.Errorf("with the permission: %v", err)
	}
	auto := *jo
	auto.Automation = &Automation{}
	if f, err := e.s.SessionFacts(e.ctx, &auto, FactsQuery{Session: "s1"}); !errors.Is(err, ErrForbidden) || f != nil || !strings.Contains(err.Error(), "automation") {
		t.Errorf("automation: %v %v", err, f)
	}
}
