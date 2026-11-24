package main

// The build's time budget bounds git calls and file reads that start in time, but a read that blocks (a hung network mount) cannot be interrupted, so the build can overrun it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotMaxFiles       = 200
	snapshotMaxMemoryFiles = 2000
	snapshotMaxFileBytes   = 4 << 20
	snapshotMaxNames       = 200
	snapshotMaxInventory   = 64 << 10 // the server's snapshotMaxBytes (internal/core): it discards a larger inventory whole
)

// snapshotBudget is the build's time budget, snapshotBudgetReal as a hook
// runs it. A var so a test can make it already spent, or roomy: the tests
// run it roomy (TestMain), since a loaded CI runner under -race can spend
// 300ms writing and reading one test's files.
const snapshotBudgetReal = 300 * time.Millisecond

var snapshotBudget = snapshotBudgetReal

// hookSnapshot is core.HookSnapshot as the CLI sends it.
type hookSnapshot struct {
	Harness                 *string           `json:"harness,omitempty"`
	Entrypoint              *string           `json:"entrypoint,omitempty"`
	HarnessVersion          *string           `json:"harness_version,omitempty"`
	WPVersion               *string           `json:"wp_version,omitempty"`
	Model                   *string           `json:"model,omitempty"`
	Effort                  *string           `json:"effort,omitempty"`
	PermissionMode          *string           `json:"permission_mode,omitempty"`
	RequestedModel          *string           `json:"requested_model,omitempty"`
	RequestedEffort         *string           `json:"requested_effort,omitempty"`
	RequestedPermissionMode *string           `json:"requested_permission_mode,omitempty"`
	GitHead                 *string           `json:"git_head,omitempty"`
	GitBranch               *string           `json:"git_branch,omitempty"`
	GitWorktree             *bool             `json:"git_worktree,omitempty"`
	MemoryFiles             *int64            `json:"memory_files,omitempty"`
	MemoryBytes             *int64            `json:"memory_bytes,omitempty"`
	MemoryIndexBytes        *int64            `json:"memory_index_bytes,omitempty"`
	Inventory               json.RawMessage   `json:"inventory,omitempty"`
	Sources                 map[string]string `json:"sources,omitempty"`
	Missing                 map[string]string `json:"missing,omitempty"`
	BuildMS                 *int64            `json:"build_ms,omitempty"`
}

// snapshotEnvAllow is the environment a snapshot may record by name and
// value: Claude Code switches that change behaviour, and Waypoint's own
// WP_TASK and WAYPOINT_FOLLOWUP (set by its launchers). Nothing else is read,
// and a value that doesn't look like a plain setting is recorded as set only.
// CLAUDE_EFFORT is not in it: see fromPayload.
var snapshotEnvAllow = []string{
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE", "CLAUDE_CODE_SUBAGENT_MODEL",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_1M_CONTEXT", "CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "MAX_THINKING_TOKENS", "DISABLE_AUTOUPDATER", "BASH_DEFAULT_TIMEOUT_MS",
	"WP_TASK", "WAYPOINT_FOLLOWUP",
}

var plainValueRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,80}$`)

// snapshotSettingsKeys are the top-level settings keys whose scalar values
// are recorded. Each is listed in Claude Code's settings reference,
// https://code.claude.com/docs/en/settings-reference.md (checked 2026-11-21).
// Not here: mcpServers, which is not a settings.json key (MCP servers live in
// .mcp.json and ~/.claude.json).
var snapshotSettingsKeys = []string{"model", "effortLevel", "outputStyle", "alwaysThinkingEnabled", "autoMemoryEnabled", "fastMode", "includeGitInstructions"}

type snapSettingsFile struct {
	Scope      string            `json:"scope"`
	Path       string            `json:"path"`
	Exists     bool              `json:"exists"`
	Bytes      int64             `json:"bytes,omitempty"`
	SHA256     string            `json:"sha256,omitempty"`
	Keys       map[string]any    `json:"keys,omitempty"`
	Env        map[string]string `json:"env,omitempty"` // allow-listed names only
	HookEvents []string          `json:"hook_events,omitempty"`
	Plugins    []string          `json:"plugins,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type snapInstructionFile struct {
	Path  string `json:"path"`
	Scope string `json:"scope"` // user, ancestor (this directory and above), subdir (below it)
	Bytes int64  `json:"bytes"`
}

// snapshotBuilder reads the machine under a deadline.
type snapshotBuilder struct {
	in       hookEventInput
	git      gitFacts
	deadline time.Time
	cfgDir   string
	cfgWhy   string
	claude   *claudeProc
	sn       hookSnapshot
	src      map[string]string
	miss     map[string]string
	inv      map[string]any
}

func (b *snapshotBuilder) expired() bool { return time.Now().After(b.deadline) }

// gitContext is a context that ends with the budget; false when none remains.
func (b *snapshotBuilder) gitContext() (context.Context, context.CancelFunc, bool) {
	left := time.Until(b.deadline)
	if left <= 0 {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), left)
	return ctx, cancel, true
}

func (b *snapshotBuilder) missing(field, why string) { b.miss[field] = why }

func sp(s string) *string { return &s }

const budgetSkipped = "skipped: hook time budget"

var (
	errBudget   = errors.New(budgetSkipped)
	errTooLarge = errors.New("too large")
)

// readCapped reads a file the snapshot needs, only while budget remains and
// never more than snapshotMaxFileBytes.
func (b *snapshotBuilder) readCapped(path string) ([]byte, error) {
	if b.expired() {
		return nil, errBudget
	}
	f, err := os.Open(path) // #nosec G304 -- a path wp built
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, snapshotMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > snapshotMaxFileBytes {
		return nil, errTooLarge
	}
	return raw, nil
}

// userConfigDir is Claude Code's config directory for the user scope
// (claudeConfigDir), or "" and why when neither CLAUDE_CONFIG_DIR nor a home
// directory is set: nothing is then read from a relative path.
func userConfigDir() (string, string) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if !filepath.IsAbs(d) {
			return "", "CLAUDE_CONFIG_DIR is not an absolute path"
		}
		return claudeConfigDir(), ""
	}
	if home, err := os.UserHomeDir(); err != nil || !filepath.IsAbs(home) {
		return "", "neither CLAUDE_CONFIG_DIR nor a home directory is set"
	}
	return claudeConfigDir(), ""
}

// claudeJSONPath is the user's .claude.json: inside CLAUDE_CONFIG_DIR when
// that is set (as claudeTrusts reads it), else in the home directory.
func claudeJSONPath(cfgDir string) string {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return filepath.Join(cfgDir, ".claude.json")
	}
	return filepath.Join(filepath.Dir(cfgDir), ".claude.json")
}

// buildSnapshot reads what the session runs with. g is gitInfo's result
// (already read for the event).
func buildSnapshot(in hookEventInput, g gitFacts) *hookSnapshot {
	start := time.Now()
	b := &snapshotBuilder{in: in, git: g, deadline: start.Add(snapshotBudget), src: map[string]string{}, miss: map[string]string{}, inv: map[string]any{}}
	b.cfgDir, b.cfgWhy = userConfigDir()
	hookSrc := "hook:" + in.HookEventName
	b.sn.Harness, b.src["harness"] = sp("claude"), "constant: the hook is installed for Claude Code"
	b.findClaude()
	b.fromEnvironment()
	b.fromPayload(hookSrc)
	b.fromArgv()
	b.fromGit()
	b.settings()
	b.instructionFiles()
	b.memory()
	b.skills()
	if b.expired() {
		b.missing("budget", "the hook's "+snapshotBudget.String()+" budget ran out; later parts were skipped")
	}
	fitInventory(b.inv, b.miss, snapshotMaxInventory)
	inv, err := json.Marshal(b.inv)
	if err != nil {
		b.missing("inventory", "could not be encoded: "+err.Error())
	} else {
		b.sn.Inventory = inv
	}
	ms := time.Since(start).Milliseconds()
	b.sn.BuildMS = &ms
	b.sn.Sources, b.sn.Missing = b.src, b.miss
	return &b.sn
}

// capNames sorts names and keeps the first snapshotMaxNames; `missing` under
// key says how many were left out.
func (b *snapshotBuilder) capNames(key string, names []string) []string {
	sort.Strings(names)
	if len(names) > snapshotMaxNames {
		b.missing(key, strconv.Itoa(len(names)-snapshotMaxNames)+" more names not listed (the list stops at "+strconv.Itoa(snapshotMaxNames)+")")
		return names[:snapshotMaxNames]
	}
	return names
}

// fitInventory makes the encoded inventory fit limit (the server discards a
// larger one whole, losing every name): it drops the largest list until it
// fits and says in miss what it left out.
func fitInventory(inv map[string]any, miss map[string]string, limit int) {
	size := func() int { raw, _ := json.Marshal(inv); return len(raw) }
	for size() > limit {
		type unit struct {
			key, scope string
			n          int
		}
		var biggest unit
		measure := func(u unit, v any) {
			raw, _ := json.Marshal(v)
			if u.n = len(raw); u.n > biggest.n {
				biggest = u
			}
		}
		for k, v := range inv {
			if lists, ok := v.(map[string][]string); ok {
				for scope, l := range lists {
					measure(unit{key: k, scope: scope}, l)
				}
				continue
			}
			measure(unit{key: k}, v)
		}
		if biggest.n == 0 {
			return
		}
		why := "left out: the inventory was over " + strconv.Itoa(limit) + " bytes and this was its largest part"
		if biggest.scope == "" {
			delete(inv, biggest.key)
			miss["inventory."+biggest.key] = why
		} else {
			delete(inv[biggest.key].(map[string][]string), biggest.scope)
			miss["inventory."+biggest.key+"."+biggest.scope] = why
		}
	}
}

func (b *snapshotBuilder) fromEnvironment() {
	if v := os.Getenv("CLAUDE_CODE_ENTRYPOINT"); v != "" && plainValueRE.MatchString(v) {
		b.sn.Entrypoint, b.src["entrypoint"] = sp(v), "env:CLAUDE_CODE_ENTRYPOINT"
	} else {
		b.missing("entrypoint", "CLAUDE_CODE_ENTRYPOINT is not set in the hook's environment")
	}
	// CLAUDE_CODE_EXECPATH is only a fallback: a nested session inherits it from its parent and can name another version.
	if v, ok := versionFromPath(b.claudeExe()); ok {
		b.sn.HarnessVersion, b.src["harness_version"] = sp(v), "process:exe"
	} else if v, ok := versionFromPath(os.Getenv("CLAUDE_CODE_EXECPATH")); ok {
		b.sn.HarnessVersion, b.src["harness_version"] = sp(v), "env:CLAUDE_CODE_EXECPATH"
	} else {
		b.missing("harness_version", "no claude process was found with an install path under a versions directory, and CLAUDE_CODE_EXECPATH is not set or not under one")
	}
	b.sn.WPVersion, b.src["wp_version"] = sp(version), "wp:build"
	env := map[string]string{}
	for _, n := range snapshotEnvAllow {
		if v, ok := os.LookupEnv(n); ok {
			env[n] = envValue(v)
		}
	}
	b.inv["env"] = env
	b.src["inventory.env"] = "env"
}

// versionFromPath is <version> of an install path .../versions/<version>;
// any other layout says nothing.
func versionFromPath(p string) (string, bool) {
	if p == "" || filepath.Base(filepath.Dir(p)) != "versions" {
		return "", false
	}
	v := filepath.Base(p)
	return v, v != "" && plainValueRE.MatchString(v)
}

// envValue is a value as recorded: itself when it is a plain setting, else
// only the fact it was set.
func envValue(v string) string {
	if plainValueRE.MatchString(v) {
		return v
	}
	return "(set; not a plain value)"
}

func (b *snapshotBuilder) fromPayload(hookSrc string) {
	in := b.in
	if in.Model != "" {
		b.sn.Model, b.src["model"] = sp(in.Model), hookSrc
	} else {
		b.missing("model", hookSrc+" carried no model (Claude Code does not always include it)")
	}
	if in.PermissionMode != "" {
		b.sn.PermissionMode, b.src["permission_mode"] = sp(in.PermissionMode), hookSrc
	} else {
		b.missing("permission_mode", hookSrc+" carries no permission_mode")
	}
	// Effort comes from the payload only: CLAUDE_EFFORT is absent under --effort low and a nested session inherits its parent's level, so it can be wrong.
	switch {
	case in.effortLevel() != "":
		b.sn.Effort, b.src["effort"] = sp(in.effortLevel()), hookSrc
	case in.effortPresent():
		b.missing("effort", hookSrc+" had an effort field in an unrecognised shape (expected {\"level\": string})")
	default:
		b.missing("effort", hookSrc+" carries no effort")
	}
}

// claudeProc is the Claude Code process the hook runs under.
type claudeProc struct {
	Args []string
	Exe  string // its executable, from /proc/<pid>/exe; "" when unreadable
}

// snapshotProc says where to look for the claude process: the /proc root and
// the pid to start from (the hook's parent). A var so tests build a fake
// process tree. Only Linux has /proc; elsewhere the root is empty and the
// launch values are `missing` (WP-1486).
var snapshotProc = func() (string, int) {
	if runtime.GOOS != "linux" {
		return "", 0
	}
	return "/proc", os.Getppid()
}

func (b *snapshotBuilder) findClaude() {
	root, pid := snapshotProc()
	b.claude = findClaudeProc(root, pid)
}

func (b *snapshotBuilder) claudeExe() string {
	if b.claude == nil {
		return ""
	}
	return b.claude.Exe
}

// fromArgv reads what the launch asked for from the claude process's own
// arguments. It is the request, not the state.
func (b *snapshotBuilder) fromArgv() {
	if b.claude == nil {
		why := "no claude process was found above the hook (arguments are read from /proc, Linux only)"
		b.missing("requested_model", why)
		b.missing("requested_effort", why)
		b.missing("requested_permission_mode", why)
		return
	}
	args := launchFlags(b.claude.Args)
	for _, f := range []struct {
		field, flag string
		dst         **string
	}{{"requested_model", "--model", &b.sn.RequestedModel}, {"requested_effort", "--effort", &b.sn.RequestedEffort}, {"requested_permission_mode", "--permission-mode", &b.sn.RequestedPermissionMode}} {
		v, given, plain := argvFlag(args, f.flag)
		switch {
		case given && plain && v != "":
			*f.dst, b.src[f.field] = sp(v), "process:argv"
		case given && !plain:
			b.missing(f.field, "the launch gave "+f.flag+" with a value that is not recorded (unexpected characters)")
		case given:
			b.missing(f.field, "the launch gave "+f.flag+" with no value")
		case f.flag == "--permission-mode" && slices.Contains(args, "--dangerously-skip-permissions"):
			*f.dst, b.src[f.field] = sp("bypassPermissions"), "process:argv"
		default:
			b.missing(f.field, "the launch gave no "+f.flag)
		}
	}
}

// launchFlags is the part of a process's argv that can hold its options:
// without argv[0] (the executable) and without everything after "--" (the
// prompt and other operands, which may look like flags).
func launchFlags(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	args := argv[1:]
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	return args
}

// argvFlag is a valued flag's value in args, whether it was given, and
// whether the value is plain enough to record. "--name=value" and
// "--name value" are read; the next argument is the value only when it is
// not itself a flag, so "--effort --verbose" is --effort with no value. A
// flag given more than once has its last value, as the CLI parser keeps.
func argvFlag(args []string, name string) (v string, given, plain bool) {
	for i, a := range args {
		if val, ok := strings.CutPrefix(a, name+"="); ok {
			v, given = val, true
		} else if a == name {
			v, given = "", true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				v = args[i+1]
			}
		}
	}
	return v, given, given && plainValueRE.MatchString(v)
}

// findClaudeProc is the nearest ancestor of pid that is Claude Code, a few
// hops up (the hook runs under a shell or a wrapper), read under root (/proc).
func findClaudeProc(root string, pid int) *claudeProc {
	for hop := 0; hop < 8 && root != "" && pid > 1; hop++ {
		dir := filepath.Join(root, strconv.Itoa(pid))
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline")) // #nosec G304 -- a pid
		if err != nil {
			return nil
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		exe, _ := os.Readlink(filepath.Join(dir, "exe"))
		exe = strings.TrimSuffix(exe, " (deleted)")
		if isClaudeProc(exe, args) {
			return &claudeProc{Args: args, Exe: exe}
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat")) // #nosec G304 -- a pid
		if err != nil {
			return nil
		}
		// "pid (comm) S ppid ...": comm may hold spaces and parentheses.
		s := string(stat)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			return nil
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return nil
		}
		next, err := strconv.Atoi(f[1])
		if err != nil {
			return nil
		}
		pid = next
	}
	return nil
}

// isClaudeProc says whether a process is Claude Code by what it runs, never
// by a substring of an argument (`sh /home/claude/x.sh` is not it): its
// executable is named claude or sits at .../claude/versions/<version> (the
// native install), or argv[0] is exactly claude. A Node-launched install
// (node .../claude/cli.js) is not recognised.
func isClaudeProc(exe string, args []string) bool {
	if exe != "" {
		if filepath.Base(exe) == "claude" {
			return true
		}
		if d := filepath.Dir(exe); filepath.Base(d) == "versions" && filepath.Base(filepath.Dir(d)) == "claude" {
			return true
		}
	}
	return len(args) > 0 && filepath.Base(args[0]) == "claude"
}

func (b *snapshotBuilder) fromGit() {
	if b.in.Cwd == "" || !filepath.IsAbs(b.in.Cwd) {
		b.missing("git_head", "no absolute cwd in the payload")
		return
	}
	if b.git.Branch == "" {
		b.missing("git_head", "the cwd is not in a git work tree")
		b.missing("git_branch", "the cwd is not in a git work tree")
		return
	}
	branchSrc := "git:symbolic-ref"
	if b.git.Detached {
		branchSrc = "git:rev-parse"
	}
	b.sn.GitBranch, b.src["git_branch"] = sp(b.git.Branch), branchSrc
	wt := b.git.Worktree
	b.sn.GitWorktree, b.src["git_worktree"] = &wt, "git:rev-parse"
	ctx, cancel, ok := b.gitContext()
	if !ok {
		b.missing("git_head", budgetSkipped)
		return
	}
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", b.in.Cwd, "rev-parse", "HEAD").Output() // #nosec G204 -- fixed arguments; cwd is the hook's
	if h := strings.TrimSpace(string(out)); err == nil && h != "" {
		b.sn.GitHead, b.src["git_head"] = sp(h), "git:rev-parse HEAD"
	} else if ctx.Err() != nil {
		b.missing("git_head", "git did not answer within the hook's time budget")
	} else {
		b.missing("git_head", "the checkout has no commit yet")
	}
}

// managedSettingsPath is where Claude Code reads managed settings on goos
// (https://code.claude.com/docs/en/managed-settings.md); "" for any other.
func managedSettingsPath(goos string) string {
	switch goos {
	case "linux":
		return "/etc/claude-code/managed-settings.json"
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		return `C:\Program Files\ClaudeCode\managed-settings.json`
	}
	return ""
}

func (b *snapshotBuilder) settings() {
	var files []snapSettingsFile
	add := func(scope, path string) { files = append(files, b.readSettingsFile(scope, path)) }
	// Reads Claude Code's user settings (CLAUDE_CONFIG_DIR), not WP_CLAUDE_SETTINGS: the snapshot must record what Claude Code reads.
	if b.cfgDir != "" {
		add("user", filepath.Join(b.cfgDir, "settings.json"))
	} else {
		b.missing("inventory.settings_files.user", "not read: "+b.cfgWhy)
	}
	if filepath.IsAbs(b.in.Cwd) {
		add("project", filepath.Join(b.in.Cwd, ".claude", "settings.json"))
		add("local", filepath.Join(b.in.Cwd, ".claude", "settings.local.json"))
	}
	if p := managedSettingsPath(runtime.GOOS); p != "" {
		add("managed", p)
	} else {
		b.missing("inventory.settings_files.managed", "no managed-settings location is known for "+runtime.GOOS)
	}
	b.inv["settings_files"] = files
	b.src["inventory.settings_files"] = "disk"
	for _, f := range files {
		if len(f.Plugins) > 0 {
			b.missing("inventory.plugins", "skills, hooks and MCP servers that enabled plugins supply are not listed, only the plugins' names")
			break
		}
	}
	mcp := map[string][]string{}
	if b.cfgDir != "" {
		if names, err := b.claudeJSONMCPNames(claudeJSONPath(b.cfgDir), b.in.Cwd); err != nil {
			b.missing("inventory.mcp_servers.user", err.Error())
		} else {
			mcp["user"] = b.capNames("inventory.mcp_servers.user.truncated", names)
		}
	} else {
		b.missing("inventory.mcp_servers.user", "not read: "+b.cfgWhy)
	}
	if filepath.IsAbs(b.in.Cwd) {
		if names, err := b.mcpJSONNames(filepath.Join(b.in.Cwd, ".mcp.json")); err == nil {
			mcp["project"] = b.capNames("inventory.mcp_servers.project.truncated", names)
		} else if !os.IsNotExist(err) {
			b.missing("inventory.mcp_servers.project", err.Error())
		}
	}
	b.inv["mcp_servers"] = mcp
	b.src["inventory.mcp_servers"] = "disk"
}

// readSettingsFile reports whether a settings file exists, its size and
// hash, a few allow-listed scalar keys, the allow-listed env it sets, and
// the names of its hook events and enabled plugins.
func (b *snapshotBuilder) readSettingsFile(scope, path string) snapSettingsFile {
	f := snapSettingsFile{Scope: scope, Path: path}
	raw, err := b.readCapped(path)
	if err != nil {
		if !os.IsNotExist(err) {
			f.Error = "unreadable: " + err.Error()
			if errors.Is(err, errTooLarge) || errors.Is(err, errBudget) {
				f.Error = err.Error()
			}
		}
		return f
	}
	f.Exists, f.Bytes = true, int64(len(raw))
	sum := sha256.Sum256(raw)
	f.SHA256 = hex.EncodeToString(sum[:])
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		f.Error = "not JSON: " + err.Error()
		return f
	}
	f.Keys = map[string]any{}
	for _, k := range snapshotSettingsKeys {
		if v, ok := scalarSetting(m[k]); ok {
			f.Keys[k] = v
		}
	}
	var perms struct {
		DefaultMode string   `json:"defaultMode"`
		Allow       []string `json:"allow"`
		Deny        []string `json:"deny"`
		Ask         []string `json:"ask"`
	}
	if json.Unmarshal(m["permissions"], &perms) == nil {
		if plainValueRE.MatchString(perms.DefaultMode) && perms.DefaultMode != "" {
			f.Keys["permissions.defaultMode"] = perms.DefaultMode
		}
		f.Keys["permissions.allow_count"], f.Keys["permissions.deny_count"], f.Keys["permissions.ask_count"] = len(perms.Allow), len(perms.Deny), len(perms.Ask)
	}
	var env map[string]any
	if json.Unmarshal(m["env"], &env) == nil {
		f.Env = map[string]string{}
		for _, n := range snapshotEnvAllow {
			if v, ok := env[n]; ok {
				if s, isStr := v.(string); isStr {
					f.Env[n] = envValue(s)
				} else {
					f.Env[n] = "(set; not a string)"
				}
			}
		}
	}
	var hooks map[string]json.RawMessage
	if json.Unmarshal(m["hooks"], &hooks) == nil {
		f.HookEvents = b.capNames("inventory.settings_files."+scope+".hook_events.truncated", sortedNames(hooks))
	}
	var plugins map[string]bool
	if json.Unmarshal(m["enabledPlugins"], &plugins) == nil {
		var on []string
		for n, enabled := range plugins {
			if enabled {
				on = append(on, n)
			}
		}
		f.Plugins = b.capNames("inventory.settings_files."+scope+".plugins.truncated", on)
	}
	return f
}

// scalarSetting is a short string, bool or number; anything else is not
// recorded.
func scalarSetting(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	switch t := v.(type) {
	case bool, float64:
		return t, true
	case string:
		if plainValueRE.MatchString(t) {
			return t, true
		}
	}
	return nil, false
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// claudeJSONMCPNames is the MCP server names in the user's .claude.json: the
// user's and the ones set for this project.
func (b *snapshotBuilder) claudeJSONMCPNames(path, cwd string) ([]string, error) {
	raw, err := b.readCapped(path)
	if err != nil {
		return nil, err
	}
	var cj struct {
		Servers  map[string]json.RawMessage `json:"mcpServers"`
		Projects map[string]struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &cj); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for n := range cj.Servers {
		names[n] = true
	}
	for n := range cj.Projects[cwd].Servers {
		names[n] = true
	}
	return sortedNames(names), nil
}

func (b *snapshotBuilder) mcpJSONNames(path string) ([]string, error) {
	raw, err := b.readCapped(path)
	if err != nil {
		return nil, err
	}
	var m struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return sortedNames(m.Servers), nil
}

// instructionFiles lists every CLAUDE.md and AGENTS.md that can apply: the
// user's, and those in the cwd and every directory above it. Below the cwd,
// those git knows of in its subtree: Claude Code loads those lazily when a
// file there is touched. Siblings of the cwd are not loaded, so are not
// listed (https://code.claude.com/docs/en/memory.md). Outside git nothing
// below the cwd is searched.
func (b *snapshotBuilder) instructionFiles() {
	var out []snapInstructionFile
	seen := map[string]bool{}
	add := func(path, scope string) {
		if seen[path] {
			return
		}
		seen[path] = true
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			out = append(out, snapInstructionFile{Path: path, Scope: scope, Bytes: st.Size()})
		}
	}
	if b.cfgDir != "" {
		add(filepath.Join(b.cfgDir, "CLAUDE.md"), "user")
	} else {
		b.missing("inventory.instruction_files.user", "not read: "+b.cfgWhy)
	}
	if cwd := b.in.Cwd; filepath.IsAbs(cwd) {
		for d := cwd; ; d = filepath.Dir(d) {
			if b.expired() {
				b.missing("inventory.instruction_files.ancestors", budgetSkipped)
				break
			}
			for _, n := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".claude", "CLAUDE.md")} {
				add(filepath.Join(d, n), "ancestor")
			}
			if filepath.Dir(d) == d {
				break
			}
		}
		if ctx, cancel, ok := b.gitContext(); ok {
			cmd := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ":(glob)**/CLAUDE.md", ":(glob)**/AGENTS.md") // #nosec G204 -- fixed arguments
			listed, err := cmd.Output()
			cancel()
			if err != nil {
				b.missing("inventory.instruction_files.subdirs", "not listed: not a git checkout, or git failed or ran out of the time budget")
			}
			n := 0
			for _, rel := range strings.Split(string(listed), "\x00") {
				if rel == "" {
					continue
				}
				if n++; n > snapshotMaxFiles {
					b.missing("inventory.instruction_files.truncated", "more than "+strconv.Itoa(snapshotMaxFiles)+" instruction files; the rest are not listed")
					break
				}
				add(filepath.Join(cwd, rel), "subdir")
			}
		} else {
			b.missing("inventory.instruction_files.subdirs", budgetSkipped)
		}
	}
	b.inv["instruction_files"] = out
	b.src["inventory.instruction_files"] = "disk"
}

// memory counts the session's auto-memory directory:
// <config dir>/projects/<slug>/memory. Claude Code derives the slug from the
// git repository, so every worktree and sub-directory of a repo shares one
// directory; outside git it is the cwd
// (https://code.claude.com/docs/en/memory.md). The slug is the main
// checkout's path with non-alphanumerics as dashes.
func (b *snapshotBuilder) memory() {
	cwd := b.in.Cwd
	if b.cfgDir == "" || !filepath.IsAbs(cwd) {
		why := "no absolute cwd"
		if b.cfgDir == "" {
			why = b.cfgWhy
		}
		b.missing("memory_files", "not read: "+why)
		return
	}
	root := b.git.Root
	if root == "" {
		root = cwd
	}
	dir := filepath.Join(b.cfgDir, "projects", projectDirName(root), "memory")
	if b.expired() {
		b.missing("memory_files", budgetSkipped)
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		b.missing("memory_files", "no memory directory at the path for this repository")
		return
	}
	var files, bytes int64
	truncated := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if files >= snapshotMaxMemoryFiles || b.expired() {
			truncated = true
			return filepath.SkipAll
		}
		if info, err := d.Info(); err == nil {
			files++
			bytes += info.Size()
		}
		return nil
	})
	if truncated {
		b.missing("memory_files.truncated", "counting stopped at a limit or the time budget; the counts are a floor")
	}
	b.sn.MemoryFiles, b.sn.MemoryBytes = &files, &bytes
	b.src["memory_files"], b.src["memory_bytes"] = "disk:"+dir, "disk:"+dir
	if idx, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err == nil {
		n := idx.Size()
		b.sn.MemoryIndexBytes, b.src["memory_index_bytes"] = &n, "disk:"+filepath.Join(dir, "MEMORY.md")
	} else {
		b.missing("memory_index_bytes", "the memory directory has no MEMORY.md")
	}
}

// skills lists skill directory names: the user's and this project's.
func (b *snapshotBuilder) skills() {
	sk := map[string][]string{}
	list := func(scope, dir string) {
		key := "inventory.skills." + scope
		if b.expired() {
			b.missing(key, budgetSkipped)
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				b.missing(key, "unreadable: "+err.Error())
			}
			return
		}
		var names []string
		for _, e := range ents {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				names = append(names, e.Name())
			}
		}
		sk[scope] = b.capNames(key+".truncated", names)
	}
	if b.cfgDir != "" {
		list("user", filepath.Join(b.cfgDir, "skills"))
	} else {
		b.missing("inventory.skills.user", "not read: "+b.cfgWhy)
	}
	if filepath.IsAbs(b.in.Cwd) {
		list("project", filepath.Join(b.in.Cwd, ".claude", "skills"))
	}
	b.inv["skills"] = sk
	b.src["inventory.skills"] = "disk"
}
