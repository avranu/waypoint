package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Risk levels, lowest first.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

var riskLevels = []string{RiskLow, RiskMedium, RiskHigh}

func riskRank(level string) int { return slices.Index(riskLevels, level) }

// Risk factors. Each has a fixed level (riskFactorLevel), except
// riskTask, which carries the level of the task behind a pull request.
const (
	riskOutside       = "outside"
	riskHeld          = "held"
	riskOutsidePR     = "outside_pr"
	riskAgentText     = "agent_text"
	riskAgentComments = "agent_comments"
	riskAgentClose    = "agent_close"
	riskAIDetected    = "ai_detected"
	riskAgentProposal = "agent_proposal"
	riskNoTask        = "no_task"
	riskTask          = "task"
	riskNoGoal        = "no_goal"
	riskLooseGoal     = "loose_goal"
)

var riskFactorLevel = map[string]string{
	riskOutside:       RiskHigh,
	riskHeld:          RiskHigh,
	riskOutsidePR:     RiskHigh,
	riskAgentText:     RiskMedium,
	riskAgentComments: RiskMedium,
	riskAgentClose:    RiskMedium,
	riskAIDetected:    RiskMedium,
	riskAgentProposal: RiskMedium,
	riskNoTask:        RiskMedium,
	riskNoGoal:        RiskMedium,
	riskLooseGoal:     RiskLow,
}

// ProvenanceRisk is an object's provenance risk: its level and why.
type ProvenanceRisk struct {
	Level   string       `json:"level"`
	Reasons []RiskReason `json:"reasons,omitempty"`
}

// RiskReason is one factor that applies, at its level.
type RiskReason struct {
	Factor string `json:"factor"`
	Level  string `json:"level"`
	Text   string `json:"text"`
	// Ref is the object the reason is about when it isn't the object
	// itself: a pull request's task.
	Ref string `json:"ref,omitempty"`
}

// Summary is the level and its reasons in one line: "medium: an agent
// (…) wrote it and no person has reviewed it".
func (r *ProvenanceRisk) Summary() string {
	if r == nil {
		return RiskLow
	}
	if len(r.Reasons) == 0 {
		return r.Level
	}
	texts := make([]string, len(r.Reasons))
	for n, x := range r.Reasons {
		texts[n] = x.Text
	}
	return r.Level + ": " + strings.Join(texts, "; ")
}

func riskReason(factor, text string) RiskReason {
	return RiskReason{Factor: factor, Level: riskFactorLevel[factor], Text: text}
}

// riskOf is the level the reasons add up to, the highest of theirs, with
// the reasons highest first (in the order given among equals).
func riskOf(reasons []RiskReason) *ProvenanceRisk {
	r := &ProvenanceRisk{Level: RiskLow, Reasons: reasons}
	for _, x := range reasons {
		if riskRank(x.Level) > riskRank(r.Level) {
			r.Level = x.Level
		}
	}
	slices.SortStableFunc(r.Reasons, func(a, b RiskReason) int { return riskRank(b.Level) - riskRank(a.Level) })
	return r
}

// AIDetection is a watermark detector's reading of text posted with a
// person's login (WP-553). Detected is the detector's own call at its
// false-positive rate; Confirmed: the person said the text is theirs.
// Only a detection counts: a detector finds only its own provider's mark,
// so its silence says nothing about who wrote the text.
type AIDetection struct {
	Detector   string
	Confidence float64
	Detected   bool
	Confirmed  bool
}

// counts says whether the detection makes a person's text read as an
// agent's.
func (d *AIDetection) counts() bool { return d != nil && d.Detected && !d.Confirmed }

func (d *AIDetection) words() string {
	s := "reads as AI-written"
	if d.Detector != "" {
		s += " (" + d.Detector
		if d.Confidence > 0 {
			s += ", " + strconv.FormatFloat(d.Confidence, 'f', 2, 64)
		}
		s += ")"
	}
	return s
}

// textRisk is the reason, if any, that text carries: an agent wrote it and
// no person has reviewed it, or a person's login posted it and it reads as
// AI-written. what names the text ("it", "this comment").
func textRisk(what, unreviewedAgent string, ai *AIDetection) []RiskReason {
	switch {
	case unreviewedAgent != "":
		return []RiskReason{riskReason(riskAgentText, fmt.Sprintf("%s wrote %s and no person has reviewed it", agentPhrase(unreviewedAgent), what))}
	case ai.counts():
		return []RiskReason{riskReason(riskAIDetected, fmt.Sprintf("posted with a person's login, %s %s: it counts as unreviewed agent text until the person says it's theirs", what, ai.words()))}
	}
	return nil
}

// agentPhrase names an agent in a sentence: "agent anthropic/claude-…", or
// "an agent" for one recorded without a name.
func agentPhrase(agent string) string {
	if agent == unnamedAgent {
		return "an agent"
	}
	return "agent " + agent
}

// unnamedAgent is what an agent's work records when it gave no name.
const unnamedAgent = "agent"

// promptRisk is the risk of a prompt Waypoint queued for a session: an
// agent wrote it, itself or as an automation's rule, and no person has
// read it before the session does. automation names the automation whose
// step sent it. A person's prompt and Waypoint's own news are low; what
// news quotes (review findings, CI logs) carries its own provenance.
func promptRisk(agent, automation string) *ProvenanceRisk {
	what := "this prompt"
	if automation != "" {
		what = "this prompt (automation “" + automation + "” sent it)"
	}
	return riskOf(textRisk(what, agent, nil))
}

// automationRisk is the risk of an automation's rule: an agent wrote it
// and no person has saved a rule since, which is how a person reviews one.
// Its steps' prompts, messages and commands all come from the rule.
func automationRisk(a *Automation) *ProvenanceRisk {
	if a.RuleAgent == "" {
		return riskOf(nil)
	}
	return riskOf([]RiskReason{riskReason(riskAgentText, agentPhrase(a.RuleAgent)+" wrote its rule and no person has saved it since")})
}

// issueRiskFacts is what an issue's risk is computed from.
type issueRiskFacts struct {
	Outside       bool   // a forge stranger's text waits on it (people only)
	OutsideBy     string // their login, when known
	Held          bool
	HeldReason    string
	TextAgent     string // the agent that wrote its title and description, while unreviewed
	CommentAgents []string
	Comments      int // agents' unreviewed comments
	CloseAgent    string
	AIDetected    *AIDetection // its title and description, when a person's
	// Grounding is its trace to a goal (WP-ADR50), when its project has a
	// goal and an agent wrote it ("" otherwise).
	Grounding string
}

func issueRisk(f issueRiskFacts) *ProvenanceRisk {
	var rs []RiskReason
	if f.Outside {
		who := "a forge user who isn't a Waypoint person"
		if f.OutsideBy != "" {
			who = "forge user " + f.OutsideBy + ", who isn't a Waypoint person,"
		}
		rs = append(rs, riskReason(riskOutside, "text from "+who+" waits on it for a person's approval"))
	}
	if f.Held {
		rs = append(rs, riskReason(riskHeld, "the safety screen held it for a person: "+strings.TrimSuffix(cmp(f.HeldReason, "no reason given"), ".")))
	}
	rs = append(rs, textRisk("it", f.TextAgent, f.AIDetected)...)
	if f.Comments > 0 {
		what := "an agent's comment"
		if f.Comments > 1 {
			what = strconv.Itoa(f.Comments) + " agents' comments"
		}
		rs = append(rs, riskReason(riskAgentComments, fmt.Sprintf("%s (%s) no person has reviewed", what, strings.Join(f.CommentAgents, ", "))))
	}
	if f.CloseAgent != "" {
		rs = append(rs, riskReason(riskAgentClose, "agent "+f.CloseAgent+" closed it and no person has reviewed the close"))
	}
	rs = append(rs, goalRisk("it", f.Grounding)...)
	return riskOf(rs)
}

// goalRisk is the goal factor for agent work with this grounding, in a
// project that has a goal (WP-ADR50): none is medium; a guess, a goal no
// person approved, or only a person's request above it is a low reason
// that informs without raising the level.
func goalRisk(what, grounding string) []RiskReason {
	switch grounding {
	case GroundNone:
		return []RiskReason{riskReason(riskNoGoal, what+" traces to no goal, and no person wrote or approved anything it hangs from")}
	case GroundInferredGoal:
		return []RiskReason{riskReason(riskLooseGoal, what+" traces to a goal only through a link Waypoint guessed")}
	case GroundProposedGoal:
		return []RiskReason{riskReason(riskLooseGoal, what+" traces only to a goal no person has approved")}
	case GroundPersonRequest:
		return []RiskReason{riskReason(riskLooseGoal, what+" traces to no goal, only to a person's request")}
	case GroundAgentGoal:
		return []RiskReason{riskReason(riskLooseGoal, what+" traces to a person's goal only through links agents made")}
	case groundNotTraced:
		return []RiskReason{riskReason(riskLooseGoal, "Waypoint hasn't traced "+what+" to a goal yet")}
	}
	return nil
}

// groundNotTraced stands in for an agent's item in a project with a goal
// that has no goal_traces row yet: the trace runs in the background after
// each change, so a new item has none for a moment.
const groundNotTraced = "not_traced"

// itemGroundingOrPending is an agent's item's grounding, or groundNotTraced
// when its project has an approved goal and Waypoint hasn't traced it yet.
func (s *Service) itemGroundingOrPending(ctx context.Context, q queryer, typ string, id, projectID int64) string {
	var g string
	var scoped bool
	err := q.QueryRowContext(ctx, `SELECT grounding, scoped FROM goal_traces WHERE item_type = ? AND item_id = ?`, typ, id).Scan(&g, &scoped)
	if err == nil {
		if scoped {
			return g
		}
		return ""
	}
	if projectID != 0 && s.projectHasGoal(ctx, q, projectID) {
		return groundNotTraced
	}
	return ""
}

// projectHasGoal says whether the project, an ancestor or global holds an
// approved goal, as goalGraph.scoped does.
func (s *Service) projectHasGoal(ctx context.Context, q queryer, projectID int64) bool {
	var n int
	_ = q.QueryRowContext(ctx, `WITH RECURSIVE up(id, depth) AS (SELECT ?, 0 UNION ALL SELECT p.parent_id, up.depth + 1 FROM projects p JOIN up ON p.id = up.id
		WHERE p.parent_id IS NOT NULL AND up.depth < 32)
		SELECT count(*) FROM design_notes WHERE kind = 'goal' AND status = 'active' AND (approved_at IS NOT NULL OR requested_by IS NOT NULL)
		AND (project_id IS NULL OR project_id IN (SELECT id FROM up))`, projectID).Scan(&n)
	return n > 0
}

// issueRiskCond is each issue factor as an SQL condition on issues i, for
// the actor: the facts for one issue and WQL's risk: are read through the
// same conditions, so they agree. Watermark detections (WP-553) aren't
// recorded yet, so ai_detected has no condition.
func issueRiskConds(actor *Actor) [][2]string {
	outside := "(i.outside = 1 OR EXISTS (SELECT 1 FROM outside_text o WHERE o.issue_id = i.id AND o.discarded = 0))"
	if actor.HidesDrafts() {
		outside = "0"
	}
	return [][2]string{
		{riskOutside, outside},
		{riskHeld, "(" + sqlUnreviewed + " AND EXISTS (SELECT 1 FROM task_safety ts WHERE ts.issue_id = i.id AND ts.verdict = 'hold'))"},
		{riskAgentText, "(i.text_agent != '' AND i.reviewed_at IS NULL)"},
		{riskAgentComments, "EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id AND c.agent != '' AND c.reviewed_at IS NULL)"},
		{riskAgentClose, "(" + closeUnreviewed("i") + ")"},
		{riskNoGoal, "EXISTS (SELECT 1 FROM goal_traces gt WHERE gt.item_type = 'issue' AND gt.item_id = i.id AND gt.grounding = 'none' AND gt.scoped = 1)"},
	}
}

// issueRiskLevelCond is the SQL condition for issues whose level is one of
// levels.
func issueRiskLevelCond(actor *Actor, levels []string) string {
	byLevel := map[string][]string{}
	for _, c := range issueRiskConds(actor) {
		l := riskFactorLevel[c[0]]
		byLevel[l] = append(byLevel[l], c[1])
	}
	or := func(level string) string {
		if len(byLevel[level]) == 0 {
			return "0"
		}
		return "(" + strings.Join(byLevel[level], " OR ") + ")"
	}
	exact := map[string]string{
		RiskHigh:   or(RiskHigh),
		RiskMedium: "(NOT " + or(RiskHigh) + " AND " + or(RiskMedium) + ")",
		RiskLow:    "(NOT " + or(RiskHigh) + " AND NOT " + or(RiskMedium) + ")",
	}
	var parts []string
	for _, l := range levels {
		parts = append(parts, exact[l])
	}
	if len(parts) == 0 {
		return "0"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// riskLevelsFor reads a WQL risk: term: the levels op values names.
// risk:medium, risk:medium,high, risk:>=medium, risk:!=low.
func riskLevelsFor(op string, vals []string) ([]string, error) {
	var ranks []int
	for _, v := range vals {
		r := riskRank(strings.ToLower(strings.TrimSpace(v)))
		if r < 0 {
			return nil, invalid("risk is low, medium or high, not %q", v)
		}
		ranks = append(ranks, r)
	}
	var out []string
	for n, l := range riskLevels {
		match := false
		switch op {
		case "", "=":
			match = slices.Contains(ranks, n)
		case "!=":
			match = !slices.Contains(ranks, n)
		case ">":
			match = n > ranks[0]
		case ">=":
			match = n >= ranks[0]
		case "<":
			match = n < ranks[0]
		case "<=":
			match = n <= ranks[0]
		default:
			return nil, invalid("risk can't be compared with %s", op)
		}
		if match {
			out = append(out, l)
		}
	}
	return out, nil
}

// issueRiskFacts reads an issue's facts.
func (s *Service) issueRiskFacts(ctx context.Context, q queryer, actor *Actor, issueID int64) (issueRiskFacts, error) {
	var f issueRiskFacts
	conds := issueRiskConds(actor)
	cols := make([]string, len(conds))
	on := make([]bool, len(conds))
	dest := make([]any, 0, len(conds)+6)
	for n, c := range conds {
		cols[n] = c[1]
		dest = append(dest, &on[n])
	}
	var textAgent, closeAgent, outsideBy, heldReason, commentAgents, grounding string
	var agentText bool
	var projectID int64
	dest = append(dest, &textAgent, &closeAgent, &outsideBy, &heldReason, &commentAgents, &f.Comments, &agentText, &projectID)
	// #nosec G202 -- the conditions are fixed SQL; the id is bound
	err := q.QueryRowContext(ctx, `SELECT `+strings.Join(cols, ", ")+`,
		i.text_agent, i.resolved_agent,
		coalesce(nullif(i.outside_by, ''), (SELECT o.author FROM outside_text o WHERE o.issue_id = i.id AND o.discarded = 0 ORDER BY o.id LIMIT 1), ''),
		coalesce((SELECT ts.reason FROM task_safety ts WHERE ts.issue_id = i.id), ''),
		coalesce((SELECT group_concat(agent, ', ') FROM (SELECT DISTINCT agent FROM comments WHERE issue_id = i.id AND agent != '' AND reviewed_at IS NULL ORDER BY agent)), ''),
		(SELECT count(*) FROM comments WHERE issue_id = i.id AND agent != '' AND reviewed_at IS NULL),
		i.text_agent != '' AND i.reviewed_at IS NULL, i.project_id
		FROM issues i WHERE i.id = ?`, issueID).Scan(dest...)
	if err != nil {
		return f, err
	}
	if agentText {
		grounding = s.itemGroundingOrPending(ctx, q, KIssue, issueID, projectID)
	}
	for n, c := range conds {
		if !on[n] {
			continue
		}
		switch c[0] {
		case riskOutside:
			f.Outside, f.OutsideBy = true, outsideBy
		case riskHeld:
			f.Held, f.HeldReason = true, heldReason
		case riskAgentText:
			f.TextAgent = textAgent
		case riskAgentComments:
			if commentAgents != "" {
				f.CommentAgents = strings.Split(commentAgents, ", ")
			}
		case riskAgentClose:
			f.CloseAgent = cmp(closeAgent, "an agent")
		}
	}
	f.Grounding = grounding
	for n, c := range conds {
		if c[0] == riskNoGoal && on[n] {
			f.Grounding = GroundNone
		}
	}
	return f, nil
}

func (s *Service) issueRiskByID(ctx context.Context, q queryer, actor *Actor, issueID int64) (*ProvenanceRisk, error) {
	f, err := s.issueRiskFacts(ctx, q, actor, issueID)
	if err != nil {
		return nil, err
	}
	return issueRisk(f), nil
}

// Kinds of object a risk is asked for.
const (
	RiskKindIssue    = "issue"
	RiskKindComment  = "comment"
	RiskKindPage     = "page"
	RiskKindDesign   = "design"
	RiskKindGlossary = "glossary"
	RiskKindPull     = "pull"
	// A prompt Waypoint queued for an agent session (session_messages).
	RiskKindPrompt     = "prompt"
	RiskKindAutomation = "automation"
)

// designRisk: an agent's design note counts once a person has approved it
// or a person's task asked for it; pending changes to an approved note
// don't, since the approved text is what stands (WP-D10).
func designRisk(n *DesignNote) *ProvenanceRisk {
	agent := cmp(n.UpdatedAgent, n.CreatedAgent)
	if n.Approved || n.Requested != nil || agent == "" {
		return riskOf(nil)
	}
	return riskOf([]RiskReason{riskReason(riskAgentProposal, "agent "+agent+" wrote it; no person has approved it and no person's task asked for it")})
}

// prRisk is a pull request's risk: a stranger's is high; otherwise the
// highest of its tasks', or medium for an agent's that carries none.
func prRisk(outsideBy string, outside bool, agent string, tasks map[string]*ProvenanceRisk, order []string) *ProvenanceRisk {
	var rs []RiskReason
	if outside {
		rs = append(rs, riskReason(riskOutsidePR, "opened by "+cmp(outsideBy, "a forge user")+", who isn't a Waypoint person, and waiting for a person"))
	}
	for _, key := range order {
		r := tasks[key]
		if r == nil || r.Level == RiskLow {
			continue
		}
		rs = append(rs, RiskReason{Factor: riskTask, Level: r.Level, Ref: key, Text: key + " is " + r.Summary()})
	}
	if len(order) == 0 && agent != "" && !outside {
		rs = append(rs, riskReason(riskNoTask, "agent "+agent+" opened it for no task: no person asked for it"))
	}
	return riskOf(rs)
}

// RiskOf is the provenance risk of the object kind ref names, for an
// actor who can see it: an issue key, a comment id, a page number or code,
// a design note code or id, a glossary entry id, a pull request id
// (project helps with a bare code).
func (s *Service) RiskOf(ctx context.Context, actor *Actor, kind, ref, project string) (*ProvenanceRisk, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if p, ok := map[string]Permission{RiskKindPage: PermPagesRead, RiskKindDesign: PermDesignRead, RiskKindGlossary: PermGlossaryRead, RiskKindPull: PermPullsRead,
		RiskKindPrompt: PermSessionsRead, RiskKindAutomation: PermAutomationsRead}[kind]; ok {
		if err := actor.Need(p); err != nil {
			return nil, err
		}
	}
	q := s.db.R
	switch kind {
	case RiskKindIssue:
		i, _, err := s.getIssueFor(ctx, q, actor, ref, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		return s.issueRiskByID(ctx, q, actor, i.ID)
	case RiskKindComment:
		var issueID int64
		var agent string
		var reviewed sql.NullInt64
		id := parseID(ref)
		if err := q.QueryRowContext(ctx, `SELECT issue_id, agent, reviewed_at FROM comments WHERE id = ?`, id).Scan(&issueID, &agent, &reviewed); err != nil {
			return nil, notFound("comment")
		}
		if _, _, err := s.getIssueFor(ctx, q, actor, itoa64(issueID), RoleViewer, false); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, notFound("comment")
			}
			return nil, err
		}
		if reviewed.Valid {
			agent = ""
		}
		return riskOf(textRisk("this comment", agent, nil)), nil
	case RiskKindPage:
		id, err := s.ResolvePage(ctx, actor, ref, project)
		if err != nil {
			return nil, err
		}
		pg, _, err := s.pageFor(ctx, q, actor, id, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		agent := ""
		if pg.Unreviewed {
			agent = pg.TextAgent
		}
		rs := textRisk("its text", agent, nil)
		if agent != "" {
			rs = append(rs, goalRisk("the page", s.itemGroundingOrPending(ctx, q, KPage, pg.ID, derefInt(pg.ProjectID)))...)
		}
		return riskOf(rs), nil
	case RiskKindDesign:
		id, err := s.ResolveDesign(ctx, actor, ref, project)
		if err != nil {
			return nil, err
		}
		n, err := s.GetDesign(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		r := designRisk(n)
		if n.Kind != "goal" && !n.Approved && n.Requested == nil && cmp(n.UpdatedAgent, n.CreatedAgent) != "" {
			r = riskOf(append(r.Reasons, goalRisk("the note", s.itemGroundingOrPending(ctx, q, KDesign, n.ID, derefInt(n.ProjectID)))...))
		}
		return r, nil
	case RiskKindGlossary:
		g, _, err := s.glossaryFor(ctx, q, actor, parseID(ref), RoleViewer)
		if err != nil {
			return nil, err
		}
		agent := ""
		if g.Unreviewed {
			agent = g.UpdatedAgent
		}
		return riskOf(textRisk("the entry", agent, nil)), nil
	case RiskKindPull:
		return s.pullRisk(ctx, actor, parseID(ref))
	case RiskKindPrompt:
		var sessionID, automationID int64
		var agent, automation string
		if err := q.QueryRowContext(ctx, `SELECT m.session_id, m.agent, coalesce(m.automation_id, 0), coalesce(a.name, '')
			FROM session_messages m LEFT JOIN automations a ON a.id = m.automation_id WHERE m.id = ?`, parseID(ref)).Scan(&sessionID, &agent, &automationID, &automation); err != nil {
			return nil, notFound("prompt")
		}
		if _, err := s.sessionByID(ctx, q, actor, sessionID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, notFound("prompt")
			}
			return nil, err
		}
		return promptRisk(agent, automation), nil
	case RiskKindAutomation:
		a, err := s.GetAutomation(ctx, actor, parseID(ref))
		if err != nil {
			return nil, err
		}
		return automationRisk(a), nil
	}
	return nil, invalid("risk is for an issue, comment, page, design, glossary, pull, prompt or automation, not %q", kind)
}

func (s *Service) pullRisk(ctx context.Context, actor *Actor, id int64) (*ProvenanceRisk, error) {
	vis, args := prVisible(actor, 0)
	var outside bool
	var author, forgeAuthor, agent string
	// #nosec G202 -- vis is a fixed condition; values are bound
	err := s.db.R.QueryRowContext(ctx, `SELECT p.outside, p.author, p.forge_author, p.agent FROM pull_requests p WHERE p.id = ? AND `+vis,
		append([]any{id}, args...)...).Scan(&outside, &author, &forgeAuthor, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("pull request")
	} else if err != nil {
		return nil, err
	}
	links, err := s.prLinks(ctx, actor, []any{id})
	if err != nil {
		return nil, err
	}
	risks, order := map[string]*ProvenanceRisk{}, []string{}
	for _, l := range links {
		if _, ok := risks[l.issue.Key]; ok {
			continue
		}
		r, err := s.issueRiskByID(ctx, s.db.R, actor, l.issue.ID)
		if err != nil {
			return nil, err
		}
		risks[l.issue.Key], order = r, append(order, l.issue.Key)
	}
	return prRisk(cmp(forgeAuthor, author), outside, agent, risks, order), nil
}
