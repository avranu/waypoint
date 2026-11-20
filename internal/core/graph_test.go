package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// graphFixture is the relate fixture plus a goal, a page, a parent, a pull
// request with its files, and a hub task many others hang from.
type graphFixture struct {
	relateFixture
	goal              *DesignNote
	page              *Page
	child, sibling    *IssueDetail
	prA, prB          *IssueDetail // tasks whose pull requests change the same file
	hub               *IssueDetail
	viaHub, viaDirect *IssueDetail
	project           string
}

func newGraphFixture(e *env) graphFixture {
	f := graphFixture{relateFixture: newRelateFixture(e, nil)}
	f.project = strings.SplitN(f.src.Key, "-", 2)[0]
	var err error
	title, kind, body := "People find the maps they need", "goal", "Maps."
	if f.goal, err = e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: &title, Kind: &kind, Body: &body, Project: &f.project}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KIssue, Key: f.src.Key}, To: LinkEnd{Type: KDesign, ID: f.goal.ID}, Rel: "serves"}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, f.src.Key, "relates", f.linked.Key); err != nil {
		e.t.Fatal(err)
	}
	ptitle, issues := "Harvest notes", []string{f.linked.Key}
	if f.page, err = e.s.CreatePage(e.ctx, e.admin, PageInput{Title: &ptitle, Project: Some(f.project), Issues: &issues}); err != nil {
		e.t.Fatal(err)
	}
	f.child = e.issue(e.admin, IssueInput{Project: f.project, Title: "Resume the walk at the town it stopped", Parent: f.src.Key})
	f.sibling = e.issue(e.admin, IssueInput{Project: f.project, Title: "Log where the walk stopped", Parent: f.src.Key})
	f.prA = e.issue(e.admin, IssueInput{Project: f.project, Title: "Speed up tile cache"})
	f.prB = e.issue(e.admin, IssueInput{Project: f.project, Title: "Evict stale tiles"})
	f.hub = e.issue(e.admin, IssueInput{Project: f.project, Title: "Quarterly cleanup"})
	for n := range 12 {
		c := e.issue(e.admin, IssueInput{Project: f.project, Title: fmt.Sprintf("Cleanup item %d", n)})
		if err := e.s.AddRelation(e.ctx, e.admin, c.Key, "relates", f.hub.Key); err != nil {
			e.t.Fatal(err)
		}
	}
	f.viaHub = e.issue(e.admin, IssueInput{Project: f.project, Title: "Rotate the archive keys"})
	f.viaDirect = e.issue(e.admin, IssueInput{Project: f.project, Title: "Archive retention"})
	for _, r := range [][3]string{{f.prA.Key, "relates", f.hub.Key}, {f.viaHub.Key, "relates", f.hub.Key},
		{f.prA.Key, "relates", f.viaDirect.Key}} {
		if err := e.s.AddRelation(e.ctx, e.admin, r[0], r[1], r[2]); err != nil {
			e.t.Fatal(err)
		}
	}
	// Two pull requests, one per task, change the same file.
	pid := e.projectID(f.project)
	now := e.clockNow().UnixMilli()
	e.exec(`INSERT INTO forges(id, name, kind, base_url, created_at) VALUES (90, 'f', 'forgejo', 'https://git.test', ?)`, now)
	e.exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, 90, 'acme/maps', ?)`, pid, now)
	for n, key := range []string{f.prA.Key, f.prB.Key} {
		url := fmt.Sprintf("https://git.test/acme/maps/pulls/%d", n+1)
		e.exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, title, state, created_at) VALUES (?, 'forgejo', 'pull', ?, 'acme/maps', ?, ?, 'merged', ?)`,
			e.issueID(key), url, n+1, "PR for "+key, now)
		e.exec(`INSERT INTO pull_requests(forge_id, repo, number, url, title, state, files, created_at, updated_at) VALUES (90, 'acme/maps', ?, ?, ?, 'merged', ?, ?, ?)`,
			n+1, url, "PR for "+key, `{"paths":["tiles/cache.go","docs/x`+fmt.Sprint(n)+`.md"]}`, now, now)
	}
	return f
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) projectID(key string) int64 {
	e.t.Helper()
	var id int64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT id FROM projects WHERE key = ?`, key).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func hitsByRef(v *GraphView) map[string]GraphHit {
	out := map[string]GraphHit{}
	for _, h := range v.Nodes {
		out[h.Ref] = h
	}
	return out
}

func pathText(p []GraphEdge) string {
	var parts []string
	for _, e := range p {
		parts = append(parts, e.Label+" "+e.To)
	}
	return strings.Join(parts, ", ")
}

// The step limit doesn't hide what lies within it: Z is two steps from C
// (C, B, Z), though the nearest route to B takes two steps (C, A, B).
// Stored guesses stay off the map with guesses off.
func TestGraphHopLimitAndGuessEdges(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	mk := func(title string) *IssueDetail { return e.issue(e.admin, IssueInput{Project: f.project, Title: title}) }
	c, a, b, z := mk("Hop source"), mk("Hop near"), mk("Hop middle"), mk("Hop far")
	for _, r := range [][3]string{{c.Key, "relates", b.Key}, {c.Key, "duplicates", a.Key}, {a.Key, "duplicates", b.Key}, {b.Key, "relates", z.Key}} {
		if err := e.s.AddRelation(e.ctx, e.admin, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`UPDATE issue_relations SET basis = 'agent' WHERE source_id = ? AND target_id = ?`, e.issueID(c.Key), e.issueID(b.Key))
	v, err := e.s.Neighbourhood(e.ctx, e.admin, GraphQuery{Ref: c.Key, Hops: 2})
	if err != nil {
		t.Fatal(err)
	}
	hits := hitsByRef(v)
	if h, ok := hits[z.Key]; !ok || h.Hops != 2 || len(h.Path) != 2 {
		t.Errorf("far end within 2 steps: %+v (found %v)", h, ok)
	}
	if h := hits[b.Key]; h.Hops != 2 || len(h.Path) != 2 {
		t.Errorf("middle at its nearest: %+v", h)
	}

	// A stored guess: drawn only with guesses on.
	g1, g2, g3 := mk("Guess one"), mk("Guess two"), mk("Guess three")
	for _, r := range [][3]string{{g1.Key, "blocks", g2.Key}, {g2.Key, "blocks", g3.Key}, {g1.Key, "relates", g3.Key}} {
		if err := e.s.AddRelation(e.ctx, e.admin, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`UPDATE issue_relations SET basis = ? WHERE source_id = ? AND target_id = ?`, linkBasisInferred, e.issueID(g1.Key), e.issueID(g3.Key))
	v, err = e.s.Neighbourhood(e.ctx, e.admin, GraphQuery{Ref: g1.Key, Hops: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hitsByRef(v)[g3.Key]; !ok {
		t.Errorf("g3 not reached through blocks: %+v", v.Nodes)
	}
	for _, ed := range v.Edges {
		if ed.Basis == "inferred" {
			t.Errorf("a guess drawn with guesses off: %+v", ed)
		}
	}
	// Unknown kinds are refused, not an empty list.
	if _, err := e.s.Neighbourhood(e.ctx, e.admin, GraphQuery{Ref: g1.Key, Types: []string{"isue"}}); err == nil {
		t.Error("unknown kind accepted")
	}
	// The whole graph's size is for admins.
	if v, err := e.s.Neighbourhood(e.ctx, f.member, GraphQuery{Ref: f.src.Key}); err != nil || v.Size != nil {
		t.Errorf("member sees size: %+v %v", v, err)
	}
}

// Writes that don't touch updated_at still show: a project's new key, a
// pull request's files.
func TestGraphSignatureSeesQuietWrites(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	if _, err := e.s.Neighbourhood(e.ctx, e.admin, GraphQuery{Ref: f.src.Key}); err != nil {
		t.Fatal(err)
	}
	key := "ZZQ"
	if _, err := e.s.UpdateProject(e.ctx, e.admin, f.project, ProjectInput{Key: &key}); err != nil {
		t.Fatal(err)
	}
	ref := key + "-" + strings.SplitN(f.src.Key, "-", 2)[1]
	v, err := e.s.Neighbourhood(e.ctx, e.admin, GraphQuery{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if v.Center.Ref != ref {
		t.Errorf("center after rename = %s, want %s", v.Center.Ref, ref)
	}
	e.exec(`UPDATE pull_requests SET files = ? WHERE number = 2`, `{"paths":["tiles/other.go"]}`)
	if p, err := e.s.Path(e.ctx, e.admin, "tiles/cache.go", "tiles/other.go", key, false); err != nil || p.Found {
		t.Errorf("files changed quietly: %+v %v", p, err)
	}
}

func TestGraphNeighbourhood(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	v, err := e.s.Neighbourhood(e.ctx, f.member, GraphQuery{Ref: f.src.Key, Hops: 3, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if v.Center.Ref != f.src.Key || v.Center.Type != KIssue {
		t.Fatalf("center = %+v", v.Center)
	}
	hits := hitsByRef(v)
	child := hits[f.child.Key]
	if child.Hops != 1 || len(child.Path) != 1 || child.Path[0].Label != "parent of" || child.Path[0].Basis != "person" {
		t.Errorf("child: %+v", child)
	}
	goal := hits["#"+itoa64(f.goal.ID)]
	if goal.Hops != 1 || goal.Path[0].Label != "serves" || goal.Type != KDesign {
		t.Errorf("goal: %+v", goal)
	}
	// The page documents the linked task, which the source relates to.
	if p := hits["page:"+itoa64(f.page.ID)]; p.Hops != 2 || p.Path[1].Label != "documented by" {
		t.Errorf("page: %+v (%v)", p, hits)
	}
	if w := hits[f.waits.Key]; w.Hops != 1 {
		t.Errorf("a key the text names relates it: %+v", w)
	}
	// Nearest first.
	for n := 1; n < len(v.Nodes); n++ {
		if v.Nodes[n].Distance < v.Nodes[n-1].Distance {
			t.Fatalf("not nearest first: %v then %v", v.Nodes[n-1], v.Nodes[n])
		}
	}
	// The edges drawn join only what's shown.
	shown := map[string]bool{v.Center.Node: true}
	for _, h := range v.Nodes {
		shown[h.Node] = true
	}
	for _, ed := range v.Edges {
		if !shown[ed.From] || !shown[ed.To] {
			t.Errorf("edge to something not shown: %+v", ed)
		}
		// Drawn edges carry the link's own length, however a route reached
		// them: no hub cost, which depends on the route.
		bare := gedgeRec{kind: ed.Kind, basis: int8(slices.Index(basisNames, ed.Basis)), score: float32(ed.Score)}
		if want := round2(edgeLength(bare, 0)); ed.Length != want {
			t.Errorf("drawn edge %s→%s length %v, want %v", ed.From, ed.To, ed.Length, want)
		}
	}
	// Without guesses nothing is merely alike; Sam's project stays hidden.
	for _, h := range v.Nodes {
		if h.Ref == f.private.Key || h.Ref == f.alike.Key {
			t.Errorf("listed %s", h.Ref)
		}
	}
	// Two steps through the parent: a sibling.
	cv, err := e.s.Neighbourhood(e.ctx, f.member, GraphQuery{Ref: f.child.Key, Hops: 2})
	if err != nil {
		t.Fatal(err)
	}
	if s := hitsByRef(cv)[f.sibling.Key]; s.Hops != 2 || pathText(s.Path) != "subtask of "+cv.Center.Node[:6]+itoa64(e.issueID(f.src.Key))+", parent of "+s.Node {
		t.Errorf("sibling: %+v", s)
	}
}

func TestGraphGuessesAndHubs(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	v, err := e.s.Neighbourhood(e.ctx, f.member, GraphQuery{Ref: f.src.Key, Hops: 1, Limit: 100, Guesses: true})
	if err != nil {
		t.Fatal(err)
	}
	hits := hitsByRef(v)
	a := hits[f.alike.Key]
	if a.Hops != 1 || a.Path[0].Kind != "similar" || a.Path[0].Basis != "inferred" || a.Path[0].Why == "" {
		t.Errorf("alike: %+v", a)
	}
	if _, ok := hits[f.private.Key]; ok {
		t.Error("a guess reached a project the member can't see")
	}
	// A route through a task twelve others hang from is longer than a
	// route through a plain link of the same kind.
	hub, err := e.s.Path(e.ctx, e.admin, f.prA.Key, f.viaHub.Key, "", false)
	if err != nil || !hub.Found || hub.Hops != 2 {
		t.Fatalf("via hub: %+v %v", hub, err)
	}
	direct, err := e.s.Path(e.ctx, e.admin, f.prA.Key, f.viaDirect.Key, "", false)
	if err != nil || !direct.Found || direct.Hops != 1 {
		t.Fatalf("direct: %+v %v", direct, err)
	}
	if hub.Distance <= direct.Distance+1.5 {
		t.Errorf("the hub costs nothing: %v vs %v", hub.Distance, direct.Distance)
	}
}

func TestGraphPath(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	// Two tasks whose pull requests change the same file.
	p, err := e.s.Path(e.ctx, e.admin, f.prA.Key, f.prB.Key, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, ed := range p.Path {
		labels = append(labels, ed.Label)
	}
	if !p.Found || strings.Join(labels, ", ") != "worked in, changes, changed by, works on" || p.Nodes[2].Ref != "tiles/cache.go" || p.Nodes[1].Ref != "acme/maps#1" {
		t.Errorf("path = %+v", p)
	}
	// From a file too.
	if fp, err := e.s.Path(e.ctx, e.admin, "tiles/cache.go", f.prB.Key, f.project, false); err != nil || !fp.Found || fp.Hops != 2 {
		t.Errorf("from a file: %+v %v", fp, err)
	}
	// Nothing joins them: found is false, not an error.
	lone := e.issue(e.admin, IssueInput{Project: f.project, Title: "Unrelated chore"})
	if np, err := e.s.Path(e.ctx, e.admin, lone.Key, f.prA.Key, "", false); err != nil || np.Found {
		t.Errorf("no path: %+v %v", np, err)
	}
	// What the actor can't see can't be asked about.
	if _, err := e.s.Path(e.ctx, f.member, f.src.Key, f.private.Key, "", false); err == nil {
		t.Error("a path to an issue the member can't see")
	}
	// A new relation shows at once.
	if err := e.s.AddRelation(e.ctx, e.admin, lone.Key, "blocks", f.prA.Key); err != nil {
		t.Fatal(err)
	}
	if np, err := e.s.Path(e.ctx, e.admin, f.prA.Key, lone.Key, "", false); err != nil || !np.Found || np.Path[0].Label != "blocked by" {
		t.Errorf("after relating: %+v %v", np, err)
	}
}

// A task's answer names work in progress near it that it doesn't already
// list: here, the task whose pull request changes the same file.
func TestGraphNearbyWork(t *testing.T) {
	e := newEnv(t)
	f := newGraphFixture(e)
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, f.prB.Key, IssuePatch{Status: ptr("In Progress")}); err != nil {
		t.Fatal(err)
	}
	task, err := e.s.agentTask(e.ctx, e.admin, e.issueID(f.prA.Key))
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Nearby) != 1 || task.Nearby[0].Ref != f.prB.Key || task.Nearby[0].Hops != 4 {
		t.Fatalf("nearby: %+v", task.Nearby)
	}
	// Related already: not listed twice.
	if err := e.s.AddRelation(e.ctx, e.admin, f.prA.Key, "relates", f.prB.Key); err != nil {
		t.Fatal(err)
	}
	if task, _ = e.s.agentTask(e.ctx, e.admin, e.issueID(f.prA.Key)); len(task.Nearby) != 0 {
		t.Errorf("listed a related task again: %+v", task.Nearby)
	}
}
