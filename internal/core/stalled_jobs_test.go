package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Stalled background jobs (WP-605).

var stalledJobFixture = StalledJob{Session: "cli-1", Task: "b1x9", PID: 4242, Group: true,
	Command: `while pgrep -f "golangci-lint run" >/dev/null; do sleep 5; done; git push`, Quiet: 30,
	Evidence: []string{"Running 30 min; no output at all.", "`pgrep -f 'golangci-lint run'` matches the job's own `bash -c` command line, so it always finds a process."},
	Running:  []string{"pid 4242: bash (the job's shell)", "pid 5000: sleep 5"}}

func (be *bsEnv) reportStalled(runner int64, jobs ...StalledJob) []StalledJobOutcome {
	be.t.Helper()
	out, err := be.s.ReportStalledJobs(be.ctx, be.admin, StalledJobsReport{Runner: runner, Jobs: jobs})
	if err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	return out
}

type stalledMsg struct {
	body, state string
	passive     bool
}

func (be *bsEnv) stalledMessages() []stalledMsg {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT body, state, passive FROM session_messages WHERE session_id = ? AND step_key LIKE 'stalled:%' ORDER BY id`, be.agent.Session)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []stalledMsg
	for rows.Next() {
		var m stalledMsg
		_ = rows.Scan(&m.body, &m.state, &m.passive)
		out = append(out, m)
	}
	return out
}

// A warm session is woken, once; its timeline says so.
func TestStalledJobWakesAWarmSession(t *testing.T) {
	be := warmEnv(t)
	r := be.runner(be.admin, "m1", nil)
	out := be.reportStalled(r.ID, stalledJobFixture)
	if len(out) != 1 || out[0].Task != "b1x9" || out[0].Outcome != "woke its session (its prompt cache is warm)" {
		t.Fatalf("outcome %+v", out)
	}
	msgs := be.stalledMessages()
	if len(msgs) != 1 || msgs[0].passive || msgs[0].state == MessageQueued {
		t.Fatalf("messages %+v, want one sent now", msgs)
	}
	for _, want := range []string{
		"your background job b1x9 has made no progress for 30 minutes",
		"it hasn't touched it",
		"- `pgrep -f 'golangci-lint run'` matches",
		"Running under it: pid 4242: bash (the job's shell); pid 5000: sleep 5.",
		"TaskStop b1x9. This was seen some time ago: only use `kill -- -4242` (its whole process group) if `ps -o args= -p 4242`",
	} {
		if !strings.Contains(msgs[0].body, want) {
			t.Errorf("message lacks %q:\n%s", want, msgs[0].body)
		}
	}
	if tl := be.timeline("stalled"); len(tl) != 1 || !strings.HasPrefix(tl[0], "No progress for 30 min; woke its session (its prompt cache is warm). `while pgrep") || !strings.HasSuffix(tl[0], "Running 30 min; no output at all.") {
		t.Errorf("timeline %q", tl)
	}
	// The runner reports it again (it restarted): told once.
	if out := be.reportStalled(r.ID, stalledJobFixture); out[0].Outcome != "its session was told already" {
		t.Errorf("again: %+v", out)
	}
	if n := len(be.stalledMessages()); n != 1 {
		t.Errorf("%d messages", n)
	}
}

// A cold one isn't: the news waits for its next turn.
func TestStalledJobWaitsForAColdSession(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	r := be.runner(be.admin, "m1", nil)
	out := be.reportStalled(r.ID, stalledJobFixture)
	if len(out) != 1 || !strings.Contains(out[0].Outcome, "reads it at its next turn (its prompt cache is cold") {
		t.Fatalf("outcome %+v", out)
	}
	if msgs := be.stalledMessages(); len(msgs) != 1 || !msgs[0].passive || msgs[0].state != MessageQueued {
		t.Fatalf("messages %+v, want one passive, queued", msgs)
	}
	if tl := be.timeline("stalled"); len(tl) != 1 || !strings.Contains(tl[0], "wasn't woken") {
		t.Errorf("timeline %q", tl)
	}
	// Its next prompt brings it.
	res := be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "go on"})
	if res == nil || !strings.Contains(res.Text, "b1x9") {
		t.Errorf("next prompt: %+v", res)
	}
}

// Mid-turn, it reads it when the turn ends; nothing wakes it.
func TestStalledJobInATurn(t *testing.T) {
	be := warmEnv(t)
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "go on"})
	r := be.runner(be.admin, "m1", nil)
	out := be.reportStalled(r.ID, stalledJobFixture)
	if len(out) != 1 || out[0].Outcome != "its session reads it when its turn ends" {
		t.Fatalf("outcome %+v", out)
	}
	if msgs := be.stalledMessages(); len(msgs) != 1 || msgs[0].passive || msgs[0].state != MessageQueued {
		t.Fatalf("messages %+v, want one queued for its Stop", msgs)
	}
	if res := be.stop(nil, "1h", "Waiting on lint."); res == nil || !strings.Contains(res.Text, "b1x9") {
		t.Errorf("its Stop: %+v", res)
	}
}

func TestCacheWarmNow(t *testing.T) {
	be := warmEnv(t)
	id := be.agent.Session
	if w, _ := be.s.cacheWarmNow(be.ctx, id); !w {
		t.Error("just stopped on a 1h cache: cold")
	}
	be.setClock(be.clockNow().Add(59*time.Minute + 30*time.Second))
	if w, _ := be.s.cacheWarmNow(be.ctx, id); w {
		t.Error("59.5 minutes after its stop: warm")
	}
	be.stop(nil, "5m", "Done.")
	be.setClock(be.clockNow().Add(3 * time.Minute))
	if w, _ := be.s.cacheWarmNow(be.ctx, id); !w {
		t.Error("3 minutes on the 5m tier: cold")
	}
	be.setClock(be.clockNow().Add(2 * time.Minute))
	if w, _ := be.s.cacheWarmNow(be.ctx, id); w {
		t.Error("5 minutes on the 5m tier: warm")
	}
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "go on"})
	if w, in := be.s.cacheWarmNow(be.ctx, id); !w || !in {
		t.Error("in a turn: cold, or not in a turn")
	}
}

func TestReportStalledJobsChecks(t *testing.T) {
	be := warmEnv(t)
	r := be.runner(be.admin, "m1", nil)
	// An agent's token isn't a runner's.
	if _, err := be.s.ReportStalledJobs(be.ctx, be.agent, StalledJobsReport{Runner: r.ID, Jobs: []StalledJob{stalledJobFixture}}); err == nil {
		t.Error("an agent reported")
	}
	// Another person's runner.
	other := be.user("other", false)
	if _, err := be.s.ReportStalledJobs(be.ctx, other, StalledJobsReport{Runner: r.ID, Jobs: []StalledJob{stalledJobFixture}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another person's runner: %v", err)
	}
	// Bad ids are skipped; an unknown session is said.
	bad, unknown := stalledJobFixture, stalledJobFixture
	bad.Session = "../x"
	unknown.Session = "no-such-session"
	out := be.reportStalled(r.ID, bad, unknown)
	if len(out) != 1 || !strings.HasPrefix(out[0].Outcome, "no open Waypoint session") {
		t.Errorf("outcomes %+v", out)
	}
	if n := len(be.stalledMessages()); n != 0 {
		t.Errorf("%d messages", n)
	}
}
