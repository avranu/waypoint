package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pushed by itself (WP-843). Commits left only on one machine are how work
// gets lost track of, so when a session lets go of a task (wp release, a
// /clear, an exit, or its checkout going quiet for strandedIdle) the
// machine's wp pushes the committed work of a branch that is the task's own
// to the forge, and says so here: one line on the task, and the task's
// stranded_work row names the branch, so wp next sends whoever takes it there.
// Uncommitted files stay where they are and keep being reported.

// AutoPushInput is what a machine's wp pushed (or failed to push).
type AutoPushInput struct {
	// Keys are the tasks the work is for; none: those the branch names.
	Keys    []string `json:"keys,omitempty"`
	Machine string   `json:"machine,omitempty"`
	Top     string   `json:"top,omitempty"`    // the checkout it was in
	Branch  string   `json:"branch"`           // the branch on the forge
	Head    string   `json:"head,omitempty"`   // the commit pushed
	Commits int      `json:"commits"`          // how many no remote had
	Why     string   `json:"why,omitempty"`    // release, clear, logout, prompt_input_exit, ended, idle
	Error   string   `json:"error,omitempty"`  // why the push failed; "" when it went
	Rescue  bool     `json:"rescue,omitempty"` // a detached head's commits, onto a branch named for the task
}

// AutoPushResult is the tasks told.
type AutoPushResult struct {
	Told []string `json:"told"`
}

// pushWhen says when the work was pushed: "when its session was cleared".
func pushWhen(why string) string {
	switch why {
	case "release":
		return "when its agent gave the task back"
	case "clear":
		return "when its session was cleared (/clear)"
	case "logout", "prompt_input_exit", "exit":
		return "when its session ended"
	case "ended":
		return "when autopilot ended its session"
	case "idle":
		return "after its checkout sat unused for " + strconv.Itoa(int(strandedIdle.Hours())) + " hours"
	}
	return "when its session let go of it"
}

// autoPushComment is the one line on the task.
func autoPushComment(key string, in AutoPushInput) string {
	place := "`" + in.Top + "`"
	if in.Machine != "" {
		place += " on " + in.Machine
	}
	what := countOf(in.Commits, "commit")
	if in.Error != "" {
		return fmt.Sprintf("Couldn't push the %s left in %s to branch `%s` %s: %s. They are only in that checkout.",
			what, place, in.Branch, pushWhen(in.Why), in.Error)
	}
	rescued := ""
	if in.Rescue {
		rescued = " (they were on a detached head, so the branch is named for the task)"
	}
	return fmt.Sprintf("Pushed %s: the %s left in %s %s on the forge as branch `%s`%s. `wp next %s` shows where to resume.",
		pushWhen(in.Why), what, place, isAre(in.Commits), in.Branch, rescued, key)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// cleanAutoPush bounds what a machine sent.
func cleanAutoPush(in AutoPushInput) (AutoPushInput, error) {
	in.Branch = strings.TrimSpace(in.Branch)
	if in.Branch == "" || len(in.Branch) > 200 || strings.ContainsAny(in.Branch, " \t\n`") {
		return in, invalid("branch is required: the branch on the forge")
	}
	in.Top = excerpt(strings.TrimSpace(in.Top), 1000)
	in.Machine = excerpt(strings.TrimSpace(in.Machine), 200)
	in.Head = excerpt(strings.TrimSpace(in.Head), 64)
	in.Why = excerpt(strings.TrimSpace(in.Why), 40)
	in.Error = excerpt(strings.Join(strings.Fields(in.Error), " "), 300)
	in.Commits = max(in.Commits, 1)
	if len(in.Keys) > 10 {
		in.Keys = in.Keys[:10]
	}
	return in, nil
}

// RecordAutoPush says on each task the work is for that its commits went to
// the forge (or why they couldn't), once per commit, and keeps the branch on
// the task's stranded work so wp next names it.
func (s *Service) RecordAutoPush(ctx context.Context, actor *Actor, in AutoPushInput) (*AutoPushResult, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	in, err := cleanAutoPush(in)
	if err != nil {
		return nil, err
	}
	keys := in.Keys
	if len(keys) == 0 {
		for _, r := range branchRefs(in.Branch) {
			keys = append(keys, r.Key)
		}
	}
	res := &AutoPushResult{Told: []string{}}
	seen := map[int64]bool{}
	for _, key := range keys {
		err := s.write(ctx, actor, func(t *tx) error {
			i, _, err := s.getIssueFor(ctx, t, actor, key, RoleEditor, false)
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
				return nil // not a task of theirs: nobody to tell
			}
			if err != nil {
				return err
			}
			if seen[i.ID] || i.Closed() {
				return nil // already told, or finished: nobody to tell
			}
			seen[i.ID] = true
			told, err := s.recordPush(ctx, t, actor, i, in)
			if told {
				res.Told = append(res.Told, i.Key)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// recordPush updates (or adds) the task's latest stranded work in that
// checkout and comments; told is false when this commit was told already.
func (s *Service) recordPush(ctx context.Context, t *tx, actor *Actor, i *Issue, in AutoPushInput) (told bool, err error) {
	now := s.nowMS()
	var id int64
	var head, pushErr string
	err = t.QueryRowContext(ctx, `SELECT id, pushed_head, push_error FROM stranded_work WHERE issue_id = ? AND machine = ? AND top = ? AND at > ?
		ORDER BY at DESC LIMIT 1`, i.ID, in.Machine, in.Top, now-strandedShowDays*24*time.Hour.Milliseconds()).Scan(&id, &head, &pushErr)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Released, or quiet without a session ending: the row wp next reads.
		// It names no session, so a session starting in the same terminal
		// isn't handed a task its agent gave back (strandedHere).
		why := cmp(in.Why, "other")
		if _, err := t.ExecContext(ctx, `INSERT INTO stranded_work(issue_id, agent, machine, top, branch, unpushed, why, at, pushed, pushed_head, push_error)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, i.ID, actor.Agent, in.Machine, in.Top, in.Branch, in.Commits, why, now,
			pushedBranch(in), in.Head, in.Error); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if in.Head != "" && head == in.Head && (pushErr == "") == (in.Error == "") {
			return false, nil // told already
		}
		// pushed keeps the branch an earlier push went to; push_error says
		// the latest commits didn't follow (ResumeText reads it first).
		if _, err := t.ExecContext(ctx, `UPDATE stranded_work SET pushed = coalesce(nullif(?, ''), pushed), pushed_head = ?, push_error = ?,
			unpushed = coalesce(nullif(?, 0), unpushed) WHERE id = ?`,
			pushedBranch(in), in.Head, in.Error, in.Commits, id); err != nil {
			return false, err
		}
	}
	if _, err := s.addComment(ctx, t, SystemActor(), i, autoPushComment(i.Key, in)); err != nil {
		return false, err
	}
	return true, nil
}

// pushedBranch is what stranded_work.pushed holds: the branch, once it went.
func pushedBranch(in AutoPushInput) string {
	if in.Error != "" {
		return ""
	}
	return in.Branch
}
