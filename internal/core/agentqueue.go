package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Why delegated work isn't in an agent's queue right now.
const (
	WaitStep          = "step"
	WaitClaimed       = "claimed"
	WaitProjectPaused = "project_paused"
	WaitBacklog       = "backlog"
	WaitHold          = "hold"
	WaitReleased      = "released"
	WaitBlocked       = "blocked"
	WaitReview        = "review"
	WaitSafety        = "safety"
	WaitObjective     = "objective"
	WaitSubtasks      = "subtasks"
)

// QueueItem is one issue on the Agents page's queue.
type QueueItem struct {
	Issue       *Issue `json:"issue"`
	ProjectName string `json:"project_name"`
	Workflow    string `json:"workflow,omitempty"`
	StepNo      int    `json:"step_no,omitempty"`
	Steps       int    `json:"steps,omitempty"`
	// Position is its place in what next hands out (1 = next); 0 when waiting.
	Position int  `json:"position,omitempty"`
	Pinned   bool `json:"pinned,omitempty"`
	// Why says what puts it where it is: "Pinned at #2 by Robin", "Urgent · due Oct 14".
	Why string `json:"why,omitempty"`
	// Reason (waiting only) is one of the Wait* codes; Detail says it in words.
	Reason   string   `json:"reason,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	// Takers are the agents the filter covers that may take it now.
	Takers      []string   `json:"takers,omitempty"`
	DelegatedBy *UserRef   `json:"delegated_by,omitempty"`
	DelegatedAt *time.Time `json:"delegated_at,omitempty"`
	PinnedBy    *UserRef   `json:"pinned_by,omitempty"`
	// Working only: time and tokens on the current claim.
	Seconds      int64 `json:"seconds,omitempty"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// AgentQueue is what agents will take, in order, what they hold, and what
// waits and why.
type AgentQueue struct {
	// Agent is the spec asked about ('' = every agent).
	Agent string `json:"agent"`
	// Exact: the spec is one agent, so Next is exactly what next hands it.
	Exact bool `json:"exact"`
	// Covers are the agents the spec stands for (known ones, or the spec
	// itself when no known agent matches it).
	Covers         []string      `json:"covers"`
	Pauses         []*AgentPause `json:"pauses"`
	PausedProjects []string      `json:"paused_projects"`
	Working        []*QueueItem  `json:"working"`
	Next           []*QueueItem  `json:"next"`
	Waiting        []*QueueItem  `json:"waiting"`
}

// queuedIssue is an issue with the agents that may take it, or why it waits.
type queuedIssue struct {
	issue          *Issue
	takers         []string
	reason         string
	detail         string
	blockers       []string
	held           bool
	planned        bool
	unblocks       int
	blockedUrgency float64
	score          float64
}

// agentWork lists the open delegated issues that concern the agents ids
// (nil: every agent, without the per-agent rules), split into those ready
// to take, in the order next hands them out, and those waiting, with why.
// Work an agent in ids already holds is in neither.
func (s *Service) agentWork(ctx context.Context, actor *Actor, ids []string, q AgentQuery) (ready, waiting []*queuedIssue, err error) {
	auto, waiting, err := s.agentWorkAuto(ctx, actor, ids, q)
	if err != nil {
		return nil, nil, err
	}
	return mergePinned(auto), waiting, nil
}

// agentWorkAuto is agentWork before people's pins: the ready issues in the
// automatic order (autoOrder), pinned or not.
func (s *Service) agentWorkAuto(ctx context.Context, actor *Actor, ids []string, q AgentQuery) (ready, waiting []*queuedIssue, err error) {
	parts := []string{"-is:human is:open is:published"}
	if q.Delegated {
		parts[0] = "is:agent is:open is:published"
	}
	if q.Step != "" {
		parts = append(parts, fmt.Sprintf("step:%q", q.Step))
	}
	parts = append(parts, q.filters()...)
	if !strings.Contains(q.Query, "order:") && !strings.Contains(q.Query, "sort:") {
		parts = append(parts, "order:-priority,due,rank")
	}
	query := strings.Join(parts, " ")
	var all []*Issue
	for offset := 0; offset < 5000; offset += 500 {
		res, err := s.Search(ctx, actor, SearchParams{Query: query, Project: q.Project, Limit: 500, Offset: offset})
		if err != nil {
			return nil, nil, err
		}
		all = append(all, res.Issues...)
		if len(res.Issues) < 500 {
			break
		}
	}
	paused, err := s.pausedProjectIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	blockers, err := s.openBlockers(ctx, actor, all)
	if err != nil {
		return nil, nil, err
	}
	shipped, err := s.outOnPRs(ctx, s.db.R, all)
	if err != nil {
		return nil, nil, err
	}
	children, err := s.openAgentChildren(ctx, all)
	if err != nil {
		return nil, nil, err
	}
	working, err := s.workingPositions(ctx, s.db.R)
	if err != nil {
		return nil, nil, err
	}
	lapsed, err := s.lapsedHolds(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	released, err := s.releaseWaits(ctx, s.db.R)
	if err != nil {
		return nil, nil, err
	}
	rounds, err := s.objectiveWaits(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	now := s.nowMS()
	mine := func(agent string) bool {
		return slices.ContainsFunc(ids, func(id string) bool { return normalizeAgent(id) == normalizeAgent(agent) })
	}
	for _, i := range all {
		held := i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now
		if held && (ids == nil || mine(i.ClaimedBy)) {
			continue
		}
		qi := &queuedIssue{issue: i}
		if ids != nil {
			spec, rule := s.stepSpec(ctx, i)
			concerns := false
			for _, id := range ids {
				if !AgentMatches(issueAgentSpec(i.Agent), id) && !AgentMatches(spec, id) {
					continue
				}
				concerns = true
				if ok, why := s.stepAllows(ctx, i, spec, rule, id); ok {
					qi.takers = append(qi.takers, id)
				} else if qi.detail == "" {
					qi.reason, qi.detail = WaitStep, why
				}
			}
			if !concerns {
				continue
			}
			if len(qi.takers) > 0 {
				qi.reason, qi.detail = "", ""
			}
		}
		switch {
		case qi.reason != "":
		case held:
			qi.reason, qi.detail = WaitClaimed, fmt.Sprintf("%s is working on it (claimed until %s).", i.ClaimedBy, s.clock(*i.ClaimedUntil))
		case lapsed[i.ID] != "":
			qi.reason, qi.detail = WaitClaimed, upperFirst(lapsed[i.ID])
		case paused[i.ProjectID]:
			qi.reason, qi.detail = WaitProjectPaused, "Agents are paused in this project: they take no new work there."
		case i.Status.Category == CatBacklog:
			qi.reason, qi.detail = WaitBacklog, "In the backlog ("+i.Status.Name+"): agents take work once it's in To Do."
		case i.Hold.on(s.now()):
			qi.reason, qi.detail = WaitHold, "On hold "+holdSpan(i.Hold, s.actorLocation(actor))+"."
		case released[i.ID] != 0:
			qi.reason, qi.detail = WaitReleased, "An agent gave it back; it goes out again at "+s.clock(msTime(released[i.ID]))+". Only a person can end the wait, with a comment or wp hold "+i.Key+" --off; wp next "+i.Key+" takes it by name."
		case hasWait(rounds, i.ID):
			qi.reason, qi.detail = WaitObjective, rounds[i.ID].detail()
		case shipped[i.ID] != nil:
			pr := shipped[i.ID]
			if pr.State == "merged" {
				qi.reason, qi.detail = WaitReview, fmt.Sprintf("%s merged; it waits in %s for a person. Move it to To Do to hand it out again.", prName(pr), i.Status.Name)
			} else {
				qi.reason, qi.detail = WaitReview, fmt.Sprintf("%s is open; it waits on the merge, not an agent. Move it to To Do to hand it out again.", prName(pr))
			}
		case pastWorking(i, working):
			qi.reason, qi.detail = WaitReview, fmt.Sprintf("It moved on to %s; it waits there for a person. Move it to To Do to hand it out again.", i.Status.Name)
		case len(blockers[i.ID]) > 0:
			qi.reason, qi.blockers = WaitBlocked, blockers[i.ID]
			qi.detail = "Blocked by " + strings.Join(blockers[i.ID], ", ") + "."
		case children[i.ID] > 0:
			qi.reason = WaitSubtasks
			qi.detail = fmt.Sprintf("Its sub-issues come first (%d of %d open for agents); it is handed out once they are done.", children[i.ID], i.ChildCount)
		default:
			if d, held := s.safetyWait(ctx, s.db.R, i); d != "" {
				qi.reason, qi.detail, qi.held = WaitSafety, d, held
				if !held {
					s.queueScreen(i.ID)
				}
			}
		}
		switch {
		case qi.reason == "":
			ready = append(ready, qi)
		case qi.held:
			waiting = append(waiting, qi)
		case !i.Delegated() && qi.reason != WaitReleased && (!q.ScreenWaits || (qi.reason != WaitSafety && qi.reason != WaitHold)):
		default:
			waiting = append(waiting, qi)
		}
	}
	planned, err := s.plannedIssues(ctx, ready)
	if err != nil {
		return nil, nil, err
	}
	for _, qi := range ready {
		qi.planned = planned[qi.issue.ID]
	}
	if err := s.scoreQueue(ctx, actor, ready); err != nil {
		return nil, nil, err
	}
	today := s.now().UTC().Format(dateLayout)
	custom := strings.Contains(q.Query, "order:") || strings.Contains(q.Query, "sort:")
	slices.SortStableFunc(ready, func(a, b *queuedIssue) int { return autoOrder(a, b, today, custom) })
	return ready, waiting, nil
}

// autoOrder is the automatic order of the queue: the higher queueScore
// first, then board order (WP-997, WP-ADR100; WP-ADR23 before it). custom: the query
// set its own order, which follows overdue.
func autoOrder(a, b *queuedIssue, today string, custom bool) int {
	x, y := a.issue, b.issue
	if custom {
		overdue := func(i *Issue) bool { return i.DueDate != nil && *i.DueDate < today }
		return cmpFirst(overdue(x), overdue(y))
	}
	if a.score != b.score {
		if a.score > b.score {
			return -1
		}
		return 1
	}
	if x.Rank != y.Rank {
		if x.Rank < y.Rank {
			return -1
		}
		return 1
	}
	return cmpInt(x.ID, y.ID)
}

// The weights of the automatic order (WP-997, WP-ADR100). One score, not a list of
// tie-breakers, so an urgent task due tomorrow that anyone may take comes
// before a low-priority one handed to agents, and no single quality (being
// delegated, having a due date) buries everything without it.
const (
	scorePriorityStep = 10.0
	scoreUrgent       = 45.0
	scoreDueToday     = 30.0
	scoreDueHalfLife  = 3.0
	scoreOverdue      = 32.0
	scoreOverdueDay   = 2.0
	scoreOverdueMax   = 20.0
	scoreDelegated    = 8.0
	scoreBug          = 5.0
	scorePlanned      = 4.0
	scoreUnblocks     = 4.0
	scoreUnblocksMax  = 12.0
	scoreSmall        = 3.0
	scoreMedium       = 1.0
	scoreAgeWeek      = 1.0
	scoreAgeMax       = 6.0
	scoreAgentText    = -5.0
)

// urgency is what priority and due date say about a task: the part a
// task that blocks it passes on to its blocker.
func urgency(priority int, due *string, today time.Time) float64 {
	var u float64
	switch {
	case priority >= 4:
		u = scoreUrgent
	case priority > 0:
		u = scorePriorityStep * float64(priority)
	}
	if due == nil {
		return u
	}
	d, err := time.Parse(dateLayout, *due)
	if err != nil {
		return u
	}
	days := d.Sub(today).Hours() / 24
	if days < 0 {
		return u + scoreOverdue + min(-days*scoreOverdueDay, scoreOverdueMax)
	}
	return u + scoreDueToday*math.Pow(0.5, days/scoreDueHalfLife)
}

// queueScore is how much a ready task should come first (WP-997): its
// urgency, or that of the most urgent open task it blocks if higher,
// then whether a person delegated it, it is a bug, it is planned, it
// unblocks others, it is small, it has waited long, and who wrote it.
func queueScore(qi *queuedIssue, today time.Time, now time.Time) float64 {
	i := qi.issue
	s := max(urgency(i.Priority, i.DueDate, today), qi.blockedUrgency)
	if i.Delegated() {
		s += scoreDelegated
	}
	if isBug(i) {
		s += scoreBug
	}
	if qi.planned {
		s += scorePlanned
	}
	s += min(float64(qi.unblocks)*scoreUnblocks, scoreUnblocksMax)
	if i.Points != nil {
		switch {
		case *i.Points <= 2:
			s += scoreSmall
		case *i.Points <= 5:
			s += scoreMedium
		}
	}
	if !i.CreatedAt.IsZero() && now.After(i.CreatedAt) {
		s += min(now.Sub(i.CreatedAt).Hours()/(24*7)*scoreAgeWeek, scoreAgeMax)
	}
	if i.TextAgent != "" && i.Unreviewed {
		s += scoreAgentText
	}
	return s
}

// scoreQueue sets each ready task's score: what the open tasks it blocks
// pass on (WP-997), then queueScore.
func (s *Service) scoreQueue(ctx context.Context, actor *Actor, ready []*queuedIssue) error {
	now := s.now()
	today, _ := time.Parse(dateLayout, now.UTC().Format(dateLayout))
	by := make(map[int64]*queuedIssue, len(ready))
	ids := make([]int64, 0, len(ready))
	for _, qi := range ready {
		by[qi.issue.ID] = qi
		ids = append(ids, qi.issue.ID)
	}
	vis, vargs := visibleSQL("t.project_id", actor)
	for chunk := range slices.Chunk(ids, 500) {
		args := append(slices.Clone(vargs), int64Args(chunk)...)
		// #nosec G202 -- vis is a fixed condition and placeholders only; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT r.source_id, t.priority, t.due_date
			FROM issue_relations r JOIN issues t ON t.id = r.target_id JOIN statuses ts ON ts.id = t.status_id
			WHERE r.kind = 'blocks' AND ts.category NOT IN ('done','canceled') AND `+vis+` AND r.source_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var prio int
			var due sql.NullString
			if err := rows.Scan(&id, &prio, &due); err != nil {
				rows.Close()
				return err
			}
			qi := by[id]
			qi.unblocks++
			var d *string
			if due.Valid && due.String != "" {
				d = &due.String
			}
			qi.blockedUrgency = max(qi.blockedUrgency, urgency(prio, d, today))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, qi := range ready {
		qi.score = queueScore(qi, today, now)
	}
	return nil
}

// cmpFirst orders what has a quality before what hasn't.
func cmpFirst(a, b bool) int {
	switch {
	case a && !b:
		return -1
	case b && !a:
		return 1
	}
	return 0
}

// isBug: a bug by its type or its category.
func isBug(i *Issue) bool { return i.Type == "bug" || i.Category == "bugfix" }

// plannedIssues are those of ready that have a plan: a plan page that
// links them, or a task prep ready for the agent (WP-ADR8).
func (s *Service) plannedIssues(ctx context.Context, ready []*queuedIssue) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ready) == 0 {
		return out, nil
	}
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT pi.issue_id FROM page_issues pi JOIN pages p ON p.id = pi.page_id WHERE p.category = 'plan' AND p.draft = 0
		UNION SELECT issue_id FROM task_preps WHERE state = 'ready'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// pin is where a person put a task in the queue (WP-311, WP-ADR23): after ahead
// automatically ordered tasks (0 = before all of them), and among pins
// with the same ahead, by rank. Counting only the automatic tasks ahead
// keeps a pin where it was put when the pinned tasks above it finish.
type pin struct {
	ahead int
	rank  float64
}

func pinOf(i *Issue) (pin, bool) {
	if i.AgentOrder == nil {
		return pin{}, false
	}
	return pin{ahead: max(i.AgentAhead, 0), rank: *i.AgentOrder}, true
}

// mergePinned puts each pinned issue at the place a person gave it, and
// the rest around them in their automatic order.
func mergePinned(auto []*queuedIssue) []*queuedIssue {
	ids := make([]int64, len(auto))
	pins := map[int64]pin{}
	by := make(map[int64]*queuedIssue, len(auto))
	for n, qi := range auto {
		ids[n] = qi.issue.ID
		by[qi.issue.ID] = qi
		if p, ok := pinOf(qi.issue); ok {
			pins[qi.issue.ID] = p
		}
	}
	if len(pins) == 0 {
		return auto
	}
	out := make([]*queuedIssue, 0, len(auto))
	for _, id := range mergePins(ids, pins) {
		out = append(out, by[id])
	}
	return out
}

// mergePins is auto (ids in the automatic order, pinned ones included)
// with each pinned id after its number of unpinned ids; pins with the same
// number go by rank, then id. A number past the end is the end.
func mergePins(auto []int64, pins map[int64]pin) []int64 {
	if len(pins) == 0 {
		return auto
	}
	rest := make([]int64, 0, len(auto))
	held := make([]int64, 0, len(pins))
	for _, id := range auto {
		if _, ok := pins[id]; ok {
			held = append(held, id)
		} else {
			rest = append(rest, id)
		}
	}
	slices.SortFunc(held, func(a, b int64) int {
		pa, pb := pins[a], pins[b]
		if pa.ahead != pb.ahead {
			return cmpInt(int64(pa.ahead), int64(pb.ahead))
		}
		if pa.rank != pb.rank {
			if pa.rank < pb.rank {
				return -1
			}
			return 1
		}
		return cmpInt(a, b)
	})
	out := make([]int64, 0, len(auto))
	j := 0
	for n, id := range rest {
		for ; j < len(held) && pins[held[j]].ahead <= n; j++ {
			out = append(out, held[j])
		}
		out = append(out, id)
	}
	return append(out, held[j:]...)
}

// placePin is where the pins sit after x is dropped at place pos (1 = first;
// 0: where it is now) in the queue as it is (pins: those of auto, by id).
// Every pin keeps its place in the order shown: each counts the unpinned
// tasks ahead of it there, and the pins rank in that order. Dropped where
// the automatic order would put it, a pinned x is let go: the result then
// leaves it out.
func placePin(auto []int64, pins map[int64]pin, x int64, pos int) map[int64]pin {
	cur := mergePins(auto, pins)
	_, wasPinned := pins[x]
	rest := slices.DeleteFunc(slices.Clone(cur), func(id int64) bool { return id == x })
	if pos <= 0 {
		if pos = slices.Index(cur, x) + 1; pos == 0 {
			pos = len(rest) + 1
		}
	}
	pos = min(max(pos, 1), len(rest)+1)
	result := slices.Insert(rest, pos-1, x)
	out := map[int64]pin{}
	ahead := 0
	for n, id := range result {
		if _, ok := pins[id]; ok || id == x {
			out[id] = pin{ahead: ahead, rank: float64(n + 1)}
		} else {
			ahead++
		}
	}
	if wasPinned {
		others := maps.Clone(out)
		delete(others, x)
		if slices.Index(mergePins(auto, others), x)+1 == pos {
			delete(out, x)
		}
	}
	return out
}

// pinRanks gives the pins of one view (in its new order, x among them
// unless it was let go) agent_order values that keep that order in every
// view (WP-ADR35): a rank is compared with pins from other projects too, so
// the ranks of one view's pins can't simply count 1, 2, 3 there. The pins
// other than x swap the ranks they already have (slots), which leaves
// each other view's pins exactly where they were among them; x takes a
// rank just after the pin before it in the view, or just before the one
// after it, or, alone, before every pin (all: every pin's rank but x's).
// ok is false when there is no room between two ranks: renumber and ask
// again.
func pinRanks(view []int64, x int64, slots, all []float64) (ranks map[int64]float64, ok bool) {
	slots = slices.Sorted(slices.Values(slots))
	ranks = map[int64]float64{}
	k := 0
	for _, id := range view {
		if id != x {
			ranks[id] = slots[k]
			k++
		}
	}
	at := slices.Index(view, x)
	if at < 0 {
		return ranks, true
	}
	all = slices.Sorted(slices.Values(all))
	var lo, hi float64
	switch {
	case at > 0:
		lo = ranks[view[at-1]]
		hi = lo + 2048
		if n, _ := slices.BinarySearch(all, math.Nextafter(lo, math.Inf(1))); n < len(all) {
			hi = all[n]
		}
	case len(view) > 1:
		hi = ranks[view[1]]
		lo = hi - 2048
		if n, _ := slices.BinarySearch(all, hi); n > 0 {
			lo = all[n-1]
		}
	case len(all) > 0:
		hi = all[0]
		lo = hi - 2048
	default:
		lo, hi = 0, 2048
	}
	if hi-lo < 2*rankRoom {
		return nil, false
	}
	ranks[x] = (lo + hi) / 2
	return ranks, true
}

// stepAllows applies a workflow step's same/different rule to an agent
// whose spec fits; why says what stops it.
func (s *Service) stepAllows(ctx context.Context, i *Issue, spec, rule, id string) (bool, string) {
	if !AgentMatches(spec, id) {
		return false, fmt.Sprintf("At step %s, which is for %s.", i.Step, spec)
	}
	if rule == "" {
		return true, ""
	}
	var prev string
	err := s.db.R.QueryRowContext(ctx, `SELECT agent FROM agent_runs WHERE issue_id = ? AND outcome = 'done' ORDER BY id DESC LIMIT 1`, i.ID).Scan(&prev)
	if err != nil {
		return true, ""
	}
	same := normalizeAgent(prev) == normalizeAgent(id)
	switch {
	case rule == "different" && same:
		return false, fmt.Sprintf("At step %s, which needs a different agent than %s, who did the step before.", i.Step, prev)
	case rule == "same" && !same:
		return false, fmt.Sprintf("At step %s, which is for %s, who did the step before.", i.Step, prev)
	}
	return true, ""
}

// pausedProjectIDs are the projects whose owners stopped agents taking new
// work there.
func (s *Service) pausedProjectIDs(ctx context.Context) (map[int64]bool, error) {
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM projects WHERE json_extract(settings, '$.agents_paused') = 1`)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// openAgentChildren counts, per issue, its open sub-issues handed to
// agents (not Anyone's or Humans only: a template's checklist sub-tasks are
// the parent's own steps, done by whoever takes it). Drafts wait for a
// person, so they never hold a parent, whoever looks at the queue.
func (s *Service) openAgentChildren(ctx context.Context, issues []*Issue) (map[int64]int, error) {
	out := map[int64]int{}
	for chunk := range slices.Chunk(issues, 500) {
		marks := make([]string, 0, len(chunk))
		args := []any{AgentAnyone, AgentHumans}
		for _, i := range chunk {
			if i.ChildCount > i.ChildDone {
				marks = append(marks, "?")
				args = append(args, i.ID)
			}
		}
		if len(marks) == 0 {
			continue
		}
		// #nosec G202 -- the condition is fixed and marks are placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT c.parent_id, count(*) FROM issues c JOIN statuses cs ON cs.id = c.status_id
			WHERE cs.category NOT IN ('done','canceled') AND c.agent NOT IN (?, ?) AND c.draft = 0
			AND c.parent_id IN (`+strings.Join(marks, ",")+`) GROUP BY c.parent_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// openBlockers are the keys of the open issues blocking each issue; one the
// actor can't see is counted, not named.
func (s *Service) openBlockers(ctx context.Context, actor *Actor, issues []*Issue) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(issues) == 0 {
		return out, nil
	}
	vis, vargs := visibleSQL("b.project_id", actor)
	for chunk := range slices.Chunk(issues, 500) {
		args := slices.Clone(vargs)
		marks := make([]string, len(chunk))
		for n, i := range chunk {
			marks[n] = "?"
			args = append(args, i.ID)
		}
		// #nosec G202 -- vis is a fixed condition and marks are placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT r.target_id, bp.key || '-' || b.number, `+vis+`
			FROM issue_relations r JOIN issues b ON b.id = r.source_id JOIN projects bp ON bp.id = b.project_id
			JOIN statuses bs ON bs.id = b.status_id
			WHERE r.kind = 'blocks' AND bs.category NOT IN ('done','canceled') AND r.target_id IN (`+strings.Join(marks, ",")+`)
			ORDER BY b.id`, args...)
		if err != nil {
			return nil, err
		}
		hidden := map[int64]int{}
		for rows.Next() {
			var id int64
			var key string
			var visible bool
			if err := rows.Scan(&id, &key, &visible); err != nil {
				rows.Close()
				return nil, err
			}
			if visible {
				out[id] = append(out[id], key)
			} else {
				hidden[id]++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for id, n := range hidden {
			out[id] = append(out[id], oneOrMany(n, "an issue you can't see", fmt.Sprintf("%d issues you can't see", n)))
		}
	}
	return out, nil
}

// clock is a time of day in the server's zone, for one-line messages.
func (s *Service) clock(t time.Time) string { return t.In(time.Local).Format("15:04 MST") }

// WhyBoardOrder is why a task with nothing else to say for it comes next.
const WhyBoardOrder = "No priority, due date or estimate: board order"

// whyQueued says what puts a ready issue where it is, in the terms of
// autoOrder. pos is its place in the queue when a person pinned it there,
// else 0; planned: it has a plan page or a task prep; unblocks: the open
// tasks waiting on it.
func whyQueued(i *Issue, pos int, pinnedBy *UserRef, today string, planned bool, unblocks int) string {
	if pos > 0 {
		s := fmt.Sprintf("Pinned at #%d", pos)
		if pinnedBy != nil {
			s += " by " + refName(pinnedBy)
		}
		return s
	}
	var parts []string
	if i.DueDate != nil && *i.DueDate < today {
		parts = append(parts, "Overdue (due "+shortDate(*i.DueDate)+")")
	}
	switch i.Priority {
	case 4:
		parts = append(parts, "Urgent")
	case 3, 2, 1:
		parts = append(parts, capitalize(PriorityName(i.Priority))+" priority")
	}
	if i.DueDate != nil && *i.DueDate >= today {
		parts = append(parts, "due "+shortDate(*i.DueDate))
	}
	if isBug(i) {
		parts = append(parts, "bug")
	}
	if i.Points != nil {
		parts = append(parts, strconv.FormatFloat(*i.Points, 'f', -1, 64)+" pts")
	}
	if planned {
		parts = append(parts, "planned")
	}
	if unblocks > 0 {
		parts = append(parts, "unblocks "+oneOrMany(unblocks, "a task", strconv.Itoa(unblocks)+" tasks"))
	}
	if len(parts) == 0 {
		return WhyBoardOrder
	}
	parts[0] = capitalize(parts[0])
	return strings.Join(parts, " · ")
}

func shortDate(d string) string {
	t, err := time.Parse(dateLayout, d)
	if err != nil {
		return d
	}
	return t.Format("Jan 2")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return upperFirst(s)
}

// refName is a person's display name, falling back to the username.
func refName(r *UserRef) string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return r.Username
}

// oneOrMany picks the wording for a count.
func oneOrMany(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// queueAgents are the agents a spec stands for: the known ones it matches,
// or the spec itself when none does (what such an agent would get). nil for
// every agent.
func (s *Service) queueAgents(ctx context.Context, actor *Actor, spec string) ([]string, error) {
	if spec == "" || strings.EqualFold(spec, "any") {
		return nil, nil
	}
	known, err := s.KnownAgents(ctx, actor)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range known {
		if normalizeAgent(a.Identity) == normalizeAgent(spec) {
			return []string{a.Identity}, nil
		}
		if AgentMatches(spec, a.Identity) {
			ids = append(ids, a.Identity)
		}
	}
	if len(ids) == 0 {
		ids = []string{spec}
	}
	return ids, nil
}

// AgentQueueFor is the queue for an agent spec (” = every agent) in a
// project (” = every project the actor can see).
func (s *Service) AgentQueueFor(ctx context.Context, actor *Actor, spec, project string) (*AgentQueue, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	spec = strings.TrimSpace(spec)
	if len(spec) > 100 {
		return nil, invalid("agent can be at most 100 characters")
	}
	out := &AgentQueue{Agent: spec, Covers: []string{}, Working: []*QueueItem{}, Next: []*QueueItem{}, Waiting: []*QueueItem{}, PausedProjects: []string{}}
	ids, err := s.queueAgents(ctx, actor, spec)
	if err != nil {
		return nil, err
	}
	if ids != nil {
		out.Covers, out.Exact = ids, len(ids) == 1
	}
	q := AgentQuery{Project: project}
	ready, waiting, err := s.agentWork(ctx, actor, ids, q)
	if err != nil {
		return nil, err
	}
	pauses, err := s.ListAgentPauses(ctx, actor)
	if err != nil {
		return nil, err
	}
	out.Pauses = []*AgentPause{}
	for _, p := range pauses {
		if ids == nil || slices.ContainsFunc(ids, func(id string) bool { return AgentMatches(p.Spec, id) }) {
			out.Pauses = append(out.Pauses, p)
		}
	}
	working, err := s.queueWorking(ctx, actor, ids, project)
	if err != nil {
		return nil, err
	}
	out.Working = working

	var issueIDs []int64
	for _, list := range [][]*queuedIssue{ready, waiting} {
		for _, qi := range list {
			issueIDs = append(issueIDs, qi.issue.ID)
		}
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return nil, err
	}
	delegated, err := s.delegations(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	pinners, err := s.pinners(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	today := s.now().UTC().Format(dateLayout)
	item := func(qi *queuedIssue) *QueueItem {
		i := qi.issue
		it := &QueueItem{Issue: i, ProjectName: names[i.ProjectID], Pinned: i.AgentOrder != nil, Takers: qi.takers,
			Reason: qi.reason, Detail: qi.detail, Blockers: qi.blockers}
		if d, ok := delegated[i.ID]; ok {
			it.DelegatedBy, it.DelegatedAt = d.by, &d.at
		} else if i.reporterID != nil {
			it.DelegatedBy, it.DelegatedAt = s.userRefPtr(ctx, s.db.R, i.reporterID), &i.CreatedAt
		}
		if it.Pinned {
			it.PinnedBy = pinners[i.ID]
		}
		s.fillWorkflow(ctx, it)
		return it
	}
	for n, qi := range ready {
		it := item(qi)
		it.Position = n + 1
		if it.Pinned {
			it.Why = whyQueued(qi.issue, it.Position, it.PinnedBy, today, qi.planned, qi.unblocks)
		} else {
			it.Why = whyQueued(qi.issue, 0, nil, today, qi.planned, qi.unblocks)
		}
		out.Next = append(out.Next, it)
	}
	for _, qi := range waiting {
		out.Waiting = append(out.Waiting, item(qi))
	}
	paused, err := s.pausedProjectIDs(ctx)
	if err != nil {
		return nil, err
	}
	for id := range paused {
		if p, err := s.projectByRef(ctx, s.db.R, itoa64(id)); err == nil && s.requireProjectRole(ctx, s.db.R, actor, id, RoleViewer) == nil {
			if project == "" || strings.EqualFold(project, p.Key) {
				out.PausedProjects = append(out.PausedProjects, p.Key)
			}
		}
	}
	slices.Sort(out.PausedProjects)
	return out, nil
}

func (s *Service) fillWorkflow(ctx context.Context, it *QueueItem) {
	i := it.Issue
	if i.WorkflowID == nil {
		return
	}
	w, err := s.workflowByID(ctx, s.db.R, *i.WorkflowID)
	if err != nil {
		return
	}
	it.Workflow, it.Steps = w.Name, len(w.Steps)
	if n := w.stepIndex(i.Step); n >= 0 {
		it.StepNo = n + 1
	}
}

// queueWorking lists claimed tasks: for agents ids (nil: any agent).
func (s *Service) queueWorking(ctx context.Context, actor *Actor, ids []string, project string) ([]*QueueItem, error) {
	res, err := s.Search(ctx, actor, SearchParams{Query: "is:claimed order:-updated", Project: project, Limit: 200})
	if err != nil {
		return nil, err
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return nil, err
	}
	out := []*QueueItem{}
	now := s.now()
	for _, i := range res.Issues {
		if ids != nil && !slices.ContainsFunc(ids, func(id string) bool { return normalizeAgent(id) == normalizeAgent(i.ClaimedBy) }) {
			continue
		}
		it := &QueueItem{Issue: i, ProjectName: names[i.ProjectID], Seconds: i.ClaimWorked, Pinned: i.AgentOrder != nil}
		if i.ClaimWorkingSince != nil {
			it.Seconds += int64(now.Sub(*i.ClaimWorkingSince).Seconds())
		}
		var since int64
		if i.ClaimedAt != nil {
			since = i.ClaimedAt.UnixMilli()
		}
		if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0) FROM agent_runs
			WHERE issue_id = ? AND agent = ? AND started_at >= ?`, i.ID, i.ClaimedBy, since).Scan(&it.InputTokens, &it.OutputTokens); err != nil {
			return nil, err
		}
		s.fillWorkflow(ctx, it)
		out = append(out, it)
	}
	return out, nil
}

func (s *Service) projectNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, name FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

type delegation struct {
	by *UserRef
	at time.Time
}

// delegations are who last set each issue's agent spec, from history.
func (s *Service) delegations(ctx context.Context, ids []int64) (map[int64]delegation, error) {
	out := map[int64]delegation{}
	cache := map[int64]*UserRef{}
	for chunk := range slices.Chunk(ids, 500) {
		marks, args := placeholders(len(chunk)), int64Args(chunk)
		// #nosec G202 -- marks are placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, actor_id, created_at FROM events
			WHERE kind = 'issue.updated' AND field = 'agent' AND new_value != '' AND actor_id IS NOT NULL AND issue_id IN (`+marks+`)
			ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		type row struct {
			issue int64
			actor sql.NullInt64
			at    int64
		}
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.issue, &r.actor, &r.at); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, r := range list {
			out[r.issue] = delegation{by: s.cachedUser(ctx, cache, r.actor), at: msTime(r.at)}
		}
	}
	return out, nil
}

// pinners are who last placed each pinned issue in the queue.
func (s *Service) pinners(ctx context.Context, ids []int64) (map[int64]*UserRef, error) {
	out := map[int64]*UserRef{}
	cache := map[int64]*UserRef{}
	for chunk := range slices.Chunk(ids, 500) {
		marks, args := placeholders(len(chunk)), int64Args(chunk)
		// #nosec G202 -- marks are placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT target_id, user_id FROM audit_log
			WHERE action IN ('queue.pin', 'queue.move') AND target_type = 'issue' AND target_id IN (`+marks+`) ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		type row struct {
			issue int64
			user  sql.NullInt64
		}
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.issue, &r.user); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, r := range list {
			out[r.issue] = s.cachedUser(ctx, cache, r.user)
		}
	}
	return out, nil
}

// QueueMove places an issue in the queue. A pin holds the place a person
// gave a task (1 = next); every other task keeps following the automatic
// order around the pins, so placing one task moves only that one (and the
// pins it passes). Agent and Project are the queue as the person sees it,
// which is what Position counts in.
type QueueMove struct {
	Key string `json:"key"`
	// Position is the place to hold, 1 = first; none (and not Unpin or Top):
	// where the task is now, pinned there.
	Position int `json:"position,omitempty"`
	// Top is Position 1.
	Top bool `json:"top,omitempty"`
	// Unpin sets the task back to the automatic order. Placing a pinned task
	// where the automatic order would put it does the same.
	Unpin   bool   `json:"unpin,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Project string `json:"project,omitempty"`
}

// MoveInQueue changes where an issue sits in the agent queue.
func (s *Service) MoveInQueue(ctx context.Context, actor *Actor, m QueueMove) error {
	if err := actor.RequireHuman("change the order agents take work in"); err != nil {
		return err
	}
	if m.Top {
		m.Position = 1
	}
	if m.Position < 0 {
		return invalid("position must be 1 or more")
	}
	ids, err := s.queueAgents(ctx, actor, strings.TrimSpace(m.Agent))
	if err != nil {
		return err
	}
	auto, _, err := s.agentWorkAuto(ctx, actor, ids, AgentQuery{Project: m.Project})
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, m.Key, RoleEditor, false)
		if err != nil {
			return err
		}
		if m.Unpin {
			return s.unpin(ctx, t, i, "back to the automatic order")
		}
		if agentHumansOnly(i.Agent) {
			return invalid("%s is for humans only; let agents take it first", i.Key)
		}
		if i.Closed() {
			return invalid("%s is already %s", i.Key, i.Status.Name)
		}
		order := make([]int64, 0, len(auto))
		pins := map[int64]pin{}
		issues := map[int64]*Issue{}
		for _, qi := range auto {
			order = append(order, qi.issue.ID)
			issues[qi.issue.ID] = qi.issue
			if p, ok := pinOf(qi.issue); ok {
				pins[qi.issue.ID] = p
			}
		}
		if _, ok := issues[i.ID]; !ok {
			order = append(order, i.ID)
			issues[i.ID] = i
			if p, ok := pinOf(i); ok {
				pins[i.ID] = p
			}
		}
		was, wasPinned := pins[i.ID]
		placed := placePin(order, pins, i.ID, m.Position)
		shown := slices.SortedFunc(maps.Keys(placed), func(a, b int64) int {
			return cmpInt(int64(placed[a].rank), int64(placed[b].rank))
		})
		view := make([]int64, 0, len(shown))
		var slots []float64
		var others []*Issue
		for _, id := range shown {
			if id == i.ID {
				view = append(view, id)
				continue
			}
			o, _, err := s.getIssueFor(ctx, t, actor, issues[id].Key, RoleEditor, false)
			if err != nil {
				if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			view = append(view, id)
			slots = append(slots, pins[id].rank)
			others = append(others, o)
		}
		ranks, err := s.pinRanksIn(ctx, t, view, i.ID, slots)
		if err != nil {
			return err
		}
		for _, o := range others {
			p := pin{ahead: placed[o.ID].ahead, rank: ranks[o.ID]}
			if pins[o.ID] == p {
				continue
			}
			if _, err := t.ExecContext(ctx, `UPDATE issues SET agent_order = ?, agent_ahead = ? WHERE id = ?`, p.rank, p.ahead, o.ID); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvProjectUpdated, ProjectID: o.ProjectID, IssueID: o.ID, IssueKey: o.Key})
		}
		p, pinned := placed[i.ID]
		p.rank = ranks[i.ID]
		switch {
		case !pinned:
			return s.unpin(ctx, t, i, "back where the automatic order puts it")
		case wasPinned && was == p:
			return nil
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET agent_order = ?, agent_ahead = ? WHERE id = ?`, p.rank, p.ahead, i.ID); err != nil {
			return err
		}
		slot := slices.Index(mergePins(order, placed), i.ID) + 1
		autoAt := slices.Index(order, i.ID) + 1
		action := "queue.move"
		if !wasPinned {
			action = "queue.pin"
		}
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
		t.audit(AuditEntry{Action: action, Category: AuditControl, ProjectID: i.ProjectID, TargetType: "issue",
			TargetID: i.ID, Target: i.Key, About: i.Agent, Detail: fmt.Sprintf("held at #%d; automatic order #%d of %d", slot, autoAt, len(order))})
		return nil
	})
}

// pinRanksIn is pinRanks with the ranks of every pin, spreading them out
// first, keeping their order, when two have no room left between them.
func (s *Service) pinRanksIn(ctx context.Context, t *tx, view []int64, x int64, slots []float64) (map[int64]float64, error) {
	for try := 0; ; try++ {
		var all []float64
		rows, err := t.QueryContext(ctx, `SELECT agent_order FROM issues WHERE agent_order IS NOT NULL AND id != ?`, x)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r float64
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if ranks, ok := pinRanks(view, x, slots, all); ok || try > 0 {
			if !ok {
				return nil, fmt.Errorf("no room between queue pins")
			}
			return ranks, nil
		}
		if err := renumberPins(ctx, t); err != nil {
			return nil, err
		}
		slots = slots[:0]
		for _, id := range view {
			if id == x {
				continue
			}
			var r float64
			if err := t.QueryRowContext(ctx, `SELECT agent_order FROM issues WHERE id = ?`, id).Scan(&r); err != nil {
				return nil, err
			}
			slots = append(slots, r)
		}
	}
}

// renumberPins spreads the ranks of every pin 1024 apart in their order.
func renumberPins(ctx context.Context, t *tx) error {
	_, err := t.ExecContext(ctx, `UPDATE issues SET agent_order = (SELECT n * 1024.0 FROM (
		SELECT id, row_number() OVER (ORDER BY agent_order, id) AS n FROM issues WHERE agent_order IS NOT NULL) r
		WHERE r.id = issues.id) WHERE agent_order IS NOT NULL`)
	return err
}

// unpin sets i back to the automatic order.
func (s *Service) unpin(ctx context.Context, t *tx, i *Issue, detail string) error {
	if i.AgentOrder == nil {
		return nil
	}
	if _, err := t.ExecContext(ctx, `UPDATE issues SET agent_order = NULL, agent_ahead = 0 WHERE id = ?`, i.ID); err != nil {
		return err
	}
	t.emit(&Event{Kind: EvProjectUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key})
	t.audit(AuditEntry{Action: "queue.unpin", Category: AuditControl, ProjectID: i.ProjectID, TargetType: "issue",
		TargetID: i.ID, Target: i.Key, About: i.Agent, Detail: detail})
	return nil
}

// ReleaseClaim lets a person take a task back from the agent holding it:
// its run ends as released and the task waits in the queue again.
func (s *Service) ReleaseClaim(ctx context.Context, actor *Actor, ref string) error {
	if err := actor.RequireHuman("take a task back from an agent"); err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		if i.ClaimedBy == "" || i.ClaimedUntil == nil || i.ClaimedUntil.UnixMilli() <= s.nowMS() {
			return invalid("no agent is working on %s", i.Key)
		}
		agent := i.ClaimedBy
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE time_entries SET ended_at = ? WHERE agent = ? AND issue_id = ? AND ended_at IS NULL`, now, agent, i.ID); err != nil {
			return err
		}
		if err := s.endRun(ctx, t, i.ID, agent, "released", nil); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ?`, i.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "claim", Old: agent})
		t.audit(AuditEntry{Action: "claim.release", Category: AuditControl, ProjectID: i.ProjectID, TargetType: "issue",
			TargetID: i.ID, Target: i.Key, About: agent, Detail: "took it back from " + agent})
		return nil
	})
}

// AgentPause stops a person's agents matching Spec taking new work.
type AgentPause struct {
	ID   int64  `json:"id"`
	Spec string `json:"spec"`
	Note string `json:"note,omitempty"`
	// Sessions: running sessions wind down too, not only the queue (WP-146).
	Sessions  bool      `json:"sessions"`
	By        *UserRef  `json:"by"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrAgentsPaused is next's answer while the agent's person paused it.
type ErrAgentsPaused struct {
	Pause *AgentPause
	Agent string
}

func (e *ErrAgentsPaused) Error() string {
	who := "your person"
	if e.Pause.By != nil {
		who = refName(e.Pause.By)
	}
	what := "every agent"
	if !strings.EqualFold(e.Pause.Spec, "any") {
		what = "agents matching " + e.Pause.Spec
	}
	msg := fmt.Sprintf("Paused: %s stopped %s (you are %s) taking new work, %s", who, what, e.Agent, e.Pause.CreatedAt.In(time.Local).Format("Jan 2 15:04 MST"))
	if e.Pause.Note != "" {
		msg += ": " + e.Pause.Note
	}
	return msg + "."
}

// ListAgentPauses lists the actor's pauses.
func (s *Service) ListAgentPauses(ctx context.Context, actor *Actor) ([]*AgentPause, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	return s.pausesOf(ctx, actor.ID())
}

func (s *Service) pausesOf(ctx context.Context, userID int64) ([]*AgentPause, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, spec, note, sessions, created_at FROM agent_pauses WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentPause{}
	for rows.Next() {
		var p AgentPause
		var at int64
		if err := rows.Scan(&p.ID, &p.Spec, &p.Note, &p.Sessions, &at); err != nil {
			return nil, err
		}
		p.CreatedAt = msTime(at)
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		by := s.userRefPtr(ctx, s.db.R, &userID)
		for _, p := range out {
			p.By = by
		}
	}
	return out, nil
}

// pauseFor is the actor's pause covering agent identity id, if any.
func (s *Service) pauseFor(ctx context.Context, actor *Actor, id string) (*AgentPause, error) {
	if actor == nil || actor.User == nil {
		return nil, nil
	}
	pauses, err := s.pausesOf(ctx, actor.ID())
	if err != nil {
		return nil, err
	}
	for _, p := range pauses {
		if AgentMatches(p.Spec, id) {
			return p, nil
		}
	}
	return nil, nil
}

// PauseAgents stops the actor's agents matching spec ("any" or "": all of
// them) taking new work; what they hold stays theirs. With sessions, their
// running sessions also start no agents, finish their step and stop
// (WP-146); pausing again without it leaves that on.
func (s *Service) PauseAgents(ctx context.Context, actor *Actor, spec, note string, sessions bool) (*AgentPause, error) {
	if err := actor.RequireHuman("pause agents"); err != nil {
		return nil, err
	}
	spec, err := checkAgentSpec(spec)
	if err != nil {
		return nil, err
	}
	if spec == "" {
		spec = "any"
	}
	note = strings.Join(strings.Fields(note), " ")
	if len(note) > 300 {
		return nil, invalid("the note can be at most 300 characters")
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO agent_pauses(user_id, spec, note, sessions, created_at) VALUES (?,?,?,?,?)
			ON CONFLICT(user_id, spec) DO UPDATE SET note = excluded.note, sessions = max(sessions, excluded.sessions)`, actor.ID(), spec, note, sessions, s.nowMS()); err != nil {
			return err
		}
		detail := "agents matching " + spec
		if spec == "any" {
			detail = "all their agents"
		}
		if sessions {
			detail += ", running sessions included"
		}
		if note != "" {
			detail += ": " + note
		}
		t.audit(AuditEntry{Action: "agent.pause", Category: AuditControl, TargetType: "agent", Target: spec, About: spec, Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	pauses, err := s.pausesOf(ctx, actor.ID())
	if err != nil {
		return nil, err
	}
	for _, p := range pauses {
		if strings.EqualFold(p.Spec, spec) {
			return p, nil
		}
	}
	return nil, notFound("pause")
}

// ResumeAgents ends the actor's pause for spec ("": every pause).
func (s *Service) ResumeAgents(ctx context.Context, actor *Actor, spec string) (int, error) {
	if err := actor.RequireHuman("resume agents"); err != nil {
		return 0, err
	}
	spec = strings.TrimSpace(spec)
	var n int64
	err := s.write(ctx, actor, func(t *tx) error {
		q, args := `DELETE FROM agent_pauses WHERE user_id = ?`, []any{actor.ID()}
		if spec != "" {
			q, args = q+` AND spec = ? COLLATE NOCASE`, append(args, spec)
		}
		res, err := t.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ = res.RowsAffected(); n == 0 {
			if spec == "" {
				return nil
			}
			return notFound("a pause for " + spec)
		}
		target := spec
		if target == "" {
			target = "every pause"
		}
		t.audit(AuditEntry{Action: "agent.resume", Category: AuditControl, TargetType: "agent", Target: target, About: spec, Detail: "agents take new work again"})
		t.emit(&Event{Kind: EvAgentsResumed, Data: map[string]any{"user": actor.ID()}})
		return nil
	})
	return int(n), err
}
