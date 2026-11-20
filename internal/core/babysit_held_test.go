package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const protHeldCI = ".forgejo/workflows/ci.yml"

// heldEnv: pull request 7 changes the CI workflow, CI failed and the round's
// job is queued.
func protHeldEnv(t *testing.T) (*bsEnv, *PullRequest, int64) {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	pr := be.open()
	be.sg.mu.Lock()
	be.sg.diffs["7777777aaaa"] = ciDiff
	be.sg.mu.Unlock()
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET files_sha = '' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	return be, pr, jobs[0]
}

// finishHeld ends a round's job as a runner that held a change would.
func (be *bsEnv) finishHeld(id int64, file string, all bool) {
	be.t.Helper()
	res := map[string]any{"pushed": true, "commits": []string{"abc"}, "commit": "abc1234", "answer": "Done.", "diff": "+x\n",
		"held_patch": "diff --git a/" + file + " b/" + file + "\n--- a/" + file + "\n+++ b/" + file + "\n@@ -1 +1 @@\n-run: make\n+run: make check\n", "held_files": []string{file}}
	if all {
		res["held_all"], res["pushed"] = true, false
	}
	raw, _ := json.Marshal(res)
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "done", nil, raw, "") }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

func (be *bsEnv) heldJobs() []int64 {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT id FROM runner_jobs WHERE source = ? ORDER BY id`, babysitHeldSource)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (be *bsEnv) heldRequests() int {
	be.t.Helper()
	var n int
	if err := be.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%").Scan(&n); err != nil {
		be.t.Fatal(err)
	}
	return n
}

func (be *bsEnv) heldOf() *HeldEdit {
	b := be.babysit()
	for i := len(b.Rounds) - 1; i >= 0; i-- {
		if b.Rounds[i].Held != nil {
			return b.Rounds[i].Held
		}
	}
	return nil
}

// A held change to a file the pull request changes, that no other model
// says could weaken CI, is pushed by a job that runs no agent, with no
// request and no handoff task.
func TestHeldChangeLandsWhenCheckedClean(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": false, "reason": "A cleaned-up command."}`
	be.finishHeld(job, protHeldCI, false)
	h := be.heldOf()
	if h == nil || h.State != heldApplying || h.Job <= 0 {
		t.Fatalf("held %+v", h)
	}
	if be.heldRequests() != 0 || len(be.protectedTasks()) != 0 {
		t.Errorf("asked a person or filed a task: %d %v", be.heldRequests(), be.protectedTasks())
	}
	jobs := be.heldJobs()
	if len(jobs) != 1 {
		t.Fatalf("held jobs %v", jobs)
	}
	in := be.jobInput(jobs[0])
	if in.Patch == "" || !in.Push || in.Branch != "add-export" || in.Base != in.Branch {
		t.Errorf("patch job %+v", in)
	}
	if !strings.Contains(be.babysit().Wait, "is being pushed") {
		t.Errorf("wait %q", be.babysit().Wait)
	}
	// The job ends: applied, and the babysit goes on.
	res, _ := json.Marshal(map[string]any{"pushed": true})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, jobs[0], "done", nil, res, "") }); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
	if h := be.heldOf(); h.State != heldApplied {
		t.Errorf("after the job: %+v", h)
	}
}

// A change the check says could weaken CI goes to one request, never a task;
// "Push it" applies it and "Leave it out" doesn't.
func TestHeldChangeWeakeningAsksOnePerson(t *testing.T) {
	for _, push := range []bool{true, false} {
		be, _, job := protHeldEnv(t)
		be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It skips the tests."}`
		be.finishHeld(job, protHeldCI, false)
		h := be.heldOf()
		if h == nil || h.State != heldAsked || !strings.Contains(h.Why, "skips the tests") {
			t.Fatalf("held %+v", h)
		}
		if be.heldRequests() != 1 || len(be.heldJobs()) != 0 || len(be.protectedTasks()) != 0 {
			t.Fatalf("requests %d jobs %v tasks %v", be.heldRequests(), be.heldJobs(), be.protectedTasks())
		}
		if b := be.babysit(); b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, "waits for your approval") {
			t.Errorf("babysit %+v", b)
		}
		be.step()
		if be.heldRequests() != 1 {
			t.Errorf("asked again: %d", be.heldRequests())
		}
		var id int64
		if err := be.s.db.R.QueryRow(`SELECT id FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%").Scan(&id); err != nil {
			t.Fatal(err)
		}
		q := be.request(id)
		text := heldLeave
		if push {
			text = heldPush
		}
		if _, err := be.s.DecideRequest(be.ctx, be.admin, q.Ref, RequestDecision{Decision: "answer", Text: text}); err != nil {
			t.Fatal(err)
		}
		be.s.Wait()
		be.s.Wait()
		h = be.heldOf()
		if push && (h.State != heldApplying || len(be.heldJobs()) != 1) {
			t.Errorf("push: %+v jobs %v", h, be.heldJobs())
		}
		if !push && (h.State != heldDeclined || len(be.heldJobs()) != 0) {
			t.Errorf("leave: %+v jobs %v", h, be.heldJobs())
		}
	}
}

// A file the pull request doesn't change goes to a person without asking a
// model.
func TestHeldChangeToAFileItDoesNotChangeAsks(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": false, "reason": "fine"}`
	be.finishHeld(job, ".github/scripts/other.py", false)
	h := be.heldOf()
	if h == nil || h.State != heldAsked || !strings.Contains(h.Why, "doesn't change") {
		t.Fatalf("held %+v", h)
	}
	if be.heldRequests() != 1 || len(be.heldJobs()) != 0 {
		t.Errorf("requests %d jobs %v", be.heldRequests(), be.heldJobs())
	}
}

// A conflict settled with a line of the agent's own pushes nothing: the whole
// change is held and the request says so.
func TestHeldAllChangeSaysNothingWasPushed(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "new line"}`
	be.finishHeld(job, protHeldCI, true)
	h := be.heldOf()
	if h == nil || !h.All || h.State != heldAsked {
		t.Fatalf("held %+v", h)
	}
	var body string
	if err := be.s.db.R.QueryRow(`SELECT body FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%").Scan(&body); err != nil || !strings.Contains(body, "nothing else of the round was pushed") {
		t.Errorf("body %q %v", body, err)
	}
}

// A conflict settled by taking the branch's side whole doesn't show in the
// patch from the branch: the check and the person see how each conflict was
// settled, against the merge as git left it (audit #1091.1).
func TestHeldAllChangeShowsHowConflictsWereSettled(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "drops main's step"}`
	merged := "diff --git a/" + protHeldCI + " b/" + protHeldCI + "\n--- a/" + protHeldCI + "\n+++ b/" + protHeldCI + "\n@@ -1,5 +1 @@\n-<<<<<<< ours\n   run: make\n-=======\n-  run: make security-scan\n->>>>>>> main\n"
	raw, _ := json.Marshal(map[string]any{"pushed": false, "commits": []string{"abc"}, "answer": "Done.",
		"held_patch": "diff --git a/work.txt b/work.txt\n--- a/work.txt\n+++ b/work.txt\n@@ -1 +1 @@\n-mine\n+both\n",
		"held_files": []string{protHeldCI}, "held_all": true, "held_merge": merged})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, job, "done", nil, raw, "") }); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
	be.llm.mu.Lock()
	var seen bool
	for _, p := range be.llm.prompts {
		seen = seen || strings.Contains(p, "-  run: make security-scan")
	}
	be.llm.mu.Unlock()
	if !seen {
		t.Error("the check didn't see the dropped side")
	}
	if h := be.heldOf(); h == nil || h.State != heldAsked || h.Merge == "" {
		t.Fatalf("held %+v", h)
	}
	var body string
	if err := be.s.db.R.QueryRow(`SELECT body FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%").Scan(&body); err != nil || !strings.Contains(body, "make security-scan") {
		t.Errorf("body %q %v", body, err)
	}
}

// finishHeldPatch is finishHeld with the patch given.
func (be *bsEnv) finishHeldPatch(id int64, file, patch string) {
	be.t.Helper()
	raw, _ := json.Marshal(map[string]any{"pushed": true, "commits": []string{"abc"}, "commit": "abc1234", "answer": "Done.", "diff": "+x\n",
		"held_patch": patch, "held_files": []string{file}})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "done", nil, raw, "") }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// bigHeldPatch: a long harmless hunk, then one that skips the tests.
func bigHeldPatch(file string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + file + " b/" + file + "\n--- a/" + file + "\n+++ b/" + file + "\n@@ -1,1 +1,2000 @@\n")
	for i := 0; b.Len() < 70<<10; i++ {
		fmt.Fprintf(&b, "+# a harmless comment line %d\n", i)
	}
	b.WriteString("@@ -500,1 +2500,1 @@\n-        run: make test\n+        run: make test || true\n")
	return b.String()
}

// The model judges the whole held patch, not its start: what it lets through
// is what is pushed.
func TestHeldChangeCheckSeesTheWholePatch(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It ignores test failures."}`
	be.finishHeldPatch(job, protHeldCI, bigHeldPatch(protHeldCI))
	be.llm.mu.Lock()
	var seen bool
	for _, p := range be.llm.prompts {
		seen = seen || strings.Contains(p, "run: make test || true")
	}
	be.llm.mu.Unlock()
	if !seen {
		t.Error("the check didn't see the hunk after the first 60 KB")
	}
	// The person's request can't carry 70 KB: it says what it leaves out
	// rather than claiming to show what is pushed.
	if h := be.heldOf(); h == nil || h.State != heldAsked {
		t.Fatalf("held %+v", h)
	}
	var body string
	if err := be.s.db.R.QueryRow(`SELECT body FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "isn't shown here") || !strings.Contains(body, protHeldCI) || strings.Contains(body, "exactly this") {
		t.Errorf("body:\n%s", excerpt(body, 600))
	}
	if len(body) > maxRequestBody {
		t.Errorf("body is %d long", len(body))
	}
}

// With no one who could answer a request, the held change isn't left
// "asked": it fails, and the babysit isn't left waiting for an approval no
// one was asked for.
func TestHeldChangeOwnerGoneFails(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It skips the tests."}`
	if _, err := be.s.db.W.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, be.babysit().ownerID); err != nil {
		t.Fatal(err)
	}
	be.finishHeld(job, protHeldCI, false)
	h := be.heldOf()
	if h == nil || h.State != heldFailed || be.heldRequests() != 0 {
		t.Fatalf("held %+v, requests %d", h, be.heldRequests())
	}
	if b := be.babysit(); strings.Contains(b.Wait, "waits for your approval") {
		t.Errorf("wait %q", b.Wait)
	}
}

// A held change whose request wasn't filed (the filing failed) is asked
// again at the next step.
func TestHeldChangeUnaskedIsAskedAgain(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It skips the tests."}`
	be.finishHeld(job, protHeldCI, false)
	if _, err := be.s.db.W.Exec(`DELETE FROM requests WHERE tool_use_id LIKE ?`, heldRequestKey+"%"); err != nil {
		t.Fatal(err)
	}
	b := be.babysit()
	b.Rounds[len(b.Rounds)-1].Held.State, b.Rounds[len(b.Rounds)-1].Held.Request = heldUnasked, 0
	be.s.saveRounds(be.ctx, b)
	be.step()
	if h := be.heldOf(); h.State != heldAsked || be.heldRequests() != 1 {
		t.Fatalf("held %+v, requests %d", h, be.heldRequests())
	}
	if b := be.babysit(); !strings.Contains(b.Wait, "waits for your approval") {
		t.Errorf("wait %q", b.Wait)
	}
}

// A job that applied a held change and finished while its event was lost
// (a restart) is read at the next step: the change isn't left "being
// pushed".
func TestHeldChangeApplyingRecoversAfterALostEvent(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": false, "reason": "A cleaned-up command."}`
	be.finishHeld(job, protHeldCI, false)
	jobs := be.heldJobs()
	if len(jobs) != 1 || be.heldOf().State != heldApplying {
		t.Fatalf("held %+v jobs %v", be.heldOf(), jobs)
	}
	// Finished behind the service's back: no event.
	if _, err := be.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done' WHERE id = ?`, jobs[0]); err != nil {
		t.Fatal(err)
	}
	be.step()
	be.s.Wait()
	be.s.Wait()
	if h := be.heldOf(); h.State != heldApplied {
		t.Errorf("after a step: %+v", h)
	}
}

// A push job whose row is gone (pruned, or lost) fails the change, so the
// babysit doesn't wait on it for good (audit #1091.3).
func TestHeldChangeApplyingJobGoneFails(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": false, "reason": "A cleaned-up command."}`
	be.finishHeld(job, protHeldCI, false)
	jobs := be.heldJobs()
	if len(jobs) != 1 || be.heldOf().State != heldApplying {
		t.Fatalf("held %+v jobs %v", be.heldOf(), jobs)
	}
	if _, err := be.s.db.W.Exec(`DELETE FROM runner_jobs WHERE id = ?`, jobs[0]); err != nil {
		t.Fatal(err)
	}
	be.step()
	be.s.Wait()
	be.s.Wait()
	if h := be.heldOf(); h.State != heldFailed {
		t.Errorf("after a step: %+v", h)
	}
}

func TestRunnerJobPatchValidation(t *testing.T) {
	be, _, _ := protHeldEnv(t)
	for name, in := range map[string]AgentJob{
		"no push":    {Repo: "https://x/y.git", Base: "b", Branch: "b", Patch: "p"},
		"other base": {Repo: "https://x/y.git", Base: "main", Branch: "b", Push: true, Patch: "p"},
		"no branch":  {Repo: "https://x/y.git", Push: true, Patch: "p"},
		"too large":  {Repo: "https://x/y.git", Base: "b", Branch: "b", Push: true, Patch: strings.Repeat("x", 600<<10)},
	} {
		raw, _ := json.Marshal(in)
		if _, err := be.s.EnqueueRunnerJob(be.ctx, SystemActor(), RunnerJobInput{Kind: JobAgent, OwnerID: be.admin.User.ID, Input: raw, Source: babysitHeldSource, SourceID: 1, Timeout: 60}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok, _ := json.Marshal(AgentJob{Repo: "https://x/y.git", Base: "b", Branch: "b", Push: true, Patch: "p"})
	if _, err := be.s.EnqueueRunnerJob(be.ctx, SystemActor(), RunnerJobInput{Kind: JobAgent, OwnerID: be.admin.User.ID, Input: ok, Source: babysitHeldSource, SourceID: 1, Timeout: 60}); err != nil {
		t.Errorf("a valid patch job: %v", err)
	}
}

// heldRequestID is the id of the first request filed with a held change's key.
func (be *bsEnv) heldRequestID() int64 {
	be.t.Helper()
	var id int64
	if err := be.s.db.R.QueryRow(`SELECT id FROM requests WHERE tool_use_id LIKE ? ORDER BY id LIMIT 1`, heldRequestKey+"%").Scan(&id); err != nil {
		be.t.Fatal(err)
	}
	return id
}

// A check's answer without "weakens" is no answer: a person is asked, the
// change isn't pushed.
func TestHeldChangeAnswerWithoutVerdictAsks(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"reason": "Looks fine."}`
	be.finishHeld(job, protHeldCI, false)
	if h := be.heldOf(); h == nil || h.State != heldAsked || len(be.heldJobs()) != 0 {
		t.Fatalf("held %+v jobs %v", h, be.heldJobs())
	}
}

// An approved change that can't be pushed fails and says so: it isn't put
// back to wait on a request that was answered already.
func TestHeldChangeApprovedButUnpushableFails(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It skips the tests."}`
	be.finishHeld(job, protHeldCI, false)
	if _, err := be.s.db.W.Exec(`DELETE FROM runners`); err != nil {
		t.Fatal(err)
	}
	q := be.request(be.heldRequestID())
	if _, err := be.s.DecideRequest(be.ctx, be.admin, q.Ref, RequestDecision{Decision: "answer", Text: heldPush}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
	if h := be.heldOf(); h.State != heldFailed || len(be.heldJobs()) != 0 {
		t.Fatalf("held %+v jobs %v", h, be.heldJobs())
	}
	if b := be.babysit(); strings.Contains(b.Wait, "waits for your approval") {
		t.Errorf("wait %q", b.Wait)
	}
}

// A request someone else filed with a held change's key neither stops the
// real one being filed nor approves the change.
func TestHeldChangeIgnoresARequestItDidNotFile(t *testing.T) {
	be, _, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": true, "reason": "It skips the tests."}`
	b := be.babysit()
	key := heldRequestKey + fmt.Sprint(b.ID) + ":" + fmt.Sprint(len(b.Rounds))
	spoof, _, err := be.s.CreateRequest(be.ctx, be.agent, RequestInput{Kind: RequestQuestion, Title: "Push?",
		Questions: []Question{{Question: "Push?", Choices: []Choice{{Label: heldPush}, {Label: heldLeave}}}}, ToolUseID: key})
	if err != nil {
		t.Fatal(err)
	}
	be.finishHeld(job, protHeldCI, false)
	h := be.heldOf()
	if h == nil || h.State != heldAsked || h.Request == 0 || h.Request == spoof.ID || be.heldRequests() != 2 {
		t.Fatalf("held %+v, requests %d", h, be.heldRequests())
	}
	if _, err := be.s.DecideRequest(be.ctx, be.admin, spoof.Ref, RequestDecision{Decision: "answer", Text: heldPush}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
	if h := be.heldOf(); h.State != heldAsked || len(be.heldJobs()) != 0 {
		t.Fatalf("the other request approved it: %+v jobs %v", h, be.heldJobs())
	}
}

// Only Waypoint queues a patch job: a person (or their automation) asking
// for one is refused, whatever source it names.
func TestPatchJobOnlyFromWaypoint(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	raw, _ := json.Marshal(AgentJob{Repo: "https://example.test/me/app.git", Base: "x", Branch: "x", Push: true, Patch: "diff --git a/a b/a\n"})
	for _, src := range []string{"", babysitHeldSource} {
		if _, err := be.s.EnqueueRunnerJob(be.ctx, be.admin, RunnerJobInput{Kind: JobAgent, Input: raw, Source: src}); !errors.Is(err, ErrForbidden) {
			t.Errorf("source %q: %v", src, err)
		}
	}
	if _, err := be.s.EnqueueRunnerJob(be.ctx, SystemActor(), RunnerJobInput{Kind: JobAgent, Input: raw, OwnerID: be.admin.User.ID}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Waypoint without the held source: %v", err)
	}
	if _, err := be.s.EnqueueRunnerJob(be.ctx, SystemActor(), RunnerJobInput{Kind: JobAgent, Input: raw, OwnerID: be.admin.User.ID, Source: babysitHeldSource}); err != nil {
		t.Errorf("Waypoint's own: %v", err)
	}
}
