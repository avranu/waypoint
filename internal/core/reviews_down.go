package core

import (
	"context"
	"fmt"
)

// EvReviewsDown: a project's reviews keep ending with every model
// unavailable, or one answered again after that (WP-214). New: down or up.
// Data: streak plus the latest run's pull request (prEventData).
const EvReviewsDown = "bot.reviews_down"

// reviewsDownAfter is how many reviews in a row must end with every model
// unavailable before someone is told: one is a blip the 😕 on the pull
// request covers, three is an outage or a spent pool.
const reviewsDownAfter = 3

// providersDownText starts every error a run records when every model was
// unavailable (runErrorText, errProvidersDown), so the streak can find them.
const providersDownText = "every review model "

// reviewsDownCheck runs where a review run ends (endBotRun, same
// transaction): when this run makes reviewsDownAfter in a row for its
// project that ended with every model unavailable, or is the first to
// answer after such a streak, it emits EvReviewsDown. Once per streak: the
// notice goes out on the run that reaches the count, not the ones after.
func (s *Service) reviewsDownCheck(ctx context.Context, t *tx, projectID, prID, runID int64) {
	rows, err := t.QueryContext(ctx, `SELECT status = 'failed' FROM bot_runs
		WHERE project_id = ? AND kind = 'review' AND finished_at IS NOT NULL
		AND (status = 'done' OR (status = 'failed' AND substr(error, 1, ?) = ?))
		ORDER BY finished_at DESC, id = ? DESC, id DESC LIMIT ?`, projectID, len(providersDownText), providersDownText, runID, reviewsDownAfter+1)
	if err != nil {
		s.log.WarnContext(ctx, "read the review streak", "project", projectID, "err", err)
		return
	}
	var down []bool
	for rows.Next() {
		var d bool
		if rows.Scan(&d) == nil {
			down = append(down, d)
		}
	}
	_ = rows.Close()
	state := reviewsDownState(down)
	if state == "" {
		return
	}
	d := map[string]any{"streak": reviewsDownAfter}
	if pr, err := s.prByID(ctx, t, prID); err == nil {
		d = prEventData(pr, d)
	}
	t.emit(&Event{Kind: EvReviewsDown, ProjectID: projectID, New: state, Data: d})
}

// reviewsDownState says what the latest reviews that either answered or
// ended with every model unavailable (newest first, true = unavailable)
// call for: "down" when the newest reviewsDownAfter are all down and the
// one before them answered, "up" when the newest answered after
// reviewsDownAfter down, else "".
func reviewsDownState(down []bool) string {
	all := func(xs []bool) bool {
		for _, x := range xs {
			if !x {
				return false
			}
		}
		return true
	}
	n := reviewsDownAfter
	switch {
	case len(down) >= n && all(down[:n]) && (len(down) == n || !down[n]):
		return "down"
	case len(down) > n && !down[0] && all(down[1:n+1]):
		return "up"
	}
	return ""
}

// notifyReviewsDown tells the site admins and the project's owners that
// reviews keep failing with every model unavailable, and again when one
// answers. Nothing retries those runs (WP-215), so without this nobody hears
// of the outage but from a 😕 on each pull request.
func (s *Service) notifyReviewsDown(ctx context.Context, e *Event) {
	var key string
	_ = s.db.R.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, e.ProjectID).Scan(&key)
	if key == "" {
		key = "a project"
	}
	latest := ""
	if repo, _ := e.Data["repo"].(string); repo != "" && dataInt(e, "number") > 0 {
		latest = fmt.Sprintf(" (latest: %s#%d)", repo, dataInt(e, "number"))
	}
	var title, body string
	switch e.New {
	case "down":
		title = fmt.Sprintf("Reviews in %s keep failing: every model unavailable", key)
		body = fmt.Sprintf("The last %d pull request reviews%s ended with every review model rate limited, down, timing out, or none at the review minimum (%s) able to take it. Each was tried again 10 and 20 minutes later before it counted, except one where no model met the review minimum, which is not retried. Once a model answers, comment @waypoint review on each (or wp pr review retry), or change the review rule's models.", reviewsDownAfter, latest, reviewFloorBar)
	case "up":
		title = fmt.Sprintf("Reviews in %s answer again", key)
		body = fmt.Sprintf("A review%s finished after %d or more in a row failed with every model unavailable. Pull requests whose review failed meanwhile still have none: comment @waypoint review on each (or wp pr review retry).", latest, reviewsDownAfter)
	default:
		return
	}
	link := "/agents?tab=pulls"
	if id := dataInt(e, "pr_id"); id > 0 {
		link = fmt.Sprintf("/agents?tab=pulls&pr=%d", id)
	}
	for _, id := range s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0
		UNION SELECT user_id FROM project_members WHERE project_id = ? AND role = 'owner'`, e.ProjectID) {
		s.notifyURL(ctx, id, nil, NotifyPR, title, body, link)
	}
}
