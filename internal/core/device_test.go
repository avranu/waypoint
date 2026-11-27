package core

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceLoginRaces(t *testing.T) {
	e := newEnv(t)

	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.s.StartDeviceLogin(e.ctx, "laptop", "", "203.0.113.9", "wp/test"); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Errorf("%d logins started from one address, cap 5", ok.Load())
	}

	d, err := e.s.StartDeviceLogin(e.ctx, "box", "anthropic/claude-opus-5-5", "198.51.100.1", "wp/test")
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := e.s.PollDeviceLogin(e.ctx, d.DeviceCode); tok != "" || err != nil {
		t.Fatalf("pending poll = %q %v", tok, err)
	}
	e.setClock(e.clockNow().Add(DeviceLoginTTL - 30*time.Second))
	if err := e.s.ApproveDeviceLogin(e.ctx, e.admin, d.UserCode); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ApproveDeviceLogin(e.ctx, e.admin, d.UserCode); err == nil {
		t.Error("approved twice")
	}
	var stored int
	e.s.db.R.QueryRow(`SELECT count(*) FROM device_logins WHERE token != ''`).Scan(&stored)
	if stored != 0 {
		t.Error("a token was stored waiting for the CLI")
	}
	e.setClock(e.clockNow().Add(time.Minute))
	var got sync.Map
	var n atomic.Int32
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := e.s.PollDeviceLogin(e.ctx, d.DeviceCode); err == nil && tok != "" {
				got.Store(tok, true)
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("token delivered %d times", n.Load())
	}
	var tok string
	got.Range(func(k, _ any) bool { tok = k.(string); return false })
	a, err := e.s.TokenActor(e.ctx, tok)
	if err != nil || a.Agent != "anthropic/claude-opus-5-5" || a.User.ID != e.admin.User.ID {
		t.Fatalf("token actor = %+v %v", a, err)
	}
	list, _, err := e.s.ListNotifications(e.ctx, e.admin, false, 10)
	if err != nil || len(list) == 0 || list[0].Kind != NotifyToken {
		t.Errorf("notifications = %+v %v", list, err)
	}
}

func TestUsageReportOnce(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Usage", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Work", Agent: "any"})
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "bot", Agent: "anthropic/claude-opus-5-5"})
	bot, _ := e.s.TokenActor(e.ctx, tok)
	if _, err := e.s.AgentNext(e.ctx, bot, AgentQuery{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := e.s.AgentReportUsage(e.ctx, bot, i.Key, "", AgentUsage{InputTokens: 100, ReportID: "r1"}); err != nil {
			t.Fatal(err)
		}
	}
	e.s.AgentReportUsage(e.ctx, bot, i.Key, "", AgentUsage{InputTokens: 5, ReportID: "r2"})
	e.s.AgentReportUsage(e.ctx, bot, i.Key, "", AgentUsage{InputTokens: 1})
	if runs := e.get(e.admin, i.Key).AgentRuns; len(runs) != 1 || runs[0].InputTokens != 106 {
		t.Errorf("runs = %+v", runs)
	}
}
