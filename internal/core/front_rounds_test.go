package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The front of the merge queue, marked ready, keeps its place and stays
// ready through a round for a review comment (WP-455, WP-456): with nothing
// pushed the full run that passed stands and it merges; a push lands on the
// ready title, runs the full tier, and it merges once that passes. The
// queue waits behind it meanwhile.
func TestFrontCommentRoundStaysReady(t *testing.T) {
	for _, pushed := range []bool{false, true} {
		name := "nothing pushed"
		if pushed {
			name = "pushed"
		}
		t.Run(name, func(t *testing.T) {
			be := newBabysitEnvOn(t, "forgejo")
			be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
			be.runnerOnline()
			const head7, head8 = "7777777aaaa", "8888888cccc"
			p7 := be.queuedDraft(7, "add-export", head7)
			be.queuedDraft(8, "add-import", head8)
			be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
			be.draftChecks(head8, draftTierGreen8[0], draftTierGreen8[1])
			if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
				t.Fatalf("#7 not marked ready: %+v", pr)
			}
			// A comment comes in during the full run (its webhook reads it).
			be.sg.setNotes(PRNote{ID: "601", Kind: "comment", Author: "carol", Body: "Should exports include archived rows?", At: be.clockNow()})
			if _, err := be.s.syncReviewComments(be.ctx, be.pr(7)); err != nil {
				t.Fatal(err)
			}
			be.runnerOnline() // runners check in every few seconds; the test clock jumps
			be.draftChecks(head7, fullRunGreen7[0], fullRunGreen7[1])
			b := be.babysitOf(7)
			if b.Round != 1 || b.JobID == 0 || len(b.Rounds[0].Comments) != 1 {
				t.Fatalf("no round for carol's comment: %+v", b)
			}
			if e := be.sg.edits(); len(e) != 1 || draftTitle(e[0]) {
				t.Fatalf("title edits %v: the round put it back to draft", e)
			}
			if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil || pr.QueuedAt == nil {
				t.Fatalf("#7 left the front for its comment round: %+v", pr)
			}
			be.sweep()
			if pr := be.pr(8); pr.State != "draft" || pr.ReadyAt != nil {
				t.Fatalf("#8 marked ready while #7's round works: %+v", pr)
			}
			if m := be.sg.merged(); len(m) != 0 {
				t.Fatalf("merged while the round works: %+v", m)
			}

			// The round answers the comment.
			if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{be.comment("601").Ref}, Verdict: VerdictRejected, Note: "Archived rows stay out, as the spec says."}); err != nil {
				t.Fatal(err)
			}
			if !pushed {
				be.finish(b.JobID, false)
				be.sweep()
				if m := be.sg.merged(); len(m) != 1 {
					t.Fatalf("merges %+v after a round that pushed nothing: %q (babysit %q)", m, be.reasons(7), be.babysitOf(7).Wait)
				}
				return
			}
			be.setClock(be.clockNow().Add(time.Minute))
			p7.Title = "Change add-export" // ready: the push runs the full tier
			be.pushTo(p7, "7777777bbbb")
			be.finish(b.JobID, true)
			be.sweep()
			if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil || pr.QueuedAt == nil {
				t.Fatalf("#7 after its round's push: %+v", pr)
			}
			if m := be.sg.merged(); len(m) != 0 {
				t.Fatalf("merged before the push's full run: %+v", m)
			}
			be.draftChecks("7777777bbbb", CommitCheck{State: CheckSuccess, ID: 11, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 12, Description: "Successful in 24m"})
			be.sweep()
			if m := be.sg.merged(); len(m) != 1 {
				t.Fatalf("merges %+v after the push's full run: %q (babysit %q)", m, be.reasons(7), be.babysitOf(7).Wait)
			}
			for _, e := range be.sg.edits() {
				if draftTitle(e) {
					t.Errorf("title edits %v: something went back to draft", be.sg.edits())
				}
			}
		})
	}
}

// The quiet period counts what others post on a pull request, not
// Waypoint's own replies and notes (WP-455): a round's answers would
// otherwise start it over.
func TestQuietPeriodIgnoresOwnPosts(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.fg.ownLogin = "robin" // Waypoint posts as the forge token's person
	pr := be.open()
	ctx := context.Background()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	be.s.onPRActivity(ctx, be.f, "me/app", &forgeReviewEvent{number: 7, kind: "commented", ForgeComment: ForgeComment{ID: "c1", Author: "carol", Body: "Why archived rows?", UpdatedAt: at}})
	be.s.onPRActivity(ctx, be.f, "me/app", &forgeReviewEvent{number: 7, kind: "commented", ForgeComment: ForgeComment{ID: "c2", Author: "robin", Body: "They stay out.\n\n" + strings.Repeat("x", 600) + "\n<!-- waypoint-reply -->", UpdatedAt: at.Add(5 * time.Minute)}})
	be.s.Wait()
	last, what, err := be.s.lastPosted(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !last.Equal(at) || !strings.Contains(what, "carol") {
		t.Fatalf("last posted %v (%s): want carol's comment at %v", last, what, at)
	}
	var own int
	if err := be.s.db.R.QueryRowContext(ctx, `SELECT own FROM pr_events WHERE ext_id = 'c2'`).Scan(&own); err != nil || own != 1 {
		t.Fatalf("own = %d, %v", own, err)
	}
}
