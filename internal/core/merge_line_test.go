package core

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Far back in its line, an armed pull request's round waits; once it is
// within roundWindow of the front, it goes (WP-428).
func TestRoundWaitsFarBackInLine(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	for n := 1; n <= roundWindow; n++ {
		be.queuedDraft(10+n, "change-"+strconv.Itoa(n), strings.Repeat(strconv.Itoa(n), 7)+"aaaa")
	}
	const head = "9999999aaaa"
	be.queuedDraft(20, "late", head)
	be.draftChecks(head, CommitCheck{State: CheckFailed, ID: 5, Description: "Failing after 2m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	// The failure is re-run once first (WP-392); it fails again, then the round waits.
	be.setClock(be.clockNow().Add(faultRerunSettle))
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runners SET last_seen_at = ? WHERE last_seen_at IS NOT NULL`, be.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.s.babysitStep(be.ctx, be.pr(20).ID, true)
	be.s.Wait()
	if b := be.babysitOf(20); b.Round != 0 || !strings.Contains(b.Wait, "its round waits") || !strings.Contains(b.Wait, "6th") {
		t.Fatalf("a round far back in the line: round %d, %q", b.Round, b.Wait)
	}
	if _, err := be.s.StopBabysit(be.ctx, be.admin, be.pr(11).ID); err != nil {
		t.Fatal(err)
	}
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
	if b := be.babysitOf(20); b.Round != 1 {
		t.Fatalf("no round within the window: round %d, %q", b.Round, b.Wait)
	}
}

// A pull request that left the queue for a round joins again at its place
// in the line, ahead of one armed after it (WP-428).
func TestQueueRejoinsAtItsPlace(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	be.queuedDraft(7, "add-export", "7777777aaaa")
	be.setClock(be.clockNow().Add(time.Minute))
	be.queuedDraft(8, "add-import", "8888888cccc")
	for _, n := range []int{7, 8} {
		be.s.queueTurn(be.ctx, be.pr(n))
	}
	be.s.leaveQueue(be.ctx, be.pr(7).ID)
	be.setClock(be.clockNow().Add(time.Hour))
	be.s.queueTurn(be.ctx, be.pr(7))
	if ahead, _ := be.s.queueAhead(be.ctx, be.pr(8)); len(ahead) != 1 || ahead[0].Number != 7 {
		t.Fatalf("#8 has %d ahead; #7 rejoined at the back", len(ahead))
	}
	// Armed again (wp ship once more), it keeps its place.
	armed := *be.pr(7).ArmedAt
	if _, err := be.s.MergePR(be.ctx, be.agent, be.pr(7).ID, MergeInput{WhenReady: true, Style: "merge"}); err != nil {
		t.Fatal(err)
	}
	if got := *be.pr(7).ArmedAt; !got.Equal(armed) {
		t.Errorf("armed again: armed_at %v, was %v", got, armed)
	}
}

// When a job fails on the base's tip, a queued pull request that holds the
// tip and passes it goes to the front; one that doesn't hold the tip, or
// comes second, doesn't (WP-428).
func TestBaseFixGoesFirst(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8, head9, head10, tip = "7777777aaaa", "8888888cccc", "9999999dddd", "aaaaaaa1111", "b0b0b0b0b0b"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.queuedDraft(9, "fix-main", head9)
	be.queuedDraft(10, "fix-main-too", head10)
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO branch_activity(forge_id, repo, branch, head_sha, pushed_at) VALUES (?,?,?,?,?)`, pr.ForgeID, pr.Repo, "main", tip, be.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	headID, err := be.s.trackHead(be.ctx, pr.ForgeID, pr.Repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	tipChecks := []CommitCheck{{Name: "ci / backend (push)", State: CheckFailed, Description: "Failing after 3m"}, {Name: "ci / e2e (push)", State: CheckSuccess, Description: "Successful in 9m"}}
	be.sg.setChecks(tip, tipChecks...)
	for _, c := range tipChecks {
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_checks(head_id, name, state, description, updated_at) VALUES (?,?,?,?,?)`, headID, c.Name, c.State, c.Description, be.s.nowMS()); err != nil {
			t.Fatal(err)
		}
	}
	green := func(sha string, id int64) {
		be.draftChecks(sha, CommitCheck{State: CheckSuccess, ID: id, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: id + 1, Description: "Has been skipped"})
	}
	ahead := func(n int) []int {
		list, err := be.s.queueAhead(be.ctx, be.pr(n))
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, p := range list {
			out = append(out, p.Number)
		}
		return out
	}
	// #7 and #8 pass on heads without the tip: they prove nothing. #7, the
	// front, is marked ready.
	be.sg.sm.Lock()
	be.sg.hidden = map[string]bool{tip: true}
	be.sg.sm.Unlock()
	green(head7, 10)
	if be.pr(7).ReadyAt == nil {
		t.Fatalf("#7 not marked ready: %q", be.babysitOf(7).Wait)
	}
	green(head8, 20)
	be.sg.sm.Lock()
	delete(be.sg.hidden, tip)
	be.sg.sm.Unlock()
	// #9 passes on top of the tip: it goes ahead of #8, not of the ready #7.
	green(head9, 30)
	if got := ahead(9); len(got) != 1 || got[0] != 7 {
		t.Fatalf("the base fix has %v ahead, want [7]", got)
	}
	if got := ahead(8); len(got) != 2 || got[1] != 9 {
		t.Fatalf("#8 has %v ahead, want [7 9]", got)
	}
	// A second fix doesn't jump ahead of the first.
	green(head10, 40)
	if got := ahead(10); len(got) != 3 || got[1] != 9 {
		t.Fatalf("#10 has %v ahead, want [7 9 8]", got)
	}
}

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
