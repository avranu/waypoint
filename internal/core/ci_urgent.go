package core

// The CI pool's urgent lane (WP-528): a pull request labelled ci-urgent has
// its jobs ask for the forgejo-ci-urgent runner label, which runners keep
// for it, so they start ahead of the queue (infrastructure
// docs/runbooks/forgejo-runner.md, "Urgent pull requests"). The label is
// the whole switch: a workflow reads it in its runs-on, and a person can
// put it on or take it off on the forge just as well. wp ship --urgent
// opens a pull request with it, wp pr urgent N [--off] changes it. A task
// tagged ci-urgent passes the label to its pull request when that opens
// (WP-697), and only then, so a push or another ship doesn't bring back a
// label taken off.
//
// The label cuts the rest of the line too (WP-686): in its merge queue an
// urgent pull request goes ahead of every one that isn't, behind the front
// and a base fix (urgentFirst); its rounds don't wait for its place in the
// line (roundTurn) nor for its checks to finish (urgentRound), and a runner
// takes them before any other job (claimOrder). Before, only its CI jobs
// were ahead: waypoint#321 waited an hour as a draft behind the line, and
// its review findings never got a round while each update of the front
// cancelled the run they waited on.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// CIUrgentLabel is the pull request label that sends its CI to the urgent
// lane.
const CIUrgentLabel = "ci-urgent"

func hasLabel(labels []string, name string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, name) })
}

// urgent: its CI runs on the urgent lane.
func (pr *PullRequest) urgent() bool { return hasLabel(pr.Labels, CIUrgentLabel) }

// taskUrgent is true when the task, as actor may see it, is tagged
// ci-urgent. No task, or one that can't be read, isn't.
func (s *Service) taskUrgent(ctx context.Context, actor *Actor, ref string) bool {
	if strings.TrimSpace(ref) == "" {
		return false
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(i.Labels, func(l Label) bool { return strings.EqualFold(l.Name, CIUrgentLabel) })
}

// taggedUrgentNotice tells whoever opened pull request n why it is urgent
// when they didn't ask, and how to undo that.
func taggedUrgentNotice(task string, n int) string {
	return fmt.Sprintf("%s is tagged %s, so this pull request is too: it delays other CI runs to cut the line. If that isn't wanted, run `wp pr urgent %d --off`.", task, CIUrgentLabel, n)
}

// urgentFromLink labels a pull request opened on the forge, whose own text
// (branch, title, closing verb) just linked it to a task tagged ci-urgent
// (WP-697). wp ship's pull requests open with the label; one from outside
// stays where it is, so naming a tagged task doesn't jump the line.
func (s *Service) urgentFromLink(ctx context.Context, e *Event) {
	l := e.pullLinked
	if !s.taskUrgent(ctx, SystemActor(), e.IssueKey) {
		return
	}
	pr, err := s.prByNumber(ctx, s.db.R, l.forgeID, l.repo, l.number)
	if err != nil {
		s.log.WarnContext(ctx, "read a pull request linked to an urgent task", "repo", l.repo, "number", l.number, "err", err)
		return
	}
	if pr == nil || pr.Outside || pr.urgent() || (pr.State != "open" && pr.State != "draft") {
		return
	}
	if err := s.labelPRUrgent(ctx, SystemActor(), pr, true); err != nil {
		s.log.WarnContext(ctx, "label a pull request linked to an urgent task", "pr", pr.ID, "task", e.IssueKey, "err", err)
	}
}

// urgentPR is SQL true for a pull_requests row of alias p labelled
// ci-urgent. Labels are a JSON array of strings; LIKE ignores case.
const urgentPR = `p.labels LIKE '%"` + CIUrgentLabel + `"%'`

// urgentRoundJob is SQL true for a runner_jobs row that is a round of a
// babysit, still going, of an urgent pull request.
const urgentRoundJob = `(source = '` + babysitSource + `' AND source_id IN (SELECT b.id FROM babysits b JOIN pull_requests p ON p.id = b.pr_id
	WHERE b.ended_at IS NULL AND ` + urgentPR + `))`

// urgentRound starts the round for what the reviews left on an urgent pull
// request's head while its checks still run, and reports whether it did
// (WP-686). The round's push starts the full run again anyway, so waiting
// for this run only delays it; and at the front of a busy line each update
// cancels the run the comments wait for, so they might never get a round.
// It still waits for the reviews to settle, so their findings go in one
// round, and a failed check still waits for the run to end.
func (s *Service) urgentRound(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	if !pr.urgent() || pr.Outside || sameSHA(b.RoundSHA, pr.HeadSHA) {
		return false
	}
	comments, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return false
	}
	findings := s.auditRoundWork(ctx, b, pr.ID)
	if len(findings) == 0 && !slices.ContainsFunc(comments, func(rc *PRReviewComment) bool { return !rc.Dealt }) {
		return false // nothing to do: the usual reads below are spared
	}
	// A read that fails can't show the reviews settled: no round (WP-748),
	// and the log says so (audit 674.3).
	going, _, err := s.reviewGoingErr(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read whether its reviews settled", "pr", pr.ID, "err", err)
		return false
	}
	if going {
		return false
	}
	checks, err := s.prChecks(ctx, pr)
	if err != nil {
		return false
	}
	pol, attached, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		s.log.WarnContext(ctx, "read its merge policy", "pr", pr.ID, "err", err)
		return false
	}
	if attached {
		if waits, _, err := s.settleWaits(ctx, pr, pol, checks); err != nil || len(waits) > 0 {
			return false
		}
	}
	work, late, _ := s.commentWork(ctx, pr, b, comments)
	if len(work) == 0 && len(findings) == 0 {
		return false
	}
	if len(work) > 0 {
		work = append(work, late...)
	}
	s.markSeen(ctx, b, pr, comments)
	s.babysitRound(ctx, b, pr, nil, false, work, findings)
	return true
}

// SetPRUrgent puts the ci-urgent label on a pull request (on) or takes it
// off, on the forge and in Waypoint's record. Whoever may merge it may (an
// agent: its own). Labelling an open pull request starts its CI again on
// the urgent lane in a repo whose workflow runs on the labeled event;
// taking it off changes no run already started.
func (s *Service) SetPRUrgent(ctx context.Context, actor *Actor, prID int64, on bool) (*PRDetail, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if err := s.mayMerge(ctx, actor, pr); err != nil {
		return nil, err
	}
	if on && pr.State != "open" && pr.State != "draft" {
		return nil, invalid("it is %s: nothing to run", pr.State)
	}
	if pr.urgent() != on {
		if err := s.labelPRUrgent(ctx, actor, pr, on); err != nil {
			return nil, err
		}
	}
	return s.GetPullRequest(ctx, actor, pr.ID)
}

// labelPRUrgent changes the label on the forge and records it.
func (s *Service) labelPRUrgent(ctx context.Context, actor *Actor, pr *PullRequest, on bool) error {
	cl, ok := s.Forges.(ForgePRMeta)
	if !ok {
		return invalid("this forge client can't label pull requests")
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if f.token == "" {
		return invalid("the forge has no API token; add one in Admin → Forges")
	}
	add, remove := []string{CIUrgentLabel}, []string(nil)
	if !on {
		add, remove = nil, add
	}
	if err := cl.ChangePRLabels(ctx, f, pr.Repo, pr.Number, add, remove); err != nil {
		return fmt.Errorf("label %s#%d: %w", pr.Repo, pr.Number, err)
	}
	labels := slices.DeleteFunc(slices.Clone(pr.Labels), func(l string) bool { return strings.EqualFold(l, CIUrgentLabel) })
	detail := "took the urgent CI lane off"
	if on {
		labels, detail = append(labels, CIUrgentLabel), "put the urgent CI lane on"
	}
	lb, _ := json.Marshal(labels)
	projectID, rerr := s.prAuthority(ctx, actor, pr, RoleViewer)
	if rerr != nil {
		// Not the access check (mayMerge, or the system, already decided that):
		// this only finds the project to record the change under, and the label
		// is already on the forge, so the record goes in unscoped. No project
		// showing it is an answer; only a failed read is said.
		if readFault(rerr) && !missingOrDenied(rerr) {
			s.log.WarnContext(ctx, "pr authority failed", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", rerr)
		}
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET labels = ? WHERE id = ?`, string(lb), pr.ID); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "pr.urgent", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail + " for “" + excerpt(pr.Title, 120) + "”", About: actor.agentName()})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: map[bool]string{true: "urgent", false: "not urgent"}[on], Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
}
