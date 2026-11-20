package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	ciBackend = "ci / backend (pull_request)"
	ciWeb     = "ci / web (pull_request)"
)

// prRule is a manual rule over the pull request named by its input.
func prRule(steps ...Action) Rule {
	return Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "pr", Type: "text"}}, Actions: steps}
}

func (a *agentEnv) openPR5() *PullRequest {
	a.t.Helper()
	a.pullHook("opened", testPR)
	a.s.Wait()
	return a.pr(5)
}

func TestOpenPRStep(t *testing.T) {
	ae := newAgentEnv(t)
	a := ae.addAuto(ae.p, "Open", Rule{Trigger: Trigger{Event: "manual"}, Inputs: []RuleInput{{Name: "branch", Type: "text"}}, Actions: []Action{
		{Type: "open_pr", ID: "pr", Extra: map[string]any{"head": "{{.Inputs.branch}}", "title": "Fixes from {{.Run.Automation}}", "body": "Looked at it.", "draft": true}},
		{Type: "notify", User: "admins", Message: "{{.Steps.pr.URL}} #{{.Steps.pr.Number}} {{.Steps.pr.State}}"},
	}})
	run := ae.manual(a, map[string]any{"branch": "review/x"})
	ae.work()
	r := ae.runStatus(run)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	created, _, _, _ := ae.sg.snapshot()
	if len(created) != 1 || created[0].Head != "review/x" || created[0].Base != "main" || created[0].Title != "Fixes from Open" || !created[0].Draft ||
		!strings.HasPrefix(created[0].Body, "Looked at it.") || !strings.Contains(created[0].Body, "“Open” automation") {
		t.Fatalf("opened = %+v", created)
	}
	st := stepByID(r, "pr")
	pr, err := ae.s.prByNumber(ae.ctx, ae.s.db.R, ae.f.ID, "me/app", 101)
	if err != nil || pr == nil || pr.HeadRef != "review/x" || pr.State != "draft" {
		t.Fatalf("recorded pull request = %+v %v", pr, err)
	}
	if st.Outputs["Number"] != 101.0 || st.Outputs["ID"] != float64(pr.ID) || st.Outputs["Head"] != "review/x" || st.Outputs["State"] != "draft" || st.Outputs["Existing"] != false {
		t.Errorf("outputs = %+v", st.Outputs)
	}
	// Again from the same branch: the pull request that is open is used.
	run2 := ae.manual(a, map[string]any{"branch": "review/x"})
	ae.work()
	st2 := stepByID(ae.runStatus(run2), "pr")
	if created, _, _, _ = ae.sg.snapshot(); len(created) != 1 || st2.Outputs["Existing"] != true || st2.Outputs["Number"] != 101.0 {
		t.Errorf("second run opened another: %d, outputs %+v", len(created), st2.Outputs)
	}
	// No branch, no pull request.
	run3 := ae.manual(a, map[string]any{"branch": " "})
	ae.work()
	if r3 := ae.runStatus(run3); r3.Status != RunFailed || !strings.Contains(r3.Error, "branch") {
		t.Errorf("empty branch: %s %q", r3.Status, r3.Error)
	}
}

func TestWaitForCIPresets(t *testing.T) {
	ae := newAgentEnv(t)
	pr := ae.openPR5()
	wait := func(what string, extra map[string]any) Rule {
		x := map[string]any{"what": what, "timeout": "2h", "pr": "{{.Inputs.pr}}"}
		for k, v := range extra {
			x[k] = v
		}
		return prRule(Action{Type: "wait_for", ID: "ci", Extra: x}, Action{Type: "notify", User: "admins", Message: "done"})
	}
	a := ae.addAuto(ae.p, "CI", wait(waitCI, nil))
	run := ae.manual(a, map[string]any{"pr": "5"})
	ae.work()
	if r := ae.runStatus(run); r.Status != RunWaiting {
		t.Fatalf("with CI pending: %s (%s)", r.Status, r.Error)
	}
	// The checks fail: the run wakes with the names.
	ae.sg.logs[ciBackend] = "FAIL: TestX"
	ae.sg.setChecks(pr.HeadSHA, CommitCheck{Name: ciBackend, State: CheckFailed, URL: "https://x/b"}, CommitCheck{Name: ciWeb, State: CheckSuccess})
	ae.poll()
	ae.work()
	r := ae.runStatus(run)
	st := stepByID(r, "ci")
	if r.Status != RunSucceeded || st.Outputs["Passed"] != false || st.Outputs["CIState"] != CIFailed || !slices.Equal(anyStrings(st.Outputs["Failed"]), []string{ciBackend}) {
		t.Fatalf("failed CI: %s %q, %+v", r.Status, r.Error, st.Outputs)
	}

	// CI that passed before the wait began counts at once.
	ae.sg.setChecks(pr.HeadSHA, CommitCheck{Name: ciBackend, State: CheckSuccess}, CommitCheck{Name: ciWeb, State: CheckSuccess})
	ae.poll()
	ae.work()
	run2 := ae.manual(a, map[string]any{"pr": "5"})
	ae.work()
	st = stepByID(ae.runStatus(run2), "ci")
	if got := ae.runStatus(run2); got.Status != RunSucceeded || st.Outputs["Passed"] != true || st.Outputs["CIState"] != CIPassed {
		t.Fatalf("passed CI: %s %q %+v", got.Status, got.Error, st.Outputs)
	}

	// "CI passes" fails the step when it fails.
	pr2 := ForgePR{Number: 6, Title: "Docs", Author: "sam", HeadRef: "docs", HeadSHA: "2222222bbbb", State: "open"}
	ae.sg.setChecks("2222222bbbb", CommitCheck{Name: ciBackend, State: CheckFailed})
	ae.pullHook("opened", pr2)
	ae.s.Wait()
	ae.poll()
	b := ae.addAuto(ae.p, "Strict CI", wait(waitCIPassed, nil))
	run3 := ae.manual(b, map[string]any{"pr": "6"})
	ae.work()
	if got := ae.runStatus(run3); got.Status != RunFailed || !strings.Contains(got.Error, "CI failed: "+ciBackend) {
		t.Errorf("ci_passed on a failing PR: %s %q", got.Status, got.Error)
	}

	// Timing out fails the step, or carries on.
	pr3 := ForgePR{Number: 7, Title: "Slow", Author: "sam", HeadRef: "slow", HeadSHA: "3333333cccc", State: "open"}
	ae.sg.setChecks("3333333cccc", CommitCheck{Name: ciBackend, State: CheckPending})
	ae.pullHook("opened", pr3)
	ae.s.Wait()
	c := ae.addAuto(ae.p, "Patient", wait(waitCI, map[string]any{"on_timeout": "continue", "timeout": "30m"}))
	run4 := ae.manual(c, map[string]any{"pr": "7"})
	ae.work()
	if got := ae.runStatus(run4); got.Status != RunWaiting {
		t.Fatalf("slow CI: %s", got.Status)
	}
	ae.setClock(ae.clockNow().Add(time.Hour))
	ae.work()
	got := ae.runStatus(run4)
	if got.Status != RunSucceeded || stepByID(got, "ci").Outputs["TimedOut"] != true {
		t.Errorf("after the timeout: %s %q %+v", got.Status, got.Error, stepByID(got, "ci").Outputs)
	}
	d := ae.addAuto(ae.p, "Impatient", wait(waitCI, map[string]any{"timeout": "30m"}))
	run5 := ae.manual(d, map[string]any{"pr": "7"})
	ae.work()
	ae.setClock(ae.clockNow().Add(time.Hour))
	ae.work()
	if got := ae.runStatus(run5); got.Status != RunFailed || !strings.Contains(got.Error, "timed out after 30m0s waiting for ci finishes") {
		t.Errorf("failing timeout: %s %q", got.Status, got.Error)
	}

	// A repository without CI never says so: after a few minutes without checks, that is a pass.
	pr4 := ForgePR{Number: 8, Title: "No CI", Author: "sam", HeadRef: "nociy", HeadSHA: "4444444dddd", State: "open"}
	ae.pullHook("opened", pr4)
	ae.s.Wait()
	e := ae.addAuto(ae.p, "No CI", wait(waitCI, nil))
	run6 := ae.manual(e, map[string]any{"pr": "8"})
	ae.work()
	if got := ae.runStatus(run6); got.Status != RunWaiting {
		t.Fatalf("no checks yet: %s", got.Status)
	}
	ae.setClock(ae.clockNow().Add(7 * time.Minute))
	ae.work()
	got = ae.runStatus(run6)
	if st := stepByID(got, "ci"); got.Status != RunSucceeded || st.Outputs["Passed"] != true || st.Outputs["CIState"] != CINone {
		t.Errorf("repository without CI: %s %q %+v", got.Status, got.Error, st.Outputs)
	}
}

func TestWaitForGateAndMerged(t *testing.T) {
	ae := newAgentEnv(t)
	ae.gate("P2")
	ae.pullHook("opened", testPR)
	ae.s.Wait()
	gate := ae.addAuto(ae.p, "Gate", prRule(Action{Type: "wait_for", ID: "g", Extra: map[string]any{"what": waitGate, "timeout": "6h", "pr": "{{.Inputs.pr}}"}}))
	run := ae.manual(gate, map[string]any{"pr": "5"})
	ae.work()
	// The review is queued, so the gate isn't green; the run waits.
	if got := ae.runStatus(run); got.Status != RunWaiting {
		t.Fatalf("gate before the review: %s %q", got.Status, got.Error)
	}
	ae.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	ae.drain()
	ae.work()
	got := ae.runStatus(run)
	if got.Status != RunSucceeded || stepByID(got, "g").Outputs["Green"] != true {
		t.Fatalf("gate after the review: %s %q %+v", got.Status, got.Error, stepByID(got, "g").Outputs)
	}
	// Already green: no waiting.
	run2 := ae.manual(gate, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run2); got.Status != RunSucceeded {
		t.Errorf("green gate: %s", got.Status)
	}

	// Waiting for the merge.
	merged := ae.addAuto(ae.p, "Merged", prRule(Action{Type: "wait_for", ID: "m", Extra: map[string]any{"what": waitMerged, "timeout": "6h", "pr": "{{.Inputs.pr}}"}}))
	run3 := ae.manual(merged, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run3); got.Status != RunWaiting {
		t.Fatalf("not merged yet: %s", got.Status)
	}
	next := testPR
	next.State = "merged"
	ae.pullHook("closed", next)
	ae.s.Wait()
	ae.work()
	if got := ae.runStatus(run3); got.Status != RunSucceeded || stepByID(got, "m").Outputs["Merged"] != true {
		t.Errorf("after the merge: %s %q", got.Status, got.Error)
	}

	// A repository with no gate has nothing to wait for.
	off := false
	if err := ae.s.SetRepoGate(ae.ctx, ae.admin, ae.p.Key, ae.repo.ID, &off, nil); err != nil {
		t.Fatal(err)
	}
	run4 := ae.manual(gate, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run4); got.Status != RunSucceeded {
		t.Errorf("gate off: %s %q", got.Status, got.Error)
	}
}

func TestMergePRStep(t *testing.T) {
	ae := newAgentEnv(t)
	ae.gate("P2")
	ae.noWaits()
	ae.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	ae.pullHook("opened", testPR)
	ae.s.Wait()
	merge := ae.addAuto(ae.p, "Merge", prRule(
		Action{Type: "merge_pr", ID: "m", Extra: map[string]any{"when_ready": true, "style": MergeSquash, "timeout": "6h", "pr": "{{.Inputs.pr}}"}},
		Action{Type: "notify", User: "admins", Message: "merged={{.Steps.m.Merged}}"}))
	run := ae.manual(merge, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run); got.Status != RunWaiting {
		t.Fatalf("not ready: %s %q", got.Status, got.Error)
	}
	if pr := ae.pr(5); pr.ArmedAt == nil || pr.ArmedStyle != MergeSquash {
		t.Errorf("not armed: %+v", pr)
	}
	// The review passes and CI passes: Waypoint merges, the run finishes.
	ae.drain()
	ae.sg.setChecks(testPR.HeadSHA, CommitCheck{Name: ciBackend, State: CheckSuccess})
	ae.poll()
	ae.work()
	got := ae.runStatus(run)
	if got.Status != RunSucceeded || stepByID(got, "m").Outputs["Merged"] != true {
		t.Fatalf("after CI: %s %q %+v", got.Status, got.Error, stepByID(got, "m"))
	}
	if ms := ae.sg.merged(); len(ms) != 1 || ms[0].Style != MergeSquash || !ms[0].DeleteBranch || ms[0].HeadSHA != testPR.HeadSHA {
		t.Errorf("merges = %+v", ms)
	}
	page, err := ae.s.ListAudit(ae.ctx, ae.admin, AuditQuery{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	var merged bool
	for _, en := range page.Entries {
		merged = merged || (en.Action == "pr.merge" && strings.Contains(en.Detail, "through the “Merge” automation"))
	}
	if !merged {
		t.Errorf("the merge isn't in the audit log as the automation's: %+v", page.Entries)
	}
	// Merging again finds it merged.
	run2 := ae.manual(merge, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run2); got.Status != RunSucceeded || stepByID(got, "m").Outputs["Merged"] != true {
		t.Errorf("already merged: %s %q", got.Status, got.Error)
	}
}

func TestMergePRStepNowAndTimeout(t *testing.T) {
	ae := newAgentEnv(t)
	ae.sg.setChecks(testPR.HeadSHA, CommitCheck{Name: ciBackend, State: CheckPending})
	ae.pullHook("opened", testPR) // CI is still running
	ae.s.Wait()
	now := ae.addAuto(ae.p, "Now", prRule(Action{Type: "merge_pr", Extra: map[string]any{"when_ready": false, "pr": "{{.Inputs.pr}}"}}))
	run := ae.manual(now, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run); got.Status != RunFailed || !strings.Contains(got.Error, "not ready to merge") {
		t.Errorf("merge now while CI runs: %s %q", got.Status, got.Error)
	}
	// Waiting out the time: the arming is taken off and the step says why.
	late := ae.addAuto(ae.p, "Late", prRule(Action{Type: "merge_pr", ID: "m", Extra: map[string]any{"when_ready": true, "timeout": "1h", "pr": "{{.Inputs.pr}}"}}))
	run2 := ae.manual(late, map[string]any{"pr": "5"})
	ae.work()
	ae.setClock(ae.clockNow().Add(2 * time.Hour))
	ae.work()
	got := ae.runStatus(run2)
	if got.Status != RunFailed || !strings.Contains(got.Error, "not merged after 1h0m0s") || !strings.Contains(got.Error, "check") {
		t.Errorf("timeout: %s %q", got.Status, got.Error)
	}
	if pr := ae.pr(5); pr.ArmedAt != nil {
		t.Errorf("still armed after giving up: %+v", pr)
	}
	// A pull request of another repository isn't this project's to merge.
	other := ae.addAuto(ae.p, "Elsewhere", prRule(Action{Type: "merge_pr", Extra: map[string]any{"pr": "other/repo#5"}}))
	run3 := ae.manual(other, map[string]any{"pr": ""})
	ae.work()
	if got := ae.runStatus(run3); got.Status != RunFailed {
		t.Errorf("another repository: %s", got.Status)
	}
}

func TestLabelReviewerStatusAndRetrySteps(t *testing.T) {
	ae := newAgentEnv(t)
	ae.openPR5()
	a := ae.addAuto(ae.p, "Tidy", prRule(
		Action{Type: "pr_label", ID: "lab", Extra: map[string]any{"add": []any{"automerge", "from-{{.Run.Automation}}"}, "remove": []any{"wip"}, "pr": "{{.Inputs.pr}}"}},
		Action{Type: "request_reviewers", Extra: map[string]any{"users": []any{"ann", "bo"}, "pr": "{{.Inputs.pr}}"}},
		Action{Type: "set_commit_status", Extra: map[string]any{"context": "waypoint/lint", "state": "failure", "description": "3 problems", "pr": "{{.Inputs.pr}}"}},
		Action{Type: "retry_ci", ID: "retry", Extra: map[string]any{"pr": "{{.Inputs.pr}}"}},
	))
	run := ae.manual(a, map[string]any{"pr": "5"})
	ae.work()
	r := ae.runStatus(run)
	if r.Status != RunSucceeded {
		t.Fatalf("status = %s (%s)", r.Status, r.Error)
	}
	_, ops, reviewers, retries := ae.sg.snapshot()
	if !slices.Equal(ops, []string{"+automerge", "+from-Tidy", "-wip"}) || len(reviewers) != 1 || !slices.Equal(reviewers[0], []string{"ann", "bo"}) || !slices.Equal(retries, []string{testPR.HeadSHA}) {
		t.Errorf("ops %v reviewers %v retries %v", ops, reviewers, retries)
	}
	if st := ae.sg.lastStatus(testPR.HeadSHA); st.Context != "waypoint/lint" || st.State != "failure" || st.Description != "3 problems" {
		t.Errorf("status = %+v", st)
	}
	if labels := stepByID(r, "lab").Outputs["Labels"]; !slices.Equal(anyStrings(labels), []string{"automerge", "from-Tidy"}) {
		t.Errorf("labels = %v", labels)
	}
	// The review gate's own status is reserved.
	b := ae.addAuto(ae.p, "Spoof", prRule(Action{Type: "set_commit_status", Extra: map[string]any{"context": gateContext, "state": "success", "pr": "{{.Inputs.pr}}"}}))
	run2 := ae.manual(b, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run2); got.Status != RunFailed || !strings.Contains(got.Error, "review gate") {
		t.Errorf("spoofing the gate: %s %q", got.Status, got.Error)
	}
	// A step with a pull request template that names nothing, and no trigger PR, says so.
	c := ae.addAuto(ae.p, "None", prRule(Action{Type: "pr_label", Extra: map[string]any{"add": []any{"x"}}}))
	run3 := ae.manual(c, map[string]any{"pr": ""})
	ae.work()
	if got := ae.runStatus(run3); got.Status != RunFailed || !strings.Contains(got.Error, "needs a pull request") {
		t.Errorf("no pull request: %s %q", got.Status, got.Error)
	}
}

func TestFixCIStep(t *testing.T) {
	ae := newAgentEnv(t)
	pr := ae.openPR5()
	fix := ae.addAuto(ae.p, "Fix", prRule(Action{Type: "fix_ci", ID: "fix", Extra: map[string]any{"agents": museChain("claude sonnet"), "pr": "{{.Inputs.pr}}"}}))
	// CI isn't failing: nothing to fix, no job.
	ae.sg.setChecks(pr.HeadSHA, CommitCheck{Name: ciBackend, State: CheckSuccess})
	ae.poll()
	run := ae.manual(fix, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run); got.Status != RunSucceeded || stepByID(got, "fix").Outputs["Skipped"] != true {
		t.Fatalf("with CI green: %s %q", got.Status, got.Error)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a job for passing CI: %+v", w.Job)
	}

	// It fails: the agent gets the logs and the branch, may run commands, and may not touch the pipeline.
	next := testPR
	next.HeadSHA = "5555555eeee"
	ae.sg.logs[ciBackend] = "--- FAIL: TestLogin\n    login_test.go:12: want 200, got 500\n"
	ae.sg.setChecks(next.HeadSHA, CommitCheck{Name: ciBackend, State: CheckFailed, URL: "https://x/b"})
	ae.pullHook("synchronize", next)
	ae.s.Wait()
	ae.poll()
	run2 := ae.manual(fix, map[string]any{"pr": "5"})
	ae.work()
	claim := ae.take()
	in := agentJobOf(t, claim)
	if in.CLI != "claude" || in.Permission != AgentEditRun || in.Base != "fix-login" || in.Branch != "fix-login" || !in.Push || in.Repo != "https://git.example.com/me/app.git" {
		t.Fatalf("job = %+v", in)
	}
	if act, err := ae.s.TokenActor(ae.ctx, claim.Token); err != nil || act.scope[ae.p.ID] != RoleEditor || len(act.scope) != 1 {
		t.Errorf("the job's token reaches %v (%v), want only the repo's project %d", act, err, ae.p.ID)
	}
	for _, want := range []string{"pull request #5", ciBackend, "want 200, got 500", "<untrusted>", "Do not change CI configuration", "https://x/b"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	for _, want := range []string{".github/**", ".forgejo/**", "**/package-lock.json", "**/go.sum", "**/.env"} {
		if !slices.Contains(in.ProtectPaths, want) {
			t.Errorf("%s isn't protected: %v", want, in.ProtectPaths)
		}
	}
	// It changed the pipeline: refused, and the run says why.
	ae.finish(claim.ID, "done", map[string]any{"answer": "Fixed", "commits": []string{"c1"}, "commit": "c1", "pushed": true, "files": []string{"login.go", ".forgejo/workflows/ci.yml"}}, "")
	ae.work()
	if got := ae.runStatus(run2); got.Status != RunFailed || !strings.Contains(got.Error, "protected files (.forgejo/workflows/ci.yml)") {
		t.Fatalf("a commit touching the pipeline: %s %q", got.Status, got.Error)
	}
	// A real fix pushes: the pull request has a new head, whose checks are followed.
	run3 := ae.manual(fix, map[string]any{"pr": "5"})
	ae.work()
	claim = ae.take()
	ae.sg.pushTo(ae.f, "me/app", 5, "6666666ffff")
	ae.sg.setChecks("6666666ffff", CommitCheck{Name: ciBackend, State: CheckSuccess})
	ae.finish(claim.ID, "done", map[string]any{"answer": "The handler returned 500.", "commits": []string{"c2"}, "commit": "c2", "pushed": true, "files": []string{"login.go"}}, "")
	ae.work()
	got := ae.runStatus(run3)
	st := stepByID(got, "fix")
	if got.Status != RunSucceeded || st.Outputs["Fixed"] != true || st.Outputs["HeadSHA"] != "6666666ffff" || st.Outputs["Answer"] != "The handler returned 500." {
		t.Fatalf("a fix: %s %q %+v", got.Status, got.Error, st.Outputs)
	}
	if cur := ae.pr(5); cur.HeadSHA != "6666666ffff" {
		t.Errorf("the pull request's head is still %s", cur.HeadSHA)
	}
	// It failed on its runner: no code fixes that, so no agent (WP-230).
	ae.sg.logs[ciBackend] = enospcLog
	ae.sg.setChecks("6666666ffff", CommitCheck{Name: ciBackend, State: CheckFailed, URL: "https://x/c"})
	ae.poll()
	run4 := ae.manual(fix, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run4); got.Status != RunSucceeded || stepByID(got, "fix").Outputs["Skipped"] != true {
		t.Fatalf("a runner fault: %s %q", got.Status, got.Error)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a job for a runner fault: %+v", w.Job)
	}
	// Every failure was a cancellation: a newer run replaces it, nothing to fix (WP-354).
	delete(ae.sg.logs, ciBackend)
	cancelledHead := testPR
	cancelledHead.HeadSHA = "7777777gggg"
	ae.sg.setChecks(cancelledHead.HeadSHA, CommitCheck{Name: ciBackend, State: CheckFailed, Description: "Has been cancelled", URL: "https://x/d"})
	ae.pullHook("synchronize", cancelledHead)
	ae.s.Wait()
	ae.poll()
	run5 := ae.manual(fix, map[string]any{"pr": "5"})
	ae.work()
	if got := ae.runStatus(run5); got.Status != RunSucceeded || stepByID(got, "fix").Outputs["Skipped"] != true {
		t.Fatalf("cancelled only: %s %q", got.Status, got.Error)
	}
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatalf("a job for a cancelled run: %+v", w.Job)
	}
}
