package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// BehindBase and UpdateBranch on the fake: a head listed in behind lacks
// the base's tip.
func (g *shipForge) BehindBase(ctx context.Context, f *Forge, repo, baseRef, headSHA string) (string, bool, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.behindErr != nil {
		return "", false, g.behindErr
	}
	return cmp(g.baseTip, "tip-1"), g.behind[headSHA], nil
}

// Contains on the fake: every commit is in the base's tip except those listed
// in hidden (a read of the base that doesn't show a merge yet).
func (g *shipForge) Contains(ctx context.Context, f *Forge, repo, sha, headSHA string) (bool, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	return !g.hidden[sha], nil
}

func (g *shipForge) UpdateBranch(ctx context.Context, f *Forge, repo string, number int) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.updates = append(g.updates, number)
	g.union.log = append(g.union.log, "update")
	if g.union.conflictUntilDrop && !g.union.dropped {
		return &forgeAPIError{method: "POST", path: "/pulls/update", status: 409, body: "merge failed because of conflict"}
	}
	if err, ok := g.updateErrs[number]; ok {
		return err
	}
	return g.updateErr
}

func (g *shipForge) updated() int {
	g.sm.Lock()
	defer g.sm.Unlock()
	return len(g.updates)
}

// A green armed pull request whose head lacks the base's tip isn't merged:
// its branch is updated, once per tip, and it merges when the new head is
// current.
func TestArmedMergeWaitsForUpToDateHead(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID

	if r := se.reasons(5); !strings.Contains(r, "behind main") {
		t.Fatalf("reasons = %q", r)
	}
	se.inLine(id)
	if len(se.sg.merged()) != 0 || se.sg.updated() != 1 {
		t.Fatalf("merges %+v, updates %d", se.sg.merged(), se.sg.updated())
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "updated the branch") {
		t.Errorf("note = %q", n)
	}
	// Looking again for the same tip updates nothing more.
	se.inLine(id)
	if len(se.sg.merged()) != 0 || se.sg.updated() != 1 {
		t.Fatalf("looped: merges %+v, updates %d", se.sg.merged(), se.sg.updated())
	}
	// The base moved again: one more update.
	se.sg.baseTip = "tip-2"
	se.inLine(id)
	if se.sg.updated() != 2 {
		t.Fatalf("updates = %d", se.sg.updated())
	}
	// The head now contains the tip: it merges.
	delete(se.sg.behind, head)
	se.s.maybeAutoMerge(se.ctx, id)
	if len(se.sg.merged()) != 1 || se.sg.updated() != 2 {
		t.Fatalf("merges %+v, updates %d", se.sg.merged(), se.sg.updated())
	}
}

// An update the forge refuses as a conflict is reported on the pull request
// and not tried again for that tip; another refusal is tried again.
func TestUpdateBranchConflict(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.sg.updateErr = &forgeAPIError{method: "POST", path: "/repos/me/app/pulls/5/update", status: 409, body: "merge conflict"}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	se.inLine(id)
	se.inLine(id)
	if se.sg.updated() != 1 || len(se.sg.merged()) != 0 {
		t.Fatalf("updates %d, merges %+v", se.sg.updated(), se.sg.merged())
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "has conflicts with main") {
		t.Errorf("note = %q", n)
	}
	// The forge's mergeable flag still says yes; the round sent for the
	// conflict must see it in its work packet (WP-430).
	if todo, err := se.s.PRWorkPacket(se.ctx, SystemActor(), id); err != nil || !todo.Conflict || !strings.Contains(todo.Text, "Conflicts with main") {
		t.Errorf("work packet = %+v, %v", todo, err)
	}
}

// An update the forge fails to make is said on the pull request, and tried
// again by the line's sweep.
func TestUpdateBranchFailureIsSaid(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true}
	se.sg.updateErr = errors.New("HTTP 502")
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	se.s.Wait()
	before := se.sg.updated() // what the poll's own look at the line did
	se.inLine(id)
	se.inLine(id)
	// Joining the line steps it once, the look asked for is another, and the
	// second inLine one more: each asks the forge, which fails.
	if n := se.sg.updated() - before; n != 3 || len(se.sg.merged()) != 0 {
		t.Fatalf("updates %d since the poll, merges %+v", n, se.sg.merged())
	}
	if n := se.pr(5).armedNote; !strings.Contains(n, "couldn't bring the branch up to date") {
		t.Errorf("note = %q", n)
	}
}

// A repo without Waypoint's merge policy attached isn't touched.
func TestUpToDateNeedsPolicy(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.sg.behind = map[string]bool{"1111111aaaa": true}
	pr := &PullRequest{ForgeID: se.f.ID, Repo: "other/repo", BaseRef: "main", HeadSHA: "1111111aaaa"}
	if _, behind, _ := se.s.behindBase(se.ctx, pr); behind {
		t.Error("a repo with no policy was held for its base")
	}
}
