package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Issue types.
var issueTypes = []string{"epic", "story", "task", "bug", "subtask"}

// Priorities, indexed by value.
var priorityNames = []string{"none", "low", "medium", "high", "urgent"}

// ParsePriority accepts a name ("high"), a prefix ("hi"), or 0-4.
func ParsePriority(v string) (int, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(priorityNames) {
		return n, nil
	}
	for i, name := range priorityNames {
		if v == name || (len(v) >= 2 && strings.HasPrefix(name, v)) {
			return i, nil
		}
	}
	switch v {
	case "critical", "highest", "blocker":
		return 4, nil
	case "lowest", "trivial":
		return 1, nil
	}
	return 0, invalid("unknown priority %q (use none, low, medium, high, urgent)", v)
}

// PriorityName returns the name for a priority value.
func PriorityName(p int) string {
	if p < 0 || p >= len(priorityNames) {
		return "none"
	}
	return priorityNames[p]
}

// IssueRef is a short reference to an issue.
type IssueRef struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// Issue is a unit of work: an epic, story, task, bug or subtask.
type Issue struct {
	ID          int64     `json:"id"`
	Key         string    `json:"key"`
	ProjectID   int64     `json:"project_id"`
	ProjectKey  string    `json:"project_key"`
	Number      int64     `json:"number"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      Status    `json:"status"`
	Priority    int       `json:"priority"`
	ParentID    *int64    `json:"parent_id"`
	Parent      *IssueRef `json:"parent,omitempty"`
	SprintID    *int64    `json:"sprint_id"`
	SprintName  string    `json:"sprint_name,omitempty"`
	Reporter    *UserRef  `json:"reporter,omitempty"`
	Assignees   []UserRef `json:"assignees"`
	Labels      []Label   `json:"labels"`
	Points      *float64  `json:"points"`
	StartDate   *string   `json:"start_date"`
	DueDate     *string   `json:"due_date"`
	Rank        float64   `json:"rank"`
	ChildCount  int       `json:"child_count"`
	// Blocks lists the keys of issues this one blocks (in list results, for
	// timeline dependency lines); only ones the viewer can see.
	Blocks     []string   `json:"blocks,omitempty"`
	ChildDone  int        `json:"child_done"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	// Agent says who may take it (WP-D7, WP-93): "" = anyone, "none" = humans
	// only, "any" or a spec = those agents.
	Agent         string     `json:"agent,omitempty"`
	ReporterAgent string     `json:"reporter_agent,omitempty"`
	Category      string     `json:"category,omitempty"`
	WorkflowID    *int64     `json:"workflow_id,omitempty"`
	Step          string     `json:"step,omitempty"`
	ClaimedBy     string     `json:"claimed_by,omitempty"`
	ClaimedUntil  *time.Time `json:"claimed_until,omitempty"`
	// ClaimedAt is when the current claim began.
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// ClaimWorked is how long (seconds) the agent has worked on its current
	// claim, not counting the stretch running since ClaimWorkingSince; that
	// is nil while the agent is idle (paused).
	ClaimWorked       int64      `json:"claim_worked,omitempty"`
	ClaimWorkingSince *time.Time `json:"claim_working_since,omitempty"`
	// AgentOrder is set when a person pinned it in the agents' queue
	// (WP-D15, WP-311): it goes after AgentAhead automatically ordered tasks
	// (0 = before all of them), among pins with the same AgentAhead lowest
	// AgentOrder first; nil = the automatic order.
	AgentOrder *float64 `json:"agent_order,omitempty"`
	AgentAhead int      `json:"agent_ahead,omitempty"`
	// Provenance (WP-D11). TextAgent is the agent that wrote the title and
	// description ("" = a person); Unreviewed: no person has reviewed that
	// text yet. ResolvedAgent is the agent that closed the issue;
	// CloseUnreviewed: an agent closed it and no person has reviewed the
	// close yet (WP-272).
	TextAgent       string `json:"text_agent,omitempty"`
	Unreviewed      bool   `json:"unreviewed,omitempty"`
	ResolvedAgent   string `json:"resolved_agent,omitempty"`
	CloseUnreviewed bool   `json:"close_unreviewed,omitempty"`
	// Draft: a person is still thinking it through; agents can't see it
	// until a person publishes it (WP-42).
	Draft bool `json:"draft,omitempty"`
	// Outside: a draft because its text came from a forge user who isn't a
	// Waypoint person (WP-279), OutsideBy (their login, "" when not known
	// yet) on the forge OutsideForge; a person approves it by publishing.
	Outside      bool   `json:"outside,omitempty"`
	OutsideBy    string `json:"outside_by,omitempty"`
	OutsideForge string `json:"outside_forge,omitempty"`
	// Hold keeps it from agents until it ends (WP-269); nil when it isn't
	// on hold, or the hold has run out.
	Hold *Hold `json:"hold,omitempty"`
	// Excerpt is the start of the description, on list results that don't
	// carry it whole, so a row can show more than its title on hover (WP-67).
	Excerpt string `json:"excerpt,omitempty"`

	reporterID, textBy, reviewedBy, resolvedBy, closeReviewedBy *int64
	reviewedAt, closeReviewedAt                                 *time.Time
}

// Closed reports whether the issue is done or canceled.
func (i *Issue) Closed() bool { return isClosed(i.Status.Category) }

// Ref returns a short reference.
func (i *Issue) Ref() IssueRef { return IssueRef{ID: i.ID, Key: i.Key, Title: i.Title, Type: i.Type} }

// IssueDetail is an issue with everything shown on its page.
type IssueDetail struct {
	*Issue
	Role      string         `json:"role"` // the actor's access: owner, editor, viewer
	Ancestors []IssueRef     `json:"ancestors"`
	Children  []*Issue       `json:"children"`
	Relations []Relation     `json:"relations"`
	Links     []ExternalLink `json:"links"`
	// ForgeMentions are the forge items that name the issue without being
	// linked to it (WP-379): a key in passing in a body, a comment.
	ForgeMentions []ForgeMention `json:"forge_mentions"`
	Watchers      []UserRef      `json:"watchers"`
	Shares        []Share        `json:"shares"`
	Watching      bool           `json:"watching"`
	Files         []Attachment   `json:"attachments"`
	AgentRuns     []AgentRun     `json:"agent_runs"`
	Pages         []PageRef      `json:"pages"`
	// TimeSpent is total tracked seconds, including running timers.
	TimeSpent int64 `json:"time_spent"`
	// Provenance (WP-D11): whose token wrote the text last, the person
	// who reviewed agent-written text, and who closed the issue.
	TextBy     *UserRef   `json:"text_by,omitempty"`
	ReviewedBy *UserRef   `json:"reviewed_by,omitempty"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	ResolvedBy *UserRef   `json:"resolved_by,omitempty"`
	// CloseReviewedBy is the person who reviewed an agent's close (WP-272).
	CloseReviewedBy *UserRef   `json:"close_reviewed_by,omitempty"`
	CloseReviewedAt *time.Time `json:"close_reviewed_at,omitempty"`
	// Tested is the newest test evidence a ship left on this task (WP-715).
	Tested *TestEvidence `json:"tested,omitempty"`
	// Safety is the safety screen of the text agents wrote and no person
	// reviewed (WP-ADR10); nil when there is none.
	Safety *TaskSafety `json:"safety,omitempty"`
	// Risk is its provenance risk (WP-554).
	Risk *ProvenanceRisk `json:"risk"`
	// Goal is how it traces to a goal (WP-382), when an approved goal
	// covers its project; nil otherwise.
	Goal *IssueGoal `json:"goal,omitempty"`
	// FiledFrom: an agent filed it while working in another repo (WP-378).
	FiledFrom *FiledFrom `json:"filed_from,omitempty"`
	// Written: what was live when it was written, and now (WP-460).
	Written *Written `json:"written,omitempty"`
	// Lineage: the tasks an agent wrote its text while working on (WP-275);
	// nil when a person wrote or reviewed it, or no task was behind it.
	Lineage *Lineage `json:"lineage,omitempty"`
	// Objective: it is worked in rounds until a check is met, or as a loop
	// (WP-ADR111); nil for an ordinary task.
	Objective *Objective `json:"objective,omitempty"`
	// Intent says what it's for in a sentence or two, apart from the
	// description (WP-1196); IntentAgent is the agent that wrote it ("" = a
	// person), shown as a suggestion until a person keeps or rewrites it.
	Intent      string `json:"intent,omitempty"`
	IntentAgent string `json:"intent_agent,omitempty"`
}

// Relation links two issues.
type Relation struct {
	ID    int64    `json:"id"`
	Kind  string   `json:"kind"` // as this issue reads it; see relationKindList
	Issue IssueRef `json:"issue"`
	// Subkind narrows the kind: a follow-up's fix or expansion (WP-1036).
	Subkind string `json:"subkind,omitempty"`
	State   string `json:"state"` // category of the other issue's status
	// Status names the other issue's status; ResolvedAt is when it finished.
	Status     string     `json:"status"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Named: made because an issue's text names the other (WP-781).
	Named bool `json:"-"`
	// Found says how Waypoint knew a follow-up (WP-255): "session" an agent
	// filed it while working on the other, "files" its pull request changed
	// mostly the same files, "after" someone related it to the other, done
	// before it was filed (WP-1003); "" someone chose it.
	Found string `json:"found,omitempty"`
	// Note is what Waypoint saw when it made the relation itself (found
	// "rewrite", WP-1370).
	Note string `json:"note,omitempty"`
	// Basis is who stands behind it, as for links (WP-ADR50): person,
	// agent, automation or inferred (Waypoint found it); "" made before
	// Waypoint recorded that. Score is how sure Waypoint was, where it
	// measured it.
	Basis        string    `json:"basis,omitempty"`
	CreatedBy    *UserRef  `json:"created_by,omitempty"`
	CreatedAgent string    `json:"created_agent,omitempty"`
	Score        *float64  `json:"score,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Share gives one user access to an issue subtree.
type Share struct {
	User UserRef `json:"user"`
	Role string  `json:"role"`
}

var (
	keyPattern     = lazyre.New(`^([A-Za-z][A-Za-z0-9]{1,9})-(\d+)$`)
	projectKeyOnly = lazyre.New(`^[A-Z][A-Z0-9]{1,9}$`)
)

// keyCandidates is what an issue reference could mean as (project key,
// number): "APP-12" one way, any case; "APP12" every way a key can run into
// the number ("AB212": AB2-12 or AB-212). Nil if it can't be a key.
func keyCandidates(ref string) [][2]string {
	ref = strings.ToUpper(strings.TrimSpace(ref))
	if m := keyPattern.FindStringSubmatch(ref); m != nil {
		return [][2]string{{m[1], m[2]}}
	}
	var out [][2]string
	for i := len(ref) - 1; i >= 2 && ref[i] >= '0' && ref[i] <= '9'; i-- {
		if projectKeyOnly.MatchString(ref[:i]) {
			out = append(out, [2]string{ref[:i], ref[i:]})
		}
	}
	return out
}

// looksLikeKey says whether ref could be an issue key, with or without its dash.
func looksLikeKey(ref string) bool { return len(keyCandidates(ref)) > 0 }

// issueSelect lists issue columns; description only when detail is wanted.
func issueSelect(withDesc bool) string {
	desc, peek := "''", "substr(i.description, 1, 600)"
	if withDesc {
		desc, peek = "i.description", "''"
	}
	return `SELECT i.id, i.project_id, p.key, i.number, i.type, i.title, ` + desc + `,
		st.id, st.name, st.category, st.color, st.position, i.priority,
		i.parent_id, pp.key || '-' || par.number, par.title, par.type,
		i.sprint_id, sp.name, i.reporter_id, i.points, i.start_date, i.due_date, i.rank,
		i.created_at, i.updated_at, i.resolved_at,
		(SELECT count(*) FROM issues c WHERE c.parent_id = i.id),
		(SELECT count(*) FROM issues c JOIN statuses cs ON cs.id = c.status_id WHERE c.parent_id = i.id AND cs.category IN ('done','canceled')),
		i.agent, i.reporter_agent, i.category, i.workflow_id, i.step, i.claimed_by, i.claimed_until, i.claimed_at,
		(SELECT coalesce(sum(r.ended_at - r.started_at), 0) FROM agent_runs r WHERE i.claimed_by != '' AND r.issue_id = i.id
			AND r.agent = i.claimed_by AND r.ended_at IS NOT NULL AND r.started_at >= i.claimed_at),
		(SELECT max(r.started_at) FROM agent_runs r WHERE i.claimed_by != '' AND r.issue_id = i.id AND r.agent = i.claimed_by AND r.ended_at IS NULL),
		i.text_by, i.text_agent, i.reviewed_by, i.reviewed_at, i.resolved_by, i.resolved_agent, i.close_review, i.close_reviewed_by, i.close_reviewed_at,
		i.agent_order, i.agent_ahead, i.draft, i.outside, i.outside_by, coalesce((SELECT f.name FROM forges f WHERE f.id = i.outside_forge), ''),
		i.hold_at, i.hold_until, i.hold_note, coalesce((SELECT u.username FROM users u WHERE u.id = i.hold_by), ''), i.hold_agent, i.hold_person, i.hold_when, ` + peek + `
	FROM issues i
	JOIN projects p ON p.id = i.project_id
	JOIN statuses st ON st.id = i.status_id
	LEFT JOIN issues par ON par.id = i.parent_id
	LEFT JOIN projects pp ON pp.id = par.project_id
	LEFT JOIN sprints sp ON sp.id = i.sprint_id`
}

func scanIssue(sc interface{ Scan(...any) error }) (*Issue, error) {
	var i Issue
	var parentID, sprintID, reporterID, resolved sql.NullInt64
	var parentKey, parentTitle, parentType, sprintName, startDate, dueDate sql.NullString
	var points, agentOrder sql.NullFloat64
	var created, updated int64
	var workflowID, claimedUntil, claimedAt, workingSince, textBy, reviewedBy, reviewedAt, resolvedBy sql.NullInt64
	var closeReview bool
	var closeReviewedBy, closeReviewedAt sql.NullInt64
	var worked int64
	var peek string
	var holdAt, holdUntil sql.NullInt64
	var hold Hold
	var holdWhen string
	err := sc.Scan(&i.ID, &i.ProjectID, &i.ProjectKey, &i.Number, &i.Type, &i.Title, &i.Description,
		&i.Status.ID, &i.Status.Name, &i.Status.Category, &i.Status.Color, &i.Status.Position, &i.Priority,
		&parentID, &parentKey, &parentTitle, &parentType,
		&sprintID, &sprintName, &reporterID, &points, &startDate, &dueDate, &i.Rank,
		&created, &updated, &resolved, &i.ChildCount, &i.ChildDone,
		&i.Agent, &i.ReporterAgent, &i.Category, &workflowID, &i.Step, &i.ClaimedBy, &claimedUntil, &claimedAt,
		&worked, &workingSince, &textBy, &i.TextAgent, &reviewedBy, &reviewedAt, &resolvedBy, &i.ResolvedAgent, &closeReview, &closeReviewedBy, &closeReviewedAt, &agentOrder, &i.AgentAhead, &i.Draft,
		&i.Outside, &i.OutsideBy, &i.OutsideForge,
		&holdAt, &holdUntil, &hold.Note, &hold.By, &hold.Agent, &hold.byPerson, &holdWhen, &peek)
	if err != nil {
		return nil, err
	}
	if holdAt.Valid {
		hold.At, hold.Until = msTime(holdAt.Int64), ptrTime(holdUntil)
		hold.When = parseHoldWhen(holdWhen)
		hold.Waits = hold.When.text()
		i.Hold = &hold
	}
	i.Excerpt = excerpt(peek, excerptLen)
	i.AgentOrder = ptrFloat(agentOrder)
	i.textBy, i.reviewedBy, i.resolvedBy, i.reviewedAt = ptrInt(textBy), ptrInt(reviewedBy), ptrInt(resolvedBy), ptrTime(reviewedAt)
	i.Unreviewed = i.TextAgent != "" && !reviewedAt.Valid
	i.closeReviewedBy, i.closeReviewedAt = ptrInt(closeReviewedBy), ptrTime(closeReviewedAt)
	i.CloseUnreviewed = closeReview && resolved.Valid && !closeReviewedAt.Valid
	i.WorkflowID, i.ClaimedUntil = ptrInt(workflowID), ptrTime(claimedUntil)
	if i.ClaimedBy != "" {
		i.ClaimedAt = ptrTime(claimedAt)
		i.ClaimWorked, i.ClaimWorkingSince = worked/1000, ptrTime(workingSince)
	}
	i.Key = fmt.Sprintf("%s-%d", i.ProjectKey, i.Number)
	i.ParentID = ptrInt(parentID)
	if parentID.Valid {
		i.Parent = &IssueRef{ID: parentID.Int64, Key: parentKey.String, Title: parentTitle.String, Type: parentType.String}
	}
	i.SprintID = ptrInt(sprintID)
	i.SprintName = sprintName.String
	i.reporterID = ptrInt(reporterID)
	i.Points = ptrFloat(points)
	i.StartDate, i.DueDate = ptrStr(startDate), ptrStr(dueDate)
	i.CreatedAt, i.UpdatedAt, i.ResolvedAt = msTime(created), msTime(updated), ptrTime(resolved)
	i.Assignees, i.Labels = []UserRef{}, []Label{}
	return &i, nil
}

// loadIssues runs an issue query and fills assignees, labels and reporters.
func (s *Service) loadIssues(ctx context.Context, q queryer, query string, args ...any) ([]*Issue, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []*Issue
	now := s.now()
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if !i.Hold.on(now) {
			i.Hold = nil
		}
		out = append(out, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []*Issue{}, nil
	}
	return out, s.hydrate(ctx, q, out)
}

func (s *Service) hydrate(ctx context.Context, q queryer, issues []*Issue) error {
	if len(issues) == 0 {
		return nil
	}
	byID := make(map[int64]*Issue, len(issues))
	ids := make([]int64, 0, len(issues))
	for _, i := range issues {
		byID[i.ID] = i
		ids = append(ids, i.ID)
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		ph, args := placeholders(len(chunk)), int64Args(chunk)
		rows, err := q.QueryContext(ctx, `SELECT a.issue_id, u.id, u.username, u.display_name, u.avatar_version
			FROM issue_assignees a JOIN users u ON u.id = a.user_id WHERE a.issue_id IN (`+ph+`) ORDER BY u.display_name`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var iid, av int64
			var r UserRef
			if err := rows.Scan(&iid, &r.ID, &r.Username, &r.DisplayName, &av); err != nil {
				rows.Close()
				return err
			}
			if r.DisplayName == "" {
				r.DisplayName = r.Username
			}
			r.AvatarURL = avatarURL(r.ID, av)
			byID[iid].Assignees = append(byID[iid].Assignees, r)
		}
		rows.Close()
		rows, err = q.QueryContext(ctx, `SELECT il.issue_id, l.id, l.name, l.color FROM issue_labels il JOIN labels l ON l.id = il.label_id
			WHERE il.issue_id IN (`+ph+`) ORDER BY lower(l.name)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var iid int64
			var l Label
			if err := rows.Scan(&iid, &l.ID, &l.Name, &l.Color); err != nil {
				rows.Close()
				return err
			}
			byID[iid].Labels = append(byID[iid].Labels, l)
		}
		rows.Close()
	}
	users := map[int64]*UserRef{}
	for _, i := range issues {
		if i.reporterID == nil {
			continue
		}
		r, ok := users[*i.reporterID]
		if !ok {
			if u, err := s.userByID(ctx, q, *i.reporterID); err == nil {
				ref := u.Ref()
				r = &ref
			}
			users[*i.reporterID] = r
		}
		i.Reporter = r
	}
	return nil
}

// issueByID loads one issue (no access check).
func (s *Service) issueByID(ctx context.Context, q queryer, id int64, withDesc bool) (*Issue, error) {
	list, err := s.loadIssues(ctx, q, issueSelect(withDesc)+` WHERE i.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("issue")
	}
	return list[0], nil
}

// resolveIssueID turns a key ("WAY-12", "way12"), an old key, or a numeric
// id into an id.
func (s *Service) resolveIssueID(ctx context.Context, q queryer, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if cands := keyCandidates(ref); len(cands) > 0 {
		var ids []int64
		var keys []string
		for _, c := range cands {
			var id int64
			err := q.QueryRowContext(ctx, `SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.key = ? COLLATE NOCASE AND i.number = ?`, c[0], c[1]).Scan(&id)
			if err != nil {
				err = q.QueryRowContext(ctx, `SELECT issue_id FROM issue_aliases WHERE project_key = ? COLLATE NOCASE AND number = ?`, c[0], c[1]).Scan(&id)
			}
			if err == nil && !slices.Contains(ids, id) {
				ids, keys = append(ids, id), append(keys, c[0]+"-"+c[1])
			}
		}
		switch len(ids) {
		case 1:
			return ids[0], nil
		case 0:
			if _, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err != nil {
				return 0, notFound("issue " + strings.ToUpper(ref))
			}
		default:
			return 0, invalid("%s could be %s; add the dash", strings.ToUpper(ref), strings.Join(keys, " or "))
		}
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
		var id int64
		if err := q.QueryRowContext(ctx, `SELECT id FROM issues WHERE id = ?`, n).Scan(&id); err == nil {
			return id, nil
		}
	}
	return 0, notFound("issue " + ref)
}

// shareRole is the best role a user has through shares on the issue or any ancestor.
func (s *Service) shareRole(ctx context.Context, q queryer, userID, issueID int64) (int, error) {
	var role sql.NullInt64
	err := q.QueryRowContext(ctx, `WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM issues WHERE id = ?
			UNION ALL SELECT i.id, i.parent_id, a.depth + 1 FROM issues i JOIN anc a ON i.id = a.parent_id WHERE a.depth < 50)
		SELECT max(CASE role WHEN 'editor' THEN 2 ELSE 1 END) FROM issue_shares WHERE user_id = ? AND issue_id IN (SELECT id FROM anc)`,
		issueID, userID).Scan(&role)
	if err != nil {
		return RoleNone, err
	}
	return int(role.Int64), nil
}

// issueRole is the actor's access to an issue: their project role, or what
// has been shared with them. Automations act as editors of their project.
// Drafts are hidden from agents and automations (WP-42).
func (s *Service) issueRole(ctx context.Context, q queryer, actor *Actor, i *Issue) (int, error) {
	if actor == nil || actor.User == nil {
		return RoleNone, nil
	}
	if actor.HidesDrafts() && s.isDraft(ctx, q, i.ID) {
		return RoleNone, nil
	}
	if actor.Automation != nil && actor.Automation.ProjectID == i.ProjectID {
		return RoleEditor, nil
	}
	r, err := s.projectRole(ctx, q, actor, i.ProjectID)
	if err != nil || r > RoleNone || actor.Scope != nil {
		return r, err
	}
	return s.shareRole(ctx, q, actor.ID(), i.ID)
}

// isDraft reports whether the issue is a draft (WP-42), from the database
// so callers holding only an id can't get it wrong; a failed lookup counts
// as one, failing closed.
func (s *Service) isDraft(ctx context.Context, q queryer, id int64) bool {
	var d bool
	err := q.QueryRowContext(ctx, `SELECT draft FROM issues WHERE id = ?`, id).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read is draft", "id", id, "err", err)
		}
		return true
	}
	return d
}

// canSee reports whether the actor can open the issue.
func (s *Service) canSee(ctx context.Context, q queryer, actor *Actor, id, projectID int64) bool {
	r, err := s.issueRole(ctx, q, actor, &Issue{ID: id, ProjectID: projectID})
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read can see", "projectID", projectID, "err", err)
		}
		return false
	}
	return r > RoleNone
}

// hideParents drops parent references a guest can't open: a share covers
// an issue and what's beneath it, never the issues above.
func (s *Service) hideParents(ctx context.Context, q queryer, actor *Actor, issues []*Issue) {
	if actor.HidesDrafts() {
		for _, i := range issues {
			if i.Parent != nil && s.isDraft(ctx, q, i.Parent.ID) {
				i.Parent, i.ParentID = nil, nil
			}
		}
	}
	if actor.IsAdmin() {
		return
	}
	member := map[int64]bool{}
	for _, i := range issues {
		if i.Parent == nil {
			continue
		}
		m, ok := member[i.ProjectID]
		if !ok {
			r, err := s.projectRole(ctx, q, actor, i.ProjectID)
			m = err == nil && r > RoleNone
			member[i.ProjectID] = m
		}
		if !m && !s.canSee(ctx, q, actor, i.Parent.ID, i.ProjectID) {
			i.Parent = nil
		}
	}
}

// listExtras fills in what list views add to issues: parents a guest can't
// see are hidden, and blockers learn which visible issues they block.
func (s *Service) listExtras(ctx context.Context, actor *Actor, issues []*Issue) error {
	s.hideParents(ctx, s.db.R, actor, issues)
	if len(issues) == 0 {
		return nil
	}
	byID := make(map[int64]*Issue, len(issues))
	ids := make([]int64, 0, len(issues))
	for _, i := range issues {
		byID[i.ID] = i
		ids = append(ids, i.ID)
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		// #nosec G202 -- only ? placeholders are joined in; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT r.source_id, o.id, o.project_id, p.key || '-' || o.number
			FROM issue_relations r JOIN issues o ON o.id = r.target_id JOIN projects p ON p.id = o.project_id
			WHERE r.kind = 'blocks' AND r.source_id IN (`+placeholders(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return err
		}
		type edge struct {
			from, to, project int64
			key               string
		}
		var edges []edge
		for rows.Next() {
			var e edge
			if err := rows.Scan(&e.from, &e.to, &e.project, &e.key); err != nil {
				rows.Close()
				return err
			}
			edges = append(edges, e)
		}
		rows.Close()
		seen := map[int64]bool{}
		for _, e := range edges {
			ok, known := seen[e.to]
			if !known {
				_, inList := byID[e.to]
				ok = inList || s.canSee(ctx, s.db.R, actor, e.to, e.project)
				seen[e.to] = ok
			}
			if ok {
				byID[e.from].Blocks = append(byID[e.from].Blocks, e.key)
			}
		}
	}
	return nil
}

// getIssueFor loads an issue and checks the actor has at least min access.
func (s *Service) getIssueFor(ctx context.Context, q queryer, actor *Actor, ref string, min int, withDesc bool) (*Issue, int, error) {
	if min > RoleViewer {
		if err := actor.CanWrite(); err != nil {
			return nil, 0, err
		}
	}
	id, err := s.resolveIssueID(ctx, q, ref)
	if err != nil {
		return nil, 0, err
	}
	i, err := s.issueByID(ctx, q, id, withDesc)
	if err != nil {
		return nil, 0, err
	}
	r, err := s.issueRole(ctx, q, actor, i)
	if err != nil {
		return nil, 0, err
	}
	if r == RoleNone {
		return nil, 0, notFound("issue " + ref)
	}
	if r < min {
		return nil, 0, ErrForbidden
	}
	return i, r, nil
}

// GetIssue returns an issue with its children, links, watchers and history summary.
func (s *Service) GetIssue(ctx context.Context, actor *Actor, ref string) (*IssueDetail, error) {
	i, role, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, true)
	if err != nil {
		return nil, err
	}
	d := &IssueDetail{Issue: i, Role: roleNames[role]}
	q := s.db.R
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM issues WHERE id = ?
			UNION ALL SELECT i.id, i.parent_id, a.depth + 1 FROM issues i JOIN anc a ON i.id = a.parent_id WHERE a.depth < 50)
		SELECT i.id, i.project_id, p.key || '-' || i.number, i.title, i.type FROM anc a JOIN issues i ON i.id = a.id JOIN projects p ON p.id = i.project_id
		WHERE a.depth > 0 ORDER BY a.depth DESC`, i.ID)
	if err != nil {
		return nil, err
	}
	d.Ancestors = []IssueRef{}
	var ancestors []IssueRef
	var ancestorProject []int64
	for rows.Next() {
		var r IssueRef
		var pid int64
		if err := rows.Scan(&r.ID, &pid, &r.Key, &r.Title, &r.Type); err != nil {
			rows.Close()
			return nil, err
		}
		ancestors = append(ancestors, r)
		ancestorProject = append(ancestorProject, pid)
	}
	rows.Close()
	for n, r := range ancestors {
		if s.canSee(ctx, q, actor, r.ID, ancestorProject[n]) {
			d.Ancestors = append(d.Ancestors, r)
		}
	}
	s.hideParents(ctx, q, actor, []*Issue{i})
	if d.Children, err = s.loadIssues(ctx, q, issueSelect(false)+` WHERE i.parent_id = ? AND `+draftsHidden(actor, "i")+` ORDER BY i.rank`, i.ID); err != nil {
		return nil, err
	}
	if d.Relations, err = s.relations(ctx, q, actor, i.ID); err != nil {
		return nil, err
	}
	if d.Links, err = s.externalLinks(ctx, q, i.ID); err != nil {
		return nil, err
	}
	if d.ForgeMentions, err = s.forgeMentions(ctx, q, i.ID, false); err != nil {
		return nil, err
	}
	if d.Files, err = s.attachments(ctx, q, AttachTarget{TargetIssue, i.ID}); err != nil {
		return nil, err
	}
	if d.AgentRuns, err = s.agentRuns(ctx, q, i.ID); err != nil {
		return nil, err
	}
	if d.Pages, err = s.issuePages(ctx, actor, i.ID); err != nil {
		return nil, err
	}
	if d.FiledFrom, err = s.filedFrom(ctx, q, actor, i); err != nil {
		return nil, err
	}
	if d.Tested, err = s.issueEvidence(ctx, q, i.ID); err != nil {
		return nil, err
	}
	if d.Safety, err = s.IssueSafety(ctx, q, i.ID); err != nil {
		return nil, err
	}
	if d.Objective, err = s.objectiveOf(ctx, q, i.ID); err != nil {
		return nil, err
	}
	if d.Lineage, err = s.lineageOf(ctx, q, actor, i.ID); err != nil {
		return nil, err
	}
	if d.Risk, err = s.issueRiskByID(ctx, q, actor, i.ID); err != nil {
		return nil, err
	}
	d.Goal = s.issueGoal(ctx, q, actor, i.ID, i.ProjectID)
	if err := q.QueryRowContext(ctx, `SELECT intent, intent_agent FROM issues WHERE id = ?`, i.ID).Scan(&d.Intent, &d.IntentAgent); err != nil {
		return nil, err
	}
	s.goalContextRead(ctx, q, actor, d)
	if d.Written, err = issueWritten(ctx, q, i.ID); err != nil {
		return nil, err
	}
	d.Watchers, d.Watching = []UserRef{}, false
	rows, err = q.QueryContext(ctx, `SELECT u.id, u.username, u.display_name, u.avatar_version FROM issue_watchers w JOIN users u ON u.id = w.user_id WHERE w.issue_id = ?`, i.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r UserRef
		var av int64
		if err := rows.Scan(&r.ID, &r.Username, &r.DisplayName, &av); err != nil {
			rows.Close()
			return nil, err
		}
		r.AvatarURL = avatarURL(r.ID, av)
		if r.DisplayName == "" {
			r.DisplayName = r.Username
		}
		d.Watchers = append(d.Watchers, r)
		if r.ID == actor.ID() {
			d.Watching = true
		}
	}
	rows.Close()
	d.Shares = []Share{}
	rows, err = q.QueryContext(ctx, `SELECT u.id, u.username, u.display_name, u.avatar_version, sh.role FROM issue_shares sh JOIN users u ON u.id = sh.user_id WHERE sh.issue_id = ?`, i.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sh Share
		var av int64
		if err := rows.Scan(&sh.User.ID, &sh.User.Username, &sh.User.DisplayName, &av, &sh.Role); err != nil {
			rows.Close()
			return nil, err
		}
		sh.User.AvatarURL = avatarURL(sh.User.ID, av)
		d.Shares = append(d.Shares, sh)
	}
	rows.Close()
	now := s.nowMS()
	if err := q.QueryRowContext(ctx, `SELECT coalesce(sum((coalesce(ended_at, ?) - started_at) / 1000), 0) FROM time_entries WHERE issue_id = ?`, now, i.ID).Scan(&d.TimeSpent); err != nil {
		return nil, err
	}
	d.TextBy, d.ReviewedBy, d.ReviewedAt = s.userRefPtr(ctx, q, i.textBy), s.userRefPtr(ctx, q, i.reviewedBy), i.reviewedAt
	if i.ResolvedAt != nil {
		d.ResolvedBy = s.userRefPtr(ctx, q, i.resolvedBy)
		d.CloseReviewedBy, d.CloseReviewedAt = s.userRefPtr(ctx, q, i.closeReviewedBy), i.closeReviewedAt
	}
	return d, nil
}

// IssueInput creates an issue. References are flexible so people and agents
// can use names: Project is a key, Parent an issue key, Status a status name
// (or category), Sprint a name / "active" / "next", Assignees usernames or "me".
type IssueInput struct {
	Project     string   `json:"project"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    any      `json:"priority"`
	Parent      string   `json:"parent"`
	Sprint      string   `json:"sprint"`
	Assignees   []string `json:"assignees"`
	Labels      []string `json:"labels"`
	Points      *float64 `json:"points"`
	StartDate   string   `json:"start_date"`
	DueDate     string   `json:"due_date"`
	// After/Before place the issue next to another in rank order.
	After  string `json:"after"`
	Before string `json:"before"`
	// Template (a name or id) fills in what's left empty and adds its
	// sub-tasks.
	Template string `json:"template"`
	Agent    string `json:"agent"`    // who may take it (WP-93): "" or "anyone" (the default), "none" (humans only), "any" or an agent spec; agents' own issues default to themselves
	Category string `json:"category"` // feature, bugfix, …
	Workflow string `json:"workflow"` // name or id; the issue starts at its first step
	// Draft keeps it from agents until a person publishes it (WP-42); a
	// sub-issue of a draft is one too.
	Draft bool `json:"draft"`
	// Hold puts it on hold from the start (WP-269).
	Hold *HoldInput `json:"hold"`
	// TitleIsText says the title is text typed where a title goes, so a
	// model may write a short title and keep the text as the description
	// (WP-67). Very long titles get that without saying so.
	TitleIsText bool `json:"title_is_text"`

	madeParent   bool
	outside      bool
	outsideBy    string
	outsideForge *int64
	template     bool
}

func parsePriorityAny(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return ParsePriority(strconv.Itoa(int(x)))
	case int:
		return ParsePriority(strconv.Itoa(x))
	case string:
		if x == "" {
			return 0, nil
		}
		return ParsePriority(x)
	}
	return 0, invalid("invalid priority")
}

// CreateIssue creates an issue.
func (s *Service) CreateIssue(ctx context.Context, actor *Actor, in IssueInput) (*IssueDetail, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if in.Agent != "" {
		if err := needDelegate(actor, &in.Agent); err != nil {
			return nil, err
		}
	}
	var holdUntil *int64
	if in.Hold != nil {
		var err error
		if holdUntil, err = s.holdUntil(actor, in.Hold.Until); err != nil {
			return nil, err
		}
	}
	if in.Template != "" {
		subtasks, err := s.fillFromTemplate(ctx, actor, &in)
		if err != nil {
			return nil, err
		}
		in.Template = ""
		d, err := s.CreateIssue(ctx, actor, in)
		if err != nil || len(subtasks) == 0 {
			return d, err
		}
		for _, title := range subtasks {
			if _, err := s.CreateIssue(ctx, actor, IssueInput{Parent: d.Key, Type: "subtask", Title: title, Draft: in.Draft, madeParent: true}); err != nil {
				return nil, err
			}
		}
		if in.Draft && actor.HidesDrafts() {
			return d, nil
		}
		return s.GetIssue(ctx, actor, d.Key)
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, invalid("title is required")
	}
	if utf8.RuneCountInString(in.Title) > maxTitleLen {
		in.Title, in.Description = splitTitle(in.Title, in.Description, true)
		in.TitleIsText = true
	}
	if len(in.Description) > maxDescriptionLen {
		return nil, invalid("description is too long")
	}
	if in.Type == "" {
		in.Type = "task"
	}
	in.Type = strings.ToLower(in.Type)
	if !slices.Contains(issueTypes, in.Type) {
		return nil, invalid("type must be one of %s", strings.Join(issueTypes, ", "))
	}
	prio, err := parsePriorityAny(in.Priority)
	if err != nil {
		return nil, err
	}
	if in.Category, err = checkIssueCategory(in.Category); err != nil {
		return nil, err
	}
	said := strings.TrimSpace(in.Agent) != ""
	if in.Agent, err = checkAgentSpec(in.Agent); err != nil {
		return nil, err
	}
	if !said && actor.Agent != "" {
		in.Agent = actor.Agent
	}
	delegated := agentDelegated(in.Agent) && said
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		var parent *Issue
		var p *Project
		if in.Parent != "" {
			pid, err := s.resolveIssueID(ctx, t, in.Parent)
			if err != nil {
				return err
			}
			if parent, err = s.issueByID(ctx, t, pid, false); err != nil {
				return err
			}
			if parent.Draft {
				if actor.HidesDrafts() && !in.madeParent {
					return notFound("issue " + in.Parent)
				}
				in.Draft = true
			}
			if in.Project == "" {
				in.Project = parent.ProjectKey
			}
		}
		if in.Project == "" {
			return invalid("project is required")
		}
		p, err := s.projectByRef(ctx, t, in.Project)
		if err != nil {
			return err
		}
		if parent != nil && parent.ProjectID != p.ID {
			return invalid("a parent must be in the same project")
		}
		role, err := s.projectRole(ctx, t, actor, p.ID)
		if err != nil {
			return err
		}
		if role < RoleEditor && actor.Automation == nil {
			if parent == nil {
				if role == RoleNone {
					return notFound("project " + in.Project)
				}
				return ErrForbidden
			}
			if actor.Scope != nil {
				return ErrForbidden
			}
			r, rerr := s.shareRole(ctx, t, actor.ID(), parent.ID)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "share role failed", "parent", parent.ID, "err", rerr)
				}
			}
			if r < RoleEditor {
				return ErrForbidden
			}
		}
		statusID := p.Settings.DefaultStatusID
		if in.Status != "" {
			st, err := s.resolveStatus(ctx, t, p.ID, in.Status)
			if err != nil {
				return err
			}
			statusID = st.ID
		}
		var sprintID *int64
		if in.Sprint != "" {
			sp, err := s.resolveSprint(ctx, t, p.ID, in.Sprint)
			if err != nil {
				return err
			}
			sprintID = &sp.ID
		} else if parent != nil && parent.SprintID != nil {
			var state string
			if err := t.QueryRowContext(ctx, `SELECT state FROM sprints WHERE id = ?`, *parent.SprintID).Scan(&state); err != nil {
				return err
			}
			if state != "closed" {
				sprintID = parent.SprintID
			}
		}
		st, err := s.statusByID(ctx, t, statusID)
		if err != nil {
			return err
		}
		if (sprintID != nil || delegated && in.Status == "") && st.Category == CatBacklog {
			if todo, err := s.firstStatusIn(ctx, t, p.ID, CatTodo); err == nil {
				st = todo
			}
		}
		start, err := s.parseDateFor(actor, in.StartDate)
		if err != nil {
			return err
		}
		due, err := s.parseDateFor(actor, in.DueDate)
		if err != nil {
			return err
		}
		if in.Points != nil && (*in.Points < 0 || *in.Points > 1000) {
			return invalid("points must be between 0 and 1000")
		}
		var number int64
		if err := t.QueryRowContext(ctx, `UPDATE projects SET next_number = next_number + 1 WHERE id = ? RETURNING next_number - 1`, p.ID).Scan(&number); err != nil {
			return err
		}
		var rank float64
		if in.After == "" && in.Before == "" && parent == nil && createdByPerson(actor) {
			rank, err = firstRank(ctx, t, p.ID)
		} else {
			rank, err = s.rankFor(ctx, t, p.ID, in.After, in.Before)
		}
		if err != nil {
			return err
		}
		now := s.nowMS()
		var resolved any
		if isClosed(st.Category) {
			resolved = now
		}
		var reporter, parentID, resolvedBy any
		if actor.ID() != 0 {
			reporter = actor.ID()
		}
		resolvedAgent := ""
		if resolved != nil {
			resolvedBy, resolvedAgent = reporter, actor.AgentName()
		}
		if parent != nil {
			parentID = parent.ID
		}
		var workflowID *int64
		step := ""
		if in.Workflow != "" {
			w, err := s.workflowFor(ctx, t, p.ID, in.Workflow)
			if err != nil {
				return err
			}
			workflowID, step = &w.ID, w.Steps[0].Name
		}
		author := authorAgent(ctx, t, actor)
		_, origin, err := s.originSets(ctx, t, actor, 0, author)
		if err != nil {
			return err
		}
		sessID, sessTask := sessionStamp(ctx, t, actor)
		res, err := t.ExecContext(ctx, `INSERT INTO issues(project_id, number, type, title, description, status_id, priority, parent_id, sprint_id,
			reporter_id, points, start_date, due_date, rank, created_at, updated_at, resolved_at,
			agent, reporter_agent, category, workflow_id, step, text_by, text_agent, resolved_by, resolved_agent, close_review, draft,
			outside, outside_by, outside_forge, origin_issue_id, origin_depth, session_id, session_issue_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, number, in.Type, in.Title, in.Description, st.ID, prio, parentID, nullInt(sprintID),
			reporter, nullFloat(in.Points), nullStr(start), nullStr(due), rank, now, now, resolved,
			in.Agent, author, in.Category, nullInt(workflowID), step,
			reporter, author, resolvedBy, resolvedAgent, resolvedAgent != "", in.Draft || in.outside,
			in.outside, in.outsideBy, nullInt(in.outsideForge), origin[0], origin[1], sessID, sessTask)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if in.template {
			if err := stampTemplate(ctx, t, id, in.Title, in.Description, now); err != nil {
				return err
			}
		}
		if parentID != nil && actor.ActsAsPerson() {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET parent_by = ?, parent_at = ? WHERE id = ?`, actor.ID(), now, id); err != nil {
				return err
			}
		}
		key := fmt.Sprintf("%s-%d", p.Key, number)
		if err := recordWritten(ctx, t, id, p.ID); err != nil {
			return err
		}
		if err := s.recordCrossFiled(ctx, t, actor, id, p.ID, author); err != nil {
			return err
		}
		s.recordMentions(ctx, t, mentionSource{typ: KIssue, id: id, project: &p.ID, text: in.Title + "\n" + in.Description,
			user: actor.ID(), agent: actor.AgentName()})
		ev := &Event{Kind: EvIssueCreated, ProjectID: p.ID, IssueID: id, IssueKey: key, New: in.Title,
			Data: map[string]any{"status_id": st.ID, "category": st.Category, "type": in.Type}}
		if in.TitleIsText || longForTitle(in.Title) {
			kernel.Attach(ev, retitleHint{})
		}
		t.emit(ev)
		if delegated && !actor.IsAgent() {
			auditDelegation(t, p.ID, id, key, "", in.Agent)
		}
		if actor.ID() != 0 {
			if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_watchers(issue_id, user_id) VALUES (?,?)`, id, actor.ID()); err != nil {
				return err
			}
		}
		for _, a := range in.Assignees {
			if err := s.addAssignee(ctx, t, actor, id, p.ID, key, a); err != nil {
				return err
			}
		}
		for _, l := range in.Labels {
			if err := s.addLabel(ctx, t, id, p.ID, l); err != nil {
				return err
			}
		}
		s.noteIssueMentions(ctx, t, actor, id)
		s.noteNamedRelations(ctx, t, actor, id, "")
		if o, ok := origin[0].(int64); ok && o != id && o != parentID {
			if _, err := s.insertRelation(ctx, t, newRelation{Src: id, Dst: o, Kind: "follows", Found: "session", Basis: "inferred"}); err != nil {
				return err
			}
		}
		if in.Hold != nil {
			if err := s.placeHold(ctx, t, actor, &Issue{ID: id, Key: key, ProjectID: p.ID}, holdUntil, nil, in.Hold.Note); err != nil {
				return err
			}
		}
		return s.reindex(ctx, t, id)
	})
	if err != nil {
		return nil, err
	}
	if in.Draft && actor.HidesDrafts() {
		i, err := s.issueByID(ctx, s.db.R, id, false)
		if err != nil {
			return nil, err
		}
		return &IssueDetail{Issue: i}, nil
	}
	return s.GetIssue(ctx, actor, itoa64(id))
}

func (s *Service) statusByID(ctx context.Context, q queryer, id int64) (*Status, error) {
	var st Status
	err := q.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE id = ?`, id).Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("status")
	} else if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	return &st, nil
}

func (s *Service) firstStatusIn(ctx context.Context, q queryer, projectID int64, category string) (*Status, error) {
	var st Status
	err := q.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE project_id = ? AND category = ? ORDER BY position LIMIT 1`,
		projectID, category).Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position)
	if err != nil {
		return nil, notFound("status in category " + category)
	}
	return &st, nil
}

// resolveStatus finds a status by id, name, or category (first status in it).
// Common words map to categories so "done" and "closed" work everywhere.
func (s *Service) resolveStatus(ctx context.Context, q queryer, projectID int64, ref string) (*Status, error) {
	ref = strings.TrimSpace(ref)
	var st Status
	err := q.QueryRowContext(ctx, `SELECT id, name, category, color, position FROM statuses WHERE project_id = ? AND (name = ? OR CAST(id AS TEXT) = ?)`,
		projectID, ref, ref).Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.Position)
	if err == nil {
		return &st, nil
	}
	cat := strings.ToLower(strings.ReplaceAll(ref, " ", "_"))
	switch cat {
	case "closed", "complete", "completed", "resolved", "finished":
		cat = CatDone
	case "open", "todo", "to_do", "reopen", "reopened":
		cat = CatTodo
	case "doing", "started", "progress", "wip", "active":
		cat = CatInProgress
	case "cancelled", "wontdo", "won't_do", "wont_do", "rejected":
		cat = CatCanceled
	}
	if validCategory(cat) {
		return s.firstStatusIn(ctx, q, projectID, cat)
	}
	return nil, invalid("no status %q in this project", ref)
}

// rankFor computes a rank placing an issue after/before another, or last.
// When the neighbour has no room beside it (repeated midpoint inserts at
// the same place made ranks equal or nearly so: WP-ADR35), the project's
// ranks are spread out first, keeping their order, so the issue lands
// exactly where it was put.
func (s *Service) rankFor(ctx context.Context, t *tx, projectID int64, after, before string) (float64, error) {
	ref := after
	if ref == "" {
		ref = before
	}
	if ref == "" {
		var last sql.NullFloat64
		if err := t.QueryRowContext(ctx, `SELECT max(rank) FROM issues WHERE project_id = ?`, projectID).Scan(&last); err != nil {
			return 0, err
		}
		return last.Float64 + 1024, nil
	}
	id, err := s.resolveIssueID(ctx, t, ref)
	if err != nil {
		return 0, err
	}
	var r float64
	var pid int64
	if err := t.QueryRowContext(ctx, `SELECT rank, project_id FROM issues WHERE id = ?`, id).Scan(&r, &pid); err != nil {
		return 0, err
	}
	if pid != projectID {
		return 0, notFound("issue " + ref)
	}
	var crowded bool
	if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issues WHERE project_id = ? AND id != ? AND abs(rank - ?) < ?)`,
		projectID, id, r, rankRoom).Scan(&crowded); err != nil {
		return 0, err
	}
	if crowded {
		if err := s.renumberRanks(ctx, t, projectID); err != nil {
			return 0, err
		}
		if err := t.QueryRowContext(ctx, `SELECT rank FROM issues WHERE id = ?`, id).Scan(&r); err != nil {
			return 0, err
		}
	}
	var other sql.NullFloat64
	if after != "" {
		if err := t.QueryRowContext(ctx, `SELECT min(rank) FROM issues WHERE project_id = ? AND rank > ?`, projectID, r).Scan(&other); err != nil {
			return 0, err
		}
		if !other.Valid {
			return r + 1024, nil
		}
		return (r + other.Float64) / 2, nil
	}
	if err := t.QueryRowContext(ctx, `SELECT max(rank) FROM issues WHERE project_id = ? AND rank < ?`, projectID, r).Scan(&other); err != nil {
		return 0, err
	}
	if !other.Valid {
		return r - 1024, nil
	}
	return (r + other.Float64) / 2, nil
}

// createdByPerson: a person adding an issue themselves, in the web or
// their own terminal, not an agent (or an agent's shell or session), an
// automation or a forge sync.
func createdByPerson(a *Actor) bool {
	return !a.HidesDrafts() && !a.ForgeSync && a.TextAgent == ""
}

// rankRoom is how close two ranks may get before they are spread out again.
const rankRoom = 1e-6

// firstRank is a rank before every issue of the project: the top of each
// column on the board.
func firstRank(ctx context.Context, t *tx, projectID int64) (float64, error) {
	var first sql.NullFloat64
	if err := t.QueryRowContext(ctx, `SELECT min(rank) FROM issues WHERE project_id = ?`, projectID).Scan(&first); err != nil {
		return 0, err
	}
	return first.Float64 - 1024, nil
}

// renumberRanksIfCrowded spreads ranks out again when repeated midpoint
// inserts have made neighbours too close to split.
func (s *Service) renumberRanksIfCrowded(ctx context.Context, t *tx, projectID int64) error {
	var minGap sql.NullFloat64
	if err := t.QueryRowContext(ctx, `SELECT min(gap) FROM (SELECT rank - lag(rank) OVER (ORDER BY rank) AS gap FROM issues WHERE project_id = ?)`, projectID).Scan(&minGap); err != nil {
		return err
	}
	if !minGap.Valid || minGap.Float64 > rankRoom {
		return nil
	}
	return s.renumberRanks(ctx, t, projectID)
}

// renumberRanks spreads a project's ranks 1024 apart in their order.
func (s *Service) renumberRanks(ctx context.Context, t *tx, projectID int64) error {
	_, err := t.ExecContext(ctx, `UPDATE issues SET rank = (SELECT n * 1024.0 FROM (SELECT id, row_number() OVER (ORDER BY rank, id) AS n FROM issues WHERE project_id = ?) r WHERE r.id = issues.id) WHERE project_id = ?`, projectID, projectID)
	return err
}

func (s *Service) addAssignee(ctx context.Context, t *tx, actor *Actor, issueID, projectID int64, key, ref string) error {
	u, err := s.resolveUser(ctx, t, actor, ref)
	if err != nil {
		return err
	}
	if err := s.grantForAssignment(ctx, t, actor, issueID, projectID, u); err != nil {
		return err
	}
	res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_assignees(issue_id, user_id) VALUES (?,?)`, issueID, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_watchers(issue_id, user_id) VALUES (?,?)`, issueID, u.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueAssigned, ProjectID: projectID, IssueID: issueID, IssueKey: key, Field: "assignee", New: u.Username,
			Data: map[string]any{"user_id": u.ID}})
	}
	return nil
}

// grantForAssignment makes sure an assignee can see the issue. Assigning
// someone outside the project shares the issue with them as an editor,
// which only project editors (who could share it anyway) may do.
func (s *Service) grantForAssignment(ctx context.Context, t *tx, actor *Actor, issueID, projectID int64, u *User) error {
	if u.Disabled {
		return invalid("%s is disabled", u.Username)
	}
	r, err := s.issueRole(ctx, t, &Actor{User: u}, &Issue{ID: issueID, ProjectID: projectID})
	if err != nil || r > RoleNone {
		return err
	}
	if actor.Automation == nil {
		if err := s.requireProjectRole(ctx, t, actor, projectID, RoleEditor); err != nil {
			return invalid("%s can't see this issue; ask a project editor to share it with them", u.Username)
		}
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO issue_shares(issue_id, user_id, role, created_by, created_at) VALUES (?,?,'editor',?,?)
		ON CONFLICT(issue_id, user_id) DO NOTHING`, issueID, u.ID, nullUser(actor.ID()), s.nowMS()); err != nil {
		return err
	}
	return nil
}

func (s *Service) removeAssignee(ctx context.Context, t *tx, actor *Actor, issueID, projectID int64, key, ref string) error {
	u, err := s.resolveUser(ctx, t, actor, ref)
	if err != nil {
		return err
	}
	res, err := t.ExecContext(ctx, `DELETE FROM issue_assignees WHERE issue_id = ? AND user_id = ?`, issueID, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		t.emit(&Event{Kind: EvIssueUnassigned, ProjectID: projectID, IssueID: issueID, IssueKey: key, Field: "assignee", Old: u.Username,
			Data: map[string]any{"user_id": u.ID}})
	}
	return nil
}

// addLabel attaches a label by name, creating it in the project if needed.
func (s *Service) addLabel(ctx context.Context, t *tx, issueID, projectID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	var lid int64
	err := t.QueryRowContext(ctx, `SELECT id FROM labels WHERE project_id = ? AND (name = ? COLLATE NOCASE OR CAST(id AS TEXT) = ?)
		ORDER BY name = ? DESC LIMIT 1`, projectID, name, name, name).Scan(&lid)
	if errors.Is(err, sql.ErrNoRows) {
		if t.Actor.Automation == nil && s.requireProjectRole(ctx, t, t.Actor, projectID, RoleEditor) != nil {
			return invalid("label %q doesn't exist in this project", name)
		}
		if utf8.RuneCountInString(name) > 50 {
			return invalid("label names can be at most 50 characters")
		}
		res, err := t.ExecContext(ctx, `INSERT INTO labels(project_id, name, color) VALUES (?,?,?)`, projectID, name, labelColor(name))
		if err != nil {
			return err
		}
		lid, _ = res.LastInsertId()
	} else if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_labels(issue_id, label_id) VALUES (?,?)`, issueID, lid)
	return err
}

var labelPalette = []string{"#ef4444", "#f97316", "#eab308", "#22c55e", "#14b8a6", "#0ea5e9", "#6366f1", "#a855f7", "#ec4899", "#64748b"}

func labelColor(name string) string {
	h := 0
	for _, r := range strings.ToLower(name) {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return labelPalette[h%len(labelPalette)]
}

// reindex refreshes the search index row for an issue.
func (s *Service) reindex(ctx context.Context, t queryer, issueID int64) error {
	if _, err := t.ExecContext(ctx, `DELETE FROM issue_search WHERE rowid = ?`, issueID); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx, `INSERT INTO issue_search(rowid, key, title, body)
		SELECT i.id, p.key || '-' || i.number, i.title,
			i.description || ' ' || coalesce((SELECT group_concat(c.body, ' ') FROM comments c WHERE c.issue_id = i.id), '')
		FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issueID)
	return err
}

// IssuePatch changes an issue. Absent fields are left alone; Opt fields can
// be set to null to clear them.
type IssuePatch struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	// AppendDescription adds Markdown to the end of the description, in the
	// same transaction as reading it, so concurrent edits aren't lost.
	AppendDescription *string `json:"append_description"`
	// Intent: what it's for in a sentence or two (WP-1196); "" clears. A
	// person sending the text an agent wrote keeps it as theirs.
	Intent    *string      `json:"intent"`
	Type      *string      `json:"type"`
	Status    *string      `json:"status"`
	Priority  any          `json:"priority"`
	Parent    Opt[string]  `json:"parent"`
	Sprint    Opt[string]  `json:"sprint"`
	Points    Opt[float64] `json:"points"`
	StartDate Opt[string]  `json:"start_date"`
	DueDate   Opt[string]  `json:"due_date"`
	Assignees *[]string    `json:"assignees"`
	AddAssign []string     `json:"add_assignees"`
	RemAssign []string     `json:"remove_assignees"`
	Labels    *[]string    `json:"labels"`
	AddLabels []string     `json:"add_labels"`
	RemLabels []string     `json:"remove_labels"`
	After     string       `json:"after"`
	Before    string       `json:"before"`
	Agent     *string      `json:"agent"`    // who may take it: "" or "anyone", "none" (humans only), "any" or an agent spec
	Category  *string      `json:"category"` // feature, bugfix, …; "" clears
	Workflow  Opt[string]  `json:"workflow"` // name or id; null leaves the workflow
	// ViaLink is the URL of a pull request link on this issue that causes
	// the status change: the link remembers the status it moved the issue
	// from, so removing the link can undo it (WP-162).
	ViaLink    string `json:"via_link"`
	mergedLink string
	Step       *string `json:"step"` // a step of the issue's workflow; "" clears
	// Comment, when set, is added in the same transaction as the change
	// (UpdateIssue only; BulkUpdate rejects it).
	Comment string `json:"comment"`
}

// Text limits, in bytes. Generous for prose; they exist to bound the FTS
// index and the payloads every viewer downloads.
const (
	maxTitleLen       = 500
	maxDescriptionLen = 100_000
	maxIntentLen      = 1_000
	maxCommentLen     = 50_000
)

// UpdateIssue applies a patch to one issue.
func (s *Service) UpdateIssue(ctx context.Context, actor *Actor, ref string, p IssuePatch) (*IssueDetail, error) {
	if p.Comment != "" {
		if err := actor.Need(PermIssuesComment); err != nil {
			return nil, err
		}
	}
	if err := needDelegate(actor, p.Agent); err != nil {
		return nil, err
	}
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, true)
		if err != nil {
			return err
		}
		id = i.ID
		if err := s.applyPatch(ctx, t, actor, i, p); err != nil {
			return err
		}
		if c := strings.TrimSpace(p.Comment); c != "" {
			_, err := s.addComment(ctx, t, actor, i, c)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, itoa64(id))
}

// BulkUpdate applies the same patch to many issues. With descendants, the
// patch also applies to everything beneath each issue — for moving an epic
// and its work into a sprint at once.
func (s *Service) BulkUpdate(ctx context.Context, actor *Actor, refs []string, p IssuePatch, descendants bool) ([]string, error) {
	if err := needDelegate(actor, p.Agent); err != nil {
		return nil, err
	}
	if len(refs) > 1000 {
		return nil, invalid("too many issues in one request")
	}
	if p.Comment != "" {
		return nil, invalid("comment is not supported in bulk updates")
	}
	var keys []string
	err := s.write(ctx, actor, func(t *tx) error {
		seen := map[int64]bool{}
		roots := map[int64]bool{}
		var ids []int64
		for _, r := range refs {
			id, err := s.resolveIssueID(ctx, t, r)
			if err != nil {
				return err
			}
			roots[id] = true
			if descendants {
				rows, err := t.QueryContext(ctx, `WITH RECURSIVE d(id, depth) AS (SELECT id, 0 FROM issues WHERE parent_id = ?
					UNION ALL SELECT i.id, d.depth + 1 FROM issues i JOIN d ON i.parent_id = d.id WHERE d.depth < 50) SELECT id FROM d ORDER BY depth DESC, id`, id)
				if err != nil {
					return err
				}
				for rows.Next() {
					var c int64
					if err := rows.Scan(&c); err != nil {
						rows.Close()
						return err
					}
					if !seen[c] {
						seen[c] = true
						ids = append(ids, c)
					}
				}
				rows.Close()
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		parentOf := map[int64]int64{}
		for _, id := range ids {
			var par sql.NullInt64
			if err := t.QueryRowContext(ctx, `SELECT parent_id FROM issues WHERE id = ?`, id).Scan(&par); err != nil {
				return err
			}
			if par.Valid && seen[par.Int64] {
				parentOf[id] = par.Int64
			}
		}
		depth := func(id int64) int {
			d := 0
			for p, ok := parentOf[id]; ok && d < 60; p, ok = parentOf[p] {
				d++
			}
			return d
		}
		slices.SortStableFunc(ids, func(a, b int64) int { return depth(b) - depth(a) })
		for _, id := range ids {
			i, _, err := s.getIssueFor(ctx, t, actor, itoa64(id), RoleEditor, true)
			if err != nil {
				return err
			}
			pp := p
			if descendants && !roots[id] {
				pp.Parent = Opt[string]{}
			}
			if err := s.applyPatch(ctx, t, actor, i, pp); err != nil {
				return fmt.Errorf("%s: %w", i.Key, err)
			}
			keys = append(keys, i.Key)
		}
		return nil
	})
	return keys, err
}

func (s *Service) applyPatch(ctx context.Context, t *tx, actor *Actor, i *Issue, p IssuePatch) error {
	sets, args := []string{}, []any{}
	wrote := false
	ev := func(field, old, new string) {
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: field, Old: old, New: new,
			Coalesce: field == "description" || field == "intent"})
	}
	if p.Title != nil {
		title := strings.TrimSpace(*p.Title)
		if title == "" {
			return invalid("title is required")
		}
		if utf8.RuneCountInString(title) > maxTitleLen {
			desc := i.Description
			if p.Description != nil {
				desc = *p.Description
			}
			var d string
			title, d = splitTitle(title, desc, true)
			p.Description = &d
		}
		if title != i.Title {
			sets, args = append(sets, "title = ?"), append(args, title)
			ev("title", i.Title, title)
			wrote = true
		}
	}
	if p.AppendDescription != nil && strings.TrimSpace(*p.AppendDescription) != "" {
		base := i.Description
		if p.Description != nil {
			base = *p.Description
		}
		joined := appendMarkdown(base, *p.AppendDescription)
		p.Description = &joined
	}
	if p.Description != nil && len(*p.Description) > maxDescriptionLen {
		return invalid("description is too long")
	}
	if p.Description != nil && *p.Description != i.Description {
		sets, args = append(sets, "description = ?"), append(args, *p.Description)
		ev("description", "", "")
		wrote = true
	}
	if p.Intent != nil {
		intent := strings.TrimSpace(*p.Intent)
		if utf8.RuneCountInString(intent) > maxIntentLen {
			return invalid("intent is too long: a sentence or two (%d characters at most)", maxIntentLen)
		}
		var old, oldAgent string
		if err := t.QueryRowContext(ctx, `SELECT intent, intent_agent FROM issues WHERE id = ?`, i.ID).Scan(&old, &oldAgent); err != nil {
			return err
		}
		by := ""
		if actor.IsAgent() {
			by = cmp(authorAgent(ctx, t, actor), "agent")
		}
		if by != "" && old != "" && oldAgent == "" && intent != old {
			return invalid("the intent is a person's words; an agent can't change or clear it. Say what you'd change in a comment (wp comment %s \"…\")", i.Key)
		}
		if intent != old || (by == "" && oldAgent != "" && intent != "") {
			if intent == "" {
				by = ""
			}
			sets, args = append(sets, "intent = ?", "intent_agent = ?"), append(args, intent, by)
			ev("intent", old, intent)
		}
	}
	if wrote {
		if author := authorAgent(ctx, t, actor); author == "" && actor.Automation != nil {
			sets = append(sets, "reviewed_by = NULL", "reviewed_at = NULL")
		} else {
			ws, wa := textWritten(actor, author)
			sets, args = append(sets, ws...), append(args, wa...)
			originCols, originArgs, err := s.originSets(ctx, t, actor, i.ID, author)
			if err != nil {
				return err
			}
			sets, args = append(sets, originCols...), append(args, originArgs...)
		}
		title, desc, project := i.Title, i.Description, i.ProjectID
		if p.Title != nil {
			title = strings.TrimSpace(*p.Title)
		}
		if p.Description != nil {
			desc = *p.Description
		}
		s.recordMentions(ctx, t, mentionSource{typ: KIssue, id: i.ID, project: &project, text: title + "\n" + desc,
			user: actor.ID(), agent: actor.AgentName()})
	}
	if p.Type != nil {
		typ := strings.ToLower(*p.Type)
		if !slices.Contains(issueTypes, typ) {
			return invalid("type must be one of %s", strings.Join(issueTypes, ", "))
		}
		if typ != i.Type {
			sets, args = append(sets, "type = ?"), append(args, typ)
			ev("type", i.Type, typ)
		}
	}
	if p.Priority != nil {
		prio, err := parsePriorityAny(p.Priority)
		if err != nil {
			return err
		}
		if prio != i.Priority {
			sets, args = append(sets, "priority = ?"), append(args, prio)
			ev("priority", PriorityName(i.Priority), PriorityName(prio))
		}
	}
	if p.Points.Set {
		if p.Points.Value != nil && (*p.Points.Value < 0 || *p.Points.Value > 1000) {
			return invalid("points must be between 0 and 1000")
		}
		if !floatPtrEq(p.Points.Value, i.Points) {
			sets, args = append(sets, "points = ?"), append(args, nullFloat(p.Points.Value))
			ev("points", fmtFloatPtr(i.Points), fmtFloatPtr(p.Points.Value))
		}
	}
	if p.Agent != nil {
		spec, err := checkAgentSpec(*p.Agent)
		if err != nil {
			return err
		}
		if spec != i.Agent {
			sets, args = append(sets, "agent = ?"), append(args, spec)
			if agentHumansOnly(spec) {
				sets = append(sets, "agent_order = NULL")
			}
			ev("agent", i.Agent, spec)
			if !actor.IsAgent() {
				auditDelegation(t, i.ProjectID, i.ID, i.Key, i.Agent, spec)
			}
		}
	}
	if p.Category != nil {
		c, err := checkIssueCategory(*p.Category)
		if err != nil {
			return err
		}
		if c != i.Category {
			sets, args = append(sets, "category = ?"), append(args, c)
			ev("category", i.Category, c)
		}
	}
	step, wfID := i.Step, i.WorkflowID
	if p.Workflow.Set {
		var wf *Workflow
		if p.Workflow.Value != nil && strings.TrimSpace(*p.Workflow.Value) != "" {
			w, err := s.workflowFor(ctx, t, i.ProjectID, strings.TrimSpace(*p.Workflow.Value))
			if err != nil {
				return err
			}
			wf = w
		}
		switch {
		case wf == nil && i.WorkflowID != nil:
			sets = append(sets, "workflow_id = NULL")
			ev("workflow", "", "")
			step, wfID = "", nil
		case wf != nil && (i.WorkflowID == nil || *i.WorkflowID != wf.ID):
			sets, args = append(sets, "workflow_id = ?"), append(args, wf.ID)
			ev("workflow", "", wf.Name)
			step, wfID = wf.Steps[0].Name, &wf.ID
		}
	}
	if p.Step != nil {
		want := strings.TrimSpace(*p.Step)
		if want != "" {
			if wfID == nil {
				return invalid("put the issue on a workflow before choosing a step")
			}
			w, err := s.workflowByID(ctx, t, *wfID)
			if err != nil {
				return err
			}
			idx := w.stepIndex(want)
			if idx < 0 {
				return invalid("the %s workflow has no step %q", w.Name, want)
			}
			want = w.Steps[idx].Name
		}
		step = want
	}
	if step != i.Step {
		sets, args = append(sets, "step = ?"), append(args, step)
		t.emit(&Event{Kind: EvIssueStep, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "step", Old: i.Step, New: step})
	}
	for _, d := range []struct {
		field string
		opt   Opt[string]
		cur   *string
	}{{"start_date", p.StartDate, i.StartDate}, {"due_date", p.DueDate, i.DueDate}} {
		if !d.opt.Set {
			continue
		}
		var v *string
		if d.opt.Value != nil && *d.opt.Value != "" {
			parsed, err := s.parseDateFor(actor, *d.opt.Value)
			if err != nil {
				return err
			}
			v = parsed
		}
		if deref(v) != deref(d.cur) {
			sets, args = append(sets, d.field+" = ?"), append(args, nullStr(v))
			ev(d.field, deref(d.cur), deref(v))
		}
	}
	if p.Parent.Set {
		var newParent *int64
		newKey := ""
		if p.Parent.Value != nil && *p.Parent.Value != "" {
			pid, err := s.resolveIssueID(ctx, t, *p.Parent.Value)
			if err != nil {
				return err
			}
			par, err := s.issueByID(ctx, t, pid, false)
			if err != nil {
				return err
			}
			if par.ProjectID != i.ProjectID {
				return invalid("a parent must be in the same project; move the issue first")
			}
			r, rerr := s.issueRole(ctx, t, actor, par)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "issue role failed", "issue", i.ID, "err", rerr)
				}
			}
			if r < RoleViewer {
				return notFound("issue " + *p.Parent.Value)
			}
			var cyc int
			if err := t.QueryRowContext(ctx, `WITH RECURSIVE anc(id, parent_id, depth) AS (SELECT id, parent_id, 0 FROM issues WHERE id = ?
				UNION ALL SELECT x.id, x.parent_id, a.depth + 1 FROM issues x JOIN anc a ON x.id = a.parent_id WHERE a.depth < 50)
				SELECT count(*) FROM anc WHERE id = ?`, pid, i.ID).Scan(&cyc); err != nil {
				return err
			}
			if cyc > 0 {
				return invalid("%s cannot be placed under itself or one of its own subtasks", i.Key)
			}
			newParent, newKey = &pid, par.Key
		}
		oldKey := ""
		if i.Parent != nil {
			oldKey = i.Parent.Key
		}
		if !int64PtrEq(newParent, i.ParentID) {
			sets, args = append(sets, "parent_id = ?"), append(args, nullInt(newParent))
			var by any
			if newParent != nil && actor.ActsAsPerson() {
				by = actor.ID()
			}
			sets, args = append(sets, "parent_by = ?", "parent_at = max(?, coalesce(parent_at, 0) + 1)"), append(args, by, s.nowMS())
			ev("parent", oldKey, newKey)
			defer func() {
				if i.ParentID != nil {
					s.checkChildrenDone(ctx, t, *i.ParentID)
				}
			}()
		}
	}
	var newStatus *Status
	shipped := false
	if p.Status != nil {
		st, err := s.resolveStatus(ctx, t, i.ProjectID, *p.Status)
		if err != nil {
			return err
		}
		if st.ID != i.Status.ID {
			newStatus = st
		}
		shipped = p.ViaLink != "" && actor != nil && actor.Agent != "" && (i.ClaimedBy == actor.Agent || i.ClaimedBy == "") && (i.WorkflowID == nil || i.Step == "")
	}
	if p.Sprint.Set {
		var newSprint *int64
		name := ""
		if p.Sprint.Value != nil && *p.Sprint.Value != "" && !strings.EqualFold(*p.Sprint.Value, "none") && !strings.EqualFold(*p.Sprint.Value, "backlog") {
			sp, err := s.resolveSprint(ctx, t, i.ProjectID, *p.Sprint.Value)
			if err != nil {
				return err
			}
			if sp.State == "closed" {
				return invalid("sprint %q is closed", sp.Name)
			}
			newSprint, name = &sp.ID, sp.Name
			cat := i.Status.Category
			if newStatus != nil {
				cat = newStatus.Category
			}
			if cat == CatBacklog {
				if todo, err := s.firstStatusIn(ctx, t, i.ProjectID, CatTodo); err == nil {
					newStatus = todo
				}
			}
		}
		if !int64PtrEq(newSprint, i.SprintID) {
			sets, args = append(sets, "sprint_id = ?"), append(args, nullInt(newSprint))
			ev("sprint", i.SprintName, name)
		}
	}
	if newStatus != nil && isClosed(newStatus.Category) && !i.Closed() {
		var err error
		if newStatus, err = s.objectiveClose(ctx, t, actor, i, newStatus, &p); err != nil {
			return err
		}
	}
	if newStatus != nil {
		if err := s.setStatus(ctx, t, i, newStatus, &sets, &args); err != nil {
			return err
		}
		if p.ViaLink != "" {
			if _, err := t.ExecContext(ctx, `UPDATE external_links SET moved_from = ?, moved_to = ? WHERE issue_id = ? AND url = ? AND kind = 'pull'`,
				i.Status.ID, newStatus.ID, i.ID, p.ViaLink); err != nil {
				return err
			}
		}
	}
	if shipped {
		if err := s.endRun(ctx, t, i.ID, actor.Agent, "done", nil); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
			return err
		}
	}
	if p.After != "" || p.Before != "" {
		rank, err := s.rankFor(ctx, t, i.ProjectID, p.After, p.Before)
		if err != nil {
			return err
		}
		sets, args = append(sets, "rank = ?"), append(args, rank)
	}
	if p.Assignees != nil {
		want := map[int64]bool{}
		for _, a := range *p.Assignees {
			u, err := s.resolveUser(ctx, t, actor, a)
			if err != nil {
				return err
			}
			want[u.ID] = true
			if err := s.addAssignee(ctx, t, actor, i.ID, i.ProjectID, i.Key, itoa64(u.ID)); err != nil {
				return err
			}
		}
		for _, cur := range i.Assignees {
			if !want[cur.ID] {
				if err := s.removeAssignee(ctx, t, actor, i.ID, i.ProjectID, i.Key, itoa64(cur.ID)); err != nil {
					return err
				}
			}
		}
	}
	for _, a := range p.AddAssign {
		if err := s.addAssignee(ctx, t, actor, i.ID, i.ProjectID, i.Key, a); err != nil {
			return err
		}
	}
	for _, a := range p.RemAssign {
		if err := s.removeAssignee(ctx, t, actor, i.ID, i.ProjectID, i.Key, a); err != nil {
			return err
		}
	}
	labelsChanged := false
	if p.Labels != nil {
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id = ?`, i.ID); err != nil {
			return err
		}
		for _, l := range *p.Labels {
			if err := s.addLabel(ctx, t, i.ID, i.ProjectID, l); err != nil {
				return err
			}
		}
		labelsChanged = true
	}
	for _, l := range p.AddLabels {
		if err := s.addLabel(ctx, t, i.ID, i.ProjectID, l); err != nil {
			return err
		}
		labelsChanged = true
	}
	for _, l := range p.RemLabels {
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id = ? AND label_id IN (SELECT id FROM labels WHERE project_id = ? AND name = ?)`, i.ID, i.ProjectID, l); err != nil {
			return err
		}
		labelsChanged = true
	}
	if labelsChanged {
		old := make([]string, len(i.Labels))
		for n, l := range i.Labels {
			old[n] = l.Name
		}
		var cur []string
		rows, err := t.QueryContext(ctx, `SELECT l.name FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = ? ORDER BY lower(l.name)`, i.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			cur = append(cur, n)
		}
		rows.Close()
		if strings.Join(old, ",") != strings.Join(cur, ",") {
			ev("labels", strings.Join(old, ", "), strings.Join(cur, ", "))
		}
	}
	sets, args = append(sets, "updated_at = ?"), append(args, s.nowMS(), i.ID)
	if _, err := t.ExecContext(ctx, `UPDATE issues SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return err
	}
	t.runPending()
	if p.After != "" || p.Before != "" {
		if err := s.renumberRanksIfCrowded(ctx, t, i.ProjectID); err != nil {
			return err
		}
	}
	if p.Title != nil || p.Description != nil {
		s.noteIssueMentions(ctx, t, actor, i.ID)
		s.noteNamedRelations(ctx, t, actor, i.ID, i.Title+"\n"+i.Description)
		return s.reindex(ctx, t, i.ID)
	}
	return nil
}

// textWritten records who wrote an issue's text (WP-D11): an agent's
// writing waits for a person's review again; a person's is theirs.
func textWritten(actor *Actor, author string) (sets []string, args []any) {
	return []string{"text_by = ?", "text_agent = ?", "reviewed_by = NULL", "reviewed_at = NULL"},
		[]any{nullUser(actor.ID()), author}
}

// agentTools are identities that name a coding tool, not the model in it.
var agentTools = map[string]bool{"anthropic/claude-code": true, "google/gemini-cli": true, "google/antigravity": true, "openai/codex": true, "opencode": true}

// authorAgent is the agent an issue's provenance names (WP-242): for a
// tool's identity, the model its session reported (anthropic/claude-code
// in an Opus session writes as anthropic/claude-opus-5-5), so people read
// which model wrote it. Claims and matching keep actor.AgentName().
func authorAgent(ctx context.Context, t *tx, actor *Actor) string {
	if actor != nil && actor.TextAgent != "" {
		return actor.TextAgent
	}
	a := actor.AgentName()
	if !agentTools[a] || actor.Session == 0 {
		return a
	}
	var model string
	if err := t.QueryRowContext(ctx, `SELECT model FROM agent_sessions WHERE id = ?`, actor.Session).Scan(&model); err != nil {
		return a
	}
	model, _, _ = strings.Cut(strings.TrimSpace(model), "[")
	switch {
	case model == "" || len(model) > 80:
		return a
	case strings.Contains(model, "/"):
		return model
	}
	if p, _, ok := strings.Cut(a, "/"); ok {
		return p + "/" + model
	}
	return model
}

// setStatus adds the status change to an update and emits its events,
// including reopened and "all children done" on the parent.
func (s *Service) setStatus(ctx context.Context, t *tx, i *Issue, st *Status, sets *[]string, args *[]any) error {
	wasClosed, nowClosed := isClosed(i.Status.Category), isClosed(st.Category)
	if st.Category == CatDone && i.Status.Category != CatDone {
		if err := s.refuseDoneWithOpenChildren(ctx, t, i, st); err != nil {
			return err
		}
	}
	*sets, *args = append(*sets, "status_id = ?"), append(*args, st.ID)
	if nowClosed && !wasClosed {
		agent := t.Actor.AgentName()
		if agent == "" && closesForWork(t.Actor) {
			var err error
			if agent, err = s.workAgent(ctx, t, i.ID); err != nil {
				return err
			}
		}
		*sets = append(*sets, "resolved_at = ?", "resolved_by = ?", "resolved_agent = ?", "close_review = ?",
			"close_reviewed_by = NULL", "close_reviewed_at = NULL", "agent_order = NULL")
		*args = append(*args, s.nowMS(), nullUser(t.Actor.ID()), agent, agent != "")
	} else if wasClosed && nowClosed && t.Actor.AgentName() != "" {
		*sets = append(*sets, "resolved_at = ?", "resolved_by = ?", "resolved_agent = ?", "close_review = 1", "close_reviewed_by = NULL", "close_reviewed_at = NULL")
		*args = append(*args, s.nowMS(), nullUser(t.Actor.ID()), t.Actor.AgentName())
	} else if !nowClosed && wasClosed {
		*sets = append(*sets, "resolved_at = NULL", "resolved_by = NULL", "resolved_agent = ''",
			"close_review = 0", "close_reviewed_by = NULL", "close_reviewed_at = NULL")
	}
	t.emit(&Event{Kind: EvIssueStatus, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "status",
		Old: i.Status.Name, New: st.Name,
		Data: map[string]any{"old_id": i.Status.ID, "new_id": st.ID, "old_category": i.Status.Category, "new_category": st.Category}})
	if wasClosed && !nowClosed {
		t.emit(&Event{Kind: EvIssueReopened, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
	}
	if nowClosed && !wasClosed {
		childID := i.ID
		t.afterUpdate(func() { s.checkParentChildrenDone(ctx, t, childID) })
	}
	return nil
}

// refuseDoneWithOpenChildren keeps a parent from being marked done while
// any of its sub-tasks is still open: done would claim work nobody finished
// (WP-680 was closed with 8 of its 9 sub-tasks still To Do). Won't Do is
// still allowed: dropping an epic is a decision, not a claim it was done (WP-ADR123).
func (s *Service) refuseDoneWithOpenChildren(ctx context.Context, t *tx, i *Issue, st *Status) error {
	rows, err := t.QueryContext(ctx, `SELECT p.key || '-' || c.number FROM issues c
		JOIN statuses cs ON cs.id = c.status_id JOIN projects p ON p.id = c.project_id
		WHERE c.parent_id = ? AND cs.category NOT IN ('done','canceled') ORDER BY c.number`, i.ID)
	if err != nil {
		return fmt.Errorf("open sub-tasks: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	list := strings.Join(keys, ", ")
	if len(keys) > 5 {
		list = strings.Join(keys[:5], ", ") + fmt.Sprintf(" and %d more", len(keys)-5)
	}
	return invalid("%s can't be %s while %d of its sub-tasks %s still open (%s): finish or close them first",
		i.Key, st.Name, len(keys), map[bool]string{true: "is", false: "are"}[len(keys) == 1], list)
}

// closesForWork reports whether the actor closes on behalf of whoever did
// the work rather than as a person deciding to: an automation (the merge
// rule), a forge sync, a permission token or a runner job (WP-500).
func closesForWork(a *Actor) bool {
	return a != nil && (a.Automation != nil || a.ForgeSync || a.Permissions != nil || a.RunnerJob != 0)
}

// workAgent is the agent that last worked on an issue: its latest run, or
// the agent behind a pull request linked to it. Empty when a person did
// the work.
func (s *Service) workAgent(ctx context.Context, t *tx, issueID int64) (string, error) {
	var agent string
	err := t.QueryRowContext(ctx, `SELECT agent FROM (
		SELECT agent, started_at AS at FROM agent_runs WHERE issue_id = ? AND agent != ''
		UNION ALL
		SELECT coalesce(nullif(pr.agent, ''), l.created_agent), l.created_at FROM external_links l
			LEFT JOIN pull_requests pr ON pr.url = l.url
		WHERE l.issue_id = ? AND l.kind = 'pull' AND coalesce(nullif(pr.agent, ''), l.created_agent) != '')
		ORDER BY at DESC LIMIT 1`, issueID, issueID).Scan(&agent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("work agent: %w", err)
	}
	return agent, nil
}

// afterUpdate defers work until the issue's UPDATE is written. applyPatch
// runs the queue right after its UPDATE; write runs whatever is still queued
// when the callback returns, before it commits, so a caller that writes its
// own UPDATE (removing a pull link) can't lose it. The work returns no error:
// it logs its failures and the transaction carries on.
func (t *tx) afterUpdate(f func()) { t.Defer(f) }

// runPending runs queued afterUpdate work, including work that queues more.
func (t *tx) runPending() { t.RunDeferred() }

// checkParentChildrenDone runs checkChildrenDone for a closed issue's
// parent, read after the UPDATE: the same update may have moved the issue to
// another parent. applyPatch checks the one it left.
func (s *Service) checkParentChildrenDone(ctx context.Context, t *tx, childID int64) {
	var parentID sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT parent_id FROM issues WHERE id = ?`, childID).Scan(&parentID); err != nil {
		s.log.WarnContext(ctx, "check children done", "issue", childID, "err", err)
		return
	}
	if parentID.Valid {
		s.checkChildrenDone(ctx, t, parentID.Int64)
	}
}

// checkChildrenDone emits children_done on a parent when its last open
// child closes.
func (s *Service) checkChildrenDone(ctx context.Context, t *tx, parentID int64) {
	var open, total int
	if err := t.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(st.category NOT IN ('done','canceled')), 0)
		FROM issues c JOIN statuses st ON st.id = c.status_id WHERE c.parent_id = ?`, parentID).Scan(&total, &open); err != nil {
		s.log.WarnContext(ctx, "check children done", "err", err)
		return
	}
	if total == 0 || open > 0 {
		return
	}
	par, err := s.issueByID(ctx, t, parentID, false)
	if err != nil {
		s.log.WarnContext(ctx, "check children done", "parent", parentID, "err", err)
		return
	}
	if par.Closed() {
		return
	}
	t.emit(&Event{Kind: EvIssueChildrenDone, ProjectID: par.ProjectID, IssueID: par.ID, IssueKey: par.Key})
}

func floatPtrEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func int64PtrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtFloatPtr(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}

// Transition moves an issue to a status by name or category; a shortcut for agents.
// An optional comment is recorded atomically with the move.
// needDelegate refuses a permission token that would change who may take an
// issue without the queue permission (WP-111): delegating work to agents
// queues it for runners that start sessions on their own.
func needDelegate(actor *Actor, agent *string) error {
	if agent == nil || !actor.Restricted() {
		return nil
	}
	return actor.Need(PermAgentsManage)
}

func (s *Service) Transition(ctx context.Context, actor *Actor, ref, status, comment string) (*IssueDetail, error) {
	return s.UpdateIssue(ctx, actor, ref, IssuePatch{Status: &status, Comment: comment})
}

// DeleteIssue deletes an issue. Its children move up to its parent unless
// cascade is set, in which case they are deleted too.
// errPublishByPerson refuses publishing from anything that can't see
// drafts: it is a person's act (WP-42).
var errPublishByPerson = forbidden("publishing a draft is for people: do it in the web app or your own terminal, not as or inside an agent")

// PublishIssue makes a draft, and the drafts under it, visible to agents
// (WP-42). Only a person can, and only where drafts show.
func (s *Service) PublishIssue(ctx context.Context, actor *Actor, ref string) (*IssueDetail, error) {
	if actor.HidesDrafts() {
		return nil, errPublishByPerson
	}
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if !i.Draft {
			return invalid("%s isn't a draft", i.Key)
		}
		ids, err := queryIDs(ctx, t, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT x.id FROM issues x JOIN d ON x.parent_id = d.id WHERE x.outside = 0)
			SELECT i.id FROM d JOIN issues i ON i.id = d.id WHERE i.draft = 1`, i.ID)
		if err != nil {
			return err
		}
		if i.Outside {
			t.audit(AuditEntry{Action: "outside.approve", Category: AuditApproval, ProjectID: i.ProjectID, TargetType: "issue", TargetID: i.ID,
				Target: i.Key, URL: "/issue/" + i.Key, Detail: "approved an issue by " + cmp(i.OutsideBy, "someone") + " from outside"})
		}
		now := s.nowMS()
		for _, id := range ids {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET draft = 0, outside = 0, outside_by = '', outside_forge = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
				return err
			}
			var key string
			if err := t.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, id).Scan(&key); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: id, IssueKey: key, Field: "draft", Old: "draft", New: "published"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, ref)
}

// DraftIssue takes an issue back to being a draft, with everything under
// it (WP-42): agents stop seeing it. Hiding work from agents leaks
// nothing, so anyone who can edit it may, but not while an agent holds it.
func (s *Service) DraftIssue(ctx context.Context, actor *Actor, ref string) (*IssueDetail, error) {
	var key string
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if i.Draft {
			return invalid("%s is already a draft", i.Key)
		}
		key, issueID = i.Key, i.ID
		ids, err := queryIDs(ctx, t, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT x.id FROM issues x JOIN d ON x.parent_id = d.id) SELECT id FROM d`, i.ID)
		if err != nil {
			return err
		}
		now := s.nowMS()
		for _, id := range ids {
			var held string
			if err := t.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id
				WHERE i.id = ? AND i.claimed_by != '' AND i.claimed_until > ?`, id, now).Scan(&held); err == nil {
				return invalid("%s is being worked on by an agent; release it first", held)
			}
		}
		for _, id := range ids {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET draft = 1, agent_order = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "draft", Old: "published", New: "draft"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if actor.HidesDrafts() {
		i, err := s.issueByID(ctx, s.db.R, issueID, false)
		if err != nil {
			return nil, err
		}
		return &IssueDetail{Issue: i}, nil
	}
	return s.GetIssue(ctx, actor, key)
}

func (s *Service) DeleteIssue(ctx context.Context, actor *Actor, ref string, cascade bool) error {
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if err := s.requireProjectRole(ctx, t, actor, i.ProjectID, RoleEditor); err != nil && actor.Automation == nil {
			return err
		}
		ids := []int64{i.ID}
		if cascade {
			rows, err := t.QueryContext(ctx, `WITH RECURSIVE d(id, depth) AS (SELECT id, 0 FROM issues WHERE parent_id = ?
				UNION ALL SELECT x.id, d.depth + 1 FROM issues x JOIN d ON x.parent_id = d.id WHERE d.depth < 50) SELECT id FROM d`, i.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var c int64
				if err := rows.Scan(&c); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, c)
			}
			rows.Close()
		} else {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET parent_id = ? WHERE parent_id = ?`, nullInt(i.ParentID), i.ID); err != nil {
				return err
			}
		}
		if err := s.objectiveDeleting(ctx, t, actor, ids); err != nil {
			return err
		}
		ph, args := placeholders(len(ids)), int64Args(ids)
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_search WHERE rowid IN (`+ph+`)`, args...); err != nil {
			return err
		}
		slices.Reverse(ids)
		for _, id := range ids {
			if _, err := t.ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, id); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvIssueDeleted, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Old: i.Title, Draft: i.Draft})
		detail := "deleted “" + excerpt(i.Title, 120) + "”"
		if i.Draft {
			detail = "deleted a draft"
		}
		if len(ids) > 1 {
			detail += fmt.Sprintf(" and %d sub-issues", len(ids)-1)
		}
		t.audit(AuditEntry{Action: "issue.delete", Category: AuditControl, ProjectID: i.ProjectID, TargetType: "issue", TargetID: i.ID,
			Target: i.Key, Detail: detail})
		if i.ParentID != nil {
			s.checkChildrenDone(ctx, t, *i.ParentID)
		}
		return nil
	})
}

// MoveToProject moves an issue and everything beneath it to another project.
// Issues get new keys (old keys keep resolving); statuses map by name, then
// by category; labels are recreated by name; sprints are cleared.
func (s *Service) MoveToProject(ctx context.Context, actor *Actor, ref, projectRef string) (*IssueDetail, error) {
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if err := s.requireProjectRole(ctx, t, actor, i.ProjectID, RoleEditor); err != nil {
			return err
		}
		dst, err := s.projectByRef(ctx, t, projectRef)
		if err != nil {
			return err
		}
		if err := s.requireProjectRole(ctx, t, actor, dst.ID, RoleEditor); err != nil {
			return err
		}
		if dst.ID == i.ProjectID {
			id = i.ID
			return nil
		}
		id = i.ID
		rows, err := t.QueryContext(ctx, `WITH RECURSIVE d(id, depth) AS (SELECT ?, 0
			UNION ALL SELECT x.id, d.depth + 1 FROM issues x JOIN d ON x.parent_id = d.id WHERE d.depth < 50)
			SELECT d.id, i.number, st.name, st.category FROM d JOIN issues i ON i.id = d.id JOIN statuses st ON st.id = i.status_id ORDER BY d.depth, i.rank`, i.ID)
		if err != nil {
			return err
		}
		type moving struct {
			id       int64
			number   int64
			status   string
			category string
		}
		var all []moving
		for rows.Next() {
			var m moving
			if err := rows.Scan(&m.id, &m.number, &m.status, &m.category); err != nil {
				rows.Close()
				return err
			}
			all = append(all, m)
		}
		rows.Close()
		var srcKey string
		if err := t.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, i.ProjectID).Scan(&srcKey); err != nil {
			return err
		}
		for _, m := range all {
			st, err := s.resolveStatus(ctx, t, dst.ID, m.status)
			if err != nil {
				if st, err = s.firstStatusIn(ctx, t, dst.ID, m.category); err != nil {
					if st, err = s.statusByID(ctx, t, dst.Settings.DefaultStatusID); err != nil {
						return err
					}
				}
			}
			var number int64
			if err := t.QueryRowContext(ctx, `UPDATE projects SET next_number = next_number + 1 WHERE id = ? RETURNING next_number - 1`, dst.ID).Scan(&number); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO issue_aliases(project_key, number, issue_id) VALUES (?,?,?)`, srcKey, m.number, m.id); err != nil {
				return err
			}
			var labelNames []string
			lr, err := t.QueryContext(ctx, `SELECT l.name FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = ?`, m.id)
			if err != nil {
				return err
			}
			for lr.Next() {
				var n string
				if err := lr.Scan(&n); err != nil {
					lr.Close()
					return err
				}
				labelNames = append(labelNames, n)
			}
			lr.Close()
			if _, err := t.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id = ?`, m.id); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET project_id = ?, number = ?, status_id = ?, sprint_id = NULL, updated_at = ? WHERE id = ?`,
				dst.ID, number, st.ID, s.nowMS(), m.id); err != nil {
				return err
			}
			for _, n := range labelNames {
				if err := s.addLabel(ctx, t, m.id, dst.ID, n); err != nil {
					return err
				}
			}
			newKey := fmt.Sprintf("%s-%d", dst.Key, number)
			t.emit(&Event{Kind: EvIssueUpdated, ProjectID: dst.ID, IssueID: m.id, IssueKey: newKey, Field: "project",
				Old: fmt.Sprintf("%s-%d", srcKey, m.number), New: newKey})
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID})
			if err := s.reindex(ctx, t, m.id); err != nil {
				return err
			}
		}
		if i.ParentID != nil {
			if _, err := t.ExecContext(ctx, `UPDATE issues SET parent_id = NULL WHERE id = ?`, i.ID); err != nil {
				return err
			}
			s.checkChildrenDone(ctx, t, *i.ParentID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, actor, itoa64(id))
}

// Watch subscribes or unsubscribes the actor from an issue's notifications.
func (s *Service) Watch(ctx context.Context, actor *Actor, ref string, on bool) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return err
	}
	if on {
		_, err = s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO issue_watchers(issue_id, user_id) VALUES (?,?)`, i.ID, actor.ID())
	} else {
		_, err = s.db.W.ExecContext(ctx, `DELETE FROM issue_watchers WHERE issue_id = ? AND user_id = ?`, i.ID, actor.ID())
	}
	return err
}

// ShareIssue gives a user access to an issue and everything beneath it,
// without making them a project member. Role "" removes the share.
func (s *Service) ShareIssue(ctx context.Context, actor *Actor, ref, userRef, role string) error {
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if err := s.requireProjectRole(ctx, t, actor, i.ProjectID, RoleEditor); err != nil {
			return err
		}
		u, err := s.resolveUser(ctx, t, actor, userRef)
		if err != nil {
			return err
		}
		if role == "" {
			_, err = t.ExecContext(ctx, `DELETE FROM issue_shares WHERE issue_id = ? AND user_id = ?`, i.ID, u.ID)
			return err
		}
		if role != "editor" && role != "viewer" {
			return invalid("share role must be editor or viewer")
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO issue_shares(issue_id, user_id, role, created_by, created_at) VALUES (?,?,?,?,?)
			ON CONFLICT(issue_id, user_id) DO UPDATE SET role = excluded.role`, i.ID, u.ID, role, actor.ID(), s.nowMS()); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_watchers(issue_id, user_id) VALUES (?,?)`, i.ID, u.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueShared, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: u.Username,
			Data: map[string]any{"user_id": u.ID, "role": role}})
		return nil
	})
}

func (s *Service) relations(ctx context.Context, q queryer, actor *Actor, issueID int64) ([]Relation, error) {
	const cols = `r.id, r.kind, o.id, o.project_id, p.key || '-' || o.number, o.title, o.type, st.category, st.name, o.resolved_at, r.named_in IS NOT NULL, coalesce(r.found, ''),
		r.subkind, r.basis, r.created_by, r.created_agent, r.score, r.created_at,
		coalesce((SELECT e.note FROM rewrite_evidence e WHERE r.found = 'rewrite' AND ((e.issue_id = r.source_id AND e.other_id = r.target_id) OR (e.issue_id = r.target_id AND e.other_id = r.source_id)) LIMIT 1), '')`
	rows, err := q.QueryContext(ctx, `
		SELECT `+cols+`, 0
		FROM issue_relations r JOIN issues o ON o.id = r.target_id JOIN projects p ON p.id = o.project_id JOIN statuses st ON st.id = o.status_id
		WHERE r.source_id = ?
		UNION ALL
		SELECT `+cols+`, 1
		FROM issue_relations r JOIN issues o ON o.id = r.source_id JOIN projects p ON p.id = o.project_id JOIN statuses st ON st.id = o.status_id
		WHERE r.target_id = ?`, issueID, issueID)
	if err != nil {
		return nil, err
	}
	var all []Relation
	var projectOf []int64
	var byOf []sql.NullInt64
	for rows.Next() {
		var r Relation
		var inverse bool
		var pid, created int64
		var resolved, by sql.NullInt64
		var basis sql.NullString
		var score sql.NullFloat64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Issue.ID, &pid, &r.Issue.Key, &r.Issue.Title, &r.Issue.Type, &r.State, &r.Status, &resolved, &r.Named, &r.Found,
			&r.Subkind, &basis, &by, &r.CreatedAgent, &score, &created, &r.Note, &inverse); err != nil {
			rows.Close()
			return nil, err
		}
		r.ResolvedAt = ptrTime(resolved)
		r.Kind = relationFrom(r.Kind, inverse)
		r.Basis = relationBasisName(basis)
		if score.Valid {
			r.Score = &score.Float64
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		all = append(all, r)
		projectOf = append(projectOf, pid)
		byOf = append(byOf, by)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for n := range all {
		all[n].CreatedBy = s.userRefByID(ctx, q, byOf[n])
	}
	out := []Relation{}
	for n, r := range all {
		if s.canSee(ctx, q, actor, r.Issue.ID, projectOf[n]) {
			out = append(out, r)
		}
	}
	return out, nil
}

// RelationInput relates an issue to another: Kind is any name in
// relationKindList, either end's (inverse kinds are stored flipped); Subkind
// narrows it, "" none. "A follows B": A is a follow-up of B (WP-255).
type RelationInput struct {
	Kind    string `json:"kind"`
	Issue   string `json:"issue"`
	Subkind string `json:"subkind"`
}

// AddRelation links two issues: ref <kind> otherRef.
func (s *Service) AddRelation(ctx context.Context, actor *Actor, ref, kind, otherRef string) error {
	_, err := s.Relate(ctx, actor, ref, RelationInput{Kind: kind, Issue: otherRef})
	return err
}

// RelationMade is the relation Relate recorded, which isn't always the one
// asked for: "relates" to a task done before this one was filed is a
// follow-up (WP-1003).
type RelationMade struct {
	// Kind as ref reads it.
	Kind string `json:"kind"`
	// Found says why Waypoint recorded another kind than the one asked
	// for ("after"); "" it didn't.
	Found string `json:"found,omitempty"`
}

// Relate links two issues. Relating a pair that already has the kind again
// changes its subkind when one is given.
func (s *Service) Relate(ctx context.Context, actor *Actor, ref string, in RelationInput) (*RelationMade, error) {
	var made *RelationMade
	err := s.write(ctx, actor, func(t *tx) error {
		a, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		b, _, err := s.getIssueFor(ctx, t, actor, in.Issue, RoleViewer, false)
		if err != nil {
			return err
		}
		made, err = s.relate(ctx, t, actor, a, b, in.Kind, in.Subkind, nil)
		return err
	})
	return made, err
}

// filedAfterDone is the pair as (later, earlier) when one of a and b was
// filed after the other was done, and that one is still done; nil when
// neither was. A sub-task is a slice of its parent, never its follow-up
// (as CreateIssue's session follow-ups).
func filedAfterDone(a, b *Issue) (later, earlier *Issue) {
	for _, p := range [][2]*Issue{{a, b}, {b, a}} {
		l, e := p[0], p[1]
		if e.Status.Category != "done" || e.ResolvedAt == nil || !e.ResolvedAt.Before(l.CreatedAt) {
			continue
		}
		if (l.ParentID != nil && *l.ParentID == e.ID) || (e.ParentID != nil && *e.ParentID == l.ID) {
			return nil, nil
		}
		return l, e
	}
	return nil, nil
}

// relationBasis is who stands behind a relation the actor makes, as
// addLink decides it for links.
func relationBasis(actor *Actor) string {
	switch {
	case actor.Automation != nil && actor.Automation.ID < 0:
		return linkBasisInferred
	case actor.Automation != nil:
		return linkBasisAutomation
	case !actor.ActsAsPerson():
		return linkBasisAgent
	}
	return ""
}

// relationBasisName is a stored basis as the API says it.
func relationBasisName(b sql.NullString) string {
	switch {
	case !b.Valid:
		return ""
	case b.String == "":
		return basisNames[basisPerson]
	}
	return b.String
}

// relate records a relation from a to b. namedIn is the issue whose text
// made it (WP-781); nil for one someone chose, which takes the place of a
// link the text made between the two.
func (s *Service) relate(ctx context.Context, t *tx, actor *Actor, a, b *Issue, kind, subkind string, namedIn *int64) (*RelationMade, error) {
	if a.ID == b.ID {
		return nil, invalid("an issue cannot be related to itself")
	}
	given := strings.ToLower(strings.TrimSpace(kind))
	k, flip, ok := storedRelation(given)
	if !ok {
		return nil, relationKindError()
	}
	subkind = strings.ToLower(strings.TrimSpace(subkind))
	if subkind != "" && !slices.Contains(k.Subkinds, subkind) {
		if len(k.Subkinds) == 0 {
			return nil, invalid("%s takes no subkind", given)
		}
		return nil, invalid("%s can be %s, or nothing", given, strings.Join(k.Subkinds, " or "))
	}
	kind = k.Kind
	src, dst := a, b
	if flip {
		src, dst = b, a
	}
	basis := any(relationBasis(actor))
	if namedIn != nil {
		basis = linkBasisInferred
	}
	found := ""
	if kind == "relates" && namedIn == nil {
		var follower int64
		var had string
		err := t.QueryRowContext(ctx, `SELECT source_id, coalesce(found, '') FROM issue_relations WHERE kind = 'follows'
			AND ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?)) LIMIT 1`, a.ID, b.ID, b.ID, a.ID).Scan(&follower, &had)
		switch {
		case err == nil:
			return &RelationMade{Kind: relationFrom("follows", follower != a.ID), Found: had}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if later, earlier := filedAfterDone(a, b); later != nil && basis != linkBasisInferred {
			k, _, _ = storedRelation("follows")
			kind, found, src, dst = k.Kind, followFoundAfter, later, earlier
			given = relationFrom(kind, a.ID != later.ID)
		}
	}
	if namedIn == nil {
		replacement := given
		if subkind != "" {
			replacement += " (" + subkind + ")"
		}
		if err := s.replaceRelations(ctx, t, a, b, found, replacement); err != nil {
			return nil, err
		}
	}
	var existing int64
	var had string
	symmetric := 0
	if k.Inverse == "" {
		symmetric = 1
	}
	err := t.QueryRowContext(ctx, `SELECT id, subkind FROM issue_relations WHERE kind = ? AND ((source_id = ? AND target_id = ?) OR (? = 1 AND source_id = ? AND target_id = ?))`,
		kind, src.ID, dst.ID, symmetric, dst.ID, src.ID).Scan(&existing, &had)
	made := &RelationMade{Kind: given, Found: found}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var inserted bool
		inserted, err = s.insertRelation(ctx, t, newRelation{Src: src.ID, Dst: dst.ID, Kind: kind, Subkind: subkind, NamedIn: namedIn, Found: found,
			Basis: basis, CreatedBy: actor.ID(), CreatedAgent: actor.CreditAgent()})
		if err == nil && !inserted {
			return made, nil
		}
	case err != nil:
		return nil, err
	case subkind == "" || subkind == had:
		return made, nil
	default:
		sessID, sessTask := sessionStamp(ctx, t, actor)
		_, err = t.ExecContext(ctx, `UPDATE issue_relations SET subkind = ?, basis = ?, created_by = ?, created_agent = ?, session_id = ?, session_issue_id = ? WHERE id = ?`,
			subkind, basis, nullUser(actor.ID()), actor.CreditAgent(), sessID, sessTask, existing)
	}
	if err != nil {
		return nil, err
	}
	if namedIn == nil && basis != linkBasisInferred && actor.ActsAsPerson() {
		if err := s.noteRelated(ctx, t, actor, a.ID, b.ID); err != nil {
			return nil, err
		}
	}
	said := given
	if subkind != "" {
		said += " (" + subkind + ")"
	}
	if holder, named := relationEventHome(a, b); holder == b {
		rel := relationFrom(kind, src.ID != b.ID)
		if subkind != "" {
			rel += " (" + subkind + ")"
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: holder.ProjectID, IssueID: holder.ID, IssueKey: holder.Key, Field: "relation", New: rel + " " + named.Key})
		return made, nil
	}
	t.emit(&Event{Kind: EvIssueUpdated, ProjectID: a.ProjectID, IssueID: a.ID, IssueKey: a.Key, Field: "relation", New: said + " " + b.Key})
	return made, nil
}

// replaceRelations deletes what a relation someone chose between a and b
// takes the place of: a link the text made, and, when found is set (the new
// one is a follow-up), any plain relation. Each deleted row is recorded as a
// removal, with what replaced it as the reason, as RemoveRelation records
// one (WP-999); a draft's key stays out of the published issue's history
// (WP-42), so then the removal is recorded on the draft.
func (s *Service) replaceRelations(ctx context.Context, t *tx, a, b *Issue, found, replacement string) error {
	rows, err := t.QueryContext(ctx, `SELECT id FROM issue_relations WHERE (named_in IS NOT NULL OR found = 'rewrite' OR ? != '') AND kind = 'relates'
		AND ((source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`, found, a.ID, b.ID, b.ID, a.ID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	holder := a
	if b.Draft && !a.Draft {
		holder = b
	}
	for _, id := range ids {
		old, data, err := s.relationGone(ctx, t, holder, id, "", "replaced by "+replacement)
		if err != nil {
			return err
		}
		if err := dismissRewrite(ctx, t, id); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_relations WHERE id = ?`, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: holder.ProjectID, IssueID: holder.ID, IssueKey: holder.Key, Field: "relation", Old: old, Data: data})
	}
	return nil
}

// RemoveRelation deletes a relation by id.
func (s *Service) RemoveRelation(ctx context.Context, actor *Actor, ref string, relationID int64) error {
	return s.write(ctx, actor, func(t *tx) error {
		a, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		var src, dst int64
		err = t.QueryRowContext(ctx, `SELECT source_id, target_id FROM issue_relations WHERE id = ? AND (source_id = ? OR target_id = ?)`, relationID, a.ID, a.ID).Scan(&src, &dst)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("relation")
		} else if err != nil {
			return err
		}
		otherID := dst
		if src != a.ID {
			otherID = src
		}
		other, err := s.issueByID(ctx, t, otherID, false)
		if err != nil {
			return err
		}
		if !s.canSee(ctx, t, actor, other.ID, other.ProjectID) {
			return notFound("relation")
		}
		holder, err := s.relationHolder(ctx, t, a, relationID)
		if err != nil {
			return err
		}
		old, data, err := s.relationGone(ctx, t, holder, relationID, "", "")
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("relation")
		} else if err != nil {
			return err
		}
		if err := dismissRewrite(ctx, t, relationID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM issue_relations WHERE id = ?`, relationID); err != nil {
			return err
		}
		if err := s.unnoteRelated(ctx, t, src, dst); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: holder.ProjectID, IssueID: holder.ID, IssueKey: holder.Key, Field: "relation", Old: old, Data: data})
		return nil
	})
}

// Comment is a message on an issue.
type Comment struct {
	ID        int64     `json:"id"`
	IssueID   int64     `json:"issue_id"`
	Author    *UserRef  `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Via names the forge a mirrored comment came from (Author is then the
	// forge user, with ID 0).
	Via string `json:"via,omitempty"`
	// Agent wrote it through Author's token (WP-D11); Unreviewed until a
	// person reviews it.
	Agent      string   `json:"agent,omitempty"`
	Unreviewed bool     `json:"unreviewed,omitempty"`
	ReviewedBy *UserRef `json:"reviewed_by,omitempty"`
	// Files are attached to the comment itself (WP-D16).
	Files []Attachment `json:"attachments,omitempty"`
}

// ListComments returns an issue's comments, oldest first.
func (s *Service) ListComments(ctx context.Context, actor *Actor, ref string) ([]Comment, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.id, c.body, c.created_at, c.updated_at, u.id, u.username, u.display_name, u.avatar_version,
		c.external_author, c.external_via, c.agent, c.reviewed_at, rv.id, rv.username, rv.display_name, rv.avatar_version
		FROM comments c LEFT JOIN users u ON u.id = c.author_id LEFT JOIN users rv ON rv.id = c.reviewed_by
		WHERE c.issue_id = ? ORDER BY c.created_at, c.id`, i.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var created, updated int64
		var uid, av, rid, rav, reviewedAt sql.NullInt64
		var uname, dname, rname, rdname sql.NullString
		var extAuthor string
		if err := rows.Scan(&c.ID, &c.Body, &created, &updated, &uid, &uname, &dname, &av, &extAuthor, &c.Via,
			&c.Agent, &reviewedAt, &rid, &rname, &rdname, &rav); err != nil {
			return nil, err
		}
		if rid.Valid {
			c.ReviewedBy = &UserRef{ID: rid.Int64, Username: rname.String, DisplayName: cmp(rdname.String, rname.String), AvatarURL: avatarURL(rid.Int64, rav.Int64)}
		}
		c.Unreviewed = c.Agent != "" && !reviewedAt.Valid
		c.IssueID, c.CreatedAt, c.UpdatedAt = i.ID, msTime(created), msTime(updated)
		if uid.Valid {
			c.Author = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		} else if extAuthor != "" {
			c.Author = &UserRef{Username: extAuthor, DisplayName: extAuthor}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.commentFiles(ctx, i.ID, out)
}

// commentFiles fills in the files attached to an issue's comments, in one query.
func (s *Service) commentFiles(ctx context.Context, issueID int64, cs []Comment) error {
	frows, err := s.db.R.QueryContext(ctx, `SELECT `+attachmentCols+attachmentFrom+`WHERE a.target_type = 'comment' AND a.issue_id = ? ORDER BY a.id`, issueID)
	if err != nil {
		return err
	}
	defer frows.Close()
	byID := map[int64]*Comment{}
	for i := range cs {
		byID[cs[i].ID] = &cs[i]
	}
	for frows.Next() {
		a, err := scanAttachment(frows)
		if err != nil {
			return err
		}
		if c := byID[a.TargetID]; c != nil {
			c.Files = append(c.Files, *a)
		}
	}
	return frows.Err()
}

// AddComment comments on an issue. Viewers may comment.
func (s *Service) AddComment(ctx context.Context, actor *Actor, ref, body string) (*Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, invalid("comment is empty")
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	var c *Comment
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleViewer, false)
		if err != nil {
			return err
		}
		c, err = s.addComment(ctx, t, actor, i, body)
		return err
	})
	return c, err
}

func (s *Service) addComment(ctx context.Context, t *tx, actor *Actor, i *Issue, body string) (*Comment, error) {
	if len(body) > maxCommentLen {
		return nil, invalid("comment is too long")
	}
	now := s.nowMS()
	var author any
	if actor.ID() != 0 {
		author = actor.ID()
	}
	agent := cmp(actor.TextAgent, actor.AgentName())
	sessID, sessTask := sessionStamp(ctx, t, actor)
	res, err := t.ExecContext(ctx, `INSERT INTO comments(issue_id, author_id, agent, body, created_at, updated_at, session_id, session_issue_id) VALUES (?,?,?,?,?,?,?,?)`, i.ID, author, agent, body, now, now, sessID, sessTask)
	if err != nil {
		return nil, err
	}
	c := &Comment{IssueID: i.ID, Body: body, CreatedAt: msTime(now), UpdatedAt: msTime(now), Agent: agent, Unreviewed: agent != ""}
	if c.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	if actor.ID() != 0 {
		r := actor.User.Ref()
		c.Author = &r
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO issue_watchers(issue_id, user_id) VALUES (?,?)`, i.ID, actor.ID()); err != nil {
			return nil, err
		}
	}
	if _, err := t.ExecContext(ctx, `UPDATE issues SET updated_at = ? WHERE id = ?`, now, i.ID); err != nil {
		return nil, err
	}
	t.emit(&Event{Kind: EvIssueCommented, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: excerpt(body, 280),
		Data: map[string]any{"comment_id": c.ID}})
	project := i.ProjectID
	s.recordMentions(ctx, t, mentionSource{typ: "comment", id: c.ID, project: &project, text: body, user: actor.ID(), agent: agent})
	s.noteIssueMentions(ctx, t, actor, i.ID)
	return c, s.reindex(ctx, t, i.ID)
}

// addLocalComment is addComment for a comment kept off the forge: it names a
// machine and a local path, which a forge's copy of the task (often a public
// repository's issue) has no use for and shouldn't show. Webhooks and
// automations still see it, as they see everything on the task: they are the
// instance's own integrations, set up by its admins.
func (s *Service) addLocalComment(ctx context.Context, t *tx, actor *Actor, i *Issue, body string) (*Comment, error) {
	c, err := s.addComment(ctx, t, actor, i, body)
	if err != nil {
		return nil, err
	}
	for _, e := range t.Events() {
		if e.Kind == EvIssueCommented && dataInt(e, "comment_id") == c.ID {
			e.Data["local"] = true
		}
	}
	return c, nil
}

// UpdateComment edits a comment (its author only).
func (s *Service) UpdateComment(ctx context.Context, actor *Actor, commentID int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return invalid("comment is empty")
	}
	if len(body) > maxCommentLen {
		return invalid("comment is too long")
	}
	if err := actor.CanWrite(); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		var issueID int64
		var author sql.NullInt64
		var agent string
		if err := t.QueryRowContext(ctx, `SELECT issue_id, author_id, agent FROM comments WHERE id = ?`, commentID).Scan(&issueID, &author, &agent); err != nil {
			return notFound("comment")
		}
		i, _, err := s.getIssueFor(ctx, t, actor, itoa64(issueID), RoleViewer, false)
		if err != nil {
			return notFound("comment")
		}
		if actor.IsAgent() && agent == "" {
			return forbidden("a person wrote this comment; agents can only edit comments agents wrote")
		}
		if author.Int64 != actor.ID() {
			return ErrForbidden
		}
		fc, err := s.forgeCommentChanged(ctx, t, issueID, commentID, false)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE comments SET body = ?, updated_at = ?, agent = ?, reviewed_by = NULL, reviewed_at = NULL WHERE id = ?`,
			body, s.nowMS(), actor.AgentName(), commentID); err != nil {
			return err
		}
		project := i.ProjectID
		s.recordMentions(ctx, t, mentionSource{typ: "comment", id: commentID, project: &project, text: body,
			user: actor.ID(), agent: actor.AgentName()})
		t.emit(withHint(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key}, fc))
		s.noteIssueMentions(ctx, t, actor, issueID)
		return s.reindex(ctx, t, issueID)
	})
}

// DeleteComment removes a comment (its author, or a project owner).
func (s *Service) DeleteComment(ctx context.Context, actor *Actor, commentID int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		var issueID int64
		var author sql.NullInt64
		var agent string
		if err := t.QueryRowContext(ctx, `SELECT issue_id, author_id, agent FROM comments WHERE id = ?`, commentID).Scan(&issueID, &author, &agent); err != nil {
			return notFound("comment")
		}
		i, _, err := s.getIssueFor(ctx, t, actor, itoa64(issueID), RoleViewer, false)
		if err != nil {
			return notFound("comment")
		}
		if actor.IsAgent() && agent == "" {
			return forbidden("a person wrote this comment; agents can only delete comments agents wrote")
		}
		if author.Int64 != actor.ID() {
			if err := s.requireProjectRole(ctx, t, actor, i.ProjectID, RoleOwner); err != nil {
				return ErrForbidden
			}
		}
		fc, err := s.forgeCommentChanged(ctx, t, issueID, commentID, true)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, commentID); err != nil {
			return err
		}
		t.emit(withHint(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key}, fc))
		s.noteIssueMentions(ctx, t, actor, issueID)
		return s.reindex(ctx, t, issueID)
	})
}

// History returns an issue's events, newest first.
func (s *Service) History(ctx context.Context, actor *Actor, ref string, limit int) ([]Event, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	evs, err := s.queryEvents(ctx, `WHERE e.issue_id = ?`, []any{i.ID}, limit)
	if err != nil {
		return nil, err
	}
	return s.hideUnseenAll(ctx, s.db.R, actor, evs), nil
}

func (s *Service) queryEvents(ctx context.Context, where string, args []any, limit int) ([]Event, error) {
	// #nosec G202 -- where is a fixed condition from this package; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT e.id, e.kind, coalesce(e.project_id, 0), coalesce(e.issue_id, 0), coalesce(p.key || '-' || i.number, ''),
		e.field, e.old_value, e.new_value, e.created_at, u.id, u.username, u.display_name, u.avatar_version, coalesce(a.name, json_extract(e.data, '$.via'))
		FROM events e LEFT JOIN users u ON u.id = e.actor_id LEFT JOIN issues i ON i.id = e.issue_id
		LEFT JOIN projects p ON p.id = i.project_id LEFT JOIN automations a ON a.id = e.via_automation
		`+where+` ORDER BY e.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		var uid, av sql.NullInt64
		var uname, dname, auto sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.ProjectID, &e.IssueID, &e.IssueKey, &e.Field, &e.Old, &e.New, &at, &uid, &uname, &dname, &av, &auto); err != nil {
			return nil, err
		}
		e.At = msTime(at)
		if uid.Valid {
			e.Actor = &UserRef{ID: uid.Int64, Username: uname.String, DisplayName: cmp(dname.String, uname.String), AvatarURL: avatarURL(uid.Int64, av.Int64)}
		}
		e.ViaAutomation = auto.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// ProjectActivity returns recent events in a project.
func (s *Service) ProjectActivity(ctx context.Context, actor *Actor, projectRef string, limit int) ([]Event, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := `WHERE e.project_id = ?`
	if actor.HidesDrafts() {
		where += ` AND coalesce(i.draft, 0) = 0`
	}
	evs, err := s.queryEvents(ctx, where, []any{p.ID}, limit)
	if err != nil {
		return nil, err
	}
	return s.hideUnseenAll(ctx, s.db.R, actor, evs), nil
}

// appendMarkdown adds md after text as a new paragraph.
func appendMarkdown(text, md string) string {
	text = strings.TrimRight(text, " \n\t")
	md = strings.TrimSpace(md)
	if text == "" {
		return md
	}
	return text + "\n\n" + md
}
