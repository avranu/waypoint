package core

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Keep-warm evidence (WP-1000).

func TestWarmBreakEven(t *testing.T) {
	w := defaultWarmWeights
	if got := w.pingNeeds(); math.Abs(got-0.1/1.9) > 1e-9 {
		t.Errorf("ping needs %v", got)
	}
	if !math.IsInf(w.compactNeeds(warmCompactKeeps), 1) {
		t.Error("a compaction that keeps everything pays at some rate")
	}
	// Bigger contexts need fewer uses again.
	if a, b := w.compactNeeds(100_000), w.compactNeeds(200_000); a <= b || b <= 0 {
		t.Errorf("needs at 100k %v, at 200k %v", a, b)
	}
	for _, p := range []float64{0.2, 0.4, 0.8} {
		from := w.compactFrom(p)
		if from <= warmCompactKeeps || w.compactNeeds(from) > p+1e-9 || w.compactNeeds(from-1000) <= p {
			t.Errorf("rate %v: from %d needs %v", p, from, w.compactNeeds(from))
		}
	}
	if w.compactFrom(0.04) != 0 { // under read/write: no size pays
		t.Errorf("4%% compacts from %d", w.compactFrom(0.04))
	}
	opus := weightsOf(ModelPrice{Input: 4, Output: 20, CacheWrite1h: 8, CacheRead: 0.2}, true)
	if opus != (warmWeights{Read: 0.05, Write: 2, Output: 5}) {
		t.Errorf("weights %+v", opus)
	}
	if weightsOf(ModelPrice{Input: 4}, true) != defaultWarmWeights {
		t.Error("a partial price is used")
	}
}

// warmRows records n past decisions of a class for the session's person:
// decided 2 days ago, used again after `used` (0: ended by a /clear).
func (be *bsEnv) warmRows(n int, class string, used time.Duration) {
	be.t.Helper()
	at := be.clock.Add(-48 * time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		var next, ended any
		reason := ""
		if used > 0 {
			next = at + used.Milliseconds()
		} else {
			ended, reason = at+time.Hour.Milliseconds(), "clear"
		}
		if _, err := be.s.db.W.Exec(`INSERT INTO warm_decisions (session_id, user_id, decided_at, action, class, next_at, ended_at, end_reason)
			SELECT id, user_id, ?, 'ping', ?, ?, ?, ? FROM agent_sessions WHERE id = ?`, at, class, next, ended, reason, be.agent.Session); err != nil {
			be.t.Fatal(err)
		}
	}
}

type warmRow struct {
	Action, Rule, Class, EndReason string
	Next, Ended                    bool
	Context                        int64
}

func (be *bsEnv) warmDecisions() []warmRow {
	be.t.Helper()
	rows, err := be.s.db.R.Query(`SELECT action, rule, class, end_reason, next_at IS NOT NULL, ended_at IS NOT NULL, context_tokens FROM warm_decisions
		WHERE session_id = ? AND decided_at >= ? ORDER BY id`, be.agent.Session, be.clock.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []warmRow
	for rows.Next() {
		var r warmRow
		if err := rows.Scan(&r.Action, &r.Rule, &r.Class, &r.EndReason, &r.Next, &r.Ended, &r.Context); err != nil {
			be.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestWarmDecisionsRecordWhatCameAfter(t *testing.T) {
	be := warmEnv(t)
	be.warmSweep(55 * time.Minute)
	if got := be.warmDecisions(); len(got) != 1 || got[0] != (warmRow{Action: "ping", Class: warmClassWork, Context: 50_000}) {
		t.Fatalf("after the ping: %+v", got)
	}
	// The ping's own turn is no use.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: keepWarmBody})
	be.stop(&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, "1h", "ok")
	if got := be.warmDecisions(); got[0].Next || got[0].Ended {
		t.Fatalf("the ping's turn counted: %+v", got)
	}
	// A real one is.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "How did the build go?"})
	be.hookEv(HookEvent{Event: "Stop", CacheTTL: "1h", LastMessage: "Done. The session can be cleared without losing work.", ContextTokens: 50_000})
	if got := be.warmDecisions(); !got[0].Next || got[0].Ended {
		t.Fatalf("after a real prompt: %+v", got)
	}
	// Left alone, then cleared.
	be.warmSweep(55 * time.Minute)
	be.hookEv(HookEvent{Event: "SessionEnd", EndReason: "clear"})
	got := be.warmDecisions()
	if len(got) != 2 || got[1] != (warmRow{Action: "cool", Class: warmClassClearable, EndReason: "clear", Ended: true, Context: 50_000}) || got[0].Ended {
		t.Fatalf("after the clear: %+v", got)
	}
}

func TestWarmEvidenceLetsAPingCool(t *testing.T) {
	be := warmEnv(t)
	be.warmRows(12, warmClassWork, 0) // all cleared, none used again
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 0 {
		t.Fatalf("acted: %+v", jobs)
	}
	x := be.session()
	want := "waiting on a background task; compacted instead of a ping: in 28 days 0 of 12 such waits were used within the hour (a ping pays from 5.3%); let cool, not compacted: in 28 days 0 of 12 such waits were used again (a compaction pays at no size)"
	if x.Cache.Decision != warmCool || x.Cache.Note != want {
		t.Fatalf("cache = %+v", x.Cache)
	}
	if got := be.warmDecisions(); len(got) != 1 || got[0].Action != "cool" || got[0].Rule != "ping" || got[0].Class != warmClassWork {
		t.Fatalf("recorded %+v", got)
	}
}

func TestWarmEvidenceCompactsInsteadOfAPing(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	be.hookEv(HookEvent{Event: "Stop", Pending: &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, CacheTTL: "1h", LastMessage: "Started.", ContextTokens: 90_000})
	be.warmRows(12, warmClassWork, 3*time.Hour) // all used, none within the hour
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 1 || !jobs[0].Compact {
		t.Fatalf("want a compaction: %+v", jobs)
	}
	if x := be.session(); !strings.HasSuffix(x.Cache.Note, "; compacted instead of a ping: in 28 days 0 of 12 such waits were used within the hour (a ping pays from 5.3%)") {
		t.Fatalf("cache = %+v", x.Cache)
	}
	if got := be.warmDecisions(); len(got) != 1 || got[0].Action != "compact" || got[0].Rule != "ping" || got[0].Context != 90_000 {
		t.Fatalf("recorded %+v", got)
	}
}

func TestWarmEvidenceKeepsTheRule(t *testing.T) {
	for name, seed := range map[string]func(be *bsEnv){
		"used soon":       func(be *bsEnv) { be.warmRows(12, warmClassWork, 10*time.Minute) },
		"too few":         func(be *bsEnv) { be.warmRows(warmEvidenceMin-1, warmClassWork, 0) },
		"another class":   func(be *bsEnv) { be.warmRows(20, warmClassChat, 0) },
		"one in the hour": func(be *bsEnv) { be.warmRows(1, warmClassWork, 30*time.Minute); be.warmRows(11, warmClassWork, 0) },
		"older than a month": func(be *bsEnv) {
			be.warmRows(12, warmClassWork, 0)
			_, _ = be.s.db.W.Exec(`UPDATE warm_decisions SET decided_at = decided_at - ?`, (30 * 24 * time.Hour).Milliseconds())
		},
	} {
		be := warmEnv(t)
		seed(be)
		be.warmSweep(55 * time.Minute)
		if jobs := be.warmJobs(); len(jobs) != 1 || jobs[0].Body != keepWarmBody {
			t.Errorf("%s: want the ping: %+v %+v", name, jobs, be.session().Cache)
		}
	}
}

func TestWarmDecisionsReport(t *testing.T) {
	be := warmEnv(t)
	be.warmRows(12, warmClassChat, 0)
	be.warmRows(3, warmClassChat, 20*time.Minute)
	be.warmRows(4, warmClassWork, 2*time.Hour)
	be.warmSweep(55 * time.Minute) // work: too few, the rule pings
	rep, err := be.s.WarmDecisions(be.ctx, be.admin, UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 28 || rep.MinSample != warmEvidenceMin || math.Abs(rep.PingNeeds-0.1/1.9) > 1e-9 || len(rep.Classes) != 2 {
		t.Fatalf("report %+v", rep)
	}
	chat, work := rep.Classes[0], rep.Classes[1]
	if chat.Class != warmClassChat || chat.Decisions != 15 || chat.Pings != 15 || chat.Soon != 3 || chat.SoonOf != 15 || chat.Later != 3 || chat.LaterOf != 15 ||
		chat.Cleared != 12 || chat.PingPays == nil || !*chat.PingPays || chat.CompactFrom != defaultWarmWeights.compactFrom(0.2) {
		t.Errorf("chat %+v", chat)
	}
	// The ping just now is open, and too recent to count within the hour.
	if work.Class != warmClassWork || work.Decisions != 5 || work.Open != 1 || work.SoonOf != 4 || work.Later != 4 || work.LaterOf != 4 || work.PingPays != nil || work.CompactFrom != 0 {
		t.Errorf("work %+v", work)
	}
	if _, err := be.s.WarmDecisions(be.ctx, be.agent, UsageQuery{User: "all"}); err == nil {
		t.Error("an agent read everyone's")
	}
}

// The migration reads past decisions back from the timelines.
func TestWarmDecisionsBackfill(t *testing.T) {
	be := newBabysitEnv(t)
	raw, err := os.ReadFile("../db/migrations/0150_warm_decisions.sql")
	if err != nil {
		t.Fatal(err)
	}
	insert := string(raw)[strings.Index(string(raw), "INSERT INTO warm_decisions"):]
	at := be.clock.Add(-10 * time.Hour).UnixMilli()
	min := time.Minute.Milliseconds()
	for i, ev := range []struct {
		kind, detail string
		at           int64
	}{
		{"cache", "Kept the prompt cache warm: its person was working with it and is active", at},
		{"cache", "Compacting it (138k tokens of context) before its prompt cache cools: an open request while its person is active; compacted instead of a second ping", at + 60*min},
		{"prompt", "go on", at + 70*min},
		{"cache", "Let the prompt cache cool: it said it can be cleared", at + 130*min},
		{"cache", "Compacted it; its prompt cache now cools", at + 131*min},
		{"ended", "clear", at + 140*min},
		{"prompt", "resumed", at + 150*min},
	} {
		if _, err := be.s.db.W.Exec(`INSERT INTO session_events (session_id, kind, detail, created_at) VALUES (?, ?, ?, ?)`, be.agent.Session, ev.kind, ev.detail, ev.at+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := be.s.db.W.Exec(`DELETE FROM warm_decisions`); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(insert); err != nil {
		t.Fatal(err)
	}
	got := be.warmDecisions()
	want := []warmRow{
		{Action: "ping", Class: warmClassChat, Next: true},
		{Action: "compact", Class: warmClassRequests, Next: true, Context: 138_000},
		{Action: "cool", Class: warmClassClearable, Ended: true, EndReason: "clear"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
