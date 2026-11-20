package core

import "testing"

func TestAutopilotActivityCountsByDay(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.stopWith("Shipped the first one.")
	act, err := a.s.AutopilotActivity(a.ctx, a.admin, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(act.Days) != 7 {
		t.Fatalf("days = %d", len(act.Days))
	}
	last := act.Days[6]
	if last.Finished != 1 || last.Day == "" {
		t.Errorf("today = %+v", last)
	}
	for _, d := range act.Days[:6] {
		if d.Finished != 0 || d.Started != 0 {
			t.Errorf("an earlier day has counts: %+v", d)
		}
	}
	// Days shift with the viewer's zone and out-of-range asks fall back.
	if act, err = a.s.AutopilotActivity(a.ctx, a.admin, 999, 99999); err != nil || len(act.Days) != 14 {
		t.Fatalf("defaults: %v %v", err, act)
	}
}
