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

	"git.jmann.me/claude/waypoint/internal/core/udiff"
)

func TestAgentFileKinds(t *testing.T) {
	for p, want := range map[string]string{
		"CLAUDE.md":                         fileInstructions,
		"web/src/CLAUDE.md":                 fileInstructions,
		"AGENTS.md":                         fileInstructions,
		"node_modules/x/CLAUDE.md":          "",
		"a/vendor/b/AGENTS.md":              "",
		".claude/settings.json":             fileSettings,
		".claude/settings.local.json":       "",
		"sub/.claude/settings.json":         "",
		".claude/agents/reviewer.md":        fileAgent,
		".claude/agents/team/reviewer.md":   fileAgent,
		".claude/commands/pr/open.md":       fileCommand,
		".claude/skills/ci/SKILL.md":        fileSkill,
		".claude/skills/ci/notes/SKILL.md":  "",
		".claude/skills/ci/reference.md":    "",
		".claude/hooks/guard.sh":            fileHookScript,
		".claude/hooks/lib/x.sh":            "",
		".mcp.json":                         fileMCP,
		"docs/.mcp.json":                    "",
		"README.md":                         "",
		"claude.md":                         "",
		".claude/agents/reviewer.md.bak":    "",
		"./.claude/commands/deploy.md":      fileCommand,
		".venv/lib/python/site/AGENTS.md":   "",
		"third_party/thing/CLAUDE.md":       "",
		"tools/third_party_notes/CLAUDE.md": fileInstructions,
	} {
		if got := agentFileKind(p); got != want {
			t.Errorf("agentFileKind(%q) = %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string]string{
		".claude/CLAUDE.md":       fileInstructions,
		".codex/AGENTS.md":        fileInstructions,
		".claude.json":            fileMCP,
		".claude/settings.json":   fileSettings,
		".claude/agents/x.md":     fileAgent,
		".bashrc":                 "",
		".claude/projects/x.json": "",
	} {
		if got := machineFileKind(p); got != want {
			t.Errorf("machineFileKind(%q) = %q, want %q", p, got, want)
		}
	}
	tree := []string{"z/y/x/CLAUDE.md", "CLAUDE.md", ".claude/agents/a.md", "README.md", "node_modules/p/CLAUDE.md"}
	if got := pickAgentFiles(tree); !slices.Equal(got, []string{".claude/agents/a.md", "CLAUDE.md", "z/y/x/CLAUDE.md"}) {
		t.Fatalf("picked %v", got)
	}
}

func TestSharedBlock(t *testing.T) {
	own := "# Repo\n\nOur own rules.\n"
	with := withSharedBlock(own, "Run tests with make check.\r\n")
	want := own + "\n" + sharedHeader + "\nRun tests with make check.\n" + sharedClose + "\n"
	if with != want {
		t.Fatalf("added:\n%q\nwant\n%q", with, want)
	}
	if b, ok := sharedBlock(with); !ok || b != "Run tests with make check." {
		t.Fatalf("read back %q %v", b, ok)
	}
	if withSharedBlock(with, "Run tests with make check.") != with {
		t.Fatal("the same text again changed the file")
	}
	after := with + "\nA line someone added below.\n"
	repl := withSharedBlock(after, "New text.")
	if !strings.HasPrefix(repl, own) || !strings.HasSuffix(repl, "\nA line someone added below.\n") || !strings.Contains(repl, "\nNew text.\n") || strings.Contains(repl, "make check") {
		t.Fatalf("replaced:\n%s", repl)
	}
	hand := "x\n<!-- waypoint:shared -->\nold\n<!-- /waypoint:shared -->\n"
	if b, ok := sharedBlock(hand); !ok || b != "old" {
		t.Fatalf("hand block %q %v", b, ok)
	}
	stray := "x\n<!-- waypoint:shared -->\nMine, keep it.\n"
	once := withSharedBlock(stray, "First.")
	if !strings.HasPrefix(once, stray) {
		t.Fatalf("stray, first publish:\n%s", once)
	}
	twice := withSharedBlock(once, "Second.")
	if twice != stray+"\n"+sharedHeader+"\nSecond.\n"+sharedClose+"\n" {
		t.Fatalf("stray, second publish:\n%q", twice)
	}
	if b, ok := sharedBlock(twice); !ok || b != "Second." {
		t.Fatalf("stray block %q %v", b, ok)
	}
	if b, ok := sharedBlock("<!-- /waypoint:shared -->\n" + with); !ok || b != "Run tests with make check." {
		t.Fatalf("after a stray close %q %v", b, ok)
	}
	if got := withSharedBlock(with, ""); got != own {
		t.Fatalf("removed: %q", got)
	}
	if got := withSharedBlock(own, ""); got != own {
		t.Fatalf("removed nothing: %q", got)
	}
	if got := withSharedBlock("", "Hi"); got != sharedHeader+"\nHi\n"+sharedClose+"\n" {
		t.Fatalf("new file: %q", got)
	}
	if _, ok := sharedBlock(own); ok {
		t.Fatal("found a block that isn't there")
	}

	for _, c := range []struct {
		claude     string
		hasC       bool
		agents     string
		hasA, want string
	}{
		{"", false, "", "", "AGENTS.md"},
		{"", false, "rules", "y", "AGENTS.md"},
		{"rules", true, "", "", "CLAUDE.md"},
		{"@AGENTS.md\n", true, "rules", "y", "AGENTS.md"},
		{"AGENTS.md", true, "rules", "y", "AGENTS.md"}, // A CLAUDE.md symlink is read as its target's path, not its text.
		{"See @AGENTS.md\n", true, "", "", "CLAUDE.md"},
		{"rules\n@AGENTS.md", true, "more", "y", "AGENTS.md"}, // An @AGENTS.md import makes AGENTS.md the file for the one block both files read.
	} {
		if got := rootInstructionFile(c.claude, c.hasC, c.agents, c.hasA == "y"); got != c.want {
			t.Errorf("root file for %q/%v %q/%v = %s, want %s", c.claude, c.hasC, c.agents, c.hasA, got, c.want)
		}
	}
}

func TestRedactSettings(t *testing.T) {
	raw := `{"env":{"GITHUB_TOKEN":"ghp_x","EDITOR":"vim"},"apiKeyHelper":"/bin/key",
		"permissions":{"allow":["Bash(make check)"]},
		"mcp":{"servers":{"x":{"headers":{"Authorization":"Bearer y"},"url":"https://x"}}},
		"my_password":"hunter2","auth":{"type":"oauth","clientSecret":"s"},"secrets":{"GH":"ghp_nested"}}`
	got, ok := redactSettings(raw)
	if !ok {
		t.Fatal("not JSON?")
	}
	for _, secret := range []string{"ghp_x", "vim", "/bin/key", "Bearer y", "hunter2", `"s"`, "ghp_nested", "oauth"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q left in %s", secret, got)
		}
	}
	for _, kept := range []string{"Bash(make check)", "https://x", "GITHUB_TOKEN"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q taken out of %s", kept, got)
		}
	}
	if _, ok := redactSettings("not json"); ok {
		t.Fatal("redacted text that isn't JSON")
	}
	m := mcpOnly(`{"mcpServers":{"github":{"env":{"TOKEN":"t"}},"indexed":{}},"oauthAccount":{"email":"x@y"}}`)
	if strings.Contains(m, "TOKEN") || strings.Contains(m, "x@y") || !strings.Contains(m, `"github"`) || !strings.Contains(m, `"indexed"`) {
		t.Fatalf("mcp only: %s", m)
	}
	if names := mcpServers(m); len(names) != 2 {
		t.Fatalf("mcp names from the kept text: %+v", names)
	}
}

func TestSettingsHooks(t *testing.T) {
	for cmd, want := range map[string]string{
		"wp hook claude": "wp hook claude",
		"/home/robin/.local/bin/wp hook claude --no-prompts": "wp hook claude",
		"bash ~/.claude/hooks/guard.sh":                      "bash guard.sh",
		"FOO=1 node /x/y/check.js --fast":                    "node check.js",
		"$CLAUDE_PROJECT_DIR/.claude/hooks/format.sh":        "format.sh",
		"/opt/tools/lint a b c d e f":                        "lint a b c",
		"":                                                   "command",
		"python3 -c 'print(1)'":                              "python3",
		"jq -r .tool_input.command | grep -q rm && exit 2":   "jq",
		"npx prettier --write":                               "npx prettier",
	} {
		if got := hookCommandName(cmd); got != want {
			t.Errorf("hookCommandName(%q) = %q, want %q", cmd, got, want)
		}
	}
	hs := settingsHooks(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"wp hook claude"},{"type":"command","command":"/usr/bin/wp hook claude"}]}],
		"Stop":[{"hooks":[{"type":"prompt","prompt":"Check the work"}]}],"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"wp hook claude"}]}]}}`)
	var names []string
	for _, h := range hs {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"PreToolUse Bash · wp hook claude", "SessionStart · wp hook claude", "Stop · prompt"}) {
		t.Fatalf("hooks: %v", names)
	}
	if len(settingsHooks("{oops")) != 0 {
		t.Fatal("hooks from broken JSON")
	}
}

func TestCompareComponents(t *testing.T) {
	c := func(names ...string) []AgentComponent {
		var out []AgentComponent
		for _, n := range names {
			out = append(out, AgentComponent{Kind: "agent", Name: n})
		}
		return out
	}
	sets := [][]AgentComponent{c("reviewer", "planner"), c("Reviewer"), c("reviewer", "odd"), c(), c("ignored")}
	missing := map[int][]string{}
	only := map[int][]string{}
	common := compareComponents(sets, func(i int, m []AgentComponent, o map[string]bool) {
		for _, x := range m {
			missing[i] = append(missing[i], x.Name)
		}
		for k := range o {
			only[i] = append(only[i], strings.SplitN(k, "\x00", 2)[1])
		}
	}, func(i int) bool { return i != 4 })
	if !slices.Equal(missing[3], []string{"reviewer"}) || len(missing[0]) != 0 || len(missing[4]) != 0 {
		t.Fatalf("missing: %v", missing)
	}
	if !slices.Equal(only[0], []string{"planner"}) || !slices.Equal(only[2], []string{"odd"}) || len(only[1]) != 0 {
		t.Fatalf("only here: %v", only)
	}
	if len(common) != 3 || common[0].Name != "reviewer" || common[0].Count != 3 || !common[0].Common || common[1].Common {
		t.Fatalf("common: %+v", common)
	}
	compareComponents([][]AgentComponent{c("a"), c()}, func(i int, m []AgentComponent, _ map[string]bool) {
		if len(m) != 0 {
			t.Fatalf("missing among two: %v", m)
		}
	}, func(int) bool { return true })
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"v0.9.0", "v0.10.0"}, {"0.25.1", "v0.25.2"}, {"v1.0.0", "v1.0.0-3-gabc"}, {"v0.2", "v0.2.1"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
	if newestVersion([]AgentMachine{{WPVersion: "v0.9.0"}, {WPVersion: "dev"}, {WPVersion: "v0.10.1"}, {}}) != "v0.10.1" {
		t.Fatal("newest")
	}
}

// fakeSetupForge is a Forgejo holding several repos, each a default branch
// "main" with files; it records what Waypoint writes.
type fakeSetupForge struct {
	t       *testing.T
	mu      sync.Mutex
	repos   map[string]*fakeSetupRepo
	commits []fakeSetupCommit
	prs     []fakeSetupPR
	closed  []string
	deleted []string
	nextPR  int
	failPR  bool
}

type fakeSetupRepo struct {
	head  string
	files map[string]string
}

type fakeSetupCommit struct {
	repo, base, branch, message string
	files                       map[string]string
	ops                         map[string]string
}

type fakeSetupPR struct {
	repo, title, body, head, base string
	number                        int
	labelled                      bool
}

func (f *fakeSetupForge) set(repo, p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	if r == nil {
		r = &fakeSetupRepo{files: map[string]string{}}
		f.repos[repo] = r
	}
	r.files[p] = content
	r.head = udiff.GitBlobSHA(fmt.Sprint(len(r.files), r.head, p, content))
}

func (f *fakeSetupForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/repos/"))
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
		return
	}
	name := parts[0] + "/" + parts[1]
	path := ""
	if len(parts) == 3 {
		path = "/" + parts[2]
	}
	repo := f.repos[name]
	if repo == nil {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == "GET" && path == "":
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	case r.Method == "GET" && path == "/branches/main":
		_, _ = fmt.Fprintf(w, `{"commit":{"id":%q}}`, repo.head)
	case r.Method == "GET" && strings.HasPrefix(path, "/git/trees/"):
		var tree []map[string]string
		for p := range repo.files {
			tree = append(tree, map[string]string{"path": p, "type": "blob"})
		}
		sort.Slice(tree, func(i, j int) bool { return tree[i]["path"] < tree[j]["path"] })
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
	case r.Method == "GET" && strings.HasPrefix(path, "/raw/"):
		if c, ok := repo.files[strings.TrimPrefix(path, "/raw/")]; ok {
			_, _ = w.Write([]byte(c))
			return
		}
		w.WriteHeader(404)
	case r.Method == "GET" && strings.HasPrefix(path, "/contents/"):
		if c, ok := repo.files[strings.TrimPrefix(path, "/contents/")]; ok {
			_, _ = fmt.Fprintf(w, `{"sha":%q,"type":"file"}`, udiff.GitBlobSHA(c))
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && path == "/contents":
		c := fakeSetupCommit{repo: name, base: fmt.Sprint(body["branch"]), branch: fmt.Sprint(body["new_branch"]), message: fmt.Sprint(body["message"]),
			files: map[string]string{}, ops: map[string]string{}}
		files, _ := body["files"].([]any)
		for _, x := range files {
			m, _ := x.(map[string]any)
			p := fmt.Sprint(m["path"])
			raw, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["content"]))
			c.files[p], c.ops[p] = string(raw), fmt.Sprint(m["operation"])
			if sha, _ := m["sha"].(string); c.ops[p] == "update" && sha != udiff.GitBlobSHA(repo.files[p]) {
				w.WriteHeader(409)
				return
			}
		}
		f.commits = append(f.commits, c)
		_, _ = w.Write([]byte(`{"commit":{"sha":"c0ffee1234567"}}`))
	case r.Method == "GET" && path == "/labels":
		_, _ = w.Write([]byte(`[{"id":7,"name":"automerge"}]`))
	case r.Method == "POST" && path == "/pulls":
		if f.failPR {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"message":"no"}`))
			return
		}
		f.nextPR++
		labels, _ := body["labels"].([]any)
		f.prs = append(f.prs, fakeSetupPR{repo: name, title: fmt.Sprint(body["title"]), body: fmt.Sprint(body["body"]), head: fmt.Sprint(body["head"]),
			base: fmt.Sprint(body["base"]), number: f.nextPR, labelled: len(labels) == 1 && labels[0] == float64(7)})
		_, _ = fmt.Fprintf(w, `{"number":%d,"title":%q,"html_url":"https://git.example.com/%s/pulls/%d","state":"open"}`, f.nextPR, body["title"], name, f.nextPR)
	case r.Method == "PATCH" && strings.HasPrefix(path, "/pulls/"):
		f.closed = append(f.closed, name+"#"+strings.TrimPrefix(path, "/pulls/"))
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "DELETE" && strings.HasPrefix(path, "/branches/"):
		f.deleted = append(f.deleted, name+":"+strings.TrimPrefix(path, "/branches/"))
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}
}

// prsTo are the pull requests opened on a repo.
func (f *fakeSetupForge) prsTo(repo string) []fakeSetupPR {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeSetupPR
	for _, p := range f.prs {
		if p.repo == repo {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeSetupForge) lastCommit(repo string) fakeSetupCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.commits) - 1; i >= 0; i-- {
		if f.commits[i].repo == repo {
			return f.commits[i]
		}
	}
	f.t.Fatalf("no commit to %s", repo)
	return fakeSetupCommit{}
}

type setupEnv struct {
	*env
	fake    *fakeSetupForge
	forgeID int64
	acme    *Project
	other   *Project
}

const reviewerAgent = "---\nname: reviewer\ndescription: Reviews diffs\n---\nReview.\n"

func newSetupEnv(t *testing.T) *setupEnv {
	e := newEnv(t)
	fake := &fakeSetupForge{t: t, repos: map[string]*fakeSetupRepo{}}
	fake.set("acme/one", "CLAUDE.md", "# One\n\nOur rules.\n")
	fake.set("acme/one", "docs/CLAUDE.md", "Docs rules.\n")
	fake.set("acme/one", "node_modules/dep/CLAUDE.md", "not ours\n")
	fake.set("acme/one", ".claude/agents/reviewer.md", reviewerAgent)
	fake.set("acme/one", ".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"wp hook claude"}]}]}}`)
	fake.set("acme/one", "main.go", "package main\n")
	fake.set("acme/two", "CLAUDE.md", "@AGENTS.md\n")
	fake.set("acme/two", "AGENTS.md", "# Two\n")
	fake.set("acme/two", ".claude/agents/reviewer.md", reviewerAgent)
	fake.set("acme/three", "README.md", "three\n")
	fake.set("acme/four", "CLAUDE.md", "# Four\n")
	fake.set("acme/four", ".claude/agents/reviewer.md", reviewerAgent)
	fake.set("acme/four", ".claude/agents/four-only.md", "Four's own.\n")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	forge, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr(srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	se := &setupEnv{env: e, fake: fake, forgeID: forge.ID, acme: e.project(e.admin, "Acme", "kanban"), other: e.project(e.admin, "Other", "kanban")}
	for _, x := range []struct {
		p    *Project
		repo string
	}{{se.acme, "acme/one"}, {se.acme, "acme/two"}, {se.acme, "acme/three"}, {se.other, "acme/four"}} {
		if _, err := e.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?,?,1)`, x.p.ID, forge.ID, x.repo); err != nil {
			t.Fatal(err)
		}
	}
	off := ProjectSettings{Mode: "kanban", NoSharedSetup: true}
	if _, err := e.s.UpdateProject(e.ctx, e.admin, se.other.Key, ProjectInput{Settings: &off}); err != nil {
		t.Fatal(err)
	}
	return se
}

func (se *setupEnv) setup(a *Actor) *AgentSetup {
	se.t.Helper()
	out, err := se.s.GetAgentSetup(se.ctx, a)
	if err != nil {
		se.t.Fatal(err)
	}
	return out
}

func (se *setupEnv) repo(out *AgentSetup, name string) *AgentRepo {
	se.t.Helper()
	for i := range out.Repos {
		if out.Repos[i].Repo == name {
			return &out.Repos[i]
		}
	}
	se.t.Fatalf("%s not on the page: %+v", name, out.Repos)
	return nil
}

func (se *setupEnv) refresh() {
	se.t.Helper()
	se.s.RefreshAgentSetup(se.ctx)
	se.s.Wait()
}

func TestAgentSetupReadsRepos(t *testing.T) {
	se := newSetupEnv(t)
	sam := se.user("sam", false)
	if err := se.s.SetMember(se.ctx, se.admin, se.acme.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}

	out := se.setup(se.admin)
	if len(out.Repos) != 4 || out.Repos[0].Status != "unread" || out.Repos[0].Shared != sharedUnread {
		t.Fatalf("unread: %+v", out.Repos)
	}
	se.refresh()
	out = se.setup(se.admin)
	one := se.repo(out, "acme/one")
	var paths []string
	for _, f := range one.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"CLAUDE.md", "docs/CLAUDE.md", ".claude/agents/reviewer.md", ".claude/settings.json"}) {
		t.Fatalf("one's files: %v", paths)
	}
	if one.Head == "" || one.Branch != "main" || one.RootFile != "CLAUDE.md" || one.Shared != sharedNone || !one.CanEdit || !strings.HasSuffix(one.URL, "/acme/one") {
		t.Fatalf("one: %+v", one)
	}
	var comps []string
	for _, c := range one.Components {
		comps = append(comps, c.Kind+":"+c.Name)
	}
	if !slices.Equal(comps, []string{"agent:reviewer", "hook:PreToolUse Bash · wp hook claude"}) {
		t.Fatalf("one's components: %v", comps)
	}
	if !one.Components[1].OnlyHere || one.Components[0].OnlyHere {
		t.Fatalf("only here: %+v", one.Components)
	}
	three := se.repo(out, "acme/three")
	if three.Status != checkFail || out.Repos[0].Repo != "acme/three" || len(three.Missing) != 1 || three.Missing[0].Name != "reviewer" || three.RootFile != "AGENTS.md" {
		t.Fatalf("three (no instructions, no reviewer) first: %+v", three)
	}
	if two := se.repo(out, "acme/two"); two.RootFile != "AGENTS.md" {
		t.Fatalf("two points at AGENTS.md: %+v", two)
	}
	if four := se.repo(out, "acme/four"); !four.OptedOut || !four.Components[0].OnlyHere {
		t.Fatalf("four: %+v", four)
	}
	if len(out.RepoCommon) == 0 || out.RepoCommon[0].Name != "reviewer" || out.RepoCommon[0].Count != 3 || !out.RepoCommon[0].Common {
		t.Fatalf("common: %+v", out.RepoCommon)
	}

	mine := se.setup(sam)
	if len(mine.Repos) != 3 || mine.CanManage || !mine.CanEdit || mine.Repos[0].CanEdit {
		t.Fatalf("sam's: %+v", mine)
	}
	if _, err := se.s.GetAgentFile(se.ctx, sam, se.forgeID, "acme/four", "CLAUDE.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sam read another project's file: %v", err)
	}
	fc, err := se.s.GetAgentFile(se.ctx, sam, se.forgeID, "acme/one", "CLAUDE.md")
	if err != nil || fc.Content != "# One\n\nOur rules.\n" || fc.Editable || fc.SHA != udiff.GitBlobSHA(fc.Content) || !strings.Contains(fc.URL, "/acme/one/src/branch/main/CLAUDE.md") {
		t.Fatalf("sam's read: %+v %v", fc, err)
	}

	se.fake.set("acme/one", "CLAUDE.md", "# One\n\nNew rules.\n")
	se.refresh()
	if fc, _ := se.s.GetAgentFile(se.ctx, se.admin, se.forgeID, "acme/one", "CLAUDE.md"); fc.Content != "# One\n\nNew rules.\n" {
		t.Fatalf("not read again: %q", fc.Content)
	}
	se.fake.set("acme/two", "AGENTS.md", "# Two, pushed\n")
	f, _ := se.s.forgeByID(se.ctx, se.forgeID)
	se.s.onPush(se.ctx, f, &forgeEvent{repo: "acme/two", ref: "refs/heads/feature", commits: []forgeCommit{{paths: []string{"AGENTS.md"}}}})
	se.s.Wait()
	if fc, _ := se.s.GetAgentFile(se.ctx, se.admin, se.forgeID, "acme/two", "AGENTS.md"); fc.Content != "# Two\n" {
		t.Fatalf("a push to another branch was read: %q", fc.Content)
	}
	se.s.onPush(se.ctx, f, &forgeEvent{repo: "acme/two", ref: "refs/heads/main", commits: []forgeCommit{{paths: []string{"AGENTS.md"}}}})
	se.s.Wait()
	if fc, _ := se.s.GetAgentFile(se.ctx, se.admin, se.forgeID, "acme/two", "AGENTS.md"); fc.Content != "# Two, pushed\n" {
		t.Fatalf("the push wasn't read: %q", fc.Content)
	}

	se.fake.mu.Lock()
	delete(se.fake.repos, "acme/one")
	se.fake.mu.Unlock()
	if err := se.s.scanAgentRepo(se.ctx, se.forgeID, "acme/one", true); err == nil {
		t.Fatal("no error for a repo the forge lost")
	}
	one = se.repo(se.setup(se.admin), "acme/one")
	if one.Error == "" || one.Status != checkFail || len(one.Files) != 4 {
		t.Fatalf("failed read: %+v", one)
	}
}

func TestAgentSetupSharedInstructions(t *testing.T) {
	se := newSetupEnv(t)
	sam := se.user("sam", false)
	se.refresh()

	if _, err := se.s.SaveSharedInstructions(se.ctx, sam, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member saved: %v", err)
	}
	if _, err := se.s.SaveSharedInstructions(se.ctx, agentActor(se.admin, "anthropic/claude-code"), "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent saved: %v", err)
	}
	if _, err := se.s.SaveSharedInstructions(se.ctx, se.admin, "a <!-- /waypoint:shared --> b"); !IsInvalid(err) {
		t.Fatalf("markers saved: %v", err)
	}
	if _, err := se.s.PublishShared(se.ctx, se.admin); !IsInvalid(err) {
		t.Fatalf("published nothing: %v", err)
	}
	v1, err := se.s.SaveSharedInstructions(se.ctx, se.admin, "  Run tests with make check.\n")
	if err != nil || v1.Version != 1 || v1.Text != "Run tests with make check." || v1.SavedBy == nil {
		t.Fatalf("v1: %+v %v", v1, err)
	}
	if again, _ := se.s.SaveSharedInstructions(se.ctx, se.admin, "Run tests with make check."); again.Version != 1 {
		t.Fatalf("the same text made a version: %+v", again)
	}
	out := se.setup(se.admin)
	if out.Shared.Current == nil || out.Shared.Counts[sharedMissing] != 4 || !out.CanManage {
		t.Fatalf("after saving: %+v", out.Shared)
	}

	se.refresh()
	for repo, file := range map[string]string{"acme/one": "CLAUDE.md", "acme/two": "AGENTS.md", "acme/three": "AGENTS.md"} {
		prs := se.fake.prsTo(repo)
		if len(prs) != 1 || !prs[0].labelled || prs[0].base != "main" || !strings.HasPrefix(prs[0].head, "waypoint/shared-v1-") {
			t.Fatalf("%s's pull request: %+v", repo, prs)
		}
		c := se.fake.lastCommit(repo)
		b, ok := sharedBlock(c.files[file])
		if !ok || b != "Run tests with make check." || len(c.files) != 1 {
			t.Fatalf("%s's commit: %+v", repo, c)
		}
	}
	if c := se.fake.lastCommit("acme/one"); !strings.HasPrefix(c.files["CLAUDE.md"], "# One\n\nOur rules.\n\n<!-- waypoint:shared") || c.ops["CLAUDE.md"] != "update" {
		t.Fatalf("one's own lines: %+v", c)
	}
	if c := se.fake.lastCommit("acme/three"); c.ops["AGENTS.md"] != "create" {
		t.Fatalf("three gets a new AGENTS.md: %+v", c)
	}
	if len(se.fake.prsTo("acme/four")) != 0 {
		t.Fatal("an opted-out repo got a pull request")
	}
	out = se.setup(se.admin)
	if one := se.repo(out, "acme/one"); one.SharedPR == nil || one.SharedPR.Version != 1 || one.Shared != sharedMissing ||
		!strings.Contains(one.Checks[len(one.Checks)-1].Note, "is open") {
		t.Fatalf("one with its pull request open: %+v", one)
	}
	if four := se.repo(out, "acme/four"); !strings.Contains(four.Checks[len(four.Checks)-1].Note, "turned automatic changes off") {
		t.Fatalf("four's note: %+v", four.Checks)
	}
	se.refresh()
	if n := len(se.fake.prsTo("acme/one")); n != 1 {
		t.Fatalf("a second pull request for the same version: %d", n)
	}

	se.fake.set("acme/one", "CLAUDE.md", se.fake.lastCommit("acme/one").files["CLAUDE.md"])
	if _, err := se.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, created_at, updated_at) VALUES (?, 'acme/one', 1, 'u', 'merged', 0, 0)`, se.forgeID); err != nil {
		t.Fatal(err)
	}
	se.refresh()
	if one := se.repo(se.setup(se.admin), "acme/one"); one.Shared != sharedCurrent || one.SharedVersion != 1 {
		t.Fatalf("merged: %+v", one)
	}

	if _, err := se.s.PublishShared(se.ctx, sam); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member published: %v", err)
	}
	if _, err := se.s.SaveSharedInstructions(se.ctx, se.admin, "Run tests with make check.\nShip with wp ship."); err != nil {
		t.Fatal(err)
	}
	if one := se.repo(se.setup(se.admin), "acme/one"); one.Shared != sharedBehind || one.SharedVersion != 1 {
		t.Fatalf("one behind: %+v", one)
	}
	res, err := se.s.PublishShared(se.ctx, se.admin)
	if err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	sort.Strings(res.Repos)
	if res.Version != 2 || !slices.Equal(res.Repos, []string{"acme/one", "acme/three", "acme/two"}) || !slices.Equal(res.Skipped, []string{"acme/four"}) {
		t.Fatalf("publish: %+v", res)
	}
	se.fake.mu.Lock()
	closed, deleted := slices.Clone(se.fake.closed), se.fake.deleted
	se.fake.mu.Unlock()
	sort.Strings(closed)
	if !slices.Equal(closed, []string{"acme/three#2", "acme/two#3"}) || len(deleted) != 2 {
		t.Fatalf("older pull requests closed: %v, branches %v", closed, deleted)
	}
	if res, _ := se.s.PublishShared(se.ctx, se.admin); len(res.Repos) != 0 {
		t.Fatalf("published twice: %+v", res)
	}
	se.s.Wait()

	se.fake.set("acme/one", "CLAUDE.md", withSharedBlock("# One\n", "My own version."))
	se.refresh()
	if one := se.repo(se.setup(se.admin), "acme/one"); one.Shared != sharedEdited {
		t.Fatalf("edited: %+v", one)
	}

	se.fake.mu.Lock()
	se.fake.failPR = true
	se.fake.mu.Unlock()
	if _, err := se.s.SaveSharedInstructions(se.ctx, se.admin, "Version three."); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.PublishShared(se.ctx, se.admin); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if one := se.repo(se.setup(se.admin), "acme/one"); !strings.Contains(one.SharedError, "could not open the pull request") || one.PRs[0].State != "failed" {
		t.Fatalf("refused: %+v", one)
	}
}

func TestAgentSetupEditFile(t *testing.T) {
	se := newSetupEnv(t)
	sam := se.user("sam", false)
	if err := se.s.SetMember(se.ctx, se.admin, se.acme.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	se.refresh()
	fc, err := se.s.GetAgentFile(se.ctx, se.admin, se.forgeID, "acme/one", "CLAUDE.md")
	if err != nil || !fc.Editable {
		t.Fatalf("admin's read: %+v %v", fc, err)
	}
	edit := AgentFileEdit{ForgeID: se.forgeID, Repo: "acme/one", Path: "CLAUDE.md", Content: fc.Content + "More rules.\n", BaseSHA: fc.SHA, Message: "Say more\n\nThe rules were thin."}
	if _, err := se.s.EditAgentFile(se.ctx, sam, edit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer edited: %v", err)
	}
	if _, err := se.s.EditAgentFile(se.ctx, agentActor(se.admin, "anthropic/claude-code"), edit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent edited: %v", err)
	}
	bad := edit
	bad.Path = "main.go"
	if _, err := se.s.EditAgentFile(se.ctx, se.admin, bad); !IsInvalid(err) {
		t.Fatalf("edited a file that isn't an agent file: %v", err)
	}
	same := edit
	same.Content = fc.Content
	if _, err := se.s.EditAgentFile(se.ctx, se.admin, same); !IsInvalid(err) {
		t.Fatalf("an edit changing nothing: %v", err)
	}
	if err := se.s.SetMember(se.ctx, se.admin, se.acme.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	res, err := se.s.EditAgentFile(se.ctx, sam, edit)
	if err != nil || res.Number == 0 || !strings.HasPrefix(res.Branch, "waypoint/edit-claude-") {
		t.Fatalf("sam's edit: %+v %v", res, err)
	}
	prs := se.fake.prsTo("acme/one")
	if len(prs) != 1 || !prs[0].labelled || prs[0].title != "Say more" || !strings.Contains(prs[0].body, "The rules were thin.") || !strings.Contains(prs[0].body, "Update CLAUDE.md") {
		t.Fatalf("the edit's pull request: %+v", prs)
	}
	if c := se.fake.lastCommit("acme/one"); c.files["CLAUDE.md"] != edit.Content || !strings.HasPrefix(c.message, "Say more\n\nUpdate CLAUDE.md.\n\nThe rules were thin.") {
		t.Fatalf("the edit's commit: %+v", c)
	}
	se.fake.set("acme/one", "CLAUDE.md", "someone else's\n")
	if _, err := se.s.EditAgentFile(se.ctx, sam, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("edited a moved file: %v", err)
	}
	nf := AgentFileEdit{ForgeID: se.forgeID, Repo: "acme/three", Path: "AGENTS.md", Content: "# Three\n"}
	if res, err := se.s.EditAgentFile(se.ctx, se.admin, nf); err != nil || res.Number == 0 {
		t.Fatalf("a new file: %+v %v", res, err)
	}
	if c := se.fake.lastCommit("acme/three"); c.ops["AGENTS.md"] != "create" {
		t.Fatalf("new file op: %+v", c)
	}
	if one := se.repo(se.setup(se.admin), "acme/one"); len(one.PRs) != 1 || one.PRs[0].Kind != agentPREdit || one.PRs[0].OpenedBy == nil || one.PRs[0].OpenedBy.Username != "sam" {
		t.Fatalf("the edit on the page: %+v", one.PRs)
	}
}

func TestAgentSetupMachines(t *testing.T) {
	se := newSetupEnv(t)
	sam := se.user("sam", false)
	bob := se.user("bob", false)
	report := MachineReport{Machine: "laptop\x07", OS: "linux", WPVersion: "v0.24.0",
		Files: []MachineFile{
			{Path: ".claude/CLAUDE.md", Content: "Be brief.\n"},
			{Path: ".claude/settings.json", Content: `{"env":{"OPENAI_API_KEY":"sk-123"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"wp hook claude"}]}]}}`},
			{Path: ".claude.json", Content: `{"mcpServers":{"github":{"env":{"GITHUB_TOKEN":"ghp_1"}}},"oauthAccount":{"emailAddress":"sam@example.com"}}`},
			{Path: ".claude/agents/helper.md", Content: "---\nname: helper\n---\n"},
			{Path: "../etc/passwd", Content: "root"},
			{Path: ".bashrc", Content: "export X=1"},
		},
		Checks: []SetupCheck{{Key: "hooks", Label: "wp hooks installed", State: checkOK}, {Key: "x", Label: "bogus", State: "great"}},
	}
	if err := se.s.ReportMachine(se.ctx, sam, report); err != nil {
		t.Fatal(err)
	}
	if err := se.s.ReportMachine(se.ctx, se.admin, MachineReport{Machine: "devhost", WPVersion: "v0.25.1"}); err != nil {
		t.Fatal(err)
	}
	if err := se.s.ReportMachine(se.ctx, sam, MachineReport{}); !IsInvalid(err) {
		t.Fatalf("a report without a name: %v", err)
	}
	mine := se.setup(sam).Machines
	if len(mine) != 1 || mine[0].Machine != "laptop" || len(mine[0].Files) != 4 || len(mine[0].Checks) != 1 {
		t.Fatalf("sam's machine: %+v", mine)
	}
	var comps []string
	for _, c := range mine[0].Components {
		comps = append(comps, c.Kind+":"+c.Name)
	}
	if !slices.Equal(comps, []string{"agent:helper", "hook:Stop · wp hook claude", "mcp:github"}) {
		t.Fatalf("components: %v", comps)
	}
	all := se.setup(se.admin).Machines
	if len(all) != 2 {
		t.Fatalf("admins see every machine: %+v", all)
	}
	for _, m := range all {
		if m.Machine == "laptop" && !slices.ContainsFunc(m.Checks, func(c SetupCheck) bool { return c.Key == "wp_current" && c.State == checkWarn }) {
			t.Fatalf("an older wp isn't flagged: %+v", m.Checks)
		}
	}
	if len(se.setup(bob).Machines) != 0 {
		t.Fatal("bob sees others' machines")
	}
	if _, err := se.s.GetMachineFile(se.ctx, bob, sam.id(), "laptop", ".claude/CLAUDE.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob read sam's file: %v", err)
	}
	for p, gone := range map[string]string{".claude/settings.json": "sk-123", ".claude.json": "ghp_1"} {
		fc, err := se.s.GetMachineFile(se.ctx, se.admin, sam.id(), "laptop", p)
		if err != nil || strings.Contains(fc.Content, gone) || strings.Contains(fc.Content, "sam@example.com") {
			t.Fatalf("%s kept a secret: %+v %v", p, fc, err)
		}
	}
	report.Files = report.Files[:1]
	report.Machine = "laptop"
	if err := se.s.ReportMachine(se.ctx, sam, report); err != nil {
		t.Fatal(err)
	}
	if m := se.setup(sam).Machines; len(m) != 1 || len(m[0].Files) != 1 {
		t.Fatalf("replaced: %+v", m)
	}
}

func TestRedactSettingsHooksAndAttached(t *testing.T) {
	got, _ := redactSettings(`{"hooks":{"SessionStart":[{"hooks":[{"command":"wp hook"}]}]},"mcpServers":{"a":{"url":"https://hooks.test/T0/B1/xyz","args":["-HAuthorization: Bearer zz","u"]}}}`)
	for _, bad := range []string{"zz", "T0/B1"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked: %s", bad, got)
		}
	}
	if !strings.Contains(got, "wp hook") {
		t.Fatalf("hook lost: %s", got)
	}
}

func TestRedactSettingsHookEntriesCommandsAndFragments(t *testing.T) {
	got, _ := redactSettings(`{"hooks":{"SessionStart":[{"hooks":[{"command":"API_TOKEN='my secret' wp hook --token abc","apiKey":"k-1"}]}]},"mcpServers":{"a":{"url":"https://x.test/#access_token=frag9"}}}`)
	for _, bad := range []string{"my secret", "abc", "k-1", "frag9"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked: %s", bad, got)
		}
	}
	if !strings.Contains(got, "SessionStart") || !strings.Contains(got, "wp hook") {
		t.Fatalf("hook lost: %s", got)
	}
	if m := mcpOnly(`{"projects":{"/p":{"mcpServers":{"proj":{}}}}}`); !strings.Contains(m, "proj") {
		t.Fatalf("project server lost: %s", m)
	}
}

func TestRedactCommandHeadersAndKeyFlags(t *testing.T) {
	for _, in := range []string{
		`curl -H 'Authorization: Bearer sekrit' x`, `curl -H'X-A: sekrit' x`, `curl --header "X-A: sekrit" x`,
		`curl --header=sekrit x`, `tool --key sekrit`, `tool --pat sekrit`,
	} {
		if got := redactCommand(in); strings.Contains(got, "sekrit") {
			t.Errorf("redactCommand(%q) = %q", in, got)
		}
	}
	if got := redactArgs([]any{"--key", "sekrit"}); got[1] != redacted {
		t.Errorf("args = %v", got)
	}
}

func TestRedactArgsBareAssignAndNonString(t *testing.T) {
	got := redactArgs([]any{"run", "-e", "API_TOKEN=abc", "--token", 12345, "next"})
	if got[2] != "API_TOKEN="+redacted || got[4] != redacted || got[5] != "next" {
		t.Fatalf("got %v", got)
	}
}
