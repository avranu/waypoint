package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Bodies as a mirror posting as the account "mirror" writes them:
// header, blank line, text, blank line, marker.
const (
	crSummary = "**coderabbitai[bot]** commented on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\n" +
		"<!-- review_stack_entry_start -->\n## Findings\n<p><a href=\"https://github.com/o/r/pull/51#discussion_r1\">Keep the field-name input.</a></p>\n\n<!-- mirror:comment github 6561453485 -->"
	crSkipped = "**coderabbitai[bot]** commented on GitHub:\n\n<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\n" +
		"<!-- This is an auto-generated comment: skip review by coderabbit.ai -->\n> [!IMPORTANT]\n> ## Review skipped\n> Draft PRs are not reviewed.\n\n<!-- mirror:comment github 6561453486 -->"
	crApproved = "**coderabbitai[bot]** approved on GitHub.\n\n<!-- mirror:review github 5720112460 -->"
	crReview   = "**coderabbitai[bot]** reviewed on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 2**\n\n---\n" +
		"<details>\n<summary>Prompt to fix review comments</summary>\n\n```\nReview comments at @app/a.go: - Line 12: fix it.\n```\n</details>\n\n" +
		"<!-- This is an auto-generated comment by CodeRabbit for review status -->\n\n" +
		"On `app/a.go` line 12:\n<!-- mirror:finding github 4439419656 -->\n\n<!-- coderabbit-ai-generated:v1 -->\n**Minor** · **Handle a failed request.**\n\nThe promise rejection escapes.\n<!-- This is an auto-generated comment by CodeRabbit -->\n\n" +
		"On `app/b.go` line 7 (before the change):\n<!-- mirror:finding github 4439419657 -->\n\n**Major** · **Keep the old check.**\n\nRemoving it lets nil through.\n\n" +
		"<!-- mirror:review github 5726966434 -->"
	crOutside = "**coderabbitai[bot]** reviewed on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 1**\n\n> [!CAUTION]\n" +
		"> **⚠️ Outside diff range comments (1)**\n> `internal/core/merge.go:193-199`: Schedule the audit deadline.\n\n" +
		"On `docs/CLI.md` line 2054:\n\n**Minor** · Add audit findings to the help text.\n\n<!-- mirror:review github 5726966435 -->"
	crNoInline = "**coderabbitai[bot]** reviewed on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 1**\n\nReview comments at @app/a.go: - Line 3: guard nil.\n\n" +
		"<!-- mirror:review github 5726966436 -->"
	personRelay = "**octo** commented on GitHub:\n\nPlease keep the old name too.\n\n<!-- mirror:comment github 99 -->"
	personLine  = "**octo** commented on `app/a.go` line 4 on GitHub:\n\nWhy not a map?\n\n<!-- mirror:line github 100 -->"
	// A lone line comment names itself
	// before its text, and replies in a GitHub thread come across too.
	crLine = "**coderabbitai[bot]** commented on `app/c.go` line 9 on GitHub:\n\n<!-- mirror:finding github 4439419700 -->\n\n" +
		"**Minor** · **Close the file.**\n\nThe handle leaks on error.\n\n<!-- mirror:line github 4439419700 -->"
	crThreadReply = "**coderabbitai[bot]** replied in the thread on `app/a.go` line 12 on GitHub:\n\n<!-- mirror:finding github 4439419800 -->\n\n" +
		"`@robinAI` Thanks for the explanation! I've noted it.\n\n<!-- mirror:line github 4439419800 -->"
	personThreadReply = "**octo** replied in the thread on `app/a.go` line 12 on GitHub:\n\n<!-- mirror:finding github 4439419801 -->\n\n" +
		"Are you sure? It can fail on a timeout.\n\n<!-- mirror:line github 4439419801 -->"
)

// CodeRabbit's chrome goes: icons, collapsible sections that repeat the
// finding or have nothing to act on, tags. Code keeps its tags; a section
// with findings stays.
func TestTidyBotText(t *testing.T) {
	in := "**coderabbitai[bot]** reviewed on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 1**\n\n" +
		"<details>\n<summary><picture><source srcset=\"x-dark.svg\"><img src=\"x.svg\" alt=\"\"></picture> Nitpick comments: 1</summary><blockquote>\n" +
		"<details>\n<summary>app/a.tsx: 1</summary><blockquote>\n\n`12-12`: **Share the index.**\n\n</blockquote></details>\n</blockquote></details>\n\n" +
		"<details>\n<summary>🤖 Prompt for AI Agents</summary>\n\n```\nFix app/a.tsx line 12.\n```\n\n</details>\n\n" +
		"<details>\n<summary>📜 Review details</summary>\n<details>\n<summary>Commits</summary>\nabc..def\n</details>\n</details>\n\n" +
		"<details>\n<summary>🐛 Proposed fix</summary>\n\n```diff\n-  <div className=\"a\">\n+  <span>&amp;</span>\n```\n</details>\n\n" +
		"<details>\n<summary>📝 Committable suggestion</summary>\n\n```suggestion\n  <span/>\n```\n</details>\n\n" +
		"**Included review availability:** 2 remain.\n"
	got := tidyBotText(in)
	for _, want := range []string{"Nitpick comments: 1", "app/a.tsx: 1", "**Share the index.**", "🐛 Proposed fix", `-  <div className="a">`, "+  <span>&amp;</span>"} {
		if !strings.Contains(got, want) {
			t.Errorf("tidied lost %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"<picture", "<summary", "<details", "<blockquote", "Prompt for AI", "Fix app/a.tsx", "Review details", "abc..def", "Committable", "<span/>", "availability", "<!--"} {
		if strings.Contains(got, gone) {
			t.Errorf("tidied kept %q:\n%s", gone, got)
		}
	}
	if tidyReviewBody("carol", "<b>keep</b> mine") != "<b>keep</b> mine" {
		t.Error("tidied a person's comment")
	}
	n := PRNote{ID: "1", Kind: "comment", Author: "mirror", Body: in + "\nOn `app/b.go` line 3:\n\n**Minor** · guard nil.\n\n<!-- mirror:review github 1 -->"}
	if ps := noteParts(n, false, "mirror", posters{}); len(ps) != 2 || ps[0].extID != "1" || ps[1].extID != relayedID("1", "`app/b.go` line 3") {
		t.Errorf("parts = %+v", ps)
	}
	if botTextChanged(excerpt(in, storedCommentChars), excerpt(tidyBotText(in), storedCommentChars)) {
		t.Error("tidying counts as an edit")
	}
}

func TestNoteParts(t *testing.T) {
	type part struct {
		kind, ext, path string
		line            int
		bot             bool
	}
	parts := func(n PRNote) []part {
		var out []part
		for _, p := range noteParts(n, false, "mirror", posters{}) {
			out = append(out, part{p.kind, p.extID, p.path, p.line, p.bot})
		}
		return out
	}
	note := func(id, body string) PRNote { return PRNote{ID: id, Kind: "comment", Author: "mirror", Body: body} }
	cases := []struct {
		name string
		n    PRNote
		want []part
	}{
		{"walkthrough summary", note("1", crSummary), nil},
		{"review skipped", note("2", crSkipped), nil},
		{"approval", note("3", crApproved), nil},
		{"review: one comment per inline finding, no status body", note("4", crReview), []part{
			{"relayed", relayedID("4", "`app/a.go` line 12"), "app/a.go", 12, true},
			{"relayed", relayedID("4", "`app/b.go` line 7 (before the change)"), "app/b.go", 0, true},
		}},
		{"review with findings outside the diff keeps its body", note("5", crOutside), []part{
			{"comment", "5", "", 0, true},
			{"relayed", relayedID("5", "`docs/CLI.md` line 2054"), "docs/CLI.md", 2054, true},
		}},
		{"review whose inline comments didn't come across keeps its body", note("6", crNoInline), []part{{"comment", "6", "", 0, true}}},
		{"a person's comment from GitHub", note("7", personRelay), []part{{"comment", "7", "", 0, false}}},
		{"a person's line comment from GitHub", note("8", personLine), []part{{"comment", "8", "app/a.go", 4, false}}},
		{"CodeRabbit's summary on GitHub itself", PRNote{ID: "9", Kind: "comment", Author: "coderabbitai[bot]", Body: "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\nWalkthrough"}, nil},
		{"CodeRabbit's review body on GitHub itself: the threads carry the findings", PRNote{ID: "10", Kind: "review", Author: "coderabbitai[bot]",
			Body: "**Actionable comments posted: 3**\n<!-- This is an auto-generated comment by CodeRabbit for review status -->"}, nil},
		{"anyone else's", PRNote{ID: "11", Kind: "comment", Author: "carol", Body: "On `a.go` line 3:\n\nnot a copy"}, []part{{"comment", "11", "", 0, false}}},
	}
	for _, c := range cases {
		if got := parts(c.n); !slices.Equal(got, c.want) {
			t.Errorf("%s: parts = %+v, want %+v", c.name, got, c.want)
		}
	}

	ps := noteParts(note("4", crReview), false, "mirror", posters{})
	if b := ps[0].body; !strings.HasPrefix(b, "**coderabbitai[bot]** reviewed on `app/a.go` line 12:") || !strings.Contains(b, "promise rejection") ||
		strings.Contains(b, "Keep the old check") || strings.Contains(b, "Actionable") {
		t.Errorf("first finding's body = %q", b)
	}
	if b := ps[1].body; !strings.Contains(b, "(before the change)") || !strings.Contains(b, "lets nil through") || strings.Contains(b, "mirror:review") {
		t.Errorf("second finding's body = %q", b)
	}
	if got := noteParts(note("1", crSummary), true, "mirror", posters{}); len(got) != 1 || got[0].extID != "1" {
		t.Errorf("kept whole = %+v", got)
	}
	long := strings.Replace(crReview, "---\n", "---\n"+strings.Repeat("<picture><source srcset=\"x.svg\"></picture> ", 120), 1)
	if got := noteParts(note("12", long), true, "mirror", posters{}); len(got) != 2 || got[0].extID != relayedID("12", "`app/a.go` line 12") || got[1].extID != relayedID("12", "`app/b.go` line 7 (before the change)") {
		t.Errorf("cut, dealt with whole = %+v", got)
	}
}

// A review a mirror copied becomes one review comment per inline finding,
// each with its file and line; summaries and approvals aren't review
// comments; the findings take verdicts one by one, with replies in the
// conversation and no emoji.
func TestRelayedReviewComments(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	be.sg.setNotes(
		PRNote{ID: "701", Kind: "comment", Author: "mirror", Body: crSummary, At: at},
		PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: crReview, At: at},
		PRNote{ID: "703", Kind: "comment", Author: "mirror", Body: crApproved, At: at},
	)
	pr := be.open()
	be.changes("7777777aaaa", "app/a.go", "app/b.go")
	be.start(pr)
	var got []string
	for _, c := range be.comments() {
		got = append(got, c.Kind+":"+c.extID+":"+c.Path)
	}
	if want := []string{"relayed:" + relayedID("702", "`app/a.go` line 12") + ":app/a.go", "relayed:" + relayedID("702", "`app/b.go` line 7 (before the change)") + ":app/b.go"}; !slices.Equal(got, want) {
		t.Fatalf("comments = %v, want %v", got, want)
	}
	if c := be.comment(relayedID("702", "`app/a.go` line 12")); c.Line != 12 || !c.Bot || c.Author != "mirror" {
		t.Errorf("finding = %+v", c)
	}
	if r := be.reasons(7); !strings.Contains(r, "2 review comments not dealt with") {
		t.Errorf("readiness = %q", r)
	}
	todo, err := be.s.PRWorkPacket(be.ctx, be.agent, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(todo.Text, "by mirror, app/a.go:12") || strings.Contains(todo.Text, "Findings") {
		t.Errorf("work packet:\n%s", todo.Text)
	}

	ref := be.comment(relayedID("702", "`app/a.go` line 12")).Ref
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{ref}, Verdict: VerdictRejected, Note: "the request can't fail here"}); err != nil {
		t.Fatal(err)
	}
	be.step()
	if c := be.comment(relayedID("702", "`app/a.go` line 12")); !c.Dealt || c.Verdict != VerdictRejected {
		t.Errorf("rejected = %+v", c)
	}
	if c := be.comment(relayedID("702", "`app/b.go` line 7 (before the change)")); c.Dealt {
		t.Errorf("the other finding = %+v", c)
	}
	be.sg.sm.Lock()
	posted := strings.Join(be.sg.posted, "\n")
	be.sg.sm.Unlock()
	if !strings.Contains(posted, "> **coderabbitai[bot]** reviewed on `app/a.go` line 12:") || !strings.Contains(posted, "([comment](https://git.example.com/me/app/pulls/7#issuecomment-702))") ||
		!strings.Contains(posted, "Not changing this: the request can't fail here") {
		t.Errorf("replies = %q", posted)
	}
	for _, r := range be.sg.reacted() {
		if strings.Contains(r, ":702") {
			t.Errorf("emoji on the copied review: %s", r)
		}
	}

	addressed := strings.Replace(crReview, "The promise rejection escapes.", "The promise rejection escapes.\n\n<!-- <review_comment_addressed> -->\n✅ Addressed in commit 1a2b3c4", 1)
	be.sg.setNotes(PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: addressed, At: at, UpdatedAt: at.Add(time.Minute)})
	be.step()
	if c := be.comment(relayedID("702", "`app/a.go` line 12")); c.Verdict != VerdictRejected {
		t.Errorf("after CodeRabbit marked it addressed = %+v", c)
	}
	be.sg.setNotes(PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: strings.Replace(crReview, "escapes", "escapes the handler", 1), At: at, UpdatedAt: at.Add(2 * time.Minute)})
	be.step()
	if c := be.comment(relayedID("702", "`app/a.go` line 12")); c.Verdict != VerdictNew || c.Dealt || !strings.Contains(c.Body, "escapes the handler") {
		t.Errorf("after an edit = %+v", c)
	}
	if c := be.comment(relayedID("702", "`app/b.go` line 7 (before the change)")); c.Verdict != VerdictNew || c.Dealt {
		t.Errorf("the finding the edit left alone = %+v", c)
	}
	if r := be.reasons(7); !strings.Contains(r, "2 review comments not dealt with") {
		t.Errorf("readiness after an edit = %q", r)
	}

	be.sg.setNotes(PRNote{ID: "5", Kind: "comment", Author: "mirror", Body: crOutside, At: at})
	be.step()
	head := be.comment("5")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{head.Ref}, Verdict: VerdictRejected, Note: "not relevant"}); err != nil {
		t.Fatal(err)
	}
	be.step()
	if c := be.comment(relayedID("5", "`docs/CLI.md` line 2054")); c.Gone || c.Dealt {
		t.Errorf("finding after its review's head was dealt with = %+v", c)
	}

	long := "**octo** commented on GitHub:\n\n" + strings.Repeat("Please also check the retry path. ", 150) + "\n\n<!-- mirror:comment github 98 -->"
	be.sg.setNotes(PRNote{ID: "720", Kind: "comment", Author: "mirror", Body: long, At: at})
	be.step()
	todo, err = be.s.PRWorkPacket(be.ctx, be.agent, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(todo.Text, "Cut at 4000 of") {
		t.Errorf("work packet doesn't say it cut a comment:\n%s", todo.Text)
	}
}

// A finding keeps its id when another is added before it in the review body.
func TestRelayedIDsStable(t *testing.T) {
	n := PRNote{ID: "4", Kind: "comment", Author: "mirror", Body: crReview}
	before := noteParts(n, false, "mirror", posters{})
	n.Body = strings.Replace(crReview, "On `app/a.go` line 12:", "On `new.go` line 1:\n\n**Minor** · new.\n\nOn `app/a.go` line 12:", 1)
	after := noteParts(n, false, "mirror", posters{})
	ids := map[string]bool{}
	for _, p := range after {
		ids[p.extID] = true
	}
	for _, p := range before {
		if !ids[p.extID] {
			t.Errorf("%s lost its id", p.path)
		}
	}
	if len(after) != len(before)+1 {
		t.Errorf("parts = %d", len(after))
	}
}

func TestTidyFences(t *testing.T) {
	for _, in := range []string{
		"a\n````\n```\n<div>x</div>\n```\n````\nb",
		"a\n~~~\n<div>x</div>\n```\n~~~\nb",
		"a\n```\n<details><summary>Tips</summary>x</details>\n```\nb",
	} {
		if got := tidyBotText(in); !strings.Contains(got, "<div>x</div>") && !strings.Contains(got, "<details>") {
			t.Errorf("lost code tags: %q -> %q", in, got)
		}
	}
	got := tidyBotText("<details><summary>Prompt for AI agents</summary>\n```\ncode\n```\n</details>\nkeep <b>me</b>")
	if got != "keep me" {
		t.Errorf("dropped section with code = %q", got)
	}
}

// CodeRabbit's static analysis log (the scripts it ran, with their
// repository and output length), its usage receipt and links into its web
// app are chrome too (WP-432): on 2026-11-02 the log was 38% of what
// the mirror copied, and the links were the only place the GitHub copy's
// pull request number showed.
func TestTidyStaticAnalysis(t *testing.T) {
	in := "**coderabbitai[bot]** commented on `internal/core/events.go` line 610 on GitHub:\n\n<!-- coderabbit-ai-generated:v1 -->\n\n" +
		"**<picture><img src=\"amber-16.svg\" alt=\"\"></picture> Minor** · **Functional Correctness** · **Quick win**\n\n" +
		"<details>\n<summary><picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"review-details-dark.svg\"><img src=\"review-details.svg\" alt=\"\"></picture> Supported by static analysis</summary>\n<dl>\n<dd>\n\n" +
		"Script executed:\n\n```bash\nrg -n \"tab\" web/src/pages/admin/Admin.tsx | head -40\n```\n\nRepository: octo/waypoint\n\nLength of output: 2686\n\n---\n\n</dd>\n</dl>\n</details>\n\n" +
		"**The machine-scope notification link points to a tab that does not exist on the Admin page.**\n\n" +
		"[CWE-601](https://cwe.mitre.org/data/definitions/601.html) [View Security blast radius](https://app.coderabbit.ai/change-stack/octo/waypoint/pull/64/blast-radius?scope=ghh_x&securityFinding=b27f)\n\n" +
		"<details>\n<summary>Suggested fix</summary>\n<dl>\n<dd>\n\n```diff\n-\t\tlink = \"/admin?tab=sandbox\"\n+\t\tlink = \"/settings?tab=runners\"\n```\n\n</dd>\n</dl>\n</details>\n\n" +
		"### Usage-based review receipt - Mode: Continue automatically - Reviewed files: 1 - Waived: $0.25 (charged $0.00) - [View usage details](https://app.coderabbit.ai/settings/billing?tab=usage) " +
		"> [!NOTE] > This review exceeded your plan’s limits. [**Manage usage-based reviews**](https://app.coderabbit.ai/settings/billing?tab=usage).\n\n" +
		"<!-- mirror:line github 2 -->"
	got := tidyBotText(in)
	for _, want := range []string{"**The machine-scope notification link", "[CWE-601](https://cwe.mitre.org/data/definitions/601.html)", "Suggested fix", `+		link = "/settings?tab=runners"`} {
		if !strings.Contains(got, want) {
			t.Errorf("tidied lost %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"static analysis", "Script executed", "rg -n", "Repository: octo", "Length of output", "receipt", "Waived", "Manage usage", "blast radius", "pull/64"} {
		if strings.Contains(got, gone) {
			t.Errorf("tidied kept %q:\n%s", gone, got)
		}
	}
}

// A mirror's GitHub copy of a pull request can show a far larger diff
// than the pull request has (commits already on main counted as its
// changes), and CodeRabbit reviews all of it. Its findings on files this
// pull request doesn't change start no round, don't hold the merge and stay
// out of the work packet, until a push changes the file (WP-432).
func TestCopiedFindingsOutsideDiff(t *testing.T) {
	review := func(author, rest string) PRNote {
		return PRNote{ID: "801", Kind: "comment", Author: "mirror", Body: "**" + author + "** reviewed on GitHub:\n\n" + rest +
			"\n\n<!-- mirror:review github 5726966440 -->"}
	}
	other := "On `app/other.go` line 3:\n\n_⚠️ Potential issue_ | _🟠 Major_\n\nClose the file; it leaks."
	for _, tc := range []struct {
		name  string
		note  PRNote
		round bool
	}{
		{"bot finding on another file", review("coderabbitai[bot]", other), false},
		{"a person's", review("alice", other), true},
		{"names a changed file", review("coderabbitai[bot]", other+" Login in app/login.go opens it."), true},
		{"on a changed file", review("coderabbitai[bot]", "On `app/login.go` line 12:\n\n_⚠️ Potential issue_ | _🟠 Major_\n\nThe nil check is inverted."), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
				t.Fatal(err)
			}
			tc.note.At = be.clockNow()
			be.sg.setNotes(tc.note)
			pr := be.open()
			be.start(pr)
			be.ci("7777777aaaa", CheckSuccess)
			if got := len(be.jobs()) == 1; got != tc.round {
				t.Fatalf("a round = %v, want %v: %+v", got, tc.round, be.babysit())
			}
			todo, err := be.s.PRWorkPacket(be.ctx, be.agent, pr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(todo.Comments) == 1; got != tc.round {
				t.Errorf("in the work packet = %v, want %v:\n%s", got, tc.round, todo.Text)
			}
			if tc.round {
				return
			}
			if len(be.sg.merged()) != 1 {
				t.Fatalf("held by a finding on a file it doesn't change: %q", be.reasons(7))
			}
		})
	}

	be := newBabysitEnv(t)
	be.runnerOnline()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	note := review("coderabbitai[bot]", other)
	note.At = be.clockNow()
	be.sg.setNotes(note)
	pr := be.open()
	be.start(pr)
	if len(be.jobs()) != 0 {
		t.Fatal("a round before the file is changed")
	}
	be.push("8888888bbbb")
	be.changes("8888888bbbb", "app/other.go")
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if len(be.jobs()) != 1 {
		t.Fatalf("no round once the pull request changes the file: %+v", be.babysit())
	}
}

// changes adds files to what the pull request's diff at sha changes.
func (be *bsEnv) changes(sha string, paths ...string) {
	be.t.Helper()
	diff := sampleDiff
	for _, p := range paths {
		diff += fmt.Sprintf("diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n@@ -1 +1 @@\n-a\n+b\n", p)
	}
	be.sg.mu.Lock()
	be.sg.diffs[sha] = diff
	be.sg.mu.Unlock()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET files_sha = '' WHERE id = ?`, be.pr(7).ID); err != nil {
		be.t.Fatal(err)
	}
}
