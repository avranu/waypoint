package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A change's stakes tier, from triage's facts (WP-1231, WP-1248).
func TestChangeStakes(t *testing.T) {
	for _, c := range []struct {
		f    TriageFeatures
		tier string
		why  string
	}{
		{TriageFeatures{Files: 3, Added: 300, Deleted: 100}, StakesNormal, ""},
		{TriageFeatures{Provenance: RiskMedium}, StakesNormal, ""},
		{TriageFeatures{Files: 30, Added: 900}, StakesElevated, "it changes 900 lines"},
		{TriageFeatures{Files: 30, Added: 900, Deleted: 101}, StakesHigh, "it changes 1001 lines"},
		{TriageFeatures{NoFacts: true}, StakesUnknown, "no facts"},
		{TriageFeatures{Added: 10, RiskErr: true}, StakesElevated, "risk couldn't be read"},
		{TriageFeatures{Provenance: RiskHigh}, StakesHigh, "risk is high"},
		{TriageFeatures{Sensitive: []string{"internal/db/migrations/0001.sql"}}, StakesHigh, "deployment paths (internal/db/migrations/0001.sql)"},
		{TriageFeatures{Sensitive: []string{"internal/auth/login.go"}}, StakesHigh, "internal/auth/login.go"},
		{TriageFeatures{Sensitive: []string{".forgejo/workflows/ci.yml"}}, StakesHigh, "workflows"},
		{TriageFeatures{Sensitive: []string{"internal/auth/login_test.go"}}, StakesElevated, "sensitive paths"},
		{TriageFeatures{Sensitive: []string{"internal/core/hooks.go", "go.mod"}}, StakesElevated, "sensitive paths (internal/core/hooks.go, go.mod)"},
		{TriageFeatures{RiskyFiles: 2, Added: 100}, StakesElevated, "2 files with past bug fixes"},
		{TriageFeatures{RiskyFiles: 2, Added: 300, Deleted: 200}, StakesHigh, "500 changed lines touch 2 files"},
	} {
		tier, why := changeStakes(&c.f)
		if tier != c.tier || !strings.Contains(why, c.why) || (c.why == "") != (why == "") {
			t.Errorf("changeStakes(%+v) = %s %q, want %s %q", c.f, tier, why, c.tier, c.why)
		}
	}
	if tierBar(StakesHigh) != trustedAuditBar || tierBar(StakesElevated) != elevatedAuditBar || tierBar(StakesNormal) != reviewFloorBar || tierBar(StakesUnknown) != reviewFloorBar {
		t.Error("bars: high Grok 4.7 or better, elevated Haiku or better, else the floor")
	}
	r := roundStakes{tier: StakesElevated, bar: elevatedAuditBar}
	if r.barFor(reviewAuditKey) != elevatedAuditBar || r.barFor(reviewOtherAuditKey) != reviewFloorBar || r.reviewer().Model != "haiku" {
		t.Errorf("elevated: %q %q %v", r.barFor(reviewAuditKey), r.barFor(reviewOtherAuditKey), r.reviewer())
	}
	r = roundStakes{tier: StakesHigh, bar: trustedAuditBar}
	if r.barFor(reviewOtherAuditKey) != trustedAuditBar || r.reviewer().Model != "sonnet" || r.reviewerName() != "Sonnet review" {
		t.Errorf("high: %q %v", r.barFor(reviewOtherAuditKey), r.reviewer())
	}
	if (roundStakes{}).reviewer().Model != "sonnet" || (roundStakes{tier: StakesNormal}).reviewer().Model != "haiku" {
		t.Error("unknown stakes get Sonnet, normal ones Haiku")
	}
}

// Many loose sensitive names can't hide a path that makes a change
// high-stakes past triage's first ten (WP-1250).
func TestStakesPathsFirst(t *testing.T) {
	var numstat strings.Builder
	for i := range 12 {
		fmt.Fprintf(&numstat, "1\t1\tinternal/hooks/h%d.go\n", i)
	}
	numstat.WriteString("1\t1\tinternal/db/migrations/0200_x.sql\n")
	f := triageFeatures(&ChangeFacts{Numstat: numstat.String()})
	if len(f.Sensitive) != 10 || f.Sensitive[0] != "internal/db/migrations/0200_x.sql" {
		t.Fatalf("sensitive = %v", f.Sensitive)
	}
	if tier, why := changeStakes(f); tier != StakesHigh || !strings.Contains(why, "migrations/0200_x.sql") {
		t.Errorf("stakes = %s %q", tier, why)
	}
}

// Rounds by stakes (WP-1231): an elevated change's audit that finds no model
// at the bar is asked again at no bar rather than going to Claude, and a
// slow audit of a change that isn't high-stakes gets a Haiku review beside
// it (WP-1306), the audit going on as the pull request's.
func TestReviewRoundStakes(t *testing.T) {
	start := func(a *apEnv, features string) int64 {
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
		a.finish(first.Key)
		if _, ok := a.s.Forges.(*prForge); ok {
			a.grokPR(first.ID, 7, "aaa")
		} else {
			a.linkPR(first.ID, 7, "aaa")
		}
		a.stopWith("Shipped as #7.")
		st := a.step()
		if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET model = 'claude-opus-5-5' WHERE id = ?`, st.sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.s.db.W.Exec(`INSERT INTO review_triage(round_id, issue_id, project_id, repo, head, rule, action, features, created_at)
			VALUES (?, ?, ?, 'acme/app', 'aaa', 'floor:sensitive_path', 'review', ?, 0)`, st.reviewID.Int64, first.ID, a.p.ID, features); err != nil {
			t.Fatal(err)
		}
		return st.reviewID.Int64
	}

	t.Run("elevated asks again at the floor", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a, `{"files":1,"added":3,"sensitive":["internal/core/hooks.go"]}`)
		a.tick(61 * time.Second)
		first, au := a.roundAuditRow(id)
		if au == nil || au.Weakest != elevatedAuditBar {
			t.Fatalf("first audit = %+v", au)
		}
		if _, err := a.s.db.W.Exec(`UPDATE review_triage SET features = '{"files":90,"added":5000}' WHERE round_id = ?`, id); err != nil {
			t.Fatal(err)
		}
		var rule, tier, bar, source string
		if err := a.s.db.R.QueryRow(`SELECT rule, tier, bar, source FROM review_stakes WHERE round_id = ?`, id).Scan(&rule, &tier, &bar, &source); err != nil ||
			rule != StakesRule || tier != StakesElevated || bar != elevatedAuditBar || source != "rule" {
			t.Fatalf("stored = %s %s %s %s (%v)", rule, tier, bar, source, err)
		}
		if r, err := a.s.roundStakes(a.ctx, id); err != nil || r.tier != StakesElevated {
			t.Fatalf("stakes = %+v (%v)", r, err)
		}
		a.endAudit(box, "failed")
		a.tick(time.Second)
		ra, au := a.roundAuditRow(id)
		if ra.id == first.id || ra.alt == "" || ra.note != "" || au == nil || au.Weakest != reviewFloorBar {
			t.Fatalf("round = %+v, audit = %+v", ra, au)
		}
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv != nil {
			t.Errorf("a Sonnet review started: %+v", rv)
		}
		a.endAudit(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		if len(a.jobs) != 1 || !strings.Contains(string(a.jobs[0].Input), "Merge reads a nil map") {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		var issueID int64
		if err := a.s.db.R.QueryRow(`SELECT issue_id FROM work_log WHERE id = ?`, id).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		res, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, state, created_at, updated_at) VALUES (?, 'review', 'waiting', 0, 0)`, issueID)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := res.LastInsertId()
		if r, err := a.s.roundStakes(a.ctx, again); err != nil || r.tier != StakesElevated || r.bar != elevatedAuditBar || !strings.Contains(r.why, "sensitive paths") {
			t.Fatalf("second round's stakes = %+v (%v)", r, err)
		}
		if err := a.s.db.R.QueryRow(`SELECT source FROM review_stakes WHERE round_id = ?`, again).Scan(&source); err != nil || source != "inherited" {
			t.Errorf("source = %q (%v)", source, err)
		}
	})

	slowReviewed := func(t *testing.T, a *apEnv, box *Runner, id int64) *RunnerClaim {
		t.Helper()
		a.tick(61 * time.Second)
		if d := a.step().detail; !strings.Contains(d, "a Haiku review in a new session starts beside it") {
			t.Errorf("the wait doesn't say what happens: %q", d)
		}
		for range 7 {
			a.tick(10 * time.Minute)
		}
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv == nil {
			t.Fatal("no Claude review started beside the slow audit")
		}
		a.s.Wait()
		w := a.poll(a.admin, box, RunnerPoll{})
		if w.Job == nil || agentJobOf(t, w.Job).CLI != GatewayCLI {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		a.endReviewerAs(box, "haiku", "done", "Merge reads a nil map")
		a.tick(time.Second)
		if err := a.s.write(a.ctx, SystemActor(), func(tx *tx) error {
			_, err := a.s.startRound(a.ctx, tx, id, a.clockNow().UnixMilli())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return w.Job
	}

	t.Run("a handed-over audit that fails: the pull request asks for its own", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a, `{"files":2,"added":20}`)
		job := slowReviewed(t, a, box, id)
		a.s.handOverRoundAudits(a.ctx)
		_, au := a.roundAuditRow(id)
		if au == nil || au.PRID == nil {
			t.Fatalf("not handed over: %+v", au)
		}
		p, err := a.s.GetProject(a.ctx, a.admin, a.p.Key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET author = ? WHERE id = ?`, a.admin.User.Username, *au.PRID); err != nil {
			t.Fatal(err)
		}
		st := p.Settings
		st.AuditPRs = true
		if _, err := a.s.UpdateProject(a.ctx, a.admin, a.p.Key, ProjectInput{Settings: &st}); err != nil {
			t.Fatal(err)
		}
		prAudits := func() int {
			a.s.Wait()
			var n int
			if err := a.s.db.R.QueryRow(`SELECT count(*) FROM code_audits WHERE pr_id = ? AND purpose = ''`, *au.PRID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := prAudits(); n != 1 {
			t.Fatalf("while it runs the round's audit is the pull request's: %d audits", n)
		}
		a.finishAudit(job, "failed")
		if n := prAudits(); n != 2 {
			t.Fatalf("the pull request didn't ask for its own audit: %d audits", n)
		}
		a.endAudit(box, "failed")
		if n := prAudits(); n != 2 {
			t.Fatalf("the pull request's own failed audit asked again: %d audits", n)
		}
		if pr, err := a.s.prByID(a.ctx, a.s.db.R, *au.PRID); err != nil || !a.s.headAudited(a.ctx, pr) {
			t.Fatalf("the pull request's own failed audit doesn't count (%v)", err)
		}
	})

	t.Run("a normal change's slow audit gets a Haiku review", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a, `{"files":2,"added":20}`)
		slowReviewed(t, a, box, id)
		var in AutopilotJob
		if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || !strings.Contains(in.Review, "Merge reads a nil map") || !strings.Contains(in.Review, "Claude Haiku") {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		ra, au := a.roundAuditRow(id)
		if !strings.HasPrefix(ra.note, roundAuditOutwaited) || !strings.Contains(ra.note, "Haiku review #") || strings.Contains(ra.note, stakesNoClaude) || au == nil || au.Status != AuditRunning {
			t.Fatalf("round = %+v, audit = %+v", ra, au)
		}
		a.s.handOverRoundAudits(a.ctx)
		if _, au := a.roundAuditRow(id); au.Purpose != "" || au.PRID == nil {
			t.Errorf("not handed over: %+v", au)
		}
		stats, err := a.s.GetReviewStakesStats(a.ctx, a.admin, "", 14, true)
		if err != nil {
			t.Fatal(err)
		}
		var normal *StakesTierStats
		for _, tier := range stats.Tiers {
			if tier.Tier == StakesNormal {
				normal = tier
			}
		}
		if normal == nil || normal.Rounds != 1 || normal.SlowHandOvers != 0 || normal.Reviewers != 1 || normal.ReviewersUsed != 1 || normal.OwnReviews != 0 || len(stats.Rows) != 1 {
			t.Errorf("stats = %+v %+v", normal, stats.Rows)
		}
		if normal != nil && (normal.Recomputed != 0 || len(normal.Outcomes) != len(experimentMetrics)-3+len(stakesExtraMetrics) || stats.Rows[0].Rule != StakesRule) {
			t.Errorf("outcomes = %+v, recomputed %d, rule %q", normal.Outcomes, normal.Recomputed, stats.Rows[0].Rule)
		}
		barred := func(weakest string) int {
			t.Helper()
			if _, err := a.s.db.W.Exec(`UPDATE code_audits SET weakest = ? WHERE id = ?`, weakest, au.ID); err != nil {
				t.Fatal(err)
			}
			st, err := a.s.GetReviewStakesStats(a.ctx, a.admin, "", 14, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, tier := range st.Tiers {
				if tier.Tier == StakesNormal {
					if tier.FreeReasks != 0 {
						t.Errorf("weakest %q: %d free re-asks, want 0 with no second ask", weakest, tier.FreeReasks)
					}
					return tier.Barred
				}
			}
			t.Fatal("no normal tier")
			return -1
		}
		for weakest, want := range map[string]int{reviewFloorBar: 0, "": 0, elevatedAuditBar: 1, trustedAuditBar: 1} {
			if got := barred(weakest); got != want {
				t.Errorf("a round whose audit was held to %q: Barred = %d, want %d", weakest, got, want)
			}
		}
		member := *a.admin.User
		member.IsAdmin = false
		stats, err = a.s.GetReviewStakesStats(a.ctx, &Actor{User: &member}, "", 14, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, tier := range stats.Tiers {
			if len(tier.Outcomes) != 0 {
				t.Errorf("a member got outcomes: %+v", tier.Outcomes)
			}
		}
		if stats.OutcomesNote == "" {
			t.Error("no note says why outcomes are missing")
		}
	})

	t.Run("a finished review the round never typed goes to the pull request", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a, `{"files":2,"added":20}`)
		a.tick(61 * time.Second)
		for range 7 {
			a.tick(10 * time.Minute)
		}
		a.s.Wait()
		if w := a.poll(a.admin, box, RunnerPoll{}); w.Job == nil || agentJobOf(t, w.Job).CLI != GatewayCLI {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		a.endReviewerAs(box, "haiku", "done", "Merge reads a nil map")
		a.s.setRoundNow(a.ctx, id, []string{RoundPending}, RoundFailed, "the session's screen was held for over 2h")
		a.s.sweepRounds(a.ctx)
		a.s.handOverRoundAudits(a.ctx)
		rv, err := a.s.roundReviewer(a.ctx, id)
		if err != nil || rv == nil || rv.Status != AuditDone || rv.Purpose != "" || rv.PRID == nil {
			t.Fatalf("the reviewer's findings weren't handed over: %+v (%v)", rv, err)
		}
		if _, au := a.roundAuditRow(id); au == nil || au.Purpose != "" || au.Status == AuditCancelled {
			t.Errorf("the audit wasn't handed over: %+v", au)
		}
		if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, *rv.PRID); err != nil {
			t.Fatal(err)
		}
		a.s.handOverRoundAudits(a.ctx)
		a.s.Wait()
		var fix int64
		if err := a.s.db.R.QueryRow(`SELECT coalesce(fix_issue_id, 0) FROM code_audits WHERE id = ?`, rv.ID).Scan(&fix); err != nil || fix == 0 {
			t.Errorf("no fix task for the merged pull request's review: %d (%v)", fix, err)
		}
	})

	t.Run("a typed review stays the round's", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a, `{"files":2,"added":20}`)
		slowReviewed(t, a, box, id)
		a.s.setRoundNow(a.ctx, id, []string{RoundRunning}, RoundInterrupted, "the session ended during the review")
		if r := a.round(id); r.state != RoundInterrupted {
			t.Fatalf("round state = %s", r.state)
		}
		a.s.handOverRoundAudits(a.ctx)
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv == nil || rv.Purpose == "" {
			t.Errorf("a review the session had went to the pull request: %+v", rv)
		}
	})

	t.Run("before the pull request the session opens it", func(t *testing.T) {
		if p := handOverPrompt(12); !strings.Contains(p, "Run `wp ship`") || !strings.Contains(p, "Audit #12") {
			t.Errorf("prompt = %q", p)
		}
	})
}
