package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The matcher links work that traces to no goal to the goal it is plainly
// about: its tf-idf cosine to one goal's profile clears goalMatchMin and
// leads the next goal's by goalMatchLead. Below that, the Goals view still
// offers the best goal over goalSuggestMin for a person's one click.
const (
	goalMatchMin    = 0.25
	goalMatchLead   = 1.4
	goalSuggestMin  = 0.1
	goalRecentDays  = 14
	goalProfileWork = 0.5
)

// goalProfile is a goal's words: its own text, the notes under it and the
// work people and agents linked to it.
type goalProfile struct {
	id, project int64
	title       string
	vec         map[string]float64
}

// goalMatcher scores items against approved goals.
type goalMatcher struct {
	idx        *simIndex
	goals      []goalProfile
	projParent map[int64]int64
	dismissed  map[[2]int64]bool
}

func (s *Service) newGoalMatcher(ctx context.Context) (*goalMatcher, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, coalesce(project_id, 0), coalesce(parent_id, 0), kind = 'goal' AND status = 'active',
		approved_at IS NOT NULL OR requested_by IS NOT NULL, title, body FROM design_notes WHERE status != 'retired' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type note struct {
		id, project, parent int64
		goal, approved      bool
		title, body         string
	}
	var notes []note
	children := map[int64][]int{}
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.id, &n.project, &n.parent, &n.goal, &n.approved, &n.title, &n.body); err != nil {
			rows.Close()
			return nil, err
		}
		n.goal = n.goal && n.approved
		if n.parent != 0 {
			children[n.parent] = append(children[n.parent], len(notes))
		}
		notes = append(notes, n)
	}
	rows.Close()
	m := &goalMatcher{projParent: map[int64]int64{}, dismissed: map[[2]int64]bool{}}
	if !slices.ContainsFunc(notes, func(n note) bool { return n.goal }) {
		return m, nil
	}
	if m.idx, err = s.similarityIndex(ctx); err != nil {
		return nil, err
	}
	work := map[int64][]int64{}
	rows, err = s.db.R.QueryContext(ctx, `SELECT goal_id, item_id FROM goal_traces WHERE item_type = 'issue' AND goal_id IS NOT NULL AND grounding = 'person_goal'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g, id int64
		if err := rows.Scan(&g, &id); err != nil {
			rows.Close()
			return nil, err
		}
		work[g] = append(work[g], id)
	}
	rows.Close()
	for _, n := range notes {
		if !n.goal {
			continue
		}
		counts := simCounts(n.title, n.body, nil)
		for t, c := range counts {
			counts[t] = 2 * c
		}
		queue := slices.Clone(children[n.id])
		for depth := 0; len(queue) > 0 && depth < 6; depth++ {
			var next []int
			for _, ci := range queue {
				c := notes[ci]
				if c.approved {
					for t, k := range simCounts(c.title, c.body, nil) {
						counts[t] += k
					}
				}
				next = append(next, children[c.id]...)
			}
			queue = next
		}
		vec := m.idx.weigh(counts)
		if ids := work[n.id]; len(ids) > 0 {
			sum := map[string]float64{}
			k := 0
			for _, id := range ids {
				if d := m.idx.docs[id]; d != nil {
					for t, w := range d.vec {
						sum[t] += w
					}
					k++
				}
			}
			for t, w := range sum {
				vec[t] += goalProfileWork * w / float64(k)
			}
			vec = unit(vec)
		}
		m.goals = append(m.goals, goalProfile{id: n.id, project: n.project, title: n.title, vec: vec})
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT id, coalesce(parent_id, 0) FROM projects`)
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
			m.projParent[id] = parent
		}
	}
	rows.Close()
	rows, err = s.db.R.QueryContext(ctx, `SELECT src_id, goal_id FROM goal_dismissals WHERE src_type = 'issue'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, g int64
		if err := rows.Scan(&src, &g); err != nil {
			rows.Close()
			return nil, err
		}
		m.dismissed[[2]int64{src, g}] = true
	}
	rows.Close()
	return m, nil
}

func unit(v map[string]float64) map[string]float64 {
	var norm float64
	for _, w := range v {
		norm += w * w
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	for t := range v {
		v[t] /= norm
	}
	return v
}

// inChain says whether goal scope gp (0: global) covers project p.
func (m *goalMatcher) inChain(gp, p int64) bool {
	if gp == 0 {
		return true
	}
	for n := 0; p != 0 && n < 32; n++ {
		if p == gp {
			return true
		}
		p = m.projParent[p]
	}
	return false
}

type goalScore struct {
	goal  *goalProfile
	score float64
}

// rank scores an issue's vector against the goals that cover its project,
// best first, leaving out goals a person turned down for it.
func (m *goalMatcher) rank(issueID, project int64, vec map[string]float64) []goalScore {
	var out []goalScore
	for n := range m.goals {
		g := &m.goals[n]
		if !m.inChain(g.project, project) || m.dismissed[[2]int64{issueID, g.id}] {
			continue
		}
		if sc := cosine(vec, g.vec); sc > 0 {
			out = append(out, goalScore{g, sc})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].goal.id < out[b].goal.id
	})
	return out
}

// sure is the goal the matcher links on its own, if one stands out.
func sureGoal(r []goalScore) *goalScore {
	if len(r) == 0 || r[0].score < goalMatchMin {
		return nil
	}
	if len(r) > 1 && r[0].score < goalMatchLead*r[1].score {
		return nil
	}
	return &r[0]
}

func (m *goalMatcher) issueVec(id int64, title, desc string) map[string]float64 {
	if d := m.idx.docs[id]; d != nil {
		return d.vec
	}
	return m.idx.vector(title, desc)
}

// matchGoals links each issue that traces to no approved goal to the one
// goal it is plainly about, as a guess (basis inferred). It returns how
// many links it made. Nothing is published: the traces it feeds are
// recomputed next.
func (s *Service) matchGoals(ctx context.Context) (int, error) {
	m, err := s.newGoalMatcher(ctx)
	if err != nil || len(m.goals) == 0 {
		return 0, err
	}
	since := s.nowMS() - goalRecentDays*24*3600*1000
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, i.title, i.description FROM goal_traces gt
		JOIN issues i ON i.id = gt.item_id JOIN statuses st ON st.id = i.status_id
		WHERE gt.item_type = 'issue' AND gt.scoped = 1 AND gt.grounding IN ('person_request', 'none', 'proposed_goal')
		AND i.draft = 0 AND i.outside = 0 AND (st.category NOT IN ('done', 'canceled') OR i.resolved_at >= ?)
		ORDER BY i.id`, since)
	if err != nil {
		return 0, err
	}
	type guess struct {
		issue, goal int64
		score       float64
		note        string
	}
	var guesses []guess
	for rows.Next() {
		var id, pid int64
		var title, desc string
		if err := rows.Scan(&id, &pid, &title, &desc); err != nil {
			rows.Close()
			return 0, err
		}
		vec := m.issueVec(id, title, desc)
		if g := sureGoal(m.rank(id, pid, vec)); g != nil {
			guesses = append(guesses, guess{id, g.goal.id, g.score, "Waypoint's guess: both are about " + m.idx.shared(vec, g.goal.vec)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(guesses) == 0 {
		return 0, err
	}
	made := 0
	now := s.nowMS()
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, g := range guesses {
			res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO knowledge_links(src_type, src_id, dst_type, dst_id, dst_ref, rel, note, created_by, created_agent, created_at, basis, score)
				VALUES ('issue', ?, 'design', ?, '', 'serves', ?, NULL, '', ?, ?, ?)`, g.issue, g.goal, g.note, now, linkBasisInferred, math.Round(g.score*1000)/1000)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				made++
			}
		}
		return nil
	})
	return made, err
}

// dismissedLink is a knowledge link as it was when a person turned it down:
// the dismissal keeps it, since the row is gone (or hidden) after.
type dismissedLink struct {
	score     sql.NullFloat64
	basis     string
	by        sql.NullInt64
	agent     string
	createdAt int64
}

// readDismissedLink reads link id before it is deleted. A link that is
// gone reads as none (nil).
func readDismissedLink(ctx context.Context, t *tx, id int64) (*dismissedLink, error) {
	var l dismissedLink
	err := t.QueryRowContext(ctx, `SELECT score, basis, created_by, created_agent, created_at FROM knowledge_links WHERE id = ?`, id).
		Scan(&l.score, &l.basis, &l.by, &l.agent, &l.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// recordGoalDismissal keeps a person's no to a guess, so it isn't made again,
// and the link it was about when there was one (was; nil for a suggestion
// dismissed): its score and who made it.
func (s *Service) recordGoalDismissal(ctx context.Context, t *tx, actor *Actor, typ string, id, goal int64, was *dismissedLink) error {
	var score, by, at any
	var basis, agent string
	if was != nil {
		if was.score.Valid {
			score = was.score.Float64
		}
		if was.by.Valid {
			by = was.by.Int64
		}
		basis, agent, at = was.basis, was.agent, was.createdAt
	}
	_, err := t.ExecContext(ctx, `INSERT INTO goal_dismissals(src_type, src_id, goal_id, user_id, created_at, link_score, link_basis, link_by, link_agent, link_created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (src_type, src_id, goal_id) DO UPDATE SET link_score = excluded.link_score, link_basis = excluded.link_basis,
			link_by = excluded.link_by, link_agent = excluded.link_agent, link_created_at = excluded.link_created_at
		WHERE goal_dismissals.link_created_at IS NULL AND excluded.link_created_at IS NOT NULL`, typ, id, goal, nullUser(actor.id()), s.nowMS(), score, basis, by, agent, at)
	return err
}

// linkBasis is who stands behind a link, as Links show it: the same
// stored rule the trace uses (storedBasis).
func linkBasis(basis, agent string, by int64, confirmed bool) string {
	return basisNames[storedBasis(basis, agent, by, confirmed)]
}

// goalTech are technologies a goal names when it says how rather than what.
var goalTech = map[string]string{}

func init() {
	for _, w := range strings.Fields(`Django Flask Rails Laravel Spring Express Next.js Nuxt React Vue Svelte Angular jQuery Tailwind Bootstrap
		Postgres PostgreSQL MySQL MariaDB SQLite MongoDB Redis Memcached Kafka RabbitMQ NATS Elasticsearch OpenSearch ClickHouse DynamoDB Cassandra
		Docker Kubernetes k8s k3s Helm Terraform Ansible Nomad Consul Vault Nginx Caddy Traefik Apache HAProxy Cloudflare AWS GCP Azure Lambda S3 EC2
		Python Golang Rust Java Kotlin Swift TypeScript JavaScript Node Node.js Deno Bun Ruby PHP Elixir Haskell Scala C++ C#
		GraphQL gRPC REST WebSocket WebSockets Protobuf JSON YAML TOML OAuth OIDC JWT SAML LDAP
		Playwright Cypress Jest Vitest Pytest Webpack Vite esbuild npm pnpm yarn pip Poetry
		Forgejo Gitea GitHub GitLab Jenkins CircleCI Grafana Prometheus Loki Sentry Datadog
		OpenAI Anthropic Claude GPT Gemini Llama Ollama LangChain htmx Electron Flutter`) {
		goalTech[strings.ToLower(w)] = w
	}
}

var (
	goalPathRE    = lazyre.New(`(?:^|\s)(\S+/\S+|\S+\.(?:go|ts|tsx|js|jsx|py|rb|rs|java|kt|sql|ya?ml|json|toml|md|sh|css|html|proto))(?:$|[\s,.;:])`)
	goalIdentRE   = lazyre.New("`[^`]+`|\\b[a-z]+[A-Z][A-Za-z0-9]*\\b|\\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*\\b|\\b[a-z0-9]+_[a-z0-9_]+\\b|\\b\\w+\\(\\)")
	goalVersionRE = lazyre.New(`\bv?\d+\.\d+(?:\.\d+)?\b`)
	goalUseRE     = lazyre.New(`(?i)\b(use|using|switch(?:ing)? to|migrat(?:e|ing) to|move to|port to|written in|built (?:with|on)|rewrite in|replace \S+ with)\s+([A-Za-z][\w.+#-]*)`)
	goalTechRE    = lazyre.New(`[A-Za-z][\w.+#]*[\w+#]|[A-Za-z]`)
)

// goalWording flags a goal whose title says how instead of what: it names
// a technology, a file, a code identifier or a version, or says "use X".
// Each flag is a sentence a person can act on.
func goalWording(title string) []string {
	var out []string
	add := func(msg string) {
		if !slices.Contains(out, msg) {
			out = append(out, msg)
		}
	}
	seen := map[string]bool{}
	if m := goalUseRE.FindStringSubmatch(title); m != nil {
		what := strings.TrimRight(m[2], ".,;:")
		if tech, ok := goalTech[strings.ToLower(what)]; ok || isCapitalized(what) {
			if ok {
				what = tech
				seen[tech] = true
			}
			add(fmt.Sprintf("says how (%q): say what it is for; using %s is a decision under this goal", strings.TrimSpace(m[0]), what))
		}
	}
	for _, w := range goalTechRE.FindAllString(title, -1) {
		tech, ok := goalTech[strings.ToLower(w)]
		if !ok || seen[tech] || strings.EqualFold(w, "go") {
			continue
		}
		seen[tech] = true
		add(fmt.Sprintf("names %s: say what you need from it; the choice of %s is a decision under this goal", tech, tech))
	}
	if m := goalPathRE.FindStringSubmatch(title); m != nil {
		add(fmt.Sprintf("names a file (%s): say what people get; where it lives is implementation", m[1]))
	}
	if m := goalIdentRE.FindString(title); m != "" && goalTech[strings.ToLower(strings.Trim(m, "`"))] == "" {
		add(fmt.Sprintf("names code (%s): say what it does for people", m))
	}
	if m := goalVersionRE.FindString(title); m != "" {
		add(fmt.Sprintf("names a version (%s): say what you need; which version is a decision", m))
	}
	if m := goalTaskRE.FindStringSubmatch(title); m != nil {
		add(fmt.Sprintf("reads as a task (%q): say what people can do or have once it's done", m[1]))
	} else if m := goalTaskObjRE.FindStringSubmatch(title); m != nil {
		add(fmt.Sprintf("reads as a task (%q): say what people can do or have once it's done", m[1]))
	}
	words := len(strings.Fields(title))
	if head, rest, ok := strings.Cut(title, ": "); ok && strings.Count(rest, ",") >= 2 {
		add(fmt.Sprintf("lists several things (“%s: …”): a goal is one outcome; make the parts goals under it, or put them in its body", clipText(head, 40)))
	} else if head, _, ok := strings.Cut(title, "; "); ok && words > 10 {
		add(fmt.Sprintf("lists several things (“%s; …”): a goal is one outcome; make the parts goals under it, or put them in its body", clipText(head, 40)))
	}
	if words > goalMaxWords {
		add(fmt.Sprintf("is long (%d words): a goal is one short sentence of at most %d; put the detail in its body or in the tasks under it", words, goalMaxWords))
	} else if n := len([]rune(title)); n > 140 {
		add(fmt.Sprintf("is long (%d characters): a goal is one short sentence; put the detail in its body or in the tasks under it", n))
	}
	return out
}

// goalMaxWords is the most words a goal's title has before Waypoint calls it
// long: a goal is a line a person reads at a glance (WP-679).
const goalMaxWords = 15

// goalTaskRE is a title that starts like a task or a spec, with what to
// build or change, rather than what people get. The verb must stand alone
// ("Drop-in …" isn't one).
var goalTaskRE = lazyre.New(`(?i)^\s*((?:add|implement|refactor|remove|create|migrate|rename|expose|wire|persist|rewrite|replace|introduce|extract|bump|downgrade|deprecate|configure|integrate|set up|hook up)(?:\s+[^\s,;:]+|\s*$))`)

// goalTaskObjRE is the same for verbs that double as nouns ("Build failures
// show up within a minute", "Store owners see their sales"): only a verb with
// its object ("Build a dashboard", "Fix the login crash") reads as a task.
var goalTaskObjRE = lazyre.New(`(?i)^\s*((?:build|fix|store|cache|port|install|upgrade|split|move|drop|delete|deploy)\s+(?:a|an|the|this|that|these|those|our|my|your|its|their|all|every|each|some|new)\b)`)

func isCapitalized(w string) bool {
	return w != "" && w[0] >= 'A' && w[0] <= 'Z'
}

// GoalCoverage is what traces to a goal, by grounding.
type GoalCoverage struct {
	Items       int            `json:"items"` // tasks, pages and notes that trace to it
	Open        int            `json:"open"`  // open tasks among them
	Finished    int            `json:"finished_week"`
	ByGrounding map[string]int `json:"by_grounding"`
	// Tension is how many open tasks are linked as in tension with it
	// (WP-1036): work that pulls against it, which no trace counts.
	Tension int `json:"tension,omitempty"`
}

// GoalView is a goal with what the view shows of it.
type GoalView struct {
	Note     *DesignNote  `json:"note"`
	Depth    int          `json:"depth"`             // 0: a top-level goal; 1: a goal under it; …
	Wording  []string     `json:"wording,omitempty"` // why its title reads as implementation
	Coverage GoalCoverage `json:"coverage"`
	// Rewording is the newest proposed edit that changes the goal's
	// words, by Waypoint's suggestions or an agent: one click takes it.
	Rewording *GoalRewording `json:"rewording,omitempty"`
	// Why is what a goal Waypoint suggested came from, while it waits.
	Why string `json:"why,omitempty"`
}

// GoalRewording is a pending edit to a goal's title.
type GoalRewording struct {
	RevisionID int64  `json:"revision_id"`
	Title      string `json:"title"`
	Agent      string `json:"agent,omitempty"`
}

// GoalLink is a link to a goal that no person stands behind yet: an
// agent's, or Waypoint's guess.
type GoalLink struct {
	LinkID    int64     `json:"link_id"`
	Item      KItem     `json:"item"`
	Goal      KItem     `json:"goal"`
	Basis     string    `json:"basis"` // agent, inferred
	Agent     string    `json:"agent,omitempty"`
	Score     *float64  `json:"score,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// GoalCandidate is the goal Waypoint thinks an item serves.
type GoalCandidate struct {
	Goal   KItem   `json:"goal"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// UntracedItem is open work that traces to no approved goal.
type UntracedItem struct {
	Item      KItem          `json:"item"`
	Grounding string         `json:"grounding"`
	Agent     string         `json:"agent,omitempty"` // the agent that wrote it, while no person reviewed it
	Candidate *GoalCandidate `json:"candidate,omitempty"`
}

// GoalDrift is how the past week's finished work traced to goals, against
// the week before.
type GoalDrift struct {
	Week          map[string]int `json:"week"`
	Previous      map[string]int `json:"previous"`
	Grounded      float64        `json:"grounded"`          // share of the week's a person or agent linked to an approved goal
	Guessed       float64        `json:"guessed"`           // share that reaches one only through Waypoint's guess
	PreviousShare float64        `json:"previous_grounded"` // the week before's
	Summary       string         `json:"summary"`
}

// GoalsOverview is a project's goals and how its work traces to them.
type GoalsOverview struct {
	Project     string         `json:"project"`
	ProjectName string         `json:"project_name"`
	Scoped      bool           `json:"scoped"` // an approved goal covers the project
	Goals       []*GoalView    `json:"goals"`
	Links       []GoalLink     `json:"links"` // to confirm or dismiss
	Untraced    []UntracedItem `json:"untraced"`
	UntracedAll int            `json:"untraced_total"`
	Unapproved  []KItem        `json:"unapproved"` // the project's notes of how no person approved or asked for
	// UnapprovedCount is how many there are; Unapproved holds the newest few.
	UnapprovedCount int               `json:"unapproved_count"`
	Totals          map[string]int    `json:"totals"`  // the project's open tasks by grounding
	Tracing         int               `json:"tracing"` // open tasks Waypoint hasn't traced yet
	Drift           *GoalDrift        `json:"drift"`
	DriftFlags      []DriftFlag       `json:"drift_flags"` // what the drift check asks a person (WP-559)
	Ask             []string          `json:"ask"`         // what to ask a person, in plain words
	Words           map[string]string `json:"words"`
	CanEdit         bool              `json:"can_edit"`
	CanApprove      bool              `json:"can_approve"`
}

// Maximum rows in each of the view's lists.
const (
	goalListMax     = 60
	goalUntracedMax = 50
)

// ListGoals is the Goals view of a project: its goals (its own, its
// ancestors' and global ones) with what traces to each, the links no
// person stands behind, its open work that traces to no goal with
// Waypoint's best guess, and what to ask.
func (s *Service) ListGoals(ctx context.Context, actor *Actor, project string) (*GoalsOverview, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(project) == "" {
		return nil, invalid("name a project")
	}
	p, err := s.projectByRef(ctx, s.db.R, project)
	if err != nil {
		return nil, err
	}
	role, err := s.projectRole(ctx, s.db.R, actor, p.ID)
	if err != nil {
		return nil, err
	}
	if role == RoleNone {
		return nil, notFound("project")
	}
	s.goalsSettle(ctx)
	s.goals.mu.Lock()
	fresh := s.goals.checked
	s.goals.mu.Unlock()
	if !fresh {
		if _, err := s.RecomputeGoalTraces(ctx); err != nil {
			return nil, err
		}
	}
	out := &GoalsOverview{Project: p.Key, ProjectName: p.Name, Goals: []*GoalView{}, Links: []GoalLink{}, Untraced: []UntracedItem{},
		Unapproved: []KItem{}, Totals: map[string]int{}, Ask: []string{}, Words: groundingWords,
		CanEdit: role >= RoleEditor && !actor.ReadOnly, CanApprove: role >= RoleEditor && !actor.ReadOnly && actor.actsAsPerson()}
	notes, err := s.ListDesign(ctx, actor, DesignQuery{Project: p.Key, Kind: "goal", Limit: 1000})
	if err != nil {
		return nil, err
	}
	out.Goals = orderGoals(notes)
	goalIDs := make([]any, 0, len(out.Goals))
	byID := map[int64]*GoalView{}
	for _, g := range out.Goals {
		g.Wording = goalWording(g.Note.Title)
		g.Coverage.ByGrounding = map[string]int{}
		goalIDs = append(goalIDs, g.Note.ID)
		byID[g.Note.ID] = g
		if g.Note.Approved || g.Note.Requested != nil {
			out.Scoped = true
		}
	}
	shown, err := s.goalOverlaps(ctx, out.Goals)
	if err != nil {
		return nil, err
	}
	if actor.actsAsPerson() {
		s.recordMeaningShown(ctx, shown)
	}
	subtree, err := s.projectSubtree(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if err := s.goalCoverage(ctx, actor, goalIDs, byID); err != nil {
		return nil, err
	}
	if err := s.goalRewordings(ctx, goalIDs, byID); err != nil {
		return nil, err
	}
	if err := s.goalLinksToConfirm(ctx, actor, subtree, out); err != nil {
		return nil, err
	}
	if err := s.untracedWork(ctx, actor, subtree, out); err != nil {
		return nil, err
	}
	if err := s.goalTotals(ctx, actor, subtree, out); err != nil {
		return nil, err
	}
	if err := s.unapprovedNotes(ctx, actor, p.ID, out); err != nil {
		return nil, err
	}
	if out.Drift, err = s.goalDrift(ctx, actor, subtree); err != nil {
		return nil, err
	}
	if err := s.goalDriftFlags(ctx, actor, subtree, out); err != nil {
		return nil, err
	}
	out.Ask = goalQuestions(out)
	return out, nil
}

// orderGoals puts sub-goals under their goals, approved goals first among
// equals; goals whose parent isn't in the list are top level.
func orderGoals(notes []*DesignNote) []*GoalView {
	in := map[int64]bool{}
	for _, n := range notes {
		in[n.ID] = true
	}
	kids := map[int64][]*DesignNote{}
	var tops []*DesignNote
	for _, n := range notes {
		if n.ParentID != nil && in[*n.ParentID] {
			kids[*n.ParentID] = append(kids[*n.ParentID], n)
		} else {
			tops = append(tops, n)
		}
	}
	order := func(list []*DesignNote) {
		slices.SortStableFunc(list, func(a, b *DesignNote) int {
			aa, bb := a.Approved || a.Requested != nil, b.Approved || b.Requested != nil
			if aa != bb {
				if aa {
					return -1
				}
				return 1
			}
			if a.Inherited != b.Inherited {
				if !a.Inherited {
					return -1
				}
				return 1
			}
			return int(a.ID - b.ID)
		})
	}
	var out []*GoalView
	var walk func(list []*DesignNote, depth int)
	walk = func(list []*DesignNote, depth int) {
		order(list)
		for _, n := range list {
			out = append(out, &GoalView{Note: n, Depth: depth})
			if depth < 8 {
				walk(kids[n.ID], depth+1)
			}
		}
	}
	walk(tops, 0)
	if out == nil {
		out = []*GoalView{}
	}
	return out
}

// projectSubtree is a project and the projects under it.
func (s *Service) projectSubtree(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `WITH RECURSIVE t(id) AS (SELECT ? UNION SELECT p.id FROM projects p JOIN t ON p.parent_id = t.id) SELECT id FROM t ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func anyIDs(ids []int64) []any {
	out := make([]any, len(ids))
	for n, id := range ids {
		out[n] = id
	}
	return out
}

func (s *Service) goalCoverage(ctx context.Context, actor *Actor, ids []any, byID map[int64]*GoalView) error {
	if len(ids) == 0 {
		return nil
	}
	vis, vargs := visibleSQL("gt.project_id", actor)
	week := s.nowMS() - 7*24*3600*1000
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT gt.goal_id, gt.grounding, count(*),
		sum(CASE WHEN gt.item_type = 'issue' AND st.category NOT IN ('done', 'canceled') THEN 1 ELSE 0 END),
		sum(CASE WHEN gt.item_type = 'issue' AND st.category = 'done' AND i.resolved_at >= ? THEN 1 ELSE 0 END)
		FROM goal_traces gt LEFT JOIN issues i ON gt.item_type = 'issue' AND i.id = gt.item_id LEFT JOIN statuses st ON st.id = i.status_id
		WHERE gt.goal_id IN (`+idList(len(ids))+`) AND NOT (gt.item_type = 'design' AND gt.item_id = gt.goal_id) AND `+vis+`
		GROUP BY gt.goal_id, gt.grounding`, append(append([]any{week}, ids...), vargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g int64
		var grounding string
		var n, open, done int
		if err := rows.Scan(&g, &grounding, &n, &open, &done); err != nil {
			return err
		}
		if v := byID[g]; v != nil {
			v.Coverage.Items += n
			v.Coverage.Open += open
			v.Coverage.Finished += done
			v.Coverage.ByGrounding[grounding] += n
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.goalTension(ctx, actor, ids, byID)
}

// goalTension counts the open tasks linked as in tension with each goal,
// whichever end the link starts from.
func (s *Service) goalTension(ctx context.Context, actor *Actor, ids []any, byID map[int64]*GoalView) error {
	vis, vargs := visibleSQL("i.project_id", actor)
	list := idList(len(ids))
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT x.goal, count(DISTINCT i.id) FROM (
			SELECT dst_id AS goal, src_id AS item FROM knowledge_links WHERE rel = 'tension' AND src_type = 'issue' AND dst_type = 'design' AND dst_id IN (`+list+`)
			UNION SELECT src_id, dst_id FROM knowledge_links WHERE rel = 'tension' AND src_type = 'design' AND dst_type = 'issue' AND src_id IN (`+list+`)
		) x JOIN issues i ON i.id = x.item JOIN statuses st ON st.id = i.status_id
		WHERE st.category NOT IN ('done', 'canceled') AND i.draft = 0 AND `+vis+`
		GROUP BY x.goal`, append(append(append([]any{}, ids...), ids...), vargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g int64
		var n int
		if err := rows.Scan(&g, &n); err != nil {
			return err
		}
		if v := byID[g]; v != nil {
			v.Coverage.Tension = n
		}
	}
	return rows.Err()
}

// goalLinksToConfirm lists the agents' links and Waypoint's guesses from
// the project's items that no person confirmed.
func (s *Service) goalLinksToConfirm(ctx context.Context, actor *Actor, subtree []int64, out *GoalsOverview) error {
	// #nosec G202 -- placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT kl.id, kl.src_type, kl.src_id, kl.dst_id, kl.basis, kl.created_agent, kl.score, kl.note, kl.created_at
		FROM knowledge_links kl JOIN goal_traces gt ON gt.item_type = kl.src_type AND gt.item_id = kl.src_id
		JOIN design_notes g ON g.id = kl.dst_id AND g.status != 'retired'
		WHERE kl.rel = 'serves' AND kl.dst_type = 'design' AND kl.confirmed_by IS NULL AND kl.acknowledged_at IS NULL AND kl.auto = 0
		AND (kl.basis != '' OR kl.created_agent != '' OR kl.created_by IS NULL) AND gt.project_id IN (`+idList(len(subtree))+`)
		AND NOT (kl.basis = ? AND g.approved_at IS NULL) -- a suggested goal's own tasks: approving it confirms them
		ORDER BY kl.basis IN (?, ?), CASE WHEN kl.basis IN (?, ?) THEN kl.score END DESC, kl.id DESC LIMIT ?`,
		append(anyIDs(subtree), linkBasisSuggested, linkBasisInferred, linkBasisSuggested, linkBasisInferred, linkBasisSuggested, goalListMax*2)...)
	if err != nil {
		return err
	}
	type row struct {
		l        GoalLink
		st       string
		sid, gid int64
		basis    string
		score    sql.NullFloat64
		created  int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.l.LinkID, &r.st, &r.sid, &r.gid, &r.basis, &r.l.Agent, &r.score, &r.l.Note, &r.created); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list {
		if len(out.Links) >= goalListMax {
			break
		}
		item, err := s.kitem(ctx, actor, r.st, r.sid, "")
		if err != nil {
			continue
		}
		goal, err := s.kitem(ctx, actor, KDesign, r.gid, "")
		if err != nil {
			continue
		}
		r.l.Item, r.l.Goal = *item, *goal
		r.l.Basis = linkBasis(r.basis, r.l.Agent, 0, false)
		if r.score.Valid && isGuess(r.basis) {
			r.l.Score = &r.score.Float64
		}
		r.l.CreatedAt = msTime(r.created)
		out.Links = append(out.Links, r.l)
	}
	return nil
}

// untracedWork lists the project's open tasks that trace to no approved
// goal (no person or agent anywhere above them first), with the goal
// Waypoint thinks each serves.
func (s *Service) untracedWork(ctx context.Context, actor *Actor, subtree []int64, out *GoalsOverview) error {
	vis, vargs := visibleSQL("i.project_id", actor)
	args := append(anyIDs(subtree), vargs...)
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	from := ` FROM goal_traces gt JOIN issues i ON i.id = gt.item_id JOIN statuses st ON st.id = i.status_id
		WHERE gt.item_type = 'issue' AND gt.grounding IN ('none', 'person_request', 'proposed_goal')
		AND st.category NOT IN ('done', 'canceled') AND i.project_id IN (` + idList(len(subtree)) + `) AND ` + vis + ` AND ` + draftsHidden(actor, "i")
	// #nosec G202 -- as above
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&out.UntracedAll); err != nil {
		return err
	}
	// #nosec G202 -- as above
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, i.title, i.description, gt.grounding, CASE WHEN i.reviewed_at IS NULL THEN i.text_agent ELSE '' END`+from+`
		ORDER BY gt.grounding = 'none' DESC, (i.text_agent != '' AND i.reviewed_at IS NULL) DESC, i.id DESC LIMIT ?`, append(args, goalUntracedMax)...)
	if err != nil {
		return err
	}
	type row struct {
		id, project      int64
		title, desc      string
		grounding, agent string
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.project, &r.title, &r.desc, &r.grounding, &r.agent); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var m *goalMatcher
	if len(list) > 0 && out.Scoped {
		if m, err = s.newGoalMatcher(ctx); err != nil {
			return err
		}
	}
	for _, r := range list {
		item, err := s.kitem(ctx, actor, KIssue, r.id, "")
		if err != nil {
			continue
		}
		u := UntracedItem{Item: *item, Grounding: r.grounding, Agent: r.agent}
		if m != nil && len(m.goals) > 0 {
			u.Candidate = s.goalCandidate(ctx, actor, m, r.id, r.project, r.title, r.desc)
		}
		out.Untraced = append(out.Untraced, u)
	}
	return nil
}

// goalTotals counts the project's open tasks by grounding.
func (s *Service) goalTotals(ctx context.Context, actor *Actor, subtree []int64, out *GoalsOverview) error {
	vis, vargs := visibleSQL("i.project_id", actor)
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT gt.grounding, count(*) FROM goal_traces gt JOIN issues i ON i.id = gt.item_id JOIN statuses st ON st.id = i.status_id
		WHERE gt.item_type = 'issue' AND st.category NOT IN ('done', 'canceled') AND i.project_id IN (`+idList(len(subtree))+`) AND `+vis+` AND `+draftsHidden(actor, "i")+`
		GROUP BY 1`, append(anyIDs(subtree), vargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		var n int
		if err := rows.Scan(&g, &n); err != nil {
			return err
		}
		out.Totals[g] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !out.Scoped {
		return nil
	}
	// #nosec G202 -- as above
	return s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE st.category NOT IN ('done', 'canceled') AND i.project_id IN (`+idList(len(subtree))+`) AND `+vis+` AND `+draftsHidden(actor, "i")+`
		AND NOT EXISTS (SELECT 1 FROM goal_traces gt WHERE gt.item_type = 'issue' AND gt.item_id = i.id)`, append(anyIDs(subtree), vargs...)...).Scan(&out.Tracing)
}

// goalCandidate is the visible goal an issue most likely serves.
func (s *Service) goalCandidate(ctx context.Context, actor *Actor, m *goalMatcher, id, project int64, title, desc string) *GoalCandidate {
	vec := m.issueVec(id, title, desc)
	for _, g := range m.rank(id, project, vec) {
		if g.score < goalSuggestMin {
			return nil
		}
		goal, err := s.kitem(ctx, actor, KDesign, g.goal.id, "")
		if err != nil {
			continue
		}
		return &GoalCandidate{Goal: *goal, Score: math.Round(g.score*1000) / 1000, Reason: "Both are about " + m.idx.shared(vec, g.goal.vec)}
	}
	return nil
}

// unapprovedNotes lists the project's notes of how (features, decisions,
// requirements, …) no person approved or asked for.
func (s *Service) unapprovedNotes(ctx context.Context, actor *Actor, project int64, out *GoalsOverview) error {
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM design_notes WHERE project_id = ? AND kind != 'goal' AND status = 'active'
		AND approved_at IS NULL AND requested_by IS NULL`, project).Scan(&out.UnapprovedCount); err != nil {
		return err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM design_notes WHERE project_id = ? AND kind != 'goal' AND status = 'active'
		AND approved_at IS NULL AND requested_by IS NULL ORDER BY id DESC LIMIT ?`, project, goalListMax)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if it, err := s.kitem(ctx, actor, KDesign, id, ""); err == nil {
			out.Unapproved = append(out.Unapproved, *it)
		}
	}
	return rows.Err()
}

// goalDrift compares the past week's finished tasks with the week before.
func (s *Service) goalDrift(ctx context.Context, actor *Actor, subtree []int64) (*GoalDrift, error) {
	now := s.nowMS()
	week := int64(7 * 24 * 3600 * 1000)
	d := &GoalDrift{Week: map[string]int{}, Previous: map[string]int{}}
	vis, vargs := "1", []any(nil)
	if actor != nil {
		vis, vargs = visibleSQL("i.project_id", actor)
		vis += " AND " + draftsHidden(actor, "i")
	}
	// #nosec G202 -- vis is a fixed condition and the rest placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT CASE WHEN gf.resolved_at = i.resolved_at THEN gf.grounding ELSE gt.grounding END, i.resolved_at >= ?, count(*)
		FROM goal_traces gt JOIN issues i ON i.id = gt.item_id JOIN statuses st ON st.id = i.status_id
		LEFT JOIN goal_finished gf ON gf.issue_id = i.id
		WHERE gt.item_type = 'issue' AND st.category = 'done' AND i.resolved_at >= ? AND i.project_id IN (`+idList(len(subtree))+`) AND `+vis+`
		GROUP BY 1, 2`, append(append([]any{now - week, now - 2*week}, anyIDs(subtree)...), vargs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		var recent bool
		var n int
		if err := rows.Scan(&g, &recent, &n); err != nil {
			return nil, err
		}
		if recent {
			d.Week[g] += n
		} else {
			d.Previous[g] += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	share := func(m map[string]int, which ...string) (float64, int) {
		var at, all int
		for g, n := range m {
			all += n
			if slices.Contains(which, g) {
				at += n
			}
		}
		if all == 0 {
			return 0, 0
		}
		return math.Round(float64(at)/float64(all)*100) / 100, all
	}
	var nw, np int
	d.Grounded, nw = share(d.Week, GroundPersonGoal, GroundAgentGoal)
	d.PreviousShare, np = share(d.Previous, GroundPersonGoal, GroundAgentGoal)
	d.Guessed, _ = share(d.Week, GroundInferredGoal)
	switch {
	case nw == 0:
		d.Summary = "no tasks finished this week"
	case np == 0:
		d.Summary = weekShare(d.Grounded, nw)
	default:
		change := "the same as"
		switch diff := d.Grounded - d.PreviousShare; {
		case diff >= 0.1:
			change = "up from"
		case diff <= -0.1:
			change = "down from"
		}
		d.Summary = fmt.Sprintf("%s, %s %d%% the week before", weekShare(d.Grounded, nw), change, int(d.PreviousShare*100))
	}
	if n := d.Week[GroundInferredGoal]; n > 0 {
		d.Summary += fmt.Sprintf("; %d more only through Waypoint's guess, which no one confirmed", n)
	}
	return d, nil
}

// weekShare words the week: "4 of the 5 tasks finished this week (80%)
// trace to an approved goal".
func weekShare(share float64, n int) string {
	at := int(math.Round(share * float64(n)))
	if n == 1 {
		if at == 1 {
			return "the 1 task finished this week traces to an approved goal"
		}
		return "the 1 task finished this week traces to no approved goal"
	}
	return fmt.Sprintf("%d of the %d tasks finished this week (%d%%) trace to an approved goal", at, n, int(share*100))
}

// goalQuestions are what to ask a person, plainly, most useful first.
func goalQuestions(o *GoalsOverview) []string {
	var q []string
	if !o.Scoped {
		q = append(q, fmt.Sprintf("What should %s do for the people who use it? Write each goal as one plain sentence; Waypoint links the work to it.", o.ProjectName))
	}
	for _, g := range o.Goals {
		if !g.Note.Approved && g.Note.Requested == nil && len(q) < 3 {
			q = append(q, fmt.Sprintf("Is %q one of your goals? Approve it or dismiss it.", g.Note.Title))
		}
	}
	for _, g := range o.Goals {
		if len(g.Wording) > 0 && len(q) < 5 {
			q = append(q, fmt.Sprintf("Goal %q %s.", g.Note.Title, g.Wording[0]))
		}
	}
	n := 0
	for _, l := range o.Links {
		if l.Basis == basisNames[basisInferred] && n < 3 {
			q = append(q, fmt.Sprintf("Does %s %q serve %q?", l.Item.Key, l.Item.Title, l.Goal.Title))
			n++
		}
	}
	n = 0
	for _, u := range o.Untraced {
		if u.Grounding == GroundNone && n < 3 {
			q = append(q, fmt.Sprintf("What is %s %q for? It traces to no goal and no person asked for it.", u.Item.Key, u.Item.Title))
			n++
		}
	}
	if q == nil {
		q = []string{}
	}
	return q
}

// TraceHop is one step of a route: From is for To.
type TraceHop struct {
	From   KItem    `json:"from"`
	To     KItem    `json:"to"`
	Kind   string   `json:"kind"`  // subtask of, serves, implements, mentions, under, written for, documents
	Basis  string   `json:"basis"` // person, agent, inferred
	By     *UserRef `json:"by,omitempty"`
	Agent  string   `json:"agent,omitempty"`
	Score  *float64 `json:"score,omitempty"`
	LinkID int64    `json:"link_id,omitempty"`
}

// TraceRoute is a way from an item to a goal.
type TraceRoute struct {
	Goal      KItem      `json:"goal"`
	Grounding string     `json:"grounding"`
	Hops      []TraceHop `json:"hops"`
}

// ItemTrace is an item's trace to the goals it serves.
type ItemTrace struct {
	Item          KItem          `json:"item"`
	Grounding     string         `json:"grounding"`
	Words         string         `json:"words"`
	Scoped        bool           `json:"scoped"` // an approved goal covers its project
	Person        bool           `json:"person"` // a person wrote or approved the item itself
	PersonHops    *int           `json:"person_hops"`
	PersonTouches int            `json:"person_touches"`
	Routes        []TraceRoute   `json:"routes"`
	PersonRoute   []TraceHop     `json:"person_route,omitempty"` // to the nearest person's item, when it isn't one
	Candidate     *GoalCandidate `json:"candidate,omitempty"`
	Through       []*ItemTrace   `json:"through,omitempty"` // a pull request's or commit's tasks
	Note          string         `json:"note,omitempty"`
}

// TraceQuery names what to trace: an item (Ref), a pull request (Pull, in
// Project, Repo narrowing it), or a commit (Commit, with the task Keys its
// message names and the pull request number it gives, if any).
type TraceQuery struct {
	Ref     string
	Project string
	Repo    string
	Pull    int
	Commit  string
	Keys    []string
}

// maxTraceRoutes is the most routes one trace shows.
const maxTraceRoutes = 3

// Trace is the live trace of an item, pull request or commit.
func (s *Service) Trace(ctx context.Context, actor *Actor, q TraceQuery) (*ItemTrace, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	s.goalsSettle(ctx)
	tr := &tracer{s: s, actor: actor, items: map[gnode]*KItem{}, users: map[int64]*UserRef{}}
	switch {
	case q.Commit != "":
		return tr.commit(ctx, q)
	case q.Pull > 0:
		return tr.pull(ctx, q.Project, q.Repo, q.Pull)
	case strings.TrimSpace(q.Ref) != "":
		n, err := s.traceNode(ctx, actor, strings.TrimSpace(q.Ref), q.Project)
		if err != nil {
			return nil, err
		}
		return tr.item(ctx, n)
	}
	return nil, invalid("trace an issue, page or design note, a pull request or a commit")
}

// traceNode resolves a ref: an issue key, a design note's code or a page's.
func (s *Service) traceNode(ctx context.Context, actor *Actor, ref, project string) (gnode, error) {
	if i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false); err == nil {
		return gnode{KIssue, i.ID}, nil
	}
	if id, err := s.ResolveDesign(ctx, actor, ref, project); err == nil {
		return gnode{KDesign, id}, nil
	}
	if id, err := s.ResolvePage(ctx, actor, ref, project); err == nil {
		return gnode{KPage, id}, nil
	}
	return gnode{}, notFound(strconvQuote(ref))
}

type tracer struct {
	s      *Service
	actor  *Actor
	g      *goalGraph
	ground map[gnode]*gtrace
	items  map[gnode]*KItem
	users  map[int64]*UserRef
	m      *goalMatcher
}

func (tr *tracer) load(ctx context.Context) error {
	if tr.g != nil {
		return nil
	}
	g, err := loadGoalGraph(ctx, tr.s.db.R)
	if err != nil {
		return err
	}
	tr.g, tr.ground = g, g.groundings()
	return nil
}

// kitem describes a node, or says it is hidden from the actor.
func (tr *tracer) kitem(ctx context.Context, n gnode) KItem {
	if it, ok := tr.items[n]; ok {
		return *it
	}
	it, err := tr.s.kitem(ctx, tr.actor, n.typ, n.id, "")
	if err != nil {
		it = &KItem{Type: n.typ, Title: "(an item you can't see)"}
	}
	tr.items[n] = it
	return *it
}

func (tr *tracer) hop(ctx context.Context, from gnode, e gedge) TraceHop {
	h := TraceHop{From: tr.kitem(ctx, from), To: tr.kitem(ctx, e.to), Kind: e.kind, Basis: basisNames[e.basis], Agent: e.agent, LinkID: e.link}
	if e.score > 0 && e.basis == basisInferred {
		sc := e.score
		h.Score = &sc
	}
	if e.by != 0 {
		if u, ok := tr.users[e.by]; ok {
			h.By = u
		} else {
			h.By = tr.s.userRefByID(ctx, tr.s.db.R, sql.NullInt64{Int64: e.by, Valid: true})
			tr.users[e.by] = h.By
		}
	}
	return h
}

// traceStep is one edge of a route, with the node it leaves.
type traceStep struct {
	from gnode
	e    gedge
}

// paths walks forward from start over edges at least minBasis strong and
// returns the routes to the nearest targets, nearest first.
func (tr *tracer) paths(start gnode, minBasis int8, target func(gnode) bool, limit int) [][]traceStep {
	prev := map[gnode]traceStep{}
	seen := map[gnode]bool{start: true}
	frontier := []gnode{start}
	var found []gnode
	for len(frontier) > 0 && len(found) < limit {
		var next []gnode
		for _, f := range frontier {
			for _, e := range tr.g.out[f] {
				if e.basis < minBasis || seen[e.to] {
					continue
				}
				seen[e.to] = true
				prev[e.to] = traceStep{f, e}
				if target(e.to) {
					found = append(found, e.to)
				}
				next = append(next, e.to)
			}
		}
		frontier = next
	}
	var out [][]traceStep
	for _, t := range found[:min(limit, len(found))] {
		var p []traceStep
		for n := t; n != start; n = prev[n].from {
			p = append(p, prev[n])
		}
		slices.Reverse(p)
		out = append(out, p)
	}
	return out
}

func (tr *tracer) item(ctx context.Context, n gnode) (*ItemTrace, error) {
	if err := tr.load(ctx); err != nil {
		return nil, err
	}
	it := tr.g.items[n]
	out := &ItemTrace{Item: tr.kitem(ctx, n), Routes: []TraceRoute{}}
	if it == nil {
		out.Grounding, out.Words = GroundNone, GroundingWords(GroundNone)
		out.Note = "drafts, personal pages and retired notes aren't traced"
		return out, nil
	}
	t := tr.ground[n]
	out.Grounding, out.Words, out.Scoped, out.Person, out.PersonTouches = t.grounding, GroundingWords(t.grounding), t.scoped, it.person, t.touches
	if t.personHops >= 0 {
		ph := t.personHops
		out.PersonHops = &ph
	}
	if it.goal {
		out.Note = "this is a goal"
	}
	seenGoal := map[gnode]bool{}
	passes := []struct {
		approved  bool
		min       int8
		grounding string
	}{{true, basisPerson, GroundPersonGoal}, {true, basisAgent, GroundAgentGoal}, {true, basisInferred, GroundInferredGoal}, {false, basisInferred, GroundProposedGoal}}
	for _, p := range passes {
		if len(out.Routes) >= maxTraceRoutes {
			break
		}
		target := func(x gnode) bool {
			gi := tr.g.items[x]
			return gi != nil && gi.goal && gi.approved == p.approved && !seenGoal[x]
		}
		for _, path := range tr.paths(n, p.min, target, maxTraceRoutes-len(out.Routes)) {
			goal := path[len(path)-1].e.to
			seenGoal[goal] = true
			r := TraceRoute{Goal: tr.kitem(ctx, goal), Grounding: p.grounding, Hops: []TraceHop{}}
			for _, st := range path {
				r.Hops = append(r.Hops, tr.hop(ctx, st.from, st.e))
			}
			out.Routes = append(out.Routes, r)
		}
	}
	if !it.person {
		person := func(x gnode) bool { gi := tr.g.items[x]; return gi != nil && gi.person }
		if ps := tr.paths(n, basisInferred, person, 1); len(ps) > 0 {
			for _, st := range ps[0] {
				out.PersonRoute = append(out.PersonRoute, tr.hop(ctx, st.from, st.e))
			}
		}
	}
	if n.typ == KIssue && t.scoped && (t.grounding == GroundNone || t.grounding == GroundPersonRequest || t.grounding == GroundProposedGoal) {
		if tr.m == nil {
			m, err := tr.s.newGoalMatcher(ctx)
			if err != nil {
				return nil, err
			}
			tr.m = m
		}
		if len(tr.m.goals) > 0 {
			var title, desc string
			if err := tr.s.db.R.QueryRowContext(ctx, `SELECT title, description FROM issues WHERE id = ?`, n.id).Scan(&title, &desc); err == nil {
				out.Candidate = tr.s.goalCandidate(ctx, tr.actor, tr.m, n.id, it.project, title, desc)
			}
		}
	}
	return out, nil
}

// through traces each task. The headline is the weakest of them: work
// carried with an ungrounded task is only as grounded as that task, so
// one well-grounded task can't vouch for the rest (WP-ADR50). Each task's
// own trace is in Through.
func (tr *tracer) through(ctx context.Context, out *ItemTrace, ids []int64) error {
	worst, best := 0, len(Groundings)-1
	for _, id := range ids {
		t, err := tr.item(ctx, gnode{KIssue, id})
		if err != nil {
			return err
		}
		out.Through = append(out.Through, t)
		out.Scoped = out.Scoped || t.Scoped
		if i := slices.Index(Groundings, t.Grounding); i >= 0 {
			worst, best = max(worst, i), min(best, i)
		}
	}
	out.Grounding = Groundings[worst]
	out.Words = GroundingWords(out.Grounding)
	if best < worst {
		out.Note = strings.TrimSpace(out.Note + " its tasks trace differently; this is the weakest, each task's own is below.")
	}
	return nil
}

func (tr *tracer) pull(ctx context.Context, project, repo string, number int) (*ItemTrace, error) {
	if project == "" {
		return nil, invalid("name the pull request's project")
	}
	id, err := tr.s.FindPullRequest(ctx, tr.actor, project, repo, number)
	if err != nil {
		return nil, err
	}
	return tr.pullByID(ctx, id, "")
}

func (tr *tracer) pullByID(ctx context.Context, id int64, note string) (*ItemTrace, error) {
	var repo, title, url string
	var number int64
	if err := tr.s.db.R.QueryRowContext(ctx, `SELECT repo, number, title, url FROM pull_requests WHERE id = ?`, id).Scan(&repo, &number, &title, &url); err != nil {
		return nil, err
	}
	links, err := tr.s.prLinks(ctx, tr.actor, []any{id})
	if err != nil {
		return nil, err
	}
	out := &ItemTrace{Item: KItem{Type: KURL, Kind: "pull", Ref: url, Repo: repo, Number: &number, Title: repo + " PR #" + itoa64(number) + ": " + title},
		Routes: []TraceRoute{}, Note: note}
	var ids []int64
	for _, l := range links {
		if !slices.Contains(ids, l.issue.ID) {
			ids = append(ids, l.issue.ID)
		}
	}
	if len(ids) == 0 {
		out.Grounding, out.Words = GroundNone, GroundingWords(GroundNone)
		out.Note = strings.TrimSpace(note + " no task carries it")
		return out, nil
	}
	return out, tr.through(ctx, out, ids)
}

func (tr *tracer) commit(ctx context.Context, q TraceQuery) (*ItemTrace, error) {
	sha := strings.ToLower(strings.TrimSpace(q.Commit))
	if len(sha) < 7 || strings.Trim(sha, "0123456789abcdef") != "" {
		return nil, invalid("a commit is at least 7 hex characters")
	}
	short := sha[:min(len(sha), 10)]
	item := KItem{Type: KURL, Kind: "commit", Title: "commit " + short, Repo: q.Repo}
	vis, args := prVisible(tr.actor, 0)
	var prID int64
	// #nosec G202 -- vis is a fixed condition; values are bound
	err := tr.s.db.R.QueryRowContext(ctx, `SELECT p.id FROM pull_requests p WHERE (p.merge_sha LIKE ? OR p.head_sha LIKE ?) AND `+vis+` ORDER BY p.merge_sha LIKE ? DESC, p.id DESC LIMIT 1`,
		append([]any{sha + "%", sha + "%", sha + "%"}, args...)...).Scan(&prID)
	if errors.Is(err, sql.ErrNoRows) && q.Pull > 0 && q.Project != "" {
		prID, err = tr.s.FindPullRequest(ctx, tr.actor, q.Project, q.Repo, q.Pull)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var viaPR *ItemTrace
	if err == nil {
		if viaPR, err = tr.pullByID(ctx, prID, "commit "+short+" through the pull request that merged it;"); err != nil {
			return nil, err
		}
		if len(viaPR.Through) > 0 {
			return viaPR, nil
		}
	}
	var ids []int64
	for _, k := range q.Keys {
		if i, _, err := tr.s.getIssueFor(ctx, tr.s.db.R, tr.actor, k, RoleViewer, false); err == nil && !slices.Contains(ids, i.ID) {
			ids = append(ids, i.ID)
		}
	}
	if len(ids) > 0 {
		out := &ItemTrace{Item: item, Routes: []TraceRoute{}, Note: "through the tasks its message names (as the message says; no pull request Waypoint knows records them)"}
		return out, tr.through(ctx, out, ids)
	}
	if viaPR != nil {
		return viaPR, nil
	}
	return &ItemTrace{Item: item, Grounding: GroundNone, Words: GroundingWords(GroundNone), Routes: []TraceRoute{},
		Note: "its message names no task and no pull request Waypoint knows carries it"}, nil
}

// ConfirmGoalLink makes an agent's link or Waypoint's guess the person's
// own: it counts as a person's link from then on.
func (s *Service) ConfirmGoalLink(ctx context.Context, actor *Actor, id int64) (*Link, error) {
	if err := actor.requirePersonDirect("confirming a link"); err != nil {
		return nil, err
	}
	var src KItem
	err := s.write(ctx, actor, func(t *tx) error {
		var auto bool
		if err := t.QueryRowContext(ctx, `SELECT src_type, src_id, auto FROM knowledge_links WHERE id = ?`, id).Scan(&src.Type, &src.ID, &auto); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("link")
			}
			return err
		}
		if auto {
			return invalid("this link comes from a [[wikilink]] in the text; it counts as the text's author's")
		}
		if _, err := s.canEditSource(ctx, t, actor, src.Type, src.ID); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `UPDATE knowledge_links SET confirmed_by = ?, confirmed_at = ? WHERE id = ? AND confirmed_by IS NULL`, actor.id(), s.nowMS(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.publishLinkChange(ctx, src)
	return s.linkByID(ctx, actor, src, id)
}

// DismissGoalLink removes an agent's link or Waypoint's guess to a goal and
// keeps the no, so Waypoint never guesses it again.
func (s *Service) DismissGoalLink(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.requirePersonDirect("dismissing a link"); err != nil {
		return err
	}
	var src KItem
	err := s.write(ctx, actor, func(t *tx) error {
		var auto, personText bool
		var dstType string
		var dstID, confirmed sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT src_type, src_id, auto, dst_type, dst_id, confirmed_by, created_by IS NOT NULL AND created_agent = '' AND basis = ''
			FROM knowledge_links WHERE id = ?`, id).Scan(&src.Type, &src.ID, &auto, &dstType, &dstID, &confirmed, &personText); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("link")
			}
			return err
		}
		if _, err := s.canEditSource(ctx, t, actor, src.Type, src.ID); err != nil {
			return err
		}
		if auto {
			if personText || dstType != KDesign || !dstID.Valid {
				return invalid("a person's [[wikilink]] in the text makes this link; edit the text to remove it")
			}
			was, err := readDismissedLink(ctx, t, id)
			if err != nil {
				return err
			}
			return s.recordGoalDismissal(ctx, t, actor, src.Type, src.ID, dstID.Int64, was)
		}
		was, err := readDismissedLink(ctx, t, id)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM knowledge_links WHERE id = ?`, id); err != nil {
			return err
		}
		if dstType == KDesign && dstID.Valid {
			return s.recordGoalDismissal(ctx, t, actor, src.Type, src.ID, dstID.Int64, was)
		}
		return nil
	})
	if err == nil {
		s.publishLinkChange(ctx, src)
	}
	return err
}

// DismissGoalMatch says an item doesn't serve a goal Waypoint offered for
// it, without a link to remove.
func (s *Service) DismissGoalMatch(ctx context.Context, actor *Actor, item LinkEnd, goal int64) error {
	if err := actor.requirePersonDirect("dismissing a suggestion"); err != nil {
		return err
	}
	var src KItem
	err := s.write(ctx, actor, func(t *tx) error {
		typ, id, _, err := s.end(ctx, t, item, nil)
		if err != nil {
			return err
		}
		if _, err := s.canEditSource(ctx, t, actor, typ, id); err != nil {
			return err
		}
		if _, _, err := s.designFor(ctx, t, actor, goal, RoleViewer); err != nil {
			return err
		}
		src = KItem{Type: typ, ID: id}
		return s.recordGoalDismissal(ctx, t, actor, typ, id, goal, nil)
	})
	if err == nil {
		s.publishLinkChange(ctx, src)
	}
	return err
}

// AcknowledgeGoalLinks stops asking about links waiting for a person
// without confirming them: a bulk "stop asking" is not a person standing
// behind each link, so each keeps its basis (an agent's link stays an
// agent's) and only leaves the waiting list. It returns how many it
// acknowledged; links already confirmed, acknowledged or gone are skipped.
func (s *Service) AcknowledgeGoalLinks(ctx context.Context, actor *Actor, ids []int64) (int, error) {
	if err := actor.requirePersonDirect("acknowledging links"); err != nil {
		return 0, err
	}
	if len(ids) == 0 || len(ids) > 500 {
		return 0, invalid("acknowledge 1 to 500 links at a time")
	}
	n := 0
	var srcs []KItem
	err := s.write(ctx, actor, func(t *tx) error {
		for _, id := range ids {
			var src KItem
			if err := t.QueryRowContext(ctx, `SELECT src_type, src_id FROM knowledge_links WHERE id = ? AND rel = 'serves' AND dst_type = 'design'
				AND confirmed_by IS NULL AND acknowledged_at IS NULL`, id).Scan(&src.Type, &src.ID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			if _, err := s.canEditSource(ctx, t, actor, src.Type, src.ID); err != nil {
				continue
			}
			if _, err := t.ExecContext(ctx, `UPDATE knowledge_links SET acknowledged_by = ?, acknowledged_at = ? WHERE id = ?`, actor.id(), s.nowMS(), id); err != nil {
				return err
			}
			n++
			srcs = append(srcs, src)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, src := range srcs {
		s.publishLinkChange(ctx, src)
	}
	return n, nil
}

// linkByID is one of an item's links as the actor sees it.
func (s *Service) linkByID(ctx context.Context, actor *Actor, src KItem, id int64) (*Link, error) {
	links, err := s.ItemLinks(ctx, actor, LinkEnd{Type: src.Type, ID: src.ID})
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.ID == id && l.Direction == "out" {
			return l, nil
		}
	}
	return nil, notFound("link")
}

// goalRewordings fills each goal's newest pending edit to its title, and
// the why of each goal Waypoint suggested.
func (s *Service) goalRewordings(ctx context.Context, ids []any, byID map[int64]*GoalView) error {
	if len(ids) == 0 {
		return nil
	}
	for _, g := range byID {
		if g.Note.Approved || g.Note.CreatedAgent != goalSuggestAgent {
			continue
		}
		var body string
		if err := s.db.R.QueryRowContext(ctx, `SELECT body FROM design_notes WHERE id = ?`, g.Note.ID).Scan(&body); err != nil {
			return err
		}
		g.Why, _, _ = strings.Cut(body, "\n\n")
	}
	// #nosec G202 -- placeholders only
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.note_id, r.title, r.agent FROM design_revisions r JOIN design_notes n ON n.id = r.note_id
		WHERE r.kind = 'edit' AND r.state = 'pending' AND r.title <> n.title AND r.note_id IN (`+idList(len(ids))+`)
		ORDER BY r.id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rw GoalRewording
		var note int64
		if err := rows.Scan(&rw.RevisionID, &note, &rw.Title, &rw.Agent); err != nil {
			return err
		}
		if g := byID[note]; g != nil {
			g.Rewording = &rw
		}
	}
	return rows.Err()
}

// IssueGoal is an issue's stored trace to a goal, as wp show and wp next
// give it: the grounding and the goal it reaches, or Pending while
// Waypoint hasn't traced it yet (the trace runs in the background after
// each change; wp trace traces it now).
type IssueGoal struct {
	Grounding string `json:"grounding,omitempty"`
	Goal      *KItem `json:"goal,omitempty"`
	Pending   bool   `json:"pending,omitempty"`
	// Chain is the goal's parent goals the task's arm shows, nearest first;
	// Above counts the parents left out (covered or past the length, wherever they sit); Arm is the arm (WP-1035,
	// goal_context.go). Empty for the goal arm.
	Chain []GoalStep `json:"chain,omitempty"`
	Above int        `json:"above,omitempty"`
	Arm   string     `json:"-"` // never sent: an agent must not learn its arm
}

func (s *Service) issueGoal(ctx context.Context, q queryer, actor *Actor, issueID, projectID int64) *IssueGoal {
	var g string
	var goal sql.NullInt64
	var scoped bool
	err := q.QueryRowContext(ctx, `SELECT grounding, goal_id, scoped FROM goal_traces WHERE item_type = 'issue' AND item_id = ?`, issueID).Scan(&g, &goal, &scoped)
	switch {
	case err == nil && !scoped:
		return nil
	case err == nil:
		out := &IssueGoal{Grounding: g}
		if goal.Valid {
			if it, err := s.kitem(ctx, actor, KDesign, goal.Int64, ""); err == nil {
				out.Goal = it
			}
		}
		return out
	case s.projectHasGoal(ctx, q, projectID):
		return &IssueGoal{Pending: true}
	}
	return nil
}

// outsideTitleInPrompt stands for the title of a linked issue whose text
// came from outside and no person has released: a model's prompt never
// carries it (WP-281, WP-1788).
const outsideTitleInPrompt = "(title not shown: written from outside, not yet approved)"

// quoteTitle puts a title in double quotes it can't end early: its own
// double quotes become single ones. Go's %q would escape them as \", and a
// reader that doesn't take a backslash as an escape sees the quote end there.
func quoteTitle(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `'`) + `"`
}

// reviewIntent is what the pull request's tasks are for (WP-382): each
// task, the goal it traces to and how firmly, and the factors of its
// provenance risk, so a reviewer can tell work no one asked for. "" with no
// task.
func (s *Service) reviewIntent(ctx context.Context, prID int64) string {
	sys := SystemActor()
	links, err := s.prLinks(ctx, sys, []any{prID})
	if err != nil {
		return ""
	}
	var b strings.Builder
	seen := map[int64]bool{}
	for _, l := range links {
		if seen[l.issue.ID] || len(seen) >= 10 {
			continue
		}
		seen[l.issue.ID] = true
		var project int64
		var outside bool
		if err := s.db.R.QueryRowContext(ctx, `SELECT project_id, outside FROM issues WHERE id = ?`, l.issue.ID).Scan(&project, &outside); err != nil {
			outside = true
		}
		// Quoted, so a title can't pass for the goal or risk clause after it.
		title := quoteTitle(kernel.Excerpt(oneLine(l.issue.Title), 160))
		if outside {
			title = outsideTitleInPrompt
		}
		fmt.Fprintf(&b, "- %s %s", l.issue.Key, title)
		switch g := s.issueGoal(ctx, s.db.R, sys, l.issue.ID, project); {
		case g == nil:
		case g.Pending:
			b.WriteString("; not traced to a goal yet")
		case g.Goal != nil:
			fmt.Fprintf(&b, "; serves goal %s, %s", quoteTitle(kernel.Excerpt(oneLine(g.Goal.Title), 160)), GroundingWords(g.Grounding))
		default:
			b.WriteString("; " + GroundingWords(g.Grounding))
		}
		if r, err := s.issueRiskByID(ctx, s.db.R, sys, l.issue.ID); err == nil && r.Level != RiskLow {
			var fs []string
			for _, x := range r.Reasons {
				if f := strings.ReplaceAll(x.Factor, "_", " "); !slices.Contains(fs, f) {
					fs = append(fs, f)
				}
			}
			fmt.Fprintf(&b, "; risk %s (%s)", r.Level, strings.Join(fs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// goalOverlapClip: how much of the other goal's title a suggestion quotes.
const goalOverlapClip = 60

// goalOverlaps adds wording suggestions to goals judged to restate their
// parent or to overlap another goal (goal_meaning.go, WP-1035,
// WP-ADR133). It reads the stored judgements and asks nothing: one counts
// only while the titles it was made on are unchanged, and with none nothing
// is suggested. They are suggestions only: a goal is never edited, and its
// title is quoted clipped, never reworded. Each pair is reported once, on
// the newer goal; a goal and its ancestor are never reported as
// overlapping. It returns the pairs it suggested, which the evaluator
// follows to see what people did about them (goal_meaning_labels.go).
func (s *Service) goalOverlaps(ctx context.Context, goals []*GoalView) ([]meaningShown, error) {
	if len(goals) == 0 {
		return nil, nil
	}
	byID := map[int64]*GoalView{}
	ids := make([]any, len(goals))
	for i, g := range goals {
		byID[g.Note.ID] = g
		ids[i] = g.Note.ID
	}
	hash := func(g *GoalView) string { return goalTitleHash(g.Note.Title) }
	quote := func(g *GoalView) string { return "“" + clipText(g.Note.Title, goalOverlapClip) + "”" }
	why := func(w string) string {
		if w == "" {
			return ""
		}
		return " (the goals model: " + w + ")"
	}
	list := idList(len(ids))
	// #nosec G202 -- placeholders only; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT judgement, a_id, b_id, a_hash, b_hash, why FROM goal_meaning_pairs
		WHERE yes = 1 AND judgement IN ('restates', 'overlap') AND a_id IN (`+list+`) AND b_id IN (`+list+`)
		ORDER BY judgement DESC, b_id, a_id`, append(slices.Clone(ids), ids...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shown []meaningShown
	for rows.Next() {
		var m meaningShown
		var w string
		if err := rows.Scan(&m.judgement, &m.a, &m.b, &m.aHash, &m.bHash, &w); err != nil {
			return nil, err
		}
		ga, gb := byID[m.a], byID[m.b]
		if ga == nil || gb == nil || hash(ga) != m.aHash || hash(gb) != m.bHash {
			continue
		}
		if ga.Note.ParentID != nil {
			m.aParent = *ga.Note.ParentID
		}
		if gb.Note.ParentID != nil {
			m.bParent = *gb.Note.ParentID
		}
		switch m.judgement {
		case judgeRestates:
			if ga.Note.ParentID == nil || *ga.Note.ParentID != m.b {
				continue
			}
			ga.Wording = append(ga.Wording, fmt.Sprintf("repeats its parent goal #%d (%s): say what this one adds, or fold it into that goal%s", gb.Note.ID, quote(gb), why(w)))
		case judgeOverlap:
			if m.a >= m.b || (ga.Note.ParentID != nil && *ga.Note.ParentID == m.b) || (gb.Note.ParentID != nil && *gb.Note.ParentID == m.a) {
				continue
			}
			gb.Wording = append(gb.Wording, fmt.Sprintf("overlaps goal #%d (%s): merge them, or say how they differ%s", ga.Note.ID, quote(ga), why(w)))
		default:
			continue
		}
		shown = append(shown, m)
	}
	return shown, rows.Err()
}
