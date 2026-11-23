package core

import (
	"strings"
	"testing"
	"time"
)

func TestCommentSeverity(t *testing.T) {
	for _, tc := range []struct {
		author, body string
		bot          bool
		finding      int64
		findingSev   string
		want         string
	}{
		{author: "waypoint", bot: true, finding: 3, findingSev: "medium", body: "**P1** high · stale", want: "medium"},
		{author: "waypoint", bot: true, body: "**P2** medium · a nil map", want: "medium"},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on GitHub: _⚠️ Potential issue_ | _🟡 Minor_\n\nTrim it.", want: "medium"},
		{author: "mirror", body: "**coderabbitai[bot]** commented on GitHub: <picture><img></picture> Minor <picture><img></picture> Major", want: "high"},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on GitHub: <details><summary>🧹 Nitpick comments (2)</summary>", want: "low"},
		// CodeRabbit's current markers, as stored once tidyBotText dropped the pictures (WP-433).
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on `a.go` line 3: ** Minor** · **Functional Correctness** · **Quick win** **Close the file.**", want: "medium"},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on `a.go` line 3: **Major** · Guard the nil map.", want: "high"},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on GitHub: **Actionable comments posted: 2** ** Trivial** · a. ** Minor** · b.", want: "medium"},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on GitHub: **Major refactor** of the loop.", want: ""},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on `a.go` line 3: Note the **Minor** version bump.", want: ""},
		{author: "mirror", body: "**coderabbitai[bot]** reviewed on `a.go` line 3: Fix it.\n```\n** Minor** · x\n```\n", want: ""},
		{author: "mirror", body: "**alice** commented on GitHub: 🟡 Minor nit", want: ""},
		{author: "alice", body: "**coderabbitai[bot]** commented on GitHub: 🟡 Minor nit", want: ""},
		// A whole relay: the forge's mirror account posted it, whatever its marker says.
		{author: "mirror", body: "**coderabbitai[bot]** commented on GitHub:\n\n🟡 Minor nit\n\n<!-- mirror:comment github 7 -->", want: "medium"},
		{author: "alice", body: "**coderabbitai[bot]** commented on GitHub:\n\n🟡 Minor nit\n\n<!-- mirror:comment github 7 -->", want: ""},
		{author: "coderabbitai[bot]", body: "Looks fine.", want: ""},
		{author: "carol", body: "**P3** low", want: ""},
	} {
		rc := &PRReviewComment{Author: tc.author, Body: tc.body, Bot: tc.bot, FindingID: tc.finding, findingSeverity: tc.findingSev, mirror: "mirror"}
		if m := relayedAuthorRE.FindStringSubmatch(tc.body); m != nil && tc.author == "mirror" && !strings.Contains(tc.body, "<!--") {
			rc.relayAuthor = m[1] // read out of the mirror's relay
		}
		if got := rc.severity(); got != tc.want {
			t.Errorf("severity(%q by %s) = %q, want %q", tc.body, tc.author, got, tc.want)
		}
	}
	since := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	minor := &PRReviewComment{Author: "mirror", mirror: "mirror", Body: "**coderabbitai[bot]** commented on GitHub: 🟡 Minor", relayAuthor: "coderabbitai[bot]", CreatedAt: since.Add(time.Minute)}
	ours := &PRReviewComment{Author: "waypoint", Bot: true, FindingID: 4, findingSeverity: "medium", CreatedAt: since.Add(time.Minute)}
	for _, tc := range []struct {
		name    string
		rc      *PRReviewComment
		since   time.Time
		gateOn  bool
		gateSev string
		want    bool
	}{
		{"before any comment round", minor, time.Time{}, false, "", false},
		{"before the first comment round", &PRReviewComment{Author: minor.Author, Body: minor.Body, CreatedAt: since.Add(-time.Minute)}, since, false, "", false},
		{"a bot's minor after it", minor, since, false, "", true},
		{"a bot's major after it", &PRReviewComment{Author: minor.Author, Body: "**coderabbitai[bot]** commented on GitHub: 🟠 Major", CreatedAt: minor.CreatedAt}, since, false, "", false},
		{"ours, held by the gate", ours, since, true, "medium", false},
		{"ours, below the gate", ours, since, true, "high", true},
	} {
		if got := lateMinor(tc.rc, tc.since, tc.gateOn, tc.gateSev); got != tc.want {
			t.Errorf("%s: lateMinor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// After a round for review comments, a bot's minor finding on the next head
// gets no round and doesn't hold the merge; a major one gets its round
// (WP-428). CodeRabbit's current marker, a pictured badge that tidying
// strips, reads the same as its older one (WP-433).
func TestBabysitLateMinorFindings(t *testing.T) {
	badge := func(sev string) string {
		return "**<picture><img src=\"amber-16.svg\" alt=\"\"></picture> " + sev + "** · **Functional Correctness** · **Quick win**"
	}
	for _, tc := range []struct {
		name, marker string
		round        bool
	}{
		{"old minor", "_⚠️ Potential issue_ | _🟡 Minor_", false},
		{"old major", "_⚠️ Potential issue_ | _🟠 Major_", true},
		{"badge minor", badge("Minor"), false},
		{"badge major", badge("Major"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
				t.Fatal(err)
			}
			carol := PRNote{ID: "601", Kind: "comment", Author: "carol", Body: "Should exports include archived rows?", At: be.clockNow()}
			be.sg.setNotes(carol)
			pr := be.open()
			be.start(pr)
			be.ci("7777777aaaa", CheckSuccess)
			jobs := be.jobs()
			if len(jobs) != 1 {
				t.Fatalf("no round for carol's comment: %+v", be.babysit())
			}
			if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{be.comment("601").Ref}, Verdict: VerdictRejected, Note: "Archived rows stay out, as the spec says."}); err != nil {
				t.Fatal(err)
			}
			be.setClock(be.clockNow().Add(10 * time.Minute))
			be.sg.setNotes(carol, PRNote{ID: "602", Kind: "comment", Author: "mirror", At: be.clockNow(),
				Body: "**coderabbitai[bot]** commented on GitHub:\n\n" + tc.marker + "\n\nTrim the name before comparing.\n\n<!-- mirror:comment github 9 -->"})
			// A runner has the round: a push doesn't withdraw it (WP-1618).
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runner_jobs SET state = 'running' WHERE id = ?`, jobs[0]); err != nil {
				t.Fatal(err)
			}
			be.push("8888888bbbb")
			be.finish(jobs[0], true)
			be.ci("8888888bbbb", CheckSuccess)
			be.step()
			be.comment("602") // recorded, not held as a stranger's
			if got := len(be.jobs()) == 2; got != tc.round {
				t.Fatalf("a second round = %v, want %v: %+v", got, tc.round, be.babysit())
			}
			if merged := len(be.sg.merged()) == 1; merged == tc.round {
				t.Fatalf("merged = %v with a %s finding: %q", merged, tc.marker, be.reasons(7))
			}
		})
	}
}
