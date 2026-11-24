package core

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Grounding is computed by code alone, never a model: an approved goal first, then the weakest edge (person over agent over guess), then fewest hops.

// Groundings, best first.
const (
	GroundPersonGoal    = "person_goal"
	GroundAgentGoal     = "agent_goal"
	GroundInferredGoal  = "inferred_goal"
	GroundProposedGoal  = "proposed_goal"
	GroundPersonRequest = "person_request"
	GroundNone          = "none"
)

// Groundings lists them best first.
var Groundings = []string{GroundPersonGoal, GroundAgentGoal, GroundInferredGoal, GroundProposedGoal, GroundPersonRequest, GroundNone}

// groundingWords says a grounding in a sentence.
var groundingWords = map[string]string{
	GroundPersonGoal:    "traces to a goal a person approved, through links people made",
	GroundAgentGoal:     "traces to a goal a person approved, through a link an agent made",
	GroundInferredGoal:  "traces to a goal only through a link Waypoint guessed",
	GroundProposedGoal:  "traces only to a goal no person has approved yet",
	GroundPersonRequest: "traces to no goal, but a person asked for it",
	GroundNone:          "traces to no goal, and no person wrote or approved anything it hangs from",
}

// GroundingWords is a grounding in a sentence ("traces to a goal …").
func GroundingWords(g string) string { return groundingWords[g] }

// Bases of an edge, weakest first.
const (
	basisInferred int8 = iota
	basisAgent
	basisPerson
)

var basisNames = []string{"inferred", "agent", "person"}

// Link basis values kept in knowledge_links.basis, set when the link is
// made: empty for a person acting directly, or one of these. A value this code
// doesn't know counts as an agent's.
const (
	linkBasisAgent      = "agent"
	linkBasisInferred   = "inferred"
	linkBasisAutomation = "automation"
	linkBasisSuggested  = "suggested"
)

// isGuess: the link is Waypoint's guess (from words, or from the goal a
// model suggested), not anyone's act.
func isGuess(basis string) bool { return basis == linkBasisInferred || basis == linkBasisSuggested }

// storedBasis is who stands behind a link, from what was stored when it
// was made and nothing about its two ends now: a person's only when a
// person confirmed it, or made it acting directly (basis ”, no agent).
func storedBasis(basis, agent string, by int64, confirmed bool) int8 {
	switch {
	case confirmed:
		return basisPerson
	case isGuess(basis):
		return basisInferred
	case basis == "" && agent == "" && by != 0:
		return basisPerson
	}
	return basisAgent
}

// Edge kinds, as a trace shows them.
const (
	edgeParent     = "subtask of"
	edgeServes     = "serves"
	edgeImplements = "implements"
	edgeMentions   = "mentions"
	edgeUnder      = "under"
	edgeRequested  = "written for"
	edgeDocuments  = "documents"
)

type gnode struct {
	typ string
	id  int64
}

type gitem struct {
	project  int64
	person   bool
	goal     bool
	approved bool
}

type gedge struct {
	to    gnode
	basis int8
	kind  string
	link  int64
	by    int64
	agent string
	score float64
}

type rgedge struct {
	from  gnode
	basis int8
}

// goalGraph is every item and edge, for the whole instance.
type goalGraph struct {
	items      map[gnode]*gitem
	out        map[gnode][]gedge
	in         map[gnode][]rgedge
	projParent map[int64]int64
	goalScopes map[int64]bool
}

func (g *goalGraph) add(from gnode, e gedge) {
	if g.items[from] == nil || g.items[e.to] == nil || from == e.to {
		return
	}
	g.out[from] = append(g.out[from], e)
	g.in[e.to] = append(g.in[e.to], rgedge{from: from, basis: e.basis})
}

// loadGoalGraph reads the graph. Drafts and personal pages stay out;
// retired notes too.
func loadGoalGraph(ctx context.Context, q queryer) (*goalGraph, error) {
	g := &goalGraph{items: map[gnode]*gitem{}, out: map[gnode][]gedge{}, in: map[gnode][]rgedge{},
		projParent: map[int64]int64{}, goalScopes: map[int64]bool{}}
	type parentRow struct {
		child, parent gnode
		person        bool
	}
	var parents []parentRow
	rows, err := q.QueryContext(ctx, `SELECT id, project_id, coalesce(parent_id, 0), outside = 0 AND (text_agent = '' OR reviewed_at IS NOT NULL),
		parent_by IS NOT NULL
		FROM issues WHERE draft = 0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, pid, parent int64
		var person, parentByPerson bool
		if err := rows.Scan(&id, &pid, &parent, &person, &parentByPerson); err != nil {
			rows.Close()
			return nil, err
		}
		g.items[gnode{KIssue, id}] = &gitem{project: pid, person: person}
		if parent != 0 {
			parents = append(parents, parentRow{gnode{KIssue, id}, gnode{KIssue, parent}, parentByPerson})
		}
	}
	rows.Close()
	// A person's 'intent' answer to a drift flag stands behind the flagged tasks.
	rows, err = q.QueryContext(ctx, `SELECT fi.issue_id FROM goal_drift_flag_issues fi JOIN goal_drift_flags f ON f.id = fi.flag_id WHERE f.answer = 'intent'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if it := g.items[gnode{KIssue, id}]; it != nil {
			it.person = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT id, project_id, text_agent = '' OR reviewed_at IS NOT NULL FROM pages WHERE draft = 0 AND project_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, pid int64
		var person bool
		if err := rows.Scan(&id, &pid, &person); err != nil {
			rows.Close()
			return nil, err
		}
		g.items[gnode{KPage, id}] = &gitem{project: pid, person: person}
	}
	rows.Close()
	type requestRow struct {
		note, issue int64
	}
	var requests []requestRow
	rows, err = q.QueryContext(ctx, `SELECT id, coalesce(project_id, 0), coalesce(parent_id, 0), kind = 'goal' AND status = 'active',
		approved_at IS NOT NULL OR requested_by IS NOT NULL, CASE WHEN requested_by IS NOT NULL THEN coalesce(requested_issue_id, 0) ELSE 0 END
		FROM design_notes WHERE status != 'retired'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, pid, parent, reqIssue int64
		var goal, approved bool
		if err := rows.Scan(&id, &pid, &parent, &goal, &approved, &reqIssue); err != nil {
			rows.Close()
			return nil, err
		}
		g.items[gnode{KDesign, id}] = &gitem{project: pid, person: approved, goal: goal, approved: approved}
		if goal && approved {
			g.goalScopes[pid] = true
		}
		if parent != 0 {
			// A note's place is part of the text a person approved.
			parents = append(parents, parentRow{gnode{KDesign, id}, gnode{KDesign, parent}, approved})
		}
		if reqIssue != 0 {
			requests = append(requests, requestRow{id, reqIssue})
		}
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT id, coalesce(parent_id, 0) FROM projects`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, parent int64
		if err := rows.Scan(&id, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		if parent != 0 {
			g.projParent[id] = parent
		}
	}
	rows.Close()

	for _, p := range parents {
		kind := edgeParent
		if p.child.typ == KDesign {
			kind = edgeUnder
		}
		b := basisAgent
		if p.person {
			b = basisPerson
		}
		g.add(p.child, gedge{to: p.parent, basis: b, kind: kind})
	}
	// A note written for a person's task traces to that task as the person's request, though nobody approved the note.
	for _, r := range requests {
		g.add(gnode{KDesign, r.note}, gedge{to: gnode{KIssue, r.issue}, basis: basisPerson, kind: edgeRequested})
	}
	rows, err = q.QueryContext(ctx, `SELECT page_id, issue_id, created_agent FROM page_issues`)
	if err != nil {
		return nil, err
	}
	type pageRow struct {
		page, issue int64
		agent       string
	}
	var pages []pageRow
	for rows.Next() {
		var r pageRow
		if err := rows.Scan(&r.page, &r.issue, &r.agent); err != nil {
			rows.Close()
			return nil, err
		}
		pages = append(pages, r)
	}
	rows.Close()
	for _, r := range pages {
		b := basisPerson
		if r.agent != "" {
			b = basisAgent
		}
		g.add(gnode{KPage, r.page}, gedge{to: gnode{KIssue, r.issue}, basis: b, kind: edgeDocuments, agent: r.agent})
	}
	rows, err = q.QueryContext(ctx, `SELECT id, src_type, src_id, dst_type, dst_id, rel, auto, basis, coalesce(score, 0),
		coalesce(created_by, 0), created_agent, coalesce(confirmed_by, 0)
		FROM knowledge_links WHERE dst_id IS NOT NULL AND src_type IN ('issue', 'page', 'design') AND dst_type IN ('issue', 'design')
		AND (rel IN ('serves', 'implements') OR (auto = 1 AND dst_type = 'design'))
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type linkRow struct {
		id              int64
		src, dst        gnode
		rel, basis      string
		auto            bool
		score           float64
		by, confirmedBy int64
		agent           string
	}
	var links []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.id, &r.src.typ, &r.src.id, &r.dst.typ, &r.dst.id, &r.rel, &r.auto, &r.basis, &r.score, &r.by, &r.agent, &r.confirmedBy); err != nil {
			rows.Close()
			return nil, err
		}
		links = append(links, r)
	}
	rows.Close()
	dismissed := map[[2]gnode]bool{}
	rows, err = q.QueryContext(ctx, `SELECT src_type, src_id, goal_id FROM goal_dismissals`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src gnode
		var goal int64
		if err := rows.Scan(&src.typ, &src.id, &goal); err != nil {
			rows.Close()
			return nil, err
		}
		dismissed[[2]gnode{src, {KDesign, goal}}] = true
	}
	rows.Close()
	for _, r := range links {
		dst := g.items[r.dst]
		kind := r.rel
		if r.auto {
			if dst == nil || !dst.goal {
				continue
			}
			kind = edgeMentions
		}
		e := gedge{to: r.dst, kind: kind, link: r.id, agent: r.agent, score: r.score}
		e.basis = storedBasis(r.basis, r.agent, r.by, r.confirmedBy != 0)
		switch {
		case r.confirmedBy != 0:
			e.by = r.confirmedBy
		case e.basis == basisPerson:
			e.by = r.by
		}
		// A person's no binds agents: only a person's own link brings a dismissed pair back.
		if e.basis != basisPerson && dismissed[[2]gnode{r.src, r.dst}] {
			continue
		}
		g.add(r.src, e)
	}
	for n := range g.out {
		slices.SortStableFunc(g.out[n], func(a, b gedge) int {
			if a.basis != b.basis {
				return int(b.basis) - int(a.basis)
			}
			return cmpNode(a.to, b.to)
		})
	}
	return g, nil
}

func cmpNode(a, b gnode) int {
	if a.typ != b.typ {
		return strings.Compare(a.typ, b.typ)
	}
	switch {
	case a.id < b.id:
		return -1
	case a.id > b.id:
		return 1
	}
	return 0
}

// gtrace is one item's grounding.
type gtrace struct {
	grounding  string
	goal       int64
	hops       int
	personHops int
	touches    int
	scoped     bool
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// goalSources are the active goals, approved or not, by id.
func (g *goalGraph) goalSources(approved bool) []gnode {
	var out []gnode
	for n, it := range g.items {
		if it.goal && it.approved == approved {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, cmpNode)
	return out
}

// reach walks back from sources over edges at least minBasis strong and
// gives each item it reaches, not already in out, this grounding: the
// nearest source, the lowest goal id among equals, then the most person
// touches.
func (g *goalGraph) reach(sources []gnode, minBasis int8, grounding string, out map[gnode]*gtrace) {
	type st struct {
		goal          int64
		dist, touches int
	}
	seen := map[gnode]*st{}
	var frontier []gnode
	for _, s := range sources {
		seen[s] = &st{goal: s.id, touches: b2i(g.items[s].person)}
		frontier = append(frontier, s)
	}
	for len(frontier) > 0 {
		next := map[gnode]*st{}
		for _, f := range frontier {
			fs := seen[f]
			for _, e := range g.in[f] {
				if e.basis < minBasis {
					continue
				}
				if _, ok := seen[e.from]; ok {
					continue
				}
				c := &st{goal: fs.goal, dist: fs.dist + 1, touches: fs.touches + b2i(e.basis == basisPerson) + b2i(g.items[e.from].person)}
				if o := next[e.from]; o == nil || c.goal < o.goal || (c.goal == o.goal && c.touches > o.touches) {
					next[e.from] = c
				}
			}
		}
		frontier = frontier[:0]
		for n, c := range next {
			seen[n] = c
			frontier = append(frontier, n)
		}
	}
	for n, c := range seen {
		if _, done := out[n]; done {
			continue
		}
		out[n] = &gtrace{grounding: grounding, goal: c.goal, hops: c.dist, touches: c.touches}
	}
}

// groundings computes every item's grounding.
func (g *goalGraph) groundings() map[gnode]*gtrace {
	out := map[gnode]*gtrace{}
	approved := g.goalSources(true)
	g.reach(approved, basisPerson, GroundPersonGoal, out)
	g.reach(approved, basisAgent, GroundAgentGoal, out)
	g.reach(approved, basisInferred, GroundInferredGoal, out)
	g.reach(g.goalSources(false), basisInferred, GroundProposedGoal, out)

	type st struct{ dist, touches int }
	seen := map[gnode]*st{}
	var frontier []gnode
	for n, it := range g.items {
		if it.person {
			seen[n] = &st{touches: 1}
			frontier = append(frontier, n)
		}
	}
	for len(frontier) > 0 {
		next := map[gnode]*st{}
		for _, f := range frontier {
			fs := seen[f]
			for _, e := range g.in[f] {
				if _, ok := seen[e.from]; ok {
					continue
				}
				c := &st{dist: fs.dist + 1, touches: fs.touches + b2i(e.basis == basisPerson)}
				if o := next[e.from]; o == nil || c.touches > o.touches {
					next[e.from] = c
				}
			}
		}
		frontier = frontier[:0]
		for n, c := range next {
			seen[n] = c
			frontier = append(frontier, n)
		}
	}
	for n, it := range g.items {
		t := out[n]
		p := seen[n]
		if t == nil {
			t = &gtrace{grounding: GroundNone}
			if p != nil {
				t.grounding, t.touches = GroundPersonRequest, p.touches
			}
			out[n] = t
		}
		t.personHops = -1
		if p != nil {
			t.personHops = p.dist
		}
		t.scoped = g.scoped(it.project)
	}
	return out
}

// scoped says whether a project, an ancestor or global holds an approved
// goal.
func (g *goalGraph) scoped(project int64) bool {
	if g.goalScopes[0] {
		return true
	}
	for n := 0; project != 0 && n < 32; n++ {
		if g.goalScopes[project] {
			return true
		}
		project = g.projParent[project]
	}
	return false
}

// goalState coalesces recomputes: one runs at a time, and a change while
// it runs starts one more after it.
type goalState struct {
	mu sync.Mutex
	// store serializes storeGoalTraces: a pass that read the graph before a link was made must not overwrite rows of a pass that read after it.
	store    sync.Mutex
	running  bool
	dirty    bool
	hasGoals bool
	checked  bool
	idle     chan struct{}
	// The goals model's judgements run on their own, so a slow model never holds up a recompute or goalsSettle.
	judging, judgeDirty      bool
	judgeIdle                chan struct{}
	judgeTimer               *time.Timer
	judgeGen                 uint64
	meaningEmbed, meaningNLI string
	suggesting               map[int64]bool
	asking                   map[int64]bool
	drifting                 bool
	driftIdle                chan struct{}
	// Per Service, so a test can shorten goalsSettle without changing other Services (0: goalsSettleWait).
	settleWait  time.Duration
	beforeStore func(context.Context)
}

// goalsSettleWait caps goalsSettle: a view waits this long for a recompute,
// then reads what is there. Tests that must outwait every pass use
// quiesceGoals instead (WP-1372).
const goalsSettleWait = 5 * time.Second

// goalsSettle waits, briefly, for a recompute a change started, so a view
// read just after a link or an edit shows it.
func (s *Service) goalsSettle(ctx context.Context) {
	s.goals.mu.Lock()
	ch := s.goals.idle
	running := s.goals.running
	wait := s.goals.settleWait
	if wait == 0 {
		wait = goalsSettleWait
	}
	s.goals.mu.Unlock()
	if !running || ch == nil {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-t.C:
	}
}

// EvGoalsTraced tells live clients a background recompute changed how
// work traces to goals.
const EvGoalsTraced = "goals.traced"

// goalQuietEvents are issue events that change nothing the trace or the
// matcher reads (who works on it, its step, its work log, its share).
var goalQuietEvents = map[string]bool{
	EvIssueAssigned: true, EvIssueUnassigned: true, EvIssueStep: true, EvIssueWorkLog: true,
	EvIssueShared: true, EvIssueChildrenDone: true,
}

// onGoalEvent notes a change that can move a grounding: an issue's text,
// parent, status or comments (comments make [[wikilink]] links), pages,
// links and design notes, a project's place.
func (s *Service) onGoalEvent(e *Event) {
	switch {
	case goalQuietEvents[e.Kind]:
		return
	case strings.HasPrefix(e.Kind, "issue."), e.Kind == EvPageUpdated, e.Kind == EvKnowledge, e.Kind == EvProjectUpdated:
	default:
		return
	}
	s.goals.mu.Lock()
	has := s.goals.hasGoals || !s.goals.checked
	s.goals.mu.Unlock()
	// Until a goal exists nothing is traced, except a design-note change, which may be the first goal.
	if !has && (e.Kind != EvKnowledge || e.Data["type"] != KDesign) {
		return
	}
	s.goalsChanged()
}

// goalsChanged recomputes the traces in the background.
func (s *Service) goalsChanged() {
	s.goals.mu.Lock()
	if s.goals.running {
		s.goals.dirty = true
		s.goals.mu.Unlock()
		return
	}
	s.goals.running = true
	idle := make(chan struct{})
	s.goals.idle = idle
	s.goals.mu.Unlock()
	s.goBG(func() {
		// Close idle in the same critical section that clears running, so a wait never sees one without the other.
		for {
			if s.bgCtx.Err() != nil {
				s.goals.mu.Lock()
				s.goals.running = false
				close(idle)
				s.goals.mu.Unlock()
				return
			}
			s.job(s.bgCtx, jobGoalTraces, goalTracesEvery, func(ctx context.Context) {
				if n, err := s.RecomputeGoalTraces(ctx); err != nil && ctx.Err() == nil {
					s.log.WarnContext(ctx, "goal traces", "err", err)
				} else if n > 0 {
					s.Hub.Publish(&Event{Kind: EvGoalsTraced})
				}
			})
			// Meaning judgements are asked here, outside any write transaction, and only for text whose judgement is missing or stale.
			s.judgementsChanged()
			s.goals.mu.Lock()
			if !s.goals.dirty {
				s.goals.running = false
				close(idle)
				s.goals.mu.Unlock()
				return
			}
			s.goals.dirty = false
			s.goals.mu.Unlock()
		}
	})
}

// RecomputeGoalTraces brings goal_traces up to date with the graph, then
// lets the matcher guess links for work that traces to no goal and, if it
// guessed any, brings them in too. It returns how many rows changed.
func (s *Service) RecomputeGoalTraces(ctx context.Context) (int, error) {
	n, err := s.storeGoalTraces(ctx)
	if err != nil {
		return n, err
	}
	guessed, err := s.matchGoals(ctx)
	if err != nil {
		return n, err
	}
	if guessed > 0 {
		m, err := s.storeGoalTraces(ctx)
		return n + m, err
	}
	return n, nil
}

func (s *Service) storeGoalTraces(ctx context.Context) (int, error) {
	// Read, compare and write under one lock: a waiting pass must start from what the one before it wrote.
	s.goals.store.Lock()
	defer s.goals.store.Unlock()
	var goals int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM design_notes WHERE kind = 'goal'`).Scan(&goals); err != nil {
		return 0, err
	}
	s.goals.mu.Lock()
	s.goals.hasGoals, s.goals.checked = goals > 0, true
	s.goals.mu.Unlock()
	type row struct {
		project    sql.NullInt64
		grounding  string
		goal       sql.NullInt64
		hops       int
		personHops sql.NullInt64
		touches    int
		scoped     bool
	}
	old := map[gnode]row{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT item_type, item_id, project_id, grounding, goal_id, hops, person_hops, person_touches, scoped FROM goal_traces`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var n gnode
		var r row
		if err := rows.Scan(&n.typ, &n.id, &r.project, &r.grounding, &r.goal, &r.hops, &r.personHops, &r.touches, &r.scoped); err != nil {
			rows.Close()
			return 0, err
		}
		old[n] = r
	}
	rows.Close()
	want := map[gnode]row{}
	if goals > 0 {
		g, err := loadGoalGraph(ctx, s.db.R)
		if err != nil {
			return 0, err
		}
		for n, t := range g.groundings() {
			r := row{grounding: t.grounding, hops: t.hops, touches: t.touches, scoped: t.scoped}
			if p := g.items[n].project; p != 0 {
				r.project = sql.NullInt64{Int64: p, Valid: true}
			}
			if t.goal != 0 {
				r.goal = sql.NullInt64{Int64: t.goal, Valid: true}
			}
			if t.personHops >= 0 {
				r.personHops = sql.NullInt64{Int64: int64(t.personHops), Valid: true}
			}
			want[n] = r
		}
	}
	var put []gnode
	var drop []gnode
	for n, r := range want {
		if o, ok := old[n]; !ok || o != r {
			put = append(put, n)
		}
	}
	for n := range old {
		if _, ok := want[n]; !ok {
			drop = append(drop, n)
		}
	}
	// Finished tasks keep the grounding they had when finished, for the week's drift; one finished since the last snapshot takes its current grounding.
	var unsnapped int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE st.category = 'done' AND i.resolved_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM goal_finished gf WHERE gf.issue_id = i.id AND gf.resolved_at = i.resolved_at)
		AND EXISTS (SELECT 1 FROM goal_traces gt WHERE gt.item_type = 'issue' AND gt.item_id = i.id)`).Scan(&unsnapped); err != nil {
		return 0, err
	}
	if len(put) == 0 && len(drop) == 0 && unsnapped == 0 {
		return 0, nil
	}
	sort.Slice(put, func(a, b int) bool { return cmpNode(put[a], put[b]) < 0 })
	if s.goals.beforeStore != nil {
		s.goals.beforeStore(ctx)
	}
	now := s.nowMS()
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, n := range drop {
			if _, err := t.ExecContext(ctx, `DELETE FROM goal_traces WHERE item_type = ? AND item_id = ?`, n.typ, n.id); err != nil {
				return err
			}
		}
		for _, n := range put {
			r := want[n]
			if _, err := t.ExecContext(ctx, `INSERT INTO goal_traces(item_type, item_id, project_id, grounding, goal_id, hops, person_hops, person_touches, scoped, computed_at)
				VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(item_type, item_id) DO UPDATE SET project_id = excluded.project_id, grounding = excluded.grounding,
				goal_id = excluded.goal_id, hops = excluded.hops, person_hops = excluded.person_hops, person_touches = excluded.person_touches,
				scoped = excluded.scoped, computed_at = excluded.computed_at`,
				n.typ, n.id, r.project, r.grounding, r.goal, r.hops, r.personHops, r.touches, r.scoped, now); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, `INSERT INTO goal_finished(issue_id, project_id, grounding, resolved_at)
			SELECT i.id, i.project_id, gt.grounding, i.resolved_at FROM issues i JOIN statuses st ON st.id = i.status_id
			JOIN goal_traces gt ON gt.item_type = 'issue' AND gt.item_id = i.id
			WHERE st.category = 'done' AND i.resolved_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM goal_finished gf WHERE gf.issue_id = i.id AND gf.resolved_at = i.resolved_at)
			ON CONFLICT(issue_id) DO UPDATE SET project_id = excluded.project_id, grounding = excluded.grounding, resolved_at = excluded.resolved_at`)
		return err
	})
	return len(put) + len(drop), err
}

// goalCond is WQL goal:VALUE (WP-382): how a task traces to the goals.
// none: to no approved goal; any: to one; people, agent, guess, proposed,
// request: by that grounding; a number: to that goal (its best route).
func goalCond(vals []string) (string, []any, error) {
	const traced = `EXISTS (SELECT 1 FROM goal_traces gt WHERE gt.item_type = 'issue' AND gt.item_id = i.id AND `
	approved := "gt.grounding IN ('" + GroundPersonGoal + "', '" + GroundAgentGoal + "', '" + GroundInferredGoal + "')"
	var parts []string
	var args []any
	for _, v := range vals {
		v = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "#"))
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			parts = append(parts, traced+"gt.goal_id = ?)")
			args = append(args, n)
			continue
		}
		g := ""
		switch v {
		case "none", "untraced", "no":
			parts = append(parts, "NOT "+traced+approved+")")
			continue
		case "any", "traced", "yes":
			parts = append(parts, traced+approved+")")
			continue
		case "people", "person", "human":
			g = GroundPersonGoal
		case "agent", "agents":
			g = GroundAgentGoal
		case "guess", "guessed", "inferred":
			g = GroundInferredGoal
		case "proposed", "suggested", "unapproved":
			g = GroundProposedGoal
		case "request", "requested", "asked":
			g = GroundPersonRequest
		default:
			return "", nil, invalid("goal:%s: use none, any, people, agent, guess, proposed, request or a goal's number", v)
		}
		parts = append(parts, traced+"gt.grounding = ?)")
		args = append(args, g)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, nil
}
