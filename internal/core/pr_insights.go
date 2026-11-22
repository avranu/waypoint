package core

// Pull requests on the Agents page (WP-D15): where each went, how many
// pushes it took, who reviewed and approved it, what bots found, and what
// agents spent on the issues it's linked to.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// PRQuery narrows the pull requests listed.
type PRQuery struct {
	Project string // a project key: its repos
	Agent   string // an agent spec: pull requests any of whose agents (or runs' models) match
	State   string // open (drafts too), merged, closed; "" for all
	Text    string // in the title, repo or number
	Repo    string // exactly this repo (owner/name), ignoring case
	Days    int    // the period: open ones, and ones opened, merged, closed or updated in it
}

// PRCounts splits a count between people and bots.
type PRCounts struct {
	People int `json:"people"`
	Bots   int `json:"bots"`
}

// PRIssue is an issue a pull request is linked to.
type PRIssue struct {
	ID       int64  `json:"id"`
	LinkID   int64  `json:"link_id"` // the issue's link to the pull request, for removing it
	Key      string `json:"key"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Category string `json:"category"`
	// Verb is the relationship a mention states (follows, part_of, relates,
	// ...): set on Mentions only.
	Verb string `json:"verb,omitempty"`
}

// PRSummary is a pull request with what happened to it.
type PRSummary struct {
	ID     int64  `json:"id"`
	Forge  string `json:"forge"` // github, forgejo, gitlab
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author string `json:"author"`
	// Outside: from someone who isn't a Waypoint person, waiting for a
	// person to approve it (WP-281); its title shows once approved.
	Outside          bool           `json:"outside,omitempty"`
	Agents           []string       `json:"agents"` // its text's agent, agents that linked it, agents that worked its issues
	State            string         `json:"state"`
	BaseRef          string         `json:"base_ref"`
	HeadRef          string         `json:"head_ref"`
	HeadSHA          string         `json:"head_sha,omitempty"`  // for "where it runs" (WP-D19)
	MergeSHA         string         `json:"merge_sha,omitempty"` // the commit it became, which its base's deployments run (WP-23)
	OpenedAt         *time.Time     `json:"opened_at"`
	MergedAt         *time.Time     `json:"merged_at"`
	ClosedAt         *time.Time     `json:"closed_at"`
	MergedBy         string         `json:"merged_by,omitempty"`
	HoursOpen        *float64       `json:"hours_open"` // to its merge or close, or until now
	Pushes           int            `json:"pushes"`     // new heads after it opened
	Comments         PRCounts       `json:"comments"`
	Reviews          PRCounts       `json:"reviews"`   // bots: Waypoint's reviews and other bots'
	Approvals        []string       `json:"approvals"` // people who approved it
	ChangesRequested int            `json:"changes_requested"`
	Findings         map[string]int `json:"findings"` // by severity: what bots raised and kept, posted or past the shown number (WP-1421)
	Issues           []PRIssue      `json:"issues"`
	// Mentions are issues its text or comments name without it being their
	// work, so not linked (WP-379).
	Mentions     []PRIssue `json:"mentions,omitempty"`
	AgentSeconds int64     `json:"agent_seconds"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	Additions    *int      `json:"additions"`
	Deletions    *int      `json:"deletions"`
	ChangedFiles *int      `json:"changed_files"`
	Commits      *int      `json:"commits"`
	LastActivity time.Time `json:"last_activity"`
	// Where it stands for merging (WP-D21): CI on its head (pending, passed,
	// failed, none), the review gate (pending, success, failure) and whether
	// it is set to merge when ready.
	CI    string `json:"ci,omitempty"`
	Gate  string `json:"gate,omitempty"`
	Armed bool   `json:"armed,omitempty"`
	// WipAuto: Waypoint opened it as a draft and marks it ready (WP-158).
	WipAuto bool `json:"wip_auto,omitempty"`
	// Urgent: it carries the ci-urgent label, so its CI runs on the pool's
	// urgent lane ahead of the queue (WP-528).
	Urgent bool `json:"urgent,omitempty"`
	models []string
}

// PRTotals sums up the pull requests listed.
type PRTotals struct {
	Opened             int      `json:"opened"` // in the period
	Merged             int      `json:"merged"`
	Closed             int      `json:"closed"` // without merging
	OpenNow            int      `json:"open_now"`
	MedianHoursToMerge *float64 `json:"median_hours_to_merge"`
	BotReviews         int      `json:"bot_reviews"`
	Findings           int      `json:"findings"`
	HumanApprovals     int      `json:"human_approvals"`
}

// PRList is the Pull requests tab.
type PRList struct {
	Totals PRTotals     `json:"totals"`
	Pulls  []*PRSummary `json:"pulls"`
}

const maxPRList = 300

// prVisible limits pull requests p to repos attached to projects the actor
// can see (and to one project's repos, when projectID is set).
func prVisible(actor *Actor, projectID int64) (string, []any) {
	vis, args := visibleSQL("r.project_id", actor)
	cond := `EXISTS (SELECT 1 FROM project_repos r WHERE r.forge_id = p.forge_id AND r.repo = p.repo COLLATE NOCASE AND ` + vis
	if actor.HidesDrafts() {
		cond = `p.outside = 0 AND ` + cond // from outside: people only (WP-281)
	}
	if projectID != 0 {
		cond += ` AND r.project_id = ?`
		args = append(args, projectID)
	}
	return cond + `)`, args
}

func prColsAs(alias string) string {
	cols := strings.Split(prCols, ",")
	for n, c := range cols {
		cols[n] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// ListPullRequests lists pull requests, most recently active first, with
// what happened to each.
func (s *Service) ListPullRequests(ctx context.Context, actor *Actor, q PRQuery) (*PRList, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var projectID int64
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projectID = p.ID
	}
	if q.Days <= 0 || q.Days > 366 {
		q.Days = 30
	}
	since := s.now().AddDate(0, 0, -q.Days).UnixMilli()
	vis, args := prVisible(actor, projectID)
	where := []string{vis, `(p.state IN ('open','draft') OR coalesce(p.merged_at, 0) >= ? OR coalesce(p.closed_at, 0) >= ?
		OR coalesce(p.opened_at, p.created_at) >= ? OR p.updated_at >= ?)`}
	args = append(args, since, since, since, since)
	switch q.State {
	case "", "all":
	case "open":
		where = append(where, `p.state IN ('open','draft')`)
	case "merged", "closed":
		where = append(where, `p.state = ?`)
		args = append(args, q.State)
	default:
		return nil, invalid("state must be open, merged or closed")
	}
	if r := strings.TrimSpace(q.Repo); r != "" {
		where = append(where, `p.repo = ? COLLATE NOCASE`)
		args = append(args, r)
	}
	if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(q.Text), "#")); t != "" {
		where = append(where, `(p.title LIKE ? ESCAPE '\' OR p.repo LIKE ? ESCAPE '\' OR CAST(p.number AS TEXT) = ? OR p.author = ? COLLATE NOCASE)`)
		like := "%" + likeEscape(t) + "%"
		args = append(args, like, like, t, t)
	}
	// #nosec G202 -- fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+`, f.kind FROM pull_requests p JOIN forges f ON f.id = p.forge_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY max(p.updated_at, coalesce(p.merged_at, 0), coalesce(p.closed_at, 0), coalesce(p.opened_at, 0)) DESC LIMIT ?`,
		append(args, maxPRList)...)
	if err != nil {
		return nil, err
	}
	var prs []*PRSummary
	for rows.Next() {
		sm, err := scanPRSummary(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		prs = append(prs, sm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillPRs(ctx, actor, prs); err != nil {
		return nil, err
	}
	out := &PRList{Pulls: []*PRSummary{}}
	var mergeHours []float64
	for _, p := range prs {
		if q.Agent != "" && !prMatchesAgent(p, q.Agent) {
			continue
		}
		out.Pulls = append(out.Pulls, p)
		t := &out.Totals
		if p.OpenedAt != nil && p.OpenedAt.UnixMilli() >= since {
			t.Opened++
		}
		switch {
		case p.State == "merged" && p.MergedAt != nil && p.MergedAt.UnixMilli() >= since:
			t.Merged++
			if p.HoursOpen != nil {
				mergeHours = append(mergeHours, *p.HoursOpen)
			}
		case p.State == "closed" && p.ClosedAt != nil && p.ClosedAt.UnixMilli() >= since:
			t.Closed++
		case p.State == "open" || p.State == "draft":
			t.OpenNow++
		}
		t.BotReviews += p.Reviews.Bots
		t.HumanApprovals += len(p.Approvals)
		for _, n := range p.Findings {
			t.Findings += n
		}
	}
	if len(mergeHours) > 0 {
		sort.Float64s(mergeHours)
		m := mergeHours[len(mergeHours)/2]
		if len(mergeHours)%2 == 0 {
			m = (mergeHours[len(mergeHours)/2-1] + m) / 2
		}
		out.Totals.MedianHoursToMerge = &m
	}
	return out, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// prMatchesAgent: one of the pull request's agents, or a model its agents'
// runs reported, fits the spec.
func prMatchesAgent(p *PRSummary, spec string) bool {
	return slices.ContainsFunc(p.Agents, func(a string) bool { return AgentMatches(spec, a) }) ||
		slices.ContainsFunc(p.models, func(m string) bool { return AgentMatches(spec, m) })
}

func scanPRSummary(sc interface{ Scan(...any) error }) (*PRSummary, error) {
	var kind string
	pr, err := scanPR(scanWithTail(sc, &kind))
	if err != nil {
		return nil, err
	}
	return newPRSummary(pr, kind), nil
}

// scanWithTail scans a row's columns plus extra ones after them.
type tailScanner struct {
	sc   interface{ Scan(...any) error }
	tail []any
}

func scanWithTail(sc interface{ Scan(...any) error }, tail ...any) tailScanner {
	return tailScanner{sc: sc, tail: tail}
}

func (t tailScanner) Scan(dest ...any) error { return t.sc.Scan(append(dest, t.tail...)...) }

func newPRSummary(pr *PullRequest, forgeKind string) *PRSummary {
	sm := &PRSummary{ID: pr.ID, Forge: forgeKind, Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
		State: pr.State, BaseRef: pr.BaseRef, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, OpenedAt: pr.OpenedAt, MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt,
		MergedBy: pr.MergedBy, Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles, Commits: pr.Commits,
		Agents: []string{}, Approvals: []string{}, Findings: map[string]int{}, Issues: []PRIssue{}, LastActivity: pr.UpdatedAt,
		Gate: pr.GateState, Armed: pr.ArmedAt != nil, WipAuto: pr.WipAuto, Outside: pr.Outside, Urgent: pr.urgent()}
	if pr.ciSHA == pr.HeadSHA {
		sm.CI = pr.CIState
	}
	if pr.Agent != "" {
		sm.Agents = append(sm.Agents, pr.Agent)
	}
	for _, t := range []*time.Time{pr.OpenedAt, pr.MergedAt, pr.ClosedAt} {
		if t != nil && t.After(sm.LastActivity) {
			sm.LastActivity = *t
		}
	}
	return sm
}

func idList(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (sm *PRSummary) addAgent(a string) {
	if a = strings.TrimSpace(a); a != "" && !slices.ContainsFunc(sm.Agents, func(x string) bool { return normalizeAgent(x) == normalizeAgent(a) }) {
		sm.Agents = append(sm.Agents, a)
	}
}

// fillPRs adds history counts, findings, linked issues and agents' work.
func (s *Service) fillPRs(ctx context.Context, actor *Actor, prs []*PRSummary) error {
	if len(prs) == 0 {
		return nil
	}
	byID := map[int64]*PRSummary{}
	ids := make([]any, len(prs))
	for n, p := range prs {
		byID[p.ID], ids[n] = p, p.ID
	}
	in := idList(len(ids))
	// #nosec G202 -- in is placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT pr_id, kind, bot, actor, count(*), max(created_at) FROM pr_events WHERE pr_id IN (`+in+`) GROUP BY pr_id, kind, bot, actor`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, last int64
		var kind, who string
		var bot bool
		var n int
		if err := rows.Scan(&id, &kind, &bot, &who, &n, &last); err != nil {
			rows.Close()
			return err
		}
		p := byID[id]
		switch kind {
		case "pushed":
			p.Pushes += n
		case "commented":
			if bot {
				p.Comments.Bots += n
			} else {
				p.Comments.People += n
			}
		case "reviewed", "approved", "changes_requested":
			if bot {
				p.Reviews.Bots += n
			} else {
				p.Reviews.People += n
			}
			if !bot && kind == "approved" && !slices.Contains(p.Approvals, who) {
				p.Approvals = append(p.Approvals, who)
			}
			if !bot && kind == "changes_requested" {
				p.ChangesRequested += n
			}
		}
		if t := msTime(last); t.After(p.LastActivity) {
			p.LastActivity = t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Waypoint's own reviews are its bot runs, posted or signed off with 👍.
	// #nosec G202 -- in is placeholders only; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT pr_id, count(*) FROM bot_runs WHERE pr_id IN (`+in+`) AND kind = 'review' AND status = 'done' GROUP BY pr_id`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		byID[id].Reviews.Bots += n
	}
	rows.Close()
	// #nosec G202 -- in is placeholders only; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT pr_id, severity, count(*) FROM bot_findings WHERE pr_id IN (`+in+`) AND placement != 'withheld' AND state != 'dropped' GROUP BY pr_id, severity`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var sev string
		var n int
		if err := rows.Scan(&id, &sev, &n); err != nil {
			rows.Close()
			return err
		}
		byID[id].Findings[sev] += n
	}
	rows.Close()

	// Linked issues the actor can see, and which agents linked them.
	links, err := s.prLinks(ctx, actor, ids)
	if err != nil {
		return err
	}
	issuePRs := map[int64][]*PRSummary{}
	var issueIDs []any
	for _, l := range links {
		p := byID[l.prID]
		if !slices.ContainsFunc(p.Issues, func(i PRIssue) bool { return i.ID == l.issue.ID }) {
			p.Issues = append(p.Issues, l.issue)
		}
		p.addAgent(l.agent)
		if _, ok := issuePRs[l.issue.ID]; !ok {
			issueIDs = append(issueIDs, l.issue.ID)
		}
		if !slices.Contains(issuePRs[l.issue.ID], p) {
			issuePRs[l.issue.ID] = append(issuePRs[l.issue.ID], p)
		}
	}
	if err := s.prMentions(ctx, actor, ids, byID); err != nil {
		return err
	}
	if len(issueIDs) == 0 {
		s.finishPRs(prs)
		return nil
	}
	now := s.nowMS()
	// #nosec G202 -- placeholders only; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT issue_id, agent, model, sum(max(0, coalesce(ended_at, ?) - started_at)), sum(input_tokens), sum(output_tokens), sum(cost_usd)
		FROM agent_runs WHERE issue_id IN (`+idList(len(issueIDs))+`) GROUP BY issue_id, agent, model`, append([]any{now}, issueIDs...)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var issue, ms, in, out int64
		var agent, model string
		var cost float64
		if err := rows.Scan(&issue, &agent, &model, &ms, &in, &out, &cost); err != nil {
			rows.Close()
			return err
		}
		for _, p := range issuePRs[issue] {
			p.addAgent(agent)
			if model != "" && !slices.Contains(p.models, model) {
				p.models = append(p.models, model)
			}
			p.AgentSeconds += ms / 1000
			p.InputTokens += in
			p.OutputTokens += out
			p.CostUSD += cost
		}
	}
	rows.Close()
	s.finishPRs(prs)
	return rows.Err()
}

func (s *Service) finishPRs(prs []*PRSummary) {
	now := s.now()
	for _, p := range prs {
		if p.OpenedAt == nil {
			continue
		}
		end := now
		switch {
		case p.MergedAt != nil && p.State == "merged":
			end = *p.MergedAt
		case p.ClosedAt != nil && p.State == "closed":
			end = *p.ClosedAt
		}
		h := max(0, end.Sub(*p.OpenedAt).Hours())
		p.HoursOpen = &h
	}
}

type prLink struct {
	prID  int64
	issue PRIssue
	agent string // the agent that linked it, if one did
}

// prLinks are the issues linked to the pull requests that the actor can
// see: by URL, or by repo and number (a link pasted in another spelling).
func (s *Service) prLinks(ctx context.Context, actor *Actor, ids []any) ([]prLink, error) {
	vis, vargs := visibleSQL("i.project_id", actor)
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.id, i.id, el.id, pj.key || '-' || i.number, i.title, st.name, st.category, el.created_agent
		FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		JOIN issues i ON i.id = el.issue_id JOIN projects pj ON pj.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE p.id IN (`+idList(len(ids))+`) AND `+vis+` AND `+draftsHidden(actor, "i")+` ORDER BY i.id`, append(slices.Clone(ids), vargs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []prLink
	for rows.Next() {
		var l prLink
		if err := rows.Scan(&l.prID, &l.issue.ID, &l.issue.LinkID, &l.issue.Key, &l.issue.Title, &l.issue.Status, &l.issue.Category, &l.agent); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// prMentions adds the issues the pull requests' text names but which aren't
// linked to them (WP-379), as the actor can see them.
func (s *Service) prMentions(ctx context.Context, actor *Actor, ids []any, byID map[int64]*PRSummary) error {
	vis, vargs := visibleSQL("i.project_id", actor)
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, i.id, pj.key || '-' || i.number, i.title, st.name, st.category, max(fm.verb)
		FROM pull_requests p
		JOIN forge_mentions fm ON fm.item_url = p.url AND fm.gone_at IS NULL
		JOIN issues i ON i.id = fm.issue_id JOIN projects pj ON pj.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE p.id IN (`+idList(len(ids))+`) AND `+vis+` AND `+draftsHidden(actor, "i")+`
		AND NOT EXISTS (SELECT 1 FROM external_links el WHERE el.issue_id = i.id AND el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number)))
		GROUP BY p.id, i.id ORDER BY i.id`, append(slices.Clone(ids), vargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var prID int64
		var m PRIssue
		if err := rows.Scan(&prID, &m.ID, &m.Key, &m.Title, &m.Status, &m.Category, &m.Verb); err != nil {
			return err
		}
		if p := byID[prID]; p != nil {
			p.Mentions = append(p.Mentions, m)
		}
	}
	return rows.Err()
}

// ---- one pull request ----

// PRTimelineItem is one thing in a pull request's story.
type PRTimelineItem struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // a history kind, or bot_review, agent_run, issue_status
	Actor   string    `json:"actor,omitempty"`
	Bot     bool      `json:"bot,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Model   string    `json:"model,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	URL     string    `json:"url,omitempty"`
	Body    string    `json:"body,omitempty"`
	Issue   string    `json:"issue,omitempty"` // an issue key
	Detail  string    `json:"detail,omitempty"`
	Seconds int64     `json:"seconds,omitempty"`
	Tokens  int64     `json:"tokens,omitempty"`
	RunID   int64     `json:"run_id,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
}

// PRAgentTime is what one agent spent on a pull request's issues.
type PRAgentTime struct {
	Agent        string   `json:"agent"`
	Models       []string `json:"models"`
	Runs         int      `json:"runs"`
	Seconds      int64    `json:"seconds"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CostUSD      float64  `json:"cost_usd"`
}

// PRDetail is one pull request's whole story.
type PRDetail struct {
	*PRSummary
	Timeline  []PRTimelineItem `json:"timeline"`
	AgentTime []*PRAgentTime   `json:"agent_time"`
	BotRuns   []*BotRun        `json:"bot_runs"`
	// Shipping: checks, the review gate, findings and readiness (WP-D21).
	Shipping *PRShipping `json:"shipping"`
	// LeftOpen: what was still open when it merged (WP-367).
	LeftOpen *PRLeftOpen `json:"left_open,omitempty"`
	// Tested: what the last ship recorded about its head's tree (WP-715).
	Tested string `json:"tested,omitempty"`
	// FixProof: whether the fix's tests fail without it (WP-709).
	FixProof string `json:"fix_proof,omitempty"`
	// MergeAfter: the pull requests it merges after (WP-912), those whoever
	// asks may see.
	MergeAfter []PRMergeWait `json:"merge_after,omitempty"`
}

const maxTimeline = 300

// GetPullRequest is a pull request's story: its history on the forge, bot
// reviews, agents' runs on its issues and their moves, oldest first.
func (s *Service) GetPullRequest(ctx context.Context, actor *Actor, id int64) (*PRDetail, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	vis, args := prVisible(actor, 0)
	// #nosec G202 -- vis is a fixed condition; values are bound
	sm, err := scanPRSummary(s.db.R.QueryRowContext(ctx, `SELECT `+prColsAs("p")+`, f.kind FROM pull_requests p JOIN forges f ON f.id = p.forge_id
		WHERE p.id = ? AND `+vis, append([]any{id}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("pull request")
	} else if err != nil {
		return nil, err
	}
	if err := s.fillPRs(ctx, actor, []*PRSummary{sm}); err != nil {
		return nil, err
	}
	d := &PRDetail{PRSummary: sm, Timeline: []PRTimelineItem{}, AgentTime: []*PRAgentTime{}, BotRuns: []*BotRun{}}
	if d.Tested, err = s.prEvidenceLine(ctx, s.db.R, sm.Repo, sm.HeadSHA, id); err != nil {
		return nil, err
	}
	if d.FixProof, err = fixProofLine(ctx, s.db.R, id); err != nil {
		return nil, err
	}
	if d.MergeAfter, err = s.visibleMergeAfter(ctx, actor, id); err != nil {
		return nil, err
	}

	rows, err := s.db.R.QueryContext(ctx, `SELECT kind, actor, bot, sha, url, body, created_at FROM pr_events WHERE pr_id = ? ORDER BY created_at, id LIMIT ?`, id, maxTimeline)
	if err != nil {
		return nil, err
	}
	// What strangers wrote before WP-281 kept their words out of history
	// stays out of an agent's sight.
	var voice *prVoice
	if actor.HidesDrafts() {
		if pr, err := s.prByID(ctx, s.db.R, id); err == nil {
			if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil {
				voice = s.prVoice(ctx, f)
			}
		}
	}
	for rows.Next() {
		var it PRTimelineItem
		var at int64
		if err := rows.Scan(&it.Kind, &it.Actor, &it.Bot, &it.SHA, &it.URL, &it.Body, &at); err != nil {
			rows.Close()
			return nil, err
		}
		it.At = msTime(at)
		if it.Body != "" && actor.HidesDrafts() && (voice == nil || voice.outside(ctx, s.db.R, it.Actor)) {
			it.Body = ""
		}
		d.Timeline = append(d.Timeline, it)
	}
	rows.Close()

	runVis, runArgs := visibleSQL("r.project_id", actor)
	// #nosec G202 -- runVis is a fixed condition; values are bound
	brows, err := s.db.R.QueryContext(ctx, `SELECT `+runCols+` FROM bot_runs r LEFT JOIN automations a ON a.id = r.automation_id
		WHERE r.pr_id = ? AND `+runVis+` ORDER BY r.id`, append([]any{id}, runArgs...)...)
	if err != nil {
		return nil, err
	}
	for brows.Next() {
		r, err := s.scanRun(ctx, brows)
		if err != nil {
			brows.Close()
			return nil, err
		}
		d.BotRuns = append(d.BotRuns, r)
	}
	brows.Close()
	for _, r := range d.BotRuns {
		if r.Findings, err = s.runFindings(ctx, r.ID); err != nil {
			return nil, err
		}
		at := r.CreatedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		it := PRTimelineItem{At: at, Kind: "bot_review", Bot: true, SHA: r.HeadSHA, URL: r.ReviewURL, Detail: cmp(r.Summary, r.Error),
			RunID: r.ID, Outcome: r.Status, Actor: cmp(r.Automation, "Waypoint")}
		if r.StartedAt != nil && r.FinishedAt != nil {
			it.Seconds = int64(r.FinishedAt.Sub(*r.StartedAt).Seconds())
		}
		d.Timeline = append(d.Timeline, it)
	}

	if pr, err := s.prByID(ctx, s.db.R, id); err == nil {
		if d.Shipping, err = s.prShipping(ctx, actor, pr); err != nil {
			return nil, err
		}
	}
	d.LeftOpen = s.leftOpen(ctx, id)
	if len(sm.Issues) > 0 {
		if err := s.prIssueStory(ctx, d); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(d.Timeline, func(a, b PRTimelineItem) int { return a.At.Compare(b.At) })
	if len(d.Timeline) > maxTimeline {
		d.Timeline = d.Timeline[len(d.Timeline)-maxTimeline:]
	}
	return d, nil
}

// prIssueStory adds the linked issues' agent runs and status moves.
func (s *Service) prIssueStory(ctx context.Context, d *PRDetail) error {
	keys := map[int64]string{}
	ids := make([]any, 0, len(d.Issues))
	for _, i := range d.Issues {
		keys[i.ID] = i.Key
		ids = append(ids, i.ID)
	}
	now := s.nowMS()
	// #nosec G202 -- placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, issue_id, agent, model, step, started_at, ended_at, input_tokens, output_tokens, cost_usd, outcome
		FROM agent_runs WHERE issue_id IN (`+idList(len(ids))+`) ORDER BY id LIMIT 200`, ids...)
	if err != nil {
		return err
	}
	byAgent := map[string]*PRAgentTime{}
	for rows.Next() {
		var runID, issue, start, in, out int64
		var end sql.NullInt64
		var agent, model, step, outcome string
		var cost float64
		if err := rows.Scan(&runID, &issue, &agent, &model, &step, &start, &end, &in, &out, &cost, &outcome); err != nil {
			rows.Close()
			return err
		}
		stop := now
		if end.Valid {
			stop = end.Int64
		}
		secs := max(0, stop-start) / 1000
		detail := "worked on " + keys[issue]
		if step != "" {
			detail += " (" + step + ")"
		}
		d.Timeline = append(d.Timeline, PRTimelineItem{At: msTime(start), Kind: "agent_run", Agent: agent, Model: model, Issue: keys[issue],
			Detail: detail, Seconds: secs, Tokens: in + out, Outcome: cmp(outcome, "running")})
		k := normalizeAgent(agent)
		at := byAgent[k]
		if at == nil {
			at = &PRAgentTime{Agent: agent, Models: []string{}}
			byAgent[k] = at
			d.AgentTime = append(d.AgentTime, at)
		}
		at.Runs++
		at.Seconds += secs
		at.InputTokens += in
		at.OutputTokens += out
		at.CostUSD += cost
		if model != "" && !slices.Contains(at.Models, model) {
			at.Models = append(at.Models, model)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	slices.SortStableFunc(d.AgentTime, func(a, b *PRAgentTime) int { return int(b.Seconds - a.Seconds) })
	// #nosec G202 -- placeholders only; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT e.issue_id, e.new_value, coalesce(u.display_name, u.username, ''), coalesce(json_extract(e.data, '$.via'), ''), e.created_at
		FROM events e LEFT JOIN users u ON u.id = e.actor_id
		WHERE e.kind = ? AND e.issue_id IN (`+idList(len(ids))+`) ORDER BY e.id LIMIT 100`, append([]any{EvIssueStatus}, ids...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var issue, at int64
		var status, who, via string
		if err := rows.Scan(&issue, &status, &who, &via, &at); err != nil {
			return err
		}
		d.Timeline = append(d.Timeline, PRTimelineItem{At: msTime(at), Kind: "issue_status", Issue: keys[issue], Actor: who, Agent: via,
			Detail: fmt.Sprintf("%s moved to %s", keys[issue], status)})
	}
	return rows.Err()
}
