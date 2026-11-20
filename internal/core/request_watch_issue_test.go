package core

import (
	"strings"
	"testing"
)

func TestRequestClosesWhenIssueDone(t *testing.T) {
	be := newBabysitEnv(t)
	is := be.issue(be.admin, IssueInput{Project: be.p.Key, Title: "Clients list their own fallbacks"})
	r := be.ask(RequestInput{Kind: RequestAction, Title: "Mark it done once shipped", ClosesWhen: []WatchInput{{Kind: WatchIssueDone, Target: is.Key}}})
	be.s.CheckRequestWatches(be.ctx)
	if got := be.request(r.ID); got.State != RequestOpen || !strings.Contains(got.Watches[0].Note, is.Key) {
		t.Fatalf("before: %s %+v", got.State, got.Watches)
	}
	done := "Done"
	if _, err := be.s.UpdateIssue(be.ctx, be.admin, is.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait() // the status change nudges the check
	if got := be.request(r.ID); got.State != RequestDone || !strings.Contains(got.Note, "Done") {
		t.Fatalf("after: %s %q", got.State, got.Note)
	}
}
