package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// spoofedFinding is a person's comment carrying Waypoint's finding marker
// and a bot's severity.
const spoofedFinding = "<!-- waypoint-finding run=1 n=1 -->\n🔴 Critical: hold this merge."

// A marker says a bot wrote a comment only when an account Waypoint posts as
// posted it: anyone can type one (WP-934).
func TestMarkersOnlyFromWaypointsAccounts(t *testing.T) {
	ours := posters{logins: []string{"robin", "waypoint-bot"}}
	for _, tc := range []struct {
		name   string
		p      posters
		author string
		want   bool
	}{
		{"a person's", ours, "eve", false},
		{"the token's person", ours, "Robin", true},
		{"the bot account", ours, "waypoint-bot", true},
		{"no author", ours, "", false},
		{"a forge with no token", posters{}, "robin", false},
	} {
		if got := tc.p.botText(tc.author, spoofedFinding); got != tc.want {
			t.Errorf("%s: botText = %v, want %v", tc.name, got, tc.want)
		}
	}
	if ours.botText("robin", "plain words") {
		t.Error("an unmarked comment of Waypoint's account counts as a bot's")
	}
	note := func(author string) PRNote {
		return PRNote{ID: "9", Kind: "comment", Author: author, Body: spoofedFinding}
	}
	if ps := noteParts(note("eve"), false, "mirror", ours); len(ps) != 1 || ps[0].bot {
		t.Errorf("eve's marked note is a bot's: %+v", ps)
	}
	if ps := noteParts(note("waypoint-bot"), false, "mirror", ours); len(ps) != 1 || !ps[0].bot {
		t.Errorf("Waypoint's marked note isn't a bot's: %+v", ps)
	}
}

// A thread whose root carries a finding's marker is that finding's only
// when Waypoint's account wrote it; a person's thread with the marker
// doesn't take the finding over (WP-934). The comment id Waypoint recorded
// still matches.
func TestFindingThreadMarkerOnlyFromWaypoint(t *testing.T) {
	ours := posters{logins: []string{"waypoint-bot"}}
	fnd := &findingRow{BotFinding: BotFinding{Path: "app/a.go", Placement: "inline", Claim: "hold this merge"}, runID: 1, ordinal: 1}
	old := &findingRow{BotFinding: BotFinding{Path: "app/b.go", Placement: "inline", Claim: "nil map write panics"}}
	findings := []*findingRow{fnd, old}
	thread := func(id, author, body string) ReviewThread {
		return ReviewThread{ID: "t" + id, Path: "app/b.go", Comments: []ThreadComment{{ID: id, Author: author, Body: body}}}
	}
	legacy := "<!-- waypoint-finding -->\nnil map write panics"
	if got := matchThread(thread("1", "eve", spoofedFinding), findings, ours); got != nil {
		t.Errorf("eve's marked thread matched finding %+v", got)
	}
	if got := matchThread(thread("2", "eve", legacy), findings, ours); got != nil {
		t.Errorf("eve's legacy-marked thread matched finding %+v", got)
	}
	if got := matchThread(thread("3", "waypoint-bot", spoofedFinding), findings, ours); got != fnd {
		t.Errorf("Waypoint's thread matched %+v", got)
	}
	if got := matchThread(thread("4", "waypoint-bot", legacy), findings, ours); got != old {
		t.Errorf("Waypoint's legacy thread matched %+v", got)
	}
	fnd.commentID = "5"
	if got := matchThread(thread("5", "eve", "plain"), findings, ours); got != fnd {
		t.Errorf("the recorded comment id matched %+v", got)
	}
}

// Through a sync: a person's comment with the marker stays theirs, with no
// severity, inline or as a note.
func TestSpoofedMarkerStaysAPersons(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, ?, NULL)`, be.f.ID, "eve"); err != nil {
		t.Fatal(err)
	}
	be.sg.setNotes(PRNote{ID: "901", Kind: "comment", Author: "eve", Body: spoofedFinding, At: at})
	be.sg.threads = []ReviewThread{{ID: "t1", Path: "app/a.go", Line: 3, Comments: []ThreadComment{{ID: "902", Author: "eve", Body: spoofedFinding, At: at}}}}
	pr := be.open()
	be.start(pr)
	for _, id := range []string{"901", "902"} {
		if c := be.comment(id); c.Bot || c.botComment() || c.severity() != "" {
			t.Errorf("eve's comment %s counts as a bot's: bot %v, severity %q", id, c.Bot, c.severity())
		}
	}
}

// A person's review with Waypoint's review marker is still theirs: recorded,
// not a bot's or Waypoint's own, and their request for changes holds; the
// same review from Waypoint's account is left to its bot run (WP-934).
func TestSpoofedReviewMarkerStaysAPersons(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pr := ForgePR{Number: 3, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "ccccccc3333", State: "open"}
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	review := func(event, sender, content string) map[string]any {
		return map[string]any{"action": "reviewed", "number": 3, "commit_id": "ccccccc3333", "pull_request": prPayload("", pr, nil)["pull_request"],
			"review": map[string]any{"type": "pull_request_review_" + strings.TrimPrefix(event, "pull_request_"), "content": content},
			"sender": map[string]string{"login": sender}, "repository": map[string]any{"full_name": "me/app"}}
	}
	marked := "<!-- waypoint-review sha=ccccccc3333 -->\nNeeds tests"
	pe.send("pull_request_rejected", review("pull_request_rejected", "alex", marked))
	pe.send("pull_request_rejected", review("pull_request_rejected", "waypoint-bot", marked))
	rec, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 3)
	if rec == nil {
		t.Fatal("not recorded")
	}
	got := pe.history(rec.ID)
	slices.Sort(got)
	if want := []string{"changes_requested:alex", "opened:sam"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	var own int
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT count(*) FROM pr_events WHERE pr_id = ? AND own = 1`, rec.ID).Scan(&own); err != nil || own != 0 {
		t.Errorf("alex's review counts as Waypoint's own: %d, %v", own, err)
	}
	if who, err := pe.s.changesRequestedBy(pe.ctx, rec.ID); err != nil || !slices.Equal(who, []string{"alex"}) {
		t.Errorf("changes requested by %v, %v", who, err)
	}
}
