package api

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAgentShort(t *testing.T) {
	for in, want := range map[string]string{
		"anthropic/claude-opus-5-5":          "opus 5.5",
		"anthropic/claude-haiku-5-5":         "haiku 5.5",
		"anthropic/claude-opus-4-1-20250805": "opus 4.1",
		"anthropic/claude-code":              "claude code",
		"google/gemini-cli":                  "gemini cli",
		"openai/gpt-5.1":                     "gpt-5.1",
		"openai/codex":                       "codex",
		"mcp":                                "mcp",
		"forge/robin":                        "forge login robin",
		"forge":                              "a forge login",
	} {
		if got := agentShort(in); got != want {
			t.Errorf("agentShort(%q) = %q, want %q", in, got, want)
		}
	}
}

// WP-224, WP-242: who wrote an issue is said once, an agent by its model.
func TestIssueTextNamesTheAuthorOnce(t *testing.T) {
	at := time.Date(2026, 10, 27, 22, 45, 0, 0, time.UTC)
	agent := "anthropic/claude-opus-5-5"
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-223", Title: "CI: move fan-out jobs", Type: "task",
		Status: core.Status{Name: "To Do"}, Agent: agent, ReporterAgent: agent, TextAgent: agent, Unreviewed: true,
		CreatedAt: at, UpdatedAt: at}}
	got := strings.SplitN(issueText(d, nil), "\n", 2)[0]
	if want := `WP-223 [To Do] "CI: move fan-out jobs" by opus 5.5 on 2026-10-27 22:45, not yet reviewed by a person`; got != want {
		t.Errorf("wp show:\n got %s\nwant %s", got, want)
	}

	why := core.WhyBoardOrder
	pts := 3.0
	d.Points, d.Priority, d.Status.Name = &pts, 0, "In Progress"
	next := agentTaskText(&core.AgentTask{Issue: d, Why: why}, taskText{})
	want := `WP-223 "CI: move fan-out jobs" by opus 5.5, not yet reviewed by a person
Written ` + age(time.Since(at)) + ` ago.

Assess and complete the task as appropriate. When done, run ` + "`wp ship WP-223 -m \"what you did\" --summary \"your reply to the person\"`; if it ships cleanly, that reply ends your turn (add `--check \"CMD\"` to run your checks first, and `--shot URL` for each new frontend change)."
	if !strings.HasPrefix(next, want) {
		t.Errorf("wp next:\n got %q\nwant %q", next, want)
	}

	// Work left stranded on it (WP-567): where to resume, after the meta line.
	d.Points = nil
	at2 := time.Date(2026, 11, 2, 14, 46, 0, 0, time.UTC)
	res := agentTaskText(&core.AgentTask{Issue: d, Why: why, Resume: &core.StrandedWork{At: at2, Why: "clear", Machine: "devhost",
		Top: "/projects/management/waypoint", Branch: "wp-437-packet-triage", Files: []string{" M internal/core/triage.go"}, FileCount: 1,
		Background: []string{"make check"}}}, taskText{})
	if want := "\nResume: this task's work was left unfinished 2026-11-02 14:46 UTC, when its session was cleared (/clear). It is in `/projects/management/waypoint` on devhost, branch `wp-437-packet-triage` (1 file uncommitted). Changed: internal/core/triage.go. Background work that died with it: make check."; !strings.Contains(res, want) {
		t.Errorf("wp next with stranded work:\n%s", res)
	}

	robin := &core.UserRef{ID: 1, DisplayName: "Robin"}
	d.Reporter, d.ReporterAgent, d.Unreviewed, d.ReviewedBy = robin, "", false, robin
	if got := provenance(d, false); got != "by Robin, text by opus 5.5 (reviewed by Robin)" {
		t.Errorf("person's issue, agent's text: %s", got)
	}
}

func TestOnlyClaim(t *testing.T) {
	task := &core.AgentTask{Issue: &core.IssueDetail{Issue: &core.Issue{Key: "WP-7"}}}
	// Other keys are looked up (still held?): TestNextShowsKeylessDoneForTheOnlyTask.
	s := &Server{}
	for h, want := range map[string]bool{"": false, "none": true, "wp-7": true, " WP-7 ,none": true} {
		r := httptest.NewRequest("POST", "/api/v1/agent/next", nil)
		if h != "" {
			r.Header.Set("X-Waypoint-Claims", h)
		}
		if got := s.onlyClaim(r, task); got != want {
			t.Errorf("claims %q: %v, want %v", h, got, want)
		}
	}
	task.Related = []*core.Issue{{Key: "WP-8"}}
	r := httptest.NewRequest("POST", "/api/v1/agent/batch", nil)
	r.Header.Set("X-Waypoint-Claims", "none")
	if s.onlyClaim(r, task) {
		t.Error("a batch has more than one")
	}
}

// WP-449: a task's answer carries its comments after the description, each
// cut at core.CommentClip, and past 20 the oldest few and the latest.
func TestAgentTaskTextComments(t *testing.T) {
	at := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-9", Title: "T", Description: "Do it.", Status: core.Status{Name: "To Do"}}}
	robin := &core.UserRef{Username: "robin"}
	task := &core.AgentTask{Issue: d, Comments: []core.Comment{
		{ID: 1, Body: "Not that way.\nThis way.", CreatedAt: at, Author: robin},
		{ID: 2, Body: strings.Repeat("é", core.CommentClip), CreatedAt: at, Agent: "anthropic/claude-code", Unreviewed: true},
	}}
	got := agentTaskText(task, taskText{})
	if !strings.Contains(got, "Do it.\n\nComments, oldest first") ||
		!strings.Contains(got, "  [#1 2026-11-02 14:00 @robin] Not that way.\n    This way.\n") ||
		!strings.Contains(got, "[#2 2026-11-02 14:00 agent anthropic/claude-code, unreviewed] ") {
		t.Errorf("comments:\n%s", got)
	}
	if !strings.Contains(got, "é… (cut; wp show WP-9 has it all)\n") || strings.Count(got, "é") != core.CommentClip/2 {
		t.Errorf("a long comment isn't cut on a rune at %d bytes: %d é", core.CommentClip, strings.Count(got, "é"))
	}

	task.Comments = nil
	for n := 1; n <= 30; n++ {
		task.Comments = append(task.Comments, core.Comment{ID: int64(n), Body: fmt.Sprintf("c%d.", n), CreatedAt: at, Author: robin})
	}
	got = agentTaskText(task, taskText{})
	for n := 1; n <= 30; n++ {
		shown := n <= 5 || n > 30-(20-5)
		if strings.Contains(got, fmt.Sprintf("] c%d.\n", n)) != shown {
			t.Errorf("comment %d shown: %v, want %v", n, !shown, shown)
		}
	}
	if !strings.Contains(got, "  … 10 more between (wp show WP-9 has every comment)\n  [#16 ") {
		t.Errorf("the gap:\n%s", got)
	}
}

// WP-733: a sub-task's answer carries its parent's description after its own
// text, under its own heading, cut to the room the task's text leaves.
func TestAgentTaskTextParent(t *testing.T) {
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-9", Title: "T", Description: "Do it.", Status: core.Status{Name: "To Do"},
		Parent: &core.IssueRef{Key: "WP-1", Title: "Epic"}}}
	task := &core.AgentTask{Issue: d, ParentText: "The broader problem."}
	got := agentTaskText(task, taskText{})
	if !strings.Contains(got, "Do it.\n\nPart of WP-1 \"Epic\":\nThe broader problem.\n") || strings.Contains(got, "part of: ") {
		t.Errorf("parent:\n%s", got)
	}

	task.ParentText = strings.Repeat("é", parentMax)
	got = agentTaskText(task, taskText{})
	if !strings.Contains(got, "é… (cut; `wp show WP-1` has the rest)\n") || strings.Count(got, "é") != parentMax/2 {
		t.Errorf("a long parent isn't cut at %d bytes: %d é", parentMax, strings.Count(got, "é"))
	}
	d.Description = strings.Repeat("x", 4000)
	task.Comments = []core.Comment{{ID: 1, Body: strings.Repeat("y", 1000), Author: &core.UserRef{Username: "robin"}}}
	got = agentTaskText(task, taskText{})
	if n := strings.Count(got, "é"); n == 0 || n >= parentMax/2 {
		t.Errorf("the task's own text leaves less room: %d é", n)
	}
	task.Comments = append(task.Comments, task.Comments[0], task.Comments[0], task.Comments[0])
	got = agentTaskText(task, taskText{})
	if strings.Contains(got, "é") || !strings.Contains(got, "Part of WP-1 \"Epic\":\n(Its description is in `wp show WP-1`.)\n") {
		t.Errorf("no room left:\n%s", got[len(got)-400:])
	}

	task.ParentText = ""
	if got := agentTaskText(task, taskText{}); !strings.Contains(got, "part of: WP-1 Epic") || strings.Contains(got, "Part of WP-1") {
		t.Errorf("no parent text: the meta line names it")
	}
}

func TestCrossLine(t *testing.T) {
	d := &core.IssueDetail{Issue: &core.Issue{Key: "APP-9", Unreviewed: true}}
	if crossLine(d) != "" {
		t.Error("an ordinary task")
	}
	d.FiledFrom = &core.FiledFrom{Project: "WP", Repo: "Robin/waypoint"}
	if l := crossLine(d); !strings.Contains(l, "Filed from WP (Robin/waypoint)") || !strings.Contains(l, "wp decline APP-9 -m") {
		t.Errorf("the disclaimer: %q", l)
	}
	d.FiledFrom.Here = true
	if crossLine(d) != "" {
		t.Error("the filer's side is told it filed it, not to decline it")
	}
	d.FiledFrom.Here, d.Unreviewed = false, false
	if crossLine(d) != "" {
		t.Error("a person reviewed it: it stands")
	}
	at := time.Now()
	d.FiledFrom.DeclinedAt, d.FiledFrom.DeclinedBy, d.FiledFrom.Declined = &at, "anthropic/claude-opus-5-5", "no UI\nhere"
	if l := crossLine(d); !strings.HasPrefix(l, "Declined: filed from WP") || !strings.Contains(l, "no UI here") {
		t.Errorf("the decline: %q", l)
	}
}

// WP-545, WP-804: linked tasks and the ones a task's text names are one list
// after its sub-tasks, a title and status each.
func TestAgentTaskTextRelated(t *testing.T) {
	done := time.Now().Add(-73 * time.Hour)
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-9", Title: "T", Description: "Like WP-3.", Status: core.Status{Name: "To Do"}},
		Children:  []*core.Issue{{Key: "WP-10", Title: "Child", Status: core.Status{Name: "To Do"}}},
		Relations: []core.Relation{{Kind: "relates", Issue: core.IssueRef{Key: "WP-4", Title: "Linked"}, State: "todo", Status: "To Do"}}}
	task := &core.AgentTask{Issue: d, Named: []*core.Issue{{Key: "WP-3", Title: "Earlier", Status: core.Status{Name: "Done"}, ResolvedAt: &done}}}
	want := "\nRelated:\n WP-4 \"Linked\" [To Do]\n WP-3 \"Earlier\" [Done 3d ago]\n`wp show KEY1 KEY2...` for more information.\n"
	got := agentTaskText(task, taskText{})
	if !strings.Contains(got, want) {
		t.Errorf("related:\n%s", got)
	}
	if strings.Index(got, "Sub-tasks:") > strings.Index(got, "Related:") {
		t.Errorf("sub-tasks come first:\n%s", got)
	}
	d.Relations, task.Named = nil, nil
	if got := agentTaskText(task, taskText{}); strings.Contains(got, "Related:") || strings.Contains(got, "wp show KEY1") {
		t.Errorf("none related:\n%s", got)
	}
}

func TestRelationNote(t *testing.T) {
	rels := []core.Relation{
		{Kind: "blocked_by", Issue: core.IssueRef{Key: "WP-1", Title: "A"}, Status: "To Do"},
		{Kind: "blocks", Issue: core.IssueRef{Key: "WP-2", Title: "B"}, Status: "To Do"},
	}
	want := "\nRelated:\n WP-1 \"A\" [To Do] (blocks this)\n WP-2 \"B\" [To Do] (waits on this)\n`wp show KEY1 KEY2...` for more information.\n"
	if got := relatedText(rels, nil, time.Now()); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestClipTitle(t *testing.T) {
	long := strings.Repeat("word ", 40)
	more := " Then " + strings.Repeat("carry on ", 12)
	for _, c := range []struct{ name, in, want string }{
		{"short stays whole", "Fix it. Then ship it.", "Fix it. Then ship it."},
		{"first sentence", "I'd like a reports page to see token usage over time, per repo, per task, and how various things relate to that. For instance: tool calls, before/after a change.",
			"I'd like a reports page to see token usage over time, per repo, per task, and how various things relate to that..."},
		{"keeps a question mark", "Similar to the reduce-approvals feature, can we implement any tools to help a repo determine how to reduce token usage? For instance, identifying repeated confusion.",
			"Similar to the reduce-approvals feature, can we implement any tools to help a repo determine how to reduce token usage?..."},
		{"abbreviation and version are no sentence end", "Waiting on small things, e.g. the wp.go flag in v0.2.1 that everyone keeps tripping over in practice, and then more of it. Second sentence here.",
			"Waiting on small things, e.g. the wp.go flag in v0.2.1 that everyone keeps tripping over in practice, and then more of it..."},
		{"no sentence end, cut at a word", long, strings.TrimSpace(strings.Repeat("word ", 32)) + "..."},
		{"list number", "1. Add a reports page that shows token usage per repo over time." + more, "1. Add a reports page that shows token usage per repo over time..."},
		{"title at the start", "Dr. Smith asked for a rebuilt runbook covering every region we run in." + more, "Dr. Smith asked for a rebuilt runbook covering every region we run in..."},
		{"dotted letters", "Rewrite the deployment runbook for the whole U.S. Army team so that every region can be rebuilt." + more, "Rewrite the deployment runbook for the whole U.S. Army team so that every region can be rebuilt..."},
		{"lower case carries on", "Rebuild the runbook for every region we run in. then write the migration and move them over." + more, "Rebuild the runbook for every region we run in. then write the migration and move them over..."},
		{"ellipsis is no end", "Ship it to every region we run in today... Then write the migration and move them all over." + more, "Ship it to every region we run in today... Then write the migration and move them all over..."},
		{"etc. can end a sentence", "Clean the caches, the logs, the temporary files, the old builds, etc." + more, "Clean the caches, the logs, the temporary files, the old builds, etc..."},
		{"a long token isn't lost", "See " + strings.Repeat("a", 200), "See " + strings.Repeat("a", 156) + "..."},
		{"only punctuation before a long token", ": " + strings.Repeat("a", 200), ": " + strings.Repeat("a", 158) + "..."},
	} {
		if got := clipTitle(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// WP-460: what was live when a task was written, and what is now.
func TestWrittenLine(t *testing.T) {
	created := time.Date(2026, 11, 2, 14, 5, 0, 0, time.UTC)
	now := created.Add(4*24*time.Hour + time.Hour)
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-9", CreatedAt: created}}
	if l := writtenLine(d, false, now); l != "" {
		t.Errorf("nothing to add to wp show's dated head: %q", l)
	}
	if l := writtenLine(d, true, now); l != "Written 4d ago.\n" {
		t.Errorf("no environment: %q", l)
	}
	d.Written = &core.Written{Environment: "production", Version: "v0.52.0", Commit: "02ad6d4abcdef",
		LiveVersion: "v0.58.6", LiveCommit: "39468e2abcdef", Releases: 41}
	if l, want := writtenLine(d, true, now), "Written 4d ago, when production ran v0.52.0 (02ad6d4); it runs v0.58.6 (39468e2) now, 41 releases later.\n"; l != want {
		t.Errorf("wp next:\n got %q\nwant %q", l, want)
	}
	if l, want := writtenLine(d, false, now), "Written when production ran v0.52.0 (02ad6d4); it runs v0.58.6 (39468e2) now, 41 releases later.\n"; l != want {
		t.Errorf("wp show:\n got %q\nwant %q", l, want)
	}
	d.Written = &core.Written{Environment: "production", Commit: "02ad6d4abcdef", LiveCommit: "02ad6d4abcdef"}
	if l, want := writtenLine(d, false, now), "Written when production ran 02ad6d4; it still does.\n"; l != want {
		t.Errorf("unchanged, commits only:\n got %q\nwant %q", l, want)
	}
	d.Written = &core.Written{Version: "v0.52.0"}
	if l, want := writtenLine(d, false, now), "Written when v0.52.0 was live.\n"; l != want {
		t.Errorf("its environment deleted:\n got %q\nwant %q", l, want)
	}
}

// WP-770: the answer offers a way to decline only a task that may need it.
func TestTaskClosingOffersDeclineWhenWarranted(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	robin := &core.UserRef{ID: 1, DisplayName: "Robin"}
	fresh := func() *core.IssueDetail {
		return &core.IssueDetail{Issue: &core.Issue{Key: "WP-7", CreatedAt: now.Add(-time.Hour)}}
	}
	const invite = "tell the user and decline it with `wp hold WP-7 -m \"why\"`"
	if got := taskClosing(fresh(), taskText{}, now); strings.Contains(got, "decline") {
		t.Errorf("a recent person's task: %s", got)
	}
	for name, c := range map[string]struct {
		set  func(*core.IssueDetail)
		want string
	}{
		"releases":   {func(d *core.IssueDetail) { d.Written = &core.Written{Releases: declineReleases} }, "This task is 20 releases old:"},
		"age":        {func(d *core.IssueDetail) { d.CreatedAt = now.Add(-declineAge) }, "This task is 14d old:"},
		"unreviewed": {func(d *core.IssueDetail) { d.TextAgent, d.Unreviewed = "anthropic/claude-opus-5-5", true }, "This task hasn't been reviewed by a person:"},
		"risk":       {func(d *core.IssueDetail) { d.Risk = &core.ProvenanceRisk{Level: core.RiskHigh} }, "This task is high risk:"},
		"both": {func(d *core.IssueDetail) {
			d.Written, d.TextAgent, d.Unreviewed = &core.Written{Releases: 30}, "anthropic/claude-opus-5-5", true
		}, "This task is 30 releases old and hasn't been reviewed by a person:"},
	} {
		d := fresh()
		c.set(d)
		if got := taskClosing(d, taskText{}, now); !strings.Contains(got, c.want) || !strings.Contains(got, invite) {
			t.Errorf("%s: %s", name, got)
		}
	}
	d := fresh()
	d.TextAgent, d.ReviewedBy = "anthropic/claude-opus-5-5", robin
	if got := taskClosing(d, taskText{}, now); strings.Contains(got, "decline") {
		t.Errorf("a person reviewed it: %s", got)
	}
	if got := taskClosing(fresh(), taskText{Keyless: true, ReviewFirst: true}, now); !strings.Contains(got, "`wp ship --review -m \"what you did\" --summary") || !strings.Contains(got, "`wp release -m \"why\"`") {
		t.Errorf("keyless, reviewed first: %s", got)
	}
}

// WP-770: autopilot's answer says who assigned the task and drops what the
// session can't use; a person's "Goal: none" is gone too.
func TestAssignedAnswerOpensWithAutopilot(t *testing.T) {
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-7", Title: "Fix it", CreatedAt: time.Now().Add(-time.Hour)}, Goal: &core.IssueGoal{Grounding: "person_asked"}}
	got := agentTaskText(&core.AgentTask{Issue: d, Resumed: true}, taskText{Assigned: true})
	if !strings.HasPrefix(got, "Autopilot assigned WP-7 to you: \"Fix it\" by") || strings.Contains(got, "You already hold") || strings.Contains(got, "Goal:") {
		t.Errorf("assigned: %s", got)
	}
	if got := agentTaskText(&core.AgentTask{Issue: d, Resumed: true}, taskText{}); !strings.HasPrefix(got, "(You already hold this task.)\nWP-7 ") {
		t.Errorf("not assigned: %s", got)
	}
	// A session a person started for the task (WP-1037) hears who did.
	if got := agentTaskText(&core.AgentTask{Issue: d}, taskText{StartedBy: "Robin"}); !strings.HasPrefix(got, "Robin started this session for WP-7: \"Fix it\" by") {
		t.Errorf("started: %s", got)
	}
}

// A plan with a story shows the story first, whose intent it is, and that
// approving the plan approved only the story (WP-190).
func TestPageTextStory(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	robin := &core.UserRef{ID: 1, Username: "robin", DisplayName: "Robin"}
	sid := int64(42)
	p := &core.Page{ID: 7, Title: "Fast boards", Category: "plan", Body: "Step one.", TextAgent: "anthropic/claude-code",
		Story: "As the owner, I want fast boards.\n\n- Smooth scrolling", StoryAgent: "waypoint/story", StoryModel: "opencode/big-pickle",
		SessionID: &sid, Prompt: &core.PagePrompt{Text: "Make boards fast", At: at, SessionID: sid}}
	got := pageText(p)
	for _, want := range []string{
		"Story (suggested by waypoint/story, model opencode/big-pickle; no person has kept it, so it is no one's intent yet):\n  As the owner, I want fast boards.\n  \n  - Smooth scrolling\n",
		`Asked for in session #42 on`,
		`"Make boards fast"`,
		"Plan (agent anthropic/claude-code's text; approving the page keeps only the story above as a person's intent):\nStep one.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("suggested story: missing %q in\n%s", want, got)
		}
	}
	if strings.Index(got, "Story (") > strings.Index(got, "Step one.") {
		t.Error("the story should come before the plan")
	}

	// Reviewed line by line before it had a story (audit #973.5): the text
	// was approved, whatever happened to the story after.
	p.StoryAgent, p.StoryBy, p.StoryAt, p.ReviewedBy, p.ReviewedAt, p.StoryStale, p.Prompt = "", robin, &at, robin, &at, true, nil
	if got = pageText(p); strings.Contains(got, "approved its story") {
		t.Errorf("a line-by-line review read as a story's:\n%s", got)
	}

	p.ReviewedOnStory = true
	got = pageText(p)
	for _, want := range []string{
		"by agent anthropic/claude-code; Robin approved its story, not its text",
		"Story (Robin's intent, kept ",
		"(The plan changed after this story was written or kept.)",
		"Written in session #42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kept story: missing %q in\n%s", want, got)
		}
	}
}

// WP-1035: the goal line gains the parent goals the task's arm shows, on
// its own line; the goal arm prints today's line alone.
func TestGoalLineChain(t *testing.T) {
	goal := &core.KItem{Type: core.KDesign, Kind: "goal", ID: 2, Title: "buttons have a consistent style"}
	d := &core.IssueDetail{Issue: &core.Issue{Key: "WP-7"}, Goal: &core.IssueGoal{Grounding: "person_asked", Goal: goal}}
	plain := goalLine(d)
	if !strings.HasPrefix(plain, "Goal: ") || strings.Count(plain, "\n") != 1 || strings.Contains(plain, "under") {
		t.Errorf("goal arm: %q", plain)
	}
	d.Goal.Chain = []core.GoalStep{{ID: 1, Title: "The UI is intuitive"}}
	got := goalLine(d)
	if !strings.HasPrefix(got, plain) || !strings.HasSuffix(got, "\n  under goal #1 \"The UI is intuitive\"\n") {
		t.Errorf("chain: %q", got)
	}
	d.Goal.Chain = []core.GoalStep{{ID: 3, Title: "Near…", Clipped: true}, {ID: 1, Title: "Far"}}
	d.Goal.Above = 2
	got = goalLine(d)
	if !strings.HasSuffix(got, "  under goal #3 \"Near…\" › goal #1 \"Far\" (2 more left out: wp goals shows them)\n") {
		t.Errorf("above: %q", got)
	}
	// Every parent left out: the count still shows.
	d.Goal.Chain = nil
	d.Goal.Above = 2
	if got := goalLine(d); !strings.HasSuffix(got, "  (2 parent goals left out: wp goals shows them)\n") {
		t.Errorf("all left out: %q", got)
	}
	d.Goal.Above = 1
	if got := goalLine(d); !strings.HasSuffix(got, "  (1 parent goal left out: wp goals shows them)\n") {
		t.Errorf("one left out: %q", got)
	}
	// Nothing to say without a goal, chain or not.
	d.Goal.Goal = nil
	if got := goalLine(d); got != "" {
		t.Errorf("no goal: %q", got)
	}
}
