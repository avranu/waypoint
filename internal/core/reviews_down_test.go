package core

import (
	"testing"
)

func TestReviewsDownState(t *testing.T) {
	for _, c := range []struct {
		name string
		down []bool
		want string
	}{
		{"too few", []bool{true, true}, ""},
		{"first streak, nothing before", []bool{true, true, true}, "down"},
		{"reaches the count", []bool{true, true, true, false}, "down"},
		{"past the count: told already", []bool{true, true, true, true}, ""},
		{"answers after a streak", []bool{false, true, true, true}, "up"},
		{"answers after a short run", []bool{false, true, true, false}, ""},
		{"all fine", []bool{false, false, false, false}, ""},
	} {
		if got := reviewsDownState(c.down); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Three reviews in a row that end with every model unavailable tell the
// admins once; a later review that answers tells them again (WP-214).
func TestReviewsDownNotifies(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pe.pullHook("opened", ForgePR{Number: 7, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "aaaaaaa1111", State: "open"})
	pe.drain() // the hook's own review answers: not part of a streak
	var prID int64
	if err := pe.s.db.R.QueryRow(`SELECT id FROM pull_requests WHERE number = 7`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	end := func(status string, err error) {
		t.Helper()
		res, xerr := pe.s.db.W.Exec(`INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?,?,'review','aaaaaaa1111','running',?)`,
			pe.p.ID, prID, pe.s.nowMS())
		if xerr != nil {
			t.Fatal(xerr)
		}
		id, _ := res.LastInsertId()
		errText := ""
		if err != nil {
			errText = runErrorText(err, []ModelAttempt{{Outcome: "rate_limit"}})
		}
		if err := pe.s.finishRun(pe.ctx, id, status, "", errText, nil, ""); err != nil {
			t.Fatal(err)
		}
		pe.s.Wait()
	}
	told := func(title string) int {
		var n int
		_ = pe.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE user_id = ? AND kind = ? AND title LIKE ?`,
			pe.admin.User.ID, NotifyPR, title+"%").Scan(&n)
		return n
	}
	down := &errProvidersDown{}
	end("failed", down)
	end("failed", down)
	if n := told("Reviews in"); n != 0 {
		t.Fatalf("two failures told %d", n)
	}
	end("failed", down)
	if n := told("Reviews in " + pe.p.Key + " keep failing"); n != 1 {
		t.Fatalf("three failures in a row: told %d, want 1", n)
	}
	end("failed", down)
	end("failed", errAllModelsFailed)
	end("failed", down)
	end("failed", down)
	end("failed", down)
	if n := told("Reviews in " + pe.p.Key + " keep failing"); n != 1 {
		t.Fatalf("more failures told again: %d", n)
	}
	end("failed", errAllModelsFailed)
	end("done", nil)
	if n := told("Reviews in " + pe.p.Key + " answer again"); n != 1 {
		t.Fatalf("recovery: told %d, want 1", n)
	}
}
