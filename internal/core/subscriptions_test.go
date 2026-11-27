package core

import (
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// TestSubscriptionOrder pins the order reactions run in after a commit (WP-ADR151
// section 1): stages in order, each stage's subscribers in order, then the batch
// subscribers; with what each stage sees and the kinds a subscriber is limited
// to. Moving, adding or rerouting one is a visible change to this list.
func TestSubscriptionOrder(t *testing.T) {
	s := &Service{}
	plan := s.subscriptionPlan()
	sees := map[kernel.Sees]string{kernel.SeesPrivate: "private", kernel.SeesEvery: "every", kernel.SeesVisible: "visible"}
	var got []string
	for _, st := range plan.stages {
		var names []string
		for _, sub := range st.subs {
			name := sub.name
			if sub.kinds != nil {
				name += "[" + strings.Join(sub.kinds, ",") + "]"
			}
			names = append(names, name)
		}
		got = append(got, st.name+" ("+sees[st.sees]+"): "+strings.Join(names, " "))
	}
	var batch []string
	for _, b := range plan.after {
		batch = append(batch, b.name)
	}
	got = append(got, "after: "+strings.Join(batch, " "))
	want := []string{
		"notices (private): deliverNotification",
		"live (every): hub prWait stackParentMerged",
		"notify (visible): notifyFor",
		"react (visible): renumberOnPR[" + EvPRUpdated + "] repoPushed[" + EvRepoPushed + "] kitPush[" + EvRepoPushed + "] resumeSessions[" + EvAgentsResumed + "] dropUserTranscripts[" + EvUserDeleted + "] refreshKit[" + EvKitSourceSet + "] checkReviewHealth[" + EvReviewLoopRecorded + "] urgentFromLink addPermissionRule backfillEstimates[" + EvProjectUpdated + "] autoEstimateAndTitle[" + EvIssueCreated + "] autoRelate[" + EvIssueCreated + "] followUpsByFiles[" + EvExternalPRMerged + "] autoScreen deliverWebhooks pushIssueState[" + EvIssueStatus + "] pushComment[" + EvIssueCommented + "] pushCommentChange runRepoHook prAudit babysitEvent parkAnswered heldAnswered crossFiled leftover workLog checkSettled reviewAudit fanout goalDrift mergeOrder objective",
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
