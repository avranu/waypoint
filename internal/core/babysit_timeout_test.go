package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// timeOut ends a round's job as the runner does when its agent runs out of
// time (WP-1597): failed, with what the runner delivered of its commits.
func (be *bsEnv) timeOut(id int64, result map[string]any) {
	be.t.Helper()
	result["timed_out"] = true
	res, _ := json.Marshal(result)
	msg := "The claude timed out after 1h0m0s and was stopped. When stopped it had been 41m0s in `Bash wp testsum -- make check`."
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "failed", nil, res, msg) }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

func timeoutRound(t *testing.T) (*bsEnv, int64) {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.sg.setNotes(PRNote{ID: "601", Kind: "comment", Author: "carol", Body: "Export never closes the file.", At: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)})
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	b, jobs := be.babysit(), be.jobs()
	if b.State != BabysitWorking || len(jobs) != 1 {
		t.Fatalf("no round for the comment: %+v", b)
	}
	if in := be.jobInput(jobs[0]); !in.KeepOnTimeout || !strings.Contains(in.Prompt, "it pushes the commits you had made") {
		t.Fatalf("round 1 isn't told its commits outlast its time limit: %+v", in)
	}
	return be, jobs[0]
}

// A round that ran out of time having committed part of its work pushed that
// part: it is a round that pushed, and the next starts from its push, not
// from the head it started at.
func TestBabysitTimedOutRoundKeepsItsCommits(t *testing.T) {
	be, job := timeoutRound(t)
	be.timeOut(job, map[string]any{"pushed": true, "commits": []string{"abc", "def"}, "commit": "def5678", "answer": "", "diff": "+merged main\n",
		"uncommitted": []string{"export.go"}})
	b := be.babysit()
	r := b.Rounds[0]
	if !r.Pushed || r.PushSHA != "def5678" || r.Diff == "" {
		t.Fatalf("round 1 = %+v", r)
	}
	for _, want := range []string{"timed out after 1h0m0s", "41m0s in `Bash wp testsum", "The 2 commits it had made were pushed", "uncommitted changes to export.go were dropped"} {
		if !strings.Contains(r.Outcome, want) {
			t.Errorf("outcome lacks %q: %s", want, r.Outcome)
		}
	}
	// No second round on the head it started from: its push is on the way.
	if len(be.jobs()) != 1 || b.State != BabysitWatching {
		t.Fatalf("after the timeout: state %s, %d jobs, %s", b.State, len(be.jobs()), b.Wait)
	}
}

// A round that ran out of time with nothing committed hands the next round
// how it ended, so that one doesn't spend its hour the same way.
func TestBabysitTimedOutRoundTellsTheNext(t *testing.T) {
	be, job := timeoutRound(t)
	be.timeOut(job, map[string]any{"commits": []string{}, "answer": ""})
	b := be.babysit()
	if r := b.Rounds[0]; r.Pushed || !strings.Contains(r.Outcome, "It had committed nothing.") {
		t.Fatalf("round 1 = %+v", r)
	}
	if len(be.jobs()) != 2 || b.Rounds[1].Model != escalateModel {
		t.Fatalf("no stronger round after the timeout: %+v", b)
	}
	in := be.jobInput(b.JobID)
	for _, want := range []string{"Round 1 left this work open", "How round 1 ended:", "timed out after 1h0m0s", "41m0s in `Bash wp testsum"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("round 2 prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
}
