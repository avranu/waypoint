package core

import (
	"strings"
	"testing"
)

// WP-275: text an agent writes while working on a task records that task,
// its depth follows the chain of agent-written tasks, and from depth 3
// (lineageHold) the safety screen holds it for a person whatever a model says.
func TestLineageChainsAndHolds(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Line", "")
	n := 0
	// in is an agent working in a session of its own, holding task (if any)
	// or bound to it by WP_TASK (bind).
	in := func(agent, task string, bind bool) *Actor {
		t.Helper()
		n++
		a := agentActor(e.admin, agent)
		sess, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", "lineage-"+string(rune('a'+n)), "SessionStart", func(ev *HookEvent) {
			if bind {
				ev.Task = task
			}
		})})
		if err != nil {
			t.Fatal(err)
		}
		a.Session = sess.Session
		if task != "" && !bind {
			if _, err := e.s.AgentTake(e.ctx, a, task, ""); err != nil {
				t.Fatalf("%s takes %s: %v", agent, task, err)
			}
		}
		return a
	}
	release := func(a *Actor, key string) {
		t.Helper()
		if _, err := e.s.AgentRelease(e.ctx, a, key, ReleaseInput{}); err != nil {
			t.Fatal(err)
		}
	}
	depth := func(key string) int {
		t.Helper()
		d := e.get(e.admin, key)
		if d.Lineage == nil {
			return 0
		}
		return d.Lineage.Depth
	}

	person := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Speed up the board", Agent: "claude"})
	if l := e.get(e.admin, person.Key).Lineage; l != nil {
		t.Errorf("a person's task has a lineage: %+v", l)
	}

	// Agent A, working on the person's task, files one: depth 1, cleared.
	a := in("anthropic/claude-opus-5-5", person.Key, false)
	t1 := e.issue(a, IssueInput{Project: p.Key, Title: "Cache the board query", Agent: "claude"})
	release(a, person.Key)
	e.s.bg.Wait()
	d1 := e.get(e.admin, t1.Key)
	if d1.Lineage == nil || d1.Lineage.Depth != 1 || d1.Lineage.Held || len(d1.Lineage.Chain) != 1 || d1.Lineage.Chain[0].Key != person.Key || d1.Lineage.Chain[0].Agent != "" {
		t.Fatalf("t1 lineage = %+v", d1.Lineage)
	}
	if d1.Safety == nil || d1.Safety.Verdict != SafetyClear {
		t.Fatalf("t1 safety = %+v", d1.Safety)
	}

	// Agent B, working on t1 (an agent's, unreviewed), files one: depth 2,
	// which a model screens (WP-837: holding from depth 2 held too much).
	llm := &fakeLLM{answer: `{"flags": [], "reason": ""}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	e.s.bg.Wait()
	// B also takes the person's task: the deepest task its session holds
	// counts, not the latest taken.
	b := in("anthropic/claude-sonnet-5-5", t1.Key, false)
	if _, err := e.s.AgentTake(e.ctx, b, person.Key, ""); err != nil {
		t.Fatal(err)
	}
	t2 := e.issue(b, IssueInput{Project: p.Key, Title: "Also add an index", Agent: "claude"})
	release(b, t1.Key)
	release(b, person.Key)
	e.s.bg.Wait()
	d2 := e.get(e.admin, t2.Key)
	if d2.Lineage == nil || d2.Lineage.Depth != 2 || d2.Lineage.Held || len(d2.Lineage.Chain) != 2 ||
		d2.Lineage.Chain[0].Key != t1.Key || d2.Lineage.Chain[0].Agent == "" || d2.Lineage.Chain[1].Key != person.Key {
		t.Fatalf("t2 lineage = %+v", d2.Lineage)
	}
	if d2.Safety == nil || d2.Safety.Verdict != SafetyClear {
		t.Fatalf("t2 safety = %+v", d2.Safety)
	}
	if !promptsMention(llm.prompts, "Also add an index") {
		t.Errorf("no model was asked about a depth-2 task")
	}
	if got := e.search(e.admin, "is:held"); len(got) != 0 {
		t.Errorf("is:held = %v", got)
	}

	// A session bound to t2 by WP_TASK, without taking it, writes at depth 3:
	// held by the rule, with no model asked.
	c := in("anthropic/claude-haiku-5-5", t2.Key, true)
	asked := len(llm.prompts)
	t3 := e.issue(c, IssueInput{Project: p.Key, Title: "Drop the old index"})
	e.s.bg.Wait()
	if got := depth(t3.Key); got != 3 {
		t.Fatalf("t3 depth = %d", got)
	}
	// The stored reason names no task: whoever can open t3 may not see t2.
	if d := e.get(e.admin, t3.Key); d.Lineage == nil || !d.Lineage.Held || d.Safety == nil || d.Safety.Verdict != SafetyHold ||
		strings.Contains(d.Safety.Reason, t2.Key) || !strings.Contains(d.Safety.Reason, "depth 3") {
		t.Fatalf("t3 = %+v / %+v", d.Lineage, d.Safety)
	}
	if promptsMention(llm.prompts[asked:], "Drop the old index") {
		t.Errorf("a model was asked about a depth-3 task")
	}
	if got := e.search(e.admin, "is:held"); len(got) != 1 || got[0] != t3.Key {
		t.Errorf("is:held = %v", got)
	}

	// A person's review up the chain shortens every chain below it.
	if _, err := e.s.Review(e.ctx, e.admin, ReviewIssue, t1.Key, ""); err != nil {
		t.Fatal(err)
	}
	if got := depth(t2.Key); got != 1 {
		t.Errorf("t2 depth after t1's review = %d", got)
	}
	if got := depth(t3.Key); got != 2 {
		t.Errorf("t3 depth after t1's review = %d", got)
	}
	// The review screens the tasks below it again: t3, at depth 2 now, is
	// cleared by the model and no longer listed as held.
	e.s.bg.Wait()
	if d := e.get(e.admin, t3.Key); d.Safety == nil || d.Safety.Verdict != SafetyClear {
		t.Errorf("t3 after the chain was reviewed: %+v", d.Safety)
	}
	if got := e.search(e.admin, "is:held"); len(got) != 0 {
		t.Errorf("is:held after the review = %v", got)
	}

	// An agent editing t3 from a shallower task doesn't shorten its chain;
	// one editing its own task keeps it.
	a2 := in("anthropic/claude-opus-5-5", person.Key, false)
	desc := "Drop it after the new one is live."
	if _, err := e.s.UpdateIssue(e.ctx, a2, t3.Key, IssuePatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if got := depth(t3.Key); got != 2 {
		t.Errorf("t3 depth after a shallower edit = %d", got)
	}
	// Nor does one at the same depth redirect it: t2 (depth 1 now) keeps t1.
	desc2 := "An index on (project, status)."
	if _, err := e.s.UpdateIssue(e.ctx, a2, t2.Key, IssuePatch{Description: &desc2}); err != nil {
		t.Fatal(err)
	}
	if l := e.get(e.admin, t2.Key).Lineage; l == nil || len(l.Chain) == 0 || l.Chain[0].Key != t1.Key {
		t.Errorf("t2's chain after an edit at the same depth = %+v", l)
	}
	release(a2, person.Key)

	// Someone t3 alone is shared with sees the depth of the tasks up its
	// chain, not which they are or who wrote them; its reason names none.
	sam := e.user("sam", false)
	if err := e.s.ShareIssue(e.ctx, e.admin, t3.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	if d := e.get(sam, t3.Key); d.Lineage == nil || len(d.Lineage.Chain) != 2 || !d.Lineage.Chain[0].Hidden ||
		d.Lineage.Chain[0].Key != "" || d.Lineage.Chain[0].Agent != "" || d.Lineage.Chain[0].Depth != 1 {
		t.Errorf("t3's chain to someone who can't open t2 = %+v", d.Lineage)
	} else if d.Safety != nil && strings.Contains(d.Safety.Reason, t2.Key) {
		t.Errorf("t3's reason names t2: %s", d.Safety.Reason)
	}

	// Deleting a task up the chain keeps the depth it had then.
	if err := e.s.DeleteIssue(e.ctx, e.admin, t2.Key, false); err != nil {
		t.Fatal(err)
	}
	// t2 was at depth 2 when t3 was written: that stands in for the chain,
	// which can't be read again (t1's later review doesn't shorten it).
	d3 := e.get(e.admin, t3.Key)
	if d3.Lineage == nil || d3.Lineage.Depth != 3 || len(d3.Lineage.Chain) != 1 || !d3.Lineage.Chain[0].Gone || d3.Lineage.Chain[0].Depth != 2 {
		t.Errorf("t3 lineage after t2's deletion = %+v", d3.Lineage)
	}

	// A person rewriting it makes it theirs: no chain.
	title := "Drop the old board index"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, t3.Key, IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if l := e.get(e.admin, t3.Key).Lineage; l != nil {
		t.Errorf("a person's rewrite kept a lineage: %+v", l)
	}
}

func promptsMention(prompts []string, s string) bool {
	for _, p := range prompts {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}
