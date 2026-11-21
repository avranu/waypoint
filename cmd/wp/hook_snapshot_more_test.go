package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// invOf decodes a snapshot's inventory.
type snapInv struct {
	Env      map[string]string     `json:"env"`
	Settings []snapSettingsFile    `json:"settings_files"`
	Instr    []snapInstructionFile `json:"instruction_files"`
	MCP      map[string][]string   `json:"mcp_servers"`
	Skills   map[string][]string   `json:"skills"`
}

func invOf(t *testing.T, sn *hookSnapshot) snapInv {
	t.Helper()
	var inv snapInv
	if err := json.Unmarshal(sn.Inventory, &inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

func commitAll(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "base")
}

func TestMemorySlugIsPinned(t *testing.T) {
	for in, want := range map[string]string{
		"/projects/management/waypoint": "-projects-management-waypoint",
		"/home/a.b/c_d e":               "-home-a-b-c-d-e",
	} {
		if got := memorySlug(in); got != want {
			t.Errorf("memorySlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// WP-1468: Claude Code derives the memory directory from the git repository
// (https://code.claude.com/docs/en/memory.md), so a linked worktree and a
// sub-directory find the main checkout's directory.
func TestSnapshotMemoryIsTheRepositorysForWorktreesAndSubdirectories(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	snapshotHome(t, repo) // memory at the repo root's slug, written with literalSlug
	commitAll(t, repo)
	wt := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-q", "-b", "wt", wt)
	for name, cwd := range map[string]string{"main checkout": repo, "sub-directory": filepath.Join(repo, "web"), "linked worktree": wt, "directory of a linked worktree": wt} {
		g := gitFactsAt(cwd)
		if g.Root != repo {
			t.Fatalf("%s: root = %q, want %q", name, g.Root, repo)
		}
		sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: cwd}, g)
		if sn.MemoryFiles == nil || *sn.MemoryFiles != 2 || sn.MemoryIndexBytes == nil || *sn.MemoryIndexBytes != 5 {
			t.Errorf("%s: memory = %v files (%q)", name, sn.MemoryFiles, sn.Missing["memory_files"])
		}
	}
	if g := gitFactsAt(wt); !g.Worktree {
		t.Errorf("linked worktree not flagged: %+v", g)
	}
}

func TestSnapshotMemoryOutsideGitUsesTheCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cwd := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "projects", literalSlug(cwd), "memory", "MEMORY.md"), "abc")
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: cwd}, gitFacts{})
	if sn.MemoryFiles == nil || *sn.MemoryFiles != 1 {
		t.Errorf("memory = %v (%q)", sn.MemoryFiles, sn.Missing["memory_files"])
	}
}

// WP-1468: every user-scope path follows claudeConfigDir, and .claude.json
// lives inside CLAUDE_CONFIG_DIR when it is set.
func TestSnapshotReadsTheUserScopeFromClaudeConfigDir(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	home := t.TempDir() // holds a decoy ~/.claude that must not be read
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "decoy")
	writeFile(t, filepath.Join(home, ".claude", "skills", "decoy", "SKILL.md"), "x")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"decoy": {}}}`)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	writeFile(t, filepath.Join(cfg, "settings.json"), `{"model": "opus"}`)
	writeFile(t, filepath.Join(cfg, "CLAUDE.md"), "real")
	writeFile(t, filepath.Join(cfg, "skills", "real-skill", "SKILL.md"), "x")
	writeFile(t, filepath.Join(cfg, ".claude.json"), `{"mcpServers": {"real-mcp": {}}}`)
	writeFile(t, filepath.Join(cfg, "projects", literalSlug(repo), "memory", "MEMORY.md"), "12345")
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFactsAt(repo))
	inv := invOf(t, sn)
	if user := inv.Settings[0]; user.Scope != "user" || user.Path != filepath.Join(cfg, "settings.json") || user.Keys["model"] != "opus" {
		t.Errorf("user settings = %+v", user)
	}
	var user bool
	for _, f := range inv.Instr {
		if f.Scope == "user" {
			user = f.Path == filepath.Join(cfg, "CLAUDE.md")
		}
	}
	if !user {
		t.Errorf("user CLAUDE.md not from the config dir: %+v", inv.Instr)
	}
	if got := inv.Skills["user"]; len(got) != 1 || got[0] != "real-skill" {
		t.Errorf("skills = %v", got)
	}
	if got := inv.MCP["user"]; len(got) != 1 || got[0] != "real-mcp" {
		t.Errorf("mcp = %v (%q)", got, sn.Missing["inventory.mcp_servers.user"])
	}
	if sn.MemoryIndexBytes == nil || *sn.MemoryIndexBytes != 5 {
		t.Errorf("memory = %v (%q)", sn.MemoryIndexBytes, sn.Missing["memory_files"])
	}
}

// WP-1468: with no HOME and no CLAUDE_CONFIG_DIR nothing is read from a
// relative path, and each user-scope part says why it was not read.
func TestSnapshotReadsNothingRelativeWithoutAHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	work := t.TempDir()
	t.Chdir(work)
	// what a relative read would find
	writeFile(t, filepath.Join(work, "settings.json"), `{"model": "relative"}`)
	writeFile(t, filepath.Join(work, ".claude", "settings.json"), `{"model": "relative"}`)
	writeFile(t, filepath.Join(work, "CLAUDE.md"), "relative")
	writeFile(t, filepath.Join(work, "skills", "relative", "SKILL.md"), "x")
	writeFile(t, filepath.Join(work, ".claude.json"), `{"mcpServers": {"relative": {}}}`)
	writeFile(t, filepath.Join(work, "projects", "x", "memory", "MEMORY.md"), "x")
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	inv := invOf(t, sn)
	for _, f := range inv.Settings {
		if f.Scope == "user" {
			t.Errorf("a user settings file was read: %+v", f)
		}
	}
	if len(inv.Skills["user"]) != 0 || len(inv.MCP["user"]) != 0 {
		t.Errorf("user skills %v / mcp %v read", inv.Skills, inv.MCP)
	}
	for _, f := range inv.Instr {
		if f.Scope == "user" {
			t.Errorf("a user CLAUDE.md was read: %+v", f)
		}
	}
	if sn.MemoryFiles != nil {
		t.Error("memory read")
	}
	for _, k := range []string{"inventory.settings_files.user", "inventory.instruction_files.user", "inventory.skills.user", "inventory.mcp_servers.user", "memory_files"} {
		if sn.Missing[k] == "" {
			t.Errorf("no reason for %s", k)
		}
	}
}

func TestSnapshotSettingsKeysAndPlugins(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	snapshotHome(t, repo)
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFacts{})
	if !strings.Contains(sn.Missing["inventory.plugins"], "plugins supply are not listed") {
		t.Errorf("no plugin note: %v", sn.Missing)
	}
	// mcpServers is not a settings.json key: not read from a settings file
	if strings.Contains(string(sn.Inventory), "inline") || strings.Contains(string(sn.Inventory), "mcp_servers\":[") {
		t.Errorf("settings mcpServers were read: %s", sn.Inventory)
	}
	// without plugins, no note
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFacts{}); sn.Missing["inventory.plugins"] != "" {
		t.Error("a plugin note with no plugins enabled")
	}
}

func TestManagedSettingsPathPerOS(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "/etc/claude-code/managed-settings.json",
		"darwin":  "/Library/Application Support/ClaudeCode/managed-settings.json",
		"windows": `C:\Program Files\ClaudeCode\managed-settings.json`,
		"plan9":   "",
	} {
		if got := managedSettingsPath(goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestSnapshotBranchSourceNamesTheCommandThatMadeIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: cwd}, gitFacts{Branch: "main"})
	if sn.Sources["git_branch"] != "git:symbolic-ref" {
		t.Errorf("branch source = %q", sn.Sources["git_branch"])
	}
	sn = buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: cwd}, gitFacts{Branch: "(detached abc1234)", Detached: true})
	if sn.Sources["git_branch"] != "git:rev-parse" || *sn.GitBranch != "(detached abc1234)" {
		t.Errorf("detached source = %q", sn.Sources["git_branch"])
	}
	repo := gitRepo(t, "git@example.com:a/b.git")
	writeFile(t, filepath.Join(repo, "f"), "x")
	commitAll(t, repo)
	git(t, repo, "checkout", "-q", "--detach")
	if g := gitFactsAt(repo); !g.Detached || !strings.HasPrefix(g.Branch, "(detached ") {
		t.Errorf("gitFactsAt on a detached head = %+v", g)
	}
}

func TestSnapshotHarnessIsClaudeAndSaysItIsAConstant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if sn.Harness == nil || *sn.Harness != "claude" || !strings.HasPrefix(sn.Sources["harness"], "constant:") {
		t.Errorf("harness = %v from %q", sn.Harness, sn.Sources["harness"])
	}
}

// A sibling of the cwd is not loaded by Claude Code, so it is not listed
// (https://code.claude.com/docs/en/memory.md): only the cwd's subtree is.
func TestSnapshotInstructionFilesListTheSubtreeNotSiblings(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	t.Setenv("HOME", t.TempDir())
	writeFile(t, filepath.Join(repo, "api", "CLAUDE.md"), "sibling")
	writeFile(t, filepath.Join(repo, "web", "AGENTS.md"), "cwd")
	writeFile(t, filepath.Join(repo, "web", "deep", "CLAUDE.md"), "below")
	commitAll(t, repo)
	cwd := filepath.Join(repo, "web")
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: cwd}, gitFactsAt(cwd))
	got := map[string]string{}
	for _, f := range invOf(t, sn).Instr {
		got[f.Path] = f.Scope
	}
	if got[filepath.Join(repo, "api", "CLAUDE.md")] != "" {
		t.Errorf("a sibling's file is listed: %v", got)
	}
	if got[filepath.Join(cwd, "AGENTS.md")] != "ancestor" || got[filepath.Join(cwd, "deep", "CLAUDE.md")] != "subdir" {
		t.Errorf("instruction files = %v", got)
	}
}

func TestSnapshotTruncatesInstructionAndMemoryFiles(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for i := 0; i < snapshotMaxFiles+5; i++ {
		writeFile(t, filepath.Join(repo, fmt.Sprintf("d%03d", i), "AGENTS.md"), "x")
	}
	commitAll(t, repo)
	mem := filepath.Join(home, ".claude", "projects", literalSlug(repo), "memory")
	for i := 0; i < snapshotMaxMemoryFiles+5; i++ {
		writeFile(t, filepath.Join(mem, "n"+strconv.Itoa(i)+".md"), "x")
	}
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFactsAt(repo))
	var subdirs int
	for _, f := range invOf(t, sn).Instr {
		if f.Scope == "subdir" {
			subdirs++
		}
	}
	if subdirs != snapshotMaxFiles || sn.Missing["inventory.instruction_files.truncated"] == "" {
		t.Errorf("listed %d subdir files (%q)", subdirs, sn.Missing["inventory.instruction_files.truncated"])
	}
	if sn.MemoryFiles == nil || *sn.MemoryFiles != snapshotMaxMemoryFiles || sn.Missing["memory_files.truncated"] == "" {
		t.Errorf("memory files = %v (%q)", sn.MemoryFiles, sn.Missing["memory_files.truncated"])
	}
}

// WP-1468: with the budget already spent every git call and file read is
// skipped, and each says so.
func TestSnapshotSkipsEverythingOnceTheBudgetIsSpent(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	snapshotHome(t, repo)
	commitAll(t, repo)
	old := snapshotBudget
	snapshotBudget = -1
	t.Cleanup(func() { snapshotBudget = old })
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFactsAt(repo))
	if sn.Missing["budget"] == "" {
		t.Error("the spent budget was not said")
	}
	if sn.GitHead != nil || sn.Missing["git_head"] != budgetSkipped {
		t.Errorf("git head read past the budget: %v (%q)", sn.GitHead, sn.Missing["git_head"])
	}
	inv := invOf(t, sn)
	for _, f := range inv.Settings {
		if f.Exists || f.Error != budgetSkipped {
			t.Errorf("settings file read past the budget: %+v", f)
		}
	}
	if len(inv.Skills) != 0 || len(inv.MCP["user"]) != 0 || len(inv.Instr) > 0 && func() bool {
		for _, f := range inv.Instr {
			if f.Scope == "subdir" {
				return true
			}
		}
		return false
	}() {
		t.Errorf("inventory read past the budget: %s", sn.Inventory)
	}
	for _, k := range []string{"inventory.skills.user", "inventory.mcp_servers.user", "inventory.instruction_files.subdirs", "memory_files"} {
		if sn.Missing[k] != budgetSkipped {
			t.Errorf("missing[%s] = %q", k, sn.Missing[k])
		}
	}
	if sn.MemoryFiles != nil {
		t.Error("memory counted past the budget")
	}
}

func TestSnapshotSkipsAFileOverTheSizeLimit(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"model":"`+strings.Repeat("x", snapshotMaxFileBytes)+`"}`)
	writeFile(t, filepath.Join(home, ".claude.json"), strings.Repeat(" ", snapshotMaxFileBytes+1))
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFacts{})
	inv := invOf(t, sn)
	if u := inv.Settings[0]; u.Exists || u.Error != "too large" {
		t.Errorf("big settings file: %+v", u)
	}
	if !strings.Contains(sn.Missing["inventory.mcp_servers.user"], "too large") {
		t.Errorf("big .claude.json: %q", sn.Missing["inventory.mcp_servers.user"])
	}
}

func TestSnapshotUnreadableSkillsDirectoryIsSaid(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dir := filepath.Join(home, ".claude", "skills")
	writeFile(t, filepath.Join(dir, "a", "SKILL.md"), "x")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if !strings.HasPrefix(sn.Missing["inventory.skills.user"], "unreadable:") {
		t.Errorf("missing = %q", sn.Missing["inventory.skills.user"])
	}
}

// WP-1468: one long list cannot push the inventory past the server's limit
// and lose everything.
func TestSnapshotCapsLongListsSoTheRestSurvives(t *testing.T) {
	repo := gitRepo(t, "git@example.com:a/b.git")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"model": "opus"}`)
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"fs": {}}}`)
	for i := 0; i < 3000; i++ {
		if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", fmt.Sprintf("a-rather-long-skill-name-%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: repo}, gitFacts{})
	if len(sn.Inventory) > snapshotMaxInventory {
		t.Fatalf("inventory is %d bytes", len(sn.Inventory))
	}
	inv := invOf(t, sn)
	if len(inv.Skills["user"]) != snapshotMaxNames || inv.Skills["user"][0] != "a-rather-long-skill-name-0000" || sn.Missing["inventory.skills.user.truncated"] == "" ||
		!strings.Contains(sn.Missing["inventory.skills.user.truncated"], "2800") {
		t.Errorf("skills: %d listed, %q", len(inv.Skills["user"]), sn.Missing["inventory.skills.user.truncated"])
	}
	if inv.Env["CLAUDE_CODE_ENTRYPOINT"] != "cli" || inv.Settings[0].Keys["model"] != "opus" || len(inv.MCP["user"]) != 1 {
		t.Errorf("the rest of the inventory was lost: %s", sn.Inventory)
	}
}

func TestFitInventoryDropsTheLargestListsAndSaysSo(t *testing.T) {
	inv := map[string]any{
		"env":         map[string]string{"A": "b"},
		"skills":      map[string][]string{"user": {strings.Repeat("u", 600)}, "project": {strings.Repeat("p", 200)}},
		"mcp_servers": map[string][]string{"user": {"fs"}},
	}
	miss := map[string]string{}
	fitInventory(inv, miss, 500)
	raw, _ := json.Marshal(inv)
	if len(raw) > 500 {
		t.Errorf("still %d bytes: %s", len(raw), raw)
	}
	if miss["inventory.skills.user"] == "" || !strings.Contains(string(raw), `"env"`) || !strings.Contains(string(raw), `"fs"`) {
		t.Errorf("dropped the wrong part: %s %v", raw, miss)
	}
	if miss["inventory.skills.project"] != "" || miss["inventory.env"] != "" {
		t.Errorf("dropped more than needed: %v", miss)
	}
	// already fits: untouched
	miss = map[string]string{}
	fitInventory(inv, miss, 1<<20)
	if len(miss) != 0 {
		t.Errorf("miss = %v", miss)
	}
}

// ---- the claude process, from a fake /proc ----

type fakeProc struct {
	pid, ppid int
	exe       string // symlink target; "" for none
	args      []string
}

// fakeProcTree writes a /proc, makes the snapshot read it starting at start,
// and returns nothing: a test never depends on the real process tree.
func fakeProcTree(t *testing.T, start int, procs ...fakeProc) {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		writeFile(t, filepath.Join(dir, "cmdline"), strings.Join(p.args, "\x00")+"\x00")
		writeFile(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0", p.pid, "co mm)", p.ppid))
		if p.exe != "" {
			if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := snapshotProc
	snapshotProc = func() (string, int) { return root, start }
	t.Cleanup(func() { snapshotProc = old })
}

func claudeTree(claudeArgs ...string) []fakeProc {
	return []fakeProc{
		{pid: 300, ppid: 200, exe: "/usr/bin/wp", args: []string{"wp", "hook", "claude-event"}},
		{pid: 200, ppid: 100, exe: "/usr/bin/dash", args: []string{"sh", "/home/claude/hook.sh"}}, // a wrapper whose path says claude
		{pid: 100, ppid: 1, exe: "/home/u/.local/share/claude/versions/2.1.330", args: append([]string{"claude"}, claudeArgs...)},
	}
}

func TestSnapshotLaunchFlagsFromTheClaudeAncestorBehindAWrapper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name         string
		args         []string
		model, eff   string
		perm, reason string
	}{
		{"equals form", []string{"--model=opus", "--effort=low", "--permission-mode=plan"}, "opus", "low", "plan", ""},
		{"separate form", []string{"--model", "sonnet", "--effort", "high", "--permission-mode", "acceptEdits"}, "sonnet", "high", "acceptEdits", ""},
		{"skip permissions", []string{"--dangerously-skip-permissions"}, "", "", "bypassPermissions", "the launch gave no --model"},
		{"nothing", nil, "", "", "", "the launch gave no --model"},
		{"value with odd characters", []string{"--model=opus[1m]", "--effort", "lo w"}, "", "", "", "with a value that is not recorded (unexpected characters)"},
		// A valued flag with no value: the next flag is not its value.
		{"next is a flag", []string{"--model", "--verbose", "--effort", "--permission-mode", "plan"}, "", "", "plan", "the launch gave --model with no value"},
		{"last is the flag", []string{"--model"}, "", "", "", "the launch gave --model with no value"},
		{"empty equals form", []string{"--model="}, "", "", "", "the launch gave --model with no value"},
		// After "--" are operands (the prompt), not flags.
		{"after the terminator", []string{"-p", "--", "--model", "opus", "--dangerously-skip-permissions"}, "", "", "", "the launch gave no --model"},
		{"given twice, the last wins", []string{"--model", "sonnet", "--model=opus"}, "opus", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeProcTree(t, 300, claudeTree(tc.args...)...)
			sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
			get := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if get(sn.RequestedModel) != tc.model || get(sn.RequestedEffort) != tc.eff || get(sn.RequestedPermissionMode) != tc.perm {
				t.Errorf("requested %q %q %q, want %q %q %q", get(sn.RequestedModel), get(sn.RequestedEffort), get(sn.RequestedPermissionMode), tc.model, tc.eff, tc.perm)
			}
			if tc.model == "" && !strings.Contains(sn.Missing["requested_model"], tc.reason) {
				t.Errorf("model reason = %q, want it to say %q", sn.Missing["requested_model"], tc.reason)
			}
			if tc.model != "" && sn.Sources["requested_model"] != "process:argv" {
				t.Errorf("source = %q", sn.Sources["requested_model"])
			}
		})
	}
}

func TestSnapshotFlagWithOddValueIsNotSaidToBeAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeProcTree(t, 300, claudeTree("--model=opus[1m]")...)
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if got := sn.Missing["requested_model"]; got != "the launch gave --model with a value that is not recorded (unexpected characters)" {
		t.Errorf("reason = %q", got)
	}
}

func TestSnapshotWrapperNamedClaudeIsNotTheClaudeProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// no real claude: only a shell script whose path says claude
	fakeProcTree(t, 300,
		fakeProc{pid: 300, ppid: 200, exe: "/usr/bin/wp", args: []string{"wp"}},
		fakeProc{pid: 200, ppid: 1, exe: "/usr/bin/dash", args: []string{"sh", "/home/claude/x.sh", "--model=wrong"}})
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if sn.RequestedModel != nil || sn.Missing["requested_model"] == "" {
		t.Errorf("requested model = %v", sn.RequestedModel)
	}
}

func TestSnapshotRecognisesClaudeByExeOrArgv0(t *testing.T) {
	for name, p := range map[string]fakeProc{
		"exe named claude":      {pid: 100, ppid: 1, exe: "/opt/bin/claude", args: []string{"node-ish"}},
		"versions layout":       {pid: 100, ppid: 1, exe: "/x/claude/versions/2.1.1", args: []string{"2.1.1"}},
		"argv0 exactly claude":  {pid: 100, ppid: 1, args: []string{"claude"}},
		"argv0 path to claude":  {pid: 100, ppid: 1, exe: "/usr/bin/other", args: []string{"/usr/local/bin/claude"}},
		"exe of a deleted file": {pid: 100, ppid: 1, exe: "/x/claude/versions/2.1.1 (deleted)", args: []string{"x"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			args := append([]string{}, p.args...)
			p.args = append(args, "--model=m")
			fakeProcTree(t, 100, p)
			sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
			if sn.RequestedModel == nil || *sn.RequestedModel != "m" {
				t.Errorf("not recognised: %q", sn.Missing["requested_model"])
			}
		})
	}
	for name, p := range map[string]fakeProc{
		"substring in argv0":    {pid: 100, ppid: 1, exe: "/usr/bin/tool", args: []string{"claude-helper", "--model=m"}},
		"claude in an argument": {pid: 100, ppid: 1, exe: "/usr/bin/node", args: []string{"node", "/home/claude/cli.js", "--model=m"}},
	} {
		t.Run("not "+name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fakeProcTree(t, 100, p)
			if sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{}); sn.RequestedModel != nil {
				t.Errorf("taken for claude: %q", *sn.RequestedModel)
			}
		})
	}
}

func TestSnapshotHarnessVersionFromTheProcessThenTheEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_EXECPATH", "/inherited/claude/versions/9.9.9") // from a parent session
	fakeProcTree(t, 300, claudeTree()...)
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if sn.HarnessVersion == nil || *sn.HarnessVersion != "2.1.330" || sn.Sources["harness_version"] != "process:exe" {
		t.Errorf("version = %v from %q", sn.HarnessVersion, sn.Sources["harness_version"])
	}
	// no claude process found: the environment's, labelled as such
	fakeProcTree(t, 300, fakeProc{pid: 300, ppid: 1, exe: "/usr/bin/wp", args: []string{"wp"}})
	sn = buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if sn.HarnessVersion == nil || *sn.HarnessVersion != "9.9.9" || sn.Sources["harness_version"] != "env:CLAUDE_CODE_EXECPATH" {
		t.Errorf("fallback version = %v from %q", sn.HarnessVersion, sn.Sources["harness_version"])
	}
}

func TestSnapshotNoProcRootIsMissingNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := snapshotProc
	snapshotProc = func() (string, int) { return "", 0 } // not Linux
	t.Cleanup(func() { snapshotProc = old })
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	if sn.RequestedModel != nil || sn.Missing["requested_model"] == "" || sn.Missing["requested_effort"] == "" {
		t.Errorf("missing = %v", sn.Missing)
	}
}
