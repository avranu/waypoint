package core

import (
	"strings"
	"testing"
	"time"
)

func hook(machine, cli, event string, mod func(*HookEvent)) HookEvent {
	ev := HookEvent{Event: event, SessionID: cli, Machine: machine, Cwd: "/work/app"}
	if mod != nil {
		mod(&ev)
	}
	return ev
}

func sessionsFor(t *testing.T, e *env, a *Actor, q AgentSessionQuery) []*AgentSession {
	t.Helper()
	page, err := e.s.ListAgentSessions(e.ctx, a, q)
	if err != nil {
		t.Fatal(err)
	}
	return page.Sessions
}

func TestSessionLifecycleAndStates(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "kanban")
	if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, 0, 'acme/app', 1)`, p.ID); err != nil {
		f, ferr := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr("https://git.example.com"), Token: ptr("t")})
		if ferr != nil {
			t.Fatal(ferr)
		}
		if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 1)`, p.ID, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	ag := agentActor(e.admin, "anthropic/claude-code")

	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", func(h *HookEvent) {
		h.Repo, h.Branch, h.Model, h.Source, h.Worktree = "acme/app", "feat/x", "claude-opus-5", "startup", true
	})})
	if err != nil {
		t.Fatal(err)
	}
	list := sessionsFor(t, e, e.admin, AgentSessionQuery{})
	if len(list) != 1 || list[0].ID != res.Session {
		t.Fatalf("sessions: %+v", list)
	}
	x := list[0]
	if x.State != SessionWorking || x.Project == nil || x.Project.Key != p.Key || x.Branch != "feat/x" || !x.Worktree || x.Agent != "anthropic/claude-code" || x.Model != "claude-opus-5" {
		t.Fatalf("session after start: %+v project=%+v", x, x.Project)
	}

	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) {
		h.Prompt, h.Title = strings.Repeat("p", 400), "Fix the thing"
	})}); err != nil {
		t.Fatal(err)
	}
	x = sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if len([]rune(x.LastPrompt)) != 300 || x.Title != "Fix the thing" || x.State != SessionWorking {
		t.Fatalf("after prompt: prompt %d title %q state %s", len([]rune(x.LastPrompt)), x.Title, x.State)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "Notification", func(h *HookEvent) { h.NotificationType = "permission_prompt" })})
	x = sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if x.State != SessionWaiting || x.WaitReason != "permission_prompt" {
		t.Fatalf("after notification: %s %s", x.State, x.WaitReason)
	}
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "Notification", func(h *HookEvent) { h.NotificationType = "auth_success" })})
	if sessionsFor(t, e, e.admin, AgentSessionQuery{})[0].State != SessionWaiting {
		t.Fatal("an auth notification shouldn't change the state")
	}
	e.setClock(e.clockNow().Add(time.Minute))
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "Stop", func(h *HookEvent) {
		h.LastMessage, h.InputTokens, h.OutputTokens = strings.Repeat("u", 2500), 1000, 200
	})})
	x = sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if x.State != SessionIdle || len([]rune(x.LastUpdate)) != 2000 || x.InputTokens != 1000 || x.OutputTokens != 200 {
		t.Fatalf("after stop: %s update %d tokens %d/%d", x.State, len([]rune(x.LastUpdate)), x.InputTokens, x.OutputTokens)
	}

	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go" })})
	e.setClock(e.clockNow().Add(29 * time.Minute))
	if got := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0].State; got != SessionWorking {
		t.Fatalf("after 29 minutes: %s", got)
	}
	e.setClock(e.clockNow().Add(2 * time.Minute))
	if got := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0].State; got != SessionIdle {
		t.Fatalf("after 31 minutes: %s", got)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{State: SessionIdle})); n != 1 {
		t.Fatalf("idle filter finds %d", n)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{State: "active"})); n != 1 {
		t.Fatalf("active filter finds %d", n)
	}
	e.setClock(e.clockNow().Add(24 * time.Hour))
	if got := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0].State; got != SessionEnded {
		t.Fatalf("after a day: %s", got)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{State: "active"})); n != 0 {
		t.Fatalf("active filter finds %d ended", n)
	}

	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", func(h *HookEvent) { h.Source = "resume" })})
	list = sessionsFor(t, e, e.admin, AgentSessionQuery{})
	if len(list) != 1 || list[0].State != SessionWorking || list[0].Repo != "acme/app" {
		t.Fatalf("resumed: %+v", list)
	}
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionEnd", func(h *HookEvent) { h.EndReason = "logout" })})
	x = sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if x.State != SessionEnded || x.EndedAt == nil {
		t.Fatalf("after end: %s", x.State)
	}

	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("laptop", "cli-1", "SessionStart", nil)})
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{})); n != 2 {
		t.Fatalf("two machines, %d sessions", n)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{Machine: "laptop"})); n != 1 {
		t.Fatalf("machine filter finds %d", n)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{Branch: "feat/x", Repo: "acme/app", Project: p.Key})); n != 1 {
		t.Fatalf("branch/repo/project filter finds %d", n)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{Agent: "claude"})); n != 2 {
		t.Fatalf("agent filter finds %d", n)
	}
	if n := len(sessionsFor(t, e, e.admin, AgentSessionQuery{Agent: "codex"})); n != 0 {
		t.Fatalf("codex filter finds %d", n)
	}
	page, _ := e.s.ListAgentSessions(e.ctx, e.admin, AgentSessionQuery{Limit: 1})
	if page.Total != 2 || len(page.Sessions) != 1 || page.Next != 1 {
		t.Fatalf("paging: total %d len %d next %d", page.Total, len(page.Sessions), page.Next)
	}

	sum, err := e.s.GetAgentSession(e.ctx, e.admin, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, te := range sum.Timeline {
		kinds = append(kinds, te.Kind)
	}
	for _, want := range []string{"started", "prompt", "waiting", "update", "ended"} {
		found := false
		for _, k := range kinds {
			found = found || k == want
		}
		if !found {
			t.Errorf("timeline %v lacks %q", kinds, want)
		}
	}
	if sum.Doing.Title != "Fix the thing" {
		t.Errorf("doing: %+v", sum.Doing)
	}
}

// A long prompt keeps its start on the timeline's line and the whole of it
// beside (WP-860); a short one has nothing beside.
func TestSessionPromptKeptWhole(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	long := "Look at this:\n\n" + strings.Repeat("word ", 200) + "\nand the end is here."
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = long })})
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "short one" })})
	list := sessionsFor(t, e, e.admin, AgentSessionQuery{})
	if len(list) != 1 {
		t.Fatalf("sessions: %+v", list)
	}
	sum, err := e.s.GetAgentSession(e.ctx, e.admin, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []TimelineEntry
	for _, te := range sum.Timeline {
		if te.Kind == "prompt" {
			prompts = append(prompts, te)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts: %+v", prompts)
	}
	short, whole := prompts[0], prompts[1]
	if short.Detail != "short one" || short.Text != "" {
		t.Errorf("short: %+v", short)
	}
	if whole.Text != strings.TrimSpace(long) || len([]rune(whole.Detail)) != sessionPromptLine || !strings.HasPrefix(whole.Detail, "Look at this: word") {
		t.Errorf("long: detail %q, text %q", whole.Detail, whole.Text)
	}
}

func TestSessionVisibility(t *testing.T) {
	e := newEnv(t)
	ana, bob := e.user("ana", false), e.user("bob", false)
	ag := agentActor(ana, "anthropic/claude-code")
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "secret plans" })})
	if got := sessionsFor(t, e, bob, AgentSessionQuery{}); len(got) != 0 {
		t.Fatalf("bob sees ana's session: %+v", got)
	}
	if got := sessionsFor(t, e, e.admin, AgentSessionQuery{}); len(got) != 1 || got[0].LastPrompt != "secret plans" {
		t.Fatalf("admin: %+v", got)
	}
	id := sessionsFor(t, e, ana, AgentSessionQuery{})[0].ID
	if _, err := e.s.GetAgentSession(e.ctx, bob, id); err == nil {
		t.Fatal("bob read ana's session")
	}
	p := e.project(e.admin, "Shared", "kanban")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	got := sessionsFor(t, e, bob, AgentSessionQuery{})
	if len(got) != 1 || got[0].LastPrompt != "" {
		t.Fatalf("bob in the project: %+v", got)
	}
	if got := sessionsFor(t, e, ana, AgentSessionQuery{}); got[0].LastPrompt != "secret plans" {
		t.Fatal("ana lost her own prompt")
	}
	long := "more secret plans " + strings.Repeat("x", 400)
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = long })})
	sum, err := e.s.GetAgentSession(e.ctx, bob, id)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Doing.LastPrompt != "" {
		t.Fatalf("bob sees the last prompt: %q", sum.Doing.LastPrompt)
	}
	for _, x := range sum.Timeline {
		if x.Kind == "prompt" && (x.Detail != "" || x.Text != "") {
			t.Fatalf("bob sees a prompt in the timeline: %+v", x)
		}
	}
	mine, err := e.s.GetAgentSession(e.ctx, ana, id)
	if err != nil {
		t.Fatal(err)
	}
	if mine.Timeline[0].Kind != "prompt" || mine.Timeline[0].Text != long {
		t.Fatalf("ana's own timeline: %+v", mine.Timeline[0])
	}
}

func TestSessionRecordsWhatTheActorDoes(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "kanban")
	ag := agentActor(e.admin, "anthropic/claude-code")
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.s.SessionForHeader(e.ctx, e.admin.id(), "c1", "box"); got != res.Session {
		t.Fatalf("header lookup %d, want %d", got, res.Session)
	}
	if got := e.s.SessionForHeader(e.ctx, e.admin.id(), "nope", ""); got != 0 {
		t.Fatalf("unknown session resolved to %d", got)
	}
	in := *ag
	in.Session = res.Session
	one := e.issue(&in, IssueInput{Project: p.Key, Title: "Later", Status: "Backlog"})
	two := e.issue(&in, IssueInput{Project: p.Key, Title: "Now"})
	if _, err := e.s.AddComment(e.ctx, &in, two.Key, "looking at it"); err != nil {
		t.Fatal(err)
	}
	done := "Done"
	if _, err := e.s.UpdateIssue(e.ctx, &in, two.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatalf("close: %v", err)
	}
	pg, err := e.s.CreatePage(e.ctx, &in, PageInput{Title: ptr("Notes"), Body: ptr("x"), Project: Some(p.Key)})
	if err != nil {
		t.Fatal(err)
	}

	sum, err := e.s.GetAgentSession(e.ctx, e.admin, res.Session)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Done.IssuesCreated) != 2 || len(sum.Done.IssuesClosed) != 1 || sum.Done.IssuesClosed[0].Label != two.Key || sum.Done.Comments != 1 {
		t.Fatalf("done: created %d closed %d comments %d", len(sum.Done.IssuesCreated), len(sum.Done.IssuesClosed), sum.Done.Comments)
	}
	if len(sum.Done.Pages) != 1 || sum.Done.Pages[0].Label != pg.Title {
		t.Fatalf("pages: %+v", sum.Done.Pages)
	}
	if len(sum.Deferred.Issues) == 0 {
		t.Fatalf("nothing deferred though %s was filed: %+v", one.Key, sum.Done.IssuesCreated)
	}
}

func TestSessionHookRefusals(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.SessionHook(e.ctx, nil, []HookEvent{{SessionID: "x"}}); err == nil {
		t.Fatal("no actor")
	}
	if _, err := e.s.SessionHook(e.ctx, e.admin, nil); !IsInvalid(err) {
		t.Fatalf("empty batch: %v", err)
	}
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{{Event: "Stop"}}); !IsInvalid(err) {
		t.Fatalf("no session id: %v", err)
	}
	ro := *e.admin
	ro.ReadOnly = true
	if _, err := e.s.SessionHook(e.ctx, &ro, []HookEvent{hook("m", "c", "Stop", nil)}); err == nil {
		t.Fatal("a read-only token reported a session")
	}
}

func TestQueuedHookEventsKeepTheirTimes(t *testing.T) {
	e := newEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	t0 := e.clockNow().Add(-10 * time.Minute).UnixMilli()
	_, err := e.s.SessionHook(e.ctx, ag, []HookEvent{
		hook("box", "c1", "SessionStart", func(h *HookEvent) { h.At = t0 }),
		hook("box", "c1", "UserPromptSubmit", func(h *HookEvent) { h.At = t0 + 1000; h.Prompt = "first" }),
		hook("box", "c1", "Stop", func(h *HookEvent) { h.At = t0 + 2000; h.LastMessage = "done" }),
	})
	if err != nil {
		t.Fatal(err)
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if x.State != SessionIdle || x.LastPrompt != "first" || x.LastUpdate != "done" {
		t.Fatalf("%+v", x)
	}
	if x.StartedAt.UnixMilli() != t0 || x.LastSeenAt.UnixMilli() != t0+2000 {
		t.Fatalf("times: started %v seen %v", x.StartedAt, x.LastSeenAt)
	}
	e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "Notification", func(h *HookEvent) { h.At = t0 + 500; h.NotificationType = "idle_prompt" })})
	if got := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0].stored; got != SessionIdle {
		t.Fatalf("a stale notification set the state to %s", got)
	}
}

// WP-242: an issue Claude Code writes names the model its session runs, so
// people read "by opus 5.5"; the claim identity stays the tool's.
func TestIssueAuthorIsTheSessionModel(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "kanban")
	ag := agentActor(e.admin, "anthropic/claude-code")
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "c1", "SessionStart", func(ev *HookEvent) { ev.Model = "claude-opus-5-5[1m]" })})
	if err != nil {
		t.Fatal(err)
	}
	in := *ag
	in.Session = res.Session
	i := e.issue(&in, IssueInput{Project: p.Key, Title: "Fix it"})
	if i.ReporterAgent != "anthropic/claude-opus-5-5" || i.TextAgent != "anthropic/claude-opus-5-5" {
		t.Fatalf("author %q, text %q", i.ReporterAgent, i.TextAgent)
	}
	if j := e.issue(ag, IssueInput{Project: p.Key, Title: "Other"}); j.ReporterAgent != "anthropic/claude-code" {
		t.Fatalf("no session: %q", j.ReporterAgent)
	}
	sonnet := agentActor(e.admin, "anthropic/claude-sonnet-5-5")
	sonnet.Session = res.Session
	if j := e.issue(sonnet, IssueInput{Project: p.Key, Title: "Third"}); j.ReporterAgent != "anthropic/claude-sonnet-5-5" {
		t.Fatalf("own model: %q", j.ReporterAgent)
	}
}
