package core

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/semantic"
)

// Kinds of node beyond the knowledge kinds.
const (
	KSession = "session"
)

// graphTypes are the kinds of node the graph holds.
var graphTypes = []string{KIssue, KPage, KDesign, KGlossary, KFile, KPull, KSession}

// graphEdgeKind is one kind of edge: its name read from its source, read
// from its target, and its length.
type graphEdgeKind struct {
	name, inverse string
	length        float64
}

// Edge kinds. The order is the wire order of nothing; names are what the API says.
var graphKinds = map[string]graphEdgeKind{
	"subtask_of":   {"subtask of", "parent of", 1},
	"relates":      {"relates to", "", 1.5},
	"blocks":       {"blocks", "blocked by", 1},
	"duplicates":   {"duplicates", "duplicated by", 0.5},
	"follows":      {"follows", "followed by", 1},
	"supersedes":   {"supersedes", "superseded by", 1},
	"tension":      {"in tension with", "", 1},
	"serves":       {"serves", "served by", 1},
	"implements":   {"implements", "implemented by", 1},
	"depends_on":   {"depends on", "needed by", 1},
	"links":        {"links to", "linked from", 1.5},
	"mentions":     {"mentions", "mentioned by", 1.5},
	"under":        {"under", "above", 1},
	"written_for":  {"written for", "has the note", 1},
	"documents":    {"documents", "documented by", 1.2},
	"filed_from":   {"filed while working on", "led to", 1.5},
	"worked_in":    {"worked in", "works on", 1},
	"named_by":     {"named by", "names", 2},
	"changes":      {"changes", "changed by", 1.5},
	"session_task": {"worked on", "worked on by", 1},
	"session_ref":  {"filed, read or named", "in the session", 2},
	"similar":      {"alike in words to", "", 1},
}

// Basis lengths: a guess counts longer than an agent's link, an agent's
// than a person's.
var graphBasisFactor = [...]float64{basisInferred: 1.5, basisAgent: 1.2, basisPerson: 1}

// graphHubWeight is what passing through a node costs per doubling of its
// links.
const graphHubWeight = 0.5

// Bounds that keep a query fast and its answer readable.
const (
	graphMaxHops      = 6
	graphMaxVisited   = 20000
	graphSimPerNode   = 5
	graphSimCands     = 40
	graphSimBudget    = 400
	graphMaxPRIssues  = 6
	graphMaxPRFiles   = 60
	graphMaxSessRefs  = 30
	graphDefaultLimit = 40
)

// gnodeRec is one node.
type gnodeRec struct {
	typ     string
	id      int64
	ref     string
	key     string
	title   string
	kind    string
	state   string
	project int64
	user    int64
	url     string
}

// gedgeRec is one edge, stored once and walked both ways.
type gedgeRec struct {
	a, b  int32
	kind  string
	sub   string
	basis int8
	score float32
	why   string
}

// relGraph is the instance's graph, rebuilt when what it reads changes.
type relGraph struct {
	sig   string
	built time.Time
	nodes []gnodeRec
	index map[string]int32
	edges []gedgeRec
	adj   [][]int32
	issue map[int64]int32
	keys  map[int64]string
}

// graphCache holds the built graph and the issues' likeness edges, which
// last as long as the similarity index they came from.
type graphCache struct {
	mu    sync.Mutex
	g     *relGraph
	simMu sync.Mutex
	simAt string
	sim   map[int64][]simMatch
}

func nodeID(typ string, id int64, ref string) string {
	if ref != "" {
		return typ + ":" + ref
	}
	return typ + ":" + strconv.FormatInt(id, 10)
}

func (g *relGraph) addNode(n gnodeRec) int32 {
	id := nodeID(n.typ, n.id, n.ref)
	if i, ok := g.index[id]; ok {
		return i
	}
	i := int32(len(g.nodes)) // #nosec G115 -- 2^31 nodes would not fit in memory long before
	g.nodes = append(g.nodes, n)
	g.adj = append(g.adj, nil)
	g.index[id] = i
	if n.typ == KIssue {
		g.issue[n.id] = i
	}
	return i
}

func (g *relGraph) node(typ string, id int64) (int32, bool) {
	i, ok := g.index[nodeID(typ, id, "")]
	return i, ok
}

func (g *relGraph) addEdge(e gedgeRec) {
	if e.a == e.b || e.a < 0 || e.b < 0 {
		return
	}
	n := int32(len(g.edges)) // #nosec G115 -- 2^31 edges would not fit in memory long before
	g.edges = append(g.edges, e)
	g.adj[e.a] = append(g.adj[e.a], n)
	g.adj[e.b] = append(g.adj[e.b], n)
}

// graphSig changes whenever anything the graph reads does. Every part is an
// aggregate over a table the graph reads, so it costs a few index scans.
// Writes that skip updated_at are covered by the columns themselves: a
// pull request's files (loadPRFiles), a note's approval, a status's
// category, a session's title, state and project, a project's key.
const graphSigSQL = `SELECT
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) || ':' || coalesce(sum(id), 0) FROM issues),
	(SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(sum(source_id * 7 + target_id * 13), 0) || ':' || coalesce(sum(length(kind) + length(subkind) + 3 * length(coalesce(basis, '-'))), 0) FROM issue_relations),
	(SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(max(coalesce(confirmed_at, created_at)), 0) FROM knowledge_links),
	(SELECT count(*) || ':' || coalesce(sum(page_id * 7 + issue_id), 0) FROM page_issues),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) FROM pages),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) || ':' || coalesce(max(approved_at), 0) FROM design_notes),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) FROM glossary),
	(SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(max(synced_at), 0) FROM external_links),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) FROM forge_mentions),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) || ':' || coalesce(sum(length(files)), 0) FROM pull_requests),
	(SELECT count(*) || ':' || coalesce(max(at), 0) FROM session_issue_refs),
	(SELECT count(*) || ':' || coalesce(max(id), 0) || ':' || coalesce(sum(coalesce(issue_id, 0)), 0) || ':' ||
		coalesce(sum(coalesce(project_id, 0) * 31 + length(title) * 7 + unicode(substr(state, 3))), 0) FROM agent_sessions),
	(SELECT count(*) || ':' || coalesce(max(updated_at), 0) || ':' || coalesce(group_concat(id || key), '') FROM projects),
	(SELECT count(*) || ':' || coalesce(group_concat(project_id || lower(repo)), '') FROM project_repos),
	(SELECT count(*) || ':' || coalesce(group_concat(id || category), '') FROM statuses)`

// relationGraph returns the graph, rebuilt if anything it reads changed.
func (s *Service) relationGraph(ctx context.Context) (*relGraph, error) {
	var parts [15]string
	ptrs := make([]any, len(parts))
	for i := range parts {
		ptrs[i] = &parts[i]
	}
	if err := s.db.R.QueryRowContext(ctx, graphSigSQL).Scan(ptrs...); err != nil {
		return nil, err
	}
	sig := strings.Join(parts[:], "|")
	s.graph.mu.Lock()
	defer s.graph.mu.Unlock()
	if s.graph.g != nil && s.graph.g.sig == sig {
		return s.graph.g, nil
	}
	g, err := loadRelGraph(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	g.sig = sig
	s.graph.g = g
	return g, nil
}

// relationBasisOf is who stands behind an issue_relations row: NULL is a
// relation made before bases were kept (WP-ADR105), counted as an agent's.
func relationBasisOf(b sql.NullString) int8 {
	switch {
	case !b.Valid:
		return basisAgent
	case b.String == "":
		return basisPerson
	case isGuess(b.String):
		return basisInferred
	}
	return basisAgent
}

// loadRelGraph reads the graph. Drafts, personal pages and retired notes
// stay out, as they do from the goal trace.
func loadRelGraph(ctx context.Context, q queryer) (*relGraph, error) {
	g := &relGraph{built: time.Now(), index: map[string]int32{}, issue: map[int64]int32{}}
	each := func(query string, scan func(*sql.Rows) error, args ...any) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	projKey := map[int64]string{}
	g.keys = projKey
	if err := each(`SELECT id, key FROM projects`, func(r *sql.Rows) error {
		var id int64
		var k string
		err := r.Scan(&id, &k)
		projKey[id] = k
		return err
	}); err != nil {
		return nil, err
	}
	type pair struct{ a, b int64 }
	var parents, origins []pair
	if err := each(`SELECT i.id, i.project_id, i.number, i.title, i.type, st.category, coalesce(i.parent_id, 0), i.parent_by IS NOT NULL,
		coalesce(i.origin_issue_id, 0)
		FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.draft = 0 ORDER BY i.id`, func(r *sql.Rows) error {
		var n gnodeRec
		var num, parent, origin int64
		var parentByPerson bool
		if err := r.Scan(&n.id, &n.project, &num, &n.title, &n.kind, &n.state, &parent, &parentByPerson, &origin); err != nil {
			return err
		}
		n.typ = KIssue
		n.key = fmt.Sprintf("%s-%d", projKey[n.project], num)
		n.url = "/issue/" + n.key
		g.addNode(n)
		if parent != 0 {
			b := int64(basisAgent)
			if parentByPerson {
				b = int64(basisPerson)
			}
			parents = append(parents, pair{n.id, parent*4 + b})
		}
		if origin != 0 {
			origins = append(origins, pair{n.id, origin})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, p := range parents {
		a, _ := g.node(KIssue, p.a)
		if b, ok := g.node(KIssue, p.b/4); ok {
			g.addEdge(gedgeRec{a: a, b: b, kind: "subtask_of", basis: int8(p.b % 4)})
		}
	}
	for _, p := range origins {
		a, _ := g.node(KIssue, p.a)
		if b, ok := g.node(KIssue, p.b); ok {
			g.addEdge(gedgeRec{a: a, b: b, kind: "filed_from", basis: basisAgent})
		}
	}
	if err := each(`SELECT id, project_id, category, number, title FROM pages WHERE draft = 0 AND project_id IS NOT NULL`, func(r *sql.Rows) error {
		var n gnodeRec
		var num sql.NullInt64
		if err := r.Scan(&n.id, &n.project, &n.kind, &num, &n.title); err != nil {
			return err
		}
		n.typ = KPage
		n.key = pageCode(n.kind, num)
		if n.key == "" {
			n.key = "page:" + itoa64(n.id)
		}
		n.url = "/pages/" + itoa64(n.id)
		g.addNode(n)
		return nil
	}); err != nil {
		return nil, err
	}
	var notes []struct {
		id, parent, issue  int64
		approved, byPerson bool
	}
	if err := each(`SELECT id, coalesce(project_id, 0), kind, number, title, status, approved_at IS NOT NULL, coalesce(parent_id, 0),
		CASE WHEN requested_by IS NOT NULL THEN coalesce(requested_issue_id, 0) ELSE 0 END,
		CASE WHEN parent_at IS NULL THEN approved_at IS NOT NULL OR requested_by IS NOT NULL ELSE parent_by IS NOT NULL END
		FROM design_notes WHERE status != 'retired'`, func(r *sql.Rows) error {
		var n gnodeRec
		var num sql.NullInt64
		var approved, byPerson bool
		var parent, issue int64
		if err := r.Scan(&n.id, &n.project, &n.kind, &num, &n.title, &n.state, &approved, &parent, &issue, &byPerson); err != nil {
			return err
		}
		if !approved {
			n.state = "proposed"
		}
		n.typ = KDesign
		n.key = designCodeOf(num)
		if n.key == "" {
			n.key = "#" + itoa64(n.id)
		}
		n.url = "/design/" + itoa64(n.id)
		g.addNode(n)
		notes = append(notes, struct {
			id, parent, issue  int64
			approved, byPerson bool
		}{n.id, parent, issue, approved, byPerson})
		return nil
	}); err != nil {
		return nil, err
	}
	for _, n := range notes {
		a, _ := g.node(KDesign, n.id)
		b := basisAgent
		if n.byPerson {
			b = basisPerson
		}
		if p, ok := g.node(KDesign, n.parent); ok {
			g.addEdge(gedgeRec{a: a, b: p, kind: "under", basis: b})
		}
		if i, ok := g.node(KIssue, n.issue); ok {
			g.addEdge(gedgeRec{a: a, b: i, kind: "written_for", basis: basisPerson})
		}
	}
	if err := each(`SELECT id, coalesce(project_id, 0), term FROM glossary`, func(r *sql.Rows) error {
		var n gnodeRec
		if err := r.Scan(&n.id, &n.project, &n.title); err != nil {
			return err
		}
		n.typ, n.key, n.url = KGlossary, n.title, "/glossary/"+itoa64(n.id)
		g.addNode(n)
		return nil
	}); err != nil {
		return nil, err
	}

	if err := each(`SELECT source_id, target_id, kind, subkind, basis, coalesce(score, 0) FROM issue_relations ORDER BY id`, func(r *sql.Rows) error {
		var src, dst int64
		var kind, sub string
		var basis sql.NullString
		var score float64
		if err := r.Scan(&src, &dst, &kind, &sub, &basis, &score); err != nil {
			return err
		}
		a, ok1 := g.node(KIssue, src)
		b, ok2 := g.node(KIssue, dst)
		if _, known := graphKinds[kind]; ok1 && ok2 && known {
			g.addEdge(gedgeRec{a: a, b: b, kind: kind, sub: sub, basis: relationBasisOf(basis), score: float32(score)})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT page_id, issue_id, created_agent FROM page_issues`, func(r *sql.Rows) error {
		var page, issue int64
		var agent string
		if err := r.Scan(&page, &issue, &agent); err != nil {
			return err
		}
		a, ok1 := g.node(KPage, page)
		b, ok2 := g.node(KIssue, issue)
		if ok1 && ok2 {
			bs := basisPerson
			if agent != "" {
				bs = basisAgent
			}
			g.addEdge(gedgeRec{a: a, b: b, kind: "documents", basis: bs})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT src_type, src_id, dst_type, coalesce(dst_id, 0), dst_ref, rel, auto, basis, coalesce(score, 0),
		coalesce(created_by, 0), created_agent, coalesce(confirmed_by, 0)
		FROM knowledge_links WHERE dst_type != 'url' ORDER BY id`, func(r *sql.Rows) error {
		var st, dt, ref, rel, basis, agent string
		var sid, did, by, confirmed int64
		var auto bool
		var score float64
		if err := r.Scan(&st, &sid, &dt, &did, &ref, &rel, &auto, &basis, &score, &by, &agent, &confirmed); err != nil {
			return err
		}
		a, ok := g.node(st, sid)
		if !ok {
			return nil
		}
		var b int32
		if dt == KFile {
			if ref = strings.TrimSpace(ref); ref == "" {
				return nil
			}
			pid := g.nodes[a].project
			b = g.addNode(gnodeRec{typ: KFile, ref: fileRef(pid, ref), key: ref, title: ref, project: pid})
		} else if b, ok = g.node(dt, did); !ok {
			return nil
		}
		kind := rel
		switch {
		case auto || rel == "mentions":
			kind = "mentions"
		case rel == "relates":
			kind = "links"
		}
		if _, known := graphKinds[kind]; !known {
			kind = "links"
		}
		g.addEdge(gedgeRec{a: a, b: b, kind: kind, basis: storedBasis(basis, agent, by, confirmed != 0), score: float32(score)})
		return nil
	}); err != nil {
		return nil, err
	}

	repoProj := map[string]int64{}
	if err := each(`SELECT lower(repo), project_id FROM project_repos ORDER BY id`, func(r *sql.Rows) error {
		var repo string
		var pid int64
		err := r.Scan(&repo, &pid)
		if _, seen := repoProj[repo]; !seen {
			repoProj[repo] = pid
		}
		return err
	}); err != nil {
		return nil, err
	}
	pullNode := func(url, repo, title, state string, number int64, issue int32) int32 {
		pid, ok := repoProj[strings.ToLower(repo)]
		if !ok {
			pid = g.nodes[issue].project
		}
		key := "#" + itoa64(number)
		if repo != "" {
			key = repo + key
		}
		if title == "" {
			title = "Pull request " + key
		}
		return g.addNode(gnodeRec{typ: KPull, ref: graphPullKey(url), key: key, title: title, state: state, project: pid, url: url})
	}
	type pullUse struct {
		issue  int32
		url    string
		own    bool
		agent  bool
		verb   string
		number int64
		repo   string
		title  string
		state  string
	}
	var uses []pullUse
	if err := each(`SELECT issue_id, url, repo, coalesce(number, 0), title, state, created_agent != '' FROM external_links WHERE kind = 'pull' ORDER BY id`, func(r *sql.Rows) error {
		var u pullUse
		var issue int64
		if err := r.Scan(&issue, &u.url, &u.repo, &u.number, &u.title, &u.state, &u.agent); err != nil {
			return err
		}
		i, ok := g.node(KIssue, issue)
		if !ok {
			return nil
		}
		u.issue, u.own = i, true
		uses = append(uses, u)
		return nil
	}); err != nil {
		return nil, err
	}
	// "Its text says": only trusted text's verbs (an outside row has no
	// excerpt), as on the pull request page (WP-1785).
	if err := each(`SELECT issue_id, item_url, repo, coalesce(number, 0), title, state, max(own), group_concat(DISTINCT CASE WHEN excerpt != '' THEN verb END)
		FROM forge_mentions WHERE item_kind = 'pull' AND gone_at IS NULL GROUP BY issue_id, item_url ORDER BY min(id)`, func(r *sql.Rows) error {
		var u pullUse
		var issue int64
		var verbs sql.NullString
		if err := r.Scan(&issue, &u.url, &u.repo, &u.number, &u.title, &u.state, &u.own, &verbs); err != nil {
			return err
		}
		i, ok := g.node(KIssue, issue)
		if !ok {
			return nil
		}
		u.issue, u.verb, u.agent = i, verbs.String, true
		uses = append(uses, u)
		return nil
	}); err != nil {
		return nil, err
	}
	named := map[string]map[int32]bool{}
	for _, u := range uses {
		k := graphPullKey(u.url)
		if named[k] == nil {
			named[k] = map[int32]bool{}
		}
		named[k][u.issue] = true
	}
	done := map[[2]string]bool{}
	for _, u := range uses {
		k := graphPullKey(u.url)
		if !u.own && len(named[k]) > graphMaxPRIssues {
			continue
		}
		ik := [2]string{k, strconv.Itoa(int(u.issue))}
		if done[ik] {
			continue
		}
		done[ik] = true
		pr := pullNode(u.url, u.repo, u.title, u.state, u.number, u.issue)
		kind, why := "named_by", ""
		if u.own {
			kind = "worked_in"
		}
		if u.verb != "" {
			why = "its text says " + strings.ReplaceAll(strings.Trim(u.verb, ","), ",", ", ")
		}
		b := basisPerson
		if u.agent {
			b = basisAgent
		}
		g.addEdge(gedgeRec{a: u.issue, b: pr, kind: kind, basis: b, why: why})
	}
	if err := each(`SELECT url, repo, files FROM pull_requests WHERE files != '' AND url != ''`, func(r *sql.Rows) error {
		var url, repo, raw string
		if err := r.Scan(&url, &repo, &raw); err != nil {
			return err
		}
		pr, ok := g.index[nodeID(KPull, 0, graphPullKey(url))]
		if !ok {
			return nil
		}
		var pf prFiles
		if json.Unmarshal([]byte(raw), &pf) != nil || pf.TooLarge || len(pf.Paths) == 0 || len(pf.Paths) > graphMaxPRFiles {
			return nil
		}
		pid := g.nodes[pr].project
		for _, p := range pf.Paths {
			f := g.addNode(gnodeRec{typ: KFile, ref: fileRef(pid, p), key: p, title: p, project: pid})
			g.addEdge(gedgeRec{a: pr, b: f, kind: "changes", basis: basisPerson})
		}
		return nil
	}); err != nil {
		return nil, err
	}

	type sessRef struct {
		sess, issue int64
		how         string
	}
	var refs []sessRef
	refCount := map[int64]int{}
	if err := each(`SELECT session_id, issue_id, how FROM session_issue_refs`, func(r *sql.Rows) error {
		var x sessRef
		if err := r.Scan(&x.sess, &x.issue, &x.how); err != nil {
			return err
		}
		refs = append(refs, x)
		refCount[x.sess]++
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT x.id, x.user_id, coalesce(x.project_id, 0), x.title, x.state, coalesce(x.issue_id, 0) FROM agent_sessions x
		WHERE x.issue_id IS NOT NULL OR x.id IN (SELECT session_id FROM session_issue_refs)`, func(r *sql.Rows) error {
		var n gnodeRec
		var issue int64
		if err := r.Scan(&n.id, &n.user, &n.project, &n.title, &n.state, &issue); err != nil {
			return err
		}
		if refCount[n.id] > graphMaxSessRefs {
			return nil
		}
		if n.title == "" {
			n.title = "Agent session " + itoa64(n.id)
		}
		n.typ, n.key, n.url = KSession, "session "+itoa64(n.id), "/agents/sessions/"+itoa64(n.id)
		x := g.addNode(n)
		if i, ok := g.node(KIssue, issue); ok {
			g.addEdge(gedgeRec{a: x, b: i, kind: "session_task", basis: basisAgent})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, r := range refs {
		x, ok1 := g.node(KSession, r.sess)
		i, ok2 := g.node(KIssue, r.issue)
		if ok1 && ok2 {
			g.addEdge(gedgeRec{a: x, b: i, kind: "session_ref", basis: basisAgent, why: r.how})
		}
	}
	return g, nil
}

// fileRef keys a file by the project it is known in.
func fileRef(project int64, path string) string {
	return itoa64(project) + ":" + strings.TrimPrefix(strings.TrimSpace(path), "./")
}

// graphPullKey keys a pull request by its URL.
func graphPullKey(url string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(url)), "/")
}

// GraphNode is one thing in the relation graph.
type GraphNode struct {
	Node    string `json:"node"` // the graph's id for it: issue:12, file:3:path, pull:URL
	Type    string `json:"type"` // issue, page, design, glossary, pull, file, session
	Ref     string `json:"ref"`  // the code to show: WP-12, PL3, ADR7, repo#4, a path
	Title   string `json:"title"`
	Kind    string `json:"kind,omitempty"`  // an issue's type, a page's category, a note's kind
	State   string `json:"state,omitempty"` // status category, PR state, note status
	Project string `json:"project,omitempty"`
	URL     string `json:"url,omitempty"` // an app path, or the forge's page
}

// GraphEdge is one link, read from From to To.
type GraphEdge struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Kind    string  `json:"kind"`              // as stored: blocks, serves, similar, …
	Label   string  `json:"label"`             // read from From: "blocked by"
	Subkind string  `json:"subkind,omitempty"` // fix, expansion
	Basis   string  `json:"basis"`             // person, agent, inferred
	Score   float64 `json:"score,omitempty"`   // likeness, where measured
	Why     string  `json:"why,omitempty"`
	Length  float64 `json:"length"` // what the step adds to the distance
}

// GraphHit is a node with how far it is from the one asked about.
type GraphHit struct {
	GraphNode
	Distance float64     `json:"distance"`
	Hops     int         `json:"hops"`
	Path     []GraphEdge `json:"path"` // from the center to it
}

// GraphView is what lies around one thing.
type GraphView struct {
	Center  GraphNode   `json:"center"`
	Nodes   []GraphHit  `json:"nodes"` // nearest first, the center left out
	Edges   []GraphEdge `json:"edges"` // every edge between the center and the nodes listed; Length without hub cost, which depends on the route
	Guesses bool        `json:"guesses"`
	More    bool        `json:"more"`           // more lay within reach than the limit allowed
	Size    *GraphSize  `json:"size,omitempty"` // admins only: it counts what others can't see
}

// GraphSize is how big the whole graph is.
type GraphSize struct {
	Nodes int       `json:"nodes"`
	Edges int       `json:"edges"`
	Built time.Time `json:"built"`
}

// GraphPath is the nearest route between two things.
type GraphPath struct {
	From     GraphNode   `json:"from"`
	To       GraphNode   `json:"to"`
	Found    bool        `json:"found"`
	Distance float64     `json:"distance"`
	Hops     int         `json:"hops"`
	Path     []GraphEdge `json:"path"`
	Nodes    []GraphNode `json:"nodes"` // along the path, from first to last
}

// GraphQuery asks what lies around Ref.
type GraphQuery struct {
	Ref     string
	Project string // where a bare code is looked up first
	Hops    int    // the most steps away (default 3)
	Limit   int    // the most nodes (default 40)
	Guesses bool   // walk Waypoint's guesses too: issues alike in words, links its matchers made
	Types   []string
	Open    bool // issues: only ones not done or canceled
}

// graphWalk is one query's view of the graph: what the actor may see, and
// the likeness edges it may score.
type graphWalk struct {
	s       *Service
	ctx     context.Context
	actor   *Actor
	g       *relGraph
	guesses bool
	idx     *simIndex
	budget  int
	roles   map[int64]bool
	shared  map[int64]bool
	issues  map[int32]bool
}

func (s *Service) newWalk(ctx context.Context, actor *Actor, guesses bool) (*graphWalk, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	g, err := s.relationGraph(ctx)
	if err != nil {
		return nil, err
	}
	w := &graphWalk{s: s, ctx: ctx, actor: actor, g: g, guesses: guesses, budget: graphSimBudget,
		roles: map[int64]bool{}, shared: map[int64]bool{}, issues: map[int32]bool{}}
	if guesses {
		if w.idx, err = s.similarityIndex(ctx); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// visible: the actor may read the node.
func (w *graphWalk) visible(n int32) bool {
	r := &w.g.nodes[n]
	if r.typ == KSession && r.user == w.actor.ID() && w.actor.Scope == nil {
		return true
	}
	if r.project == 0 {
		return r.typ != KSession || w.actor.IsAdmin()
	}
	ok, seen := w.roles[r.project]
	if !seen {
		role, err := w.s.projectRole(w.ctx, w.s.db.R, w.actor, r.project)
		ok = err == nil && role > RoleNone
		w.roles[r.project] = ok
		if !ok {
			w.shared[r.project] = w.s.hasShareInProject(w.ctx, w.s.db.R, w.actor, r.project)
		}
	}
	if ok || r.typ != KIssue || !w.shared[r.project] {
		return ok
	}
	v, seen := w.issues[n]
	if !seen {
		v = w.s.canSee(w.ctx, w.s.db.R, w.actor, r.id, r.project)
		w.issues[n] = v
	}
	return v
}

// gstep is one way out of a node.
type gstep struct {
	to   int32
	edge gedgeRec
	fwd  bool
}

// steps lists the ways out of n: its stored edges and, with guesses, the
// issues most alike to it.
func (w *graphWalk) steps(n int32) []gstep {
	var out []gstep
	for _, ei := range w.g.adj[n] {
		e := w.g.edges[ei]
		if e.basis == basisInferred && !w.guesses {
			continue
		}
		if e.a == n {
			out = append(out, gstep{e.b, e, true})
		} else {
			out = append(out, gstep{e.a, e, false})
		}
	}
	if w.guesses && w.g.nodes[n].typ == KIssue {
		for _, m := range w.alike(w.g.nodes[n].id) {
			if to, ok := w.g.issue[m.id]; ok {
				out = append(out, gstep{to, gedgeRec{a: n, b: to, kind: "similar", basis: basisInferred, score: float32(m.blend),
					why: w.idx.shared(w.idx.docs[w.g.nodes[n].id].vec, w.idx.docs[m.id].vec)}, true})
			}
		}
	}
	return out
}

// degree is how many links a node has that this walk could take, for the
// cost of passing through it: links to what the actor can't see, and
// stored guesses with guesses off, don't count, so a route's length says
// nothing about what is hidden. Its alike issues count as steps finds them
// (alike is cached, so this scores nothing steps doesn't).
func (w *graphWalk) degree(n int32) int {
	d := 0
	for _, ei := range w.g.adj[n] {
		e := &w.g.edges[ei]
		if e.basis == basisInferred && !w.guesses {
			continue
		}
		other := e.a
		if other == n {
			other = e.b
		}
		if w.visible(other) {
			d++
		}
	}
	if w.guesses && w.g.nodes[n].typ == KIssue {
		for _, m := range w.alike(w.g.nodes[n].id) {
			if to, ok := w.g.issue[m.id]; ok && w.visible(to) {
				d++
			}
		}
	}
	return d
}

// length is what walking e from a node of degree deg adds.
func edgeLength(e gedgeRec, passDeg int) float64 {
	k := graphKinds[e.kind]
	l := k.length
	if e.kind == "similar" {
		l = 1 + max(0, 1-float64(e.score))
	}
	l *= graphBasisFactor[e.basis]
	if passDeg > 1 {
		l += graphHubWeight * math.Log2(float64(passDeg))
	}
	return l
}

// alike lists the issues most alike to issue id, the best graphSimPerNode,
// kept per similarity index so later queries don't score them again.
func (w *graphWalk) alike(id int64) []simMatch {
	c := &w.s.graph
	c.simMu.Lock()
	if c.simAt != w.idx.key {
		c.simAt, c.sim = w.idx.key, map[int64][]simMatch{}
	}
	got, ok := c.sim[id]
	c.simMu.Unlock()
	if ok {
		return got
	}
	if w.budget <= 0 {
		return nil
	}
	w.budget--
	got = w.idx.nearest(id, graphSimPerNode)
	c.simMu.Lock()
	if c.simAt == w.idx.key {
		c.sim[id] = got
	}
	c.simMu.Unlock()
	return got
}

// nearest lists the issues (open or not) most alike to issue id, best
// first, kept as related keeps them: alike in words (simLow), or about the
// same things (simBlend) with some words shared. With a concept model only
// the issues nearest in concepts are scored in words.
func (idx *simIndex) nearest(id int64, n int) []simMatch {
	d := idx.docs[id]
	if d == nil || len(d.vec) == 0 {
		return nil
	}
	cands := make([]int64, 0, len(idx.docs))
	if d.concept != nil {
		type cc struct {
			id  int64
			cos float64
		}
		var best []cc
		for oid, o := range idx.docs {
			if oid != id && o.concept != nil {
				best = append(best, cc{oid, semantic.Cos(d.concept, o.concept)})
			}
		}
		sort.Slice(best, func(a, b int) bool {
			if best[a].cos != best[b].cos {
				return best[a].cos > best[b].cos
			}
			return best[a].id < best[b].id
		})
		for i := 0; i < len(best) && i < graphSimCands; i++ {
			cands = append(cands, best[i].id)
		}
		for oid, o := range idx.docs {
			if oid != id && o.concept == nil {
				cands = append(cands, oid)
			}
		}
	}
	scan := func(ids []int64) []simMatch {
		var out []simMatch
		for _, oid := range ids {
			o := idx.docs[oid]
			sc := cosine(d.vec, o.vec)
			if sc < simFloor {
				continue
			}
			bl := sc
			if d.concept != nil && o.concept != nil {
				bl += simBlendWeight * semantic.Cos(d.concept, o.concept)
			}
			if sc >= simLow || (d.concept != nil && bl >= simBlend) {
				out = append(out, simMatch{oid, sc, bl})
			}
		}
		return out
	}
	var out []simMatch
	if d.concept != nil {
		out = scan(cands)
	} else {
		all := make([]int64, 0, len(idx.docs))
		for oid := range idx.docs {
			if oid != id {
				all = append(all, oid)
			}
		}
		out = scan(all)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].blend != out[b].blend {
			return out[a].blend > out[b].blend
		}
		return out[a].id < out[b].id
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// reach is a shortest-path search from src.
type reach struct {
	dist  map[int32]float64
	hops  map[int32]int
	route map[int32][]gstep
	done  []int32
}

// hopState is a node reached in a given number of steps.
type hopState struct {
	n    int32
	hops int
}

type pqItem struct {
	n    int32
	hops int
	d    float64
}
type pq []pqItem

func (p pq) Len() int { return len(p) }
func (p pq) Less(a, b int) bool {
	if p[a].d != p[b].d {
		return p[a].d < p[b].d
	}
	return p[a].n < p[b].n
}
func (p pq) Swap(a, b int) { p[a], p[b] = p[b], p[a] }
func (p *pq) Push(x any)   { *p = append(*p, x.(pqItem)) }
func (p *pq) Pop() any {
	old := *p
	it := old[len(old)-1]
	*p = old[:len(old)-1]
	return it
}

// walk runs Dijkstra from src up to maxHops steps, stopping at stop (if
// not -1) or after maxSettled nodes. Nodes the actor can't see aren't
// entered.
//
// The search is over (node, steps), so the step limit can't hide a node: a
// nearer route with more steps doesn't stop a farther one with fewer from
// going on. A state is expanded only when every nearer state of its node
// took more steps; each node is reported once, at its nearest.
func (w *graphWalk) walk(src int32, maxHops int, stop int32, maxSettled int) *reach {
	r := &reach{dist: map[int32]float64{}, hops: map[int32]int{}, route: map[int32][]gstep{}}
	start := hopState{src, 0}
	best := map[hopState]float64{start: 0}
	via := map[hopState]gstep{}
	from := map[hopState]hopState{}
	fewest := map[int32]int{}
	h := &pq{{n: src}}
	for h.Len() > 0 && len(r.done) < maxSettled {
		it := heap.Pop(h).(pqItem)
		at := hopState{it.n, it.hops}
		if it.d > best[at] {
			continue
		}
		if f, seen := fewest[it.n]; seen && f <= it.hops {
			continue
		}
		fewest[it.n] = it.hops
		if _, reported := r.dist[it.n]; !reported {
			r.dist[it.n], r.hops[it.n] = it.d, it.hops
			r.done = append(r.done, it.n)
			var route []gstep
			for st := at; st != start; st = from[st] {
				route = append(route, via[st])
			}
			slices.Reverse(route)
			r.route[it.n] = route
		}
		if it.n == stop {
			break
		}
		if it.hops >= maxHops {
			continue
		}
		pass := 0
		if it.n != src {
			pass = w.degree(it.n)
		}
		for _, st := range w.steps(it.n) {
			next := hopState{st.to, it.hops + 1}
			if f, seen := fewest[st.to]; (seen && f <= next.hops) || !w.visible(st.to) {
				continue
			}
			nd := it.d + edgeLength(st.edge, pass)
			if old, seen := best[next]; !seen || nd < old-1e-9 {
				best[next], via[next], from[next] = nd, st, at
				heap.Push(h, pqItem{n: st.to, hops: next.hops, d: nd})
			}
		}
	}
	return r
}

// path is the steps from the search's source to n.
func (r *reach) path(n int32) []gstep { return r.route[n] }

func (w *graphWalk) graphNode(n int32) GraphNode {
	r := &w.g.nodes[n]
	out := GraphNode{Node: nodeID(r.typ, r.id, r.ref), Type: r.typ, Ref: r.key, Title: r.title, Kind: r.kind, State: r.state, URL: r.url}
	if r.project != 0 {
		out.Project = w.g.keys[r.project]
	}
	return out
}

// edgeOut reads a step as an edge from where it was walked from.
func (w *graphWalk) edgeOut(st gstep, from int32, passDeg int) GraphEdge {
	e := st.edge
	k := graphKinds[e.kind]
	label := k.name
	if !st.fwd && k.inverse != "" {
		label = k.inverse
	}
	if e.sub != "" {
		label += " (" + e.sub + ")"
	}
	out := GraphEdge{From: w.graphNode(from).Node, To: w.graphNode(st.to).Node, Kind: e.kind, Label: label, Subkind: e.sub,
		Basis: basisNames[e.basis], Score: round2(float64(e.score)), Why: e.why, Length: round2(edgeLength(e, passDeg))}
	return out
}

// edgesOf reads a path from src.
func (w *graphWalk) edgesOf(src int32, steps []gstep) []GraphEdge {
	out := make([]GraphEdge, 0, len(steps))
	at := src
	for _, st := range steps {
		pass := 0
		if at != src {
			pass = w.degree(at)
		}
		out = append(out, w.edgeOut(st, at, pass))
		at = st.to
	}
	return out
}

// graphRef finds the node a ref names: an issue key, a page or decision
// code, a pull request, glossary:Term, page:4, design:12, file:path, or
// session:12.
func (s *Service) graphRef(ctx context.Context, w *graphWalk, ref, project string) (int32, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return -1, invalid("name something: an issue key, a page or decision code, a pull request, glossary:Term or a file")
	}
	if v, ok := strings.CutPrefix(ref, "session:"); ok {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if n, found := w.g.node(KSession, id); err == nil && found && w.visible(n) {
			return n, nil
		}
		return -1, notFound("session " + v)
	}
	if n, ok := w.g.index[ref]; ok && w.visible(n) {
		return n, nil
	}
	var sc refScope
	if project != "" {
		if p, err := s.projectByRef(ctx, s.db.R, project); err == nil && s.requireProjectRole(ctx, s.db.R, w.actor, p.ID, RoleViewer) == nil {
			sc = refScope{p.Key, p.ID}
		}
	}
	if toks := ScanRefs(ref); len(toks) == 1 && toks[0] == ref {
		if r := s.resolveRef(ctx, w.actor, ref, sc); r != nil {
			switch r.Kind {
			case KPull:
				if n, ok := w.g.index[nodeID(KPull, 0, graphPullKey(r.URL))]; ok && w.visible(n) {
					return n, nil
				}
				return -1, notFound(ref + ": no task names that pull request")
			case KIssue, KPage, KDesign, KGlossary:
				if n, ok := w.g.node(r.Kind, r.ID); ok && w.visible(n) {
					return n, nil
				}
			}
		}
	}
	it, err := s.Resolve(ctx, w.actor, ref, project)
	if err != nil {
		return -1, err
	}
	switch it.Type {
	case KFile:
		var hit int32 = -1
		for n, r := range w.g.nodes {
			if r.typ == KFile && (r.key == it.Ref || strings.HasSuffix(r.key, "/"+it.Ref)) && w.visible(int32(n)) {
				if hit >= 0 && (sc.id == 0 || r.project != sc.id) {
					continue
				}
				hit = int32(n)
			}
		}
		if hit < 0 {
			return -1, notFound(it.Ref + ": no link or known pull request names that file")
		}
		return hit, nil
	case KURL:
		if n, ok := w.g.index[nodeID(KPull, 0, graphPullKey(it.Ref))]; ok && w.visible(n) {
			return n, nil
		}
		return -1, notFound(it.Ref)
	}
	if n, ok := w.g.node(it.Type, it.ID); ok && w.visible(n) {
		return n, nil
	}
	return -1, notFound(ref + " (drafts and retired notes aren't in the graph)")
}

// Neighbourhood lists what lies within q.Hops steps of q.Ref, nearest
// first, each with the route that reaches it.
func (s *Service) Neighbourhood(ctx context.Context, actor *Actor, q GraphQuery) (*GraphView, error) {
	w, err := s.newWalk(ctx, actor, q.Guesses)
	if err != nil {
		return nil, err
	}
	src, err := s.graphRef(ctx, w, q.Ref, q.Project)
	if err != nil {
		return nil, err
	}
	for _, t := range q.Types {
		if !slices.Contains(graphTypes, t) {
			return nil, invalid("unknown kind %q: use %s", t, strings.Join(graphTypes, ", "))
		}
	}
	if q.Hops <= 0 {
		q.Hops = 3
	}
	q.Hops = min(q.Hops, graphMaxHops)
	if q.Limit <= 0 {
		q.Limit = graphDefaultLimit
	}
	q.Limit = min(q.Limit, 500)
	r := w.walk(src, q.Hops, -1, graphMaxVisited)
	view := &GraphView{Center: w.graphNode(src), Nodes: []GraphHit{}, Edges: []GraphEdge{}, Guesses: q.Guesses}
	if actor.IsAdmin() {
		view.Size = &GraphSize{Nodes: len(w.g.nodes), Edges: len(w.g.edges), Built: w.g.built}
	}
	shown := map[int32]bool{src: true}
	var listed []int32
	for _, n := range r.done {
		if n == src || !graphWanted(&w.g.nodes[n], q) {
			continue
		}
		if len(view.Nodes) >= q.Limit {
			view.More = true
			break
		}
		shown[n], listed = true, append(listed, n)
		view.Nodes = append(view.Nodes, GraphHit{GraphNode: w.graphNode(n), Distance: round2(r.dist[n]), Hops: r.hops[n], Path: w.edgesOf(src, r.path(n))})
	}
	seen := map[string]bool{}
	add := func(e GraphEdge) {
		k := e.From + "|" + e.To + "|" + e.Kind
		if !seen[k] && !seen[e.To+"|"+e.From+"|"+e.Kind] {
			seen[k] = true
			view.Edges = append(view.Edges, e)
		}
	}
	for n := range shown {
		for _, ei := range w.g.adj[n] {
			e := w.g.edges[ei]
			if shown[e.a] && shown[e.b] && e.a == n && (w.guesses || e.basis != basisInferred) {
				add(w.edgeOut(gstep{e.b, e, true}, n, 0))
			}
		}
	}
	for _, n := range listed {
		at := src
		for _, st := range r.path(n) {
			add(w.edgeOut(st, at, 0))
			at = st.to
		}
	}
	if !view.More && len(r.done) >= graphMaxVisited {
		view.More = true
	}
	sort.SliceStable(view.Edges, func(a, b int) bool {
		if view.Edges[a].From != view.Edges[b].From {
			return view.Edges[a].From < view.Edges[b].From
		}
		return view.Edges[a].To < view.Edges[b].To
	})
	return view, nil
}

// graphWanted: the node passes the query's filters (paths still go through
// ones it leaves out).
func graphWanted(n *gnodeRec, q GraphQuery) bool {
	if len(q.Types) > 0 && !slices.Contains(q.Types, n.typ) {
		return false
	}
	if q.Open && n.typ == KIssue && (n.state == "done" || n.state == "canceled") {
		return false
	}
	return true
}

// Path finds the nearest route from one thing to another: how far apart
// they are and through what.
func (s *Service) Path(ctx context.Context, actor *Actor, from, to, project string, guesses bool) (*GraphPath, error) {
	w, err := s.newWalk(ctx, actor, guesses)
	if err != nil {
		return nil, err
	}
	a, err := s.graphRef(ctx, w, from, project)
	if err != nil {
		return nil, err
	}
	b, err := s.graphRef(ctx, w, to, project)
	if err != nil {
		return nil, err
	}
	out := &GraphPath{From: w.graphNode(a), To: w.graphNode(b), Path: []GraphEdge{}, Nodes: []GraphNode{}}
	if a == b {
		out.Found, out.Nodes = true, []GraphNode{out.From}
		return out, nil
	}
	r := w.walk(a, graphMaxHops, b, graphMaxVisited)
	if _, ok := r.dist[b]; !ok {
		return out, nil
	}
	steps := r.path(b)
	out.Found, out.Distance, out.Hops = true, round2(r.dist[b]), len(steps)
	out.Path = w.edgesOf(a, steps)
	out.Nodes = append(out.Nodes, out.From)
	for _, st := range steps {
		out.Nodes = append(out.Nodes, w.graphNode(st.to))
	}
	return out, nil
}

// nearbyMax caps the work in progress a task's answer lists as nearby.
const nearbyMax = 3

// nearbyWork is the open work in progress nearest a task, four steps out (a shared file is four: task, pull request, file, pull request, task)
// with guesses, leaving out what its answer already lists. It never fails
// the answer: a graph that can't be read lists nothing.
func (s *Service) nearbyWork(ctx context.Context, actor *Actor, t *AgentTask) []GraphHit {
	d := t.Issue
	listed := map[string]bool{d.Key: true}
	if d.Parent != nil {
		listed[d.Parent.Key] = true
	}
	for _, r := range d.Relations {
		listed[r.Issue.Key] = true
	}
	for _, c := range d.Children {
		listed[c.Key] = true
	}
	for _, i := range t.Named {
		listed[i.Key] = true
	}
	v, err := s.Neighbourhood(ctx, actor, GraphQuery{Ref: d.Key, Hops: 4, Limit: 200, Guesses: true, Types: []string{KIssue}, Open: true})
	if err != nil {
		return nil
	}
	var out []GraphHit
	for _, h := range v.Nodes {
		if h.State != "in_progress" || listed[h.Ref] {
			continue
		}
		if out = append(out, h); len(out) == nearbyMax {
			break
		}
	}
	return out
}
