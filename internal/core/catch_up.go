package core

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	catchUpMax        = 100
	catchUpShots      = 12
	catchUpSummaryMax = 400
	tryLinkMax        = 2000
	tryLinksPerIssue  = 20
	problemTitleMax   = 100
	notFinishedMax    = 20000
	catchUpLiveDays   = 30
)

// CatchUp is the finished work waiting for a person's look.
type CatchUp struct {
	Items []*CatchUpItem `json:"items"`
	// Total counts all of it; Items holds the oldest catchUpMax.
	Total int `json:"total"`
}

// CatchUpItem is one finished task.
type CatchUpItem struct {
	Issue    IssueRef `json:"issue"`
	Project  string   `json:"project"`
	Status   string   `json:"status"`
	Category string   `json:"category"` // done or canceled
	// Agent is whose work it is: the agent that closed it, or the agent of
	// the work an automation closed it for.
	Agent      string    `json:"agent"`
	ResolvedAt time.Time `json:"resolved_at"`
	// Summary is the agent's last comment on it, shortened.
	Summary string       `json:"summary"`
	PR      *CatchUpPR   `json:"pr,omitempty"`
	Shots   []Attachment `json:"shots"`
	Try     []TryLink    `json:"try"`
	// Live is where its pull request stands (a delivery stage: live,
	// merged, review, stuck, untracked); LiveIn names the environments it
	// runs in. Both empty when Waypoint can't tell.
	Live   string   `json:"live,omitempty"`
	LiveIn []string `json:"live_in,omitempty"`
}

// CatchUpPR is the pull request a task went out on.
type CatchUpPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// TryLink is a page where the change shows.
type TryLink struct {
	ID int64 `json:"id"`
	// URL is where to open it: Raw itself, or Raw completed with the
	// project's environment URL; "" when a path has nothing to complete it.
	URL string `json:"url"`
	Raw string `json:"raw"`
}

// CatchUp lists the closes waiting for a person's review, oldest first, in
// one project (a key) or everywhere the actor can see.
func (s *Service) CatchUp(ctx context.Context, actor *Actor, project string) (*CatchUp, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if err := actor.need(PermIssuesRead); err != nil {
		return nil, err
	}
	cond, args := "", []any{}
	if project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		cond, args = " AND i.project_id = ?", []any{p.ID}
	}
	q := s.db.R
	// #nosec G202 -- the conditions are fixed SQL from this package; values are bound
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.project_id, p.key, p.key || '-' || i.number, i.title, i.type, st.name, st.category, i.resolved_agent, i.resolved_at
		FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE `+closeUnreviewed("i")+` AND `+draftsHidden(actor, "i")+cond+` ORDER BY i.resolved_at, i.id`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		it  *CatchUpItem
		pid int64
	}
	var all []row
	for rows.Next() {
		it := &CatchUpItem{Shots: []Attachment{}, Try: []TryLink{}}
		var pid, at int64
		if err := rows.Scan(&it.Issue.ID, &pid, &it.Project, &it.Issue.Key, &it.Issue.Title, &it.Issue.Type, &it.Status, &it.Category, &it.Agent, &at); err != nil {
			rows.Close()
			return nil, err
		}
		it.ResolvedAt = msTime(at)
		all = append(all, row{it, pid})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := &CatchUp{Items: []*CatchUpItem{}}
	for _, r := range all {
		if !s.canSee(ctx, q, actor, r.it.Issue.ID, r.pid) {
			continue
		}
		out.Total++
		if len(out.Items) < catchUpMax {
			out.Items = append(out.Items, r.it)
		}
	}
	if len(out.Items) == 0 {
		return out, nil
	}
	envURLs := s.tryBases(ctx, actor, project)
	for _, it := range out.Items {
		if err := s.fillCatchUp(ctx, q, it, envURLs[it.Project]); err != nil {
			return nil, err
		}
	}
	s.catchUpLive(ctx, actor, project, out.Items)
	return out, nil
}

// fillCatchUp adds a task's summary, pull request, screenshots and try links.
func (s *Service) fillCatchUp(ctx context.Context, q queryer, it *CatchUpItem, base string) error {
	var body string
	err := q.QueryRowContext(ctx, `SELECT body FROM comments WHERE issue_id = ? AND agent != '' ORDER BY created_at DESC, id DESC LIMIT 1`, it.Issue.ID).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	it.Summary = clipRunes(strings.TrimSpace(body), catchUpSummaryMax)
	var pr CatchUpPR
	var num sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT repo, number, title, url, state FROM external_links
		WHERE issue_id = ? AND kind = 'pull' ORDER BY state = 'merged' DESC, id DESC LIMIT 1`, it.Issue.ID).Scan(&pr.Repo, &num, &pr.Title, &pr.URL, &pr.State)
	switch {
	case err == nil:
		pr.Number = int(num.Int64)
		it.PR = &pr
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.target_type = ? AND a.target_id = ? AND a.content_type IN ('image/png','image/jpeg','image/gif','image/webp')
		ORDER BY a.review DESC, a.id DESC LIMIT ?`, TargetIssue, it.Issue.ID, catchUpShots)
	if err != nil {
		return err
	}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			rows.Close()
			return err
		}
		it.Shots = append(it.Shots, *a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(it.Shots, func(a, b int) bool {
		x, y := it.Shots[a], it.Shots[b]
		if x.Review != y.Review {
			return x.Review
		}
		return x.ID < y.ID
	})
	it.Try, err = s.tryLinks(ctx, q, it.Issue.ID, base)
	return err
}

// catchUpLive says where each task's pull request stands, from Delivery;
// when Waypoint can't tell (no permission, no environments), it says
// nothing.
func (s *Service) catchUpLive(ctx context.Context, actor *Actor, project string, items []*CatchUpItem) {
	d, err := s.Delivery(ctx, actor, DeliveryQuery{Project: project, Days: catchUpLiveDays})
	if err != nil {
		return
	}
	byIssue := map[int64]*DeliveryItem{}
	for _, di := range d.Items {
		for _, t := range di.Tasks {
			if byIssue[t.ID] == nil {
				byIssue[t.ID] = di
			}
		}
	}
	for _, it := range items {
		di := byIssue[it.Issue.ID]
		if di == nil || di.PR == nil {
			continue
		}
		it.Live = di.Stage
		for _, e := range di.Envs {
			if e.In {
				it.LiveIn = append(it.LiveIn, e.Name)
			}
		}
	}
}

// tryBases maps each project key to the URL its paths open at: its
// production environment's, else its staging one's.
func (s *Service) tryBases(ctx context.Context, actor *Actor, project string) map[string]string {
	out := map[string]string{}
	envs, err := s.ListEnvironments(ctx, actor, project, false)
	if err != nil {
		return out
	}
	for _, kind := range []string{EnvStaging, EnvProduction} {
		for _, e := range envs {
			if e.Kind == kind && webURL(e.URL) {
				out[e.Project] = strings.TrimRight(e.URL, "/")
			}
		}
	}
	return out
}

func webURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Service) tryLinks(ctx context.Context, q queryer, issueID int64, base string) ([]TryLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, url FROM try_links WHERE issue_id = ? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TryLink{}
	for rows.Next() {
		var l TryLink
		if err := rows.Scan(&l.ID, &l.Raw); err != nil {
			return nil, err
		}
		switch {
		case webURL(l.Raw):
			l.URL = l.Raw
		case base != "":
			l.URL = base + l.Raw
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// normTryLink checks a try link: an http(s) URL, or a path from the site's
// root.
func normTryLink(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", invalid("which page? Give a URL or a path like /live")
	case len(raw) > tryLinkMax:
		return "", invalid("a try link is at most %d characters", tryLinkMax)
	case webURL(raw):
		return raw, nil
	case strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//"):
		if _, err := url.Parse(raw); err != nil {
			return "", invalid("%q isn't a path", raw)
		}
		return raw, nil
	}
	return "", invalid("a try link is an http(s) URL or a path that starts with /, not %q", raw)
}

// AddTryLink records a page where an issue's change shows (WP-245).
func (s *Service) AddTryLink(ctx context.Context, actor *Actor, ref, raw string) (*TryLink, error) {
	link, err := normTryLink(raw)
	if err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	out := &TryLink{Raw: link}
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if err := t.QueryRowContext(ctx, `SELECT id FROM try_links WHERE issue_id = ? AND url = ?`, i.ID, link).Scan(&out.ID); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM try_links WHERE issue_id = ?`, i.ID).Scan(&n); err != nil {
			return err
		}
		if n >= tryLinksPerIssue {
			return invalid("%s has %d try links already", i.Key, n)
		}
		res, err := t.ExecContext(ctx, `INSERT INTO try_links(issue_id, url, user_id, agent, created_at) VALUES (?,?,?,?,?)`,
			i.ID, link, nullUser(actor.id()), actor.agentName(), s.nowMS())
		if err != nil {
			return err
		}
		out.ID, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "try_link", New: link})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if webURL(link) {
		out.URL = link
	}
	return out, nil
}

// NotFinished reopens a task a person found unfinished, with their note
// as a comment (WP-245). Reopening clears the close's review.
func (s *Service) NotFinished(ctx context.Context, actor *Actor, ref, note string) (*IssueDetail, error) {
	if err := actor.requireHuman("sending finished work back"); err != nil {
		return nil, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	note = strings.TrimSpace(note)
	if len(note) > notFinishedMax {
		return nil, invalid("the note is at most %d characters", notFinishedMax)
	}
	var key string
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if !i.Closed() {
			return invalid("%s is %s: it isn't closed", i.Key, i.Status.Name)
		}
		key = i.Key
		todo := CatTodo
		if err := s.applyPatch(ctx, t, actor, i, IssuePatch{Status: &todo}); err != nil {
			return err
		}
		body := "Not finished (from Catch up)."
		if note != "" {
			body = "Not finished: " + note
		}
		_, err = s.addComment(ctx, t, actor, i, body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, key)
}

// ReportProblem files a bug about a finished task, related to it, from a
// person's note (WP-245). The note may be empty: a screenshot attached to
// the new bug can say it all.
func (s *Service) ReportProblem(ctx context.Context, actor *Actor, ref, note string) (*IssueDetail, error) {
	if err := actor.requireHuman("reporting a problem from Catch up"); err != nil {
		return nil, err
	}
	note = strings.TrimSpace(note)
	if len(note) > notFinishedMax {
		return nil, invalid("the note is at most %d characters", notFinishedMax)
	}
	of, err := s.GetIssue(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	title := clipRunes(firstLineOf(note), problemTitleMax)
	if title == "" {
		title = clipRunes("Problem with "+of.Key+": "+of.Title, problemTitleMax)
	}
	desc := "Found while checking " + of.Key + " (" + of.Title + ")."
	if note != "" {
		desc = note + "\n\n" + desc
	}
	d, err := s.CreateIssue(ctx, actor, IssueInput{Project: of.ProjectKey, Type: "bug", Title: title, Description: desc})
	if err != nil {
		return nil, err
	}
	if err := s.AddRelation(ctx, actor, d.Key, "relates", of.Key); err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, d.Key)
}
