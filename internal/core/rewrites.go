package core

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Rewritten work (WP-782, WP-ADR131): when a change replaces lines another
// task's pull request put on the base branch days before, the two tasks may
// pull against each other (WP-232 rewrote half of what WP-233 had merged
// hours earlier) or the later one may supersede the earlier. Which it is
// takes reading both; that it happened is a fact git blame gives for free.
// wp ship blames the lines the branch replaces and asks here which tasks
// those pull requests were for; the agent is told, and records supersedes or
// tension when that is what it did. Words alone don't say it reliably: a
// lexical stance reader and "filed together" both measured too weak to show
// (the decision note has the figures).

const (
	// rewriteMaxAge: work merged longer ago than this is the base the change
	// builds on, not someone's recent intent. Every rewrite measured
	// (2026-11-03..17) fell within it.
	rewriteMaxAge = 14 * 24 * time.Hour
	// A task's recent lines count when the change replaces at least
	// rewriteMinLines of them making up rewriteMinShare of what it replaces,
	// or rewriteBigLines whatever their share, summed over its pull
	// requests (measured per pull request: no task had two in one ship). Over 260 merges: 2-line
	// touches are half of all pairs, these keep about one ship in five.
	rewriteMinLines = 15
	rewriteMinShare = 0.25
	rewriteBigLines = 60
	rewriteMaxPRs   = 50 // read from one ship
	rewriteMaxShown = 5
)

// RewrittenPR is one pull request whose lines a change replaces, as git
// blame found it (codeindex.FixIntroducer).
type RewrittenPR struct {
	Number int     `json:"number"`
	Lines  int     `json:"lines"`
	Share  float64 `json:"share"` // of the lines the change replaces
	At     int64   `json:"at"`    // ms, when it reached the base
}

// RewrittenTask is another task whose recent work a change replaces, with
// nothing yet saying how the two tasks stand.
type RewrittenTask struct {
	Key   string  `json:"key"`
	Title string  `json:"title"`
	PR    int     `json:"pr"`    // its pull request with the most lines replaced
	PRs   int     `json:"prs"`   // how many of its pull requests the lines are from
	Lines int     `json:"lines"` // over all its recent pull requests
	Share float64 `json:"share"`
	At    int64   `json:"at"` // when PR merged
}

// rewriteRecent says whether a pull request reached the base recently
// enough for its lines to be someone's recent intent.
func rewriteRecent(p RewrittenPR, now time.Time) bool {
	return p.Number > 0 && p.At > 0 && now.Sub(time.UnixMilli(p.At)) <= rewriteMaxAge
}

// rewriteCounts says whether a task's replaced lines, over all its recent
// pull requests, are enough to mention.
func rewriteCounts(lines int, share float64) bool {
	return lines >= rewriteBigLines || (lines >= rewriteMinLines && share >= rewriteMinShare)
}

// RewrittenWork lists the tasks behind the pull requests in repo whose
// lines the change for task ref replaces, leaving out the task itself, its
// parent and subtasks, tasks already related to it in any way, and tasks
// the actor can't see. Tasks whose replaced lines are below the bar are
// passed over.
func (s *Service) RewrittenWork(ctx context.Context, actor *Actor, ref, repo string, prs []RewrittenPR) ([]*RewrittenTask, error) {
	d, err := s.GetIssue(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	out := []*RewrittenTask{}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return out, nil
	}
	skip := map[int64]bool{d.ID: true}
	for _, r := range d.Relations {
		skip[r.Issue.ID] = true
	}
	if d.Parent != nil {
		skip[d.Parent.ID] = true
	}
	for _, c := range d.Children {
		skip[c.ID] = true
	}
	now := s.now()
	byTask := map[int64]*RewrittenTask{}
	top := map[int64]int{} // lines of the pull request each task is named by
	for i, p := range prs {
		if i == rewriteMaxPRs {
			break
		}
		if !rewriteRecent(p, now) {
			continue
		}
		ids, err := s.pullTasks(ctx, repo, p.Number)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if skip[id] {
				continue
			}
			if t := byTask[id]; t != nil {
				// One task's work spread over several pull requests is
				// judged whole; the one named is the largest.
				if p.Lines > top[id] {
					t.PR, t.At, top[id] = p.Number, p.At, p.Lines
				}
				t.Lines += p.Lines
				t.PRs++
				t.Share += p.Share
				continue
			}
			ci, err := s.issueByID(ctx, s.db.R, id, false)
			if err != nil || !s.canSee(ctx, s.db.R, actor, id, ci.ProjectID) {
				skip[id] = true
				continue
			}
			byTask[id] = &RewrittenTask{Key: ci.Key, Title: ci.Title, PR: p.Number, PRs: 1, Lines: p.Lines, Share: p.Share, At: p.At}
			top[id] = p.Lines
		}
	}
	for _, t := range byTask {
		if rewriteCounts(t.Lines, t.Share) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lines != out[j].Lines {
			return out[i].Lines > out[j].Lines
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > rewriteMaxShown {
		out = out[:rewriteMaxShown]
	}
	return out, nil
}

// pullTasks are the tasks linked to pull request number in repo, by its
// repo and number or by its URL.
func (s *Service) pullTasks(ctx context.Context, repo string, number int) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT el.issue_id FROM external_links el
		WHERE el.kind = 'pull' AND ((el.repo = ? COLLATE NOCASE AND el.number = ?)
			OR el.url IN (SELECT p.url FROM pull_requests p WHERE p.repo = ? COLLATE NOCASE AND p.number = ?))
		ORDER BY el.issue_id`, repo, number, repo, number)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
