package main

// The session snapshot (WP-1468, WP-ADR145 points 3 to 5): what a Claude Code
// session is actually running with, read by the hook at the moment it runs.
// Every value says where it was seen; one the source does not carry is left
// out and `missing` says why. Names and sizes only: environment values come
// from an allow-list, settings are read for a few non-secret keys, and
// nothing under an unlisted name is ever sent. The build is bounded by
// snapshotBudget (anything not reached is marked skipped) so a slow disk
// never holds the session up.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotBudget   = 300 * time.Millisecond
	snapshotMaxFiles = 200 // instruction files and memory files listed or counted
)

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
// value: Claude Code switches that change behaviour. Nothing else is read,
// and a value that doesn't look like a plain setting is recorded as set only.
var snapshotEnvAllow = []string{
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE", "CLAUDE_EFFORT", "CLAUDE_CODE_SUBAGENT_MODEL",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_1M_CONTEXT", "CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "MAX_THINKING_TOKENS", "DISABLE_AUTOUPDATER", "BASH_DEFAULT_TIMEOUT_MS",
	"WP_TASK", "WAYPOINT_FOLLOWUP",
}

var plainValueRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,80}$`)

// snapshotSettingsKeys are the top-level settings keys whose scalar values
// are recorded.
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
	MCP        []string          `json:"mcp_servers,omitempty"`
	Plugins    []string          `json:"plugins,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type snapInstructionFile struct {
	Path  string `json:"path"`
	Scope string `json:"scope"` // user, ancestor (this directory and above), subdir
	Bytes int64  `json:"bytes"`
}

// snapshotBuilder reads the machine under a deadline.
type snapshotBuilder struct {
	in       hookEventInput
	deadline time.Time
	sn       hookSnapshot
	src      map[string]string
	miss     map[string]string
	inv      map[string]any
}

func (b *snapshotBuilder) expired() bool { return time.Now().After(b.deadline) }

func (b *snapshotBuilder) set(field, source string) { b.src[field] = source }

func (b *snapshotBuilder) missing(field, why string) { b.miss[field] = why }

func sp(s string) *string { return &s }

// buildSnapshot reads what the session runs with. branch and worktree are
// gitInfo's (already read for the event).
func buildSnapshot(in hookEventInput, branch string, worktree bool) *hookSnapshot {
	start := time.Now()
	b := &snapshotBuilder{in: in, deadline: start.Add(snapshotBudget), src: map[string]string{}, miss: map[string]string{}, inv: map[string]any{}}
	ev := "hook:" + in.HookEventName
	b.sn.Harness, b.src["harness"] = sp("claude-code"), ev
	b.fromEnvironment()
	b.fromPayload(ev)
	b.fromArgv()
	b.fromGit(branch, worktree)
	home, _ := os.UserHomeDir()
	b.settings(home)
	b.instructionFiles(home)
	b.memory(home)
	b.skills(home)
	if b.expired() {
		b.missing("budget", "the hook's "+snapshotBudget.String()+" budget ran out; later parts were skipped")
	}
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

func (b *snapshotBuilder) fromEnvironment() {
	if v := os.Getenv("CLAUDE_CODE_ENTRYPOINT"); v != "" && plainValueRE.MatchString(v) {
		b.sn.Entrypoint, b.src["entrypoint"] = sp(v), "env:CLAUDE_CODE_ENTRYPOINT"
	} else {
		b.missing("entrypoint", "CLAUDE_CODE_ENTRYPOINT is not set in the hook's environment")
	}
	// The install path is .../versions/<version>; any other layout says nothing.
	if p := os.Getenv("CLAUDE_CODE_EXECPATH"); filepath.Base(filepath.Dir(p)) == "versions" && plainValueRE.MatchString(filepath.Base(p)) {
		b.sn.HarnessVersion, b.src["harness_version"] = sp(filepath.Base(p)), "env:CLAUDE_CODE_EXECPATH"
	} else {
		b.missing("harness_version", "CLAUDE_CODE_EXECPATH is not set or not under a versions directory")
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

// envValue is a value as recorded: itself when it is a plain setting, else
// only the fact it was set.
func envValue(v string) string {
	if plainValueRE.MatchString(v) {
		return v
	}
	return "(set; not a plain value)"
}

func (b *snapshotBuilder) fromPayload(ev string) {
	in := b.in
	if in.Model != "" {
		b.sn.Model, b.src["model"] = sp(in.Model), ev
	} else {
		b.missing("model", ev+" carried no model (Claude Code omits it, e.g. after /clear)")
	}
	if in.PermissionMode != "" {
		b.sn.PermissionMode, b.src["permission_mode"] = sp(in.PermissionMode), ev
	} else {
		b.missing("permission_mode", ev+" carries no permission_mode")
	}
	switch {
	case in.effortLevel() != "":
		b.sn.Effort, b.src["effort"] = sp(in.effortLevel()), ev
	case os.Getenv("CLAUDE_EFFORT") != "" && plainValueRE.MatchString(os.Getenv("CLAUDE_EFFORT")):
		// Claude Code documents $CLAUDE_EFFORT as the same level.
		b.sn.Effort, b.src["effort"] = sp(os.Getenv("CLAUDE_EFFORT")), "env:CLAUDE_EFFORT"
	default:
		b.missing("effort", ev+" carries no effort and CLAUDE_EFFORT is not set")
	}
}

// fromArgv reads what the launch asked for from the claude process's own
// arguments (Linux: /proc). It is the request, not the state.
func (b *snapshotBuilder) fromArgv() {
	args, ok := claudeArgv()
	if !ok {
		why := "the claude process's arguments could not be read on this machine"
		b.missing("requested_model", why)
		b.missing("requested_effort", why)
		b.missing("requested_permission_mode", why)
		return
	}
	flag := func(name string) (string, bool) {
		for i, a := range args {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v, plainValueRE.MatchString(v)
			}
			if a == name && i+1 < len(args) {
				return args[i+1], plainValueRE.MatchString(args[i+1])
			}
		}
		return "", false
	}
	for _, f := range []struct {
		field, flag string
		dst         **string
	}{{"requested_model", "--model", &b.sn.RequestedModel}, {"requested_effort", "--effort", &b.sn.RequestedEffort}, {"requested_permission_mode", "--permission-mode", &b.sn.RequestedPermissionMode}} {
		if v, ok := flag(f.flag); ok && v != "" {
			*f.dst, b.src[f.field] = sp(v), "process:argv"
		} else if f.flag == "--permission-mode" && slices.Contains(args, "--dangerously-skip-permissions") {
			*f.dst, b.src[f.field] = sp("bypassPermissions"), "process:argv"
		} else {
			b.missing(f.field, "the launch gave no "+f.flag)
		}
	}
}

// claudeArgv is the arguments of the nearest ancestor process that is
// Claude Code, read from /proc (a few hops: the hook runs under a shell).
func claudeArgv() ([]string, bool) {
	pid := os.Getppid()
	for hop := 0; hop < 6 && pid > 1; hop++ {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline") // #nosec G304 -- a pid
		if err != nil {
			return nil, false
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) > 0 && (strings.Contains(filepath.Base(args[0]), "claude") || len(args) > 1 && strings.Contains(args[1], "claude")) && !strings.HasPrefix(filepath.Base(args[0]), "wp") {
			return args, true
		}
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- a pid
		if err != nil {
			return nil, false
		}
		// "pid (comm) S ppid ...": comm may hold spaces and parentheses.
		s := string(stat)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			return nil, false
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return nil, false
		}
		next, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, false
		}
		pid = next
	}
	return nil, false
}

func (b *snapshotBuilder) fromGit(branch string, worktree bool) {
	if b.in.Cwd == "" || !filepath.IsAbs(b.in.Cwd) {
		b.missing("git_head", "no absolute cwd in the payload")
		return
	}
	if branch == "" {
		b.missing("git_head", "the cwd is not in a git work tree")
		b.missing("git_branch", "the cwd is not in a git work tree")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", b.in.Cwd, "rev-parse", "HEAD").Output() // #nosec G204 -- fixed arguments; cwd is the hook's
	if h := strings.TrimSpace(string(out)); err == nil && h != "" {
		b.sn.GitHead, b.src["git_head"] = sp(h), "git:rev-parse HEAD"
	} else {
		b.missing("git_head", "the checkout has no commit yet")
	}
	b.sn.GitBranch, b.src["git_branch"] = sp(branch), "git:symbolic-ref"
	b.sn.GitWorktree, b.src["git_worktree"] = &worktree, "git:rev-parse"
}

func (b *snapshotBuilder) settings(home string) {
	cfgDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfgDir == "" && home != "" {
		cfgDir = filepath.Join(home, ".claude")
	}
	var files []snapSettingsFile
	add := func(scope, path string) {
		if path == "" {
			return
		}
		if b.expired() {
			files = append(files, snapSettingsFile{Scope: scope, Path: path, Error: "skipped: hook time budget"})
			return
		}
		files = append(files, readSettingsFile(scope, path))
	}
	add("user", filepath.Join(cfgDir, "settings.json"))
	if b.in.Cwd != "" {
		add("project", filepath.Join(b.in.Cwd, ".claude", "settings.json"))
		add("local", filepath.Join(b.in.Cwd, ".claude", "settings.local.json"))
	}
	add("managed", "/etc/claude-code/managed-settings.json")
	b.inv["settings_files"] = files
	b.src["inventory.settings_files"] = "disk"
	// MCP servers by name: the user file's, this project's, and .mcp.json.
	mcp := map[string][]string{}
	if home != "" && !b.expired() {
		if names, err := claudeJSONMCPNames(filepath.Join(home, ".claude.json"), b.in.Cwd); err != nil {
			b.missing("inventory.mcp_servers.user", err.Error())
		} else {
			mcp["user"] = names
		}
	}
	if b.in.Cwd != "" && !b.expired() {
		if names, err := mcpJSONNames(filepath.Join(b.in.Cwd, ".mcp.json")); err == nil {
			mcp["project"] = names
		} else if !os.IsNotExist(err) {
			b.missing("inventory.mcp_servers.project", err.Error())
		}
	}
	b.inv["mcp_servers"] = mcp
	b.src["inventory.mcp_servers"] = "disk"
}

// readSettingsFile reports whether a settings file exists, its size and
// hash, a few allow-listed scalar keys, the allow-listed env it sets, and
// the names of its hook events, MCP servers and plugins.
func readSettingsFile(scope, path string) snapSettingsFile {
	f := snapSettingsFile{Scope: scope, Path: path}
	raw, err := os.ReadFile(path) // #nosec G304 -- a settings path wp built
	if err != nil {
		if !os.IsNotExist(err) {
			f.Error = "unreadable: " + err.Error()
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
		f.HookEvents = sortedNames(hooks)
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(m["mcpServers"], &servers) == nil {
		f.MCP = sortedNames(servers)
	}
	var plugins map[string]bool
	if json.Unmarshal(m["enabledPlugins"], &plugins) == nil {
		for n, on := range plugins {
			if on {
				f.Plugins = append(f.Plugins, n)
			}
		}
		sort.Strings(f.Plugins)
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

// claudeJSONMCPNames is the MCP server names in ~/.claude.json: the user's
// and the ones set for this project.
func claudeJSONMCPNames(path, cwd string) ([]string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the user's Claude Code file
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

func mcpJSONNames(path string) ([]string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- a path wp built
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
// user's, those in this directory and above, and those in sub-directories of
// the checkout (git knows them; outside git they are not searched).
func (b *snapshotBuilder) instructionFiles(home string) {
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
	if home != "" {
		add(filepath.Join(home, ".claude", "CLAUDE.md"), "user")
	}
	if cwd := b.in.Cwd; filepath.IsAbs(cwd) {
		for d := cwd; ; d = filepath.Dir(d) {
			for _, n := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".claude", "CLAUDE.md")} {
				add(filepath.Join(d, n), "ancestor")
			}
			if filepath.Dir(d) == d {
				break
			}
		}
		if !b.expired() {
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			cmd := exec.CommandContext(ctx, "git", "-C", cwd, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ":(glob)**/CLAUDE.md", ":(glob)**/AGENTS.md") // #nosec G204 -- fixed arguments
			listed, err := cmd.Output()
			cancel()
			if err != nil {
				b.missing("inventory.instruction_files.subdirs", "not listed: not a git checkout, or git failed or timed out")
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
			b.missing("inventory.instruction_files.subdirs", "skipped: hook time budget")
		}
	}
	b.inv["instruction_files"] = out
	b.src["inventory.instruction_files"] = "disk"
}

// memory counts the session's auto-memory directory:
// ~/.claude/projects/<cwd with non-alphanumerics as dashes>/memory.
func (b *snapshotBuilder) memory(home string) {
	cwd := b.in.Cwd
	if home == "" || !filepath.IsAbs(cwd) {
		b.missing("memory_files", "no home directory or absolute cwd")
		return
	}
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(home, ".claude")
	}
	dir := filepath.Join(cfg, "projects", memorySlug(cwd), "memory")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		b.missing("memory_files", "no memory directory at the path for this cwd")
		return
	}
	var files, bytes int64
	truncated := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // an unreadable entry is not counted; the directory still is
		}
		if files >= snapshotMaxFiles*10 || b.expired() {
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

var slugRE = regexp.MustCompile(`[^A-Za-z0-9]`)

func memorySlug(cwd string) string { return slugRE.ReplaceAllString(cwd, "-") }

// skills lists skill directory names: the user's and this project's.
func (b *snapshotBuilder) skills(home string) {
	sk := map[string][]string{}
	list := func(scope, dir string) {
		if b.expired() || dir == "" {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				sk[scope] = append(sk[scope], e.Name())
			}
		}
		sort.Strings(sk[scope])
	}
	if home != "" {
		list("user", filepath.Join(home, ".claude", "skills"))
	}
	if b.in.Cwd != "" {
		list("project", filepath.Join(b.in.Cwd, ".claude", "skills"))
	}
	b.inv["skills"] = sk
	b.src["inventory.skills"] = "disk"
}
