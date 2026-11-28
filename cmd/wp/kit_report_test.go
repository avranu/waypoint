package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type machineServer struct {
	mu      sync.Mutex
	reports []machineReport
}

func (m *machineServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/kit":
		_, _ = w.Write([]byte(`{"configured":true,"kit":{"marketplace":"agent-kit","plugins":[]}}`))
	case "/api/v1/agent-setup/machines":
		var rep machineReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		m.reports = append(m.reports, rep)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(404)
	}
}

func TestKitReport(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("WP_CLAUDE_SETTINGS", filepath.Join(cfg, "settings.json"))
	oldInstalled, oldAgy := claudeInstalled, antigravityInstalled
	claudeInstalled = func() bool { return false }
	antigravityInstalled = func() bool { return false }
	t.Cleanup(func() { claudeInstalled, antigravityInstalled = oldInstalled, oldAgy })

	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/CLAUDE.md", "# Mine\n")
	write(".claude/settings.json", `{"env":{"OPENAI_API_KEY":"sk-live"},"apiKeyHelper":"pass show key","model":"opus",
		"hooks":{"Stop":[{"hooks":[{"type":"command","command":"wp hook claude"}]}]},"mcp":{"x":{"authToken":"abc","url":"https://m"}}}`)
	write(".claude/agents/reviewer.md", "---\nname: reviewer\n---\n")
	write(".claude/agents/team/helper.md", "helper")
	write(".claude/agents/notes.txt", "not an agent")
	write(".claude/commands/ship.md", "ship it")
	write(".claude/skills/tidy/SKILL.md", "tidy")
	write(".claude/skills/tidy/extra.md", "not the skill file")
	write(".claude/hooks/guard.sh", "#!/bin/sh\n")
	write(".claude.json", `{"mcpServers":{"github":{"env":{"GITHUB_TOKEN":"ghp_x"}}},"oauthAccount":{"email":"a@b"}}`)
	write(".codex/AGENTS.md", "codex")

	srv := &machineServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg = Config{Server: ts.URL, Token: "wpt_x"}

	r := c.machineReport()
	var paths []string
	byPath := map[string]string{}
	for _, f := range r.Files {
		paths = append(paths, f.Path)
		byPath[f.Path] = f.Content
	}
	want := []string{".claude/CLAUDE.md", ".claude/settings.json", ".claude/agents/reviewer.md", ".claude/agents/team/helper.md",
		".claude/commands/ship.md", ".claude/skills/tidy/SKILL.md", ".claude/hooks/guard.sh", ".claude.json", ".codex/AGENTS.md"}
	if !slices.Equal(paths, want) {
		t.Fatalf("files:\n%v\nwant\n%v", paths, want)
	}
	set := byPath[".claude/settings.json"]
	for _, secret := range []string{"sk-live", "pass show", "abc"} {
		if strings.Contains(set, secret) {
			t.Errorf("settings keep %q:\n%s", secret, set)
		}
	}
	if !strings.Contains(set, "OPENAI_API_KEY") || !strings.Contains(set, "wp hook claude") || !strings.Contains(set, "https://m") {
		t.Errorf("settings lost what they should keep:\n%s", set)
	}
	if cj := byPath[".claude.json"]; !strings.Contains(cj, "github") || strings.Contains(cj, "ghp_x") || strings.Contains(cj, "a@b") {
		t.Errorf(".claude.json:\n%s", cj)
	}
	states := func(r machineReport) string {
		var s []string
		for _, k := range r.Checks {
			s = append(s, k.Key+"="+k.State)
		}
		return strings.Join(s, " ")
	}
	if got := states(r); got != "hooks=warn kit=warn" {
		t.Fatalf("checks: %s %+v", got, r.Checks)
	}
	write(".claude/plugins/known_marketplaces.json", `{"agent-kit":{"source":{"source":"git","url":"u"}}}`)

	if err := kitReport(c, []string{"--quiet"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.reports) != 1 || states(srv.reports[0]) != "hooks=warn kit=ok" || srv.reports[0].WPVersion != version || srv.reports[0].Machine == "" {
		t.Fatalf("sent: %+v", srv.reports)
	}
	if out.Len() != 0 {
		t.Errorf("--quiet printed: %s", out.String())
	}
	if time.Since(c.state.MachineReport) > time.Minute {
		t.Fatalf("not noted: %v", c.state.MachineReport)
	}

	if err := kitReport(c, []string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.reports) != 1 || !strings.Contains(out.String(), "~/.claude/agents/team/helper.md") {
		t.Fatalf("check: %d sent\n%s", len(srv.reports), out.String())
	}

	var started [][]string
	oldStart, oldExe := backgroundStart, selfExe
	backgroundStart = func(exe string, args ...string) bool { started = append(started, args); return true }
	selfExe = func() (string, error) { return "/usr/local/bin/wp", nil }
	t.Cleanup(func() { backgroundStart, selfExe = oldStart, oldExe })
	c.maybeReportMachine()
	if len(started) != 0 {
		t.Fatalf("started within 6h: %v", started)
	}
	_ = c.updateState(func(s *State) { s.MachineReport = time.Now().Add(-7 * time.Hour) })
	c.maybeReportMachine()
	c.maybeReportMachine()
	if len(started) != 1 || !slices.Equal(started[0], []string{"kit", "report", "--quiet"}) {
		t.Fatalf("background: %v", started)
	}
	selfExe = func() (string, error) { return "/tmp/go-build123/b001/exe/wp", nil }
	_ = c.updateState(func(s *State) { s.MachineReport = time.Time{} })
	c.maybeReportMachine()
	if len(started) != 1 {
		t.Fatalf("a throwaway build reported: %v", started)
	}
}

func TestRedactJSON(t *testing.T) {
	if got := redactJSON("not json"); got != "" {
		t.Fatalf("not JSON: %q", got)
	}
	got := redactJSON(`{"permissions":{"allow":["Bash(git:*)"]},"statusLine":{"command":"x"},"servers":[{"headers":{"Authorization":"Bearer q"},"password":"p"}]}`)
	if strings.Contains(got, "Bearer q") || strings.Contains(got, `"p"`) || !strings.Contains(got, "Bash(git:*)") || !strings.Contains(got, `"x"`) {
		t.Fatalf("redacted:\n%s", got)
	}
}

func TestRedactJSONArgsAndURL(t *testing.T) {
	out := redactJSON(`{"mcpServers":{"a":{"url":"https://x.test/mcp?key=sk-1","args":["--api-key","sk-2","--token=sk-3","--port","80"]}}}`)
	for _, leak := range []string{"sk-1", "sk-2", "sk-3"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%s leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "https://x.test") || !strings.Contains(out, `"80"`) {
		t.Fatalf("too much removed: %s", out)
	}
}

func TestRedactJSONHooksAndAttached(t *testing.T) {
	out := redactJSON(`{"hooks":{"SessionStart":[{"hooks":[{"command":"wp hook"}]}]},"secrets":{"GH":"ghp_x"},"mcpServers":{"a":{"url":"https://hooks.test/T0/B1/xyz","args":["-HAuthorization: Bearer zz","u"]}}}`)
	for _, bad := range []string{"ghp_x", "zz", "T0/B1"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q leaked: %s", bad, out)
		}
	}
	if !strings.Contains(out, "wp hook") {
		t.Fatalf("hook lost: %s", out)
	}
}

func TestRedactJSONHookEntriesCommandsAndFragments(t *testing.T) {
	out := redactJSON(`{"hooks":{"SessionStart":[{"hooks":[{"command":"API_TOKEN='my secret' wp hook --token abc","apiKey":"k-1"}]}]},"mcpServers":{"a":{"url":"https://x.test/#access_token=frag9"}}}`)
	for _, bad := range []string{"my secret", "abc", "k-1", "frag9"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q leaked: %s", bad, out)
		}
	}
	if !strings.Contains(out, "SessionStart") || !strings.Contains(out, "wp hook") {
		t.Fatalf("hook lost: %s", out)
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
}
