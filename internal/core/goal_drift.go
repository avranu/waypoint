package core

import (
	"context"
	"fmt"
	"strings"
)

// NotifyGoals is the notification kind of the weekly goals check.
const NotifyGoals = "goals"

const goalDriftEvery = 7 * 24 * 3600 * 1000

// startGoalDrift runs maybeGoalDrift off the scheduler's loop (a
// project's suggestions wait on a model), one run at a time.
func (s *Service) startGoalDrift(ctx context.Context) {
	s.goals.mu.Lock()
	if s.goals.drifting {
		s.goals.mu.Unlock()
		jobBusy(ctx, "the last drift check is still going")
		return
	}
	s.goals.drifting = true
	idle := make(chan struct{})
	s.goals.driftIdle = idle
	s.goals.mu.Unlock()
	s.goBG(func() {
		defer func() {
			s.goals.mu.Lock()
			s.goals.drifting = false
			close(idle)
			s.goals.mu.Unlock()
		}()
		s.maybeGoalDrift(ctx)
	})
}

// maybeGoalDrift runs the check for each project that is due.
func (s *Service) maybeGoalDrift(ctx context.Context) {
	ids, err := s.goalDriftDue(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "goal drift", "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.suggestGoalsWeekly(ctx, id)
		if err := s.goalDriftCheck(ctx, id); err != nil {
			s.log.WarnContext(ctx, "goal drift", "project", id, "err", err)
		}
	}
}

// goalDriftDue lists the projects whose check is due: those with an
// approved goal of their own, and top-level projects under a global one.
// A project whose goals are its parent's is in its parent's check.
func (s *Service) goalDriftDue(ctx context.Context) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id FROM projects p LEFT JOIN goal_drift_checks c ON c.project_id = p.id
		WHERE p.archived = 0 AND (c.checked_at IS NULL OR c.checked_at <= ?)
		AND EXISTS (SELECT 1 FROM design_notes g WHERE g.kind = 'goal' AND g.status = 'active'
			AND (g.approved_at IS NOT NULL OR g.requested_by IS NOT NULL)
			AND (g.project_id = p.id OR (g.project_id IS NULL AND p.parent_id IS NULL)))
		ORDER BY p.id`, s.nowMS()-goalDriftEvery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// goalDriftCheck records one project's week and tells its owners what
// changed, when there is anything to say.
func (s *Service) goalDriftCheck(ctx context.Context, projectID int64) error {
	p, err := s.projectByID(ctx, s.db.R, projectID)
	if err != nil {
		return err
	}
	owners := s.userIDsWhere(ctx, `SELECT m.user_id FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? AND m.role = 'owner' AND u.disabled = 0 ORDER BY m.user_id`, projectID)
	var notes []string
	if note := s.driftWeekly(ctx, projectID); note != "" {
		notes = append(notes, note)
	}
	switch n, err := s.driftUnchecked(ctx, projectID); {
	case err != nil:
		s.log.WarnContext(ctx, "goal drift unchecked", "project", projectID, "err", err)
		notes = append(notes, "The drift check couldn't tell whether any pull request went on without it this week.")
	case n > 0:
		notes = append(notes, fmt.Sprintf("The drift check couldn't read %s this week; each went on without it.", goalCount(n, "pull request", "pull requests")))
	}
	var prevChecked, prevFinished int64
	var prevGrounded float64
	_ = s.db.R.QueryRowContext(ctx, `SELECT checked_at, grounded, finished FROM goal_drift_checks WHERE project_id = ?`, projectID).Scan(&prevChecked, &prevGrounded, &prevFinished)
	// Each owner hears the week as they see it (a sub-project they can't
	// open stays out of their counts); the record keeps the first owner's
	// view, or the whole project's with no owner.
	views := make([]*GoalsOverview, len(owners))
	for i, id := range owners {
		u, err := s.userByID(ctx, s.db.R, id)
		if err != nil {
			return err
		}
		if views[i], err = s.ListGoals(ctx, &Actor{User: u}, p.Key); err != nil {
			return err
		}
	}
	var o *GoalsOverview
	if len(views) > 0 {
		o = views[0]
	} else {
		o = &GoalsOverview{Project: p.Key, ProjectName: p.Name}
		subtree, err := s.projectSubtree(ctx, projectID)
		if err != nil {
			return err
		}
		if o.Drift, err = s.goalDrift(ctx, nil, subtree); err != nil {
			return err
		}
	}
	finished := 0
	if o.Drift != nil {
		for _, n := range o.Drift.Week {
			finished += n
		}
	}
	grounded := 0.0
	summary := ""
	if o.Drift != nil {
		grounded, summary = o.Drift.Grounded, o.Drift.Summary
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT INTO goal_drift_checks(project_id, checked_at, grounded, finished, untraced, summary) VALUES (?,?,?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET checked_at = excluded.checked_at, grounded = excluded.grounded, finished = excluded.finished,
			untraced = excluded.untraced, summary = excluded.summary`, projectID, s.nowMS(), grounded, finished, o.UntracedAll, summary)
		return err
	})
	if err != nil {
		return err
	}
	for i, id := range owners {
		title, body := goalDriftNotice(views[i], prevChecked != 0, prevGrounded, prevFinished, notes)
		if title != "" {
			s.notifyURL(ctx, id, nil, NotifyGoals, title, body, "/p/"+p.Key+"/goals")
		}
	}
	return nil
}

// goalDriftNotice words the week; an empty title means nothing to say.
func goalDriftNotice(o *GoalsOverview, hadPrev bool, prevGrounded float64, prevFinished int64, notes []string) (string, string) {
	var lines []string
	flagged := 0
	for _, f := range o.DriftFlags {
		if f.Answer != "" {
			continue
		}
		if flagged++; flagged <= 10 {
			what := f.Headline
			if f.PR != nil {
				what = fmt.Sprintf("%s#%d: %s", f.PR.Repo, f.PR.Number, what)
			}
			lines = append(lines, what+` — the check's read, not a fact: "`+clipRunes(strings.Join(strings.Fields(f.Reason), " "), 300)+`"`)
		}
	}
	if flagged > 10 {
		lines = append(lines, fmt.Sprintf("%d more drift flags wait.", flagged-10))
	}
	lines = append(lines, notes...)
	finished := 0
	if o.Drift != nil {
		for _, n := range o.Drift.Week {
			finished += n
		}
		if finished > 0 && o.Drift.Summary != "" {
			lines = append(lines, upperFirst(o.Drift.Summary)+".")
		}
	}
	idle := 0
	for _, g := range o.Goals {
		if (g.Note.Approved || g.Note.Requested != nil) && g.Coverage.Open == 0 && g.Coverage.Finished == 0 {
			idle++
		}
	}
	if idle > 0 {
		lines = append(lines, fmt.Sprintf("%s no open or recent work.", goalCount(idle, "goal has", "goals have")))
	}
	none := 0
	for _, u := range o.Untraced {
		if u.Grounding == GroundNone {
			none++
		}
	}
	if o.UntracedAll > 0 {
		line := fmt.Sprintf("%s to no approved goal", goalCount(o.UntracedAll, "open task traces", "open tasks trace"))
		if none > 0 {
			line += fmt.Sprintf("; %d of them no person asked for", none)
		}
		lines = append(lines, line+".")
	}
	if n := len(o.Links); n > 0 {
		lines = append(lines, goalCount(n, "link to a goal waits", "links to goals wait")+" for a yes or no.")
	}
	pending := 0
	for _, g := range o.Goals {
		if !g.Note.Approved && g.Note.Requested == nil {
			pending++
		}
	}
	if pending > 0 {
		lines = append(lines, goalCount(pending, "suggested goal waits", "suggested goals wait")+" for approval.")
	}
	reworded := 0
	for _, g := range o.Goals {
		if g.Rewording != nil {
			reworded++
		}
	}
	if reworded > 0 {
		lines = append(lines, goalCount(reworded, "goal has", "goals have")+" clearer wording to take or leave.")
	}
	if len(lines) == 0 {
		return "", ""
	}
	title := o.ProjectName + ": goals this week"
	if flagged > 0 || (o.Drift != nil && finished > 0 && hadPrev && prevFinished > 0 && o.Drift.Grounded <= prevGrounded-0.15) {
		title = o.ProjectName + ": work is drifting from its goals"
	}
	return title, strings.Join(lines, "\n") + "\nThe Goals view has each with a one-click answer."
}

// goalCount puts the count in front of the phrase for one or for many.
func goalCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
