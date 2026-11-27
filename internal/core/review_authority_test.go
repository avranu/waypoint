package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The server ship needs what the API route needs: pulls.write (WP-1370).
func TestServerShipNeedsPullsWrite(t *testing.T) {
	t.Run("core refuses a token without it", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		ag := *e.ag
		ag.Permissions = permissionSet([]Permission{PermPullsRead})
		_, err := e.s.OpenPullRequest(e.ctx, &ag, OpenPRInput{Project: e.p.Key, Repo: "acme/app", Head: "wp-1-first", Title: "First", Issue: e.task.Key, For: e.task.Key, Ready: true, Review: ShipSkip, Reason: "test"})
		if !errors.Is(err, ErrForbidden) || len(e.g.created) != 0 {
			t.Fatalf("err = %v, opened %d", err, len(e.g.created))
		}
	})
	t.Run("the server ship stops on a ship's token without it", func(t *testing.T) {
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		e.reviewedUntilShip()
		_, tok, err := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "ship", Permissions: []Permission{PermPullsRead}})
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.token_id', ?) WHERE kind = 'review'`, tok.ID)
		e.finishShip()
		var body string
		if err := e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&body); err != nil {
			t.Fatalf("read the state: %v", err)
		}
		if len(e.g.created) != 0 || !strings.Contains(body, "did not open the pull request") || !strings.Contains(body, "can't open pull requests (it lacks pulls.write)") {
			t.Fatalf("opened %d, round says %q", len(e.g.created), body)
		}
	})
}

// Authority lost while the forge call is out: the pull request exists on the
// forge, so it is recorded, but unarmed and unwatched, the task is left as
// it is (a closed task is never reopened), the round says so and the person
// is told once.
func TestServerShipAuthorityLostDuringTheForgeCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(e *loopEnv)
	}{
		{"deactivated", func(e *loopEnv) {
			e.exec(`UPDATE users SET disabled = 1 WHERE id = (SELECT user_id FROM agent_sessions WHERE id = ?)`, e.ag.Session)
		}},
		{"token deleted", func(e *loopEnv) {
			var id int64
			if err := e.s.db.R.QueryRow(`SELECT json_extract(before_pr, '$.token_id') FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if err := e.s.RevokeAPIToken(e.ctx, e.admin, id); err != nil {
				e.t.Error(err)
			}
		}},
		{"token expired", func(e *loopEnv) {
			e.exec(`UPDATE api_tokens SET expires_at = 1 WHERE id = (SELECT json_extract(before_pr, '$.token_id') FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1)`)
		}},
		{"token made read-only", func(e *loopEnv) {
			e.exec(`UPDATE api_tokens SET read_only = 1 WHERE id = (SELECT json_extract(before_pr, '$.token_id') FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1)`)
		}},
		{"demoted to viewer", func(e *loopEnv) {
			who := `(SELECT user_id FROM agent_sessions WHERE id = ?)`
			e.exec(`UPDATE users SET is_admin = 0 WHERE id = `+who, e.ag.Session)
			e.exec(`INSERT OR REPLACE INTO project_members (project_id, user_id, role, created_at) VALUES (?, `+who+`, 'viewer', 0)`, e.p.ID, e.ag.Session)
		}},
		{"project archived", func(e *loopEnv) { e.exec(`UPDATE projects SET archived = 1 WHERE key = ?`, e.p.Key) }},
		{"task closed", func(e *loopEnv) {
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, e.task.Key, IssuePatch{Status: ptrOf("Done")}); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			e.reviewedUntilShip()
			if strings.HasPrefix(tc.name, "token ") {
				_, tok, err := e.s.CreateAPIToken(e.ctx, e.admin, "ship", false, 0)
				if err != nil {
					t.Fatal(err)
				}
				e.exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.token_id', ?) WHERE kind = 'review'`, tok.ID)
			}
			reporter := e.user("pat", true)
			e.exec(`UPDATE issues SET reporter_id = ? WHERE id = ?`, reporter.User.ID, e.task.ID)
			e.g.afterCreate = func(ForgePR) error { tc.do(e); return nil }
			e.finishShip()
			e.tickAll(time.Hour)
			e.s.Wait()
			if len(e.g.created) != 1 {
				t.Fatalf("opened %d", len(e.g.created))
			}
			var armed, babysits, linked int
			if err := e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests WHERE armed_at IS NOT NULL`).Scan(&armed); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if err := e.s.db.R.QueryRow(`SELECT count(*) FROM babysits`).Scan(&babysits); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if err := e.s.db.R.QueryRow(`SELECT count(*) FROM pull_requests`).Scan(&linked); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if armed != 0 || babysits != 0 || linked != 1 {
				t.Errorf("armed %d, babysat %d, recorded %d: want the pull request recorded, unarmed and unwatched", armed, babysits, linked)
			}
			var status string
			if err := e.s.db.R.QueryRow(`SELECT s.name FROM issues i JOIN statuses s ON s.id = i.status_id WHERE i.id = ?`, e.task.ID).Scan(&status); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if tc.name == "task closed" && status != "Done" {
				t.Errorf("a closed task is now %q", status)
			}
			var body string
			if err := e.s.db.R.QueryRow(`SELECT body FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&body); err != nil {
				t.Fatalf("read the state: %v", err)
			}
			if !strings.Contains(body, "opened") || !strings.Contains(body, "not armed") {
				t.Errorf("the round says %q", body)
			}
			if tc.name == "deactivated" && !strings.Contains(body, "is deactivated") {
				t.Errorf("the round says %q, not that its person was deactivated", body)
			}
			if n := e.noticeCount("%pull request opened, then stopped"); n != 1 {
				t.Errorf("the person was told %d times", n)
			}
		})
	}
}
