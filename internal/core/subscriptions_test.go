package core

import (
	"strings"
	"testing"
)

// TestSubscriptionOrder pins the order reactions run in after a commit (WP-ADR151
// section 1): stages in order, each stage's subscribers in order, then the batch
// subscribers. Moving or adding one is a visible change to this list.
func TestSubscriptionOrder(t *testing.T) {
	s := &Service{}
	plan := s.subscriptionPlan()
	var got []string
	for _, st := range plan.stages {
		var names []string
		for _, sub := range st.subs {
			names = append(names, sub.name)
		}
		got = append(got, st.name+": "+strings.Join(names, " "))
	}
	var batch []string
	for _, b := range plan.after {
		batch = append(batch, b.name)
	}
	got = append(got, "after: "+strings.Join(batch, " "))
	want := []string{
		"notices: deliverNotification",
		"live: hub prWait stackParentMerged",
		"notify: notifyFor",
		"react: repoPushed kitPush resumeSessions dropUserTranscripts refreshKit checkReviewHealth urgentFromLink addPermissionRule backfillEstimates autoEstimateAndTitle autoRelate followUpsByFiles autoScreen deliverWebhooks pushIssueState pushComment pushCommentChange runRepoHook prAudit babysitEvent parkAnswered heldAnswered crossFiled leftover workLog checkSettled reviewAudit fanout goalDrift mergeOrder objective",
		"after: afterDispatch automations",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got  %s\n want %s", i, got[i], want[i])
		}
	}
}
