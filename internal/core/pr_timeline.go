package core

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// PRTimeline is one pull request's ship-to-merge timeline: its pushes, the
// merge being armed, every CI run of its heads and what held it back, so the
// time spent waiting for a runner, running CI and held after green can be read
// without re-running anything (WP-1806).
type PRTimeline struct {
	OpenedAt time.Time        `json:"opened_at"`
	MergedAt *time.Time       `json:"merged_at,omitempty"`
	Pushes   []PRPush         `json:"pushes"`
	Armed    []PRArmEvent     `json:"armed"`
	Runs     []PRRun          `json:"runs"`
	Holds    []PRTimelineHold `json:"holds"`
	Totals   PRTimelineTotals `json:"totals"`
}

type PRPush struct {
	At   time.Time `json:"at"`
	Head string    `json:"head,omitempty"`
}

// PRArmEvent is the merge being armed, disarmed or done, from the audit log.
type PRArmEvent struct {
	At   time.Time `json:"at"`
	What string    `json:"what"` // arm, disarm, merge
	By   string    `json:"by,omitempty"`
}

// PRRun is a finished CI run of one of the pull request's heads.
type PRRun struct {
	Number    int64      `json:"number"`
	Workflow  string     `json:"workflow"`
	Event     string     `json:"event"`
	Tier      string     `json:"tier,omitempty"`
	Stack     bool       `json:"stack,omitempty"` // the merge queue's run of a stack this pull request rode
	Head      string     `json:"head"`
	Status    string     `json:"status"`
	QueuedAt  time.Time  `json:"queued_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	WaitS     *int64     `json:"wait_s,omitempty"`
	RunS      *int64     `json:"run_s,omitempty"`
}

type PRTimelineHold struct {
	What  string     `json:"what"`
	Head  string     `json:"head"`
	From  time.Time  `json:"from"`
	Until *time.Time `json:"until,omitempty"`
}

type PRTimelineTotals struct {
	OpenToMergedS *int64 `json:"open_to_merged_s,omitempty"`
	CIWaitS       int64  `json:"ci_wait_s"`
	CIRunS        int64  `json:"ci_run_s"`
	HeldS         int64  `json:"held_s"`
}

// PRTimeline reads a pull request's timeline, with the access check
// GetPullRequest has: the project's viewers, for pull requests they can see.
func (s *Service) PRTimeline(ctx context.Context, actor *Actor, projectKey, repo string, number int) (*PRTimeline, error) {
	id, err := s.FindPullRequest(ctx, actor, projectKey, repo, number)
	if err != nil {
		return nil, err
	}
	var forge int64
	var prRepo string
	var opened, merged sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, repo, coalesce(opened_at, created_at), merged_at FROM pull_requests WHERE id = ?`, id).Scan(&forge, &prRepo, &opened, &merged); err != nil {
		return nil, err
	}
	tl := &PRTimeline{OpenedAt: msTime(opened.Int64), Pushes: []PRPush{}, Armed: []PRArmEvent{}, Runs: []PRRun{}, Holds: []PRTimelineHold{}}
	if merged.Valid {
		m := msTime(merged.Int64)
		tl.MergedAt = &m
		d := (merged.Int64 - opened.Int64) / 1000
		tl.Totals.OpenToMergedS = &d
	}

	// A run belongs to the pull request when it ran on one of the heads the
	// pull request had (opened or pushed events keep each) or on its own ref,
	// as ci_doctor's fullRunOf matches by head and leaves branch pushes out,
	// or on the commit of a merge queue stack it rode (a push to a wp-queue/
	// branch, which a batch's pull requests share).
	heads := map[string]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, sha, created_at FROM pr_events WHERE pr_id = ? AND kind IN ('opened', 'pushed') ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, sha string
		var at int64
		if err := rows.Scan(&kind, &sha, &at); err != nil {
			return nil, err
		}
		if sha != "" {
			heads[sha] = true
		}
		if kind == "pushed" {
			tl.Pushes = append(tl.Pushes, PRPush{At: msTime(at), Head: short(sha)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cur string
	if err := s.db.R.QueryRowContext(ctx, `SELECT head_sha FROM pull_requests WHERE id = ?`, id).Scan(&cur); err != nil {
		return nil, err
	}
	if cur != "" {
		heads[cur] = true
	}

	stacks := map[string]bool{}
	st, err := s.db.R.QueryContext(ctx, `SELECT commit_sha FROM queue_stacks WHERE pr_id = ? AND commit_sha != ''`, id)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	for st.Next() {
		var sha string
		if err := st.Scan(&sha); err != nil {
			return nil, err
		}
		stacks[sha] = true
	}
	if err := st.Err(); err != nil {
		return nil, err
	}

	arm, err := s.db.R.QueryContext(ctx, `SELECT a.action, a.created_at, coalesce(nullif(u.username, ''), a.actor)
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.target_type = 'pr' AND a.target_id = ? AND a.action IN ('pr.arm', 'pr.disarm', 'pr.merge') ORDER BY a.created_at, a.id`, id)
	if err != nil {
		return nil, err
	}
	defer arm.Close()
	for arm.Next() {
		var action string
		var at int64
		var by sql.NullString
		if err := arm.Scan(&action, &at, &by); err != nil {
			return nil, err
		}
		tl.Armed = append(tl.Armed, PRArmEvent{At: msTime(at), What: action[len("pr."):], By: by.String})
	}
	if err := arm.Err(); err != nil {
		return nil, err
	}

	runs, err := s.db.R.QueryContext(ctx, `SELECT number, workflow, event, ref, sha, tier, status, created_at, started_at, stopped_at FROM ci_runs
		WHERE forge_id = ? AND repo = ? COLLATE NOCASE ORDER BY created_at, number`, forge, prRepo)
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	for runs.Next() {
		var r PRRun
		var ref, sha string
		var created int64
		var started, stopped sql.NullInt64
		if err := runs.Scan(&r.Number, &r.Workflow, &r.Event, &ref, &sha, &r.Tier, &r.Status, &created, &started, &stopped); err != nil {
			return nil, err
		}
		r.Stack = stacks[sha]
		own := heads[sha] && r.Event != "push" || prRunRef(ref, number)
		if !r.Stack && !own {
			continue
		}
		r.Head, r.QueuedAt = short(sha), msTime(created)
		if started.Valid {
			t := msTime(started.Int64)
			r.StartedAt = &t
			w := max(started.Int64-created, 0) / 1000
			r.WaitS = &w
			tl.Totals.CIWaitS += w
		}
		if stopped.Valid {
			t := msTime(stopped.Int64)
			r.StoppedAt = &t
		}
		if started.Valid && stopped.Valid {
			d := max(stopped.Int64-started.Int64, 0) / 1000
			r.RunS = &d
			tl.Totals.CIRunS += d
		}
		tl.Runs = append(tl.Runs, r)
	}
	if err := runs.Err(); err != nil {
		return nil, err
	}

	// Every hold, however short: prHolds' newest-8, minute-or-more cut is for
	// the detail page, not for adding up where a merge's time went.
	holds, err := s.db.R.QueryContext(ctx, `SELECT what, head_sha, first_at, cleared_at FROM merge_holds WHERE pr_id = ? ORDER BY first_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer holds.Close()
	type span struct{ from, until int64 }
	var spans []span
	now := s.nowMS()
	for holds.Next() {
		var h PRTimelineHold
		var from int64
		var until sql.NullInt64
		if err := holds.Scan(&h.What, &h.Head, &from, &until); err != nil {
			return nil, err
		}
		h.Head, h.From = short(h.Head), msTime(from)
		end := now
		if until.Valid {
			u := msTime(until.Int64)
			h.Until, end = &u, until.Int64
		}
		spans = append(spans, span{from, end})
		tl.Holds = append(tl.Holds, h)
	}
	if err := holds.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(spans, func(a, b span) int { return int(a.from - b.from) })
	var to int64
	for _, sp := range spans {
		from := max(sp.from, to)
		if sp.until > from {
			tl.Totals.HeldS += (sp.until - from) / 1000
		}
		to = max(to, sp.until)
	}
	return tl, nil
}

// prRunRef: a run made for the pull request itself, whose ref the forge
// names #N or refs/pull/N/head.
func prRunRef(ref string, number int) bool {
	return ref == fmt.Sprintf("#%d", number) || ref == fmt.Sprintf("refs/pull/%d/head", number)
}
