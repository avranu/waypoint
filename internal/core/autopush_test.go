package core

import (
	"errors"
	"strings"
	"testing"
)

// A session cleared with commits no remote had: its machine pushes them and
// says so. The task gets one line naming the branch, once per commit; wp
// next sends whoever takes it there; a later failed push keeps the branch
// that has them (WP-843).
func TestAutoPushRecordedOnStrandedWork(t *testing.T) {
	se := newStrandEnv(t)
	branch := strings.ToLower(se.key) + "-triage"
	co := HookCheckout{Top: "/work/app", Branch: branch, Unpushed: 2}
	if _, err := se.s.SessionHook(se.ctx, se.ag, []HookEvent{hook("m1", "cli-old", "Stop", func(h *HookEvent) {
		h.Checkouts = []HookCheckout{co}
	})}); err != nil {
		t.Fatal(err)
	}
	endSession(t, se.env, se.ag, "cli-old", "/work/app", "clear")

	in := AutoPushInput{Keys: []string{se.key}, Machine: "m1", Top: "/work/app", Branch: branch, Head: "abc123", Commits: 2, Why: "clear"}
	res, err := se.s.RecordAutoPush(se.ctx, se.ag, in)
	if err != nil || len(res.Told) != 1 || res.Told[0] != se.key {
		t.Fatalf("record = %+v, %v", res, err)
	}
	bodies := se.commentBodies(se.key)
	c := bodies[len(bodies)-1]
	for _, want := range []string{"Pushed when its session was cleared (/clear)", "the 2 commits left in `/work/app` on m1 are on the forge as branch `" + branch + "`", "`wp next " + se.key + "`"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment lacks %q: %s", want, c)
		}
	}
	// The same commit again: nothing new to say.
	if res, err := se.s.RecordAutoPush(se.ctx, se.ag, in); err != nil || len(res.Told) != 0 {
		t.Errorf("repeat = %+v, %v", res, err)
	}
	if n := len(se.commentBodies(se.key)); n != len(bodies) {
		t.Errorf("repeat commented: %d comments, want %d", n, len(bodies))
	}

	fresh := sessionAgent(t, se.env, se.ag, "cli-new", "/work/other", nil)
	task, err := se.s.AgentNext(se.ctx, fresh, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != se.key || task.Resume == nil {
		t.Fatalf("next = %+v, %v", task, err)
	}
	r := task.Resume.ResumeText(se.key)
	for _, want := range []string{"2 commits pushed to branch `" + branch + "`", "`git fetch origin " + branch + "`"} {
		if !strings.Contains(r, want) {
			t.Errorf("resume lacks %q: %s", want, r)
		}
	}

	// A newer commit that couldn't go: said, and the branch that has the
	// earlier ones stays named.
	in.Head, in.Error = "def456", "! [rejected] app-1-triage -> app-1-triage (fetch first)"
	if res, err := se.s.RecordAutoPush(se.ctx, se.ag, in); err != nil || len(res.Told) != 1 {
		t.Fatalf("failed push = %+v, %v", res, err)
	}
	bodies = se.commentBodies(se.key)
	if c := bodies[len(bodies)-1]; !strings.Contains(c, "Couldn't push the 2 commits left in `/work/app` on m1 to branch `"+branch+"`") || !strings.Contains(c, "(fetch first)") {
		t.Errorf("failure comment = %s", c)
	}
	w := se.s.strandedFor(se.ctx, se.s.db.R, task.Issue.ID)
	if w == nil || w.Pushed != branch || !strings.Contains(w.PushError, "rejected") {
		t.Errorf("stranded = %+v", w)
	}
	// wp next mustn't send the next agent to the forge for commits that
	// aren't there.
	r = w.ResumeText(se.key)
	if !strings.Contains(r, "Pushing its latest commits failed") || !strings.Contains(r, "earlier ones are on branch `"+branch+"`") ||
		strings.Contains(r, "git fetch") || strings.Contains(r, "pushed to branch") {
		t.Errorf("resume after a failed push = %s", r)
	}
}

// wp release pushes where no session ended: the row it adds names no
// session, so the terminal that gave the task back isn't handed it again;
// the task still says where the commits are. A detached head's commits go
// to a branch named for the task.
func TestAutoPushOnRelease(t *testing.T) {
	se := newStrandEnv(t)
	if _, err := se.s.AgentRelease(se.ctx, se.sess, se.key, ReleaseInput{Comment: "blocked"}); err != nil {
		t.Fatal(err)
	}
	in := AutoPushInput{Keys: []string{se.key}, Machine: "m1", Top: "/work/app", Branch: "wp-rescue/" + se.key, Head: "abc", Commits: 1, Why: "release", Rescue: true}
	if res, err := se.s.RecordAutoPush(se.ctx, se.ag, in); err != nil || len(res.Told) != 1 {
		t.Fatalf("record = %+v, %v", res, err)
	}
	bodies := se.commentBodies(se.key)
	if c := bodies[len(bodies)-1]; !strings.Contains(c, "Pushed when its agent gave the task back: the 1 commit left in `/work/app` on m1 is on the forge") ||
		!strings.Contains(c, "detached head") {
		t.Errorf("comment = %s", c)
	}
	if here := se.s.strandedHere(se.ctx, se.sess, se.ag.Agent); len(here) != 0 {
		t.Errorf("released task handed back to its terminal: %v", here)
	}
	i, _ := se.s.GetIssue(se.ctx, se.admin, se.key)
	w := se.s.strandedFor(se.ctx, se.s.db.R, i.ID)
	if w == nil || w.Pushed != "wp-rescue/"+se.key || w.Why != "release" || !strings.Contains(w.ResumeText(se.key), "gave it back") {
		t.Errorf("stranded = %+v", w)
	}
}

// Keys come from the branch when none are sent; a branch is required, and
// a person's session-less token can't record for tasks it can't edit.
func TestAutoPushInputs(t *testing.T) {
	se := newStrandEnv(t)
	if _, err := se.s.RecordAutoPush(se.ctx, se.ag, AutoPushInput{Top: "/x", Commits: 1}); err == nil {
		t.Error("no branch accepted")
	}
	if _, err := se.s.RecordAutoPush(se.ctx, nil, AutoPushInput{Branch: "b"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("nil actor: %v", err)
	}
	res, err := se.s.RecordAutoPush(se.ctx, se.ag, AutoPushInput{Machine: "m1", Top: "/work/app", Branch: "worktree-" + strings.ToLower(se.key), Head: "h", Commits: 3, Why: "idle"})
	if err != nil || len(res.Told) != 1 || res.Told[0] != se.key {
		t.Fatalf("by branch = %+v, %v", res, err)
	}
	bodies := se.commentBodies(se.key)
	if c := bodies[len(bodies)-1]; !strings.Contains(c, "Pushed after its checkout sat unused for 4 hours") {
		t.Errorf("comment = %s", c)
	}
	if res, err := se.s.RecordAutoPush(se.ctx, se.ag, AutoPushInput{Branch: "no-task-here", Commits: 1}); err != nil || len(res.Told) != 0 {
		t.Errorf("no task = %+v, %v", res, err)
	}
}
