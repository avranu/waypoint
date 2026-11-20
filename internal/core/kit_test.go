package core

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

const fixtureKitTOML = `
[kit]
marketplace = "agent-kit"
compact_window = 200000

[plugins.waypoint]
repos = ["*"]

[plugins.harbor]
repos = ["Harbor/*", "Robin/Quarry", "!Harbor/Harbor-docs"]

[plugins.indexed]
when = ["exists:.indexed"]
repos = ["!acme/secret"]

[plugins.gpu]
repos = ["WP"]
machines = ["gpu"]

[[notes]]
repos = ["*"]
text = "Wait on CI with ` + "`wp pr wait`" + `; never sleep-poll."

[[notes]]
repos = ["Harbor/*"]
text = "Run the Harbor tests with the run-tests skill."
`

const fixtureMarketplace = `{
  "name": "agent-kit",
  "owner": {"name": "Robin"},
  "plugins": [
    {"name": "waypoint", "source": "./plugins/waypoint", "description": "The PR workflow"},
    {"name": "harbor", "source": "./plugins/harbor", "description": "Harbor tooling"},
    {"name": "indexed", "source": "./plugins/indexed"},
    {"name": "gpu", "source": "./plugins/gpu"},
    {"name": "demo", "source": "./plugins/demo", "description": "Not targeted"}
  ]
}`

func fixtureKitFiles() map[string]string {
	return map[string]string{
		"kit.toml":                                    fixtureKitTOML,
		".claude-plugin/marketplace.json":             fixtureMarketplace,
		"plugins/waypoint/.claude-plugin/plugin.json": `{"name":"waypoint","version":"1.2.0"}`,
		"plugins/waypoint/skills/ship/SKILL.md":       "---\nname: ship\ndescription: \"Ship a change: commit, PR, arm\"\n---\n# Ship\n",
		"plugins/waypoint/skills/wait/SKILL.md":       "---\nname: wait\ndescription: >\n  Wait on CI\n  without polling\n---\n",
		"plugins/waypoint/agents/reviewer.md":         "---\nname: reviewer\ndescription: Reviews a diff\n---\nYou review.\n",
		"plugins/waypoint/commands/pr/open.md":        "---\ndescription: Open a PR\n---\n",
		"plugins/waypoint/hooks/hooks.json":           `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"nudge"}]}],"SessionStart":[{"hooks":[]}]}}`,
		"plugins/waypoint/.mcp.json":                  `{"mcpServers":{"waypoint":{"command":"wp","args":["mcp"]}}}`,
		"plugins/waypoint/bin/wt":                     "#!/bin/sh\n",
		"plugins/harbor/skills/run-tests/SKILL.md":    "---\nname: run-tests\ndescription: Run tests the right way\n---\n",
		"plugins/indexed/.mcp.json":                   `{"indexed":{"command":"indexed"}}`,
		"plugins/gpu/commands/train.md":               "Train.\n",
		"plugins/demo/commands/hello.md":              "---\ndescription: Say hello\n---\n",
	}
}

func TestParseKitFiles(t *testing.T) {
	k, errs := parseKitFiles(fixtureKitTOML, fixtureMarketplace)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if k.Marketplace != "agent-kit" || k.Owner != "Robin" || len(k.Plugins) != 5 || len(k.Notes) != 2 || k.CompactWindow != 200000 {
		t.Fatalf("kit: %+v", k)
	}
	demo := k.Plugins[4]
	if demo.Name != "demo" || demo.Targeted || demo.Source != "./plugins/demo" {
		t.Fatalf("demo: %+v", demo)
	}
	bad := []struct{ toml, mkt, want string }{
		{fixtureKitTOML + "\n[plugins.nope]\nrepos=[\"*\"]\n", fixtureMarketplace, "plugins.nope isn't a plugin"},
		{"[kit]\nmarketplace = \"other\"\n", fixtureMarketplace, `marketplace "other"`},
		{"[plugins.waypoint]\nrepos = [\"acme/[x\"]\n", fixtureMarketplace, "isn't a valid glob"},
		{"[plugins.waypoint]\nwhen = [\"gpu\"]\n", fixtureMarketplace, "isn't a condition"},
		{"[plugins.waypoint]\nrepos = [\"no such thing!\"]\n", fixtureMarketplace, "neither OWNER/NAME"},
		{"[plugins.waypoint]\ncolour = \"red\"\n", fixtureMarketplace, "unknown key plugins.waypoint.colour"},
		{"[[notes]]\nrepos=[\"*\"]\n", fixtureMarketplace, "a note needs text"},
		{"[kit\n", fixtureMarketplace, "kit.toml:"},
		{"", fixtureMarketplace, "kit.toml is missing"},
		{fixtureKitTOML, "", "marketplace.json is missing"},
		{fixtureKitTOML, "{", "marketplace.json:"},
		{"[kit]\n", `{"name":"x","plugins":[{"name":"a","source":"../out"}]}`, "must start with ./"},
		{"[kit]\ngit_url = \"file:///tmp/kit\"\n", fixtureMarketplace, "git_url"},
		{"[kit]\ncompact_window = 2000000\n", fixtureMarketplace, "compact_window 2000000 isn't from"},
		{"[kit]\ncompact_window = 5000\n", fixtureMarketplace, "compact_window 5000"},
	}
	for _, b := range bad {
		_, errs := parseKitFiles(b.toml, b.mkt)
		if !slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, b.want) }) {
			t.Errorf("%q: want an error with %q, got %v", b.toml, b.want, errs)
		}
	}
}

func TestKitTargeting(t *testing.T) {
	cases := []struct {
		repos    []string
		repo     string
		projects []string
		want     bool
	}{
		{nil, "a/b", []string{"X"}, true},
		{[]string{"*"}, "a/b", nil, true},
		{[]string{"Harbor/*"}, "harbor/Quarry", nil, true}, // case-insensitive
		{[]string{"Harbor/*"}, "Robin/Quarry", nil, false},
		{[]string{"Harbor/*", "!Harbor/Harbor-docs"}, "Harbor/Harbor-docs", nil, false},
		{[]string{"Harbor/*", "!Harbor/Harbor-docs"}, "Harbor/Harbor", nil, true},
		{[]string{"!Harbor/Harbor"}, "Robin/agent-kit", nil, true}, // only exclusions: everything else
		{[]string{"!Harbor/Harbor"}, "Harbor/Harbor", nil, false},
		{[]string{"WP"}, "claude/waypoint", []string{"WP"}, true}, // a project key
		{[]string{"wp"}, "claude/waypoint", []string{"WP"}, true},
		{[]string{"WP"}, "claude/other", []string{"APP"}, false},
		{[]string{"*", "!APP"}, "Harbor/Harbor", []string{"APP", "WP"}, false}, // excluded through one of its projects
		{[]string{"Robin/*-kit"}, "Robin/agent-kit", nil, true},
	}
	for _, c := range cases {
		if got := (KitTarget{Repos: c.repos}).matchesRepo(c.repo, c.projects); got != c.want {
			t.Errorf("%v on %s %v: got %v", c.repos, c.repo, c.projects, got)
		}
	}

	k, _ := parseKitFiles(fixtureKitTOML, fixtureMarketplace)
	names := func(r *KitResolved) []string {
		var out []string
		for _, p := range r.Plugins {
			out = append(out, p.Name)
		}
		return out
	}
	r := k.resolve("Harbor/Quarry", []string{"APP"})
	if got := names(r); !slices.Equal(got, []string{"waypoint", "harbor", "indexed"}) || len(r.Notes) != 2 || len(r.All) != 5 {
		t.Fatalf("Harbor/Quarry: %v %+v", got, r.Notes)
	}
	if r.CompactWindow != 200000 {
		t.Fatalf("compact window not passed on: %+v", r)
	}
	if r.Plugins[2].When[0] != "exists:.indexed" {
		t.Fatalf("when not passed on: %+v", r.Plugins[2])
	}
	r = k.resolve("claude/waypoint", []string{"WP"})
	if got := names(r); !slices.Equal(got, []string{"waypoint", "indexed", "gpu"}) || r.Plugins[2].Machines[0] != "gpu" || len(r.Notes) != 1 {
		t.Fatalf("claude/waypoint: %v %+v", got, r)
	}
	if got := names(k.resolve("acme/secret", []string{"AC"})); !slices.Equal(got, []string{"waypoint"}) {
		t.Fatalf("acme/secret: %v", got)
	}
	// A repo attached to no project gets nothing, but still learns what the
	// kit manages.
	r = k.resolve("someone/else", nil)
	if r.Attached || len(r.Plugins) != 0 || len(r.Notes) != 0 || len(r.All) != 5 || r.CompactWindow != 0 {
		t.Fatalf("unattached: %+v", r)
	}
}

func TestFrontMatterAndHooks(t *testing.T) {
	fm := frontMatter("---\nname: 'it''s'\ndescription: |\n  two\n  lines\nother: x # not a comment\n---\nbody: no\n")
	if fm["name"] != "it's" || fm["description"] != "two lines" || fm["other"] != "x # not a comment" || fm["body"] != "" {
		t.Fatalf("front matter: %q", fm)
	}
	if len(frontMatter("no front matter: here")) != 0 {
		t.Fatal("read front matter that isn't there")
	}

	cur := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"a"}]}]}}`
	add := `{"hooks":{"PreToolUse":[{"hooks":[{"command":"a","type":"command"}],"matcher":"Bash"},{"matcher":"Edit","hooks":[{"type":"command","command":"b"}]}],"Stop":[{"hooks":[{"type":"command","command":"c"}]}]}}`
	merged, err := mergeHooks(cur, add)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hooks["PreToolUse"]) != 2 || len(got.Hooks["Stop"]) != 1 {
		t.Fatalf("merged (the same group twice?): %s", merged)
	}
	if _, err := mergeHooks("", `{"hooks":{"not an event":[{}]}}`); !IsInvalid(err) {
		t.Fatalf("a bad event: %v", err)
	}
	if _, err := mergeHooks("", `{}`); !IsInvalid(err) {
		t.Fatalf("nothing to add: %v", err)
	}
}

// fakeKitForge is a Forgejo holding the kit repo Robin/agent-kit.
type fakeKitForge struct {
	t      *testing.T
	mu     sync.Mutex
	head   string
	files  map[string]string
	commit []map[string]any
	prs    []map[string]any
	reads  int
}

func (f *fakeKitForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/repos/Robin/agent-kit"))
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == "GET" && path == "":
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	case r.Method == "GET" && path == "/branches/main":
		_, _ = fmt.Fprintf(w, `{"commit":{"id":%q}}`, f.head)
	case r.Method == "GET" && strings.HasPrefix(path, "/git/trees/"):
		var tree []map[string]string
		for p := range f.files {
			tree = append(tree, map[string]string{"path": p, "type": "blob"})
		}
		sort.Slice(tree, func(i, j int) bool { return tree[i]["path"] < tree[j]["path"] })
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
	case r.Method == "GET" && strings.HasPrefix(path, "/raw/"):
		f.reads++
		if c, ok := f.files[strings.TrimPrefix(path, "/raw/")]; ok {
			_, _ = w.Write([]byte(c))
			return
		}
		w.WriteHeader(404)
	case r.Method == "GET" && strings.HasPrefix(path, "/contents/"):
		if c, ok := f.files[strings.TrimPrefix(path, "/contents/")]; ok {
			_, _ = fmt.Fprintf(w, `{"sha":%q,"type":"file"}`, gitBlobSHA(c))
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && path == "/contents":
		f.commit = append(f.commit, body)
		_, _ = w.Write([]byte(`{"commit":{"sha":"c0ffee1234567"}}`))
	case r.Method == "POST" && path == "/pulls":
		f.prs = append(f.prs, body)
		_, _ = fmt.Fprintf(w, `{"number":12,"title":%q,"html_url":"https://git.example.com/Robin/agent-kit/pulls/12","state":"open"}`, body["title"])
	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		w.WriteHeader(500)
	}
}

type kitEnv struct {
	*env
	fake   *fakeKitForge
	repoID int64
	p      *Project
}

func newKitEnv(t *testing.T) *kitEnv {
	e := newEnv(t)
	fake := &fakeKitForge{t: t, head: "1111111111111111111111111111111111111111", files: fixtureKitFiles()}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	forge, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	k := &kitEnv{env: e, fake: fake, p: e.project(e.admin, "Waypoint", "kanban")}
	res, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,'Robin/agent-kit',1)`, k.p.ID, forge.ID)
	if err != nil {
		t.Fatal(err)
	}
	k.repoID, _ = res.LastInsertId()
	ul := e.project(e.admin, "Harbor", "kanban")
	for _, x := range []struct {
		p    int64
		repo string
	}{{ul.ID, "Harbor/Harbor"}, {ul.ID, "Harbor/Harbor-docs"}, {k.p.ID, "claude/waypoint"}} {
		if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,?,1)`, x.p, forge.ID, x.repo); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

func (k *kitEnv) setSource() {
	k.t.Helper()
	if _, err := k.s.SetKitSource(k.ctx, k.admin, k.repoID); err != nil {
		k.t.Fatal(err)
	}
	k.s.Wait() // the read EvKitSourceSet sets off
}

func TestKitReadFromTheForge(t *testing.T) {
	k := newKitEnv(t)
	sam := k.user("sam", false)

	// No kit yet: an empty answer, not an error.
	info, err := k.s.GetKit(k.ctx, sam)
	if err != nil || info.Configured || info.CanManage {
		t.Fatalf("no kit: %+v %v", info, err)
	}
	if r, err := k.s.ResolveKit(k.ctx, sam, "claude/waypoint"); err != nil || len(r.Plugins) != 0 {
		t.Fatalf("resolve without a kit: %+v %v", r, err)
	}
	// Only admins pick the kit, and not through an agent.
	if _, err := k.s.SetKitSource(k.ctx, sam, k.repoID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member picked the kit: %v", err)
	}
	if _, err := k.s.SetKitSource(k.ctx, agentActor(k.admin, "anthropic/claude-code"), k.repoID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent picked the kit: %v", err)
	}
	k.setSource()

	info, err = k.s.GetKit(k.ctx, sam)
	if err != nil || !info.Configured || info.Kit == nil || info.Commit != k.fake.head || info.Branch != "main" || len(info.Errors) != 0 {
		t.Fatalf("read: %+v %v", info, err)
	}
	if !strings.HasSuffix(info.Kit.GitURL, "/Robin/agent-kit.git") {
		t.Fatalf("git url defaults to the clone URL: %q", info.Kit.GitURL)
	}
	wp := info.Kit.Plugins[0]
	if wp.Name != "waypoint" || wp.Version != "1.2.0" || len(wp.Skills) != 2 || wp.Skills[1].Description != "Wait on CI without polling" ||
		len(wp.Agents) != 1 || wp.Agents[0].Name != "reviewer" || len(wp.Commands) != 1 || wp.Commands[0].Name != "pr:open" ||
		len(wp.Hooks) != 2 || wp.Hooks[0].Description != "matches Bash" || len(wp.MCP) != 1 || len(wp.Bin) != 1 || wp.Bin[0].Name != "wt" {
		t.Fatalf("waypoint plugin: %+v", wp)
	}
	if rw := info.Kit.Plugins[2]; len(rw.MCP) != 1 || rw.MCP[0].Name != "indexed" {
		t.Fatalf("indexed's .mcp.json without mcpServers: %+v", rw.MCP)
	}
	if got := info.Reach["harbor"]; !slices.Equal(got, []string{"Harbor/Harbor"}) {
		t.Fatalf("harbor reaches %v", got)
	}
	if got := info.Reach["waypoint"]; len(got) != 4 || len(info.Reach["demo"]) != 0 {
		t.Fatalf("reach: %+v", info.Reach)
	}

	// The same head again: nothing is read.
	reads := k.fake.reads
	k.s.RefreshKit(k.ctx)
	if k.fake.reads != reads {
		t.Fatalf("read again at the same commit: %d → %d", reads, k.fake.reads)
	}

	// A push breaks kit.toml: the last good kit stays, with the errors.
	k.fake.mu.Lock()
	k.fake.head = "2222222222222222222222222222222222222222"
	k.fake.files["kit.toml"] = "[plugins.nope]\n"
	k.fake.mu.Unlock()
	k.s.dispatch(k.ctx, SystemActor(), []*Event{{Kind: EvRepoPushed, Data: map[string]any{"repo": "robin/agent-kit", "branch": "main", "after": k.fake.head}}})
	k.s.Wait()
	info, _ = k.s.GetKit(k.ctx, sam)
	if info.Commit != "1111111111111111111111111111111111111111" || info.ReadSHA != k.fake.head || len(info.Errors) == 0 || info.Kit == nil || len(info.Kit.Plugins) != 5 {
		t.Fatalf("broken kit replaced the good one: %+v", info)
	}
	if r, _ := k.s.ResolveKit(k.ctx, sam, "claude/waypoint"); r.Commit != "1111111111111111111111111111111111111111" || len(r.Plugins) == 0 {
		t.Fatalf("resolve after a broken push: %+v", r)
	}
	// Fixed: the new commit is the kit.
	k.fake.mu.Lock()
	k.fake.head = "3333333333333333333333333333333333333333"
	k.fake.files["kit.toml"] = fixtureKitTOML
	k.fake.mu.Unlock()
	if _, err := k.s.ReadKitNow(k.ctx, sam); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member read the kit now: %v", err)
	}
	info, err = k.s.ReadKitNow(k.ctx, k.admin)
	if err != nil || info.Commit != k.fake.head || len(info.Errors) != 0 {
		t.Fatalf("fixed: %+v %v", info, err)
	}
	r, err := k.s.ResolveKit(k.ctx, sam, "Harbor/Harbor")
	if err != nil || r.Commit != k.fake.head || !r.Attached || len(r.Plugins) != 3 || len(r.Notes) != 2 {
		t.Fatalf("resolve: %+v %v", r, err)
	}
	if _, err := k.s.ResolveKit(k.ctx, sam, "nope"); !IsInvalid(err) {
		t.Fatalf("a bad repo: %v", err)
	}
}

func TestKitCheckoutsAndRollout(t *testing.T) {
	k := newKitEnv(t)
	k.setSource()
	sam := k.user("sam", false)
	head := k.fake.head
	report := func(a *Actor, in KitReport) {
		t.Helper()
		if err := k.s.ReportKitCheckout(k.ctx, a, in); err != nil {
			t.Fatal(err)
		}
	}
	report(sam, KitReport{Machine: "laptop", Path: "/src/waypoint", Repo: "claude/waypoint", KitCommit: head, Plugins: []string{"waypoint"}})
	report(sam, KitReport{Machine: "laptop", Path: "/src/old", Repo: "claude/waypoint", KitCommit: "0000000"})
	report(k.admin, KitReport{Machine: "devhost", Path: "/p/ul", Repo: "Harbor/Harbor", KitCommit: head,
		Drift: []KitDrift{{Plugin: "harbor", What: "disabled here by hand"}, {What: " "}}})
	// The same checkout again replaces its report.
	report(sam, KitReport{Machine: "laptop", Path: "/src/waypoint", Repo: "claude/waypoint", KitCommit: head, Plugins: []string{"waypoint", "gpu"}})
	if err := k.s.ReportKitCheckout(k.ctx, sam, KitReport{Path: "/x"}); !IsInvalid(err) {
		t.Fatalf("a report without a machine: %v", err)
	}

	mine, err := k.s.ListKitCheckouts(k.ctx, sam)
	if err != nil || len(mine) != 2 {
		t.Fatalf("sam's: %+v %v", mine, err)
	}
	if mine[0].Path != "/src/old" || !mine[0].Behind || mine[1].Behind || !slices.Equal(mine[1].Plugins, []string{"waypoint", "gpu"}) {
		t.Fatalf("behind first: %+v", mine)
	}
	all, _ := k.s.ListKitCheckouts(k.ctx, k.admin)
	if len(all) != 3 {
		t.Fatalf("admins see everyone's: %+v", all)
	}
	drifted := all[slices.IndexFunc(all, func(c KitCheckout) bool { return c.Machine == "devhost" })]
	if len(drifted.Drift) != 1 || drifted.Behind || drifted.User.Username != "robin" {
		t.Fatalf("drift: %+v", drifted)
	}

	// Rolling out: admins only, onto their own online runners.
	if _, err := k.s.RolloutKit(k.ctx, sam); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member rolled out: %v", err)
	}
	if _, err := k.s.RolloutKit(k.ctx, k.admin); !IsInvalid(err) {
		t.Fatalf("no runners: %v", err)
	}
	k.runner(k.admin, "devhost", nil)
	k.runner(k.admin, "nodirs", func(r *RunnerRegistration) { r.AllowedDirs = nil })
	k.runner(sam, "sams-box", nil)
	out, err := k.s.RolloutKit(k.ctx, k.admin)
	if err != nil || len(out) != 2 {
		t.Fatalf("rollout: %+v %v", out, err)
	}
	if out[0].Runner != "devhost" || out[0].JobID == 0 || out[1].Skipped == "" {
		t.Fatalf("rollout: %+v", out)
	}
	job, err := k.s.GetRunnerJob(k.ctx, k.admin, out[0].JobID, -1)
	if err != nil {
		t.Fatal(err)
	}
	var in CommandJob
	_ = json.Unmarshal(job.Input, &in)
	if !strings.Contains(in.Command, "kit sync --all") || in.Dir != "/work" || job.Source != "kit_rollout" {
		t.Fatalf("job: %+v %+v", job, in)
	}
}

func TestKitAddOpensAPullRequest(t *testing.T) {
	k := newKitEnv(t)
	sam := k.user("sam", false)
	skill := KitAddInput{Kind: "skill", Plugin: "waypoint", Name: "tidy", Message: "Branch hygiene",
		Files: []KitAddFile{{Path: "SKILL.md", Content: "---\nname: tidy\n---\n"}, {Path: "scripts/tidy.sh", Content: "echo\n"}}}
	if _, err := k.s.AddToKit(k.ctx, sam, skill); !IsInvalid(err) {
		t.Fatalf("no kit yet: %v", err)
	}
	k.setSource()
	for _, bad := range []KitAddInput{
		{Kind: "theme", Plugin: "waypoint", Name: "x"},
		{Kind: "skill", Plugin: "Way Point", Name: "x", Files: skill.Files},
		{Kind: "skill", Plugin: "waypoint", Name: "x", Files: []KitAddFile{{Path: "README.md", Content: "x"}}},
		{Kind: "skill", Plugin: "waypoint", Name: "x", Files: []KitAddFile{{Path: "../SKILL.md", Content: "x"}}},
		{Kind: "agent", Plugin: "waypoint", Name: "x", Files: []KitAddFile{{Path: "a.md"}, {Path: "b.md"}}},
		{Kind: "command", Plugin: "waypoint", Name: "x", Files: []KitAddFile{{Path: "x.md", Content: "\x00bin"}}},
		{Kind: "hook", Plugin: "waypoint", Name: "x"},
	} {
		if _, err := k.s.AddToKit(k.ctx, sam, bad); !IsInvalid(err) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if len(k.fake.commit) != 0 {
		t.Fatal("committed something invalid")
	}

	res, err := k.s.AddToKit(k.ctx, agentActor(sam, "anthropic/claude-code"), skill)
	if err != nil {
		t.Fatal(err)
	}
	if res.PRNumber != 12 || res.NewPlugin || !strings.HasPrefix(res.Branch, "kit/add-skill-tidy-") ||
		!slices.Equal(res.Files, []string{"plugins/waypoint/skills/tidy/SKILL.md", "plugins/waypoint/skills/tidy/scripts/tidy.sh"}) {
		t.Fatalf("skill: %+v", res)
	}
	c := k.fake.commit[0]
	if c["branch"] != "main" || c["new_branch"] != res.Branch || !strings.HasPrefix(c["message"].(string), "Add skill tidy to waypoint\n\nBranch hygiene") {
		t.Fatalf("commit: %+v", c)
	}
	if pr := k.fake.prs[0]; pr["base"] != "main" || !strings.Contains(pr["body"].(string), "anthropic/claude-code (for sam)") {
		t.Fatalf("pr: %+v", pr)
	}

	// A hook into a new plugin: hooks.json is new, and the marketplace
	// gains the plugin.
	res, err = k.s.AddToKit(k.ctx, sam, KitAddInput{Kind: "hook", Plugin: "python", Name: "format_python", Description: "Python formatting",
		Files: []KitAddFile{{Path: "format_python.sh", Content: "ruff format\n"}},
		Hooks: `{"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"bash ${CLAUDE_PLUGIN_ROOT}/hooks/format_python.sh"}]}]}}`})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewPlugin || len(res.Files) != 4 {
		t.Fatalf("hook: %+v", res)
	}
	files := map[string]string{}
	for _, f := range k.fake.commit[1]["files"].([]any) {
		m := f.(map[string]any)
		b, _ := base64.StdEncoding.DecodeString(m["content"].(string))
		files[m["path"].(string)] = string(b)
		if m["operation"] != "create" && m["path"] != ".claude-plugin/marketplace.json" {
			t.Errorf("%s: %s", m["path"], m["operation"])
		}
	}
	if !strings.Contains(files["plugins/python/hooks/hooks.json"], "format_python.sh") ||
		!strings.Contains(files[".claude-plugin/marketplace.json"], `"source": "./plugins/python"`) ||
		!strings.Contains(files[".claude-plugin/marketplace.json"], `"demo"`) ||
		!strings.Contains(files["plugins/python/.claude-plugin/plugin.json"], "Python formatting") {
		t.Fatalf("files: %v", files)
	}
	if !strings.Contains(k.fake.prs[1]["body"].(string), "[plugins.python]") {
		t.Fatalf("new plugin PR body: %v", k.fake.prs[1]["body"])
	}
}
