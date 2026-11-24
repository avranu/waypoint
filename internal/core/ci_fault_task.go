package core

// One open task per forge and fault kind; agents only read and report, and host changes happen only through deterministic tools.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// runnerFaultTitle is the task about a kind of runner fault on a forge, and
// how a sighting finds the open one it adds to.
func runnerFaultTitle(forge string, f *RunnerFault) string {
	return excerpt("CI runner fault on "+forge+": "+f.What, 200)
}

// runnerFaultTitleBefore is what runnerFaultTitle was until WP-838. An open
// task under it is still the one a sighting adds to (not a duplicate), and
// the sighting brings it up to date: its old text told agents to ssh in.
func runnerFaultTitleBefore(forge string, f *RunnerFault) string {
	return excerpt("Fix the CI runners of "+forge+": "+f.What, 200)
}

// runnerFaultTextBeforeEnd ends what runnerFaultTaskText said until WP-838;
// what follows it is the task's first sighting.
const runnerFaultTextBeforeEnd = "Each later sighting is added here as a comment.\n\n"

// runnerFaultKey keys the request about a kind of runner fault on a forge.
func runnerFaultKey(forgeID int64, f *RunnerFault) string {
	return fmt.Sprintf("%s%d:%s", runnerFaultRequestKey, forgeID, f.What)
}

// firstFault is the fault of the first failed check, by name: what a
// task or request about them is named after.
func firstFault(faults map[string]*RunnerFault) *RunnerFault {
	return faults[slices.Min(slices.Collect(maps.Keys(faults)))]
}

// runnerFaultProjects are where a runner fault on forgeID may be filed, best
// first: the projects that own the CI runners (those with repos on that
// forge first), then the project the failing repo is attached to.
func (s *Service) runnerFaultProjects(ctx context.Context, forgeID int64, repo string) []int64 {
	ids, err := queryIDs(ctx, s.db.R, `SELECT p.id FROM projects p WHERE p.archived = 0 AND json_extract(p.settings, '$.ci_runners') = 1
		ORDER BY EXISTS (SELECT 1 FROM project_repos r WHERE r.project_id = p.id AND r.forge_id = ?) DESC, p.id`, forgeID)
	if err != nil {
		s.log.WarnContext(ctx, "read the projects that own the CI runners", "err", err)
	}
	p, err := s.attachedProject(ctx, s.db.R, forgeID, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project of a repo with a CI runner fault", "repo", repo, "err", err)
		}
	} else if p != 0 && !slices.Contains(ids, p) {
		ids = append(ids, p)
	}
	return ids
}

// openRunnerFaultTask is the open task about f on the forge, or "" (also when
// it can't be looked up). Only for words, a wait naming its task: filing goes
// through openRunnerFault, whose error stops it rather than file a duplicate.
func (s *Service) openRunnerFaultTask(ctx context.Context, forgeID int64, f *RunnerFault) string {
	t, rerr := s.openRunnerFault(ctx, forgeID, f)
	if rerr != nil {
		if readFault(rerr) && !errors.Is(rerr, ErrNotFound) {
			s.log.WarnContext(ctx, "open runner fault failed", "forgeid", forgeID, "err", rerr)
		}
	}
	return t.key
}

// openFaultTask is an open runner-fault task; before says it has its
// pre-WP-838 title and text.
type openFaultTask struct {
	key, text string
	before    bool
}

func (s *Service) openRunnerFault(ctx context.Context, forgeID int64, f *RunnerFault) (openFaultTask, error) {
	fg, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return openFaultTask{}, err
	}
	var t openFaultTask
	now, before := runnerFaultTitle(fg.Name, f), runnerFaultTitleBefore(fg.Name, f)
	var title string
	err = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number, i.title, i.description FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.title IN (?, ?) AND st.category NOT IN ('done','canceled') ORDER BY i.id DESC LIMIT 1`, now, before).Scan(&t.key, &title, &t.text)
	if errors.Is(err, sql.ErrNoRows) {
		return openFaultTask{}, nil
	}
	t.before = title != now
	return t, err
}

// updateRunnerFaultTask gives a task filed before WP-838 today's title and
// text, keeping its first sighting.
func (s *Service) updateRunnerFaultTask(ctx context.Context, writer *Actor, forge string, f *RunnerFault, t openFaultTask) error {
	title := runnerFaultTitle(forge, f)
	text := runnerFaultTaskText(f)
	if _, first, ok := strings.Cut(t.text, runnerFaultTextBeforeEnd); ok {
		text += first
	} else {
		text += "Its earlier text is in this task's history.\n"
	}
	_, err := s.UpdateIssue(ctx, writer, t.key, IssuePatch{Title: &title, Description: &text})
	return err
}

// reportRunnerFault tracks the runner fault in a task (or adds this
// sighting to the open one), and closes the open requests about it into the
// task. It returns the task's key; with no project to file it in, or none
// the owner may write to, it asks the owner instead (askAboutRunner) and
// returns "" with asked saying whether a request is open.
func (s *Service) reportRunnerFault(ctx context.Context, userID int64, at faultSubject, faults map[string]*RunnerFault, why string) (task string, asked bool) {
	f := firstFault(faults)
	task, err := s.fileRunnerFault(ctx, userID, at, faults, why)
	if readFault(err) {
		s.log.WarnContext(ctx, "file a CI runner fault as a task", "at", at.name, "err", err)
	}
	if task == "" {
		return "", s.askAboutRunner(ctx, userID, at, faults, why)
	}
	s.closeRunnerFaultRequests(ctx, runnerFaultKey(at.forgeID, f), task)
	return task, true
}

func (s *Service) fileRunnerFault(ctx context.Context, userID int64, at faultSubject, faults map[string]*RunnerFault, why string) (string, error) {
	owner, err := s.userByID(ctx, s.db.R, userID)
	if err != nil {
		return "", err
	}
	if owner.Disabled {
		return "", nil
	}
	fg, err := s.forgeByID(ctx, at.forgeID)
	if err != nil {
		return "", err
	}
	writer := Actor{User: owner}
	writer.textAgent = "waypoint"
	f := firstFault(faults)
	seen := s.runnerFaultSighting(ctx, at, faults, why)
	open, err := s.openRunnerFault(ctx, at.forgeID, f)
	if err != nil {
		return "", err
	}
	if open.key != "" {
		if open.before {
			if err := s.updateRunnerFaultTask(ctx, &writer, fg.Name, f, open); err != nil {
				s.log.WarnContext(ctx, "bring a runner-fault task's text up to date", "task", open.key, "err", err)
			}
		}
		if _, err := s.AddComment(ctx, &writer, open.key, "Seen again. "+seen); err != nil {
			return "", fmt.Errorf("add a runner fault's sighting to %s: %w", open.key, err)
		}
		return open.key, nil
	}
	var errs []error
	for _, p := range s.runnerFaultProjects(ctx, at.forgeID, at.repo) {
		in := IssueInput{Project: itoa64(p), Type: "bug", Agent: "claude", Priority: "high", Labels: []string{"ci"},
			Title: runnerFaultTitle(fg.Name, f), Description: runnerFaultTaskText(f) + seen}
		made, err := s.CreateIssue(ctx, &writer, in)
		if err == nil {
			return made.Key, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// runnerFaultTaskText is what the task asks of the agent that takes it.
func runnerFaultTaskText(f *RunnerFault) string {
	return "CI jobs fail because " + f.What + ", not because of their code. Waypoint keeps re-running them on a schedule that backs off, and babysat pull requests wait without an agent round, keeping their place in the merge queue. " +
		"This task tracks the fault.\n\n" +
		"What to do:\n\n" +
		"1. **Read what is known**: `wp ci doctor` and `wp ci status -v` (the hosts' health and recent faults), and the failed jobs' logs linked below.\n" +
		"2. **Say what you found.** If a host or its setup needs changing, file a draft task for the project that owns the infrastructure (`wp new \"…\" -p PROJECT --draft`) with the evidence and what it should assess. Then finish this task; the re-runs carry on without it.\n\n" +
		"Don't ssh to a host, delete or prune anything by hand, restart anything by hand, or edit or open a pull request in the infrastructure repo. Freeing disk and restarting a runner happen only through deterministic tools that can do nothing else, such as `tidy`, and only where that tool is already set up to run.\n\n" +
		"Each later sighting is added here as a comment.\n\n"
}

// runnerFaultSighting is one sighting: where, the failed jobs and their log
// lines, the runners' hosts as they report, why Waypoint stopped re-running,
// and the pull requests waiting on a runner fault on the forge now.
func (s *Service) runnerFaultSighting(ctx context.Context, at faultSubject, faults map[string]*RunnerFault, why string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Seen on %s (%s) at %s.\n\n", faultCode(at.name), faultCode(at.url), s.now().UTC().Format("2006-01-02 15:04 UTC"))
	var runners []string
	for _, n := range slices.Sorted(maps.Keys(faults)) {
		fl := faults[n]
		on := ""
		if fl.Runner != "" {
			on = " on runner " + faultCode(fl.Runner)
			if !slices.Contains(runners, fl.Runner) {
				runners = append(runners, fl.Runner)
			}
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", faultCode(n), on, faultCode(fl.Line))
	}
	if len(runners) > 0 {
		var hosts []string
		for _, r := range runners {
			switch h, w := s.runnerHealth(ctx, at.forgeID, r); {
			case h == "unknown":
				hosts = append(hosts, r+": doesn't report")
			case w != "":
				hosts = append(hosts, r+": "+h+" ("+w+")")
			default:
				hosts = append(hosts, r+": "+h)
			}
		}
		fmt.Fprintf(&b, "\nHost reports: %s.\n", strings.Join(hosts, "; "))
	}
	if why != "" {
		fmt.Fprintf(&b, "\n%s\n", why)
	}
	if waiting := s.runnerFaultWaiting(ctx, at.forgeID); len(waiting) > 0 {
		fmt.Fprintf(&b, "\nWaiting on a runner fault now: %s.\n", strings.Join(waiting, ", "))
	}
	return b.String()
}

// faultCode quotes text that came from a pull request or a job log as one
// inline code span: no backticks, newlines or control characters to break out
// of it into the task's instructions.
func faultCode(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029:
			return ' '
		}
		return r
	}, s)
	return "`" + excerpt(strings.TrimSpace(s), 300) + "`"
}

// runnerFaultWaiting are the babysat pull requests on the forge waiting on
// a runner fault.
func (s *Service) runnerFaultWaiting(ctx context.Context, forgeID int64) []string {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.repo, p.number FROM babysits b JOIN pull_requests p ON p.id = b.pr_id
		WHERE b.ended_at IS NULL AND p.forge_id = ? AND b.wait LIKE '%failed on the CI runner, not the code%' ORDER BY p.repo, p.number`, forgeID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var repo string
		var n int
		if rows.Scan(&repo, &n) == nil {
			out = append(out, fmt.Sprintf("%s#%d", repo, n))
		}
	}
	return out
}

// closeRunnerFaultRequests closes the open requests asking people to fix
// the runner for this fault (from before WP-589, or a fallback): the task
// has it now.
func (s *Service) closeRunnerFaultRequests(ctx context.Context, key, task string) {
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM requests WHERE tool_use_id = ? AND state = ?`, key, RequestOpen)
	if err != nil || len(ids) == 0 {
		return
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, id := range ids {
			if _, err := s.closeRequestTx(ctx, t, id, closedByWatch, "Moved to "+task+", which tracks this fault.", false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "close runner-fault requests into their task", "task", task, "err", err)
	}
}
