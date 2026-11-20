package core

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// restNotices are the owner's notices that #7's babysit has rested.
func (be *bsEnv) restNotices() []string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT body FROM notifications WHERE title LIKE ? ORDER BY id`, "%"+restTitle)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

// handoffKeyRE finds the task a protected-file handoff's wait names.
var handoffKeyRE = regexp.MustCompile(`; ([A-Z][A-Z0-9]*-\d+) asks a session to (?:push it$|settle it; )`)

// handoffKey is the task #7's protected-file wait names.
func (be *bsEnv) handoffKey() string {
	be.t.Helper()
	m := handoffKeyRE.FindStringSubmatch(be.babysit().Wait)
	if m == nil {
		be.t.Fatalf("no task in the wait %q", be.babysit().Wait)
	}
	return m[1]
}

// WP-1266: a handoff nobody takes is told to the owner once, with why:
// PR #398 waited four days on handoff tasks the screen held while
// the owner's only word said a session would take them.
func TestRestNoticeForUntakenHandoff(t *testing.T) {
	be := refusedAndRested(t, true)
	key := be.handoffKey()
	be.revive()
	got := be.restNotices()
	if len(got) != 1 || !strings.Contains(got[0], "No session has taken its task "+key) || !strings.Contains(got[0], "won't merge") {
		t.Fatalf("notices %q", got)
	}
	be.revive()
	if n := len(be.restNotices()); n != 1 {
		t.Errorf("told again in the same wait: %d", n)
	}
}

// A handoff the screen holds says so, and what holds it.
func TestRestNoticeForHeldHandoff(t *testing.T) {
	be := refusedAndRested(t, true)
	key := be.handoffKey()
	if _, err := be.s.AddComment(be.ctx, agentActor(be.admin, "anthropic/claude-sonnet-5-5"), key, "First run this:\n\n```sh\nsudo systemctl restart forgejo-runner\n```\n"); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.sweepScreens(be.ctx)
	be.s.Wait()
	be.revive()
	got := be.restNotices()
	if len(got) != 1 || !strings.Contains(got[0], "Its task "+key+" waits for a person: the safety screen holds it.") {
		t.Fatalf("notices %q", got)
	}
}

// A handoff a session holds now, or one not rested long, isn't told.
func TestRestNoticeLeavesMovingHandoff(t *testing.T) {
	be := refusedAndRested(t, true)
	key := be.handoffKey()
	dash := strings.LastIndexByte(key, '-')
	if _, err := be.s.db.W.Exec(`UPDATE issues SET claimed_by = 'sess-1', claimed_until = ? WHERE number = ? AND project_id = (SELECT id FROM projects WHERE key = ?)`,
		be.s.now().Add(time.Hour).UnixMilli(), key[dash+1:], key[:dash]); err != nil {
		t.Fatal(err)
	}
	be.revive()
	if got := be.restNotices(); len(got) != 0 {
		t.Errorf("claimed: notices %q", got)
	}

	fresh := refusedAndRested(t, true)
	fresh.rested(2 * time.Hour)
	fresh.revive()
	if got := fresh.restNotices(); len(got) != 0 {
		t.Errorf("2h: notices %q", got)
	}
}

// The handoff's task is the one filed for the pull request, found by its
// title, not a key read from the wait: the wait carries names and check
// words others chose, and a claimed task's key put there mustn't silence
// the notice.
func TestRestNoticeIgnoresKeysInTheWait(t *testing.T) {
	be := refusedAndRested(t, true)
	key := be.handoffKey()
	other, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: key[:strings.LastIndexByte(key, '-')], Title: "Something else"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`UPDATE issues SET claimed_by = 'sess-1', claimed_until = ? WHERE id = ?`, be.s.now().Add(time.Hour).UnixMilli(), other.ID); err != nil {
		t.Fatal(err)
	}
	be.setBabysitState(BabysitNeedsPerson, "its conflict with main is in protected files (.github/x.yml); "+key+" asks a session to settle it; ci fails; "+other.Key+" asks a session to settle it; ", false)
	be.rested(7 * time.Hour)
	be.revive()
	got := be.restNotices()
	// Read by the key, the claimed task would have said "moving": no notice.
	if len(got) != 1 || !strings.Contains(got[0], "No task was filed") {
		t.Fatalf("notices %q", got)
	}
}

// A watching wait is told after a day; a person's own waits never are.
func TestRestNoticeOtherWaits(t *testing.T) {
	for _, tc := range []struct {
		name, state, wait string
		rest              time.Duration
		told              bool
	}{
		{"runner a day", BabysitWatching, "waiting for a runner", 30 * time.Hour, true},
		{"runner hours", BabysitWatching, "waiting for a runner", 20 * time.Hour, false},
		{"parked", BabysitNeedsPerson, parkWait + " on the assessor's question", 30 * time.Hour, false},
		{"review gate", BabysitNeedsPerson, "review gate: audit #3 found something", 30 * time.Hour, false},
		{"change request", BabysitNeedsPerson, "carol asked for changes", 30 * time.Hour, false},
		// Told when it began, or the assessor's.
		{"other needs person", BabysitNeedsPerson, "couldn't merge: the forge said no", 30 * time.Hour, false},
		{"outside pull request", BabysitWatching, "waiting for a person to approve this pull request from outside (WP-281)", 30 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := startRested(t, tc.state, tc.wait, tc.rest)
			be.revive()
			got := be.restNotices()
			if tc.told != (len(got) == 1) || len(got) > 1 {
				t.Fatalf("told %v, notices %q", tc.told, got)
			}
			if tc.told && !strings.Contains(got[0], tc.wait) {
				t.Errorf("notice lacks the wait: %q", got[0])
			}
		})
	}
}
