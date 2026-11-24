package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestPlanKitSync(t *testing.T) {
	want := kitWant{Marketplace: "agent-kit", GitURL: "https://git.example/Robin/agent-kit.git", Commit: "c2", Plugins: []string{"waypoint", "python"}, All: []string{"waypoint", "python", "old", "mine"}}
	kinds := func(p kitPlan) []string {
		var out []string
		for _, a := range p.Actions {
			out = append(out, a.Kind+" "+a.Plugin)
		}
		return out
	}
	cases := []struct {
		name    string
		have    kitHave
		force   bool
		actions []string
		drift   int
		managed []string
	}{
		{name: "fresh machine",
			have:    kitHave{},
			actions: []string{"marketplace-add ", "install waypoint@agent-kit", "install python@agent-kit"},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "in sync: nothing to do",
			have: kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"},
				Enabled: map[string]bool{"waypoint@agent-kit": true, "python@agent-kit": true}, Managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "kit moved on: update the marketplace and the plugins",
			have: kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c1", Installed: map[string]string{"waypoint@agent-kit": "c1", "python@agent-kit": "c2"},
				Enabled: map[string]bool{"waypoint@agent-kit": true, "python@agent-kit": true}, Managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
			actions: []string{"marketplace-update ", "update waypoint@agent-kit"},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "no longer targeted: disable what wp enabled, leave what the person enabled",
			have: kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2", "old@agent-kit": "c2", "mine@agent-kit": "c2"},
				Enabled: map[string]bool{"waypoint@agent-kit": true, "python@agent-kit": true, "old@agent-kit": true, "mine@agent-kit": true}, Managed: []string{"old@agent-kit", "python@agent-kit", "waypoint@agent-kit"}},
			actions: []string{"disable old@agent-kit"},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "a person disabled a kit plugin after wp enabled it: drift, left alone",
			have: kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"},
				Enabled: map[string]bool{"waypoint@agent-kit": false, "python@agent-kit": true}, Managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
			drift:   1,
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "drift with --force: enabled again",
			have: kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"},
				Enabled: map[string]bool{"waypoint@agent-kit": false, "python@agent-kit": true}, Managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
			force:   true,
			actions: []string{"enable waypoint@agent-kit"},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "disabled before wp ever managed it: drift, not wp's",
			have:    kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"python@agent-kit": "c2"}, Enabled: map[string]bool{"waypoint@agent-kit": false, "python@agent-kit": true}},
			drift:   1,
			managed: []string{"python@agent-kit"}},
		{name: "installed but not enabled",
			have:    kitHave{MarketplaceURL: want.GitURL, MarketplaceCommit: "c2", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"}, Enabled: map[string]bool{"python@agent-kit": true}},
			actions: []string{"enable waypoint@agent-kit"},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "marketplace from elsewhere: drift",
			have:    kitHave{MarketplaceURL: "dir:/home/robin/agent-kit", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"}, Enabled: map[string]bool{"waypoint@agent-kit": true, "python@agent-kit": true}},
			drift:   1,
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
		{name: "marketplace from elsewhere, --force: replaced",
			have:    kitHave{MarketplaceURL: "dir:/home/robin/agent-kit", Installed: map[string]string{"waypoint@agent-kit": "c2", "python@agent-kit": "c2"}, Enabled: map[string]bool{"waypoint@agent-kit": true, "python@agent-kit": true}},
			force:   true,
			actions: []string{"marketplace-remove ", "marketplace-add "},
			managed: []string{"python@agent-kit", "waypoint@agent-kit"}},
	}
	for _, c := range cases {
		p := planKitSync(want, c.have, c.force)
		if got := kinds(p); !slices.Equal(got, c.actions) {
			t.Errorf("%s: actions %q, want %q", c.name, got, c.actions)
		}
		if len(p.Drift) != c.drift {
			t.Errorf("%s: drift %+v", c.name, p.Drift)
		}
		if !slices.Equal(p.Managed, c.managed) {
			t.Errorf("%s: managed %v, want %v", c.name, p.Managed, c.managed)
		}
	}
}

func TestMergeSettingsKeepsOtherKeys(t *testing.T) {
	raw := []byte(`{"permissions":{"allow":["Bash(make check)"]},"enabledPlugins":{"mine@other":true,"waypoint@agent-kit":false},"env":{"A":"1"}}`)
	on, off := true, false
	out, err := mergeSettings(raw, map[string]*bool{"waypoint@agent-kit": &on, "old@agent-kit": &off, "mine@other": nil},
		map[string]any{"agent-kit": map[string]any{"source": map[string]string{"source": "git", "url": "u"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	ep := got["enabledPlugins"].(map[string]any)
	if ep["waypoint@agent-kit"] != true || ep["old@agent-kit"] != false || ep["mine@other"] != nil {
		t.Fatalf("enabledPlugins: %v", ep)
	}
	if got["permissions"].(map[string]any)["allow"].([]any)[0] != "Bash(make check)" || got["env"].(map[string]any)["A"] != "1" || got["extraKnownMarketplaces"] == nil {
		t.Fatalf("other keys: %s", out)
	}
	// A new file; a broken one is an error, not overwritten.
	if out, err := mergeSettings(nil, map[string]*bool{"a@b": &on}, nil); err != nil || !strings.Contains(string(out), `"a@b": true`) {
		t.Fatalf("new: %s %v", out, err)
	}
	if _, err := mergeSettings([]byte("{"), map[string]*bool{"a@b": &on}, nil); err == nil {
		t.Fatal("merged into broken JSON")
	}
	d := lineDiff("a\nb\nc\n", "a\nc\nd\n")
	if d != "  a\n- b\n  c\n+ d\n" {
		t.Fatalf("diff:\n%s", d)
	}
}

// fakePluginCLI stands in for `claude plugin`, changing Claude Code's files the
// way the real one does (as checked against 2.1.309).
type fakePluginCLI struct {
	t       *testing.T
	cfg     string // CLAUDE_CONFIG_DIR
	mktRepo string // the marketplace clone
	commit  string
	calls   []string
	fail    string // a call that fails
}

func (f *fakePluginCLI) Run(ctx context.Context, dir string, args ...string) error {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.Contains(call, f.fail) {
		return &exec.ExitError{}
	}
	plugins := filepath.Join(f.cfg, "plugins")
	_ = os.MkdirAll(plugins, 0o755)
	readJ := func(name string, v any) { b, _ := os.ReadFile(filepath.Join(plugins, name)); _ = json.Unmarshal(b, v) }
	writeJ := func(name string, v any) {
		b, _ := json.Marshal(v)
		_ = os.WriteFile(filepath.Join(plugins, name), b, 0o644)
	}
	setEnabled := func(id string, v bool) {
		path := filepath.Join(dir, ".claude", "settings.local.json")
		raw, _ := os.ReadFile(path)
		out, err := mergeSettings(raw, map[string]*bool{id: &v}, nil)
		if err != nil {
			f.t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, out, 0o644)
	}
	switch {
	case args[0] == "marketplace" && args[1] == "add":
		writeJ("known_marketplaces.json", map[string]any{"agent-kit": map[string]any{"source": map[string]string{"source": "git", "url": args[2]}, "installLocation": f.mktRepo}})
	case args[0] == "marketplace":
	case args[0] == "install" || args[0] == "update":
		inst := map[string]any{}
		readJ("installed_plugins.json", &inst)
		ps, _ := inst["plugins"].(map[string]any)
		if ps == nil {
			ps = map[string]any{}
		}
		ps[args[1]] = []map[string]string{{"scope": "local", "projectPath": dir, "gitCommitSha": f.commit}}
		writeJ("installed_plugins.json", map[string]any{"version": 2, "plugins": ps})
		if args[0] == "install" {
			setEnabled(args[1], true)
		}
	case args[0] == "enable":
		setEnabled(args[1], true)
	case args[0] == "disable":
		setEnabled(args[1], false)
	}
	return nil
}

type kitServer struct {
	mu       sync.Mutex
	resolved map[string]any
	reports  []map[string]any
	resolves int
}

func (k *kitServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/kit/resolve":
		k.resolves++
		_ = json.NewEncoder(w).Encode(k.resolved)
	case "/api/v1/kit/reports":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.reports = append(k.reports, body)
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

type kitTestEnv struct {
	t      *testing.T
	srv    *kitServer
	claude *fakePluginCLI
	dir    string
	out    *bytes.Buffer
	c      *cli
}

func newKitTestEnv(t *testing.T) *kitTestEnv {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	// The marketplace's clone: a git repo with one commit.
	mkt := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "kit"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = mkt
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	commit := gitHead(mkt)
	srv := &kitServer{resolved: map[string]any{
		"repo": "acme/app", "attached": true, "commit": commit, "marketplace": "agent-kit", "git_url": "https://git.example/Robin/agent-kit.git",
		"plugins": []map[string]any{{"name": "waypoint"}, {"name": "indexed", "when": []string{"exists:.indexed"}}},
		"notes":   []map[string]any{{"text": "Wait on CI with wp pr wait."}, {"text": "Only on gpu boxes.", "machines": []string{"gpu-box-nowhere"}}},
		"all":     []string{"waypoint", "indexed", "demo", "mine"},
	}}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	fc := &fakePluginCLI{t: t, cfg: cfg, mktRepo: mkt, commit: commit}
	old := kitClaude
	kitClaude = func() claudePlugins { return fc }
	t.Cleanup(func() { kitClaude = old })
	dir := gitRepo(t, "https://git.example/acme/app.git")
	dir, _ = filepath.EvalSymlinks(dir)
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg = Config{Server: ts.URL, Token: "wpt_x"}
	return &kitTestEnv{t: t, srv: srv, claude: fc, dir: dir, out: &out, c: c}
}

func (k *kitTestEnv) sync(o kitSyncOpts) *kitSyncResult {
	k.t.Helper()
	k.out.Reset()
	res, err := k.c.syncKitDir(context.Background(), k.dir, nil, o)
	if err != nil {
		k.t.Fatalf("sync: %v", err)
	}
	return res
}

func TestKitSync(t *testing.T) {
	k := newKitTestEnv(t)
	// A plugin the person enabled themselves, and other settings, stay.
	_ = os.MkdirAll(filepath.Join(k.dir, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(k.dir, ".claude", "settings.local.json"), []byte(`{"permissions":{"allow":["Bash(ls)"]},"enabledPlugins":{"mine@agent-kit":true}}`), 0o644)

	// --check plans and changes nothing; --diff shows the settings.
	res := k.sync(kitSyncOpts{check: true, diff: true})
	if len(k.claude.calls) != 0 || len(k.srv.reports) != 0 || len(res.Plan.Actions) != 2 {
		t.Fatalf("check changed something: %v %+v", k.claude.calls, res.Plan)
	}
	if !strings.Contains(k.out.String(), `+     "waypoint@agent-kit": true`) || !strings.Contains(k.out.String(), "would change") {
		t.Fatalf("check output:\n%s", k.out)
	}

	k.sync(kitSyncOpts{})
	if !slices.Equal(k.claude.calls, []string{"marketplace add https://git.example/Robin/agent-kit.git --scope user", "install waypoint@agent-kit --scope local"}) {
		t.Fatalf("first sync: %v", k.claude.calls)
	}
	if len(k.srv.reports) != 1 || k.srv.reports[0]["kit_commit"] != k.claude.commit || k.srv.reports[0]["repo"] != "acme/app" ||
		k.srv.reports[0]["path"] != k.dir || len(k.srv.reports[0]["plugins"].([]any)) != 1 {
		t.Fatalf("report: %+v", k.srv.reports)
	}
	settings, _ := os.ReadFile(filepath.Join(k.dir, ".claude", "settings.local.json"))
	if !strings.Contains(string(settings), "Bash(ls)") || !strings.Contains(string(settings), `"mine@agent-kit": true`) {
		t.Fatalf("settings lost keys: %s", settings)
	}

	// Again: one request, nothing else.
	calls, resolves := len(k.claude.calls), k.srv.resolves
	res = k.sync(kitSyncOpts{})
	if !res.Skipped || len(k.claude.calls) != calls || len(k.srv.reports) != 1 || k.srv.resolves != resolves+1 {
		t.Fatalf("second sync wasn't a no-op: %+v %v", res, k.claude.calls[calls:])
	}

	// The person turns waypoint off here: drift, reported and left alone.
	_ = k.claude.Run(context.Background(), k.dir, "disable", "waypoint@agent-kit", "--scope", "local")
	calls = len(k.claude.calls)
	res = k.sync(kitSyncOpts{})
	if len(k.claude.calls) != calls || len(res.Plan.Drift) != 1 || !strings.Contains(k.out.String(), "after wp enabled it") {
		t.Fatalf("drift: %v %+v\n%s", k.claude.calls[calls:], res.Plan, k.out)
	}
	if d := k.srv.reports[len(k.srv.reports)-1]["drift"].([]any); len(d) != 1 {
		t.Fatalf("drift not reported: %v", d)
	}
	k.sync(kitSyncOpts{force: true})
	if got := k.claude.calls[calls:]; !slices.Equal(got, []string{"enable waypoint@agent-kit --scope local"}) {
		t.Fatalf("--force: %v", got)
	}

	// The kit sends indexed when the checkout has an index, and stops
	// sending waypoint: wp disables only what it enabled.
	_ = os.MkdirAll(filepath.Join(k.dir, ".indexed"), 0o755)
	k.srv.mu.Lock()
	k.srv.resolved["plugins"] = []map[string]any{{"name": "indexed", "when": []string{"exists:.indexed"}}}
	k.srv.mu.Unlock()
	calls = len(k.claude.calls)
	k.sync(kitSyncOpts{})
	if got := k.claude.calls[calls:]; !slices.Equal(got, []string{"install indexed@agent-kit --scope local", "disable waypoint@agent-kit --scope local"}) {
		t.Fatalf("retargeted: %v", got)
	}
	st := k.c.state.Kit[k.dir]
	if st == nil || !slices.Equal(st.Managed, []string{"indexed@agent-kit"}) || st.Commit != k.claude.commit {
		t.Fatalf("state: %+v", st)
	}

	// A failed install: not recorded as applied, so the next sync tries again.
	k.srv.mu.Lock()
	k.srv.resolved["plugins"] = []map[string]any{{"name": "indexed", "when": []string{"exists:.indexed"}}, {"name": "demo"}}
	k.srv.resolved["commit"] = "f00"
	k.srv.mu.Unlock()
	k.claude.fail = "install demo"
	res = k.sync(kitSyncOpts{})
	if !res.failed || k.c.state.Kit[k.dir].Commit == "f00" || k.srv.reports[len(k.srv.reports)-1]["error"] == "" {
		t.Fatalf("failure: %+v %+v", res, k.c.state.Kit[k.dir])
	}
}

func TestKitSyncWithoutAKit(t *testing.T) {
	k := newKitTestEnv(t)
	k.srv.resolved = map[string]any{"repo": "acme/app", "commit": "", "plugins": []any{}, "notes": []any{}, "all": []any{}}
	if _, err := k.c.syncKitDir(context.Background(), k.dir, nil, kitSyncOpts{}); err != errKitNone || len(k.claude.calls) != 0 {
		t.Fatalf("no kit: %v %v", err, k.claude.calls)
	}
	k.srv.resolved = map[string]any{"repo": "acme/app", "commit": "abc", "attached": false, "plugins": []any{}, "notes": []any{}, "all": []any{"x"}}
	if _, err := k.c.syncKitDir(context.Background(), k.dir, nil, kitSyncOpts{}); err != errKitNone {
		t.Fatalf("unattached: %v", err)
	}
	if _, err := k.c.syncKitDir(context.Background(), t.TempDir(), nil, kitSyncOpts{}); err != errKitNone {
		t.Fatalf("not a checkout: %v", err)
	}
}

func TestKitSessionStart(t *testing.T) {
	k := newKitTestEnv(t)
	var started [][]string
	old := backgroundStart
	backgroundStart = func(exe string, args ...string) bool { started = append(started, args); return true }
	t.Cleanup(func() { backgroundStart = old })

	text := k.c.kitSessionStart(k.dir, "acme/app")
	if !strings.Contains(text, "Agent kit updated to") || !strings.Contains(text, "plugins: waypoint") || !strings.Contains(text, "- Wait on CI with wp pr wait.") ||
		strings.Contains(text, "gpu boxes") {
		t.Fatalf("context:\n%s", text)
	}
	if len(started) != 1 || !slices.Equal(started[0], []string{"kit", "sync", "--quiet", k.dir}) {
		t.Fatalf("background sync: %v", started)
	}
	// Within ten minutes: no request, and no second background sync.
	resolves := k.srv.resolves
	text = k.c.kitSessionStart(k.dir, "acme/app")
	if k.srv.resolves != resolves || len(started) != 1 || strings.Contains(text, "updated") || !strings.Contains(text, "Wait on CI") {
		t.Fatalf("cached: %d requests, %v\n%s", k.srv.resolves-resolves, started, text)
	}
	// Synced: just the notes.
	k.sync(kitSyncOpts{quiet: true})
	if text = k.c.kitSessionStart(k.dir, "acme/app"); strings.Contains(text, "updated") {
		t.Fatalf("after a sync:\n%s", text)
	}

	// The kit's part is cut to fit what Claude Code keeps.
	out := sessionStartOutput(strings.Repeat("x", 7500), "From the agent kit:\n- "+strings.Repeat("é", 2000))
	var v struct {
		H struct {
			Ctx string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &v); err != nil || len(v.H.Ctx) > maxSessionContext+3 || !strings.HasSuffix(v.H.Ctx, "…") || !strings.Contains(v.H.Ctx, sessionHint) {
		t.Fatalf("capped: %d %v", len(v.H.Ctx), err)
	}
}

func TestPlanCompact(t *testing.T) {
	file := func(name string, top int, models map[string]int) compactFile {
		return compactFile{Name: name, Top: top, Models: models}
	}
	settings := func(acts []kitAction) []string {
		var out []string
		for _, a := range acts {
			out = append(out, a.Setting)
		}
		return out
	}
	// devhost on 2026-10-26: the top-level window applied to every model but
	// Opus 5.5, whose "auto" in the same file replaced it.
	acts, drift := planCompact(200000, compactHave{Files: []compactFile{file("~/.claude/settings.json", 200000, map[string]int{"claude-opus-5-5": compactAuto, "claude-sonnet-5-5": 150000})}})
	if got := settings(acts); !slices.Equal(got, []string{"modelSettings.claude-opus-5-5.autoCompactWindow"}) || len(drift) != 0 ||
		!strings.Contains(acts[0].Why, `"auto" in ~/.claude/settings.json`) {
		t.Fatalf("per-model auto: %+v %+v", acts, drift)
	}
	// Nothing set anywhere is the model's own window; a project above the
	// ceiling beats a user file under it.
	if got, _ := planCompact(200000, compactHave{}); !slices.Equal(settings(got), []string{"autoCompactWindow"}) {
		t.Fatalf("unset: %+v", got)
	}
	acts, _ = planCompact(200000, compactHave{Files: []compactFile{file("~/.claude/settings.json", 150000, nil), file(".claude/settings.json", 600000, nil)}})
	if !slices.Equal(settings(acts), []string{"autoCompactWindow"}) || !strings.Contains(acts[0].Why, "600000 in .claude/settings.json") {
		t.Fatalf("project above: %+v", acts)
	}
	// Under the ceiling everywhere: nothing, including what local already caps.
	if got, _ := planCompact(200000, compactHave{Files: []compactFile{file("~/.claude/settings.json", 600000, map[string]int{"m": compactAuto}), file(".claude/settings.local.json", 200000, map[string]int{"m": 120000})}}); len(got) != 0 {
		t.Fatalf("capped already: %+v", got)
	}
	// No kit window: nothing. The environment beats the files: reported only.
	if got, _ := planCompact(0, compactHave{}); len(got) != 0 {
		t.Fatalf("no ceiling: %+v", got)
	}
	_, drift = planCompact(200000, compactHave{Env: "600000", Files: []compactFile{file("~/.claude/settings.json", 200000, nil)}})
	if len(drift) != 1 || !strings.Contains(drift[0].What, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=600000") {
		t.Fatalf("env: %+v", drift)
	}
	if _, drift = planCompact(200000, compactHave{Env: "150000"}); len(drift) != 0 {
		t.Fatalf("env under: %+v", drift)
	}

	got := parseCompactFile([]byte(`{"autoCompactWindow":"auto","modelSettings":{"claude-opus-5-5":{"autoCompactWindow":300000,"effortLevel":"high"},"x":{"effortLevel":"low"}}}`))
	if got.Top != compactAuto || len(got.Models) != 1 || got.Models["claude-opus-5-5"] != 300000 {
		t.Fatalf("parse: %+v", got)
	}
}

func TestKitSyncCapsCompactWindow(t *testing.T) {
	k := newKitTestEnv(t)
	_ = os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), []byte(`{"autoCompactWindow":200000,"modelSettings":{"claude-opus-5-5":{"autoCompactWindow":"auto"}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(k.dir, ".claude"), 0o755)
	local := filepath.Join(k.dir, ".claude", "settings.local.json")
	_ = os.WriteFile(local, []byte(`{"permissions":{"allow":["Bash(ls)"]},"modelSettings":{"claude-opus-5-5":{"effortLevel":"high"}}}`), 0o644)
	k.srv.mu.Lock()
	k.srv.resolved["compact_window"] = 200000
	k.srv.mu.Unlock()

	k.sync(kitSyncOpts{})
	if slices.ContainsFunc(k.claude.calls, func(c string) bool { return strings.Contains(c, "compact") }) {
		t.Fatalf("compact window went to claude: %v", k.claude.calls)
	}
	var s struct {
		Permissions json.RawMessage           `json:"permissions"`
		Window      json.RawMessage           `json:"autoCompactWindow"`
		Models      map[string]map[string]any `json:"modelSettings"`
	}
	raw, _ := os.ReadFile(local)
	if err := json.Unmarshal(raw, &s); err != nil || s.Permissions == nil || s.Window != nil ||
		s.Models["claude-opus-5-5"]["autoCompactWindow"] != float64(200000) || s.Models["claude-opus-5-5"]["effortLevel"] != "high" {
		t.Fatalf("settings.local.json: %s", raw)
	}
	if !strings.Contains(k.out.String(), "modelSettings.claude-opus-5-5.autoCompactWindow") {
		t.Fatalf("output:\n%s", k.out)
	}

	// Capped: the next sync is a no-op; raised again by hand, it caps again.
	if res := k.sync(kitSyncOpts{}); !res.Skipped {
		t.Fatalf("second sync: %+v", res.Plan)
	}
	raw, _ = os.ReadFile(local)
	_ = os.WriteFile(local, []byte(strings.Replace(string(raw), `"permissions"`, `"autoCompactWindow": 800000, "permissions"`, 1)), 0o644)
	if res := k.sync(kitSyncOpts{}); res.Skipped || len(res.Plan.Actions) != 1 || res.Plan.Actions[0].Setting != "autoCompactWindow" {
		t.Fatalf("raised by hand: %+v", res.Plan)
	}
	if raw, _ = os.ReadFile(local); !strings.Contains(string(raw), `"autoCompactWindow": 200000`) {
		t.Fatalf("not capped: %s", raw)
	}
}

func TestKitSyncForgetsGoneCheckouts(t *testing.T) {
	k := newKitTestEnv(t)
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	gone, alive := filepath.Join(t.TempDir(), "job1-gone"), t.TempDir()
	path := filepath.Join(cfg, "plugins", "installed_plugins.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(`{"version":2,"plugins":{
		"demo@agent-kit":[{"scope":"local","projectPath":"`+gone+`","gitCommitSha":"a"},{"scope":"local","projectPath":"`+alive+`","gitCommitSha":"b"},{"scope":"user","gitCommitSha":"c"}],
		"mine@agent-kit":[{"scope":"local","projectPath":"`+gone+`"}],
		"other@elsewhere":[{"scope":"local","projectPath":"`+gone+`"}]}}`), 0o600)

	k.sync(kitSyncOpts{})
	if !strings.Contains(k.out.String(), "Forgot 2 kit plugin installs") {
		t.Fatalf("output:\n%s", k.out)
	}
	var got struct {
		Version int                         `json:"version"`
		Plugins map[string][]map[string]any `json:"plugins"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || len(got.Plugins["demo@agent-kit"]) != 2 || got.Plugins["demo@agent-kit"][0]["projectPath"] != alive ||
		got.Plugins["mine@agent-kit"] != nil || len(got.Plugins["other@elsewhere"]) != 1 || len(got.Plugins["waypoint@agent-kit"]) != 1 {
		t.Fatalf("installed_plugins.json: %s", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}

	// Nothing gone: the file isn't rewritten.
	before, _ := os.Stat(path)
	k.sync(kitSyncOpts{force: true})
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) || strings.Contains(k.out.String(), "Forgot") {
		t.Fatalf("rewrote it with nothing gone:\n%s", k.out)
	}
}
