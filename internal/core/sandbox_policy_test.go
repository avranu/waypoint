package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// browser signs a person in and returns them as the web app's actor.
func (e *env) browser(a *Actor) *Actor {
	e.t.Helper()
	tok, _, err := e.s.Login(e.ctx, a.User.Username, "correct-horse-battery", "test", "127.0.0.1")
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := e.s.SessionActor(e.ctx, tok)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// Only a person in the browser changes a project's sandbox; loosening it
// needs a password from the last ten minutes and a reason; every change is
// logged, notifies, and can be reverted (WP-418, WP-ADR28 decision 3).
func TestProjectSandboxPolicy(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	owner, viewer := e.user("olive", false), e.user("vera", false)
	for u, role := range map[*Actor]string{owner: "owner", viewer: "viewer"} {
		if err := e.s.SetMember(e.ctx, e.admin, p.Key, u.User.Username, role); err != nil {
			t.Fatal(err)
		}
	}
	web := e.browser(owner)

	// Not a token (the person's own included), MCP, an agent or an automation.
	tok, _, err := e.s.CreateAPIToken(e.ctx, owner, "cli", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	tokActor, err := e.s.TokenActor(e.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	mcp := &Actor{User: owner.User, MCP: true}
	auto := &Actor{User: owner.User, Automation: &Automation{ID: 1}}
	agentInBrowser := *web
	agentInBrowser.Agent = "anthropic/claude-code"
	for name, a := range map[string]*Actor{"token": tokActor, "mcp": mcp, "automation": auto, "agent": &agentInBrowser, "no session": owner} {
		if _, err := e.s.SetProjectSandbox(e.ctx, a, p.Key, SandboxPolicyInput{Level: ptr(SandboxVM)}); !errors.Is(err, ErrSandboxUI) {
			t.Errorf("%s set the sandbox: %v", name, err)
		}
	}
	// A viewer in the browser may read but not change it.
	vweb := e.browser(viewer)
	if pol, err := e.s.ProjectSandbox(e.ctx, vweb, p.Key); err != nil || pol.CanEdit {
		t.Errorf("viewer read = %+v, %v", pol, err)
	}
	if _, err := e.s.SetProjectSandbox(e.ctx, vweb, p.Key, SandboxPolicyInput{Level: ptr(SandboxVM)}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer set the sandbox: %v", err)
	}
	// A token reads it.
	if pol, err := e.s.ProjectSandbox(e.ctx, tokActor, p.Key); err != nil || pol.CanEdit || pol.Level != "" {
		t.Errorf("token read = %+v, %v", pol, err)
	}

	// Signing in counts as entering the password: for ten minutes.
	e.setClock(e.clockNow().Add(11 * time.Minute))
	// Tightening needs no password: the default may be anything, but VM is
	// the strongest.
	pol, err := e.s.SetProjectSandbox(e.ctx, web, p.Key, SandboxPolicyInput{Level: ptr(SandboxVM)})
	if err != nil || pol.Level != SandboxVM || !pol.CanEdit || pol.UpdatedBy == nil || pol.UpdatedBy.ID != owner.User.ID {
		t.Fatalf("tighten = %+v, %v", pol, err)
	}
	// Loosening does, and a reason.
	if _, err := e.s.SetProjectSandbox(e.ctx, web, p.Key, SandboxPolicyInput{Level: ptr(SandboxContained), Reason: "x"}); !errors.Is(err, ErrReauth) {
		t.Fatalf("loosened without a password: %v", err)
	}
	if _, err := e.s.Reauth(e.ctx, web, "wrong"); err == nil {
		t.Fatal("a wrong password re-authenticated")
	}
	if _, err := e.s.Reauth(e.ctx, tokActor, "correct-horse-battery"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a token re-authenticated: %v", err)
	}
	until, err := e.s.Reauth(e.ctx, web, "correct-horse-battery")
	if err != nil || !until.Equal(e.clockNow().Add(10*time.Minute)) {
		t.Fatalf("reauth = %v, %v", until, err)
	}
	if _, err := e.s.SetProjectSandbox(e.ctx, web, p.Key, SandboxPolicyInput{Level: ptr(SandboxOff)}); err == nil || !strings.Contains(err.Error(), "say why") {
		t.Fatalf("loosened without a reason: %v", err)
	}
	if _, err := e.s.SetProjectSandbox(e.ctx, web, p.Key, SandboxPolicyInput{Level: ptr("chroot")}); err == nil {
		t.Error("an unknown level was accepted")
	}
	pol, err = e.s.SetProjectSandbox(e.ctx, web, p.Key, SandboxPolicyInput{Level: ptr(SandboxOff), InteractiveLevel: ptr(SandboxContained), Reason: "the build needs docker"})
	if err != nil || pol.Level != SandboxOff || pol.InteractiveLevel != SandboxContained {
		t.Fatalf("loosen = %+v, %v", pol, err)
	}
	// Another browser session of the same person hasn't re-authenticated.
	other := e.browser(owner)
	e.setClock(e.clockNow().Add(11 * time.Minute))
	if _, err := e.s.SetProjectSandbox(e.ctx, other, p.Key, SandboxPolicyInput{InteractiveLevel: ptr(SandboxOff), Reason: "x"}); !errors.Is(err, ErrReauth) {
		t.Errorf("the password check outlived ten minutes: %v", err)
	}

	log, err := e.s.SandboxLog(e.ctx, web, SandboxLogQuery{Project: p.Key})
	if err != nil || len(log) != 3 {
		t.Fatalf("log = %+v, %v", log, err)
	}
	if c := log[0]; c.Field != "interactive_level" || c.Old != "" || c.New != SandboxContained || !c.Loosens || c.Reason != "the build needs docker" || !c.Current {
		t.Errorf("log[0] = %+v", c)
	}
	off := log[1]
	if off.Field != "level" || off.Old != SandboxVM || off.New != SandboxOff || !off.Loosens || off.User == nil || off.User.ID != owner.User.ID {
		t.Errorf("log[1] = %+v", off)
	}
	if c := log[2]; c.Loosens || c.New != SandboxVM || c.Current {
		t.Errorf("log[2] = %+v", c)
	}
	// Someone who can't see the project doesn't see its log.
	stranger := e.browser(e.user("sam", false))
	if l, err := e.s.SandboxLog(e.ctx, stranger, SandboxLogQuery{}); err != nil || len(l) != 0 {
		t.Errorf("stranger's log = %+v, %v", l, err)
	}

	// Reverting off → vm tightens: no password needed.
	if err := e.s.RevertSandboxChange(e.ctx, web, off.ID, ""); err != nil {
		t.Fatal(err)
	}
	if pol, _ := e.s.ProjectSandbox(e.ctx, web, p.Key); pol.Level != SandboxVM {
		t.Errorf("after revert level = %q", pol.Level)
	}
	if err := e.s.RevertSandboxChange(e.ctx, web, off.ID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("a second revert: %v", err)
	}
	log, _ = e.s.SandboxLog(e.ctx, web, SandboxLogQuery{Project: p.Key})
	if c := log[0]; c.Reverts == nil || *c.Reverts != off.ID || c.New != SandboxVM {
		t.Errorf("revert log = %+v", c)
	}

	// The admin heard about each change; the owner who made them didn't.
	e.s.Wait()
	ns, _, _ := e.s.ListNotifications(e.ctx, e.admin, false, 20)
	var loosened bool
	for _, n := range ns {
		if n.Kind == NotifySandbox && n.Title == "Sandbox loosened in "+p.Key && strings.Contains(n.Body, "level vm → off") && strings.Contains(n.Body, "the build needs docker") {
			loosened = true
		}
	}
	if !loosened {
		t.Errorf("admin notifications = %+v", ns)
	}
	if ns, _, _ := e.s.ListNotifications(e.ctx, owner, false, 20); len(ns) != 0 {
		t.Errorf("the owner was told of their own change: %+v", ns)
	}
}

// A machine's kill switch and default level: its owner or an admin, in the
// browser; pausing loosens (WP-418, PL7 §12).
func TestSandboxMachine(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	box := e.runner(bob, "box", nil)
	mine := e.runner(e.admin, "devhost", nil)
	web, bweb := e.browser(e.admin), e.browser(bob)

	ms, err := e.s.SandboxMachines(e.ctx, bweb)
	if err != nil || len(ms) != 1 || ms[0].Runner.ID != box.ID || !ms[0].CanEdit || ms[0].Paused {
		t.Fatalf("bob's machines = %+v, %v", ms, err)
	}
	if ms, _ := e.s.SandboxMachines(e.ctx, web); len(ms) != 2 {
		t.Errorf("admin's machines = %d", len(ms))
	}
	if _, err := e.s.SetSandboxMachine(e.ctx, bweb, mine.ID, SandboxMachineInput{Paused: ptr(true), Reason: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob paused someone else's machine: %v", err)
	}
	if _, err := e.s.SetSandboxMachine(e.ctx, e.admin, mine.ID, SandboxMachineInput{Paused: ptr(true), Reason: "x"}); !errors.Is(err, ErrSandboxUI) {
		t.Errorf("paused without a browser: %v", err)
	}
	e.setClock(e.clockNow().Add(time.Hour))
	if _, err := e.s.SetSandboxMachine(e.ctx, web, mine.ID, SandboxMachineInput{Paused: ptr(true), Reason: "jobs fail"}); !errors.Is(err, ErrReauth) {
		t.Errorf("paused without a password: %v", err)
	}
	// Tightening the default needs none.
	m, err := e.s.SetSandboxMachine(e.ctx, web, mine.ID, SandboxMachineInput{DefaultLevel: ptr(SandboxVM)})
	if err != nil || m.DefaultLevel != SandboxVM {
		t.Fatalf("default = %+v, %v", m, err)
	}
	if _, err := e.s.Reauth(e.ctx, web, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	m, err = e.s.SetSandboxMachine(e.ctx, web, mine.ID, SandboxMachineInput{Paused: ptr(true), Reason: "jobs fail"})
	if err != nil || !m.Paused {
		t.Fatalf("pause = %+v, %v", m, err)
	}
	log, _ := e.s.SandboxLog(e.ctx, web, SandboxLogQuery{Runner: mine.ID})
	if len(log) != 2 || log[0].Field != "paused" || log[0].Old != "no" || log[0].New != "yes" || !log[0].Loosens || !log[0].Current {
		t.Fatalf("log = %+v", log)
	}
	// Bob doesn't see the admin's machine's log.
	if l, _ := e.s.SandboxLog(e.ctx, bweb, SandboxLogQuery{}); len(l) != 0 {
		t.Errorf("bob's log = %+v", l)
	}
	// Unpausing (a revert) tightens.
	e.setClock(e.clockNow().Add(time.Hour))
	if err := e.s.RevertSandboxChange(e.ctx, web, log[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	ms, _ = e.s.SandboxMachines(e.ctx, web)
	for _, m := range ms {
		if m.Runner.ID == mine.ID && (m.Paused || m.DefaultLevel != SandboxVM) {
			t.Errorf("after revert = %+v", m)
		}
	}
}

func TestLevelLoosens(t *testing.T) {
	for _, c := range []struct {
		old, new string
		want     bool
	}{
		{"", SandboxVM, false},
		{"", SandboxContained, true},
		{SandboxVM, "", true},
		{SandboxContained, SandboxVM, false},
		{SandboxVM, SandboxContained, true},
		{SandboxTool, SandboxOff, true},
		{SandboxOff, SandboxTool, false},
		{SandboxOff, SandboxOff, false},
	} {
		if got := levelLoosens(c.old, c.new); got != c.want {
			t.Errorf("%q → %q: %v", c.old, c.new, got)
		}
	}
}
