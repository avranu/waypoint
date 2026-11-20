package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const noFindings = `{"findings":[]}`

func (se *shipEnv) poll() {
	se.t.Helper()
	se.setClock(se.clockNow().Add(70 * time.Second))
	se.s.PollCI(se.ctx)
	se.s.Wait()
}

func TestCIIsKnown(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	ci := se.watch(EvPRCI)
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.s.Wait()
	if pr := se.pr(5); pr.CIState != CIPending {
		t.Fatalf("CI before any check reported = %q", pr.CIState)
	}
	const backend, web = "ci / backend (pull_request)", "ci / web (pull_request)"
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: backend, State: CheckPending, URL: "https://x/b"}, CommitCheck{Name: web, State: CheckSuccess})
	se.poll()
	pr := se.pr(5)
	checks, _ := se.s.prChecks(se.ctx, pr)
	if pr.CIState != CIPending || len(checks) != 2 {
		t.Fatalf("pending: %q %+v", pr.CIState, checks)
	}
	if len(ci.all()) != 0 {
		t.Error("announced CI before it settled")
	}
	// Failed: pr.ci once, with the failed names and links, and the log tail kept.
	se.sg.logs[backend] = "step\nFAIL: TestX\n"
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: backend, State: CheckFailed, URL: "https://x/b"}, CommitCheck{Name: web, State: CheckSuccess})
	se.poll()
	got := ci.all()
	if len(got) != 1 || got[0].Data["state"] != "failed" || got[0].Data["sha"] != "1111111aaaa" || got[0].Data["number"] != 5 {
		t.Fatalf("events = %+v", got)
	}
	if failed, _ := got[0].Data["failed"].([]string); !slices.Equal(failed, []string{backend}) {
		t.Errorf("failed = %v", got[0].Data["failed"])
	}
	if links, _ := got[0].Data["links"].([]map[string]string); len(links) != 1 || links[0]["url"] != "https://x/b" {
		t.Errorf("links = %v", got[0].Data["links"])
	}
	fc, err := se.s.FailedChecks(se.ctx, se.admin, pr.ID)
	if err != nil || len(fc) != 1 || fc[0].Name != backend || fc[0].Log != "step\nFAIL: TestX\n" {
		t.Errorf("failed checks = %+v %v", fc, err)
	}
	// Waypoint's own status doesn't count, and isn't listed.
	checks, _ = se.s.prChecks(se.ctx, se.pr(5))
	for _, c := range checks {
		if ownCheck(c.Name) {
			t.Errorf("own check listed: %+v", c)
		}
	}
	// A webhook says checks changed: read at once. Passed now, but this head was announced.
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: backend, State: CheckSuccess}, CommitCheck{Name: web, State: CheckSuccess})
	if err := se.hook(se.f, "status", map[string]any{"sha": "1111111aaaa", "repository": map[string]any{"full_name": "me/app"}}); err != nil {
		t.Fatal(err)
	}
	if pr := se.pr(5); pr.CIState != CIPassed {
		t.Errorf("after the status webhook: %q", pr.CIState)
	}
	if len(ci.all()) != 1 {
		t.Error("announced one head twice")
	}
	// A new head has its own announcement.
	next := testPR
	next.HeadSHA = "2222222bbbb"
	se.sg.setChecks("2222222bbbb", CommitCheck{Name: backend, State: CheckSuccess}, CommitCheck{Name: web, State: CheckSuccess})
	se.pullHook("synchronize", next)
	se.s.Wait()
	got = ci.all()
	if len(got) != 2 || got[1].Data["state"] != "passed" || got[1].Data["sha"] != "2222222bbbb" {
		t.Fatalf("events = %+v", got)
	}
	// A repo with no CI: no checks for five minutes means none, not passed or failed.
	other := ForgePR{Number: 6, Title: "Docs", Author: "sam", HeadRef: "docs", HeadSHA: "3333333cccc", State: "open"}
	se.pullHook("opened", other)
	se.s.Wait()
	se.poll()
	if pr := se.pr(6); pr.CIState != CIPending {
		t.Errorf("one minute without checks: %q", pr.CIState)
	}
	se.setClock(se.clockNow().Add(6 * time.Minute))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if pr := se.pr(6); pr.CIState != CINone || len(ci.all()) != 2 {
		t.Errorf("no checks: %q, %d events", pr.CIState, len(ci.all()))
	}
	// Heads are polled for two hours at most.
	stuck := ForgePR{Number: 7, Title: "Slow", Author: "sam", HeadRef: "slow", HeadSHA: "4444444dddd", State: "open"}
	se.sg.setChecks("4444444dddd", CommitCheck{Name: backend, State: CheckPending})
	se.pullHook("opened", stuck)
	se.s.Wait()
	reads := se.sg.reads
	_ = reads
	se.setClock(se.clockNow().Add(3 * time.Hour))
	se.s.PollCI(se.ctx)
	se.s.Wait()
	if h := se.pr(7); h.CIState != CIPending {
		t.Errorf("stuck = %q", h.CIState)
	}
}

func TestCICancelledPushRunIsSuperseded(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.s.Wait()
	cancelled := func(name string) CommitCheck {
		return CommitCheck{Name: name, State: CheckFailed, Description: "Has been cancelled"}
	}
	// Forgejo cancelled the branch's push run when the pull request run started.
	se.sg.setChecks("1111111aaaa",
		cancelled("ci / backend (push)"), cancelled("ci / e2e (push)"),
		CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckPending})
	se.poll()
	pr := se.pr(5)
	if checks, _ := se.s.prChecks(se.ctx, pr); pr.CIState != CIPending || len(checks) != 2 {
		t.Fatalf("pull request run still going: %q %+v", pr.CIState, checks)
	}
	se.sg.setChecks("1111111aaaa",
		cancelled("ci / backend (push)"), cancelled("ci / e2e (push)"),
		CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess},
		CommitCheck{Name: "ci / e2e (pull_request)", State: CheckSuccess})
	se.poll()
	if pr := se.pr(5); pr.CIState != CIPassed {
		t.Errorf("every job passed under pull_request: %q", pr.CIState)
	}
	// A cancellation with no run of the same job that didn't fail still fails,
	// and so does a real failure next to a passing sibling.
	next := testPR
	next.HeadSHA = "2222222bbbb"
	se.sg.setChecks("2222222bbbb",
		cancelled("ci / backend (push)"),
		CommitCheck{Name: "ci / web (push)", State: CheckFailed, Description: "Failing after 40s"},
		CommitCheck{Name: "ci / web (pull_request)", State: CheckSuccess})
	se.pullHook("synchronize", next)
	se.s.Wait()
	pr = se.pr(5)
	checks, _ := se.s.prChecks(se.ctx, pr)
	if _, why := ciSummary(pr.CIState, checks); pr.CIState != CIFailed || why != "checks failed: ci / backend (push), ci / web (push)" {
		t.Errorf("unsuperseded failures: %q %q", pr.CIState, why)
	}
}

func TestJobOf(t *testing.T) {
	for name, want := range map[string]string{"ci / backend (push)": "ci / backend", "ci / web (pull_request)": "ci / web", "build": "", "lint (": "", "(x)": ""} {
		if got, _ := jobOf(name); got != want {
			t.Errorf("jobOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func armedPR(t *testing.T, se *shipEnv) *PullRequest {
	t.Helper()
	pr := se.pr(5)
	if pr.ArmedAt == nil {
		t.Fatalf("not armed: %+v", pr)
	}
	return pr
}

func TestMergeWhenReady(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.noWaits()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.s.Wait()
	id := se.pr(5).ID

	// Not ready yet, so merging now is refused with the reasons.
	_, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "not ready to merge") {
		t.Fatalf("merge now: %v", err)
	}
	// Arming waits.
	res, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true, Style: "squash"})
	if err != nil || !res.Armed || res.Merged || len(res.Reasons) == 0 {
		t.Fatalf("arm: %+v %v", res, err)
	}
	pr := armedPR(t, se)
	if pr.ArmedStyle != "squash" || pr.armedBy == nil {
		t.Errorf("armed = %+v", pr)
	}
	d, err := se.s.GetPullRequest(se.ctx, se.admin, id)
	if err != nil || d.Shipping.Armed == nil || d.Shipping.Armed.By != "robin" || d.Shipping.Armed.Style != "squash" || !d.Shipping.CanMerge || d.Shipping.Readiness.Ready {
		t.Fatalf("detail shipping = %+v %v", d.Shipping, err)
	}
	// The review passes (the gate goes green), then CI passes: merged.
	se.drain()
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged before CI passed")
	}
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	merges := se.sg.merged()
	if len(merges) != 1 || merges[0].HeadSHA != "1111111aaaa" || merges[0].Style != "squash" || !merges[0].DeleteBranch {
		t.Fatalf("merges = %+v", merges)
	}
	if pr := se.pr(5); pr.State != "merged" || pr.ArmedAt != nil {
		t.Errorf("after merging: %+v", pr)
	}
	page, err := se.s.ListAudit(se.ctx, se.admin, AuditQuery{Project: se.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range page.Entries {
		actions = append(actions, e.Action)
	}
	if !slices.Contains(actions, "pr.arm") || !slices.Contains(actions, "pr.merge") {
		t.Errorf("audit = %v", actions)
	}
}

func TestSyncRefreshesMergeable(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	id := se.pr(5).ID
	// The forge said "conflicts" while it was still working it out after the push.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET mergeable = 0 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged a pull request recorded as conflicting")
	}
	rd, _ := se.s.PRReadiness(se.ctx, se.admin, id)
	if !strings.Contains(strings.Join(rd.Reasons, "|"), "still checking") {
		t.Fatalf("reasons = %v", rd.Reasons)
	}
	// No webhook says it changed; the sync's list does, and it merges.
	listed := testPR
	listed.Mergeable = ptr(true)
	se.sg.mu.Lock()
	se.sg.listed = []*ForgePR{&listed}
	se.sg.mu.Unlock()
	se.s.RefreshPullRequests(se.ctx)
	se.s.Wait()
	if merges := se.sg.merged(); len(merges) != 1 || merges[0].HeadSHA != "1111111aaaa" {
		t.Fatalf("merges = %+v", merges)
	}
}

func TestMergeReadinessReasons(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain() // one finding: the gate fails
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / web", State: CheckFailed})
	se.poll()
	pr := se.pr(5)
	rd, err := se.s.PRReadiness(se.ctx, se.admin, pr.ID)
	if err != nil || rd.Ready {
		t.Fatalf("readiness = %+v %v", rd, err)
	}
	all := strings.Join(rd.Reasons, " | ")
	for _, want := range []string{"checks failed: ci / web", "review gate: 1 open finding"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons lack %q: %s", want, all)
		}
	}
	// A person's request for changes blocks; their later approval doesn't.
	insert := func(kind string, at int64) {
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at) VALUES (?,?,?,?,?,?)`, pr.ID, kind, "ann", 0, "", at); err != nil {
			t.Fatal(err)
		}
	}
	insert("changes_requested", 1000)
	rd, _ = se.s.PRReadiness(se.ctx, se.admin, pr.ID)
	if !strings.Contains(strings.Join(rd.Reasons, "|"), "ann asked for changes") {
		t.Errorf("reasons = %v", rd.Reasons)
	}
	insert("approved", 2000)
	rd, _ = se.s.PRReadiness(se.ctx, se.admin, pr.ID)
	if strings.Contains(strings.Join(rd.Reasons, "|"), "ann") {
		t.Errorf("approved later but still blocked: %v", rd.Reasons)
	}
	// Draft, conflicts.
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'draft', mergeable = 0 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	rd, _ = se.s.PRReadiness(se.ctx, se.admin, pr.ID)
	all = strings.Join(rd.Reasons, "|")
	if !strings.Contains(all, "still a draft") && !strings.Contains(all, "conflicts") {
		t.Errorf("reasons = %v", rd.Reasons)
	}
	// A viewer sees readiness but can't merge; strangers can't see the pull request.
	viewer := se.user("vera", false)
	if err := se.s.SetMember(se.ctx, se.admin, se.p.Key, "vera", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.PRReadiness(se.ctx, viewer, pr.ID); err != nil {
		t.Errorf("a viewer can't read readiness: %v", err)
	}
	if _, err := se.s.MergePR(se.ctx, viewer, pr.ID, MergeInput{}); err == nil {
		t.Error("a viewer merged")
	}
	stranger := se.user("stan", false)
	if _, err := se.s.PRReadiness(se.ctx, stranger, pr.ID); err == nil {
		t.Error("a stranger read a pull request")
	}
}

func TestMergeAgentsAndLabels(t *testing.T) {
	se := newShipEnv(t, "github")
	se.noWaits()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	mine := ForgePR{Number: 5, Title: "By Claude", Author: "robin", HeadRef: "c", HeadSHA: "1111111aaaa", State: "open",
		Body: "Generated with [Claude Code](https://claude.com/claude-code)"}
	other := ForgePR{Number: 6, Title: "By Codex", Author: "robin", HeadRef: "x", HeadSHA: "2222222bbbb", State: "open", Body: "Generated by Codex"}
	se.pullHook("opened", mine)
	se.pullHook("opened", other)
	se.s.Wait()
	claude := agentActorFor(se.admin, "anthropic/claude-opus-5-5")
	if res, err := se.s.MergePR(se.ctx, claude, se.pr(5).ID, MergeInput{WhenReady: true}); err != nil || !res.Armed {
		t.Fatalf("an agent arming its own pull request: %+v %v", res, err)
	}
	if pr := armedPR(t, se); pr.ArmedAgent != "anthropic/claude-opus-5-5" {
		t.Errorf("armed agent = %q", pr.ArmedAgent)
	}
	if _, err := se.s.MergePR(se.ctx, claude, se.pr(6).ID, MergeInput{WhenReady: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent armed another agent's pull request: %v", err)
	}
	mcp := &Actor{User: se.admin.User, MCP: true}
	if _, err := se.s.MergePR(se.ctx, mcp, se.pr(5).ID, MergeInput{WhenReady: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an unidentified agent armed: %v", err)
	}
	if err := se.s.DisarmMerge(se.ctx, claude, se.pr(5).ID); err != nil {
		t.Fatal(err)
	}
	if pr := se.pr(5); pr.ArmedAt != nil {
		t.Error("still armed")
	}

	// A repo that merges by hand: an agent's arm (wp ship) is declined and
	// leaves it out of the merge line; a person still arms it (WP-520).
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	if res, err := se.s.MergePR(se.ctx, claude, se.pr(5).ID, MergeInput{WhenReady: true}); err != nil || res.Armed || !res.ByHand || len(res.Reasons) != 1 {
		t.Fatalf("an agent arming on a merge-by-hand repo: %+v %v", res, err)
	}
	if pr := se.pr(5); pr.ArmedAt != nil {
		t.Error("an agent armed a pull request its repo merges by hand")
	}
	if res, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{WhenReady: true}); err != nil || !res.Armed {
		t.Fatalf("a person arming on a merge-by-hand repo: %+v %v", res, err)
	}
	if err := se.s.DisarmMerge(se.ctx, se.admin, se.pr(5).ID); err != nil {
		t.Fatal(err)
	}
	se.policy(MergePolicyInput{AutoMerge: ptr(true)})

	// The automerge label arms; taking it off disarms; a person's disarming sticks while it is on.
	labelled := ForgePR{Number: 8, Title: "Labelled", Author: "sam", HeadRef: "l", HeadSHA: "5555555eeee", State: "open"}
	hook := func(labels ...string) {
		t.Helper()
		var ls []map[string]string
		for _, l := range labels {
			ls = append(ls, map[string]string{"name": l})
		}
		err := se.hook(se.f, "pull_request", map[string]any{"action": "labeled", "pull_request": map[string]any{
			"number": 8, "title": "Labelled", "state": "open", "html_url": "https://git.example.com/me/app/pulls/8", "labels": ls,
			"user": map[string]string{"login": "sam"}, "head": map[string]string{"ref": "l", "sha": "5555555eeee"}, "base": map[string]string{"ref": "main"}},
			"repository": map[string]any{"full_name": "me/app"}})
		if err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	se.fg.setPR(se.f, "me/app", labelled, sampleDiff)
	hook("automerge")
	if pr := se.pr(8); pr.ArmedAt == nil || !pr.ArmedLabel || pr.armedBy != nil {
		t.Fatalf("labelled: %+v", pr)
	}
	hook()
	if pr := se.pr(8); pr.ArmedAt != nil {
		t.Errorf("label removed but armed: %+v", pr)
	}
	hook("AutoMerge")
	if pr := se.pr(8); pr.ArmedAt == nil {
		t.Error("labels are case-insensitive")
	}
	if err := se.s.DisarmMerge(se.ctx, se.admin, se.pr(8).ID); err != nil {
		t.Fatal(err)
	}
	hook("automerge")
	if pr := se.pr(8); pr.ArmedAt != nil {
		t.Error("the label re-armed what a person disarmed")
	}
	hook()
	hook("automerge")
	if pr := se.pr(8); pr.ArmedAt == nil {
		t.Error("a new label doesn't arm")
	}
	// The label alone merges once it's ready, audited as such (the merge
	// reads the pull request afresh: the forge has the label).
	labelled.Labels = []string{"automerge"}
	se.fg.setPR(se.f, "me/app", labelled, sampleDiff)
	se.sg.setChecks("5555555eeee", CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.poll()
	if m := se.sg.merged(); len(m) != 1 || m[0].HeadSHA != "5555555eeee" {
		t.Fatalf("merges = %+v", m)
	}
	page, _ := se.s.ListAudit(se.ctx, se.admin, AuditQuery{Project: se.p.Key})
	found := false
	for _, e := range page.Entries {
		if e.Action == "pr.merge" && e.Actor == "automerge label" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry for the label's merge: %+v", page.Entries)
	}
}

func TestMergeRefusedByForge(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)}) // merged by hand first
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / web", State: CheckSuccess})
	se.poll()
	se.sg.mergeErr = &forgeAPIError{method: "POST", path: "/merge", status: 405, body: "branch protection"}
	_, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "can't be merged") {
		t.Fatalf("refused: %v", err)
	}
	// Armed, it keeps the reason for the page and stays armed.
	res, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{WhenReady: true})
	if err != nil || !res.Armed {
		t.Fatalf("arm: %+v %v", res, err)
	}
	pr := armedPR(t, se)
	if !strings.Contains(pr.armedNote, "can't be merged") {
		t.Errorf("note = %q", pr.armedNote)
	}
	// A moved head is what the forge decides: Waypoint passes the head it checked.
	se.sg.mergeErr = nil
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{Style: "rebase", DeleteBranch: new(bool)}); err != nil {
		t.Fatal(err)
	}
	m := se.sg.merged()
	if len(m) == 0 || m[len(m)-1].HeadSHA != "1111111aaaa" || m[len(m)-1].DeleteBranch || m[len(m)-1].Style != "rebase" {
		t.Errorf("merge options = %+v", m)
	}
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); !IsInvalid(err) {
		t.Errorf("merging a merged pull request: %v", err)
	}
}

func TestPushesAndBranches(t *testing.T) {
	se := newShipEnv(t, "github")
	events := se.watch(EvRepoPushed, EvRepoRelease)
	push := func(ref, before, after string, files ...string) {
		t.Helper()
		err := se.hook(se.f, "push", map[string]any{"ref": ref, "before": before, "after": after, "pusher": map[string]string{"name": "sam"},
			"repository": map[string]any{"full_name": "me/app"},
			"commits":    []map[string]any{{"id": after, "message": "work", "url": "u", "added": files[:1], "modified": files[1:]}}})
		if err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	zero := strings.Repeat("0", 40)
	push("refs/heads/feat-x", zero, "aaaaaaa1", "web/b.tsx", "api/a.go")
	got := events.all()
	if len(got) != 1 || got[0].Kind != EvRepoPushed || got[0].Data["branch"] != "feat-x" || got[0].Data["created"] != true || got[0].Data["pusher"] != "sam" || got[0].ProjectID != se.p.ID {
		t.Fatalf("events = %+v", got)
	}
	if paths, _ := got[0].Data["paths"].([]string); !slices.Equal(paths, []string{"api/a.go", "web/b.tsx"}) {
		t.Errorf("paths = %v", got[0].Data["paths"])
	}
	push("refs/tags/v1", zero, "aaaaaaa1", "x")
	push("refs/heads/main", "bbbbbbb0", "bbbbbbb1", "README.md")
	if len(events.all()) != 2 {
		t.Errorf("a tag push was announced (or main not): %d events", len(events.all()))
	}
	// A branch with a pull request shows it; main is not "in work".
	se.sg.setChecks("aaaaaaa1", CommitCheck{Name: "ci / web", State: CheckPending})
	pr := ForgePR{Number: 5, Title: "Fix", Author: "sam", HeadRef: "feat-x", HeadSHA: "aaaaaaa1", State: "open"}
	se.pullHook("opened", pr)
	se.s.Wait()
	branches, err := se.s.ListBranches(se.ctx, se.admin, se.p.Key)
	if err != nil || len(branches) != 1 {
		t.Fatalf("branches = %+v %v", branches, err)
	}
	b := branches[0]
	if b.Branch != "feat-x" || b.Repo != "me/app" || b.Pusher != "sam" || b.PR == nil || b.PR.Number != 5 || b.CI != CIPending || b.HeadSHA != "aaaaaaa1" || b.Forge != "github" {
		t.Errorf("branch = %+v", b)
	}
	// Once its pull request is merged and nothing was pushed since, the work is done.
	merged := pr
	merged.State = "merged"
	se.pullHook("closed", merged)
	se.s.Wait()
	if branches, _ := se.s.ListBranches(se.ctx, se.admin, se.p.Key); len(branches) != 0 {
		t.Errorf("a merged branch is still in work: %+v", branches)
	}
	// A new push to it, with no pull request open, is work again.
	se.setClock(se.clockNow().Add(time.Hour))
	push("refs/heads/feat-x", "aaaaaaa1", "aaaaaaa2", "x.go")
	if branches, _ := se.s.ListBranches(se.ctx, se.admin, se.p.Key); len(branches) != 1 || branches[0].PR != nil {
		t.Errorf("branches = %+v", branches)
	}
	// Deleting the branch ends it.
	if err := se.hook(se.f, "push", map[string]any{"ref": "refs/heads/feat-x", "before": "aaaaaaa2", "after": zero, "deleted": true,
		"repository": map[string]any{"full_name": "me/app"}}); err != nil {
		t.Fatal(err)
	}
	if branches, _ := se.s.ListBranches(se.ctx, se.admin, se.p.Key); len(branches) != 0 {
		t.Errorf("a deleted branch is listed: %+v", branches)
	}
	if _, err := se.s.ListBranches(se.ctx, se.user("stan", false), se.p.Key); err == nil {
		t.Error("a stranger listed a project's branches")
	}
	// A release.
	err = se.hook(se.f, "release", map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.4.0", "name": "1.4", "html_url": "https://x/r"},
		"repository": map[string]any{"full_name": "me/app"}})
	if err != nil {
		t.Fatal(err)
	}
	all := events.all()
	last := all[len(all)-1]
	if last.Kind != EvRepoRelease || last.Data["tag"] != "v1.4.0" || last.Data["repo"] != "me/app" || last.Data["url"] != "https://x/r" {
		t.Errorf("release event = %+v", last)
	}
}

func TestRepliesToWaypointHistoryEvent(t *testing.T) {
	// A submitted review reaches automations as pr.review.
	se := newShipEnv(t, "github")
	reviews := se.watch(EvPRReview)
	se.pullHook("opened", testPR)
	err := se.hook(se.f, "pull_request_review", map[string]any{"action": "submitted",
		"pull_request": map[string]any{"number": 5, "title": "Fix login", "state": "open", "user": map[string]string{"login": "sam"},
			"head": map[string]string{"ref": "fix-login", "sha": "1111111aaaa"}, "base": map[string]string{"ref": "main"}},
		"review":     map[string]any{"id": 4, "state": "changes_requested", "body": "no", "user": map[string]string{"login": "ann"}, "commit_id": "1111111aaaa"},
		"repository": map[string]any{"full_name": "me/app"}})
	if err != nil {
		t.Fatal(err)
	}
	got := reviews.all()
	if len(got) != 1 || got[0].Data["reviewer"] != "ann" || got[0].Data["verdict"] != "changes_requested" || got[0].Data["bot"] != false || got[0].Data["sha"] != "1111111aaaa" {
		t.Fatalf("pr.review = %+v", got)
	}
}
