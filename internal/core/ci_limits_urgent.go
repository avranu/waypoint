package core

// The fix for a CI time limit cuts the merge line (WP-949). While a repo's
// runs are stopped by a limit, every merge ahead of the fix pays for it:
// on 2026-11-12 the pull request that made backend tests 3-7× faster was
// 8th in a line of 18 that took 10½ hours to drain, while shards hit their
// 40m limit 11 times a day, and only a person's `wp pr urgent` moved it.
//
// So the CI limits task (WP-922), filed by Waypoint itself from job logs,
// is tagged ci-urgent when a run is stopped by a limit (the "problem"
// level), and the existing tag rules carry it to the fix: wp ship's pull
// request for the task opens urgent, one whose own text names the task gets
// the label (WP-697), and open pull requests already linked to it get it
// now. The tag goes on once per spell: a person who takes it off the task
// keeps it off until the limits are fine and a run is stopped again. When
// the hourly check finds no run stopped any more, the watch takes off the
// tag it put on; pull requests keep the label they have.
//
// Only Waypoint tags the task, and only from CI evidence; a pull request
// can still name the task to ride along, as with any tagged task, and
// `wp pr urgent N --off` or `wp edit KEY --untag ci-urgent` undo it.

import (
	"context"
	"fmt"
	"strings"
)

// ciLimitUrgeNote is added to the task when the watch tags it. The task is
// read by the agent that takes it, so it states the tag and nothing to act
// on: code carries the label to the fix's pull request (WP-697), and how to
// take the tag off is for people, in the doctor line below. Lane and untag
// instructions got every limits task held by the safety screen (WP-1015).
const ciLimitUrgeNote = "A run was stopped by a limit, so this task is now tagged `ci-urgent`; Waypoint takes the tag off once no run is stopped.\n"

// ciLimitUrgentLine says so in the doctor.
func ciLimitUrgentLine(task string) string {
	return fmt.Sprintf("%s is tagged %s: pull requests for it go ahead of the merge line (`wp edit %s --untag %s` stops that)", task, CIUrgentLabel, task, CIUrgentLabel)
}

// ciLimitUrgedKey is the server setting holding the limits task of a repo
// that the watch tagged ci-urgent this spell, "" when none.
func ciLimitUrgedKey(forgeID int64, repo string) string {
	return fmt.Sprintf("ci_limits_urgent:%d|%s", forgeID, strings.ToLower(repo))
}

func (s *Service) ciLimitUrged(ctx context.Context, forgeID int64, repo string) string {
	v, rerr := s.serverSetting(ctx, s.db.R, ciLimitUrgedKey(forgeID, repo))
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "server setting failed", "err", rerr)
		}
	}
	return v
}

// urgeCILimits tags the open limits task ci-urgent and puts the urgent lane
// on the open pull requests in repo already linked to it.
func (s *Service) urgeCILimits(ctx context.Context, writer *Actor, forgeID int64, repo, task string) {
	// A tag a person put on is theirs: the watch does not claim it, so the
	// calm check leaves it.
	byPerson := s.taskUrgent(ctx, SystemActor(), task)
	d, err := s.UpdateIssue(ctx, writer, task, IssuePatch{AddLabels: []string{CIUrgentLabel}})
	if err != nil {
		s.log.WarnContext(ctx, "tag the CI limits task urgent", "task", task, "err", err)
		return
	}
	if !byPerson {
		s.putSetting(ctx, ciLimitUrgedKey(forgeID, repo), task)
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT number FROM external_links
		WHERE issue_id = ? AND kind = 'pull' AND repo = ? COLLATE NOCASE AND number IS NOT NULL`, d.ID, repo)
	if err != nil {
		s.log.WarnContext(ctx, "read the pull requests of the CI limits task", "task", task, "err", err)
		return
	}
	var numbers []int
	for rows.Next() {
		var n int
		if rows.Scan(&n) == nil {
			numbers = append(numbers, n)
		}
	}
	_ = rows.Close()
	for _, n := range numbers {
		pr, err := s.prByNumber(ctx, s.db.R, forgeID, repo, n)
		if err != nil || pr == nil || pr.Outside || pr.urgent() || (pr.State != "open" && pr.State != "draft") {
			continue
		}
		if err := s.labelPRUrgent(ctx, SystemActor(), pr, true); err != nil {
			s.log.WarnContext(ctx, "label a pull request of the CI limits task urgent", "pr", pr.ID, "task", task, "err", err)
		}
	}
}

// calmCILimits takes the tag the watch put on off the limits task, now that
// no run is stopped by a limit. A task closed since, or one a person
// untagged, is left as it is.
func (s *Service) calmCILimits(ctx context.Context, forgeID int64, repo string) {
	task := s.ciLimitUrged(ctx, forgeID, repo)
	if task == "" {
		return
	}
	forget := func() { s.putSetting(ctx, ciLimitUrgedKey(forgeID, repo), "") }
	if task != s.ciLimitsTask(ctx, forgeID, repo) || !s.taskUrgent(ctx, SystemActor(), task) {
		forget()
		return
	}
	// From here the record stays until the tag is off, so a failed untag is
	// tried again at the next check.
	owner := s.repoProjectOwner(ctx, forgeID, repo)
	u, err := s.userByID(ctx, s.db.R, owner)
	if owner == 0 || err != nil || u.Disabled {
		return
	}
	writer := Actor{User: u}
	writer.textAgent = "waypoint"
	if _, err := s.UpdateIssue(ctx, &writer, task, IssuePatch{RemLabels: []string{CIUrgentLabel}}); err != nil {
		s.log.WarnContext(ctx, "untag the CI limits task", "task", task, "err", err)
		return
	}
	forget()
	if _, err := s.AddComment(ctx, &writer, task, "No run is stopped by a limit any more, so the `ci-urgent` tag is off."); err != nil {
		s.log.WarnContext(ctx, "say the CI limits task is calm", "task", task, "err", err)
	}
}
