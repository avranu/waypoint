package core

import (
	"context"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// subscription is one reaction to events: its name, the kinds it is for (none:
// every event its stage sees) and what it runs.
type subscription struct {
	name  string
	kinds []string
	h     kernel.Handler
}

// stageSpec is a bus stage and its subscribers in the order they run.
type stageSpec struct {
	name string
	sees kernel.Sees
	subs []subscription
}

// batchSpec is a batch subscriber, which runs after every stage.
type batchSpec struct {
	name string
	h    kernel.BatchHandler
}

// subscriptionPlan is every reaction to a committed event, in the order they
// run (WP-ADR151 section 1). A reaction is added here, in the place its order
// requires; TestSubscriptionOrder pins the order.
type subscriptionPlan struct {
	stages []stageSpec
	after  []batchSpec
}

// subscribe builds the plan and subscribes it to the kernel's bus. It runs in
// New, before any write.
func (s *Service) subscribe() {
	plan := s.subscriptionPlan()
	for _, sp := range plan.stages {
		st := s.k.Stage(sp.name, sp.sees)
		for _, sub := range sp.subs {
			if sub.kinds == nil {
				st.On(sub.h)
			} else {
				st.OnKind(sub.h, sub.kinds...)
			}
		}
	}
	for _, b := range plan.after {
		s.k.AfterStages(b.h)
	}
}

func (s *Service) subscriptionPlan() subscriptionPlan {
	kinds := func(k ...string) []string { return k }
	return subscriptionPlan{
		stages: []stageSpec{
			{"notices", kernel.SeesPrivate, []subscription{
				{"deliverNotification", nil, func(ctx context.Context, _ *Actor, e *Event) {
					if n := hint[*storedNotice](e); n != nil {
						s.deliverNotification(ctx, n)
					}
				}},
			}},
			{"live", kernel.SeesEvery, []subscription{
				{"hub", nil, func(_ context.Context, _ *Actor, e *Event) { s.Hub.Publish(e) }},
				{"prWait", nil, func(_ context.Context, _ *Actor, e *Event) { s.prWait.wake(e) }},
				{"stackParentMerged", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onStackParentMerged(ctx, e) }},
			}},
			{"notify", kernel.SeesVisible, []subscription{
				{"notifyFor", nil, func(ctx context.Context, actor *Actor, e *Event) { s.notifyFor(ctx, actor, e) }},
			}},
			{"react", kernel.SeesVisible, []subscription{
				{"renumberOnPR", kinds(EvPRUpdated), func(ctx context.Context, _ *Actor, e *Event) { s.renumberOnPR(ctx, e) }},
				{"repoPushed", kinds(EvRepoPushed), func(ctx context.Context, _ *Actor, e *Event) { s.onRepoPushed(ctx, e) }},
				{"kitPush", kinds(EvRepoPushed), func(ctx context.Context, _ *Actor, e *Event) { s.onKitPush(ctx, e) }},
				{"resumeSessions", kinds(EvAgentsResumed), func(ctx context.Context, _ *Actor, e *Event) {
					if uid, ok := e.Data["user"].(int64); ok {
						s.resumeSessions(ctx, uid)
					}
				}},
				{"dropUserTranscripts", kinds(EvUserDeleted), func(_ context.Context, _ *Actor, e *Event) { s.dropUserTranscripts(e) }},
				{"refreshKit", kinds(EvKitSourceSet), func(ctx context.Context, _ *Actor, _ *Event) {
					s.goBG(func() { s.RefreshKit(withoutLineLocks(ctx)) })
				}},
				{"checkReviewHealth", kinds(EvReviewLoopRecorded), func(ctx context.Context, _ *Actor, e *Event) {
					s.goBG(func() { s.checkReviewHealth(withoutLineLocks(ctx), e.ProjectID) })
				}},
				{"urgentFromLink", nil, func(ctx context.Context, _ *Actor, e *Event) {
					if hint[*pullRef](e) != nil {
						s.goBG(func() { s.urgentFromLink(withoutLineLocks(ctx), e) })
					}
				}},
				{"addPermissionRule", nil, func(ctx context.Context, actor *Actor, e *Event) {
					if j := hint[*permRuleJob](e); j != nil {
						s.addPermissionRule(ctx, actor, j)
					}
				}},
				{"backfillEstimates", kinds(EvProjectUpdated), func(ctx context.Context, _ *Actor, e *Event) {
					if e.Field == "auto_estimate" && e.New == "on" {
						if p, err := s.projectByID(ctx, s.db.R, e.ProjectID); err == nil && p.Settings.AutoEstimate {
							s.backfillEstimates(ctx, p)
						}
					}
				}},
				{"autoEstimateAndTitle", kinds(EvIssueCreated), func(ctx context.Context, actor *Actor, e *Event) {
					if (actor == nil || actor.Automation == nil || actor.Automation.ID > 0) && actor.Allows(PermAIUse) {
						s.autoEstimate(ctx, e)
						s.autoTitle(ctx, e)
					}
				}},
				{"autoRelate", kinds(EvIssueCreated), func(ctx context.Context, _ *Actor, e *Event) { s.autoRelate(ctx, e) }},
				{"followUpsByFiles", kinds(EvExternalPRMerged), func(ctx context.Context, _ *Actor, e *Event) {
					s.goBG(func() { s.followUpsByFiles(withoutLineLocks(ctx), e) })
				}},
				{"autoScreen", nil, func(_ context.Context, _ *Actor, e *Event) { s.autoScreen(e) }},
				{"deliverWebhooks", nil, func(ctx context.Context, _ *Actor, e *Event) { s.deliverWebhooks(ctx, e) }},
				{"pushIssueState", kinds(EvIssueStatus), func(ctx context.Context, actor *Actor, e *Event) {
					if actor == nil || !actor.ForgeSync {
						s.pushIssueState(ctx, e)
					}
				}},
				{"pushComment", kinds(EvIssueCommented), func(ctx context.Context, _ *Actor, e *Event) { s.pushComment(ctx, e) }},
				{"pushCommentChange", nil, func(ctx context.Context, actor *Actor, e *Event) {
					if hint[*forgeCommentChange](e) != nil && (actor == nil || !actor.ForgeSync) {
						s.pushCommentChange(ctx, e)
					}
				}},
				{"runRepoHook", nil, func(ctx context.Context, _ *Actor, e *Event) {
					if j := hint[*repoHookJob](e); j != nil {
						s.goBG(func() { s.runRepoHook(withoutLineLocks(ctx), j) })
					}
				}},
				{"prAudit", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onPRAudit(ctx, e) }},
				{"babysitEvent", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onBabysitEvent(ctx, e) }},
				{"parkAnswered", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onParkAnswered(ctx, e) }},
				{"heldAnswered", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onHeldAnswered(ctx, e) }},
				{"crossFiled", nil, func(ctx context.Context, _ *Actor, e *Event) { s.crossFiledEvent(ctx, e) }},
				{"leftover", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onLeftoverEvent(ctx, e) }},
				{"workLog", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onWorkLogEvent(ctx, e) }},
				{"checkSettled", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onCheckSettled(ctx, e) }},
				{"reviewAudit", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onReviewAudit(ctx, e) }},
				{"fanout", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onFanoutEvent(ctx, e) }},
				{"goalDrift", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onGoalDriftEvent(ctx, e) }},
				{"mergeOrder", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onMergeOrderEvent(ctx, e) }},
				{"objective", nil, func(ctx context.Context, _ *Actor, e *Event) { s.onObjectiveEvent(ctx, e) }},
			}},
		},
		after: []batchSpec{
			{"afterDispatch", s.afterDispatch},
			{"automations", s.runAutomationsBatch},
		},
	}
}

// runAutomationsBatch runs the automations for each event; runAutomations
// enforces maxAutomationDepth so the limit is logged only when a matching
// automation is skipped.
func (s *Service) runAutomationsBatch(ctx context.Context, actor *Actor, events []*Event) {
	for _, e := range events {
		s.runAutomations(ctx, actor, e)
	}
}
