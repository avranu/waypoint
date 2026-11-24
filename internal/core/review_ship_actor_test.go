package core

import (
	"strings"
	"testing"
	"time"
)

// reviewedUntilShip runs the review of a held ship to its audit's end, and
// stops before Waypoint's tick that opens the pull request, so a test can
// change the world first.
func (e *loopEnv) reviewedUntilShip() {
	e.t.Helper()
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		e.t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	e.tick(61 * time.Second)
	e.endAudit(e.box, "done")
}

func (e *loopEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.W.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// notStopped: the server finishes the ship, then reports what opened.
func (e *loopEnv) finishShip() {
	e.t.Helper()
	e.tick(time.Second)
	e.s.Wait()
}

func (e *loopEnv) noticeCount(like string) (n int) {
	e.t.Helper()
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE title LIKE ?`, like).Scan(&n)
	return n
}

// H: the server ship acts with the authority the session's ship had and
// still has. When any of it is gone it opens nothing, ends the round with
// the reason, and tells a person.
func TestServerShipStopsWhenAuthorityIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(e *loopEnv)
	}{
		{"the person was deactivated", func(e *loopEnv) {
			e.exec(`UPDATE users SET disabled = 1 WHERE id = (SELECT user_id FROM agent_sessions WHERE id = ?)`, e.ag.Session)
		}},
		// With no token on the ship, only the Disabled check stands between
		// the person and the open; the token's own constructor refuses a
		// disabled user in the case above.
		{"the person was deactivated and the ship had no token", func(e *loopEnv) {
			e.exec(`UPDATE work_log SET before_pr = json_remove(before_pr, '$.token_id') WHERE kind = 'review'`)
			e.exec(`UPDATE users SET disabled = 1 WHERE id = (SELECT user_id FROM agent_sessions WHERE id = ?)`, e.ag.Session)
		}},
		{"the person can only read the project now", func(e *loopEnv) {
			who := `(SELECT user_id FROM agent_sessions WHERE id = ?)`
			e.exec(`UPDATE users SET is_admin = 0 WHERE id = `+who, e.ag.Session)
			e.exec(`INSERT OR REPLACE INTO project_members (project_id, user_id, role, created_at) VALUES (?, `+who+`, 'viewer', 0)`, e.p.ID, e.ag.Session)
		}},
		{"another agent holds the task now", func(e *loopEnv) {
			e.exec(`UPDATE issues SET claimed_by = 'other/agent' WHERE id = ?`, e.task.ID)
		}},
		{"the task is no longer meant for agents", func(e *loopEnv) {
			e.exec(`UPDATE issues SET claimed_by = '', agent = ? WHERE id = ?`, AgentHumans, e.task.ID)
		}},
		{"the task moved to another project", func(e *loopEnv) {
			name := "Elsewhere"
			q, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &name})
			if err != nil {
				e.t.Fatal(err)
			}
			e.exec(`UPDATE issues SET project_id = ? WHERE id = ?`, q.ID, e.task.ID)
		}},
		{"a person closed the task", func(e *loopEnv) {
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, e.task.Key, IssuePatch{Status: ptrOf("Done")}); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"the project was archived", func(e *loopEnv) {
			e.exec(`UPDATE projects SET archived = 1 WHERE key = ?`, e.p.Key)
		}},
		{"the token it shipped with was revoked", func(e *loopEnv) {
			e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.token_id', 987654) WHERE kind = 'review'`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			e.reviewedUntilShip()
			// When the person who shipped can't be told, the notice goes to the issue's reporter.
			reporter := e.user("pat", true)
			e.exec(`UPDATE issues SET reporter_id = ? WHERE id = ?`, reporter.User.ID, e.task.ID)
			tc.do(e)
			e.finishShip()
			if len(e.g.created) != 0 {
				t.Fatalf("Waypoint opened %d pull requests", len(e.g.created))
			}
			var body string
			_ = e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&body)
			if !strings.Contains(body, "did not open the pull request") {
				t.Errorf("the round says %q", body)
			}
			if strings.Contains(tc.name, "no token") && !strings.Contains(body, "deactivated") {
				t.Errorf("the round says %q, not that the person is deactivated", body)
			}
			e.tickAll(time.Hour)
			if n := e.noticeCount("%pull request not opened"); n != 1 {
				t.Errorf("told %d times, want once", n)
			}
		})
	}
}

// The deactivated person can't hear it: the task's reporter does.
func TestServerShipStoppedTellsTheReporterWhenThePersonIsGone(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.reviewedUntilShip()
	reporter := e.user("pat", true)
	e.exec(`UPDATE issues SET reporter_id = ? WHERE id = ?`, reporter.User.ID, e.task.ID)
	e.exec(`UPDATE users SET disabled = 1 WHERE id = (SELECT user_id FROM agent_sessions WHERE id = ?)`, e.ag.Session)
	e.finishShip()
	if len(e.g.created) != 0 || e.noticeCount("%pull request not opened") == 0 {
		t.Fatalf("opened %d, told %d", len(e.g.created), e.noticeCount("%pull request not opened"))
	}
}

// The same ship, nothing changed: it opens, as before.
func TestServerShipStillOpensWhenNothingChanged(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.reviewedUntilShip()
	e.finishShip()
	if len(e.g.created) != 1 {
		t.Fatalf("Waypoint opened %d pull requests", len(e.g.created))
	}
}

// H6: a session of another agent is shipped as that agent.
func TestServerShipAgentIsTheSessions(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.exec(`UPDATE agent_sessions SET agent = 'acme/other-cli' WHERE id = ?`, e.ag.Session)
	r := &roundRow{}
	if got := e.s.shipAgent(e.ctx, r, "", e.ag.Session); got != "acme/other-cli" {
		t.Errorf("shipped as %q", got)
	}
	if got := e.s.shipAgent(e.ctx, r, "x/y", e.ag.Session); got != "x/y" {
		t.Errorf("the given agent: %q", got)
	}
	if got := e.s.shipAgent(e.ctx, r, "", 0); got != "anthropic/claude-code" {
		t.Errorf("no session: %q", got)
	}
}

// A server ship acts as the token the ship came with, but doesn't use it up:
// the token's last-used time is the person's, not Waypoint's.
func TestServerShipDoesNotTouchTheTokensLastUsed(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	e.reviewedUntilShip()
	_, tok, err := e.s.CreateAPIToken(e.ctx, e.admin, "ship", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE api_tokens SET last_used_at = 1000 WHERE id = ?`, tok.ID)
	e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.token_id', ?) WHERE kind = 'review'`, tok.ID)
	e.finishShip()
	if len(e.g.created) != 1 {
		t.Fatalf("Waypoint opened %d pull requests", len(e.g.created))
	}
	var used int64
	_ = e.s.db.R.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id = ?`, tok.ID).Scan(&used)
	if used != 1000 {
		t.Errorf("the server's ship set the token's last use to %d", used)
	}
}
