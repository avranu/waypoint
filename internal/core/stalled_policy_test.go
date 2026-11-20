package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// When sessions are told of stalled jobs (WP-1045): the policy, quiet
// spells reported from the runner's floor, early notices at a keep-warm
// ping, cold wakes, withdrawn notices and the evidence.

func (be *bsEnv) reportV2(runner int64, settled []SettledJob, jobs ...StalledJob) []StalledJobOutcome {
	be.t.Helper()
	out, err := be.s.ReportStalledJobs(be.ctx, be.admin, StalledJobsReport{Runner: runner, Jobs: jobs, Settled: settled, V: 2})
	if err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	return out
}

type spellRow struct {
	how, settled, outcome string
	told                  bool
}

func (be *bsEnv) spells() []spellRow {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT told_how, settled, outcome, told_at IS NOT NULL FROM stalled_jobs ORDER BY id`)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []spellRow
	for rows.Next() {
		var r spellRow
		_ = rows.Scan(&r.how, &r.settled, &r.outcome, &r.told)
		out = append(out, r)
	}
	return out
}

func quietJob(quiet int, age time.Duration, now time.Time) StalledJob {
	j := stalledJobFixture
	j.Quiet = quiet
	j.StartedAt = now.Add(-age)
	return j
}

// Quiet under the stall time: kept, not told, asked again soon.
func TestStalledQuietNotYet(t *testing.T) {
	be := warmEnv(t)
	r := be.runner(be.admin, "m1", nil)
	out := be.reportV2(r.ID, nil, quietJob(6, 40*time.Minute, be.clockNow()))
	if len(out) != 1 || out[0].Outcome != "quiet, not stalled yet" || out[0].Retry || out[0].AgainIn != 300 {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(be.stalledMessages()); n != 0 {
		t.Fatalf("%d messages", n)
	}
	// An older runner is asked to report it again instead.
	out = be.reportStalled(r.ID, quietJob(6, 40*time.Minute, be.clockNow()))
	if !out[0].Retry || out[0].AgainIn != 0 {
		t.Errorf("older runner: %+v", out)
	}
	// It moved again: its spell ends by itself, and the next is a new one.
	be.reportV2(r.ID, []SettledJob{{Session: "cli-1", Task: "b1x9", How: "progress"}})
	be.reportV2(r.ID, nil, quietJob(5, 50*time.Minute, be.clockNow()))
	if sp := be.spells(); len(sp) != 2 || sp[0].settled != "progress" || sp[0].outcome != "self" || sp[0].told || sp[1].settled != "" {
		t.Errorf("spells %+v", sp)
	}
	// Told, or nothing more to come: an older runner isn't asked again
	// (it would report it every minute).
	for _, j := range []StalledJob{quietJob(20, 40*time.Minute, be.clockNow()), quietJob(21, 41*time.Minute, be.clockNow())} {
		if out = be.reportStalled(r.ID, j); out[0].Retry {
			t.Errorf("older runner asked again: %+v", out)
		}
	}
	nobody := quietJob(20, 40*time.Minute, be.clockNow())
	nobody.Session = "cli-nope"
	if out = be.reportStalled(r.ID, nobody); out[0].Retry {
		t.Errorf("no session, asked again: %+v", out)
	}
}

// Off, or a short timeout, and nobody is told; a project's own wins.
func TestStalledPolicyOffAndTimeouts(t *testing.T) {
	be := warmEnv(t)
	r := be.runner(be.admin, "m1", nil)
	j := quietJob(30, time.Hour, be.clockNow())
	j.Timeout = 45
	if out := be.reportV2(r.ID, nil, j); !strings.Contains(out[0].Outcome, "45-minute timeout") {
		t.Errorf("timeout: %+v", out)
	}
	// A spell that couldn't be told isn't evidence once it ends.
	be.reportV2(r.ID, []SettledJob{{Session: "cli-1", Task: "b1x9", How: "ended"}})
	var skipped string
	if err := be.s.db.R.QueryRow(`SELECT skipped FROM stalled_jobs`).Scan(&skipped); err != nil || skipped != "timeout" {
		t.Errorf("skipped %q %v", skipped, err)
	}
	if f := be.s.stallFacts(be.ctx, be.admin.User.ID); len(f) != 0 {
		t.Errorf("untold spell counted: %+v", f)
	}
	off := DefaultStallPolicy
	off.Off = true
	if _, err := be.s.SetStallPolicy(be.ctx, be.agent, &off); err == nil {
		t.Error("an agent set the policy")
	}
	g, err := be.s.SetStallPolicy(be.ctx, be.admin, &off)
	if err != nil || !g.Custom || !g.Policy.Off {
		t.Fatalf("set: %+v %v", g, err)
	}
	j.Timeout = 0
	if out := be.reportV2(r.ID, nil, j); !strings.Contains(out[0].Outcome, "aren't told") {
		t.Errorf("off: %+v", out)
	}
	// The session's project turns it back on for itself.
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ? WHERE id = ?`, be.p.ID, be.agent.Session); err != nil {
		t.Fatal(err)
	}
	var pin ProjectInput
	if err := json.Unmarshal([]byte(`{"settings":{"stalled_jobs":{"min_age":20,"quiet":20,"skip_timeout":0,"early":0,"cold_wake":0}}}`), &pin); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.UpdateProject(be.ctx, be.admin, be.p.Key, pin); err != nil {
		t.Fatal(err)
	}
	pp, err := be.s.ProjectStallPolicy(be.ctx, be.admin, be.p.Key)
	if err != nil || !pp.Custom || pp.Policy.Quiet != 20 || pp.Inherited == nil || !pp.Inherited.Off {
		t.Fatalf("project policy %+v %v", pp, err)
	}
	if out := be.reportV2(r.ID, nil, j); out[0].Outcome != "woke its session (its prompt cache is warm)" {
		t.Errorf("project's own: %+v", out)
	}
	// Times out of range are refused.
	if err := json.Unmarshal([]byte(`{"settings":{"stalled_jobs":{"min_age":20,"quiet":2}}}`), &pin); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.UpdateProject(be.ctx, be.admin, be.p.Key, pin); err == nil {
		t.Error("a 2-minute stall time was taken")
	}
	// nil goes back to the default.
	if g, err := be.s.SetStallPolicy(be.ctx, be.admin, nil); err != nil || g.Custom || g.Policy != DefaultStallPolicy {
		t.Errorf("reset: %+v %v", g, err)
	}
}

// A notice its session hasn't read when the job moves again is withdrawn.
func TestStalledNoticeWithdrawn(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	r := be.runner(be.admin, "m1", nil)
	out := be.reportV2(r.ID, nil, quietJob(30, time.Hour, be.clockNow()))
	if !strings.Contains(out[0].Outcome, "it is if there's still no progress after 60 min") || out[0].AgainIn != 15*60 {
		t.Fatalf("cold: %+v", out)
	}
	be.reportV2(r.ID, []SettledJob{{Session: "cli-1", Task: "b1x9", How: "progress"}})
	if msgs := be.stalledMessages(); len(msgs) != 1 || msgs[0].state != "cancelled" {
		t.Fatalf("messages %+v", msgs)
	}
	if sp := be.spells(); len(sp) != 1 || sp[0].outcome != "withdrawn" || sp[0].how != stallPassive {
		t.Errorf("spells %+v", sp)
	}
	if tl := be.timeline("stalled"); len(tl) != 2 || !strings.Contains(tl[1], "withdrew it unsent") {
		t.Errorf("timeline %q", tl)
	}
	if res := be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "go on"}); res != nil && strings.Contains(res.Text, "b1x9") {
		t.Errorf("a withdrawn notice was read: %q", res.Text)
	}
}

// A spell lost (no report for 40 minutes) and found again: the session is
// told once, by the new spell's notice.
func TestStalledLostSpellNoticeGivesWay(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	r := be.runner(be.admin, "m1", nil)
	be.reportV2(r.ID, nil, quietJob(30, time.Hour, be.clockNow()))
	be.setClock(be.clockNow().Add(45 * time.Minute))
	be.runner(be.admin, "m1", nil)
	be.reportV2(r.ID, nil, quietJob(30, 2*time.Hour, be.clockNow()))
	msgs := be.stalledMessages()
	if len(msgs) != 2 || msgs[0].state != "cancelled" || msgs[1].state != MessageQueued {
		t.Fatalf("messages %+v", msgs)
	}
	if sp := be.spells(); len(sp) != 2 || sp[0].outcome != "unknown" || !sp[1].told {
		t.Errorf("spells %+v", sp)
	}
}

// A cold session is woken once its job has made no progress for ColdWake.
func TestStalledColdWake(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(2 * time.Hour))
	r := be.runner(be.admin, "m1", nil)
	be.reportV2(r.ID, nil, quietJob(30, time.Hour, be.clockNow()))
	be.setClock(be.clockNow().Add(20 * time.Minute))
	be.runner(be.admin, "m1", nil)
	out := be.reportV2(r.ID, nil, quietJob(50, 80*time.Minute, be.clockNow()))
	if !strings.Contains(out[0].Outcome, "it waits for its next turn") || out[0].AgainIn != 10*60 {
		t.Fatalf("before: %+v", out)
	}
	be.setClock(be.clockNow().Add(11 * time.Minute))
	be.runner(be.admin, "m1", nil)
	out = be.reportV2(r.ID, nil, quietJob(61, 91*time.Minute, be.clockNow()))
	if !strings.HasPrefix(out[0].Outcome, "woke its session after 61 minutes") {
		t.Fatalf("cold wake: %+v", out)
	}
	if msgs := be.stalledMessages(); len(msgs) != 1 || msgs[0].passive || msgs[0].state == MessageQueued {
		t.Fatalf("messages %+v", msgs)
	}
	if sp := be.spells(); sp[0].how != stallColdWake {
		t.Errorf("spells %+v", sp)
	}
	// Read, and the job ended soon after: acted on.
	if _, err := be.s.db.W.Exec(`UPDATE session_messages SET state = 'delivered', delivered_at = ?`, be.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(3 * time.Minute))
	be.reportV2(r.ID, []SettledJob{{Session: "cli-1", Task: "b1x9", How: "ended"}})
	if sp := be.spells(); sp[0].outcome != "acted" || sp[0].settled != "ended" {
		t.Errorf("spells %+v", sp)
	}
}

// When its keep-warm ping is due, a job quiet for Early is told of then,
// in place of the ping.
func TestStalledEarlyAtKeepWarm(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(52 * time.Minute))
	r := be.runner(be.admin, "m1", nil)
	out := be.reportV2(r.ID, nil, quietJob(11, 45*time.Minute, be.clockNow()))
	if out[0].Outcome != "quiet, not stalled yet" || out[0].AgainIn != 4*60 {
		t.Fatalf("outcome %+v", out)
	}
	be.warmSweep(3 * time.Minute) // 55 minutes after its stop
	if jobs := be.warmJobs(); len(jobs) != 0 {
		t.Errorf("pinged as well: %+v", jobs)
	}
	msgs := be.stalledMessages()
	if len(msgs) != 1 || msgs[0].passive || !strings.Contains(msgs[0].body, "no progress for 14 minutes") {
		t.Fatalf("messages %+v", msgs)
	}
	if sp := be.spells(); sp[0].how != stallKeepWarm {
		t.Errorf("spells %+v", sp)
	}
	if x := be.session(); x.Cache == nil || !strings.HasPrefix(x.Cache.Note, "its quiet background job's notice went now") {
		t.Errorf("cache %+v", x.Cache)
	}
	if out := be.reportV2(r.ID, nil, quietJob(15, 48*time.Minute, be.clockNow())); out[0].Outcome != "its session was told already" {
		t.Errorf("again: %+v", out)
	}
	// What the session reads is headed as news of its job.
	if text := MessagesText([]*SessionMessage{{Body: "x", stepKey: "stalled:1:2"}}); !strings.HasPrefix(text, "Waypoint, news of your background job") {
		t.Errorf("header %q", text)
	}
}

// Early stretches with the stall time the evidence sets: a kind of job
// that is often quiet and fine isn't told of early at 14 minutes either.
func TestStalledEarlyFollowsEvidence(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(52 * time.Minute))
	now := be.s.nowMS()
	for i := range 10 {
		if _, err := be.s.db.W.Exec(`INSERT INTO stalled_jobs (user_id, cli_session, task, class, started_at, quiet_from, seen_at, settled_at, settled, outcome)
			VALUES (?, 'cli-old', ?, 'loop', ?, ?, ?, ?, 'ended', 'self')`, be.admin.User.ID, fmt.Sprintf("t%d", i), now-3_600_000, now-3_000_000, now-1_800_000, now-1_800_000); err != nil {
			t.Fatal(err)
		}
	}
	if th := be.s.stallThreshold(be.ctx, be.admin.User.ID, "loop", DefaultStallPolicy); th.Quiet != 60 {
		t.Fatalf("threshold %+v", th)
	}
	r := be.runner(be.admin, "m1", nil)
	be.reportV2(r.ID, nil, quietJob(11, 45*time.Minute, be.clockNow()))
	be.warmSweep(3 * time.Minute)
	if msgs := be.stalledMessages(); len(msgs) != 0 {
		t.Errorf("told early: %+v", msgs)
	}
}

// The evidence: a kind of job whose spells mostly end by themselves gets
// a longer quiet time; cold wakes stop while seldom acted on.
func TestStallEvidence(t *testing.T) {
	var facts []stallFact
	for range 8 {
		facts = append(facts, stallFact{Class: "go test", Outcome: "self", Length: 20})
	}
	for range 4 {
		facts = append(facts, stallFact{Class: "go test", Outcome: "acted", How: stallWoke})
	}
	// 12 reach 15 (4 stuck: 33%); from 23 only the 4 stuck ones are left.
	if q := stallQuietFor(facts, "go test", 15); q.Quiet != 23 || q.Sample != 12 || q.Stuck != 4 || !strings.Contains(q.Why, "`go test` jobs'") {
		t.Errorf("go test: %+v", q)
	}
	// Another kind with too few spells of its own uses everyone's.
	if q := stallQuietFor(facts, "git push", 15); q.Quiet != 23 || !strings.Contains(q.Why, "all jobs'") {
		t.Errorf("git push: %+v", q)
	}
	// Too few spells: the setting stands.
	if q := stallQuietFor(facts[:9], "go test", 15); q.Quiet != 15 || q.Why != "" {
		t.Errorf("few: %+v", q)
	}
	if ok, _ := stallColdFor(facts); !ok {
		t.Error("no cold wakes, yet they stop")
	}
	var cold []stallFact
	for i := range 10 {
		o := "self"
		if i < 2 {
			o = "acted"
		}
		cold = append(cold, stallFact{How: stallColdWake, Outcome: o})
	}
	if ok, why := stallColdFor(cold); ok || why != "2 of 10 cold wakes in 28 days led to the job being stopped" {
		t.Errorf("cold: %v %q", ok, why)
	}
}

func TestJobClass(t *testing.T) {
	for cmd, want := range map[string]string{
		`cd /x && go test ./... -count=1`:                          "go test",
		`timeout 600 git push origin HEAD`:                         "git push",
		`while pgrep -f "[g]olangci" >/dev/null; do sleep 5; done`: "loop",
		`GOFLAGS=-mod=mod make check`:                              "make check",
		`/usr/bin/curl -s http://x`:                                "curl",
		`npm run build`:                                            "npm run",
		`sleep 300`:                                                "sleep",
		``:                                                         "",
	} {
		if got := jobClass(cmd); got != want {
			t.Errorf("jobClass(%q) = %q, want %q", cmd, got, want)
		}
	}
}
