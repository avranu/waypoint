package core

import (
	"strings"
	"testing"
	"time"
)

// ownReviewDone is the SubagentStop of an adversarial-review subagent the
// session ran, as the hook sends it.
func (a *apEnv) ownReviewDone(report string) {
	a.t.Helper()
	a.ownReviewAsked("Attack the diff.", report)
}

func (a *apEnv) ownReviewAsked(prompt, report string) {
	a.t.Helper()
	a.setClock(a.clockNow().Add(10 * time.Second))
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SubagentStop", func(h *HookEvent) {
		h.AgentID, h.AgentType, h.AgentModel = "a1", "adversarial-review", "claude-sonnet-5-5"
		h.AgentDescription, h.AgentPrompt, h.AgentReport = "Adversarial review of the diff", prompt, report
	})}); err != nil {
		a.t.Fatal(err)
	}
}

// ownReviews are the session's own reviews kept: run, reason, prompt, body.
func (a *apEnv) ownReviews() []string {
	a.t.Helper()
	rows, err := a.s.db.R.Query(`SELECT coalesce(run_id, 0) != 0, reason, prompt, body, read_state FROM work_log WHERE kind = ? ORDER BY id`, WorkOwnReview)
	if err != nil {
		a.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var run bool
		var reason, prompt, body, read string
		if err := rows.Scan(&run, &reason, &prompt, &body, &read); err != nil {
			a.t.Fatal(err)
		}
		r := "no run"
		if run {
			r = "run"
		}
		out = append(out, strings.Join([]string{r, reason, prompt, body, read}, " | "))
	}
	return out
}

func TestOwnReviewRecorded(t *testing.T) {
	a, _, key := introEnv(t)
	if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{hook("box", a.cli, "SubagentStop", func(h *HookEvent) { h.AgentType = "Explore" })}); err != nil {
		t.Fatal(err)
	}
	if got := a.ownReviews(); len(got) != 0 {
		t.Fatalf("explore kept %q", got)
	}
	a.ownReviewDone("1. x.go:12 Save writes to a nil map.")
	got := a.ownReviews()
	if len(got) != 1 || got[0] != "run | adversarial-review (claude-sonnet-5-5): Adversarial review of the diff | Attack the diff. | 1. x.go:12 Save writes to a nil map. | pending" {
		t.Fatalf("own reviews = %q", got)
	}
	wl, err := a.s.GetWorkLog(a.ctx, a.admin, key)
	if err != nil {
		t.Fatal(err)
	}
	var e *WorkLogEntry
	for _, x := range wl.Entries {
		if x.Kind == WorkOwnReview {
			e = x
		}
	}
	if e == nil || e.Title != "Own review" || e.Body != "1. x.go:12 Save writes to a nil map." || wl.Summary.OwnReviews != 1 {
		t.Fatalf("work log = %+v, %+v", wl.Summary, e)
	}
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET read_state = 'done', found = 1, fixed = 0, rejected = 0 WHERE kind = ?`, WorkOwnReview); err != nil {
		t.Fatal(err)
	}
	wl, _ = a.s.GetWorkLog(a.ctx, a.admin, key)
	if wl.Summary.OwnFound != 1 {
		t.Fatalf("summary = %+v", wl.Summary)
	}

	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other"})
	if _, err := a.s.AddWorkNote(a.ctx, a.ag, other.Key, WorkNoteInput{Body: "x", Tag: TagFound, Review: true}); !IsInvalid(err) {
		t.Fatalf("tagged review: %v", err)
	}
	if _, err := a.s.AddWorkNote(a.ctx, a.ag, other.Key, WorkNoteInput{Body: " ", Review: true}); !IsInvalid(err) {
		t.Fatalf("empty review: %v", err)
	}
	long := strings.Repeat("a", maxWorkNote+1)
	if _, err := a.s.AddWorkNote(a.ctx, a.ag, other.Key, WorkNoteInput{Body: long}); !IsInvalid(err) {
		t.Fatalf("long note: %v", err)
	}
	e, err = a.s.AddWorkNote(a.ctx, a.ag, other.Key, WorkNoteInput{Body: long, Review: true})
	if err != nil || e.Kind != WorkOwnReview || e.Reason != ownReviewAttached {
		t.Fatalf("attached = %+v, %v", e, err)
	}
	if got := a.ownReviews(); len(got) != 2 || !strings.HasPrefix(got[1], "no run | "+ownReviewAttached) {
		t.Fatalf("own reviews = %q", got)
	}
}

// With no task under way in the session, a review is attached to nothing.
func TestOwnReviewNoTask(t *testing.T) {
	a, _, _ := firstEnv(t)
	a.stopWith("hello")
	a.ownReviewDone("Found nothing.")
	if got := a.ownReviews(); len(got) != 0 {
		t.Fatalf("own reviews = %q", got)
	}
}

// The session's own review is the task's round: wp ship and --pr record a
// round as run by it, a round still pending becomes it, and the pull
// request waits for the session's report of what the review found
// (WP-1320); --review still asks for a round of its own.
func TestShipWithOwnReview(t *testing.T) {
	t.Run("the round, held for its report", func(t *testing.T) {
		a, g, key := introEnv(t)
		a.ownReviewDone("No issues found.")
		for range 2 {
			res, err := a.ship(key, ShipPR)
			if err != nil || res.Pull != nil || !strings.Contains(res.Held, "wp audit report "+key) || res.Notice != "" || len(g.created) != 0 {
				t.Fatalf("pr = %+v, %v", res, err)
			}
		}
		if got := a.rounds(); len(got) != 1 || got[0] != RoundDone {
			t.Fatalf("rounds = %q", got)
		}
		id := a.newestRoundID()
		if note := a.roundNote(id); note != ownReviewNote {
			t.Fatalf("audit note = %q", note)
		}
		if body := a.body(id); !strings.Contains(body, "its reviewer's report is in the work log") {
			t.Fatalf("body = %q", body)
		}
		if rows := a.triageRows(); len(rows) != 1 || rows[0].action != triageReview || rows[0].why != triageWhyOwnReview {
			t.Fatalf("triage = %+v", rows)
		}
		rep, err := a.reportNone(key, "The adversarial-review agent read the whole diff against main; nothing stood.")
		if err != nil || rep.Round != id || len(rep.Audit.Findings) != 0 {
			t.Fatalf("report = %+v, %v", rep, err)
		}
		if _, err := a.s.db.W.Exec(`UPDATE work_log SET read_state = 'done', found = 0, fixed = 0, rejected = 0 WHERE kind = ?`, WorkOwnReview); err != nil {
			t.Fatal(err)
		}
		res, err := a.ship(key, ShipPR)
		if err != nil || res.Pull == nil || res.Held != "" || len(g.created) != 1 {
			t.Fatalf("pr after the report = %+v, %v", res, err)
		}
		if got := a.roundCounts(id); got != "0/0/0" {
			t.Fatalf("round counts = %s", got)
		}
	})
	t.Run("a pending round becomes it", func(t *testing.T) {
		a, g, key := introEnv(t)
		if res, err := a.shipStarted(key, ShipAuto); err != nil || res.Held == "" || len(g.created) != 0 {
			t.Fatalf("auto = %+v, %v", res, err)
		}
		pending := a.newestRoundID()
		a.ownReviewDone("1. a bug")
		res, err := a.ship(key, ShipAuto)
		if err != nil || res.Pull != nil || !strings.Contains(res.Held, "wp audit report") || len(g.created) != 0 {
			t.Fatalf("auto after own = %+v, %v", res, err)
		}
		if got := a.rounds(); len(got) != 1 || got[0] != RoundDone || a.newestRoundID() != pending {
			t.Fatalf("rounds = %q", got)
		}
		ra, _ := a.roundAuditRow(pending)
		if note := a.roundNote(pending); ra.id == 0 || !strings.HasPrefix(note, roundAuditOutwaited) || !strings.Contains(note, "audit #"+itoa64(ra.id)) {
			t.Fatalf("audit note = %q, audit %d", note, ra.id)
		}
	})
	t.Run("a pending round whose audit ended becomes it", func(t *testing.T) {
		a, g, key := introEnv(t)
		if res, err := a.shipStarted(key, ShipAuto); err != nil || res.Held == "" || len(g.created) != 0 {
			t.Fatalf("auto = %+v, %v", res, err)
		}
		pending := a.newestRoundID()
		ra, _ := a.roundAuditRow(pending)
		if ra.id == 0 {
			t.Fatal("the first ship asked for no audit")
		}
		if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'failed' WHERE id = ?`, ra.id); err != nil {
			t.Fatal(err)
		}
		a.ownReviewDone("1. a bug")
		if res, err := a.ship(key, ShipAuto); err != nil || res.Pull != nil || !strings.Contains(res.Held, "wp audit report") {
			t.Fatalf("auto after own = %+v, %v", res, err)
		}
		if note := a.roundNote(pending); note != ownReviewNote {
			t.Fatalf("audit note = %q, want %q", note, ownReviewNote)
		}
	})
	t.Run("review still asks", func(t *testing.T) {
		a, g, key := introEnv(t)
		a.ownReviewDone("No issues found.")
		res, err := a.ship(key, ShipReview)
		if err != nil || !strings.Contains(res.Held, "end your turn") || len(g.created) != 0 {
			t.Fatalf("review = %+v, %v", res, err)
		}
		if res, err := a.ship(key, ShipPR); err != nil || res.Held == "" || len(g.created) != 0 {
			t.Fatalf("pr after review = %+v, %v", res, err)
		}
		if got := a.rounds(); len(got) != 1 || got[0] != RoundPending {
			t.Fatalf("rounds = %q", got)
		}
	})
}

// A session holding two tasks: a review goes to the one its text names,
// and to neither when it names none, so no task ships on a review of
// another (and the other still asks for its round).
func TestOwnReviewTwoTasks(t *testing.T) {
	a, g, first := introEnv(t)
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.ownReviewAsked("Attack the diff.", "No issues found.")
	if got := a.ownReviews(); len(got) != 0 {
		t.Fatalf("unnamed review kept: %q", got)
	}
	a.ownReviewAsked("Attack "+second.Key+"'s change.", "No issues found.")
	var on int64
	if err := a.s.db.R.QueryRow(`SELECT issue_id FROM work_log WHERE kind = ?`, WorkOwnReview).Scan(&on); err != nil || on != second.ID {
		t.Fatalf("on %d (%v), want %d", on, err, second.ID)
	}
	if res, err := a.ship(first, ShipPR); err != nil || res.Pull != nil || res.Held == "" || len(g.created) != 0 {
		t.Fatalf("first shipped on the second's review: %+v, %v", res, err)
	}
	a.ownReviewAsked("Attack "+first+"0 and "+first+"-x", "ok")
	a.ownReviewAsked("Attack "+first+" and "+second.Key, "ok")
	if got := a.ownReviews(); len(got) != 1 {
		t.Fatalf("own reviews = %q", got)
	}
	a.ownReviewAsked("Review "+first+".", "No issues found.")
	if res, err := a.ship(first, ShipPR); err != nil || !strings.Contains(res.Held, "wp audit report "+first) {
		t.Fatalf("first with its review: %+v, %v", res, err)
	}
	if got := a.rounds(); len(got) != 1 || got[0] != RoundDone {
		t.Fatalf("rounds = %q", got)
	}
}

// An own review covers what was committed before it, and what came after
// a review that found issues (its fixes); commits after one that found
// nothing are a change it never saw, so a round reviews them.
func TestOwnReviewCommitsSince(t *testing.T) {
	for _, c := range []struct {
		name   string
		found  string
		after  bool
		stands bool
	}{
		{"clean, nothing since", "0", false, true},
		{"clean, commits since", "0", true, false},
		{"found, commits since", "2", true, true},
		{"not counted yet, commits since", "", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, g, key := introEnv(t)
			a.ownReviewDone("No issues found.")
			if c.found != "" {
				if _, err := a.s.db.W.Exec(`UPDATE work_log SET read_state = 'done', found = ? WHERE kind = ?`, c.found, WorkOwnReview); err != nil {
					t.Fatal(err)
				}
			}
			var ended int64
			if err := a.s.db.R.QueryRow(`SELECT ended_at FROM work_log WHERE kind = ?`, WorkOwnReview).Scan(&ended); err != nil {
				t.Fatal(err)
			}
			head := ended/1000 - 5
			if c.after {
				head = ended/1000 + 5
			}
			res, err := a.ship(key, ShipPR, func(in *OpenPRInput) { in.HeadTime = head })
			if err != nil {
				t.Fatal(err)
			}
			if c.stands {
				if res.Pull != nil || !strings.Contains(res.Held, "wp audit report") || len(g.created) != 0 {
					t.Fatalf("own review didn't stand: %+v", res)
				}
				if got := a.rounds(); len(got) != 1 || got[0] != RoundDone {
					t.Fatalf("rounds = %q", got)
				}
				return
			}
			if res.Pull != nil || res.Held == "" || len(g.created) != 0 || !strings.Contains(res.Notice, "committed since") {
				t.Fatalf("stale own review stood: %+v", res)
			}
			if got := a.rounds(); len(got) != 1 || got[0] != RoundPending {
				t.Fatalf("rounds = %q", got)
			}
		})
	}
}

// A report the agent attached by hand is the change's round as the
// hook's is: the round says how it came, and the pull request waits for
// the findings all the same.
func TestShipWithAttachedReview(t *testing.T) {
	a, g, key := introEnv(t)
	if _, err := a.s.AddWorkNote(a.ctx, a.ag, key, WorkNoteInput{Body: "ok", Review: true}); err != nil {
		t.Fatal(err)
	}
	res, err := a.ship(key, ShipPR)
	if err != nil || res.Pull != nil || !strings.Contains(res.Held, "wp audit report") || len(g.created) != 0 {
		t.Fatalf("pr = %+v, %v", res, err)
	}
	if got := a.rounds(); len(got) != 1 || got[0] != RoundDone {
		t.Fatalf("rounds = %q", got)
	}
	if body := a.body(a.newestRoundID()); !strings.Contains(body, "the agent attached its report by hand") {
		t.Fatalf("body = %q", body)
	}
	if rows := a.triageRows(); len(rows) != 1 || rows[0].action != triageReview || rows[0].why != triageWhyOwnReview {
		t.Fatalf("triage = %+v", rows)
	}
}

// The hook's outbox replays an event whose POST failed: the same review
// is kept once.
func TestOwnReviewReplayed(t *testing.T) {
	a, _, _ := introEnv(t)
	at := a.clockNow().UnixMilli()
	ev := hook("box", a.cli, "SubagentStop", func(h *HookEvent) {
		h.At, h.AgentID, h.AgentType, h.AgentReport = at, "a1", "adversarial-review", "No issues found."
	})
	for range 2 {
		if _, err := a.s.SessionHook(a.ctx, a.ag, []HookEvent{ev}); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.ownReviews(); len(got) != 1 {
		t.Fatalf("own reviews = %q", got)
	}
}

// With no run open, a review goes to the task whose run ended last only
// when it names that task or no task at all.
func TestOwnReviewEndedRun(t *testing.T) {
	a, _, key := introEnv(t)
	other := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Other"})
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET ended_at = ?`, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a.ownReviewAsked("Attack "+other.Key+"'s change.", "No issues found.")
	if got := a.ownReviews(); len(got) != 0 {
		t.Fatalf("review of %s kept on %s: %q", other.Key, key, got)
	}
	a.ownReviewAsked("Attack the diff.", "No issues found.")
	a.ownReviewAsked("Attack "+key+".", "Found nothing.")
	if got := a.ownReviews(); len(got) != 2 {
		t.Fatalf("own reviews = %q", got)
	}
}
