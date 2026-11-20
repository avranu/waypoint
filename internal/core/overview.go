package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The Agents overview (WP-D16): which agents are working where, what they
// finished, what comes next, what waits on a person, in one round trip. It
// is built from the queries each part of the app already has (sessions,
// requests, the queue, pull requests, branches, environments, usage) plus a
// few small ones for changes and what was done; nothing is stored for it.

const (
	ovSessions = 100 // live sessions listed
	ovPulls    = 30  // open pull requests listed
	ovBranches = 30
	ovChanges  = 12
	ovDone     = 15
	ovHeads    = 3   // issues shown per agent under up next
	ovTop      = 5   // items the summary lists link to
	ovScan     = 200 // rows read to count what was done
)

// OverviewQuery narrows the overview.
type OverviewQuery struct {
	Agent   string // an agent spec
	Project string // a project key
	Days    int    // the period the summary and done cover (default 7)
}

// OverviewItem is a record the overview points to.
type OverviewItem struct {
	Kind   string    `json:"kind"` // issue, pr, request, session, review, design
	Ref    string    `json:"ref,omitempty"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	URL    string    `json:"url,omitempty"`   // an app path
	Agent  string    `json:"agent,omitempty"` // who did it (for a request: who asked)
	By     string    `json:"by,omitempty"`    // a request: the person who decided it
	At     time.Time `json:"at"`
}

// SummaryPart counts one kind of thing in a summary list.
type SummaryPart struct {
	Kind  string `json:"kind"`
	Label string `json:"label"` // "3 issues closed"
	Count int    `json:"count"`
}

// SummaryList is one of the four lists of the summary: how many, what kinds,
// and the top few linked.
type SummaryList struct {
	Count int            `json:"count"`
	Parts []SummaryPart  `json:"parts"`
	Items []OverviewItem `json:"items"`
}

// OverviewSummary is what is being done, what was done, what is deferred and
// what waits on a person.
type OverviewSummary struct {
	Doing    SummaryList `json:"doing"`
	Done     SummaryList `json:"done"`     // what agents did: issues closed, pull requests merged
	Resolved SummaryList `json:"resolved"` // requests people decided: their work, not the agents'
	Deferred SummaryList `json:"deferred"`
	Waiting  SummaryList `json:"waiting"`
}

// NextItem is an issue at the head of an agent's queue.
type NextItem struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Priority int    `json:"priority"`
	Why      string `json:"why"`
	Position int    `json:"position"` // in what next hands out
	Pinned   bool   `json:"pinned,omitempty"`
}

// OverviewNext is the head of the queue for one agent (the spec the work was
// delegated to).
type OverviewNext struct {
	Agent string     `json:"agent"`
	Items []NextItem `json:"items"`
	More  int        `json:"more"` // ready issues beyond those listed
}

// OverviewClaim is an issue an agent holds.
type OverviewClaim struct {
	Key     string    `json:"key"`
	Title   string    `json:"title"`
	Project string    `json:"project"`
	Agent   string    `json:"agent"`
	Since   time.Time `json:"since"`
	Seconds int64     `json:"seconds"`
}

// BranchSession is a session on a branch.
type BranchSession struct {
	ID    int64  `json:"id"`
	Agent string `json:"agent"`
	State string `json:"state"`
	Title string `json:"title,omitempty"`
}

// OverviewBranch is a branch somebody works on: pushed lately, with an open
// pull request, or holding a live session.
type OverviewBranch struct {
	Branch
	Sessions []BranchSession `json:"sessions"`
}

// OverviewPulls are the open pull requests and the period's counts.
type OverviewPulls struct {
	Open   []*PRSummary `json:"open"`
	Total  int          `json:"total"` // open now, listed or not
	Totals PRTotals     `json:"totals"`
}

// Overview is everything the Agents → Overview tab shows.
type Overview struct {
	At      time.Time `json:"at"`
	Days    int       `json:"days"`
	Agent   string    `json:"agent"`
	Project string    `json:"project"`

	Waiting       *Waiting            `json:"waiting"`
	Sessions      []*AgentSession     `json:"sessions"`
	SessionsTotal int                 `json:"sessions_total"`
	Next          []*OverviewNext     `json:"next"`
	Claims        []*OverviewClaim    `json:"claims"`
	Pulls         OverviewPulls       `json:"pulls"`
	Branches      []OverviewBranch    `json:"branches"`
	Changes       *ChangeList         `json:"changes"`
	Done          []OverviewItem      `json:"done"`
	Resolved      []OverviewItem      `json:"resolved"` // requests a person decided
	Environments  *EnvironmentSummary `json:"environments"`
	// Usage is this week's, for the person themself; null where nothing was
	// reported.
	Usage   *UsageSummaryData `json:"usage"`
	Summary OverviewSummary   `json:"summary"`
}

// AgentOverview gathers the overview for the actor.
func (s *Service) AgentOverview(ctx context.Context, actor *Actor, q OverviewQuery) (*Overview, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	q.Agent, q.Project = strings.TrimSpace(q.Agent), strings.TrimSpace(q.Project)
	if len(q.Agent) > 100 {
		return nil, invalid("agent can be at most 100 characters")
	}
	if q.Days <= 0 {
		q.Days = 7
	}
	q.Days = min(q.Days, 366)
	var projectID int64
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projectID, q.Project = p.ID, p.Key
	}
	now := s.now()
	since := now.AddDate(0, 0, -q.Days).UnixMilli()
	o := &Overview{At: now.UTC(), Days: q.Days, Agent: q.Agent, Project: q.Project,
		Sessions: []*AgentSession{}, Next: []*OverviewNext{}, Claims: []*OverviewClaim{}, Branches: []OverviewBranch{}, Done: []OverviewItem{}, Resolved: []OverviewItem{}}
	var err error

	if o.Waiting, err = s.waitingOnYou(ctx, actor, q.Project, q.Agent); err != nil {
		return nil, err
	}
	page, err := s.ListAgentSessions(ctx, actor, AgentSessionQuery{State: "active", Agent: q.Agent, Project: q.Project, Limit: ovSessions})
	if err != nil {
		return nil, err
	}
	o.Sessions, o.SessionsTotal = page.Sessions, page.Total
	if err := s.overviewQueue(ctx, actor, q, o); err != nil {
		return nil, err
	}
	prs, err := s.ListPullRequests(ctx, actor, PRQuery{Project: q.Project, Agent: q.Agent, Days: q.Days})
	if err != nil {
		return nil, err
	}
	o.Pulls.Open, o.Pulls.Totals = []*PRSummary{}, prs.Totals
	var merged []*PRSummary
	for _, p := range prs.Pulls {
		switch {
		case p.State == "open" || p.State == "draft":
			o.Pulls.Total++
			if len(o.Pulls.Open) < ovPulls {
				o.Pulls.Open = append(o.Pulls.Open, p)
			}
		case p.State == "merged" && p.MergedAt != nil && p.MergedAt.UnixMilli() >= since && (q.Agent != "" || len(p.Agents) > 0):
			merged = append(merged, p)
		}
	}
	if err := s.ovBranches(ctx, actor, q, o, prs.Pulls); err != nil {
		return nil, err
	}
	if o.Changes, err = s.RecentChanges(ctx, actor, ChangeQuery{Project: q.Project, Agent: q.Agent, Limit: ovChanges}); err != nil {
		return nil, err
	}
	if o.Environments, err = s.summarizeEnvironments(ctx, actor, q.Project); err != nil {
		return nil, err
	}
	// Usage is a person's own, and their agents'.
	if u, err := s.UsageSummary(ctx, actor); err == nil && u.HasData {
		o.Usage = u
	}
	return o, s.overviewSummary(ctx, actor, q, projectID, since, merged, o)
}

// overviewQueue fills up next (the head per agent) and the claims.
func (s *Service) overviewQueue(ctx context.Context, actor *Actor, q OverviewQuery, o *Overview) error {
	ids, err := s.queueAgents(ctx, actor, q.Agent)
	if err != nil {
		return err
	}
	ready, _, err := s.agentWork(ctx, actor, ids, AgentQuery{Project: q.Project})
	if err != nil {
		return err
	}
	// Heads, grouped by the agent the work was delegated to, in the order
	// next would hand them out.
	by := map[string]*OverviewNext{}
	var pinned []int64
	type pick struct {
		issue *Issue
		pos   int
		pin   int
		plan  bool
		unb   int
		grp   *OverviewNext
	}
	var picks []pick
	for n, qi := range ready {
		i := qi.issue
		g := by[i.Agent]
		if g == nil {
			g = &OverviewNext{Agent: i.Agent, Items: []NextItem{}}
			by[i.Agent] = g
			o.Next = append(o.Next, g)
		}
		if shown := len(g.Items) + g.More; shown >= ovHeads {
			g.More++
			continue
		}
		p := pick{issue: i, pos: n + 1, plan: qi.planned, unb: qi.unblocks, grp: g}
		if i.AgentOrder != nil {
			p.pin = n + 1
			pinned = append(pinned, i.ID)
		}
		picks = append(picks, p)
		g.Items = append(g.Items, NextItem{Key: i.Key}) // filled below, once the pinners are known
	}
	pinners, err := s.pinners(ctx, pinned)
	if err != nil {
		return err
	}
	today := s.now().UTC().Format(dateLayout)
	filled := map[*OverviewNext]int{}
	for _, p := range picks {
		i := p.issue
		p.grp.Items[filled[p.grp]] = NextItem{Key: i.Key, Title: i.Title, Project: i.ProjectKey, Priority: i.Priority,
			Why: whyQueued(i, p.pin, pinners[i.ID], today, p.plan, p.unb), Position: p.pos, Pinned: i.AgentOrder != nil}
		filled[p.grp]++
	}
	// The agent with the most waiting first; "" is work anyone may take
	// (WP-93), after all the delegated work.
	slices.SortStableFunc(o.Next, func(a, b *OverviewNext) int {
		if (a.Agent == "") != (b.Agent == "") {
			if a.Agent == "" {
				return 1
			}
			return -1
		}
		return cmpInt(int64(len(b.Items)+b.More), int64(len(a.Items)+a.More))
	})

	// What agents hold now.
	res, err := s.Search(ctx, actor, SearchParams{Query: "is:claimed order:-updated", Project: q.Project, Limit: 50})
	if err != nil {
		return err
	}
	for _, i := range res.Issues {
		if ids != nil && !slices.ContainsFunc(ids, func(id string) bool { return normalizeAgent(id) == normalizeAgent(i.ClaimedBy) }) {
			continue
		}
		c := &OverviewClaim{Key: i.Key, Title: i.Title, Project: i.ProjectKey, Agent: i.ClaimedBy, Seconds: i.ClaimWorked}
		if i.ClaimedAt != nil {
			c.Since = *i.ClaimedAt
		}
		if i.ClaimWorkingSince != nil {
			c.Seconds += int64(s.now().Sub(*i.ClaimWorkingSince).Seconds())
		}
		o.Claims = append(o.Claims, c)
	}
	return nil
}

// ovBranches lists branches in work: pushed lately, with a pull
// request, or holding a live session, each with its sessions.
func (s *Service) ovBranches(ctx context.Context, actor *Actor, q OverviewQuery, o *Overview, pulls []*PRSummary) error {
	branches, err := s.ListBranches(ctx, actor, q.Project)
	if err != nil {
		return err
	}
	type key struct{ repo, branch string }
	k := func(repo, branch string) key { return key{strings.ToLower(repo), branch} }
	sessions := map[key][]BranchSession{}
	for _, x := range o.Sessions {
		if x.Repo != "" && x.Branch != "" {
			sessions[k(x.Repo, x.Branch)] = append(sessions[k(x.Repo, x.Branch)], BranchSession{ID: x.ID, Agent: x.Agent, State: x.State, Title: x.Title})
		}
	}
	prIDs := map[int64]bool{}
	for _, p := range pulls {
		prIDs[p.ID] = true
	}
	have := map[key]bool{}
	for _, b := range branches {
		ss := sessions[k(b.Repo, b.Branch)]
		have[k(b.Repo, b.Branch)] = true
		if q.Agent != "" && len(ss) == 0 && (b.PR == nil || !prIDs[b.PR.ID]) {
			continue
		}
		o.Branches = append(o.Branches, OverviewBranch{Branch: b, Sessions: nonNilSessions(ss)})
	}
	// Sessions on branches nothing was pushed from yet.
	for _, x := range o.Sessions {
		kk := k(x.Repo, x.Branch)
		if x.Repo == "" || x.Branch == "" || have[kk] {
			continue
		}
		have[kk] = true
		o.Branches = append(o.Branches, OverviewBranch{
			Branch:   Branch{Repo: x.Repo, Branch: x.Branch, PushedAt: x.LastSeenAt},
			Sessions: nonNilSessions(sessions[kk]),
		})
	}
	slices.SortStableFunc(o.Branches, func(a, b OverviewBranch) int {
		// Live sessions first, then the newest push.
		if (len(a.Sessions) > 0) != (len(b.Sessions) > 0) {
			if len(a.Sessions) > 0 {
				return -1
			}
			return 1
		}
		return b.PushedAt.Compare(a.PushedAt)
	})
	if len(o.Branches) > ovBranches {
		o.Branches = o.Branches[:ovBranches]
	}
	return nil
}

func nonNilSessions(l []BranchSession) []BranchSession {
	if l == nil {
		return []BranchSession{}
	}
	return l
}

// overviewSummary fills the four lists and the done list.
func (s *Service) overviewSummary(ctx context.Context, actor *Actor, q OverviewQuery, projectID, since int64, merged []*PRSummary, o *Overview) error {
	sum := &o.Summary
	sum.Doing.Items, sum.Done.Items, sum.Deferred.Items, sum.Waiting.Items = []OverviewItem{}, []OverviewItem{}, []OverviewItem{}, []OverviewItem{}
	sum.Doing.Parts, sum.Done.Parts, sum.Deferred.Parts, sum.Waiting.Parts = []SummaryPart{}, []SummaryPart{}, []SummaryPart{}, []SummaryPart{}
	sum.Resolved.Items, sum.Resolved.Parts = []OverviewItem{}, []SummaryPart{}
	add := func(l *SummaryList, kind, one, many string, n int) {
		if n > 0 {
			word := many
			if n == 1 {
				word = one
			}
			l.Count += n
			l.Parts = append(l.Parts, SummaryPart{Kind: kind, Label: fmt.Sprintf("%d %s", n, word), Count: n})
		}
	}

	// Doing: sessions at work (or waiting for a person), and issues agents
	// hold that no listed session is on.
	held := map[string]bool{}
	var doing []*AgentSession
	for _, x := range o.Sessions {
		if x.State == SessionWorking || x.State == SessionWaiting {
			doing = append(doing, x)
			if x.Issue != nil {
				held[x.Issue.Key] = true
			}
		}
	}
	for _, x := range doing {
		it := OverviewItem{Kind: "session", Title: cmp(x.Title, cmp(taskTitle(x), "Session on "+x.Machine)), URL: "/agents/sessions/" + itoa64(x.ID),
			Detail: excerpt(firstLineOf(cmp(x.LastUpdate, x.LastPrompt)), 140), Agent: x.Agent, At: x.LastSeenAt}
		if x.Issue != nil {
			it.Ref = x.Issue.Key
		}
		sum.Doing.Items = append(sum.Doing.Items, it)
	}
	add(&sum.Doing, "session", "session working", "sessions working", len(doing))
	var tasks int
	for _, c := range o.Claims {
		if held[c.Key] {
			continue
		}
		tasks++
		sum.Doing.Items = append(sum.Doing.Items, OverviewItem{Kind: "issue", Ref: c.Key, Title: c.Title, URL: "/issue/" + c.Key, Agent: c.Agent, At: c.Since,
			Detail: "claimed"})
	}
	add(&sum.Doing, "issue", "task claimed", "tasks claimed", tasks)

	// Done in the period: issues agents closed and pull requests they merged.
	// Requests a person resolved are kept apart: a person did that work.
	issues, err := s.doneIssues(ctx, actor, projectID, since, q.Agent)
	if err != nil {
		return err
	}
	resolved, err := s.resolvedRequests(ctx, actor, projectID, since, q.Agent)
	if err != nil {
		return err
	}
	all := append(append([]OverviewItem{}, issues...), prItems(merged)...)
	slices.SortStableFunc(all, func(a, b OverviewItem) int { return b.At.Compare(a.At) })
	add(&sum.Done, "issue", "issue closed", "issues closed", len(issues))
	add(&sum.Done, "pr", "PR merged", "PRs merged", len(merged))
	sum.Done.Items = all[:min(len(all), ovTop)]
	o.Done = all[:min(len(all), ovDone)]
	add(&sum.Resolved, "request", "request resolved", "requests resolved", len(resolved))
	sum.Resolved.Items = resolved[:min(len(resolved), ovTop)]
	o.Resolved = resolved[:min(len(resolved), ovDone)]

	// Deferred: what agents filed into the backlog or To Do, requests put
	// off, and pull requests waiting to merge.
	filed, err := s.filedIssues(ctx, actor, projectID, since, q.Agent)
	if err != nil {
		return err
	}
	snoozed, err := s.snoozedRequests(ctx, actor, projectID, q.Agent)
	if err != nil {
		return err
	}
	open := prItems(o.Pulls.Open)
	for n := range open {
		open[n].Detail = prStanding(o.Pulls.Open[n])
	}
	add(&sum.Deferred, "issue", "issue filed for later", "issues filed for later", len(filed))
	add(&sum.Deferred, "request", "request snoozed", "requests snoozed", len(snoozed))
	add(&sum.Deferred, "pr", "PR waiting", "PRs waiting", o.Pulls.Total)
	items := append(append(append([]OverviewItem{}, filed...), snoozed...), open...)
	slices.SortStableFunc(items, func(a, b OverviewItem) int { return b.At.Compare(a.At) })
	sum.Deferred.Items = items[:min(len(items), ovTop)]

	// Waiting on you: open requests, design notes to approve, agent text to review.
	w := o.Waiting
	for _, r := range w.Requests {
		sum.Waiting.Items = append(sum.Waiting.Items, OverviewItem{Kind: "request", Ref: r.Code, Title: r.Title, Detail: kindWords[r.Kind], URL: "/requests/" + r.Ref, Agent: r.Agent, At: r.CreatedAt})
	}
	add(&sum.Waiting, "request", "open request", "open requests", w.Total-w.Design-w.Review-w.Pulls)
	add(&sum.Waiting, "design", "design note to approve", "design notes to approve", w.Design)
	add(&sum.Waiting, "review", "piece of agent text to review", "pieces of agent text to review", w.Review)
	if w.Design > 0 {
		sum.Waiting.Items = append(sum.Waiting.Items, OverviewItem{Kind: "design", Title: fmt.Sprintf("%d to approve", w.Design), URL: "/design/review", At: o.At})
	}
	if w.Review > 0 {
		sum.Waiting.Items = append(sum.Waiting.Items, OverviewItem{Kind: "review", Title: fmt.Sprintf("%d to review", w.Review), URL: "/search?q=is%3Aunreviewed&mode=issues", At: o.At})
	}
	if len(sum.Waiting.Items) > ovTop {
		sum.Waiting.Items = sum.Waiting.Items[:ovTop]
	}
	return nil
}

var kindWords = map[string]string{
	RequestQuestion: "asks", RequestChange: "wants to change files", RequestCommand: "wants a command run",
	RequestPermission: "was refused", RequestAction: "needs you",
}

func taskTitle(x *AgentSession) string {
	if x.Issue == nil {
		return ""
	}
	return x.Issue.Key + " " + x.Issue.Title
}

// prStanding says where a pull request stands for merging, in a few words.
func prStanding(p *PRSummary) string {
	var parts []string
	switch p.CI {
	case "failed":
		parts = append(parts, "checks failing")
	case "pending":
		parts = append(parts, "checks running")
	case "passed":
		parts = append(parts, "checks passed")
	}
	switch p.Gate {
	case "failure":
		parts = append(parts, "review findings open")
	case "pending":
		parts = append(parts, "review pending")
	}
	if p.Armed {
		parts = append(parts, "set to merge")
	}
	if p.State == "draft" {
		parts = append(parts, "draft")
	}
	return strings.Join(parts, " · ")
}

func prItems(list []*PRSummary) []OverviewItem {
	out := make([]OverviewItem, 0, len(list))
	for _, p := range list {
		at := p.LastActivity
		if p.MergedAt != nil {
			at = *p.MergedAt
		}
		agent := ""
		if len(p.Agents) > 0 {
			agent = p.Agents[0]
		}
		out = append(out, OverviewItem{Kind: "pr", Ref: fmt.Sprintf("%s#%d", p.Repo, p.Number), Title: p.Title, URL: fmt.Sprintf("/agents?tab=pulls&pr=%d", p.ID), Agent: agent, At: at})
	}
	return out
}

// visibleIssues limits issues i to projects the actor can read (and one).
func visibleIssues(actor *Actor, projectID int64) (string, []any) {
	where, args := visibleSQL("i.project_id", actor)
	where += " AND " + draftsHidden(actor, "i") // WP-42
	if projectID != 0 {
		where, args = where+" AND i.project_id = ?", append(args, projectID)
	}
	return where, args
}

// doneIssues are issues an agent closed since a time (newest first).
func (s *Service) doneIssues(ctx context.Context, actor *Actor, projectID, since int64, spec string) ([]OverviewItem, error) {
	where, args := visibleIssues(actor, projectID)
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key, i.number, i.title, i.resolved_agent, i.resolved_at FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.resolved_at >= ? AND i.resolved_agent != '' AND `+where+` ORDER BY i.resolved_at DESC LIMIT ?`, append([]any{since}, append(args, ovScan)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverviewItem{}
	for rows.Next() {
		var key, title, agent string
		var number, at int64
		if err := rows.Scan(&key, &number, &title, &agent, &at); err != nil {
			return nil, err
		}
		if spec != "" && !AgentMatches(spec, agent) {
			continue
		}
		ref := fmt.Sprintf("%s-%d", key, number)
		out = append(out, OverviewItem{Kind: "issue", Ref: ref, Title: title, URL: "/issue/" + ref, Agent: agent, At: msTime(at), Detail: "closed"})
	}
	return out, rows.Err()
}

// filedIssues are issues an agent filed since a time that still wait in the
// backlog or To Do.
func (s *Service) filedIssues(ctx context.Context, actor *Actor, projectID, since int64, spec string) ([]OverviewItem, error) {
	where, args := visibleIssues(actor, projectID)
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key, i.number, i.title, i.reporter_agent, i.created_at FROM issues i JOIN projects p ON p.id = i.project_id
		JOIN statuses st ON st.id = i.status_id
		WHERE i.reporter_agent != '' AND i.created_at >= ? AND i.resolved_at IS NULL AND st.category IN ('backlog','todo') AND `+where+`
		ORDER BY i.created_at DESC LIMIT ?`, append([]any{since}, append(args, ovScan)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverviewItem{}
	for rows.Next() {
		var key, title, agent string
		var number, at int64
		if err := rows.Scan(&key, &number, &title, &agent, &at); err != nil {
			return nil, err
		}
		if spec != "" && !AgentMatches(spec, agent) {
			continue
		}
		ref := fmt.Sprintf("%s-%d", key, number)
		out = append(out, OverviewItem{Kind: "issue", Ref: ref, Title: title, URL: "/issue/" + ref, Agent: agent, At: msTime(at), Detail: "filed for later"})
	}
	return out, rows.Err()
}

// requestItems reads requests (columns: number, project key, kind, title, state, agent, time) the actor sees.
func (s *Service) requestItems(ctx context.Context, actor *Actor, projectID int64, spec, cond string, args []any, detail string) ([]OverviewItem, error) {
	vis, vargs := requestVisible(actor, "r")
	where := vis + " AND " + cond
	args = append(vargs, args...)
	if projectID != 0 {
		where, args = where+" AND r.project_id = ?", append(args, projectID)
	}
	// #nosec G202 -- where is fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.number, coalesce(p.key, ''), r.title, r.state, r.agent, coalesce(r.decided_at, r.snoozed_until, r.created_at),
			coalesce(nullif(du.display_name, ''), du.username, '')
		FROM requests r LEFT JOIN projects p ON p.id = r.project_id LEFT JOIN users du ON du.id = r.decided_by WHERE `+where+` ORDER BY 6 DESC LIMIT ?`, append(args, ovScan)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverviewItem{}
	for rows.Next() {
		var number, at int64
		var key, title, state, agent, by string
		if err := rows.Scan(&number, &key, &title, &state, &agent, &at, &by); err != nil {
			return nil, err
		}
		if spec != "" && !AgentMatches(spec, agent) {
			continue
		}
		ref := requestRef(key, int(number))
		_, code, _ := strings.Cut(ref, ":")
		it := OverviewItem{Kind: "request", Ref: code, Title: title, URL: "/requests/" + ref, Agent: agent, By: by, At: msTime(at), Detail: detail}
		if detail == "" { // a decided request: what the person did
			it.Detail = decisionWords(state, by)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// decisionWords says what a person did to a request: "Answered by Robin".
func decisionWords(state, by string) string {
	verb := map[string]string{
		RequestAnswered: "Answered", RequestApproved: "Approved", RequestApplied: "Approved", RequestFailed: "Approved (it could not be applied)",
		RequestConflict: "Approved (the files had changed)", RequestDenied: "Declined", RequestDone: "Marked done", RequestDismissed: "Dismissed",
	}[state]
	if verb == "" {
		verb = "Resolved"
	}
	if by == "" {
		return verb
	}
	return verb + " by " + by
}

// resolvedRequests are requests a person decided since a time.
func (s *Service) resolvedRequests(ctx context.Context, actor *Actor, projectID, since int64, spec string) ([]OverviewItem, error) {
	args := []any{since}
	states := placeholders(len(decidedStates))
	for _, st := range decidedStates {
		args = append(args, st)
	}
	return s.requestItems(ctx, actor, projectID, spec, `r.decided_at >= ? AND r.state IN (`+states+`)`, args, "")
}

// snoozedRequests are open requests hidden until later.
func (s *Service) snoozedRequests(ctx context.Context, actor *Actor, projectID int64, spec string) ([]OverviewItem, error) {
	return s.requestItems(ctx, actor, projectID, spec, `r.state = 'open' AND r.snoozed_until > ?`, []any{s.nowMS()}, "snoozed")
}
