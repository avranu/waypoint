package core

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// prPayload is a pull_request webhook body, GitHub-style, with the forge's
// dates and size.
func prPayload(action string, pr ForgePR, extra map[string]any) map[string]any {
	p := map[string]any{
		"number": pr.Number, "title": pr.Title, "body": pr.Body, "html_url": "https://git.example.com/me/app/pulls/" + fmt.Sprint(pr.Number),
		"state": map[bool]string{true: "closed", false: "open"}[pr.State == "closed" || pr.State == "merged"], "merged": pr.State == "merged", "draft": pr.State == "draft",
		"user": map[string]string{"login": pr.Author}, "head": map[string]string{"ref": pr.HeadRef, "sha": pr.HeadSHA}, "base": map[string]string{"ref": "main"},
		"created_at": "2026-10-04T09:00:00Z",
	}
	for k, v := range extra {
		p[k] = v
	}
	return map[string]any{"action": action, "pull_request": p, "repository": map[string]any{"full_name": "me/app"},
		"sender": map[string]string{"login": cmp(pr.Author, "sam")}}
}

func (pe *prEnv) history(prID int64) []string {
	pe.t.Helper()
	rows, err := pe.s.db.R.QueryContext(pe.ctx, `SELECT kind, actor, bot FROM pr_events WHERE pr_id = ? ORDER BY created_at, id`, prID)
	if err != nil {
		pe.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, actor string
		var bot bool
		if err := rows.Scan(&kind, &actor, &bot); err != nil {
			pe.t.Fatal(err)
		}
		if bot {
			actor += "(bot)"
		}
		out = append(out, kind+":"+actor)
	}
	return out
}

// audits are the audit entries about pull requests and the review note
// (setting up the forge and repo records its own).
func (pe *prEnv) audits() []string {
	pe.t.Helper()
	rows, err := pe.s.db.R.QueryContext(pe.ctx, `SELECT action, actor, coalesce(user_id, 0), about_agent FROM audit_log
		WHERE action LIKE 'pr.%' OR action IN ('settings.review_note', 'settings.ship_reply') ORDER BY id`)
	if err != nil {
		pe.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, actor, about string
		var user int64
		if err := rows.Scan(&action, &actor, &user, &about); err != nil {
			pe.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s by %s/%d about %s", action, actor, user, about))
	}
	return out
}

func (pe *prEnv) send(event string, payload map[string]any) {
	pe.t.Helper()
	if err := pe.hook(pe.f, event, payload); err != nil {
		pe.t.Fatal(err)
	}
}

func TestPullRequestHistory(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.fg.ownLogin = "robin"
	pe.policy(MergePolicyInput{AutoMerge: ptr(false)})
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	pr := ForgePR{Number: 12, Title: "Add search", Author: "robin", HeadRef: "search", HeadSHA: "aaaaaaa1111", State: "open",
		Body: "Adds search.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"}
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	pe.send("pull_request", prPayload("opened", pr, map[string]any{"additions": 40, "deletions": 3, "changed_files": 2, "commits": 1}))
	pe.drain()
	rec, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 12)
	if rec.Agent != "anthropic/claude-opus-5-5" || rec.OpenedAt == nil || rec.OpenedAt.Day() != 4 || rec.Additions == nil || *rec.Additions != 40 {
		t.Fatalf("record = %+v", rec)
	}

	pe.setClock(pe.clockNow().Add(time.Hour))
	pr.HeadSHA = "bbbbbbb2222"
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	pe.send("pull_request", prPayload("synchronize", pr, nil))
	pe.drain()
	approve := map[string]any{"action": "submitted", "review": map[string]any{"id": 77, "user": map[string]string{"login": "alex"}, "state": "APPROVED",
		"body": "LGTM", "commit_id": "bbbbbbb2222", "html_url": "https://git.example.com/me/app/pulls/12#r77"},
		"pull_request": prPayload("", pr, nil)["pull_request"], "repository": map[string]any{"full_name": "me/app"}}
	pe.send("pull_request_review", approve)
	pe.send("pull_request_review", approve)
	pe.send("pull_request_review", map[string]any{"action": "submitted", "review": map[string]any{"id": 78, "user": map[string]string{"login": "robin"},
		"state": "COMMENTED", "body": "**Automated review** …\n<!-- waypoint-review sha=bbbbbbb2222 rule=1 -->"},
		"pull_request": prPayload("", pr, nil)["pull_request"], "repository": map[string]any{"full_name": "me/app"}})
	pe.send("pull_request_review", map[string]any{"action": "submitted", "review": map[string]any{"id": 79, "user": map[string]string{"login": "coderabbitai[bot]"},
		"state": "COMMENTED", "body": "Nitpicks"},
		"pull_request": prPayload("", pr, nil)["pull_request"], "repository": map[string]any{"full_name": "me/app"}})
	pe.send("issue_comment", map[string]any{"action": "created", "issue": map[string]any{"number": 12, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 501, "body": "Can you add a test?", "user": map[string]string{"login": "alex"}}, "repository": map[string]any{"full_name": "me/app"}})
	pe.send("pull_request_review_comment", map[string]any{"action": "created",
		"comment":      map[string]any{"id": 601, "body": "**P1** high · …\n<!-- waypoint-finding -->", "user": map[string]string{"login": "robin"}},
		"pull_request": prPayload("", pr, nil)["pull_request"], "repository": map[string]any{"full_name": "me/app"}})

	pe.setClock(pe.clockNow().Add(2 * time.Hour))
	pr.State = "merged"
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	pe.send("pull_request", prPayload("closed", pr, map[string]any{"merged_at": "2026-10-05T15:00:00Z", "merged_by": map[string]string{"login": "alex"}}))

	want := []string{"opened:robin", "pushed:robin", "reviewed:coderabbitai[bot](bot)", "approved:alex", "commented:alex", "commented:robin(bot)", "merged:alex"}
	got := pe.history(rec.ID)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("history = %v\nwant      %v", got, want)
	}
	if a := pe.audits(); len(a) != 2 || a[0] != "pr.approved by alex/0 about anthropic/claude-opus-5-5" || a[1] != "pr.merged by alex/0 about anthropic/claude-opus-5-5" {
		t.Errorf("audit = %v", a)
	}

	i := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "Search"})
	if _, err := pe.s.AddLink(pe.ctx, pe.admin, i.Key, "https://git.example.com/me/app/pulls/12", ""); err != nil {
		t.Fatal(err)
	}
	agent := &Actor{User: pe.admin.User, Agent: "anthropic/claude-code"}
	if _, err := pe.s.AgentTake(pe.ctx, agent, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	pe.setClock(pe.clockNow().Add(90 * time.Minute))
	if _, err := pe.s.AgentDone(pe.ctx, agent, i.Key, AgentDoneInput{Usage: &AgentUsage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.5}}); err != nil {
		t.Fatal(err)
	}
	pe.s.Wait()

	list, err := pe.s.ListPullRequests(pe.ctx, pe.admin, PRQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Pulls) != 1 {
		t.Fatalf("pulls = %+v", list.Pulls)
	}
	p := list.Pulls[0]
	if p.State != "merged" || p.MergedBy != "alex" || p.Pushes != 1 || p.Comments.People != 1 || p.Comments.Bots != 1 ||
		p.Reviews.People != 1 || p.Reviews.Bots != 3 || len(p.Approvals) != 1 || len(p.Issues) != 1 ||
		p.AgentSeconds != 90*60 || p.InputTokens != 1000 || p.HoursOpen == nil || *p.HoursOpen != 30 {
		t.Errorf("summary = %+v (hours %v)", p, p.HoursOpen)
	}
	if !slices.Contains(p.Agents, "anthropic/claude-opus-5-5") || !slices.Contains(p.Agents, "anthropic/claude-code") {
		t.Errorf("agents = %v", p.Agents)
	}
	if list.Totals.Merged != 1 || list.Totals.Opened != 1 || list.Totals.HumanApprovals != 1 || list.Totals.MedianHoursToMerge == nil {
		t.Errorf("totals = %+v", list.Totals)
	}
	for q, n := range map[PRQuery]int{
		{Repo: "ME/app"}: 1, {Repo: "me/other"}: 0, {Agent: "opus"}: 1, {Agent: "claude code"}: 1, {Agent: "codex"}: 0, {State: "open"}: 0, {State: "merged"}: 1, {Text: "#12"}: 1, {Text: "nothing"}: 0,
	} {
		l, err := pe.s.ListPullRequests(pe.ctx, pe.admin, q)
		if err != nil || len(l.Pulls) != n {
			t.Errorf("%+v: %d pulls, %v", q, len(l.Pulls), err)
		}
	}

	d, err := pe.s.GetPullRequest(pe.ctx, pe.admin, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, it := range d.Timeline {
		kinds = append(kinds, it.Kind)
	}
	for _, k := range []string{"opened", "bot_review", "pushed", "approved", "merged", "agent_run", "issue_status"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("timeline lacks %s: %v", k, kinds)
		}
	}
	if len(d.BotRuns) != 2 || len(d.AgentTime) != 1 || d.AgentTime[0].Seconds != 90*60 {
		t.Errorf("detail: %d bot runs, agent time %+v", len(d.BotRuns), d.AgentTime)
	}

	other := pe.user("eve", false)
	if l, err := pe.s.ListPullRequests(pe.ctx, other, PRQuery{}); err != nil || len(l.Pulls) != 0 {
		t.Errorf("outsider sees %v, %v", l, err)
	}
	if _, err := pe.s.GetPullRequest(pe.ctx, other, rec.ID); err == nil {
		t.Error("outsider read a pull request")
	}
}

func TestForgejoReviewEvents(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pr := ForgePR{Number: 3, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "ccccccc3333", State: "open"}
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	review := func(event, sender, content string) map[string]any {
		return map[string]any{"action": "reviewed", "number": 3, "commit_id": "ccccccc3333", "pull_request": prPayload("", pr, nil)["pull_request"],
			"review": map[string]any{"type": "pull_request_review_" + strings.TrimPrefix(event, "pull_request_"), "content": content},
			"sender": map[string]string{"login": sender}, "repository": map[string]any{"full_name": "me/app"}}
	}
	pe.send("pull_request_rejected", review("pull_request_rejected", "alex", "Needs tests"))
	pe.send("pull_request_approved", review("pull_request_approved", "alex", "Thanks"))
	pe.send("pull_request_approved", review("pull_request_approved", "alex", "Thanks"))
	pe.send("pull_request_comment", review("pull_request_comment", "llm-review", "Low-trust review"))
	if len(pe.drain()) != 0 {
		t.Error("a review set the bots off")
	}
	rec, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 3)
	if rec == nil {
		t.Fatal("not recorded")
	}
	got := pe.history(rec.ID)
	slices.Sort(got)
	want := []string{"approved:alex", "changes_requested:alex", "opened:sam", "reviewed:llm-review(bot)"}
	if !slices.Equal(got, want) {
		t.Errorf("history = %v", got)
	}
	if a := pe.audits(); len(a) != 2 || !strings.HasPrefix(a[0], "pr.changes_requested by alex") {
		t.Errorf("audit = %v", a)
	}
}

func TestGitLabMergeRequestHistory(t *testing.T) {
	pe := newPREnv(t, "gitlab")
	secret, _ := pe.s.ForgeSecret(pe.ctx, pe.admin, pe.f.ID)
	send := func(payload string) {
		h := http.Header{}
		h.Set("X-Gitlab-Token", secret)
		if err := pe.s.HandleForgeWebhook(pe.ctx, pe.f.ID, h, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	setState := func(state string) {
		pe.fg.setPR(pe.f, "me/app", ForgePR{Number: 8, Title: "Tidy", Author: "sam", HeadRef: "tidy", HeadSHA: "ddddddd4444", State: state}, sampleDiff)
	}
	setState("open")
	mr := func(action, state string) string {
		return `{"object_kind":"merge_request","user":{"username":"alex"},"project":{"path_with_namespace":"me/app"},
			"object_attributes":{"iid":8,"title":"Tidy","state":"` + state + `","action":"` + action + `","url":"https://git.example.com/me/app/-/merge_requests/8",
			"source_branch":"tidy","target_branch":"main","created_at":"2026-10-03 08:00:00 UTC","last_commit":{"id":"ddddddd4444"}}}`
	}
	send(mr("open", "opened"))
	send(mr("approved", "opened"))
	setState("merged")
	send(mr("merge", "merged"))
	rec, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 8)
	if rec == nil || rec.MergedBy != "alex" || rec.OpenedAt == nil || rec.OpenedAt.Day() != 3 || rec.MergedAt == nil {
		t.Fatalf("record = %+v", rec)
	}
	got := pe.history(rec.ID)
	slices.Sort(got)
	if want := []string{"approved:alex", "merged:alex", "opened:sam"}; !slices.Equal(got, want) {
		t.Errorf("history = %v", got)
	}
}

func TestRefreshPullRequests(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.policy(MergePolicyInput{AutoMerge: ptr(false)})
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	merged := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	opened := merged.Add(-48 * time.Hour)
	known := ForgePR{Number: 1, Title: "Known", Author: "sam", HeadRef: "k", HeadSHA: "1111111aaaa", State: "open"}
	pe.fg.setPR(pe.f, "me/app", known, sampleDiff)
	pe.send("pull_request", prPayload("opened", known, nil))
	pe.drain()
	newer := known
	newer.HeadSHA, newer.Title = "9999999zzzz", "Known, retitled"
	pe.fg.listed = []*ForgePR{
		{Number: 1, Title: newer.Title, Author: "sam", HeadRef: "k", HeadSHA: newer.HeadSHA, State: "open"},
		{Number: 2, Title: "Unseen", Author: "sam", HeadRef: "u", HeadSHA: "2222222bbbb", State: "open", OpenedAt: &opened},
		{Number: 3, Title: "Old", Author: "sam", HeadRef: "o", HeadSHA: "3333333cccc", State: "merged", OpenedAt: &opened, MergedAt: &merged, MergedBy: "alex"},
	}
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 0 {
		t.Fatalf("a refresh set the bots off: %+v", runs)
	}
	k, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 1)
	if k.HeadSHA != "1111111aaaa" || k.Title != "Known, retitled" {
		t.Errorf("known: %+v", k)
	}
	old, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 3)
	if old == nil || old.State != "merged" || !old.MergedAt.Equal(merged) {
		t.Fatalf("old: %+v", old)
	}
	if h := pe.history(old.ID); strings.Join(h, " ") != "opened:sam merged:alex" {
		t.Errorf("old history = %v", h)
	}
	if a := pe.audits(); len(a) != 0 {
		t.Errorf("history from a refresh was audited: %v", a)
	}
	missed := ForgePR{Number: 4, Title: "Missed", Author: "sam", HeadRef: "m", HeadSHA: "4444444dddd", State: "open"}
	pe.fg.setPR(pe.f, "me/app", missed, sampleDiff)
	pe.fg.listed = []*ForgePR{&missed}
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 0 {
		t.Fatalf("first sighting set the bots off: %+v", runs)
	}
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 1 || runs[0].Reason != "opened" {
		t.Fatalf("missed webhook: %+v", runs)
	}
	if m, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 4); m.HeadSHA != missed.HeadSHA {
		t.Errorf("missed head: %+v", m)
	}
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 0 {
		t.Fatalf("missed webhook, again: %+v", runs)
	}
	pe.fg.setPR(pe.f, "me/app", newer, sampleDiff)
	pe.send("pull_request", prPayload("synchronize", newer, nil))
	unseen := ForgePR{Number: 2, Title: "Unseen", Author: "sam", HeadRef: "u", HeadSHA: "2222222bbbb", State: "open"}
	pe.fg.setPR(pe.f, "me/app", unseen, sampleDiff)
	pe.send("pull_request", prPayload("opened", unseen, nil))
	runs := pe.drain()
	if len(runs) != 2 || runs[0].Reason != "pushed" || runs[1].Reason != "opened" {
		t.Errorf("runs after the webhooks: %+v", runs)
	}
}

func TestRefreshPullRequestsWithoutWebhooks(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	one := ForgePR{Number: 1, Title: "One", Author: "sam", HeadRef: "a", HeadSHA: "1111111aaaa", State: "open"}
	draft := ForgePR{Number: 2, Title: "Two", Author: "sam", HeadRef: "b", HeadSHA: "2222222bbbb", State: "draft"}
	pe.fg.setPR(pe.f, "me/app", one, sampleDiff)
	pe.fg.setPR(pe.f, "me/app", draft, sampleDiff)
	list := func(prs ...ForgePR) {
		pe.fg.listed = nil
		for i := range prs {
			pe.fg.listed = append(pe.fg.listed, &prs[i])
		}
	}
	list(one, draft)
	pe.s.RefreshPullRequests(pe.ctx)
	runs := pe.drain()
	if len(runs) != 1 || runs[0].Reason != "opened" || runs[0].PR == nil || runs[0].PR.Number != 1 {
		t.Fatalf("first refresh: %+v", runs)
	}
	if p, _ := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 1); p.HeadSHA != one.HeadSHA {
		t.Errorf("head not recorded: %+v", p)
	}
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 0 {
		t.Fatalf("repeat refresh: %+v", runs)
	}
	pushed := one
	pushed.HeadSHA = "9999999zzzz"
	pe.fg.setPR(pe.f, "me/app", pushed, sampleDiff)
	list(pushed, draft)
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 1 || runs[0].Reason != "pushed" {
		t.Fatalf("after a push: %+v", runs)
	}
	pe.send("pull_request", prPayload("synchronize", pushed, nil))
	if runs := pe.drain(); len(runs) != 0 {
		t.Fatalf("webhook for a head already seen: %+v", runs)
	}
	again := pushed
	again.HeadSHA = "8888888yyyy"
	list(again, draft)
	pe.s.RefreshPullRequests(pe.ctx)
	if runs := pe.drain(); len(runs) != 0 {
		t.Errorf("refresh with working webhooks: %+v", runs)
	}
}

func TestCodingAgent(t *testing.T) {
	cases := map[string]string{
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)":                          "anthropic/claude-code",
		"Generated with Claude Code\n\nCo-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>": "anthropic/claude-sonnet-4-5",
		"Codex task: https://chatgpt.com/codex/tasks/task_e_1":                                    "openai/codex",
		"Co-authored-by: Cursor Agent <cursoragent@cursor.com>":                                   "cursor/cursor-agent",
		"Just a fix.": "",
	}
	for body, want := range cases {
		if got := codingAgent(body, "sam"); got != want {
			t.Errorf("%q → %q, want %q", body, got, want)
		}
	}
	if codingAgent("", "Copilot") != "github/copilot" {
		t.Error("Copilot's pull requests")
	}
}

func TestReviewNote(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.policy(MergePolicyInput{AutoMerge: ptr(false)})
	n, err := pe.s.GetReviewNote(pe.ctx, pe.admin)
	if err != nil || n.Custom || n.Note != DefaultReviewNote {
		t.Fatalf("default: %+v %v", n, err)
	}
	if _, err := pe.s.SetReviewNote(pe.ctx, pe.user("sam", false), "x"); err == nil {
		t.Error("a non-admin worded the note")
	}
	if _, err := pe.s.SetReviewNote(pe.ctx, &Actor{User: pe.admin.User, Agent: "anthropic/claude-code"}, "x"); err == nil {
		t.Error("an agent worded the note")
	}
	if _, err := pe.s.SetReviewNote(pe.ctx, pe.admin, "hi <!-- waypoint-review sha=1 -->"); err == nil {
		t.Error("a marker in the note")
	}
	n, err = pe.s.SetReviewNote(pe.ctx, pe.admin, "Low-trust review by a free model\n({models}); check before acting.")
	if err != nil || !n.Custom || n.Note != "Low-trust review by a free model ({models}); check before acting." {
		t.Fatalf("set: %+v %v", n, err)
	}
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pr := ForgePR{Number: 4, Title: "Fix login", Author: "sam", HeadRef: "f", HeadSHA: "4444444dddd", State: "open"}
	pe.pullHook("opened", pr)
	pe.drain()
	r := pe.fg.reviewsOn(pe.f, "me/app", 4)
	if len(r) != 1 || !strings.HasPrefix(r[0], "Low-trust review by a free model (`opencode/muse-spark-1.3-contributor-free`); check before acting.\n<!-- waypoint-review") {
		t.Errorf("review = %q", r)
	}
	if lc := pe.fg.lines[0]; len(lc) != 1 || !strings.Contains(lc[0].Body, "<sub>Low-trust review by a free model (`opencode/muse-spark-1.3-contributor-free`); check before acting.</sub>") {
		t.Errorf("line comment = %+v", lc)
	}
	if n, err = pe.s.SetReviewNote(pe.ctx, pe.admin, ""); err != nil || n.Custom {
		t.Errorf("reset: %+v %v", n, err)
	}
	if a := pe.audits(); len(a) != 2 || !strings.HasPrefix(a[0], "settings.review_note by /") {
		t.Errorf("audit = %v", a)
	}
}
