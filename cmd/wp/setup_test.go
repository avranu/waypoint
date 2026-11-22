//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/api"
)

// The hook set has a version (hookSchema) that hooks compare to bring an
// older install up to date. Changing the hooks without bumping it would
// leave every machine on the old set: this pins the set to the version.
func TestHookSchemaFollowsTheHooks(t *testing.T) {
	var b strings.Builder
	for _, h := range claudeHooks {
		fmt.Fprintf(&b, "%s|%s|%+v|%v\n", h.event, h.command, h.opts, h.prompts)
	}
	fmt.Fprintf(&b, "%v|%s|statusline\n", usageHookEvents, hookUsageSync)
	if hookSchema >= 8 {
		b.WriteString(keepCommand)
	}
	if hookSchema >= 13 {
		b.WriteString(wpCommand)
	}
	sum := sha256.Sum256([]byte(b.String()))
	got := hex.EncodeToString(sum[:8])
	pinned := map[int]string{1: "a73d2ceaa79a8f01", 2: "038da532b966d4bd", 3: "3e9d0008ec58eba8", 4: "7b8454b5327011ba", 5: "ef507a9b170b7aa3", 6: "05bd711d08d0dd98", 7: "05a51fa134326b6b", 8: "674cd320a2292fb2", 9: "c498858a7d602aaa", 10: "32c58ea4d401f6d7", 11: "ce86eb2f2fda25d8", 12: "58e6c0e01b516fa2", 13: "9a2ec57a8529f0a5", 14: "a6079908bfc1108d", 15: "db80b000d9ce370b", 16: "d946321be14a4944", 17: "14c91a25271b28e1", 18: "ea018f273918a683", 19: "e55b34af7abc96aa"}
	if pinned[hookSchema] != got {
		t.Errorf("the hooks changed (fingerprint %s): bump hookSchema to %d and pin %q to it here", got, hookSchema+1, got)
	}
}

// writeSettings writes a Claude Code settings file and points wp at it.
func writeSettings(t *testing.T, v any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WP_CLAUDE_SETTINGS", path)
	return path
}

func readSettings(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func swapHookExe(t *testing.T, exe string) {
	t.Helper()
	old := hookExe
	hookExe = func() string { return exe }
	t.Cleanup(func() { hookExe = old })
}

// Hooks an older wp installed bring themselves up to date the next time
// one runs: every hook of the current set, the binary that runs now, and
// --no-prompts kept. Once a day at most, never in a file without our
// hooks, never back to an older set, never for a test binary.
func TestHooksRefreshThemselves(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_NO_HOOK_REFRESH", "")
	// An old install: two hooks, another wp, prompts off; and someone else's hook.
	old := map[string]any{"model": "opus", "hooks": map[string]any{
		"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/old/wp hook claude-report", "timeout": 30}}}},
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/old/wp hook claude-event --no-prompts", "timeout": 30}}}},
		"PreToolUse":       []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "guard"}}}},
	}}
	path := writeSettings(t, old)
	swapHookExe(t, "/opt/wp/bin/wp")
	newCLIq := func() *cli {
		c, err := newCLI(io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	newCLIq().refreshHooks()
	got := readSettings(t, path)
	for _, want := range []string{"/opt/wp/bin/wp hook claude-event --no-prompts", "SessionStart", "SubagentStop", "/opt/wp/bin/wp usage sync --hook", `"guard"`, `"opus"`} {
		if !strings.Contains(got, want) {
			t.Errorf("after the refresh the settings lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/old/wp") {
		t.Errorf("the old binary is still in the settings:\n%s", got)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 1 {
		t.Errorf("backups: %v", backups)
	}
	h := newCLIq().state.Hooks
	if h == nil || h.Schema != hookSchema || h.Exe != "/opt/wp/bin/wp" || !h.NoPrompts || h.Settings != path {
		t.Fatalf("recorded %+v", h)
	}

	// Current: nothing is read or written.
	_ = os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/wp hook claude-report"}]}]}}`), 0o600)
	newCLIq().refreshHooks()
	if got := readSettings(t, path); !strings.Contains(got, "/x/wp") {
		t.Errorf("current hooks were rewritten: %s", got)
	}

	// An older set, tried less than a day ago: left for tomorrow.
	c := newCLIq()
	_ = c.updateState(func(s *State) { s.Hooks.Schema, s.Hooks.Tried = hookSchema-1, time.Now() })
	newCLIq().refreshHooks()
	if got := readSettings(t, path); !strings.Contains(got, "/x/wp") {
		t.Errorf("refreshed twice in a day: %s", got)
	}
	// A day later it goes.
	_ = c.updateState(func(s *State) { s.Hooks.Tried = time.Now().Add(-25 * time.Hour) })
	newCLIq().refreshHooks()
	if got := readSettings(t, path); strings.Contains(got, "/x/wp") || !strings.Contains(got, "/opt/wp/bin/wp hook claude-report") {
		t.Errorf("not refreshed a day later: %s", got)
	}

	// A newer set (a newer wp installed them): left alone.
	_ = os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/wp hook claude-report"}]}]}}`), 0o600)
	_ = c.updateState(func(s *State) { s.Hooks.Schema, s.Hooks.Tried = hookSchema+1, time.Time{} })
	newCLIq().refreshHooks()
	if got := readSettings(t, path); !strings.Contains(got, "/x/wp") {
		t.Errorf("went back to an older set: %s", got)
	}

	// No state at all (hooks from before it was recorded), but no hooks of
	// ours in the file: nothing is added.
	_ = c.updateState(func(s *State) { s.Hooks = nil })
	_ = os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600)
	newCLIq().refreshHooks()
	if got := readSettings(t, path); got != `{"model":"opus"}` {
		t.Errorf("hooks were added to a file that had none: %s", got)
	}

	// A test binary never writes its own path into the settings.
	_ = c.updateState(func(s *State) { s.Hooks = nil })
	_ = os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/wp hook claude-report"}]}]}}`), 0o600)
	swapHookExe(t, "/tmp/go-build123/b001/wp.test")
	newCLIq().refreshHooks()
	if got := readSettings(t, path); !strings.Contains(got, "/x/wp") {
		t.Errorf("a test binary refreshed the hooks: %s", got)
	}
}

func TestWorkRoot(t *testing.T) {
	home := "/home/me"
	for cwd, want := range map[string]string{
		"/projects/management/waypoint":         "/projects",
		"/projects/x/.claude/worktrees/agent-1": "/projects",
		"/home/me/code/app":                     "/home/me/code",
		"/home/me/code":                         "/home/me/code",
		"/home/me":                              "",
		"/home/me/.config/waypoint":             "",
		"/home/me/Downloads/x":                  "",
		"/":                                     "",
		"/etc/nginx":                            "",
		"/usr/local/src/x":                      "",
		"/mnt/data/repos/x":                     "/mnt/data",
		"/mnt":                                  "",
		"/home/other/code":                      "",
		"relative/path":                         "",
		"/srv/ci/build":                         "/srv",
		filepath.Join(os.TempDir(), "claude-xyz", "a"): "",
	} {
		if got := workRoot(cwd, home); got != want {
			t.Errorf("workRoot(%s) = %q, want %q", cwd, got, want)
		}
	}
	// A root that holds the home directory is never one.
	if got := workRoot("/data/other", "/data/me"); got != "" {
		t.Errorf("a root above home: %q", got)
	}
}

func TestDecodeProjectDir(t *testing.T) {
	base := t.TempDir()
	want := filepath.Join(base, "my-app", ".hidden", "sub_dir")
	if err := os.MkdirAll(want, 0o700); err != nil {
		t.Fatal(err)
	}
	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(want)
	if got := decodeProjectDir(enc); got != want {
		t.Errorf("decodeProjectDir(%s) = %q, want %q", enc, got, want)
	}
	// Gone since: as far as it exists.
	gone := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(filepath.Join(base, "my-app", "deleted"))
	if got := decodeProjectDir(gone); got != filepath.Join(base, "my-app") {
		t.Errorf("decodeProjectDir(%s) = %q", gone, got)
	}
}

// Session folders under ~/.claude/projects give the runner's default
// folders: from a transcript's cwd, or else from the folder's name.
func TestClaudeWorkRoots(t *testing.T) {
	home := t.TempDir()
	code, work := filepath.Join(home, "code"), filepath.Join(home, "my-work")
	claude := filepath.Join(home, ".claude")
	cwds := map[string]bool{
		filepath.Join(code, "app"):          true,
		filepath.Join(code, "lib"):          true,
		filepath.Join(work, "x-y"):          false, // decoded from the folder name
		home:                                true,  // the home directory itself: never
		filepath.Join(home, ".config", "x"): true,  // hidden: never
	}
	for cwd, transcript := range cwds {
		_ = os.MkdirAll(cwd, 0o700)
		dir := filepath.Join(claude, "projects", strings.NewReplacer("/", "-", ".", "-").Replace(cwd))
		_ = os.MkdirAll(dir, 0o700)
		if transcript {
			_ = os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(`{"type":"summary"}`+"\n"+`{"cwd":"`+cwd+`"}`+"\n"), 0o600)
		}
	}
	got := claudeWorkRoots(claude, home)
	if strings.Join(got, ",") != code+","+work {
		t.Errorf("roots = %v, want %s (most used first) and %s", got, code, work)
	}
	if got := claudeWorkRoots(filepath.Join(home, "nothing"), home); len(got) != 0 {
		t.Errorf("no Claude Code: %v", got)
	}
}

// setupEnv points wp setup at temporary everything: settings, units,
// Claude Code's folder, the binaries it installs, and the answers.
type setupEnv struct {
	*env
	home, settings, units string
	calls                 []string
	active                bool
	answers               string // "" and noTTY: nobody to ask
	noTTY                 bool
}

func newSetupEnv(t *testing.T) *setupEnv {
	if runtime.GOOS != "linux" {
		t.Skip("the runner service here is systemd")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl")
	}
	e := newEnv(t)
	e.runnerUser("robin")
	s := &setupEnv{env: e}
	s.home = t.TempDir()
	t.Setenv("HOME", s.home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(s.home, ".claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(s.home, ".config"))
	s.units = filepath.Join(s.home, ".config", "systemd", "user")
	s.settings = filepath.Join(s.home, ".claude", "settings.json")
	t.Setenv("WP_CLAUDE_SETTINGS", s.settings)
	// Claude Code worked in ~/code/app.
	app := filepath.Join(s.home, "code", "app")
	_ = os.MkdirAll(app, 0o700)
	proj := filepath.Join(s.home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(app))
	_ = os.MkdirAll(proj, 0o700)
	_ = os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(`{"cwd":"`+app+`"}`+"\n"), 0o600)

	swapHookExe(t, "/usr/local/bin/wp")
	oldSvc, oldClaude, oldAnswers, oldRun, oldWait := serviceExe, claudeInstalled, openAnswers, sysRun, onlineWait
	serviceExe = func() (string, error) { return "/usr/local/bin/wp", nil }
	claudeInstalled = func() bool { return true }
	openAnswers = func() io.ReadCloser {
		if s.noTTY {
			return nil
		}
		return io.NopCloser(strings.NewReader(s.answers))
	}
	sysRun = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		s.calls = append(s.calls, call)
		if strings.HasSuffix(call, "is-active "+unitName) && s.active {
			return "active", nil
		}
		return "", nil
	}
	onlineWait = 0
	t.Cleanup(func() {
		serviceExe, claudeInstalled, openAnswers, sysRun, onlineWait = oldSvc, oldClaude, oldAnswers, oldRun, oldWait
	})
	return s
}

func TestSetupSetsTheMachineUpAndIsSafeToRunAgain(t *testing.T) {
	s := newSetupEnv(t)
	code := filepath.Join(s.home, "code")

	// Nobody to ask and no --yes: nothing happens.
	s.noTTY = true
	out, exit := s.run("setup")
	if exit != 2 || !strings.Contains(out, "--yes") {
		t.Errorf("no terminal: exit %d\n%s", exit, out)
	}
	if _, err := os.Stat(s.settings); err == nil {
		t.Error("settings written without consent")
	}
	// Answered no.
	s.noTTY, s.answers = false, "n\n"
	if out := s.wp("setup"); !strings.Contains(out, "Nothing changed") {
		t.Errorf("answered no:\n%s", out)
	}

	// Yes: the plan shows what it will do, then it does it.
	s.answers = "\n"
	out = s.wp("setup")
	t.Log(out)
	for _, want := range []string{
		"already signed in as @robin",
		"install the hooks in ~/.claude/settings.json",
		"where Claude Code has worked on this computer",
		"~/code\n",
		"Go ahead? [Y/n]",
		"Done.",
		"Runner: a systemd user service; allowed: ~/code.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if got := readSettings(t, s.settings); !strings.Contains(got, "/usr/local/bin/wp hook claude-event") || strings.Contains(got, "--no-prompts") {
		t.Errorf("hooks: %s", got)
	}
	unit, err := os.ReadFile(filepath.Join(s.units, unitName))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/usr/local/bin/wp runner --allow "+code+"\n") {
		t.Errorf("unit = %s (%v)", unit, err)
	}
	st := (&cli{dir: s.dir}).loadState()
	if st.Hooks == nil || st.Hooks.Schema != hookSchema || st.Runner == nil || strings.Join(st.Runner.Allow, ",") != code {
		t.Errorf("state: hooks %+v runner %+v", st.Hooks, st.Runner)
	}

	// Again, with --yes and the runner running: nothing changes or restarts.
	s.active, s.calls = true, nil
	out = s.wp("setup", "--yes")
	if !strings.Contains(out, "are up to date") || !strings.Contains(out, "installed and running already") || strings.Contains(out, "Go ahead") {
		t.Errorf("second run:\n%s", out)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "restart") || strings.Contains(c, "enable") {
			t.Errorf("the second run touched the service: %v", s.calls)
		}
	}

	// The runner's settings are kept; --no-prompts is kept once chosen;
	// --allow replaces the folders.
	other := filepath.Join(s.home, "work")
	_ = os.MkdirAll(other, 0o700)
	s.wp("setup", "--yes", "--no-prompts", "--allow", other, "--name", "box", "--no-hooks")
	unit, _ = os.ReadFile(filepath.Join(s.units, unitName))
	if !strings.Contains(string(unit), "runner --name box --allow "+other+"\n") {
		t.Errorf("unit after --allow = %s", unit)
	}
	if got := readSettings(t, s.settings); strings.Contains(got, "--no-prompts") {
		t.Errorf("--no-hooks changed the hooks: %s", got)
	}
	s.wp("setup", "--yes", "--no-prompts", "--no-runner")
	s.wp("setup", "--yes", "--no-runner")
	if got := readSettings(t, s.settings); !strings.Contains(got, "claude-event --no-prompts") {
		t.Errorf("--no-prompts wasn't kept: %s", got)
	}
	unit, _ = os.ReadFile(filepath.Join(s.units, unitName))
	if !strings.Contains(string(unit), "--name box") {
		t.Errorf("--no-runner changed the runner: %s", unit)
	}
}

// cliServer serves wp binaries (shell scripts that print a version) and
// says it runs version.
func cliServer(t *testing.T, version, binVersion string, badSum bool) *env {
	t.Helper()
	dir := t.TempDir()
	bin := "#!/bin/sh\nif [ \"$1\" = version ]; then echo \"wp " + binVersion + "\"; exit 0; fi\necho \"$@\" >> \"$WP_FAKE_LOG\"\n"
	if err := os.WriteFile(filepath.Join(dir, cliAsset()), []byte(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(bin))
	if badSum {
		sum[0] ^= 0xff
	}
	sums := hex.EncodeToString(sum[:]) + "  " + cliAsset() + "\n" + strings.Repeat("0", 64) + "  wp-other-os\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	return newEnvWith(t, func(cfg *api.Config, url string) {
		cfg.Version, cfg.CLIDir, cfg.BaseURL = version, dir, url
	})
}

// installedWP makes the "installed" wp an update replaces.
func installedWP(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "bin", "wp")
	_ = os.MkdirAll(filepath.Dir(exe), 0o755)
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := selfExe
	selfExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExe = old })
	return exe
}

func setVersion(t *testing.T, v string) {
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func TestUpdate(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	exe := installedWP(t)
	setVersion(t, "v1.1.0")

	if out := e.wp("upgrade", "--check"); !strings.Contains(out, "v1.1.0") || !strings.Contains(out, "wp upgrade installs v1.2.0") {
		t.Errorf("--check:\n%s", out)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("--check changed the binary")
	}
	out := e.wp("upgrade")
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "wp v1.2.0") || !strings.Contains(out, "from v1.1.0 to v1.2.0") {
		t.Errorf("update: %s\nbinary %s", out, b)
	}
	if fi, _ := os.Stat(exe); fi.Mode()&0o111 == 0 {
		t.Error("the new wp isn't executable")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".wp-update-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	setVersion(t, "v1.2.0")
	if out := e.wp("upgrade"); !strings.Contains(out, "serves already") {
		t.Errorf("same version: %s", out)
	}
}

func TestUpdateRefusesABadDownload(t *testing.T) {
	setVersion(t, "v1.1.0")
	for name, e := range map[string]*env{
		"checksum": cliServer(t, "v1.2.0", "v1.2.0", true),
		"version":  cliServer(t, "v1.2.0", "v1.1.9", false), // the binary isn't what the server says
	} {
		exe := installedWP(t)
		t.Setenv("WAYPOINT_SERVER", e.url)
		out, code := e.run("upgrade")
		if code == 0 || !strings.Contains(out, "not installing") {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Errorf("%s: the binary was replaced", name)
		}
	}
}

// The SessionStart hook starts `wp upgrade --auto` in the background once a
// day, for release builds only; --auto updates and records what happened.
func TestAutoUpdateFromTheHook(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	exe := installedWP(t)
	var mu sync.Mutex
	var started []string
	old := backgroundStart
	backgroundStart = func(bin string, args ...string) {
		mu.Lock()
		started = append(started, bin+" "+strings.Join(args, " "))
		mu.Unlock()
	}
	t.Cleanup(func() { backgroundStart = old })
	cl := func() *cli {
		c, err := newCLI(io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	setVersion(t, "dev")
	cl().autoUpdateInBackground()
	if len(started) != 0 {
		t.Fatalf("a dev build looked for updates: %v", started)
	}
	setVersion(t, "v1.1.0")
	t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "1")
	cl().autoUpdateInBackground()
	if len(started) != 0 {
		t.Fatalf("WAYPOINT_NO_AUTO_UPDATE was ignored: %v", started)
	}
	t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "")
	cl().autoUpdateInBackground()
	cl().autoUpdateInBackground() // the same day: no
	if len(started) != 1 || started[0] != exe+" upgrade --auto" {
		t.Fatalf("started %v", started)
	}

	e.wp("upgrade", "--auto")
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "wp v1.2.0") {
		t.Error("--auto didn't update")
	}
	if u := cl().state.Update; u == nil || u.Server != "v1.2.0" || !strings.Contains(u.Result, "updated from v1.1.0 to v1.2.0") {
		t.Errorf("recorded %+v", u)
	}
	// A dirty build stays as it is.
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	setVersion(t, "v1.1.0-dirty")
	e.wp("upgrade", "--auto")
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Error("--auto updated a -dirty build")
	}
}

// A runner on a release build updates itself to the server's version
// before it takes work, and asks to be restarted on the new wp.
func TestRunnerUpdatesItself(t *testing.T) {
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	e.runnerUser("robin")
	exe := installedWP(t)
	setVersion(t, "v1.1.0")
	var out syncBuffer
	c := &cli{dir: e.dir, out: &out, err: &out}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := runRunner(ctx, c, runnerOpts{Name: "box", MaxJobs: 1, PollWait: 0, NoUsageSync: true})
	var re *restartError
	if !errors.As(err, &re) || re.exe != exe {
		t.Fatalf("runner ended with %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "wp v1.2.0") {
		t.Errorf("binary not replaced:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Updated "+exe+" to wp v1.2.0; restarting") {
		t.Errorf("log:\n%s", out.String())
	}

	// --no-auto-update: it keeps serving with the wp it has.
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	out = syncBuffer{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if err := runRunner(ctx2, c, runnerOpts{Name: "box", MaxJobs: 1, PollWait: 0.2, NoUsageSync: true, NoAutoUpdate: true}); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Error("--no-auto-update updated")
	}
}

// The installer, piped to sh, installs wp and goes on to wp setup with the
// arguments given: /setup always, /cli after "setup".
func TestInstallScriptRunsSetup(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "uname", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	e := cliServer(t, "v1.2.0", "v1.2.0", false)
	for _, tc := range []struct{ path, args, want string }{
		{"/setup", "--yes --allow /x", "setup --yes --allow /x"},
		{"/setup", "", "setup"},
		{"/cli", "setup --no-runner", "setup --no-runner"},
		{"/cli", "", ""},
	} {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		script := filepath.Join(dir, "install.sh")
		out, err := exec.Command("curl", "-fsSL", "-o", script, e.url+tc.path).CombinedOutput() // #nosec G204 -- the test server
		if err != nil {
			t.Fatalf("curl: %v %s", err, out)
		}
		cmd := exec.Command("sh", append([]string{script}, strings.Fields(tc.args)...)...) // #nosec G204 -- the script just served
		cmd.Env = append(os.Environ(), "WP_INSTALL_DIR="+filepath.Join(dir, "bin"), "WP_FAKE_LOG="+log)
		cmd.Stdin = bytes.NewReader(nil)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", tc.path, tc.args, err, out)
		}
		b, _ := os.ReadFile(log)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if lines[0] != "config server "+e.url {
			t.Errorf("%s %s: first ran %q", tc.path, tc.args, lines[0])
		}
		got := ""
		if len(lines) > 1 {
			got = lines[1]
		}
		if got != tc.want {
			t.Errorf("%s %s: ran %q, want %q", tc.path, tc.args, got, tc.want)
		}
	}
}

func TestAgentCLISummary(t *testing.T) {
	got := strings.Join(agentCLISummary([]runnerCLI{
		{Name: "claude", SignedIn: boolp(true), Detail: "claude.ai"},
		{Name: "cursor", SignedIn: boolp(false), Detail: "Not logged in"},
	}), "\n")
	if !strings.Contains(got, "Coding agents here: Claude Code (claude.ai), Cursor (signed out).") ||
		!strings.Contains(got, "To do: sign Cursor in: cursor-agent login.") || strings.Contains(got, "cursor.com/install") {
		t.Errorf("summary =\n%s", got)
	}
	if got := strings.Join(agentCLISummary(nil), "\n"); !strings.Contains(got, "none found") || !strings.Contains(got, "cursor.com/install") {
		t.Errorf("nothing found =\n%s", got)
	}
}
