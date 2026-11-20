package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// policy changes the test repo's merge policy.
func (se *prEnv) policy(in MergePolicyInput) {
	se.t.Helper()
	if err := se.s.SetRepoMergePolicy(se.ctx, se.admin, se.p.Key, se.repo.ID, in); err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

// noWaits lets armed merges go as soon as the rest is ready: for tests of
// merge when ready itself.
func (se *prEnv) noWaits() {
	se.t.Helper()
	se.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
}

// prHook sends a pull_request webhook with pr's labels, draft state and
// head repo.
func (se *shipEnv) prHook(action string, pr ForgePR) {
	se.t.Helper()
	se.fg.setPR(se.f, "me/app", pr, sampleDiff)
	head := map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA, "repo": map[string]any{"full_name": cmp(pr.HeadRepo, "me/app")}}
	if pr.HeadRepo == "-" { // the forge doesn't say (GitHub, once a fork is deleted)
		delete(head, "repo")
	}
	labels := []map[string]string{}
	for _, l := range pr.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	err := se.hook(se.f, "pull_request", map[string]any{"action": action, "repository": map[string]any{"full_name": "me/app"},
		"pull_request": map[string]any{"number": pr.Number, "title": pr.Title, "body": pr.Body, "html_url": fmt.Sprintf("https://git.example.com/me/app/pulls/%d", pr.Number),
			"state": "open", "draft": pr.State == "draft", "labels": labels, "user": map[string]string{"login": pr.Author},
			"head": head, "base": map[string]string{"ref": cmp(pr.BaseRef, "main")}}})
	if err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

// reasons is why pull request n isn't ready, in one line.
func (se *shipEnv) reasons(n int) string {
	se.t.Helper()
	rd, err := se.s.PRReadiness(se.ctx, se.admin, se.pr(n).ID)
	if err != nil {
		se.t.Fatal(err)
	}
	return strings.Join(rd.Reasons, " | ")
}

func TestMergeAutomaticallyArms(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	pr := func(n int, title, head string, labels ...string) ForgePR {
		return ForgePR{Number: n, Title: title, Author: "sam", HeadRef: head, HeadSHA: fmt.Sprintf("%07daaaa", n), State: "open", Labels: labels}
	}
	armed := func(n int) bool { return se.pr(n).ArmedAt != nil }

	// An ordinary pull request is armed as soon as Waypoint sees it, as the setting.
	se.prHook("opened", pr(5, "Fix the thing", "fix"))
	p5 := se.pr(5)
	if p5.ArmedAt == nil || p5.ArmedAgent != mergeSettingAgent || p5.armedBy != nil {
		t.Fatalf("not armed by the setting: %+v", p5)
	}
	d, err := se.s.GetPullRequest(se.ctx, se.admin, p5.ID)
	if err != nil || d.Shipping.Armed == nil || !d.Shipping.Armed.Setting || d.Shipping.Armed.Agent != "" {
		t.Fatalf("armed = %+v %v", d.Shipping.Armed, err)
	}
	page, _ := se.s.ListAudit(se.ctx, se.admin, AuditQuery{Project: se.p.Key})
	if !slices.ContainsFunc(page.Entries, func(e *AuditRecord) bool { return e.Action == "pr.arm" && e.Actor == mergeSettingName }) {
		t.Errorf("no audit entry for the setting's arming: %+v", page.Entries)
	}

	// A draft is armed when it leaves draft.
	draft := pr(6, "Draft", "d")
	draft.State = "draft"
	se.prHook("opened", draft)
	if armed(6) {
		t.Error("a draft was armed")
	}
	draft.State = "open"
	se.prHook("ready_for_review", draft)
	if !armed(6) {
		t.Error("not armed when it left draft")
	}

	// Never: forks, do-not-merge and wip labels, release pull requests.
	fork := pr(7, "From a fork", "f")
	fork.HeadRepo = "someone/app"
	same := pr(14, "From its own repo", "s")
	same.HeadRepo = "Me/App"
	unknown := pr(18, "From somewhere", "u")
	unknown.HeadRepo = "-"
	for _, p := range []ForgePR{fork, same, unknown, pr(8, "Half done", "w", "WIP"), pr(9, "Hold", "h", "Do-Not-Merge"),
		pr(10, "chore(main): release 1.2.0", "r1"), pr(11, "Release v2", "r2"), pr(12, "Bump", "release-please--branches--main"),
		pr(13, "Releases page: fix the table", "rp")} {
		se.prHook("opened", p)
	}
	for n, want := range map[int]bool{7: false, 14: true, 18: false, 8: false, 9: false, 10: false, 11: false, 12: false, 13: true} {
		if armed(n) != want {
			t.Errorf("#%d (%s) armed = %v, want %v", n, se.pr(n).Title, !want, want)
		}
	}

	// A do-not-merge label later takes the setting's arming off; taking it off arms again.
	held := pr(13, "Releases page: fix the table", "rp", "do-not-merge")
	se.prHook("labeled", held)
	if armed(13) {
		t.Error("still armed with do-not-merge")
	}
	held.Labels = nil
	se.prHook("unlabeled", held)
	if !armed(13) {
		t.Error("not armed again once the label came off")
	}

	// A person taking it off sticks, through pushes.
	if err := se.s.DisarmMerge(se.ctx, se.admin, p5.ID); err != nil {
		t.Fatal(err)
	}
	pushed := pr(5, "Fix the thing", "fix")
	pushed.HeadSHA = "5555555bbbb"
	se.prHook("synchronize", pushed)
	if armed(5) {
		t.Error("the setting armed again what a person disarmed")
	}

	// Open before the setting existed (the migration marks them): an edit
	// doesn't arm it, its next push does; turning the setting on does too.
	se.prHook("opened", pr(16, "Old work", "o"))
	se.prHook("opened", pr(17, "Older work", "o2"))
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET armed_at = NULL, armed_agent = '', arm_declined = 2 WHERE number IN (16, 17)`); err != nil {
		t.Fatal(err)
	}
	se.prHook("edited", pr(16, "Old work, renamed", "o"))
	if armed(16) {
		t.Error("an edit armed a pull request open since before the setting")
	}
	old := pr(16, "Old work, renamed", "o")
	old.HeadSHA = "1616161cccc"
	se.prHook("synchronize", old)
	if !armed(16) || armed(17) {
		t.Errorf("after a push: #16 %v (want armed), #17 %v", armed(16), armed(17))
	}
	// One recorded without its head (its webhook never came) getting it isn't a push.
	se.prHook("opened", pr(19, "Headless", "h19"))
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET armed_at = NULL, armed_agent = '', head_sha = '', arm_declined = 2 WHERE number = 19`); err != nil {
		t.Fatal(err)
	}
	se.prHook("edited", pr(19, "Headless", "h19"))
	if armed(19) || se.pr(19).HeadSHA == "" {
		t.Errorf("a headless pull request from before the setting, now with its head: armed %v", armed(19))
	}

	// Turned off: the setting's own arming comes off, a person's stays.
	if _, err := se.s.MergePR(se.ctx, se.admin, se.pr(14).ID, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	if armed(6) || armed(13) || !armed(14) {
		t.Errorf("after turning it off: #6 %v, #13 %v, #14 (a person's) %v", armed(6), armed(13), armed(14))
	}
	se.prHook("opened", pr(15, "New while off", "n"))
	if armed(15) {
		t.Error("armed while the setting is off")
	}
	// On again: armed, except what a person disarmed.
	se.policy(MergePolicyInput{AutoMerge: ptr(true)})
	if !armed(6) || !armed(13) || !armed(15) || !armed(17) || armed(5) {
		t.Errorf("after turning it on: #6 %v, #13 %v, #15 %v, #17 (open since before) %v, #5 (disarmed by a person) %v", armed(6), armed(13), armed(15), armed(17), armed(5))
	}
	repos, _ := se.s.ListProjectRepos(se.ctx, se.admin, se.p.Key)
	if len(repos) != 1 || !repos[0].AutoMerge || repos[0].ReviewWaitMinutes != 30 || repos[0].QuietMinutes != 10 || len(repos[0].ExternalReviewers) != 0 {
		t.Errorf("repo policy = %+v", repos[0].MergePolicy)
	}
	// Only owners set it; minutes are bounded.
	if err := se.s.SetRepoMergePolicy(se.ctx, se.admin, se.p.Key, se.repo.ID, MergePolicyInput{QuietMinutes: ptr(-1)}); !IsInvalid(err) {
		t.Errorf("negative quiet: %v", err)
	}
	if err := se.s.SetRepoMergePolicy(se.ctx, se.user("vic", false), se.p.Key, se.repo.ID, MergePolicyInput{AutoMerge: ptr(false)}); err == nil {
		t.Error("a stranger changed the policy")
	}
}

// GitLab names no head repo, only project ids: the same project's is its own.
func TestHeadRepoOfGitLab(t *testing.T) {
	own := glMR{IID: 1, State: "opened", SourceProjectID: 7, TargetProjectID: 7}.pr()
	fork := glMR{IID: 2, State: "opened", SourceProjectID: 9, TargetProjectID: 7}.pr()
	unknown := glMR{IID: 3, State: "opened"}.pr()
	if got := headRepoOf(own, "g/app"); got != "g/app" {
		t.Errorf("own project: %q", got)
	}
	if headRepoOf(fork, "g/app") != "" || headRepoOf(unknown, "g/app") != "" {
		t.Error("a fork or an unknown source counted as the repo's own")
	}
}

// A person taking merge when ready off while an armed merge is under way
// stops it: the merge looks again after reading the forge.
func TestDisarmStopsAnArmedMerge(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.policy(MergePolicyInput{AutoMerge: ptr(false), ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	pr := se.pr(5)
	// As if the armed merge had read it armed, and a person disarmed it since.
	_, err := se.s.mergeNow(se.ctx, se.admin, pr, MergeInput{armed: true}, "armed by test")
	if !IsInvalid(err) || !strings.Contains(err.Error(), "taken off") || len(se.sg.merged()) != 0 {
		t.Fatalf("merged a disarmed pull request: %v %+v", err, se.sg.merged())
	}
}

// A repo expects no external reviewers until an owner names some, on
// Forgejo too (WP-1185).
func TestMergePolicyExternalReviewers(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	repos, err := se.s.ListProjectRepos(se.ctx, se.admin, se.p.Key)
	if err != nil || len(repos) != 1 || repos[0].ExternalReviewers == nil || len(repos[0].ExternalReviewers) != 0 {
		t.Fatalf("reviewers = %+v %v", repos, err)
	}
	if pol, ok, err := se.s.repoMergePolicy(se.ctx, repos[0].ForgeID, repos[0].Repo); err != nil || !ok || len(pol.ExternalReviewers) != 0 {
		t.Fatalf("effective reviewers = %v %v %v", pol.ExternalReviewers, ok, err)
	}
	se.policy(MergePolicyInput{ExternalReviewers: &[]string{" llm-review ", "LLM-review", ""}, RequiredChecks: &[]string{"local-ci"}})
	repos, _ = se.s.ListProjectRepos(se.ctx, se.admin, se.p.Key)
	if strings.Join(repos[0].ExternalReviewers, ",") != "llm-review" || strings.Join(repos[0].RequiredChecks, ",") != "local-ci" {
		t.Errorf("policy = %+v", repos[0].MergePolicy)
	}
}

func TestReviewsSettle(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.policy(MergePolicyInput{RequiredChecks: &[]string{"local-ci"}, ExternalReviewers: &[]string{"llm-review", "forge-checks/codeql"}})
	seen := se.clockNow()
	se.pullHook("opened", testPR) // armed by the setting
	waitEnd := se.s.clock(seen.Add(30 * time.Minute))

	// It waits for Waypoint's review rule, the external reviewers and the required check.
	r := se.reasons(5)
	if want := "waiting for Waypoint's “Review pull requests”, llm-review and forge-checks/codeql on 1111111 until " + waitEnd; !strings.Contains(r, want) {
		t.Errorf("reasons lack %q: %s", want, r)
	}
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	if r := se.reasons(5); !strings.Contains(r, "required check local-ci hasn't reported on 1111111") {
		t.Errorf("reasons = %s", r)
	}
	// The checks report, CodeQL among them; Waypoint's review runs (a run for this head counts).
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess},
		CommitCheck{Name: "local-ci", State: CheckSuccess}, CommitCheck{Name: "forge-checks/codeql", State: CheckSuccess})
	se.s.refreshPRChecks(se.ctx, se.pr(5).ID) // the head's checks had settled: no more polling
	se.s.Wait()
	se.drain()
	r = se.reasons(5)
	if strings.Contains(r, "local-ci") || strings.Contains(r, "Review pull requests") || strings.Contains(r, "codeql") ||
		!strings.Contains(r, "waiting for llm-review on 1111111 until "+waitEnd) {
		t.Errorf("after the review and checks: %s", r)
	}
	// Waypoint's review was posted: quiet for ten minutes from it.
	if want := "quiet until " + se.s.clock(se.clockNow().Add(10*time.Minute)) + " (last: Waypoint's review)"; !strings.Contains(r, want) {
		t.Errorf("reasons lack %q: %s", want, r)
	}
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged before reviews settled")
	}

	// llm-review reviews this head: reviewers settled, and the quiet period restarts.
	se.setClock(se.clockNow().Add(5 * time.Minute))
	err := se.hook(se.f, "pull_request_review", map[string]any{"action": "submitted", "repository": map[string]any{"full_name": "me/app"},
		"pull_request": map[string]any{"number": 5}, "sender": map[string]any{"login": "llm-review"},
		"review": map[string]any{"id": 77, "user": map[string]any{"login": "llm-review"}, "state": "commented", "commit_id": "1111111aaaa",
			"body": "<!-- llm-review:v1 sha=1111111aaaa model=m -->"}})
	if err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	reviewed := se.clockNow()
	r = se.reasons(5)
	if strings.Contains(r, "waiting for") || !strings.Contains(r, "quiet until "+se.s.clock(reviewed.Add(10*time.Minute))+" (last: review by llm-review)") {
		t.Errorf("after llm-review: %s", r)
	}
	rd, _ := se.s.PRReadiness(se.ctx, se.admin, se.pr(5).ID)
	if !rd.OnlyWaiting {
		t.Error("only waiting, but not said so")
	}

	// A comment by anyone starts the quiet period again.
	se.setClock(se.clockNow().Add(8 * time.Minute))
	err = se.hook(se.f, "issue_comment", map[string]any{"action": "created", "repository": map[string]any{"full_name": "me/app"},
		"issue":   map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 501, "body": "Is the migration safe?", "user": map[string]any{"login": "bob"}}})
	if err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	commented := se.clockNow()
	if r := se.reasons(5); !strings.Contains(r, "quiet until "+se.s.clock(commented.Add(10*time.Minute))+" (last: comment by bob)") {
		t.Errorf("after bob's comment: %s", r)
	}
	// Nothing merges before then, even on the scheduler's tick.
	se.setClock(se.clockNow().Add(9 * time.Minute))
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged during the quiet period")
	}
	// The tick after the quiet period merges it, on the head it checked.
	se.setClock(se.clockNow().Add(90 * time.Second))
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	m := se.sg.merged()
	if len(m) != 1 || m[0].HeadSHA != "1111111aaaa" {
		t.Fatalf("merges = %+v", m)
	}
	page, _ := se.s.ListAudit(se.ctx, se.admin, AuditQuery{Project: se.p.Key})
	if !slices.ContainsFunc(page.Entries, func(e *AuditRecord) bool { return e.Action == "pr.merge" && e.Actor == mergeSettingName }) {
		t.Errorf("no audit entry for the setting's merge: %+v", page.Entries)
	}
}

// When a reviewer never comes, the review wait ends the waiting; a person
// may merge now while an armed merge still waits.
func TestReviewWaitTimesOut(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.policy(MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}, QuietMinutes: ptr(0)})
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	if r := se.reasons(5); !strings.Contains(r, "waiting for llm-review") || len(se.sg.merged()) != 0 {
		t.Fatalf("reasons = %s, merges %d", r, len(se.sg.merged()))
	}
	var wake int64
	if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT merge_wake_at FROM pull_requests WHERE id = ?`, se.pr(5).ID).Scan(&wake); err != nil || wake == 0 {
		t.Fatalf("no wake time: %v", err)
	}
	se.setClock(se.clockNow().Add(27 * time.Minute)) // 28 since the head was seen
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged before the review wait ended")
	}
	se.setClock(se.clockNow().Add(3 * time.Minute))
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("not merged once the review wait passed: %+v (%s)", m, se.reasons(5))
	}
}

// A person merging now doesn't wait for reviews to settle.
func TestMergeNowSkipsTheWaits(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.policy(MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}})
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks("1111111aaaa", CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	if len(se.sg.merged()) != 0 {
		t.Fatal("merged while waiting")
	}
	if res, err := se.s.MergePR(se.ctx, se.admin, se.pr(5).ID, MergeInput{}); err != nil || !res.Merged {
		t.Fatalf("merge now: %+v %v", res, err)
	}
}

// Merge automatically for some base branches only (WP-1041): release
// branches merge by themselves while the default branch stays by hand.
func TestMergeAutomaticallyByBase(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	armed := func(n int) bool { return se.pr(n).ArmedAt != nil }
	open := func(n int, base string) {
		se.prHook("opened", ForgePR{Number: n, Title: fmt.Sprintf("Work %d", n), Author: "sam", HeadRef: fmt.Sprintf("w%d", n),
			HeadSHA: fmt.Sprintf("%07daaaa", n), State: "open", BaseRef: base, Body: "Generated with [Claude Code](https://claude.com/claude-code)"})
	}
	want := func(step string, states map[int]bool) {
		t.Helper()
		for n, w := range states {
			if armed(n) != w {
				t.Errorf("%s: #%d (into %s) armed = %v, want %v", step, n, se.pr(n).BaseRef, !w, w)
			}
		}
	}

	// Every branch but the default (main, as the forge says).
	se.policy(MergePolicyInput{AutoMergeInto: ptr(IntoOthers)})
	open(1, "main")
	open(2, "release/v_0_9_0")
	open(3, "develop")
	want("others", map[int]bool{1: false, 2: true, 3: true})
	repos, _ := se.s.ListProjectRepos(se.ctx, se.admin, se.p.Key)
	if r := repos[0]; r.AutoMergeInto != IntoOthers || r.DefaultBranch != "main" {
		t.Errorf("repo = into %q, default %q", r.AutoMergeInto, r.DefaultBranch)
	}
	d, err := se.s.GetPullRequest(se.ctx, se.admin, se.pr(2).ID)
	if err != nil || d.Shipping.Armed == nil || d.Shipping.Armed.Scope != "into branches other than main" {
		t.Errorf("armed = %+v %v", d.Shipping.Armed, err)
	}

	// An agent's wp ship into the default branch merges by hand; a person still arms it.
	claude := agentActorFor(se.admin, "anthropic/claude-opus-5-5")
	res, err := se.s.MergePR(se.ctx, claude, se.pr(1).ID, MergeInput{WhenReady: true})
	if err != nil || res.Armed || !res.ByHand || len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "into the default branch (main) by hand") {
		t.Fatalf("an agent arming into the default branch: %+v %v", res, err)
	}
	if armed(1) {
		t.Error("an agent armed a pull request into the default branch")
	}

	// Branches matching globs: the setting's arming comes off the others.
	se.policy(MergePolicyInput{AutoMergeInto: ptr(IntoMatch), AutoMergeBases: &[]string{"release/**"}})
	want("match", map[int]bool{1: false, 2: true, 3: false})
	if d, _ := se.s.GetPullRequest(se.ctx, se.admin, se.pr(2).ID); d == nil || d.Shipping.Armed == nil || d.Shipping.Armed.Scope != "into release/**" {
		t.Errorf("armed scope after match = %+v", d)
	}
	if res, err := se.s.MergePR(se.ctx, claude, se.pr(3).ID, MergeInput{WhenReady: true}); err != nil || !res.ByHand {
		t.Errorf("an agent arming into develop: %+v %v", res, err)
	}

	// The default branch only, then any branch again.
	se.policy(MergePolicyInput{AutoMergeInto: ptr(IntoDefault)})
	want("default", map[int]bool{1: true, 2: false, 3: false})
	se.policy(MergePolicyInput{AutoMergeInto: ptr(IntoAny)})
	want("any", map[int]bool{1: true, 2: true, 3: true})

	if err := se.s.SetRepoMergePolicy(se.ctx, se.admin, se.p.Key, se.repo.ID, MergePolicyInput{AutoMergeInto: ptr("production")}); !IsInvalid(err) {
		t.Errorf("an unknown scope: %v", err)
	}
}

// A repo attached to several projects arms only into a base every one of
// them lets in.
func TestMergePolicyBaseScopesCombine(t *testing.T) {
	pol := MergePolicy{AutoMerge: true, into: []baseScope{{IntoOthers, nil}, {IntoMatch, []string{"release/*"}}}}
	for base, want := range map[string]bool{"main": false, "develop": false, "release/v1": true, "release/v1/x": false} {
		if got := pol.baseSkip(base, "main") == ""; got != want {
			t.Errorf("%s: in scope = %v, want %v", base, got, want)
		}
	}
	if !pol.needsDefault() {
		t.Error("others needs the default branch")
	}
	if got := pol.scopeText("main"); got != "into branches other than main, and into release/*" {
		t.Errorf("scopeText = %q", got)
	}
	if (MergePolicy{AutoMergeInto: IntoAny}).scopeText("main") != "" {
		t.Error("any branch has a scope text")
	}
	// A base the forge didn't name is in no scope; branch names have case.
	if (MergePolicy{AutoMergeInto: IntoOthers}).baseSkip("", "main") == "" {
		t.Error("others let in an unknown base")
	}
	if l, _ := cleanPolicyList([]string{"Release/**", "release/**"}, "bases", true); len(l) != 2 {
		t.Errorf("bases deduped ignoring case: %v", l)
	}
}

// An agent's arming answers to the branches too (WP-1041): narrowing the
// scope or retargeting the pull request takes it off, and an armed merge
// asks the forge for the default branch afresh before it merges.
func TestMergeAutomaticallyByBaseArms(t *testing.T) {
	se := newShipEnv(t, "github")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(0)})
	armed := func(n int) bool { return se.pr(n).ArmedAt != nil }
	pr := func(n int, base string, labels ...string) ForgePR {
		return ForgePR{Number: n, Title: fmt.Sprintf("Work %d", n), Author: "sam", HeadRef: fmt.Sprintf("w%d", n),
			HeadSHA: fmt.Sprintf("%07daaaa", n), State: "open", BaseRef: base, Labels: labels, Body: "Generated with [Claude Code](https://claude.com/claude-code)"}
	}
	claude := agentActorFor(se.admin, "anthropic/claude-opus-5-5")
	agentArms := func(n int) {
		t.Helper()
		if err := se.s.DisarmMerge(se.ctx, se.admin, se.pr(n).ID); err != nil {
			t.Fatal(err)
		}
		if res, err := se.s.MergePR(se.ctx, claude, se.pr(n).ID, MergeInput{WhenReady: true}); err != nil || !res.Armed {
			t.Fatalf("agent arming #%d: %+v %v", n, res, err)
		}
	}
	setDefault := func(d string) {
		se.sg.sm.Lock()
		se.sg.defaultBranch = d
		se.sg.sm.Unlock()
	}

	// Any branch: the agent arms into main; narrowing to the others takes it off.
	se.prHook("opened", pr(1, "main"))
	agentArms(1)
	se.policy(MergePolicyInput{AutoMergeInto: ptr(IntoOthers)})
	if armed(1) {
		t.Error("an agent's arming into the default branch stayed after narrowing")
	}
	// Retargeted from develop to main: the agent's arming comes off.
	se.prHook("opened", pr(3, "develop"))
	agentArms(3)
	se.prHook("edited", pr(3, "main"))
	if armed(3) {
		t.Error("an agent's arming stayed after a retarget into the default branch")
	}

	// The default branch renamed to trunk while main is remembered: a pull
	// request into trunk is armed on the stale word, but the merge asks
	// afresh and takes the arming off; one into a release branch merges.
	setDefault("trunk")
	se.prHook("opened", pr(5, "trunk"))
	se.prHook("opened", pr(6, "release/v1"))
	if !armed(5) || !armed(6) {
		t.Fatalf("armed: #5 %v, #6 %v", armed(5), armed(6))
	}
	se.drain()
	for _, n := range []int{5, 6} {
		se.sg.setChecks(se.pr(n).HeadSHA, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	}
	se.poll()
	m := se.sg.merged()
	if len(m) != 1 || m[0].HeadSHA != se.pr(6).HeadSHA || armed(5) {
		t.Errorf("merges = %+v, #5 armed %v (%s)", m, armed(5), se.reasons(5))
	}

	// The forge can't say which is the default branch: a do-not-merge label
	// still takes the setting's arming off; nothing new is armed.
	se.prHook("opened", pr(7, "release/v2"))
	if !armed(7) {
		t.Fatal("#7 not armed")
	}
	setDefault("-")
	se.s.defaultBranches.Clear()
	se.prHook("edited", pr(7, "release/v2", "do-not-merge"))
	se.prHook("opened", pr(8, "release/v3"))
	if armed(7) || armed(8) {
		t.Errorf("forge can't say the default: #7 (do-not-merge) armed %v, #8 armed %v", armed(7), armed(8))
	}
}
