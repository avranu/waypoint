package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The live check with the real CLIs is in runner_confine_live_test.go; these pin the arguments.

func TestClaudeReadIsConfinedToTheCheckout(t *testing.T) {
	work := t.TempDir()
	real, _ := filepath.EvalSymlinks(work)
	d, err := claudeDriver(agentJob{CLI: "claude", ReadOnly: true, Permission: "read", Confine: true}, "p", work)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(d.args, " ")
	for _, want := range []string{"--tools Read,Grep,Glob", "--permission-mode dontAsk", "Read(/" + real + "/**)", "Grep(/" + real + "/**)", "Glob(/" + real + "/**)"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	for _, bad := range []string{"--allowedTools Read,Grep,Glob", "Bash", "Edit", "Write", "acceptEdits"} {
		if strings.Contains(args, bad) {
			t.Errorf("args contain %q: %s", bad, args)
		}
	}
	if i := slices.Index(d.args, "--setting-sources"); i < 0 || d.args[i+1] != "user" || !slices.Contains(d.args, "--strict-mcp-config") {
		t.Errorf("settings and MCP aren't limited: %v", d.args)
	}
	if _, err := claudeDriver(agentJob{CLI: "claude", ReadOnly: true, Permission: "read"}, "p", filepath.Join(work, "a(b)")); err == nil {
		t.Error("a path with parentheses was put in a permission rule")
	}
	e, _ := claudeDriver(agentJob{CLI: "claude", Permission: "edit"}, "p", work)
	if a := strings.Join(e.args, " "); !strings.Contains(a, "acceptEdits") || strings.Contains(a, "dontAsk") {
		t.Errorf("edit args = %s", a)
	}
}

func TestOpencodeReadDeniesEverythingButTheCheckout(t *testing.T) {
	p := &runnerProc{ocFormatKnown: true, ocFormat: true}
	d := p.opencodeDriver(context.Background(), agentJob{CLI: "opencode", ReadOnly: true, Permission: "read", Confine: true}, "p", "/bin/opencode", "/w/job1")
	if i := slices.Index(d.args, "--dir"); i < 0 || d.args[i+1] != "/w/job1" {
		t.Errorf("opencode isn't told where to work: %v", d.args)
	}
	if i := slices.Index(d.args, "--agent"); i < 0 || d.args[i+1] != "plan" {
		t.Errorf("not the plan agent: %v", d.args)
	}
	perm := d.env["OPENCODE_PERMISSION"]
	for _, want := range []string{`"external_directory":"deny"`, `"edit":"deny"`, `"bash":"deny"`, `"webfetch":"deny"`} {
		if !strings.Contains(perm, want) {
			t.Errorf("permission %s lacks %s", perm, want)
		}
	}
}

// cursorDenies is cursor-agent's check of a Read deny rule (2026.10.22 and
// 2026.11.05 alike): the pattern is literal but for *, which is any characters.
func cursorDenies(t *testing.T, rule, path string) bool {
	t.Helper()
	pat, ok := strings.CutPrefix(rule, "Read(")
	if !ok {
		return false
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSuffix(pat, ")")), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(path)
}

func TestCursorReadRulesCoverAllButTheCheckout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "home", "runner", "work", "job1-abc")
	for _, f := range []string{
		"home/runner/.ssh/id_ed25519",
		"home/runner/.env",
		"home/runner/work/job2-def/secret",
		"home/runner/work/job1-abc-older/secret",
		"home/runner-two/.ssh/id",
		"secret",
		"home/runner/work/job1-abc/src/deep/.hidden",
	} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Reached through a link, as a temporary directory can be.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(work, link); err != nil {
		t.Fatal(err)
	}
	b, err := cursorReadRules(link)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(cfg.Permissions.Allow) != 0 {
		t.Errorf("allow = %v", cfg.Permissions.Allow)
	}
	denied := func(p string) bool {
		return slices.ContainsFunc(cfg.Permissions.Deny, func(r string) bool { return cursorDenies(t, r, p) })
	}
	for _, f := range []string{"secret", "home/runner/.ssh/id_ed25519", "home/runner/.env", "home/runner/work/job2-def/secret",
		"home/runner/work/job1-abc-older/secret", "home/runner-two/.ssh/id"} {
		if !denied(filepath.Join(root, f)) {
			t.Errorf("%s can be read", f)
		}
	}
	for _, f := range []string{"", "a.go", "src/deep/.hidden", ".cursor/rules/a.md"} {
		if denied(filepath.Join(work, f)) {
			t.Errorf("the checkout's %q is refused", f)
		}
	}
	if !denied(filepath.Join(work, ".cursor", "cli.json")) {
		t.Error("the rules file can be read")
	}
	for _, name := range []string{"job0-x", "job2-x", "job9-x", "job10-x", "job19-x", "job123456-x", "job2-x.schema.json"} {
		if !denied(filepath.Join(root, "home/runner/work", name, "secret")) {
			t.Errorf("a later checkout %s can be read", name)
		}
		if !denied(filepath.Join(root, "home/runner/work", name)) {
			t.Errorf("%s can be read", name)
		}
	}
	for _, want := range []string{"Write(**)", "Write(/**)", "Shell(*)", "WebFetch(*)", "Mcp(*:*)"} {
		if !slices.Contains(cfg.Permissions.Deny, want) {
			t.Errorf("deny lacks %s", want)
		}
	}
	if _, err := cursorReadRules(filepath.Join(root, "gone")); err == nil {
		t.Error("a checkout that isn't there got rules")
	}
}

// A relative cache directory still gives absolute rules, the only kind that
// can match what Cursor compares.
func TestCursorReadRulesAreAbsolute(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cache", "job1-a"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	b, err := cursorReadRules(filepath.Join("cache", "job1-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Read(`+filepath.Join(root, "secret")+`)"`) {
		t.Errorf("no absolute rule for the file beside the cache: %s", b)
	}
}

func TestCursorReadRulesRefuseANameWithAStar(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work", "job1-a")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "*"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cursorReadRules(work); err == nil || !strings.Contains(err.Error(), "* in its name") {
		t.Errorf("err = %v", err)
	}
}

func TestConfinedJobsRefuseCLIsThatCannotBeConfined(t *testing.T) {
	p := &runnerProc{ocFormatKnown: true}
	for _, cli := range []string{"codex", "cursor"} {
		_, err := p.buildDriver(context.Background(), agentJob{CLI: cli, ReadOnly: true, Permission: "read", Confine: true}, "/bin/x", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "can't be confined") {
			t.Errorf("%s: %v", cli, err)
		}
	}
	if _, err := p.buildDriver(context.Background(), agentJob{CLI: "codex", ReadOnly: true, Permission: "read"}, "/bin/x", t.TempDir()); err != nil {
		t.Errorf("an unmarked read job was refused: %v", err)
	}
	for _, cli := range []string{"claude", "opencode"} {
		if _, err := p.buildDriver(context.Background(), agentJob{CLI: cli, ReadOnly: true, Permission: "read", Confine: true}, "/bin/x", t.TempDir()); err != nil {
			t.Errorf("%s was refused: %v", cli, err)
		}
	}
}

func TestRemoveEscapingLinks(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(work, "sub"), 0o750))
	must(os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o600))
	must(os.WriteFile(filepath.Join(root, "secret"), []byte("s"), 0o600))
	must(os.Symlink("../a.txt", filepath.Join(work, "sub", "ok.txt")))
	must(os.Symlink("../../secret", filepath.Join(work, "sub", "up.txt")))
	must(os.Symlink(root, filepath.Join(work, "dir")))
	must(os.Symlink("/nonexistent/x", filepath.Join(work, "dangling")))
	must(os.Symlink("/etc/passwd", filepath.Join(work, "abs")))
	gone, err := removeEscapingLinks(work)
	must(err)
	slices.Sort(gone)
	if want := []string{"abs", "dangling", "dir", filepath.Join("sub", "up.txt")}; !slices.Equal(gone, want) {
		t.Errorf("removed %v, want %v", gone, want)
	}
	if _, err := os.Stat(filepath.Join(work, "sub", "ok.txt")); err != nil {
		t.Errorf("a link inside the checkout was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secret")); err != nil {
		t.Error("the target was touched")
	}
}

// An agent job's effort reaches Claude Code (the babysit assessor's high
// effort, WP-439); without one the CLI keeps its own.
func TestClaudeEffort(t *testing.T) {
	d, err := claudeDriver(agentJob{CLI: "claude", Permission: "edit", Model: "opus", Effort: "high"}, "p", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(d.args, " "); !strings.Contains(a, "--model opus --effort high") {
		t.Errorf("args = %s", a)
	}
	d, _ = claudeDriver(agentJob{CLI: "claude", Permission: "edit"}, "p", t.TempDir())
	if slices.Contains(d.args, "--effort") {
		t.Errorf("args = %v", d.args)
	}
}
