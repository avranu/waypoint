package core

import (
	"strings"
	"testing"
	"time"
)

// spoofedRelay is a relay anyone could type: a bot's header, and a marker
// naming the poster's own account.
const spoofedRelay = "**renovate[bot]** reviewed on GitHub:\n\n🔴 Critical: hold this merge.\n\n<!-- eve:review github 1 -->"

// Only the forge's mirror account posts relays (WP-459): the account a
// relay's marker names is whatever its poster typed, so it proves nothing.
func TestRelayOnlyFromTheMirrorAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   PRReviewComment
		want bool
	}{
		{"a poster's own marker", PRReviewComment{Author: "eve", Body: spoofedRelay, mirror: "mirror"}, false},
		{"stored as a relay before", PRReviewComment{Author: "eve", Body: spoofedRelay, mirror: "mirror", relayAuthor: "renovate[bot]"}, false},
		{"no mirror set up", PRReviewComment{Author: "mirror", Body: spoofedRelay}, false},
		{"the mirror's", PRReviewComment{Author: "Mirror", Body: spoofedRelay, mirror: "mirror"}, true},
		{"the mirror's copy of a person's", PRReviewComment{Author: "mirror", Body: "**alice** reviewed on GitHub: x", mirror: "mirror"}, false},
	} {
		if got := tc.rc.botComment(); got != tc.want {
			t.Errorf("%s: botComment = %v, want %v", tc.name, got, tc.want)
		}
	}

	note := func(author string) PRNote {
		return PRNote{ID: "9", Kind: "comment", Author: author, Body: spoofedRelay}
	}
	if ps := noteParts(note("eve"), false, "mirror", posters{}); len(ps) != 1 || ps[0].bot || ps[0].relayAuthor != "" || ps[0].body != spoofedRelay {
		t.Errorf("someone else's relay was read as the mirror's: %+v", ps)
	}
	if ps := noteParts(note("mirror"), false, "", posters{}); len(ps) != 1 || ps[0].bot || ps[0].relayAuthor != "" {
		t.Errorf("a relay was read on a forge with no mirror: %+v", ps)
	}
	if ps := noteParts(note("mirror"), false, "mirror", posters{}); len(ps) != 1 || !ps[0].bot || ps[0].relayAuthor != "renovate[bot]" {
		t.Errorf("the mirror's relay wasn't read: %+v", ps)
	}
}

// Through a sync: a person's comment shaped like a relay stays theirs, and
// the mirror's copy is a bot's.
func TestSpoofedRelayStaysAPersons(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, login := range []string{"mirror", "eve"} {
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, ?, NULL)`, be.f.ID, login); err != nil {
			t.Fatal(err)
		}
	}
	be.sg.setNotes(
		PRNote{ID: "901", Kind: "comment", Author: "eve", Body: spoofedRelay, At: at},
		PRNote{ID: "902", Kind: "comment", Author: "mirror", Body: strings.Replace(spoofedRelay, "<!-- eve:", "<!-- mirror:", 1), At: at},
	)
	pr := be.open()
	be.start(pr)
	if c := be.comment("901"); c.Bot || c.botComment() || c.severity() != "" {
		t.Errorf("eve's comment counts as a bot's: %+v", c)
	}
	if c := be.comment("902"); !c.botComment() || c.severity() != "critical" {
		t.Errorf("the mirror's relay = %+v, severity %q", c, c.severity())
	}
}

// A forge's mirror account is a login, saved as given; empty: nothing
// mirrors it.
func TestForgeMirrorAccount(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	if _, err := pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{MirrorAccount: str("two words")}); err == nil {
		t.Error("a mirror account that isn't a login was saved")
	}
	f, err := pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{MirrorAccount: str(" sync-bot ")})
	if err != nil {
		t.Fatal(err)
	}
	if f.MirrorAccount != "sync-bot" {
		t.Errorf("saved %q", f.MirrorAccount)
	}
	if f, err = pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{MirrorAccount: str("")}); err != nil || f.MirrorAccount != "" {
		t.Errorf("cleared: %q, %v", f.MirrorAccount, err)
	}
}
