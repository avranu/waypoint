package core

import "testing"

// A babysit never runs on Fable or Mythos: they are for planning.
func TestBabysitRefusesPlanningModels(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	for _, m := range []string{"fable", "claude-fable-5-2", "claude-mythos-5-2", "Claude-FABLE-5"} {
		if _, err := be.s.StartBabysit(be.ctx, be.admin, pr.ID, BabysitInput{Model: m}); !IsInvalid(err) {
			t.Errorf("model %q: %v, want invalid", m, err)
		}
	}
	for _, m := range []string{"opus", "sonnet", "haiku", "claude-sonnet-5-5"} {
		if _, err := be.s.StartBabysit(be.ctx, be.admin, pr.ID, BabysitInput{Model: m}); err != nil {
			t.Errorf("model %q refused: %v", m, err)
		}
	}
}

// A babysit stored with a planning model (from before the refusal) runs its
// rounds on sonnet.
func TestBabysitStoredFableRunsOnSonnet(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	if _, err := be.s.db.W.Exec(`UPDATE babysits SET model = 'claude-fable-5-2' WHERE pr_id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	if in := be.jobInput(jobs[0]); in.Model != babysitModel {
		t.Errorf("round ran on %q, want %q", in.Model, babysitModel)
	}
}

// A babysit stored with a planning model resumes on the default model
// instead of having its fresh start refused.
func TestResumeBabysitStoredFable(t *testing.T) {
	be, pr := armedBabysit(t)
	be.setBabysitState(BabysitGaveUp, "5 rounds and still 1 review comment", true)
	if _, err := be.s.db.W.Exec(`UPDATE babysits SET model = 'claude-fable-5-2' WHERE pr_id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.ResumeBabysits(be.ctx)
	if b := be.babysit(); !b.Active || b.Via != viaResume || b.Model != babysitModel || be.babysitCount() != 2 {
		t.Fatalf("after a resume: %+v", b)
	}
}
