package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Commits left unpushed on a pull request's branch (WP-599). WP-567 recovers
// a task's unfinished work when its session ends, but a task out on a pull
// request counts as shipped, so a commit made after the push (a review fix,
// a late change a person asked for) and never pushed stayed in the checkout
// while the pull request went on, and merged, without it. Now:
//
//   - when a session ends with them, the pull request's tasks get a comment
//     saying where the commits are and how they reach it (their watchers
//     are notified; with no task, the session's person is), and the
//     session's timeline says so;
//   - a session starting in the checkout hears of them in one line;
//   - the hourly sweep does the same for a session that went away with no
//     SessionEnd, once unpushedGrace has passed with nothing pushed to the
//     pull request.
//
// Waypoint tells; it doesn't push. Pushing onto a pull request changes what
// it merges and sets off its CI and reviews, and WP-843's push of what a
// session let go of stops short of a branch with an open pull request for
// that reason. What a session left is known from its last Stop
// (work_state); each session's branch is told of once (a session_events
// row of kind unpushed).

// EvUnpushedLeft: a session went away leaving commits its pull request
// hasn't got. UserID is the session's person.
const EvUnpushedLeft = "session.unpushed_left"

// NotifyUnpushed is the notification kind of commits left unpushed.
const NotifyUnpushed = "unpushed"

const (
	// unpushedGrace is how long the sweep leaves a session that went
	// away before saying what it left: a push may still come.
	unpushedGrace = time.Hour
	// unpushedLookback is how far back the sweep looks at sessions.
	unpushedLookback = 3 * 24 * time.Hour
)

// unpushedPR is a checkout's commits that its branch's pull request hasn't
// got.
type unpushedPR struct {
	c      HookCheckout
	pr     int64
	number int
	url    string
	merged bool // it merged without them
	issues []int64
}

// unpushedOnPRs is the checkouts in list, of the session's repository, with
// commits no remote has on a branch of their own whose pull request is open
// (or merged since) and has had nothing pushed to it since: the state of
// list was seen at since.
func (s *Service) unpushedOnPRs(ctx context.Context, q queryer, repo string, list []HookCheckout, since int64) ([]unpushedPR, error) {
	if repo == "" {
		return nil, nil
	}
	var out []unpushedPR
	for _, c := range list {
		if c.Unpushed <= 0 || c.shared() {
			continue
		}
		u := unpushedPR{c: c}
		var state string
		err := q.QueryRowContext(ctx, `SELECT p.id, p.number, p.url, p.state FROM pull_requests p
			WHERE p.repo = ? COLLATE NOCASE AND p.head_ref = ? AND p.outside = 0
			  AND p.forge_id IN (SELECT forge_id FROM project_repos WHERE repo = ? COLLATE NOCASE)
			  AND (p.state IN ('open', 'draft') OR (p.state = 'merged'
			    AND EXISTS (SELECT 1 FROM pr_events e WHERE e.pr_id = p.id AND e.kind = 'merged' AND e.created_at > ?)))
			  AND NOT EXISTS (SELECT 1 FROM pr_events e WHERE e.pr_id = p.id AND e.kind = 'pushed' AND e.created_at > ?)
			ORDER BY p.id DESC LIMIT 1`, repo, c.Branch, repo, since, since).Scan(&u.pr, &u.number, &u.url, &state)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		u.merged = state == "merged"
		ids, err := queryIDs(ctx, q, `SELECT DISTINCT l.issue_id FROM external_links l JOIN pull_requests p ON p.id = ?
			WHERE l.kind = 'pull' AND (l.url = p.url OR (l.repo = p.repo COLLATE NOCASE AND l.number = p.number)) ORDER BY l.issue_id`, u.pr)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			// No task links it: the one its branch names.
			for _, r := range branchRefs(c.Branch) {
				if id, err := s.resolveIssueID(ctx, q, r.Key); err == nil && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
		u.issues = ids
		out = append(out, u)
	}
	return out, nil
}

// what is "1 commit not pushed in `/path` on host, branch `x`".
func (u unpushedPR) what(machine string) string {
	w := &StrandedWork{Top: u.c.Top, Branch: u.c.Branch, Machine: machine}
	return fmt.Sprintf("%s not pushed in %s", countOf(u.c.Unpushed, "commit"), w.where())
}

// comment is what the pull request's tasks are told.
func (u unpushedPR) comment(machine, how string, at time.Time) string {
	var b strings.Builder
	if u.merged {
		fmt.Fprintf(&b, "Pull request #%d merged without commits its session left: %s, when %s (last seen %s).\n\n", u.number, u.what(machine), how, strandedTime(at))
		fmt.Fprintf(&b, "They need a pull request of their own: from that checkout, put them on a new branch off the default branch and `wp ship`. `git -C %s log @{u}..HEAD` lists them.", u.c.Top)
		return b.String()
	}
	// What Waypoint knows is the checkout at the session's last Stop and the
	// pushes the forge has reported since: a push it hasn't heard of yet is
	// possible, so the comment says what was seen.
	fmt.Fprintf(&b, "Pull request #%d hadn't got commits its session left: %s, when %s (last seen %s), and no push has reached it since.\n\n", u.number, u.what(machine), how, strandedTime(at))
	fmt.Fprintf(&b, "It merges without them unless they are pushed: `git -C %s push` (nothing to do if they already were).", u.c.Top)
	return b.String()
}

// tellUnpushed records, once per session and branch, that the session went
// away leaving commits on a pull request's branch: a timeline entry, a
// comment on the pull request's tasks, and a notification for its person.
// how says how it went away ("its session ended").
func (s *Service) tellUnpushed(ctx context.Context, t *tx, x *AgentSession, ws workState, how string) error {
	found, err := s.unpushedOnPRs(ctx, t, x.Repo, ws.Checkouts, ws.At)
	if err != nil || len(found) == 0 {
		return err
	}
	sys := SystemActor()
	for _, u := range found {
		if told, err := s.sessionTold(ctx, t, x.ID, "unpushed", "", u.c.Branch); err != nil || told {
			if err != nil {
				return err
			}
			continue
		}
		detail := fmt.Sprintf("%s on pull request #%d's branch", countOf(u.c.Unpushed, "commit"), u.number)
		if err := s.sessionTimeline(ctx, t, x.ID, s.nowMS(), "unpushed", "pr", u.pr, u.c.Branch, u.url, detail, x.projectID,
			map[string]any{"top": u.c.Top, "unpushed": u.c.Unpushed, "merged": u.merged}); err != nil {
			return err
		}
		body := u.comment(x.Machine, how, msTime(ws.At))
		// heard is the tasks whose watchers hear of the comment: a draft's
		// sets nothing off until a person publishes it (WP-42).
		var keys []string
		var heard []int64
		var first *Issue
		for _, id := range u.issues {
			i, err := s.issueByID(ctx, t, id, false)
			if errors.Is(err, ErrNotFound) {
				continue // deleted since it was linked
			}
			if err != nil {
				return err
			}
			if _, err := s.addLocalComment(ctx, t, sys, i, body); err != nil {
				return err
			}
			if i.Draft {
				continue
			}
			keys, heard = append(keys, i.Key), append(heard, i.ID)
			if first == nil {
				first = i
			}
		}
		if x.userID == 0 {
			continue
		}
		e := &Event{Kind: EvUnpushedLeft, UserID: x.userID, New: body,
			Data: map[string]any{"session": x.ID, "pr": u.number, "issues": heard, "keys": keys}}
		if first != nil {
			e.IssueID, e.IssueKey, e.ProjectID = first.ID, first.Key, first.ProjectID
		}
		t.emit(e)
	}
	return nil
}

// notifyUnpushed tells the session's person, unless they watch a task the
// comment went to (they heard of it from there).
func (s *Service) notifyUnpushed(ctx context.Context, e *Event) {
	if ids, ok := e.Data["issues"].([]int64); ok {
		for _, id := range ids {
			if slices.Contains(s.watchers(ctx, id), e.UserID) {
				return
			}
		}
	}
	title := fmt.Sprintf("Commits left unpushed for pull request #%d", dataInt(e, "pr"))
	if e.IssueID != 0 {
		s.notify(ctx, e.UserID, e, NotifyUnpushed, title, e.New)
		return
	}
	s.notifyURL(ctx, e.UserID, e, NotifyUnpushed, title, e.New, "/agents/sessions/"+itoa64(dataInt(e, "session")))
}

// unpushedHere is the line a session starting in a checkout hears of commits
// there that its branch's pull request hasn't got; "" when there are none.
func (s *Service) unpushedHere(ctx context.Context, q queryer, repo string, list []HookCheckout, now int64) string {
	found, err := s.unpushedOnPRs(ctx, q, repo, list, now)
	if err != nil {
		s.log.WarnContext(ctx, "unpushed at session start", "repo", repo, "err", err)
		return ""
	}
	if len(found) == 0 {
		return ""
	}
	u := found[0]
	if u.merged {
		return ""
	}
	return fmt.Sprintf("Branch `%s` in `%s` has %s that pull request #%d hasn't got: it merges without them unless they are pushed (`git -C %s push`).",
		u.c.Branch, u.c.Top, countOf(u.c.Unpushed, "commit"), u.number, u.c.Top)
}

// sweepUnpushed tells of the commits sessions left on pull requests'
// branches when they went away with no SessionEnd (or their end couldn't
// tell): sessions gone for unpushedGrace whose pull request has had nothing
// pushed since their last Stop.
func (s *Service) sweepUnpushed(ctx context.Context) error {
	now := s.nowMS()
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM agent_sessions
		WHERE last_seen_at BETWEEN ? AND ? AND work_state LIKE '%"unpushed":%' AND repo != ''
		AND NOT EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = agent_sessions.id AND e.kind = 'unpushed')
		ORDER BY id DESC LIMIT 500`, now-unpushedLookback.Milliseconds(), now-unpushedGrace.Milliseconds())
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, id), now)
			if err != nil {
				return err
			}
			if sessionStillAbout(x.stored, x.LastSeenAt.UnixMilli(), now) {
				return nil
			}
			ws := s.loadWorkState(ctx, t, x.ID)
			if ws.At == 0 {
				return nil
			}
			how := "its session ended"
			if x.stored != SessionEnded {
				how = "its session went away without ending"
			}
			return s.tellUnpushed(ctx, t, x, ws, how)
		})
		if err != nil {
			s.log.WarnContext(ctx, "unpushed sweep", "session", id, "err", err)
		}
	}
	return nil
}

// endedHow is how a session's end is told: "its session was cleared (/clear)".
func endedHow(reason string) string {
	if reason == "" {
		return "its session ended"
	}
	return strandedWhy(reason)
}
