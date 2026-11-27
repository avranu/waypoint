package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// storyLLM answers story requests with a fixed story and anything else
// (estimates, titles) with a small estimate.
type storyLLM struct {
	mu      sync.Mutex
	answer  storyAnswer
	prompts []string
}

func (f *storyLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (f *storyLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(prompt, "Plan title:") {
		return json.RawMessage(`{"points": 3, "confidence": "high", "reason": "Small."}`), "", nil
	}
	f.prompts = append(f.prompts, prompt)
	b, _ := json.Marshal(f.answer)
	return b, "", nil
}

func (f *storyLLM) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

func storyEnv(t *testing.T) (*env, *Project, *storyLLM) {
	t.Helper()
	e := newEnv(t)
	llm := &storyLLM{answer: storyAnswer{
		AsA: "As the project owner", IWant: "to see `slow` boards load fast.", SoThat: "so that I can plan without waiting",
		Points: []string{"Boards with many cards scroll smoothly", "- Dragging a card keeps up with the pointer", ""},
	}}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	return e, e.project(e.admin, "Boards", "kanban"), llm
}

func longPlan(word string) string {
	return strings.Repeat("Step "+word+": render only the visible cards and recycle the rest.\n", 40)
}

func (e *env) plan(a *Actor, p *Project, title, body string) *Page {
	e.t.Helper()
	cat, key := "plan", p.Key
	pg, err := e.s.CreatePage(e.ctx, a, PageInput{Title: &title, Body: &body, Category: &cat, Project: Some(key)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.s.Wait()
	return e.page(a, pg.ID)
}

func (e *env) page(a *Actor, id int64) *Page {
	e.t.Helper()
	pg, err := e.s.GetPage(e.ctx, a, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return pg
}

const wantStory = "As the project owner, I want to see slow boards load fast, so that I can plan without waiting.\n\n- Boards with many cards scroll smoothly\n- Dragging a card keeps up with the pointer"

// An agent's long plan gets a story from the story model; approving the
// plan keeps the story as the person's, and only the story (WP-190).
func TestPlanStoryWrittenAndKept(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")

	pg := e.plan(ag, p, "Fast boards", longPlan("one"))
	if pg.Story != wantStory || pg.StoryAgent != storyAgent || pg.StoryModel == "" || pg.StoryIsPersons() || pg.StoryStale || pg.StoryBy != nil {
		t.Fatalf("suggested story: %q agent %q model %q stale %v by %+v", pg.Story, pg.StoryAgent, pg.StoryModel, pg.StoryStale, pg.StoryBy)
	}
	if got := llm.asked(); len(got) != 1 || !strings.Contains(got[0], "The plan (written by anthropic/claude-code)") {
		t.Fatalf("story prompts: %q", got)
	}

	mine, title := "As an agent, I want my plan approved.", "Faster boards"
	got, err := e.s.UpdatePage(e.ctx, ag, pg.ID, PageInput{Story: &mine, Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if got.Notice != StoryAgentNotice || got.Story != wantStory || got.Title != title {
		t.Fatalf("agent's story edit: notice %q story %q title %q", got.Notice, got.Story, got.Title)
	}

	if n, err := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); err != nil || n != 1 {
		t.Fatalf("review: %d %v", n, err)
	}
	pg = e.page(e.admin, pg.ID)
	if !pg.StoryIsPersons() || pg.StoryBy == nil || pg.StoryBy.ID != e.admin.id() || pg.StoryModel == "" || pg.Unreviewed {
		t.Fatalf("after review: agent %q by %+v model %q unreviewed %v", pg.StoryAgent, pg.StoryBy, pg.StoryModel, pg.Unreviewed)
	}

	body := longPlan("two")
	if _, err := e.s.UpdatePage(e.ctx, ag, pg.ID, PageInput{Body: &body}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	pg = e.page(e.admin, pg.ID)
	if !pg.StoryIsPersons() || pg.Story != wantStory || !pg.StoryStale {
		t.Fatalf("after the plan changed: agent %q stale %v story %q", pg.StoryAgent, pg.StoryStale, pg.Story)
	}
	if len(llm.asked()) != 1 {
		t.Fatalf("a person's story was rewritten: %d prompts", len(llm.asked()))
	}
	if n, err := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); err != nil || n != 1 {
		t.Fatalf("second review: %d %v", n, err)
	}
	if pg = e.page(e.admin, pg.ID); pg.StoryStale {
		t.Fatal("approving the changed plan should keep its story against it")
	}
	if n, _ := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); n != 0 {
		t.Fatalf("a review with nothing new counted %d", n)
	}

	own := "  As the owner, I want boards to keep up.  "
	pg, err = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Story: &own})
	if err != nil {
		t.Fatal(err)
	}
	if pg.Story != strings.TrimSpace(own) || !pg.StoryIsPersons() || pg.StoryModel != "" {
		t.Fatalf("person's story: %q agent %q model %q", pg.Story, pg.StoryAgent, pg.StoryModel)
	}
	if _, err := e.s.SuggestStory(e.ctx, e.admin, pg.ID); err == nil {
		t.Fatal("suggesting over a person's story should be refused")
	}

	empty := ""
	if _, err = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Story: &empty}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if pg = e.page(e.admin, pg.ID); pg.Story != wantStory || pg.StoryAgent != storyAgent || pg.StoryBy != nil {
		t.Fatalf("after clearing: %q agent %q by %+v", pg.Story, pg.StoryAgent, pg.StoryBy)
	}
	if pg, err = e.s.SuggestStory(e.ctx, e.admin, pg.ID); err != nil || pg.Story != wantStory || pg.StoryAgent != storyAgent {
		t.Fatalf("suggested again: %v %+v", err, pg)
	}
	keep := pg.Story
	if pg, err = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Story: &keep}); err != nil || !pg.StoryIsPersons() || pg.StoryModel == "" {
		t.Fatalf("kept: %v agent %q model %q", err, pg.StoryAgent, pg.StoryModel)
	}
}

// The story model's own story follows the plan: a new body gets a new one.
func TestPlanStoryFollowsPlan(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	pg := e.plan(ag, p, "Fast boards", longPlan("one"))
	llm.mu.Lock()
	llm.answer.IWant = "boards that never stutter"
	llm.mu.Unlock()
	body := longPlan("two")
	if _, err := e.s.UpdatePage(e.ctx, ag, pg.ID, PageInput{Body: &body}); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	pg = e.page(e.admin, pg.ID)
	if !strings.Contains(pg.Story, "I want boards that never stutter") || pg.StoryStale || pg.StoryAgent != storyAgent {
		t.Fatalf("after the plan changed: %q stale %v", pg.Story, pg.StoryStale)
	}
}

// Short plans, people's plans, other pages and drafts get no story.
func TestPlanStoryOnlyForLongAgentPlans(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	if pg := e.plan(ag, p, "Short", "Render fewer cards."); pg.Story != "" {
		t.Fatalf("short plan got a story: %q", pg.Story)
	}
	if pg := e.plan(e.admin, p, "Mine", longPlan("mine")); pg.Story != "" {
		t.Fatalf("a person's plan got a story: %q", pg.Story)
	}
	title, body, key := "Notes", longPlan("notes"), p.Key
	note, err := e.s.CreatePage(e.ctx, ag, PageInput{Title: &title, Body: &body, Project: Some(key)})
	if err != nil {
		t.Fatal(err)
	}
	cat := "plan"
	draft, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: &title, Body: &body, Category: &cat, Project: Some(key), Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if e.page(e.admin, note.ID).Story != "" || e.page(e.admin, draft.ID).Story != "" {
		t.Fatal("a non-plan page or a draft got a story")
	}
	if n := len(llm.asked()); n != 0 {
		t.Fatalf("the story model was asked %d times", n)
	}
	e.s.db.W.Exec(`UPDATE pages SET text_agent = 'anthropic/claude-code' WHERE id = ?`, draft.ID)
	if _, err := e.s.PublishPage(e.ctx, e.admin, draft.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	e.s.db.W.Exec(`UPDATE pages SET story = '', story_tried = 0 WHERE id = ?`, draft.ID)
	e.s.sweepStories(e.ctx)
	e.s.Wait()
	if e.page(e.admin, draft.ID).Story != wantStory {
		t.Fatal("the sweep didn't write the missing story")
	}
}

// The prompt a person typed in the session that wrote the plan is kept with
// it, read by the story model, and shown only to them and admins.
func TestPlanStoryPrompt(t *testing.T) {
	e, p, llm := storyEnv(t)
	sam := e.user("sam", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	ag := agentActor(sam, "anthropic/claude-code")
	res, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	asked := "Boards with hundreds of cards are unusable. " + strings.Repeat("Please make them fast. ", 30)
	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.s.SessionHook(e.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = asked })}); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	in := *ag
	in.Session = res.Session
	pg := e.plan(&in, p, "Fast boards", longPlan("one"))

	if pg.SessionID == nil || *pg.SessionID != res.Session || pg.Prompt == nil || pg.Prompt.Text != strings.TrimSpace(asked) || pg.Prompt.SessionID != res.Session {
		t.Fatalf("prompt for its person: session %v prompt %+v", pg.SessionID, pg.Prompt)
	}
	if got := llm.asked(); len(got) != 1 || !strings.Contains(got[0], "What the person typed to the agent before it wrote the plan:\n<<<\n"+strings.TrimSpace(asked)) {
		t.Fatalf("story prompt: %q", got)
	}
	if e.page(e.admin, pg.ID).Prompt == nil {
		t.Fatal("an admin should see the prompt")
	}
	vic := e.user("vic", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "vic", "editor")
	if got := e.page(vic, pg.ID); got.Prompt != nil || got.SessionID == nil {
		t.Fatalf("someone else saw the prompt: %+v", got.Prompt)
	}
}

func TestStoryMarkdown(t *testing.T) {
	for _, c := range []struct {
		in   storyAnswer
		want string
	}{
		{storyAnswer{IWant: ""}, ""},
		{storyAnswer{IWant: "I want to ship.", Points: []string{"* one.", "two", "three", "four", "five", "six"}},
			"As the person who asked, I want to ship.\n\n- one\n- two\n- three\n- four\n- five"},
		{storyAnswer{AsA: "a  maintainer\n", IWant: "fewer pings", SoThat: "So that I focus;"},
			"As a maintainer, I want fewer pings, so that I focus."},
		{storyAnswer{AsA: "As a reviewer", IWant: "I want to read less"}, "As a reviewer, I want to read less."},
		{storyAnswer{AsA: "an editor", IWant: strings.Repeat("x", 300)},
			"As an editor, I want " + clipRunes(strings.Repeat("x", 300), 200) + "."},
	} {
		if got := c.in.markdown(); got != c.want {
			t.Errorf("markdown(%+v) =\n%q\nwant\n%q", c.in, got, c.want)
		}
	}
}

// Audit #973.1: asking for a story is a person's, one at a time per page.
func TestPlanStoryAsking(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	pg := e.plan(ag, p, "Fast boards", longPlan("one"))
	before := len(llm.asked())
	if _, err := e.s.SuggestStory(e.ctx, ag, pg.ID); err == nil {
		t.Fatal("an agent asked for a story")
	}
	e.s.aiMu.Lock()
	e.s.storying[pg.ID] = true
	e.s.aiMu.Unlock()
	if _, err := e.s.SuggestStory(e.ctx, e.admin, pg.ID); err == nil {
		t.Fatal("a second story was asked for while one was being written")
	}
	e.s.aiMu.Lock()
	delete(e.s.storying, pg.ID)
	e.s.aiMu.Unlock()
	if n := len(llm.asked()); n != before {
		t.Fatalf("the story model was asked %d more times", n-before)
	}
}

// Audit #973.3: the sweep moves past plans whose story keeps failing.
func TestPlanStorySweepMovesOn(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	llm.mu.Lock()
	llm.answer.IWant = ""
	llm.mu.Unlock()
	for i := range storySweepMax + 2 {
		e.plan(ag, p, "Plan "+itoa64(int64(i)), longPlan(itoa64(int64(i))))
	}
	asked := len(llm.asked())
	e.s.sweepStories(e.ctx)
	e.s.Wait()
	if n := len(llm.asked()) - asked; n != 0 {
		t.Fatalf("the sweep retried %d plans tried a moment ago", n)
	}
	e.setClock(e.clockNow().Add(25 * time.Hour))
	seen := map[string]int{}
	for range 3 {
		asked = len(llm.asked())
		e.s.sweepStories(e.ctx)
		e.s.Wait()
		for _, q := range llm.asked()[asked:] {
			seen[strings.SplitN(q, "\n", 2)[0]]++
		}
	}
	if len(seen) != storySweepMax+2 {
		t.Fatalf("the sweep reached %d of %d failing plans: %v", len(seen), storySweepMax+2, seen)
	}
	for title, n := range seen {
		if n != 1 {
			t.Fatalf("%s was retried %d times in a day", title, n)
		}
	}
}

// Audit #973.4-6: a review records what was approved, and lists show a
// stale story as the page does.
func TestPlanStoryReviewRecord(t *testing.T) {
	e, p, llm := storyEnv(t)
	ag := agentActor(e.admin, "anthropic/claude-code")
	lastReview := func() string {
		for _, r := range e.audit(e.admin, AuditQuery{Limit: 20}).Entries {
			if r.Action == "review" {
				return r.Detail
			}
		}
		return ""
	}

	pg := e.plan(ag, p, "Fast boards", longPlan("one"))
	if _, err := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); err != nil {
		t.Fatal(err)
	}
	if d := lastReview(); d != "approved a plan by anthropic/claude-code on its story, not its text" {
		t.Fatalf("audit: %q", d)
	}
	if pg = e.page(e.admin, pg.ID); !pg.ReviewedOnStory {
		t.Fatal("approved on its story, but not recorded so")
	}
	own := "As the owner, I want boards to keep up."
	if pg, _ = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Story: &own}); !pg.ReviewedOnStory {
		t.Fatal("rewriting the story later lost that the plan was approved on it")
	}

	llm.mu.Lock()
	llm.answer.IWant = ""
	llm.mu.Unlock()
	pg = e.plan(ag, p, "Slow boards", longPlan("two"))
	if _, err := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); err != nil {
		t.Fatal(err)
	}
	if d := lastReview(); d != "reviewed text by anthropic/claude-code" {
		t.Fatalf("audit: %q", d)
	}
	if pg, _ = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Story: &own}); pg.ReviewedOnStory || !pg.StoryIsPersons() {
		t.Fatalf("a story added after a line-by-line review: on story %v", pg.ReviewedOnStory)
	}

	e.s.db.W.Exec(`UPDATE pages SET body = ? WHERE id = ?`, longPlan("three"), pg.ID)
	if n, err := e.s.Review(e.ctx, e.admin, ReviewPage, itoa64(pg.ID), p.Key); err != nil || n != 1 {
		t.Fatalf("keeping a stale story: %d %v", n, err)
	}
	if d := lastReview(); d != "kept the story of a plan by anthropic/claude-code as their intent" {
		t.Fatalf("audit: %q", d)
	}

	e.s.db.W.Exec(`UPDATE pages SET body = ? WHERE id = ?`, longPlan("four"), pg.ID)
	list, err := e.s.ListPages(e.ctx, e.admin, PageQuery{Project: p.Key})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range list {
		if l.ID == pg.ID {
			found = true
			if !l.StoryStale {
				t.Fatal("the list shows a stale story as current")
			}
		}
	}
	if !found {
		t.Fatal("the plan isn't listed")
	}
}
