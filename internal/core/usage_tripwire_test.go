package core

import (
	"strings"
	"testing"
	"time"
)

func TestQuotaTripwire(t *testing.T) {
	e := newAnalyticsEnv(t)
	uid := e.me.User.ID
	reset := e.clockNow().Add(3 * 24 * time.Hour).UnixMilli()
	next := reset + 7*24*3600*1000
	send := func(pct float64, resetsAt int64) {
		t.Helper()
		in := UsageQuotaInput{Machine: "devhost", Session: "s", SevenDay: &QuotaWindow{Percent: pct, ResetsAt: resetsAt}}
		if err := e.s.RecordQuota(e.ctx, e.me, in); err != nil {
			t.Fatal(err)
		}
	}
	pushes := func() (n int) {
		_ = e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ? AND user_id = ?`, NotifyQuota, uid).Scan(&n)
		return
	}
	setPref := func(json string) {
		t.Helper()
		if _, err := e.s.db.W.Exec(`UPDATE users SET prefs = ? WHERE id = ?`, json, uid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.db.W.Exec(`INSERT INTO usage_hours(user_id, hour, machine, session, repo, model, output) VALUES (?,?,?,?,?,?,?)`,
		uid, e.hour(0), "devhost", "s", "Quarry", "m", 500); err != nil {
		t.Fatal(err)
	}
	step := func(d time.Duration) { e.setClock(e.clockNow().Add(d)) }

	send(30, reset)
	step(20 * time.Minute)
	send(33, reset)
	if pushes() != 0 {
		t.Fatal("pushed below the threshold")
	}
	step(32 * time.Minute)
	send(36, reset)
	if pushes() != 1 {
		t.Fatalf("pushes = %d, want 1", pushes())
	}
	var body string
	_ = e.s.db.R.QueryRow(`SELECT body FROM notifications WHERE kind = ?`, NotifyQuota).Scan(&body)
	if !strings.HasPrefix(body, "+6% of your week in 52 min") || !strings.Contains(body, "Busiest: devhost, Quarry session") {
		t.Errorf("body = %q", body)
	}
	step(10 * time.Minute)
	send(42, reset)
	if pushes() != 1 {
		t.Errorf("debounce failed: %d", pushes())
	}
	step(2 * time.Hour)
	send(50, reset)
	step(5 * time.Minute)
	send(55, reset)
	if pushes() != 2 {
		t.Errorf("after the hour: %d, want 2", pushes())
	}

	step(10 * time.Minute)
	send(2, next)
	step(time.Hour + time.Minute)
	send(1, next)
	if pushes() != 2 {
		t.Errorf("a reset fired: %d", pushes())
	}

	setPref(`{"quota_jump":0}`)
	step(2 * time.Hour)
	send(10, next)
	step(10 * time.Minute)
	send(40, next)
	if pushes() != 2 {
		t.Errorf("fired while off: %d", pushes())
	}
	setPref(`{"quota_jump":8}`)
	step(2 * time.Hour)
	send(50, next)
	step(10 * time.Minute)
	send(56, next)
	if pushes() != 2 {
		t.Errorf("fired below a custom threshold: %d", pushes())
	}
}
