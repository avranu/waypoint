package core

import (
	"strings"
	"testing"
	"time"
)

// The session's own ship asks decideRound what it means for the round it
// reaches (WP-1370): its report when the round is under way, ended, or
// given up after its prompt went in; nothing when the round never started.
func TestShipAsksDecideRound(t *testing.T) {
	for _, c := range []struct {
		state   string
		started bool
		report  bool
	}{
		{RoundRunning, true, true},
		{RoundDone, true, true},
		{RoundFailed, true, true},
		{RoundFailed, false, false},
		{RoundPending, false, false},
		{RoundSkipped, true, false},
		{RoundInterrupted, true, false},
	} {
		out := decideRound(roundIn{State: c.state, Started: c.started, ShipAsks: true})
		if got := out.Do == doReport; got != c.report {
			t.Errorf("%s started=%v: report=%v, want %v (%+v)", c.state, c.started, got, c.report, out)
		}
		if out.Do == doNothing && out.Settled == "" {
			t.Errorf("%s started=%v: a doNothing without its reason", c.state, c.started)
		}
		if roundDecidable(c.state, c.started) != c.report {
			t.Errorf("roundDecidable(%s, %v) disagrees with decideRound", c.state, c.started)
		}
	}
}

// The session's plain ship of a pending round whose audit it read asks
// decideRound too (WP-1370; shipAudited): its report only for a pending
// round with a finished, read audit nothing is left to decide in; a tell
// while findings are left; nothing otherwise.
func TestShipAuditedDecideRound(t *testing.T) {
	for _, c := range []struct {
		name string
		in   roundIn
		do   int
	}{
		{"done audit, all decided", roundIn{State: RoundPending, Audit: AuditDone}, doReport},
		{"done audit, findings left", roundIn{State: RoundPending, Audit: AuditDone, Undecided: 2}, doTell},
		{"done audit that read nothing", roundIn{State: RoundPending, Audit: AuditDone, ReadNothing: true}, doNothing},
		{"audit queued", roundIn{State: RoundPending, Audit: AuditQueued}, doNothing},
		{"audit running", roundIn{State: RoundPending, Audit: AuditRunning}, doNothing},
		{"audit failed", roundIn{State: RoundPending, Audit: AuditFailed}, doNothing},
		{"no audit", roundIn{State: RoundPending}, doNothing},
		{"round under way", roundIn{State: RoundRunning, Audit: AuditDone}, doNothing},
		{"round done already", roundIn{State: RoundDone, Audit: AuditDone}, doNothing},
		{"round skipped", roundIn{State: RoundSkipped, Audit: AuditDone}, doNothing},
	} {
		c.in.ShipAudited = true
		out := decideRound(c.in)
		if out.Do != c.do {
			t.Errorf("%s: %+v, want %d", c.name, out, c.do)
		}
		if out.Do == doNothing && out.Settled == "" {
			t.Errorf("%s: a doNothing without its reason", c.name)
		}
		if out.Do == doTell && out.Until <= 0 {
			t.Errorf("%s: a tell with no time", c.name)
		}
	}
}

// shipAuditedEnv is a pending round whose audit is on the box, the session
// stopped and the round's job queued: the state a plain wp ship --pr finds.
func shipAuditedEnv(t *testing.T) (a *apEnv, box *Runner, g *shipForge, key string, roundID int64) {
	t.Helper()
	a, box, g = firstEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
		t.Fatal(err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.clock = a.clock.Add(time.Minute)
	if res, err := a.ship(first.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	roundID = a.newestRoundID()
	a.stopWith("Pushed; the review comes first.")
	a.tick(61 * time.Second)
	return a, box, g, first.Key, roundID
}

// The same call through the real ship, which decideRound alone doesn't
// prove: the audit's status shipAudited hands decideRound, and that it is
// decideRound's answer that ends the round (WP-1370). Each row would end
// the round wrongly (or leave it wrongly) if shipAudited fed decideRound
// the wrong status, or did not ask it.
func TestShipAuditedAsksDecideRoundWithTheAuditsStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		status string
		marks  int
		ends   bool
		notice string
	}{
		{"audit queued", AuditQueued, 0, false, ""},
		{"audit running", AuditRunning, 0, false, ""},
		{"audit done, two findings undecided", "", 0, false, "2 findings still to decide"},
		{"audit done, one finding undecided", "", 1, false, "1 finding still to decide"},
		{"audit done, every finding decided", "", 2, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, box, g, key, roundID := shipAuditedEnv(t)
			if c.status == "" {
				a.endAudit(box, "done", "Off by one", "Missing test")
				ra, _ := a.roundAuditRow(roundID)
				for i := 1; i <= c.marks; i++ {
					id := itoa64(ra.id) + "." + itoa64(int64(i))
					if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{id}, State: "fixed", Note: "done"}); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				ra, _ := a.roundAuditRow(roundID)
				if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = ? WHERE id = ?`, c.status, ra.id); err != nil {
					t.Fatal(err)
				}
			}
			res, err := a.ship(key, ShipPR)
			if err != nil {
				t.Fatal(err)
			}
			if len(g.created) != 0 {
				t.Fatalf("the pull request opened with the round's review not decided")
			}
			r := a.round(roundID)
			if ended := r.state == RoundDone; ended != c.ends {
				t.Fatalf("round %s after the ship (%q): ended=%v, want %v", r.state, res.Held, ended, c.ends)
			}
			if c.notice != "" && !strings.Contains(res.Notice, c.notice) {
				t.Fatalf("notice = %q, want it to say %q", res.Notice, c.notice)
			}
			if c.notice == "" && strings.Contains(res.Notice, "still to decide") {
				t.Fatalf("notice names findings: %q", res.Notice)
			}
		})
	}
}
