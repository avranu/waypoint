package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// TaskUsageRound is one adversarial review round of a task, as the work log
// counted it; Found, Fixed and Rejected are nil for a round nobody counted.
type TaskUsageRound struct {
	At       time.Time `json:"at"`
	State    string    `json:"state"`
	Found    *int      `json:"found,omitempty"`
	Fixed    *int      `json:"fixed,omitempty"`
	Rejected *int      `json:"rejected,omitempty"`
}

// TaskUsageReviewer is the tokens the audits of a task's rounds used.
type TaskUsageReviewer struct {
	Audits int   `json:"audits"`
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// TaskUsagePR is a pull request of the task; MergedAt is nil until it merges.
type TaskUsagePR struct {
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	State    string     `json:"state"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
}

// taskReviewInto fills what is stored of a task's review rounds, the tokens
// of their audits and its pull requests (WP-1882). The rounds come from the
// work log's own reader.
func (s *Service) taskReviewInto(ctx context.Context, issueID int64, o *TaskUsage) error {
	wl := &WorkLog{}
	if err := s.storedWorkLog(ctx, wl, issueID); err != nil {
		return err
	}
	for _, e := range wl.Entries {
		if e.Kind == WorkReview {
			o.Rounds = append(o.Rounds, TaskUsageRound{At: e.At, State: e.State, Found: e.Found, Fixed: e.Fixed, Rejected: e.Rejected})
		}
	}
	// The audits of a round are keyed review-round:ID, with a suffix for the
	// check, the sonnet pass and the other model.
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.tokens FROM work_log w JOIN code_audits a
		ON a.request_key = 'review-round:' || w.id OR a.request_key LIKE 'review-round:' || w.id || ':%'
		WHERE w.kind = 'review' AND w.issue_id = ?`, issueID)
	if err != nil {
		return err
	}
	var r TaskUsageReviewer
	for rows.Next() {
		var tokens string
		if err := rows.Scan(&tokens); err != nil {
			_ = rows.Close()
			return err
		}
		var t map[string]any
		if json.Unmarshal([]byte(tokens), &t) != nil {
			continue
		}
		in, _ := anyFloat(t["input"])
		out, _ := anyFloat(t["output"])
		if in+out > 0 {
			r.Audits++
			r.Input += int64(in)
			r.Output += int64(out)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if r.Audits > 0 {
		o.Reviewer = &r
	}
	prs, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.repo, p.number, p.state, p.merged_at, p.created_at FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ? ORDER BY p.created_at, p.number`, issueID)
	if err != nil {
		return err
	}
	defer prs.Close()
	for prs.Next() {
		var p TaskUsagePR
		var merged sql.NullInt64
		var created int64
		if err := prs.Scan(&p.Repo, &p.Number, &p.State, &merged, &created); err != nil {
			return err
		}
		p.MergedAt = msTimePtr(merged)
		o.PullRequests = append(o.PullRequests, p)
	}
	return prs.Err()
}
