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

// The agent work loop (WP-D7): an agent asks for the next task it may
// take, claims it, works, and reports done; Waypoint advances the workflow
// and records the run's time and tokens.

// ClaimLease is how long a claim lasts without being finished or renewed.
const ClaimLease = 2 * time.Hour

// AgentQuery narrows what next/batch hand out.
type AgentQuery struct {
	Agent   string `json:"agent"`   // the agent's identity; defaults to the token's
	Project string `json:"project"` // a project key
	Repo    string `json:"repo"`    // owner/name[,…]: issues linked to it or in projects it's attached to
	Step    string `json:"step"`    // only issues at this workflow step
	Query   string `json:"query"`   // more WQL
	// Delegated: only work handed to agents on purpose, not Anyone's. For
	// callers that start work without a person asking (a runner draining
	// the queue); an agent asking for its next task leaves it off.
	Delegated bool `json:"delegated"`
	Peek      bool `json:"peek"` // look without claiming
	// ScreenWaits: list anyone's tasks that wait for the safety screen too,
	// not only delegated ones (for a peek that says why nothing is ready).
	ScreenWaits bool `json:"-"`
	// CrossProject is why this claim reaches outside the session's project.
	CrossProject string `json:"cross_project"`
	// Within is WQL for the session's project: without CrossProject, batch
	// leaves related work outside it unclaimed.
	Within string `json:"within"`
}

// AgentTask is a task handed to an agent.
type AgentTask struct {
	Issue    *IssueDetail
	Workflow *Workflow
	Step     *WorkflowStep // the current step, with its instructions
	StepNo   int           // 1-based
	Related  []*Issue      // batch: the related issues claimed along with it
	Resumed  bool          // the agent already held this claim
	// Skipped are tasks this agent holds in another live session, passed
	// over: "WP-85 (in /projects/app)" (WP-89).
	Skipped []string
	Design  []DesignRef // design notes linked to the task (wp spec for each)
	// Comments on the task, oldest first: they often correct or narrow
	// what the description asks, so the answer carries them (WP-449).
	Comments []Comment
	// Why says what put it first in the queue ("Pinned at #1 by Robin",
	// "Urgent · due Oct 14"); empty for a task the agent already held.
	Why string
	// Named are the other tasks its description, comments and step
	// instructions name by key, first named first, that the answer doesn't
	// already list (parent, relations, sub-tasks): a title and status
	// each, so the agent opens one only when it needs the body (WP-545).
	Named []*Issue
	// Resume is where the task's work was left when its session ended
	// before it shipped (WP-567); nil when it wasn't.
	Resume *StrandedWork
	// ParentText is the description of the epic (or story) the task is a
	// sub-task of, so the agent knows the broader problem (WP-733): "" when
	// it has none, or when its text is kept from agents as the task's own
	// would be: a draft, from outside (WP-ADR11), or not cleared by the
	// safety screen.
	ParentText string
	// Nearby is work in progress the relation graph puts within a few steps
	// of the task that the answer doesn't already list (WP-385): another
	// session may be changing the same thing (WP-232 and WP-233 were).
	Nearby []GraphHit
}

// namedMax caps the tasks listed as named in a task's text; namedLookups
// caps the keys looked up for them (some are ADR-0007 or a key in a project
// the agent can't see).
const (
	namedMax     = 8
	namedLookups = 20
)

// AgentUsage is what a run cost.
type AgentUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	// Model is the model that did the work, when tooling knows it (Claude
	// Code's transcript names it; the agent's identity may only say
	// anthropic/claude-code). The run keeps the latest one reported.
	Model string `json:"model,omitempty"`
	// ReportID, if set, makes a report count once: a retry with the same id
	// (after a lost response) is ignored.
	ReportID string `json:"report_id,omitempty"`
}

// AgentRun is one claim of an issue by an agent.
type AgentRun struct {
	ID        int64      `json:"id"`
	Agent     string     `json:"agent"`
	Step      string     `json:"step,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	Seconds   int64      `json:"seconds"`
	Outcome   string     `json:"outcome,omitempty"` // done, released, expired; "" while running
	AgentUsage
}

// agentIdentity is who the agent is: its token's identity, or (for tokens
// without one) what it says it is.
func agentIdentity(actor *Actor, override string) (string, error) {
	id, say := "", strings.TrimSpace(override)
	if actor != nil {
		id = actor.Agent
	}
	switch {
	case id != "" && say != "" && normalizeAgent(say) != normalizeAgent(id):
		return "", invalid("this token acts as %s", id)
	case id == "":
		id = say
	}
	if id == "" {
		return "", invalid("say which agent you are: use an agent token, the X-Waypoint-Agent header or agent=provider/model")
	}
	if len(id) > 100 {
		return "", invalid("agent names can be at most 100 characters")
	}
	return id, nil
}

// AgentNext hands the agent the most urgent task it may take and claims it.
// If the agent already holds a live claim that still fits, that task comes
// back instead, so an agent that lost its place just asks again. Nil means
// nothing to do.
func (s *Service) AgentNext(ctx context.Context, actor *Actor, q AgentQuery) (*AgentTask, error) {
	id, err := agentIdentity(actor, q.Agent)
	if err != nil {
		return nil, err
	}
	if !q.Peek && actor.ReadOnly {
		return nil, invalid("a read-only token can only look (peek)")
	}
	now := s.nowMS()
	held, err := queryIDs(ctx, s.db.R, `SELECT id FROM issues WHERE claimed_by = ? AND claimed_until > ? ORDER BY claimed_until DESC`, id, now)
	if err != nil {
		return nil, err
	}
	var skipped []string
	for _, iid := range held {
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, itoa64(iid), RoleViewer, false)
		if err != nil {
			continue // not visible to this token
		}
		// Finished, taken off agents, handed elsewhere or put on hold since:
		// let it go.
		if i.Closed() || !s.stillDelegated(ctx, i, id) || i.Hold.on(s.now()) {
			if !q.Peek {
				if err := s.dropClaim(ctx, actor, i.ID, id, "released"); err != nil {
					return nil, err
				}
			}
			continue
		}
		// Shipped: its pull request is open or merged, or it moved on to In
		// Review, so the agent's part is done and asking again shouldn't hand
		// it back (WP-246, WP-267).
		handed, err := s.handedOn(ctx, s.db.R, i)
		if err != nil {
			return nil, err // can't tell if it moved on: keep the claim, ask again (WP-1406)
		}
		if s.outOnPR(ctx, s.db.R, i) != nil || handed {
			if !q.Peek {
				if err := s.dropClaim(ctx, actor, i.ID, id, "done"); err != nil {
					return nil, err
				}
			}
			continue
		}
		if (q.Project != "" && !strings.EqualFold(q.Project, i.ProjectKey)) || (q.Step != "" && !strings.EqualFold(q.Step, i.Step)) {
			continue // the agent asked for something else this time
		}
		if fits, err := s.heldFits(ctx, actor, i.Key, q); err != nil {
			return nil, err
		} else if !fits {
			continue // outside the repo or query asked for (another checkout's task)
		}
		if where := s.heldElsewhere(ctx, actor, iid, id); where != "" {
			skipped = append(skipped, i.Key+" ("+where+")")
			continue // another live session of this agent is on it (WP-89)
		}
		if !q.Peek {
			if err := s.renewClaim(ctx, actor, iid, id); err != nil {
				return nil, err
			}
		}
		t, err := s.agentTaskWith(ctx, actor, iid, true, "") // a resume draws no arm (WP-1035)
		if err != nil {
			return nil, err
		}
		t.Resumed, t.Skipped = true, skipped
		return t, nil
	}
	// A person who paused this agent stops it taking new work (WP-D15);
	// what it already holds (above) stays its own.
	if p, err := s.pauseFor(ctx, actor, id); err != nil {
		return nil, err
	} else if p != nil {
		return nil, &ErrAgentsPaused{Pause: p, Agent: id}
	}
	ready, _, err := s.agentWork(ctx, actor, []string{id}, q)
	if err != nil {
		return nil, err
	}
	today := s.now().UTC().Format(dateLayout)
	// Work this terminal left stranded (WP-567) comes first, so a /clear
	// still hands the task on to the session after it (WP-253).
	resumeHere := s.strandedHere(ctx, actor, id)
	if n := slices.IndexFunc(ready, func(qi *queuedIssue) bool { return resumeHere[qi.issue.ID] }); n > 0 {
		first := ready[n]
		ready = append([]*queuedIssue{first}, slices.Delete(ready, n, n+1)...)
	}
	for n, qi := range ready {
		i := qi.issue
		why := ""
		if resumeHere[i.ID] {
			why = "Resumes the work its session left in this checkout"
		} else if i.AgentOrder != nil {
			by, rerr := s.pinners(ctx, []int64{i.ID})
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "pinners failed", "err", rerr)
				}
			}
			why = whyQueued(i, n+1, by[i.ID], today, qi.planned, qi.unblocks)
		} else {
			why = whyQueued(i, 0, nil, today, qi.planned, qi.unblocks)
		}
		if !q.Peek {
			switch err := s.claim(ctx, actor, i.ID, id); {
			case errors.Is(err, errAlreadyClaimed), errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
				continue // someone got there first, it changed since, or this token can only read it
			case err != nil:
				return nil, err
			}
		}
		via := "next"
		if q.Peek {
			via = "" // only the call that makes the claim draws (WP-1035)
		}
		t, err := s.agentTaskWith(ctx, actor, i.ID, true, via)
		if t != nil {
			t.Why, t.Skipped = why, skipped
		}
		return t, err
	}
	return nil, nil
}

// AgentBatch is next plus the work related to it (prerequisites, related
// and duplicate issues, open sub-issues) that the same agent may take,
// claimed together. Issues the task itself blocks wait their turn.
func (s *Service) AgentBatch(ctx context.Context, actor *Actor, q AgentQuery) (*AgentTask, error) {
	t, err := s.AgentNext(ctx, actor, q)
	if err != nil || t == nil {
		return t, err
	}
	id, _ := agentIdentity(actor, q.Agent)
	ids := []int64{}
	for _, r := range t.Issue.Relations {
		// A link the text made isn't a claim that the tasks go together (WP-ADR81).
		if r.Kind != "blocks" && !r.Named && r.State != CatDone && r.State != CatCanceled {
			ids = append(ids, r.Issue.ID)
		}
	}
	for _, c := range t.Issue.Children {
		if !c.Closed() {
			ids = append(ids, c.ID)
		}
	}
	now := s.nowMS()
	for _, rid := range ids {
		if len(t.Related) >= 10 {
			break
		}
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, itoa64(rid), RoleViewer, false)
		if err != nil || !s.agentCanTake(ctx, i, id, now) {
			continue
		}
		if q.Within != "" && strings.TrimSpace(q.CrossProject) == "" {
			if fits, err := s.heldFits(ctx, actor, i.Key, AgentQuery{Query: q.Within}); err != nil || !fits {
				continue // relations cross projects; a claim needs a reason to (WP-117)
			}
		}
		if !q.Peek {
			if err := s.claim(ctx, actor, i.ID, id); err != nil {
				continue
			}
			if i, err = s.issueByID(ctx, s.db.R, rid, false); err != nil {
				return nil, err
			}
		}
		t.Related = append(t.Related, i)
	}
	return t, nil
}

// AgentTake claims a named task for the agent, as next does (lease, run,
// In Progress), even if it wasn't delegated: a person told the agent to
// work on it. Refused when it's closed or another agent holds it.
func (s *Service) AgentTake(ctx context.Context, actor *Actor, ref, agent string) (*AgentTask, error) {
	id, err := agentIdentity(actor, agent)
	if err != nil {
		return nil, err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleEditor, false)
	if err != nil {
		return nil, err
	}
	if !i.Closed() && s.hasOpenAgentChildren(ctx, i) {
		if t, err := s.takeUnder(ctx, actor, i, id); err != nil || t != nil {
			return t, err
		}
	}
	resumed := i.ClaimedBy == id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > s.nowMS()
	if err := s.claimIssue(ctx, actor, i.ID, id, true); err != nil {
		if errors.Is(err, errAlreadyClaimed) {
			return nil, invalid("%s was just taken by another agent", i.Key)
		}
		return nil, err
	}
	t, err := s.agentTaskWith(ctx, actor, i.ID, true, "take")
	if err != nil {
		return nil, err
	}
	t.Resumed = resumed
	return t, nil
}

// takeUnder hands out the first task under an epic whose sub-issues are
// open for agents, as wp next would (the one the agent holds there, else
// the queue's first), so no run opens on the epic itself: a session
// holding it would keep its finished sub-task from its review and the
// next task (WP-730). Nil when none can be taken now (blocked, held,
// another agent's): the epic is taken as named. A pause stops wp next,
// not a task a person named, so it falls back the same way.
func (s *Service) takeUnder(ctx context.Context, actor *Actor, epic *Issue, id string) (*AgentTask, error) {
	t, err := s.AgentNext(ctx, actor, AgentQuery{Agent: id, Query: "under:" + epic.Key})
	var paused *ErrAgentsPaused
	switch {
	case errors.As(err, &paused):
		return nil, nil
	case err != nil || t == nil:
		return nil, err
	}
	t.Why = fmt.Sprintf("A sub-task of %s: an epic's open sub-tasks are handed out before it (wp show %s)", epic.Key, epic.Key)
	return t, nil
}

// agentCanTake: what next would hand this agent: open, out of the backlog,
// not on hold, unblocked, with no open sub-issues for agents, not waiting
// to start as a fan-out's package (WP-148), eligible, and cleared by the
// safety screen.
func (s *Service) agentCanTake(ctx context.Context, i *Issue, id string, now int64) bool {
	if i.Closed() || i.Status.Category == CatBacklog || i.Hold.on(s.now()) || s.inReleaseWait(ctx, i.ID) || s.isBlocked(ctx, i.ID) || s.agentsPausedIn(ctx, i.ProjectID) ||
		s.hasOpenAgentChildren(ctx, i) || s.outOnPR(ctx, s.db.R, i) != nil {
		return false
	}
	if handed, err := s.handedOn(ctx, s.db.R, i); handed || err != nil {
		return false // moved on, or can't tell: not handed out (WP-1406)
	}
	if s.fanoutOf(ctx, i.ID) != 0 || !s.agentEligible(ctx, i, id, now) {
		return false
	}
	d, _ := s.safetyWait(ctx, s.db.R, i)
	return d == ""
}

// agentsPausedIn: the project's owners stopped agents taking new work there.
func (s *Service) agentsPausedIn(ctx context.Context, projectID int64) bool {
	var paused bool
	err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(json_extract(settings, '$.agents_paused'), 0) = 1 FROM projects WHERE id = ?`, projectID).Scan(&paused)
	if err != nil {
		if readFault(err) && !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "read agents paused in", "projectID", projectID, "err", err)
		}
		return true
	}
	return paused
}

// hasOpenAgentChildren: sub-issues handed to agents are still open, so they
// come first (WP-ADR35).
func (s *Service) hasOpenAgentChildren(ctx context.Context, i *Issue) bool {
	if i.ChildCount <= i.ChildDone {
		return false
	}
	n, err := s.openAgentChildren(ctx, []*Issue{i})
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read has open agent children", "issue", i.ID, "err", err)
		}
		return true
	}
	return n[i.ID] > 0
}

// isBlocked: an open issue blocks this one.
func (s *Service) isBlocked(ctx context.Context, issueID int64) bool {
	var blocked bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_relations r JOIN issues b ON b.id = r.source_id
		JOIN statuses st ON st.id = b.status_id WHERE r.target_id = ? AND r.kind = 'blocks' AND st.category NOT IN ('done','canceled'))`, issueID).Scan(&blocked)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read is blocked", "issueID", issueID, "err", err)
		}
		return true
	}
	return blocked
}

// outOnPR is the pull request a started issue is out on: an open, draft or
// merged one linked to it while it is in progress (In Review, typically),
// outside a workflow. Its agent work is done; it waits on the merge or on
// a person, so no agent is handed it until someone moves it back to To Do
// (WP-246). Nil when it isn't out on one.
func (s *Service) outOnPR(ctx context.Context, q queryer, i *Issue) *ExternalLink {
	prs, err := s.outOnPRs(ctx, q, []*Issue{i})
	if err != nil {
		return nil
	}
	return prs[i.ID]
}

// outOnPRs is outOnPR for many issues in one query per 500; an open pull
// request is named before a merged one. A pull request that merged in an
// earlier round of an objective isn't one the issue is out on (WP-ADR111).
func (s *Service) outOnPRs(ctx context.Context, q queryer, issues []*Issue) (map[int64]*ExternalLink, error) {
	out := map[int64]*ExternalLink{}
	var ids []int64
	for _, i := range issues {
		// A workflow hands its steps on with wp done (a review step may be
		// for another agent while the pull request is open): left to it.
		if i.Status.Category == CatInProgress && (i.WorkflowID == nil || i.Step == "") {
			ids = append(ids, i.ID)
		}
	}
	for chunk := range slices.Chunk(ids, 500) {
		// #nosec G202 -- placeholders only; values are bound
		rows, err := q.QueryContext(ctx, `SELECT issue_id, url, repo, number, state FROM external_links
			WHERE kind = 'pull' AND state IN ('', 'open', 'draft', 'merged') AND issue_id IN (`+placeholders(len(chunk))+`)
			AND NOT (state = 'merged' AND EXISTS (SELECT 1 FROM objectives o WHERE o.issue_id = external_links.issue_id
				AND o.state IN ('active', 'met', 'ended') AND external_links.created_at < o.round_started_at))
			ORDER BY state = 'merged', id DESC`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var l ExternalLink
			if err := rows.Scan(&id, &l.URL, &l.Repo, &l.Number, &l.State); err != nil {
				rows.Close()
				return nil, err
			}
			if out[id] == nil {
				l.Kind = "pull"
				out[id] = &l
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// handedOn: a started issue outside a workflow that moved on from the
// status a claim puts it in (In Review after In Progress). Whoever moved it
// (a pull request opening, the agent, a person) handed it on, so it waits
// for a person like one out on a pull request, even with no open or merged
// one linked: its pull request closed after the commits merged through
// another, or the work went in without one (WP-267).
//
// A positions read that fails is an error, not an answer either way: the
// caller neither drops a claim as done nor treats the issue as stranded
// (WP-1406). The Warn is here, once.
func (s *Service) handedOn(ctx context.Context, q queryer, i *Issue) (bool, error) {
	working, err := s.workingPositions(ctx, q)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read handed on", "issue", i.ID, "err", err)
		}
		return false, err
	}
	return pastWorking(i, working), nil
}

// workingPositions is the position of each project's working status: the
// first started one, where a claim moves an issue.
func (s *Service) workingPositions(ctx context.Context, q queryer) (map[int64]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT project_id, min(position) FROM statuses WHERE category = ? GROUP BY project_id`, CatInProgress)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var project int64
		var pos int
		if err := rows.Scan(&project, &pos); err != nil {
			return nil, err
		}
		out[project] = pos
	}
	return out, rows.Err()
}

// pastWorking is handedOn given the working statuses' positions.
func pastWorking(i *Issue, working map[int64]int) bool {
	pos, ok := working[i.ProjectID]
	return ok && i.Status.Category == CatInProgress && (i.WorkflowID == nil || i.Step == "") && i.Status.Position > pos
}

// prName is how a wait or a refusal names a pull request: acme/waypoint#134.
func prName(l *ExternalLink) string {
	if l.Repo != "" && l.Number != nil {
		return fmt.Sprintf("%s#%d", l.Repo, *l.Number)
	}
	return l.URL
}

// filters are the repo and query parts of an agent's request, as query
// terms.
func (q AgentQuery) filters() []string {
	var parts []string
	if q.Repo != "" {
		// One repo or several (an agent working across a group of repos).
		var repos []string
		for _, r := range strings.Split(q.Repo, ",") {
			if r = strings.TrimSpace(r); r != "" {
				repos = append(repos, fmt.Sprintf("%q", r))
			}
		}
		parts = append(parts, "repo:"+strings.Join(repos, ","))
	}
	if q.Query != "" {
		parts = append(parts, q.Query)
	}
	return parts
}

// heldFits says whether a task the agent holds matches the repo and query
// it asked with now, so a claim taken in another checkout (same token,
// another repo) isn't handed back here.
func (s *Service) heldFits(ctx context.Context, actor *Actor, key string, q AgentQuery) (bool, error) {
	f := q.filters()
	if len(f) == 0 {
		return true, nil
	}
	res, err := s.Search(ctx, actor, SearchParams{Query: "key:" + key + " " + strings.Join(f, " "), Limit: 1})
	if err != nil {
		return false, err
	}
	return res.Total > 0, nil
}

// stillDelegated: a task the agent holds is still meant for it. Unlike
// agentEligible, the step's same/different rule isn't checked again: it
// decides who may start a step, not who may finish it.
func (s *Service) stillDelegated(ctx context.Context, i *Issue, id string) bool {
	if agentHumansOnly(i.Agent) {
		return false
	}
	spec, _ := s.stepSpec(ctx, i)
	return AgentMatches(spec, id)
}

// agentEligible: unclaimed (or claimed by this agent), and the agent fits
// the spec of the issue or of its current step.
func (s *Service) agentEligible(ctx context.Context, i *Issue, id string, now int64) bool {
	if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now && i.ClaimedBy != id {
		return false
	}
	if agentHumansOnly(i.Agent) || i.Draft {
		return false // humans only, whatever a step says; drafts wait for a person (WP-42)
	}
	spec, rule := s.stepSpec(ctx, i)
	if !AgentMatches(spec, id) {
		return false
	}
	if rule != "" {
		var prev string
		err := s.db.R.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE issue_id = ? AND outcome = 'done' ORDER BY id DESC LIMIT 1`, i.ID).Scan(&prev)
		if err == nil {
			same := normalizeAgent(prev) == normalizeAgent(id)
			if (rule == "different" && same) || (rule == "same" && !same) {
				return false
			}
		}
	}
	return true
}

// stepSpec is the agent spec for an issue's current step: the step's own,
// or the issue's; rule is "same"/"different" when the step says so.
func (s *Service) stepSpec(ctx context.Context, i *Issue) (spec, rule string) {
	spec = issueAgentSpec(i.Agent) // Anyone is open to any agent
	if i.WorkflowID == nil || i.Step == "" {
		return spec, ""
	}
	w, err := s.workflowByID(ctx, s.db.R, *i.WorkflowID)
	if err != nil {
		return spec, ""
	}
	n := w.stepIndex(i.Step)
	if n < 0 {
		return spec, ""
	}
	switch st := w.Steps[n].Agent; strings.ToLower(st) {
	case "":
	case "same", "different":
		rule = strings.ToLower(st)
	default:
		spec = st
	}
	return spec, rule
}

// dropClaim ends a claim that no longer fits (the issue closed, stopped
// being delegated or shipped), with the run's outcome.
func (s *Service) dropClaim(ctx context.Context, actor *Actor, issueID int64, id, outcome string) error {
	return s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, issueID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.endRun(ctx, t, issueID, id, outcome, nil)
	})
}

var errAlreadyClaimed = errors.New("claimed by another agent")

// claim takes the lease, starts a run, and moves the issue into progress.
func (s *Service) claim(ctx context.Context, actor *Actor, issueID int64, id string) error {
	return s.claimIssue(ctx, actor, issueID, id, false)
}

// claimIssue is claim; explicit skips the delegation rules (a person named
// the task), keeping only "open and not held by another agent".
func (s *Service) claimIssue(ctx context.Context, actor *Actor, issueID int64, id string, explicit bool) error {
	return s.write(ctx, actor, func(t *tx) error {
		return s.claimIssueTx(ctx, t, actor, issueID, id, explicit)
	})
}

// claimIssueTx is claimIssue inside a write already open (AgentDone takes
// what the agent never took, then finishes it, in one transaction).
func (s *Service) claimIssueTx(ctx context.Context, t *tx, actor *Actor, issueID int64, id string, explicit bool) error {
	i, _, err := s.getIssueFor(ctx, t, actor, itoa64(issueID), RoleEditor, false)
	if err != nil {
		return err
	}
	if i.Draft {
		// Whoever asks (a runner acts as its owner), agents don't work on
		// drafts (WP-42); one that can't see it hears what a missing issue says.
		if actor.HidesDrafts() {
			return notFound("issue " + itoa64(issueID))
		}
		return invalid("%s is a draft; a person must publish it before an agent can take it", i.Key)
	}
	now := s.nowMS()
	// Checked again under the write lock: the issue may have closed, been
	// blocked or taken off agents since the candidates were listed.
	if explicit {
		if i.Closed() {
			return invalid("%s is already %s; reopen it first", i.Key, i.Status.Name)
		}
		if i.ClaimedBy != "" && i.ClaimedBy != id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
			return invalid("%s is being worked on by %s (claimed until %s)", i.Key, i.ClaimedBy, i.ClaimedUntil.Format("15:04 MST"))
		}
		if err := s.holdRefusal(actor, i); err != nil {
			return err
		}
	} else if !s.agentCanTake(ctx, i, id, now) {
		return errAlreadyClaimed
	}
	if explicit {
		if err := s.explicitStepRefusal(ctx, i, id); err != nil {
			return err
		}
		// A person naming the task in a session they drive is the authority
		// the screen stands in for; a task session or an automation isn't.
		if d, _ := s.safetyWait(ctx, t, i); d != "" && s.unattended(ctx, actor) {
			return invalid("%s isn't cleared for agents yet: %s", i.Key, d)
		}
	}
	// An objective's round is taken only when it is due, checked here under
	// the write lock (WP-ADR111). Whoever holds the round keeps it.
	if held := i.ClaimedBy == id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now; !held {
		if err := s.objectiveTakeRefusal(ctx, t, actor, i, explicit); err != nil {
			return err
		}
	}
	if explicit && !s.stillDelegated(ctx, i, id) {
		// Taking a task assigns it to the agent: delegated to it from now
		// on (recorded in its history like any other change).
		if err := s.applyPatch(ctx, t, actor, i, IssuePatch{Agent: &id}); err != nil {
			return err
		}
		if i, err = s.issueByID(ctx, t, i.ID, false); err != nil {
			return err
		}
	}
	// claimed_at only moves for a new claim: renewing keeps the timer.
	res, err := t.ExecContext(ctx, `UPDATE issues SET claimed_at = CASE WHEN claimed_by = ? AND claimed_until > ? AND claimed_at IS NOT NULL THEN claimed_at ELSE ? END,
		claimed_by = ?, claimed_until = ?
		WHERE id = ? AND (claimed_by = '' OR claimed_until IS NULL OR claimed_until < ? OR claimed_by = ?)`,
		id, now, now, id, now+ClaimLease.Milliseconds(), i.ID, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errAlreadyClaimed
	}
	// Coming back after its lease lapsed (before the sweep ended the run)
	// starts a new run: the gap wasn't work. A run held for its review
	// before the pull request goes on, as the sweep leaves it (WP-752): a
	// new run has no round, so its wp ship --pr would ask for the review
	// again.
	if live := i.ClaimedBy == id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now; !live {
		var until any
		if i.ClaimedBy == id && i.ClaimedUntil != nil {
			until = i.ClaimedUntil.UnixMilli()
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_runs SET outcome = 'expired', ended_at = min(?, coalesce(?, started_at + ?))
			WHERE issue_id = ? AND agent = ? AND ended_at IS NULL AND NOT `+runHeldForReview, now, until, ClaimLease.Milliseconds(), i.ID, id); err != nil {
			return err
		}
	}
	// Re-claiming what it already holds (batch asked twice) keeps the open run.
	opened, err := t.ExecContext(ctx, `INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, session_id) SELECT ?,?,?,?,?,?
		WHERE NOT EXISTS (SELECT 1 FROM agent_runs WHERE issue_id = ? AND agent = ? AND ended_at IS NULL)`,
		i.ID, id, i.Step, nullUser(actor.id()), now, nullUser(actor.Session), i.ID, id)
	if err != nil {
		return err
	}
	if actor.Session != 0 {
		// Taken from this session (a person named the task here): its run
		// is this session's from now on, even if another one had it (WP-89).
		if _, err := t.ExecContext(ctx, `UPDATE agent_runs SET session_id = ? WHERE issue_id = ? AND agent = ? AND ended_at IS NULL`, actor.Session, i.ID, id); err != nil {
			return err
		}
	}
	s.recordRun(ctx, t, actor, i.ID, id, opened)
	t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "claim", New: id})
	if i.Status.Category == CatTodo || i.Status.Category == CatBacklog {
		st := CatInProgress
		if err := s.applyPatch(ctx, t, actor, i, IssuePatch{Status: &st}); err != nil && !IsInvalid(err) {
			return err // a project without an in-progress status just stays put
		}
	}
	return nil
}

// explicitStepRefusal is why a person-named task can't go to this agent at
// its workflow step, or nil. A step meant for another agent stays theirs
// (taking it would only be dropped by the next `next`), and a step that
// needs a different agent than the one who did the previous step (a review)
// keeps its independence.
func (s *Service) explicitStepRefusal(ctx context.Context, i *Issue, id string) error {
	spec, rule := s.stepSpec(ctx, i)
	if spec != issueAgentSpec(i.Agent) && !AgentMatches(spec, id) {
		return invalid("%s is at workflow step %q, which is for %s; change that step's agent or the task's step first", i.Key, i.Step, spec)
	}
	if rule == "" {
		return nil
	}
	var prev string
	if err := s.db.R.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE issue_id = ? AND outcome = 'done' ORDER BY id DESC LIMIT 1`, i.ID).Scan(&prev); err != nil {
		return nil
	}
	same := normalizeAgent(prev) == normalizeAgent(id)
	if rule == "different" && same {
		return invalid("%s is at workflow step %q, which needs a different agent than %s, who did the step before; another agent can take it, or a person can change the step", i.Key, i.Step, prev)
	}
	if rule == "same" && !same {
		return invalid("%s is at workflow step %q, which needs the same agent as the step before (%s); that agent can take it, or a person can change the step", i.Key, i.Step, prev)
	}
	return nil
}

// claimableByPerson is the refusals of a person-named take, for reporting
// usage on a task the agent never took: closed, held by another agent, a
// draft, on hold, or a workflow step meant for another agent.
func (s *Service) claimableByPerson(ctx context.Context, actor *Actor, i *Issue, id string, now int64) error {
	if i.Draft {
		if actor.HidesDrafts() {
			return notFound("issue " + i.Key)
		}
		return invalid("%s is a draft; a person must publish it before an agent can work on it", i.Key)
	}
	if i.Closed() {
		return invalid("%s is already %s; reopen it first", i.Key, i.Status.Name)
	}
	if i.ClaimedBy != "" && i.ClaimedBy != id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
		return invalid("%s is being worked on by %s", i.Key, i.ClaimedBy)
	}
	if err := s.holdRefusal(actor, i); err != nil {
		return err
	}
	return s.explicitStepRefusal(ctx, i, id)
}

func (s *Service) renewClaim(ctx context.Context, actor *Actor, issueID int64, id string) error {
	_, err := s.db.W.ExecContext(ctx, `UPDATE issues SET claimed_until = ? WHERE id = ? AND claimed_by = ?`,
		s.nowMS()+ClaimLease.Milliseconds(), issueID, id)
	if err != nil || actor.Session == 0 {
		return err
	}
	// Resumed from this session (its own, or one that ended): the run is
	// this session's now, a paused one too, so the next wp next of another
	// session sees who holds it.
	res, err := s.db.W.ExecContext(ctx, `UPDATE agent_runs SET session_id = ? WHERE id = (`+holdingRun+`) AND coalesce(session_id, 0) != ?`, actor.Session, issueID, id, actor.Session)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.recordRunChanges(ctx, s.db.W, actor, issueID, id)
	}
	return nil
}

// holdingRun selects the run that says which session holds a claimed task:
// the agent's latest run on it, open or paused. A paused run (the session
// went idle waiting for its person, or is compacting its context) still
// holds the task; only a run that ended for good doesn't (WP-566).
const holdingRun = `SELECT h.id FROM (SELECT id, ended_at, outcome FROM agent_runs WHERE issue_id = ? AND agent = ? ORDER BY id DESC LIMIT 1) h
	WHERE h.ended_at IS NULL OR h.outcome = 'paused'`

// heldElsewhere says where, when the agent's open or paused run on the
// issue belongs to another of its sessions that hasn't ended: every Claude
// Code session on a machine can be the same agent identity, so the identity
// alone doesn't say which session holds a task. Empty when it is this
// session's to resume.
//
// Quiet isn't gone: a session reports at prompts, stops and a few tools, so
// one deep in its work (subagents, long commands) or waiting on a
// permission prompt goes unheard for well over half an hour while it still
// edits its checkout (WP-580). What lets a session that vanished without
// ending give its task up is the claim's lease, which only its own reports
// renew; until then, the person can still take the task by name.
func (s *Service) heldElsewhere(ctx context.Context, actor *Actor, issueID int64, id string) string {
	if actor.Session == 0 {
		return ""
	}
	var sess sql.NullInt64
	var state, cwd, machine string
	var seen int64
	err := s.db.R.QueryRowContext(ctx, `SELECT r.session_id, coalesce(x.state, ''), coalesce(x.last_seen_at, 0), coalesce(x.cwd, ''), coalesce(x.machine, '')
		FROM agent_runs r LEFT JOIN agent_sessions x ON x.id = r.session_id
		WHERE r.id = (`+holdingRun+`)`, issueID, id).Scan(&sess, &state, &seen, &cwd, &machine)
	now := s.nowMS()
	if err != nil || !sess.Valid || sess.Int64 == actor.Session || sessionState(state, seen, now) == SessionEnded {
		return ""
	}
	return sessionHoldsText("another session of yours", cwd, machine, seen, now)
}

// sessionHoldsText is "another session of yours in /x on a dev host holds it,
// last heard from 2h 10m ago".
func sessionHoldsText(who, cwd, machine string, seen, now int64) string {
	where := who
	switch {
	case cwd != "" && machine != "":
		where += " in " + cwd + " on " + machine
	case cwd != "":
		where += " in " + cwd
	}
	where += " holds it"
	if quiet := time.Duration(now-seen) * time.Millisecond; quiet >= sessionIdleAfter {
		where += ", last heard from " + fmtMinutes(quiet) + " ago"
	}
	return where
}

// sessionStillAbout: a session that held a task may still come back to it.
// One waiting for its person (a permission prompt, a question) is still
// there until it ends; any other one counts as gone after strandedIdle
// without a word, as the stranded sweep has it (WP-ADR54).
func sessionStillAbout(stored string, seen, now int64) bool {
	if sessionState(stored, seen, now) == SessionEnded {
		return false
	}
	return stored == SessionWaiting || now-seen < strandedIdle.Milliseconds()
}

// lapsedHolds are the tasks whose claim lapsed while the session holding
// them went quiet, keyed by issue id, with who holds them (WP-ADR54). Only a
// session's own reports renew its claim, so one stuck on a permission
// prompt loses its claim after the lease; it still has the task's work in
// its checkout and carries on once its person answers. Handing the task to
// another session meanwhile had two sessions doing the same work (WP-439 on
// 2026-11-07). A task stays the quiet session's while:
//   - its latest run is that session's and ended because the lease ran out
//     (or paused, its claim lapsing since);
//   - the session's last claim was this task (it hasn't moved on);
//   - the session is still about (sessionStillAbout).
//
// The asking session's own lapsed task isn't in it: it may take it back.
func (s *Service) lapsedHolds(ctx context.Context, actor *Actor) (map[int64]string, error) {
	holds, err := s.lapsedHoldRows(ctx, actor.Session)
	if err != nil {
		return nil, err
	}
	now := s.nowMS()
	out := map[int64]string{}
	for _, h := range holds {
		who := "another agent session"
		if h.userID == actor.id() {
			who = "another session of yours"
		}
		out[h.issueID] = sessionHoldsText(who, h.cwd, h.machine, h.seen, now) + "; its claim lapsed while it was quiet."
	}
	return out, nil
}

// lapsedHold is a task a quiet session still holds.
type lapsedHold struct {
	issueID, userID, seen int64
	cwd, machine          string
}

// lapsedHoldRows lists the tasks quiet sessions hold, other than notSession's.
func (s *Service) lapsedHoldRows(ctx context.Context, notSession int64) ([]lapsedHold, error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.issue_id, x.user_id, x.state, x.last_seen_at, x.cwd, x.machine
		FROM agent_sessions x JOIN agent_runs r ON r.session_id = x.id AND r.issue_id = x.issue_id
		WHERE x.state != ? AND x.last_seen_at > ? AND x.id != ?
		  AND r.outcome IN ('expired', 'paused') AND r.ended_at IS NOT NULL
		  AND r.id = (SELECT max(id) FROM agent_runs WHERE issue_id = r.issue_id)`,
		SessionEnded, now-sessionEndedAfter.Milliseconds(), notSession)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []lapsedHold
	for rows.Next() {
		var h lapsedHold
		var state string
		if err := rows.Scan(&h.issueID, &h.userID, &state, &h.seen, &h.cwd, &h.machine); err != nil {
			return nil, err
		}
		if sessionStillAbout(state, h.seen, now) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

func (s *Service) agentTask(ctx context.Context, actor *Actor, issueID int64) (*AgentTask, error) {
	return s.agentTaskWith(ctx, actor, issueID, true, "next")
}

// agentTaskWith builds a task's answer; nearby false leaves out the work in
// progress nearby, which reads the relation graph and may rebuild it, for
// callers inside a write transaction that don't show it (reserveRelated).
// Those callers are also the ones that must not write, so the task's goal
// context arm is drawn only when nearby is true (WP-1035); via is how the
// agent got the task (next, take).
func (s *Service) agentTaskWith(ctx context.Context, actor *Actor, issueID int64, nearby bool, via string) (*AgentTask, error) {
	d, err := s.GetIssue(ctx, actor, itoa64(issueID))
	if err != nil {
		return nil, err
	}
	if nearby {
		s.goalContextFor(ctx, actor, d, via)
	}
	t := &AgentTask{Issue: d, Design: s.DesignNotesFor(ctx, actor, issueID)}
	if t.Comments, err = s.ListComments(ctx, actor, d.Key); err != nil {
		return nil, err
	}
	if d.WorkflowID != nil {
		if w, err := s.workflowByID(ctx, s.db.R, *d.WorkflowID); err == nil {
			t.Workflow = w
			if n := w.stepIndex(d.Step); n >= 0 {
				t.Step, t.StepNo = &w.Steps[n], n+1
			}
		}
	}
	t.Named = s.namedIssues(ctx, actor, t)
	if nearby {
		t.Nearby = s.nearbyWork(ctx, actor, t)
	}
	t.Resume = s.strandedFor(ctx, s.db.R, issueID)
	t.ParentText = s.parentText(ctx, actor, d)
	return t, nil
}

// parentText is d's parent's description for its answer, "" when there is
// none or agents aren't given its text (see AgentTask.ParentText).
func (s *Service) parentText(ctx context.Context, actor *Actor, d *IssueDetail) string {
	if d.Parent == nil {
		return ""
	}
	p, _, err := s.getIssueFor(ctx, s.db.R, actor, d.Parent.Key, RoleViewer, true)
	if err != nil || p.Draft || p.Outside || strings.TrimSpace(p.Description) == "" {
		return ""
	}
	if wait, _ := s.safetyWait(ctx, s.db.R, p); wait != "" {
		return ""
	}
	return strings.TrimSpace(p.Description)
}

// namedIssues looks up the task keys the task's own text names (WP-545):
// keys the answer already lists, and ones the agent can't see or that
// aren't tasks, are left out.
func (s *Service) namedIssues(ctx context.Context, actor *Actor, t *AgentTask) []*Issue {
	d := t.Issue
	text := []string{d.Description}
	for _, c := range t.Comments {
		text = append(text, c.Body)
	}
	if t.Step != nil {
		text = append(text, t.Step.Instructions)
	}
	listed := map[string]bool{strings.ToUpper(d.Key): true}
	if d.Parent != nil {
		listed[strings.ToUpper(d.Parent.Key)] = true
	}
	for _, r := range d.Relations {
		listed[strings.ToUpper(r.Issue.Key)] = true
	}
	for _, c := range d.Children {
		listed[strings.ToUpper(c.Key)] = true
	}
	var out []*Issue
	looked := 0
	for _, m := range issueKeyRE.FindAllString(strings.Join(text, "\n"), -1) {
		if len(out) >= namedMax || looked >= namedLookups {
			break
		}
		if listed[m] {
			continue
		}
		listed[m] = true
		looked++
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, m, RoleViewer, false)
		if err != nil || (i.Key != m && listed[strings.ToUpper(i.Key)]) {
			continue // not a task the agent can see, or an old key of one it has
		}
		listed[strings.ToUpper(i.Key)] = true
		out = append(out, i)
	}
	return out
}

// AgentDoneInput reports finished work.
type AgentDoneInput struct {
	Agent   string      `json:"agent"`
	Comment string      `json:"comment"`
	Usage   *AgentUsage `json:"usage"`
}

// AgentResult says where an issue went.
type AgentResult struct {
	Key      string `json:"key"`
	Step     string `json:"step,omitempty"`    // the step it moved to
	Finished bool   `json:"finished"`          // the issue is done
	Waiting  string `json:"waiting,omitempty"` // the unmerged pull request it stays out on (left as it was)
	Status   string `json:"status"`            // its status now
	Message  string `json:"message"`           // one line for the agent
	// Autopilot: the session's project has autopilot on, so the session is
	// cleared and given the next task once it stops (WP-ADR9).
	Autopilot bool `json:"autopilot,omitempty"`
}

// AgentDone finishes the agent's part: the issue moves to its workflow's
// next step (released for whoever takes that step) or, after the last step
// or without a workflow, to Done. The comment and usage are recorded in the
// same transaction.
func (s *Service) AgentDone(ctx context.Context, actor *Actor, ref string, in AgentDoneInput) (*AgentResult, error) {
	id, err := agentIdentity(actor, in.Agent)
	if err != nil {
		return nil, err
	}
	if in.Usage != nil {
		if err := in.Usage.check(); err != nil {
			return nil, err
		}
	}
	res := &AgentResult{}
	var issueID int64
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		live := i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > s.nowMS()
		if i.ClaimedBy != "" && i.ClaimedBy != id && live {
			return invalid("%s is claimed by %s", i.Key, i.ClaimedBy)
		}
		if i.Closed() {
			return invalid("%s is already %s", i.Key, i.Status.Name)
		}
		// Out on a pull request that hasn't merged: the merge (or a person)
		// finishes it, not wp done. The agent's part ends here: its run and
		// claim do, and the issue stays where it is (WP-246).
		if pr := s.outOnPR(ctx, t, i); pr != nil && pr.State != "merged" {
			res.Key, issueID, res.Waiting = i.Key, i.ID, prName(pr)
			if c := strings.TrimSpace(in.Comment); c != "" {
				if _, err := s.addComment(ctx, t, actor, i, c); err != nil {
					return err
				}
			}
			if err := s.endRun(ctx, t, i.ID, id, "done", in.Usage); err != nil {
				return err
			}
			_, err = t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, i.ID, id)
			return err
		}
		// Finishing work the agent never took is next KEY then done: a person
		// told it to do the task, so it can always mark it done. The take is
		// recorded as usual (run, assignment, In Progress), so usage and
		// activity still count. A workflow step's own rule still holds (a
		// review by a different agent), and so do drafts.
		if held := i.ClaimedBy == id && live; !held {
			if err := s.claimIssueTx(ctx, t, actor, i.ID, id, true); err != nil {
				if errors.Is(err, errAlreadyClaimed) {
					return invalid("%s was just taken by another agent", i.Key)
				}
				return err
			}
			if i, err = s.issueByID(ctx, t, i.ID, false); err != nil {
				return err
			}
		}
		res.Key, issueID = i.Key, i.ID
		var p IssuePatch
		finished := true
		if i.WorkflowID != nil {
			w, err := s.workflowByID(ctx, t, *i.WorkflowID)
			if err != nil {
				return err
			}
			n := w.stepIndex(i.Step)
			if n < 0 {
				return invalid("%s is at step %q, which the %s workflow no longer has; set its step first", i.Key, i.Step, w.Name)
			}
			if n < len(w.Steps)-1 {
				next := w.Steps[n+1].Name
				p.Step, res.Step, finished = &next, next, false
			}
		}
		if finished {
			done := CatDone
			p.Status = &done
		}
		if err := s.applyPatch(ctx, t, actor, i, p); err != nil {
			return err
		}
		if c := strings.TrimSpace(in.Comment); c != "" {
			if _, err := s.addComment(ctx, t, actor, i, c); err != nil {
				return err
			}
		}
		if err := s.endRun(ctx, t, i.ID, id, "done", in.Usage); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID)
		res.Finished = finished
		return err
	})
	if err != nil {
		return nil, err
	}
	i, err := s.issueByID(ctx, s.db.R, issueID, false)
	if err != nil {
		return nil, err
	}
	res.Status = i.Status.Name
	switch {
	case res.Waiting != "":
		res.Message = fmt.Sprintf("%s stays %s: %s hasn't merged, and the merge finishes it.", res.Key, res.Status, res.Waiting)
	case res.Finished && !i.Closed():
		// Its objective isn't met: the round is done, the task isn't.
		res.Finished = false
		if res.Message = s.objectiveReopened(ctx, i.ID); res.Message == "" {
			res.Message = fmt.Sprintf("%s is %s.", res.Key, res.Status)
		}
	case res.Finished:
		res.Message = fmt.Sprintf("%s done (%s).", res.Key, res.Status)
	default:
		res.Message = fmt.Sprintf("%s moved to step %s and released for the next agent.", res.Key, res.Step)
	}
	if actor.Session != 0 && s.autopilotSessionNote(ctx, actor.Session, i.ProjectID) != "" {
		res.Autopilot = true
	}
	return res, nil
}

// AgentReportUsage adds tokens and cost to the agent's latest run on an
// issue (tools call this from hooks, so the agent itself spends nothing).
func (s *Service) AgentReportUsage(ctx context.Context, actor *Actor, ref, agent string, u AgentUsage) error {
	id, err := agentIdentity(actor, agent)
	if err != nil {
		return err
	}
	if err := u.check(); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		// Reporting is activity: it keeps the claim alive.
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_until = ? WHERE id = ? AND claimed_by = ? AND claimed_until > ?`,
			now+ClaimLease.Milliseconds(), i.ID, id, now); err != nil {
			return err
		}
		if u.ReportID != "" {
			res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO agent_usage_reports(issue_id, agent, report_id, created_at) VALUES (?,?,?,?)`,
				i.ID, id, u.ReportID, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil // already counted
			}
		}
		var runID int64
		err = t.QueryRowContext(ctx, `SELECT id FROM agent_runs WHERE issue_id = ? AND agent = ? ORDER BY id DESC LIMIT 1`, i.ID, id).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			// No run yet (the agent never ran next KEY): a person told it to
			// work on this, so the usage is recorded like a take would.
			if err := s.claimableByPerson(ctx, actor, i, id, now); err != nil {
				return err
			}
			_, err = t.ExecContext(ctx, `INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, ended_at, input_tokens, output_tokens, cost_usd, outcome, model)
				VALUES (?,?,?,?,?,?,?,?,?,'',?)`, i.ID, id, i.Step, nullUser(actor.id()), now, now, u.InputTokens, u.OutputTokens, u.CostUSD, u.Model)
			return err
		} else if err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE agent_runs SET input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cost_usd = cost_usd + ?,
			model = CASE WHEN ? != '' THEN ? ELSE model END, session_id = coalesce(session_id, ?) WHERE id = ?`,
			u.InputTokens, u.OutputTokens, u.CostUSD, u.Model, u.Model, nullUser(actor.Session), runID)
		return err
	})
}

func (u AgentUsage) check() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CostUSD < 0 {
		return invalid("usage can't be negative")
	}
	if len(u.ReportID) > 100 {
		return invalid("report_id can be at most 100 characters")
	}
	if len(u.Model) > 100 {
		return invalid("model can be at most 100 characters")
	}
	return nil
}

// endRun closes the agent's open run on an issue (or records one, if the
// agent never claimed it) with an outcome and usage, and notes the versions
// it ended on where they changed.
func (s *Service) endRun(ctx context.Context, t *tx, issueID int64, id, outcome string, u *AgentUsage) error {
	if err := s.closeRun(ctx, t, issueID, id, outcome, u); err != nil {
		return err
	}
	s.recordRunChanges(ctx, t, t.actor, issueID, id)
	return nil
}

func (s *Service) closeRun(ctx context.Context, t *tx, issueID int64, id, outcome string, u *AgentUsage) error {
	if u == nil {
		u = &AgentUsage{}
	}
	now := s.nowMS()
	// The session it ends in, if the claim's session wasn't known: a task
	// finished in a session counts for that session's size note (WP-34).
	var sess any
	if t.actor != nil {
		sess = nullUser(t.actor.Session)
	}
	// Its own timer on the issue stops with it, and nothing paused there
	// restarts later.
	if _, err := t.ExecContext(ctx, `UPDATE time_entries SET ended_at = ? WHERE user_id = ? AND agent = ? AND issue_id = ? AND ended_at IS NULL`,
		now, t.actor.id(), id, issueID); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE time_entries SET paused = 0 WHERE agent = ? AND issue_id = ? AND paused = 1`, id, issueID); err != nil {
		return err
	}
	res, err := t.ExecContext(ctx, `UPDATE agent_runs SET ended_at = ?, outcome = ?,
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cost_usd = cost_usd + ?,
		model = CASE WHEN ? != '' THEN ? ELSE model END, session_id = coalesce(session_id, ?)
		WHERE id = (SELECT id FROM agent_runs WHERE issue_id = ? AND agent = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1)`,
		now, outcome, u.InputTokens, u.OutputTokens, u.CostUSD, u.Model, u.Model, sess, issueID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// Paused: the latest run takes the outcome and usage.
	res, err = t.ExecContext(ctx, `UPDATE agent_runs SET outcome = ?,
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cost_usd = cost_usd + ?,
		model = CASE WHEN ? != '' THEN ? ELSE model END, session_id = coalesce(session_id, ?)
		WHERE id = (SELECT id FROM agent_runs WHERE issue_id = ? AND agent = ? ORDER BY id DESC LIMIT 1) AND outcome = 'paused'`,
		outcome, u.InputTokens, u.OutputTokens, u.CostUSD, u.Model, u.Model, sess, issueID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var step string
	if err := t.QueryRowContext(ctx, `SELECT step FROM issues WHERE id = ?`, issueID).Scan(&step); err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, ended_at, input_tokens, output_tokens, cost_usd, outcome, model, session_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, issueID, id, step, nullUser(t.actor.id()), now, now, u.InputTokens, u.OutputTokens, u.CostUSD, outcome, u.Model, sess)
	if err == nil {
		s.recordRunFeatures(ctx, t, t.actor, issueID, id)
	}
	return err
}

// agentRuns lists an issue's runs, newest first.
func (s *Service) agentRuns(ctx context.Context, q queryer, issueID int64) ([]AgentRun, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, agent, step, started_at, ended_at, input_tokens, output_tokens, cost_usd, outcome, model
		FROM agent_runs WHERE issue_id = ? ORDER BY id DESC LIMIT 100`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.nowMS()
	out := []AgentRun{}
	for rows.Next() {
		var r AgentRun
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Agent, &r.Step, &start, &end, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.Outcome, &r.Model); err != nil {
			return nil, err
		}
		r.StartedAt, r.EndedAt = msTime(start), ptrTime(end)
		stop := now
		if end.Valid {
			stop = end.Int64
		}
		r.Seconds = max(0, (stop-start)/1000)
		out = append(out, r)
	}
	return out, rows.Err()
}

// expireRuns closes runs whose claim lapsed, and stops agents' clocks
// that ran past AgentTimerLimit; the scheduler calls it.
func (s *Service) expireRuns(ctx context.Context) error {
	s.noteDeployDuringRuns(ctx)
	now := s.nowMS()
	// A day without a pause means nobody stopped the clock: it stops at the
	// limit. The claim stays; the agent's next sign of work starts a new run.
	limit := AgentTimerLimit.Milliseconds()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE agent_runs SET outcome = 'timeout', ended_at = started_at + ?
		WHERE ended_at IS NULL AND started_at < ?`, limit, now-limit); err != nil {
		return err
	}
	if err := s.stopLongAgentTimers(ctx); err != nil {
		return err
	}
	// A run ends when its lease ran out: the last renewal plus the lease if
	// the agent still holds the (lapsed) claim, otherwise one lease long.
	_, err := s.db.W.ExecContext(ctx, `UPDATE agent_runs SET outcome = 'expired', ended_at = min(?, coalesce(
			(SELECT i.claimed_until FROM issues i WHERE i.id = agent_runs.issue_id AND i.claimed_by = agent_runs.agent),
			started_at + ?))
		WHERE ended_at IS NULL AND started_at < ? AND NOT `+runHeldForReview+` AND NOT EXISTS (
			SELECT 1 FROM issues i WHERE i.id = agent_runs.issue_id AND i.claimed_by = agent_runs.agent AND i.claimed_until > ?)`,
		now, ClaimLease.Milliseconds(), now-ClaimLease.Milliseconds(), now)
	if err != nil {
		return err
	}
	// Then the lapsed claims themselves, so nobody shows as working on them.
	_, err = s.db.W.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL
		WHERE claimed_by != '' AND claimed_until IS NOT NULL AND claimed_until <= ?
			AND NOT EXISTS (SELECT 1 FROM agent_runs WHERE agent_runs.issue_id = issues.id AND agent_runs.agent = issues.claimed_by AND agent_runs.ended_at IS NULL AND `+runHeldForReview+`)`, now)
	return err
}

// AgentPause stops the agent's clock on a task it holds, keeping the task:
// tools call it when the agent goes idle (a Claude Code session waiting
// for its person), and AgentResume when it works again. Its own timer on
// the task (wp timer) pauses and resumes with it.
func (s *Service) AgentPause(ctx context.Context, actor *Actor, ref, agent string) (string, error) {
	id, err := agentIdentity(actor, agent)
	if err != nil {
		return "", err
	}
	var key string
	var timerPaused *Event
	err = s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		key = i.Key
		now := s.nowMS()
		if i.ClaimedBy != id || i.ClaimedUntil == nil || i.ClaimedUntil.UnixMilli() <= now {
			return invalid("%s isn't claimed by %s", i.Key, id)
		}
		// A run held for its review before the pull request (WP-688) is
		// waiting on Waypoint, not its person: it stays open, or the review
		// has nothing to belong to (WP-752).
		res, err := t.ExecContext(ctx, `UPDATE agent_runs SET ended_at = ?, outcome = 'paused' WHERE issue_id = ? AND agent = ? AND ended_at IS NULL
			AND NOT `+runHeldForReview, now, i.ID, id)
		if err != nil {
			return err
		}
		pausedTimer, err := t.ExecContext(ctx, `UPDATE time_entries SET ended_at = ?, paused = 1 WHERE user_id = ? AND agent = ? AND issue_id = ? AND ended_at IS NULL`,
			now, actor.id(), id, i.ID)
		if err != nil {
			return err
		}
		if n, _ := pausedTimer.RowsAffected(); n > 0 {
			timerPaused = &Event{Kind: EvTimerPaused, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Data: map[string]any{"agent": id}}
		}
		// Going idle is activity too: the claim lasts a lease from now.
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_until = ? WHERE id = ?`, now+ClaimLease.Milliseconds(), i.ID); err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.recordRunChanges(ctx, t, actor, i.ID, id)
			t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "claim", New: id})
			t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if timerPaused != nil {
		s.automate(ctx, actor, timerPaused)
	}
	return key + " paused; wp resume (or the next prompt) restarts the clock.", nil
}

// AgentResumeOwn is AgentResume for the Claude Code hook at a session's
// next prompt (WP-720): the task is the directory's, which may be one
// another session there took and left. It restarts the clock only when the
// task's newest run is this session's (it paused it, or /clear carried it
// over) or has no session on record; a task another session last worked on stays
// as it is, rather than becoming a run this session never took (which
// kept autopilot from moving it on).
func (s *Service) AgentResumeOwn(ctx context.Context, actor *Actor, ref, agent string) (string, error) {
	if actor == nil || actor.Session == 0 {
		// Ownership can't be told without a session: leave the task as it is.
		return "", invalid("no session to resume %s in; wp resume %s takes it on purpose", ref, ref)
	}
	id, err := agentIdentity(actor, agent)
	if err != nil {
		return "", err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleEditor, false)
	if err != nil {
		return "", err
	}
	var sid sql.NullInt64
	err = s.db.R.QueryRowContext(ctx, `SELECT session_id FROM agent_runs WHERE issue_id = ? AND agent = ? ORDER BY id DESC LIMIT 1`, i.ID, id).Scan(&sid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", err
	case sid.Valid && sid.Int64 != actor.Session:
		return "", invalid("%s was last worked on in another session; its clock stays stopped (wp resume %s takes it here)", i.Key, i.Key)
	}
	return s.AgentResume(ctx, actor, ref, agent)
}

// AgentResume restarts the agent's clock on a task it paused. If the claim
// lapsed meanwhile and nobody else took the task, it's claimed again.
func (s *Service) AgentResume(ctx context.Context, actor *Actor, ref, agent string) (string, error) {
	id, err := agentIdentity(actor, agent)
	if err != nil {
		return "", err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleEditor, false)
	if err != nil {
		return "", err
	}
	now := s.nowMS()
	held := i.ClaimedBy == id && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now
	if held {
		// Another live session of this agent holds the paused task: resuming
		// here would take it over (WP-566).
		if where := s.heldElsewhere(ctx, actor, i.ID, id); where != "" {
			return "", invalid("%s: %s", i.Key, where)
		}
	}
	if !held {
		// claimIssue refuses closed tasks and other agents' claims, and
		// opens a run.
		if err := s.claimIssue(ctx, actor, i.ID, id, true); err != nil {
			return "", err
		}
	}
	err = s.write(ctx, actor, func(t *tx) error {
		// The run goes on in this session (WP-566): without it, the task
		// would read as no session's and another one could take it over.
		opened, err := t.ExecContext(ctx, `INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, session_id) SELECT ?,?,?,?,?,?
			WHERE NOT EXISTS (SELECT 1 FROM agent_runs WHERE issue_id = ? AND agent = ? AND ended_at IS NULL)`,
			i.ID, id, i.Step, nullUser(actor.id()), now, nullUser(actor.Session), i.ID, id)
		if err != nil {
			return err
		}
		s.recordRun(ctx, t, actor, i.ID, id, opened)
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_until = ? WHERE id = ? AND claimed_by = ?`, now+ClaimLease.Milliseconds(), i.ID, id); err != nil {
			return err
		}
		// Its own timer on the task, if pausing stopped it (and it hasn't
		// started another since).
		var entry int64
		err = t.QueryRowContext(ctx, `SELECT id FROM time_entries WHERE user_id = ? AND agent = ? AND issue_id = ? AND paused = 1
			AND NOT EXISTS (SELECT 1 FROM time_entries r WHERE r.user_id = ? AND r.agent = ? AND r.ended_at IS NULL)
			ORDER BY id DESC LIMIT 1`, actor.id(), id, i.ID, actor.id(), id).Scan(&entry)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if _, err := t.ExecContext(ctx, `INSERT INTO time_entries(user_id, agent, issue_id, project_id, note, started_at, created_at)
				SELECT user_id, agent, issue_id, project_id, note, ?, ? FROM time_entries WHERE id = ?`, now, now, entry); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvTimer, UserID: actor.id()})
		}
		if _, err := t.ExecContext(ctx, `UPDATE time_entries SET paused = 0 WHERE user_id = ? AND agent = ? AND issue_id = ? AND paused = 1`, actor.id(), id, i.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "claim", New: id})
		return nil
	})
	if err != nil {
		return "", err
	}
	return i.Key + " resumed.", nil
}
