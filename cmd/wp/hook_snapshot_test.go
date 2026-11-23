package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// literalSlug writes out what Claude Code does to a path to name its
// project directory (every non-alphanumeric byte becomes a dash), apart from
// the code under test, so a wrong projectDirName fails.
func literalSlug(p string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, p)
}

// A fake Claude Code home: a user settings file with secrets in the places
// Claude Code keeps them, a memory directory for the repo, and instruction
// files at the root, above it and in a sub-directory.
func snapshotHome(t *testing.T, repo string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "settings.json"), `{
	  "model": "opus", "effortLevel": "high", "apiKeyHelper": "echo sk-secret-helper",
	  "permissions": {"defaultMode": "acceptEdits", "allow": ["Bash(ls)"]},
	  "env": {"ANTHROPIC_API_KEY": "sk-secret-env", "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE": "1", "ANTHROPIC_MODEL": "bad value with spaces"},
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x --token sk-secret-hook"}]}]},
	  "mcpServers": {"inline": {"headers": {"Authorization": "Bearer sk-secret-header"}}},
	  "enabledPlugins": {"agent-kit@market": true, "off@market": false}}`)
	write(filepath.Join(home, ".claude.json"), `{"mcpServers": {"fs": {"env": {"TOKEN": "sk-secret-mcp"}}}, "projects": {"`+repo+`": {"mcpServers": {"proj-only": {}}}}}`)
	write(filepath.Join(home, ".claude", "CLAUDE.md"), "user rules")
	write(filepath.Join(home, ".claude", "skills", "my-skill", "SKILL.md"), "x")
	write(filepath.Join(repo, "AGENTS.md"), "root rules")
	write(filepath.Join(repo, "web", "AGENTS.md"), "web rules!")
	write(filepath.Join(repo, ".claude", "settings.local.json"), `{"model": "sonnet"}`)
	mem := filepath.Join(home, ".claude", "projects", literalSlug(repo), "memory")
	write(filepath.Join(mem, "MEMORY.md"), "12345")
	write(filepath.Join(mem, "note.md"), "1234567")
	return home
}

func TestSnapshotRecordsWhatTheMachineHasWithoutSecrets(t *testing.T) {
	s := newSessionEnv(t)
	home := snapshotHome(t, s.repo)
	git(t, s.repo, "add", "-A")
	git(t, s.repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "base")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_EXECPATH", filepath.Join(home, "share", "claude", "versions", "2.1.330"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret-process-env")
	t.Setenv("GITHUB_TOKEN", "sk-secret-github")

	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: s.repo, Model: "claude-opus-5"}, gitFacts{Branch: "main"})
	raw, err := json.Marshal(sn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(string(raw), "Bearer") {
		t.Fatalf("a secret reached the snapshot: %s", raw)
	}
	if sn.Model == nil || *sn.Model != "claude-opus-5" || sn.Sources["model"] != "hook:SessionStart" {
		t.Errorf("model = %v from %q", sn.Model, sn.Sources["model"])
	}
	if sn.Entrypoint == nil || *sn.Entrypoint != "cli" || sn.HarnessVersion == nil || *sn.HarnessVersion != "2.1.330" {
		t.Errorf("entrypoint/version = %v %v", sn.Entrypoint, sn.HarnessVersion)
	}
	if sn.MemoryFiles == nil || *sn.MemoryFiles != 2 || *sn.MemoryBytes != 12 || sn.MemoryIndexBytes == nil || *sn.MemoryIndexBytes != 5 {
		t.Errorf("memory = %v files, %v bytes, %v index", sn.MemoryFiles, sn.MemoryBytes, sn.MemoryIndexBytes)
	}
	var inv struct {
		Env       map[string]string     `json:"env"`
		Settings  []snapSettingsFile    `json:"settings_files"`
		Instr     []snapInstructionFile `json:"instruction_files"`
		MCP       map[string][]string   `json:"mcp_servers"`
		Skills    map[string][]string   `json:"skills"`
		Truncated string                `json:"truncated"`
	}
	if err := json.Unmarshal(sn.Inventory, &inv); err != nil {
		t.Fatal(err)
	}
	if _, leaked := inv.Env["ANTHROPIC_API_KEY"]; leaked || inv.Env["CLAUDE_CODE_ENTRYPOINT"] != "cli" {
		t.Errorf("env = %v", inv.Env)
	}
	user := inv.Settings[0]
	if !user.Exists || len(user.SHA256) != 64 || user.Keys["model"] != "opus" || user.Keys["permissions.defaultMode"] != "acceptEdits" ||
		user.Env["CLAUDE_CODE_EXPERIMENTAL_CODE_MODE"] != "1" || user.Env["ANTHROPIC_MODEL"] != "(set; not a plain value)" {
		t.Errorf("user settings = %+v", user)
	}
	if len(user.Plugins) != 1 || user.Plugins[0] != "agent-kit@market" || len(user.HookEvents) != 1 || user.HookEvents[0] != "Stop" {
		t.Errorf("user settings names = %+v", user)
	}
	if local := inv.Settings[2]; !local.Exists || local.Keys["model"] != "sonnet" {
		t.Errorf("local settings = %+v", local)
	}
	if p := inv.Settings[1]; p.Exists {
		t.Errorf("project settings reported present: %+v", p)
	}
	paths := map[string]string{}
	for _, f := range inv.Instr {
		paths[strings.TrimPrefix(f.Path, home)] = f.Scope
	}
	if paths["/.claude/CLAUDE.md"] != "user" || paths[filepath.Join(s.repo, "AGENTS.md")] != "ancestor" || paths[filepath.Join(s.repo, "web", "AGENTS.md")] != "subdir" {
		t.Errorf("instruction files = %v", paths)
	}
	if got := strings.Join(inv.MCP["user"], ","); got != "fs,proj-only" {
		t.Errorf("mcp names = %q", got)
	}
	if got := inv.Skills["user"]; len(got) != 1 || got[0] != "my-skill" {
		t.Errorf("skills = %v", got)
	}
	if sn.GitHead == nil || len(*sn.GitHead) != 40 {
		t.Errorf("git head = %v", sn.GitHead)
	}
}

func TestSnapshotLeavesWhatASourceDidNotCarryNullAndSaysWhy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	t.Setenv("CLAUDE_CODE_EXECPATH", "")
	t.Setenv("CLAUDE_EFFORT", "")
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir()}, gitFacts{})
	for field, null := range map[string]bool{"model": sn.Model == nil, "permission_mode": sn.PermissionMode == nil, "effort": sn.Effort == nil,
		"entrypoint": sn.Entrypoint == nil, "harness_version": sn.HarnessVersion == nil, "git_head": sn.GitHead == nil, "memory_files": sn.MemoryFiles == nil} {
		if !null {
			t.Errorf("%s was not NULL", field)
		}
		if sn.Missing[field] == "" {
			t.Errorf("%s is NULL with no reason", field)
		}
	}
	if sn.MemoryFiles != nil && *sn.MemoryFiles == 0 {
		t.Error("an unknown memory size was recorded as zero")
	}
}

func TestSnapshotEffortComesFromThePayloadOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// CLAUDE_EFFORT can be wrong (absent under --effort low, inherited from a
	// parent session): it is never used.
	t.Setenv("CLAUDE_EFFORT", "medium")
	in := hookEventInput{HookEventName: "Stop", Cwd: t.TempDir(), EffortRaw: json.RawMessage(`{"level":"low"}`)}
	sn := buildSnapshot(in, gitFacts{})
	if sn.Effort == nil || *sn.Effort != "low" || sn.Sources["effort"] != "hook:Stop" {
		t.Errorf("payload effort: %v from %q", sn.Effort, sn.Sources["effort"])
	}
	in.EffortRaw = nil
	sn = buildSnapshot(in, gitFacts{})
	if sn.Effort != nil || !strings.Contains(sn.Missing["effort"], "carries no effort") {
		t.Errorf("absent effort: %v, %q", sn.Effort, sn.Missing["effort"])
	}
	// present but not {"level": string}: said apart from absent, and the rest is still read
	in.EffortRaw = json.RawMessage(`"high"`)
	sn = buildSnapshot(in, gitFacts{})
	if sn.Effort != nil || !strings.Contains(sn.Missing["effort"], "unrecognised shape") {
		t.Errorf("odd effort: %v, %q", sn.Effort, sn.Missing["effort"])
	}
	var parsed hookEventInput
	if err := json.Unmarshal([]byte(`{"session_id":"s","hook_event_name":"Stop","effort":"high","model":"m"}`), &parsed); err != nil || parsed.Model != "m" || parsed.effortLevel() != "" || !parsed.effortPresent() {
		t.Errorf("odd effort payload: %v %+v", err, parsed)
	}
	if _, set := buildSnapshot(in, gitFacts{}).Sources["effort"]; set {
		t.Error("an unreadable effort got a source")
	}
}

func TestSnapshotRequestedValuesComeFromTheLaunchNotThePayload(t *testing.T) {
	// No claude process above the hook (the default in tests): nothing is
	// requested: NULL with a reason, never a copy of the observed value.
	t.Setenv("HOME", t.TempDir())
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: t.TempDir(), Model: "m", PermissionMode: "auto"}, gitFacts{})
	if sn.RequestedModel != nil || sn.Missing["requested_model"] == "" {
		t.Errorf("requested model = %v (%q)", sn.RequestedModel, sn.Missing["requested_model"])
	}
	if sn.Model == nil || sn.PermissionMode == nil {
		t.Error("observed values lost")
	}
}

func TestSnapshotStaysInItsTimeBudget(t *testing.T) {
	old := snapshotBudget
	snapshotBudget = snapshotBudgetReal // the tests run roomy (TestMain)
	t.Cleanup(func() { snapshotBudget = old })
	s := newSessionEnv(t)
	snapshotHome(t, s.repo)
	start := time.Now()
	sn := buildSnapshot(hookEventInput{HookEventName: "SessionStart", Cwd: s.repo}, gitFacts{Branch: "main"})
	if took := time.Since(start); took > snapshotBudget+time.Second {
		t.Errorf("snapshot took %v", took)
	}
	if sn.BuildMS == nil {
		t.Error("the build time was not recorded")
	}
}

func TestSessionStartStoresSnapshotAndChangesOverTime(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	snapshotHome(t, s.repo)
	sent := recordRequests(t)
	s.hook(in("snap-1", "SessionStart", s.repo, map[string]any{"source": "startup", "model": "claude-sonnet-5"}))
	s.hook(in("snap-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "hi", "permission_mode": "default"}))
	s.hook(in("snap-1", "Stop", s.repo, map[string]any{"permission_mode": "default", "effort": map[string]any{"level": "low"}}))
	s.hook(in("snap-1", "PostModelSwitch", s.repo, map[string]any{"from_model": "claude-sonnet-5", "to_model": "claude-opus-5", "requested_model": "opus", "source": "command"}))
	s.hook(in("snap-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "again", "permission_mode": "plan"}))
	s.hook(in("snap-1", "InstructionsLoaded", s.repo, map[string]any{"file_path": filepath.Join(s.repo, "web", "AGENTS.md"), "memory_type": "Project", "load_reason": "nested_traversal"}))
	s.hook(in("snap-1", "PostCompact", s.repo, map[string]any{"trigger": "auto", "compact_summary": "SECRET SUMMARY TEXT"}))
	s.hook(in("snap-1", "ConfigChange", s.repo, map[string]any{"source": "project_settings", "file_path": filepath.Join(s.repo, ".claude", "settings.json")}))

	list := s.sessions()
	if len(list) != 1 {
		t.Fatalf("sessions: %d", len(list))
	}
	if list[0].Model != "claude-opus-5" {
		t.Errorf("the session's model went stale after a switch: %q", list[0].Model)
	}
	ctx := context.Background()
	snaps, err := s.svc.SessionSnapshots(ctx, s.me, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("snapshots: %d, want SessionStart, PostCompact, ConfigChange", len(snaps))
	}
	if snaps[0].TriggerEvent != "SessionStart" || *snaps[0].TriggerSource != "startup" || *snaps[0].Model != "claude-sonnet-5" || snaps[0].PermissionMode != nil {
		t.Errorf("first snapshot: %+v", snaps[0])
	}
	if snaps[0].Missing["permission_mode"] == "" || snaps[0].Sources["model"] != "hook:SessionStart" {
		t.Errorf("provenance: %v / %v", snaps[0].Sources, snaps[0].Missing)
	}
	if snaps[1].TriggerEvent != "PostCompact" || *snaps[1].TriggerSource != "auto" || snaps[2].TriggerEvent != "ConfigChange" {
		t.Errorf("later snapshots: %+v %+v", snaps[1], snaps[2])
	}
	obs, err := s.svc.SessionObserved(ctx, s.me, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range obs {
		got = append(got, o.Kind+"="+filepath.Base(o.Value))
	}
	want := "model=claude-sonnet-5 permission_mode=default effort=low model=claude-opus-5 permission_mode=plan instruction_file=AGENTS.md config_change=settings.json"
	if strings.Join(got, " ") != want {
		t.Errorf("observed:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	for _, o := range obs {
		if strings.Contains(string(o.Detail)+o.Value, "SECRET SUMMARY") {
			t.Error("the compaction summary was stored")
		}
	}
	// the snapshots as built, and the request bodies as sent, never hold it either
	raw, _ := json.Marshal(snaps)
	if strings.Contains(string(raw), "SECRET SUMMARY") {
		t.Error("the compaction summary is in a snapshot")
	}
	bodies := sent()
	if !strings.Contains(bodies, "PostCompact") {
		t.Fatalf("the PostCompact event was not seen on the wire: %q", bodies)
	}
	if strings.Contains(bodies, "SECRET SUMMARY") {
		t.Error("the compaction summary was sent to the server")
	}
	// the model switch sends both model fields (finding 29, CLI half)
	if !strings.Contains(bodies, "from_model") || !strings.Contains(bodies, `"requested_model":"opus"`) {
		t.Errorf("model switch fields not sent: %s", bodies)
	}
}

// recordRequests puts a reverse proxy in front of WAYPOINT_SERVER and returns
// a function giving every request body it has forwarded.
func recordRequests(t *testing.T) func() string {
	t.Helper()
	target, err := url.Parse(os.Getenv("WAYPOINT_SERVER"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var all strings.Builder
	rp := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		all.Write(b)
		all.WriteByte('\n')
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	t.Setenv("WAYPOINT_SERVER", front.URL)
	return func() string { mu.Lock(); defer mu.Unlock(); return all.String() }
}

func TestOutboxDropsAreCountedAndReported(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	up := os.Getenv("WAYPOINT_SERVER")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	t.Setenv("WAYPOINT_SERVER", deadURL)

	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	for i := 0; i < outboxMax+5; i++ {
		c.queueEvent(sessionEvent{Event: "UserPromptSubmit", At: time.Now().UnixMilli(), SessionID: "drop-1", Prompt: "p"})
	}
	// the oldest is now past its day: forgotten unsent, so counted
	if err := c.updateState(func(st *State) { st.Outbox[0].Event.At = time.Now().Add(-48 * time.Hour).UnixMilli() }); err != nil {
		t.Fatal(err)
	}
	c.queueEvent(sessionEvent{Event: "PostToolUse", Tool: "Bash", SessionID: "drop-1"})
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if n := c.state.Drops["overflow/UserPromptSubmit"]; n != 5 {
		t.Errorf("overflow counted %d, want 5 (%v)", n, c.state.Drops)
	}
	if n := c.state.Drops["unqueued/PostToolUse"]; n != 1 {
		t.Errorf("unqueued counted %d (%v)", n, c.state.Drops)
	}

	// Back up: the next event reports the counts, the expired one is counted
	// as it is forgotten, and the counts leave the state once the server has them.
	t.Setenv("WAYPOINT_SERVER", up)
	s.hook(in("drop-1", "SessionStart", s.repo, nil))
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	// the expired one is counted before the counts are taken: it went out in this call
	if len(c.state.Drops) != 0 || c.state.DropsOut != nil {
		t.Errorf("after the first send the state holds %v / %+v, want nothing", c.state.Drops, c.state.DropsOut)
	}
	if first, _ := s.svc.HookDrops(context.Background(), core.SystemActor()); len(first) != 3 {
		t.Errorf("after the first send the server has %+v, want overflow, unqueued and expired", first)
	}
	s.hook(in("drop-1", "Stop", s.repo, nil))
	c, _ = newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if len(c.state.Drops) != 0 {
		t.Errorf("reported counts stayed in the state: %v", c.state.Drops)
	}
	drops, err := s.svc.HookDrops(context.Background(), core.SystemActor())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	for _, d := range drops {
		by[d.Reason+"/"+d.Event] += d.Dropped
		if d.Machine != "box" {
			t.Errorf("machine = %q", d.Machine)
		}
	}
	if by["overflow/UserPromptSubmit"] != 5 || by["unqueued/PostToolUse"] != 1 || by["expired/UserPromptSubmit"] != 1 {
		t.Errorf("server counts = %v", by)
	}
}
