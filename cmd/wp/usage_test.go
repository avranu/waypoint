package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

// fixtureDir copies cmd/wp/testdata/usage into a temp Claude config dir. The
// main transcript ends in a half-written line, as a live one does.
func fixtureDir(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "usage")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if filepath.Base(p) == "sess1.jsonl" {
			b = bytes.TrimRight(b, "\n")
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func parseMain(t *testing.T, dir string, st usageFileState) *usageParsed {
	t.Helper()
	f := usageFile{Path: filepath.Join(dir, "projects", "-work-app", "sess1.jsonl"), Session: "sess1", Kind: "main", Main: true}
	p, err := readUsageFile(f, st, func(string) string { return "owner/app" }, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// toolRow sums the rows (one per hour) of a tool; nil when there are none.
func toolRow(p *usageParsed, kind, tool, detail string) *usageToolRow {
	var sum *usageToolRow
	for i := range p.Chunk.Tools {
		r := p.Chunk.Tools[i]
		if r.Kind == kind && r.Tool == tool && r.Detail == detail {
			if sum == nil {
				sum = &r
				continue
			}
			sum.Calls += r.Calls
			sum.Errors += r.Errors
			sum.Refusals += r.Refusals
			sum.Prompts += r.Prompts
			sum.Bytes += r.Bytes
		}
	}
	return sum
}

func TestUsageParseFixture(t *testing.T) {
	dir := fixtureDir(t)
	p := parseMain(t, dir, usageFileState{})
	hour10 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).UnixMilli()
	hour11 := hour10 + 3_600_000

	want := []usageMsgRow{
		{Hour: hour10, Session: "sess1", Repo: "owner/app", Model: "claude-opus-5-5", Kind: "main", N: 1, Input: 10, Output: 50, CW5: 60, CW1: 40, CR: 1000},
		{Hour: hour11, Session: "sess1", Repo: "owner/app", Model: "claude-opus-5-5", Kind: "main", N: 1, Input: 3, Output: 20, CR: 1200},
	}
	if len(p.Chunk.Messages) != 2 || p.Chunk.Messages[0] != want[0] || p.Chunk.Messages[1] != want[1] {
		t.Errorf("messages: %+v\nwant %+v", p.Chunk.Messages, want)
	}

	if r := toolRow(p, "builtin", "Bash", ""); r == nil || r.Calls != 2 || r.Refusals != 1 || r.Errors != 0 || r.Bytes == 0 {
		t.Errorf("Bash: %+v", r)
	}
	for _, head := range []string{"cd", "git status", "head"} {
		if r := toolRow(p, "bash", "Bash", head); r == nil || r.Calls != 1 || r.Hour != hour10 {
			t.Errorf("head %q: %+v", head, r)
		}
	}
	if r := toolRow(p, "bash", "Bash", "git push"); r == nil || r.Refusals != 1 || r.Hour != hour11 {
		t.Errorf("git push: %+v", r)
	}
	if r := toolRow(p, "builtin", "Read", ""); r == nil || r.Errors != 1 {
		t.Errorf("Read: %+v", r)
	}
	if r := toolRow(p, "skill", "", "claude-api"); r == nil || r.Calls != 1 || r.Prompts != 1 {
		t.Errorf("skill: %+v", r)
	}
	if r := toolRow(p, "subagent", "", "Explore"); r == nil || r.Calls != 1 {
		t.Errorf("subagent: %+v", r)
	}
	if r := toolRow(p, "mcp", "mcp__notion__search", "notion"); r == nil || r.Calls != 1 || r.Refusals != 1 {
		t.Errorf("mcp: %+v", r)
	}
	if r := toolRow(p, "command", "", "/review"); r == nil || r.Calls != 1 || r.Model != "" {
		t.Errorf("command: %+v", r)
	}

	if len(p.Chunk.Refusals) != 2 {
		t.Fatalf("refusals: %+v", p.Chunk.Refusals)
	}
	a, b := p.Chunk.Refusals[0], p.Chunk.Refusals[1]
	if a.Tool != "Bash" || a.Detail != "git push" || a.Reason != "Production Deploy" || a.Source != "classifier" || a.Excerpt != "git push origin main --token=***" {
		t.Errorf("classifier refusal: %+v", a)
	}
	if b.Tool != "mcp__notion__search" || b.Reason != "hook: guard.py" || b.Source != "hook" || b.Repo != "owner/app" {
		t.Errorf("hook refusal: %+v", b)
	}

	if len(p.Chunk.Sessions) != 1 {
		t.Fatalf("sessions: %+v", p.Chunk.Sessions)
	}
	s := p.Chunk.Sessions[0]
	if s.Title != "Fix the thing" || s.Branch != "feature/x" || s.Version != "2.1.1" || s.Cwd != "/work/app" || s.Compactions != 1 || s.Repo != "owner/app" ||
		s.First != time.Date(2026, 10, 5, 10, 0, 1, 0, time.UTC).UnixMilli() {
		t.Errorf("session: %+v", s)
	}

	if p.Stats.Skipped != 2 || p.Stats.Unknown["weird-new-thing"] != 1 || p.Stats.Unknown["(no type)"] != 1 || p.Stats.Ignored != 2 {
		t.Errorf("stats: %+v", p.Stats)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects", "-work-app", "sess1.jsonl"))
	if cut := bytes.LastIndexByte(raw, '\n') + 1; p.State.Off != int64(cut) {
		t.Errorf("offset %d, want %d (end of the last whole line)", p.State.Off, cut)
	}
}

func TestUsageParseIncremental(t *testing.T) {
	dir := fixtureDir(t)
	first := parseMain(t, dir, usageFileState{})
	path := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	more := `,"output_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"timestamp":"2026-10-05T12:00:00.000Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":10,"output_tokens":60,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":60,"ephemeral_1h_input_tokens":40}}},"timestamp":"2026-10-05T10:00:06.500Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"m4","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t9","name":"Bash","input":{"command":"make check"}}],"usage":{"input_tokens":2,"output_tokens":2}},"timestamp":"2026-10-05T12:01:00.000Z"}` + "\n"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(more)
	_ = f.Close()

	second := parseMain(t, dir, first.State)
	if second.From != first.State.Off || second.State.Off <= first.State.Off {
		t.Errorf("second run read %d..%d", second.From, second.State.Off)
	}
	byModelHour := map[int64]usageMsgRow{}
	for _, m := range second.Chunk.Messages {
		byModelHour[m.Hour] = m
	}
	h10 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).UnixMilli()
	h12 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC).UnixMilli()
	if m := byModelHour[h10]; m.N != 0 || m.Output != 10 || m.Input != 0 || m.CR != 0 {
		t.Errorf("m1 delta: %+v", m)
	}
	if m := byModelHour[h12]; m.N != 2 || m.Output != 4+2 || m.Input != 1+2 {
		t.Errorf("hour 12: %+v", m)
	}
	if r := toolRow(second, "bash", "Bash", "make check"); r == nil || r.Calls != 1 {
		t.Errorf("new call: %+v", r)
	}
	if len(second.Chunk.Sessions) != 1 || second.Chunk.Sessions[0].Compactions != 0 || second.Chunk.Sessions[0].Title != "Fix the thing" {
		t.Errorf("session meta carries over without recounting compactions: %+v", second.Chunk.Sessions)
	}
	if len(second.State.Open) != 1 || second.State.Open["t9"] == nil {
		t.Fatalf("the unanswered call should stay open: %+v", second.State.Open)
	}

	result := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","is_error":true,"content":"make: *** [check] Error 1"}]},"timestamp":"2026-10-05T12:09:00.000Z"}` + "\n"
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(result)
	_ = f.Close()
	third := parseMain(t, dir, second.State)
	if r := toolRow(third, "bash", "Bash", "make check"); r == nil || r.Errors != 1 || r.Calls != 0 || r.Hour != h12 {
		t.Errorf("late result: %+v", r)
	}
	if len(third.State.Open) != 0 {
		t.Errorf("open calls left: %+v", third.State.Open)
	}
	fourth := parseMain(t, dir, third.State)
	if fourth.Changed || fourth.Chunk.rows() != 0 {
		t.Errorf("idle run: %+v", fourth.Chunk)
	}
	_ = os.WriteFile(path, []byte(`{"type":"user","message":{"content":"hello"},"timestamp":"2026-10-05T13:00:00.000Z"}`+"\n"), 0o600)
	fifth := parseMain(t, dir, third.State)
	if fifth.From != 0 || !fifth.Changed {
		t.Errorf("rewritten file: from %d changed %v", fifth.From, fifth.Changed)
	}
}

func TestRewrittenMessageWithSmallerTotalsAddsNothingNegative(t *testing.T) {
	dir := fixtureDir(t)
	first := parseMain(t, dir, usageFileState{})
	path := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	var base int64
	for _, m := range first.State.Last {
		if m.ID == "m1" {
			base = m.Out
		}
	}
	appendLine := func(out int) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString(`{"type":"assistant","message":{"id":"m1","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":` + strconv.Itoa(out) + `}},"timestamp":"2026-10-05T10:00:06.500Z"}` + "\n")
		_ = f.Close()
	}
	appendLine(1)
	second := parseMain(t, dir, first.State)
	for _, m := range second.Chunk.Messages {
		if m.Input < 0 || m.Output < 0 || m.CW5 < 0 || m.CW1 < 0 || m.CR < 0 {
			t.Errorf("negative usage: %+v", m)
		}
	}
	appendLine(int(base) + 7)
	third := parseMain(t, dir, second.State)
	var out int64
	for _, m := range third.Chunk.Messages {
		out += m.Output
	}
	if out != 7 {
		t.Errorf("m1 grew by 7 over what was counted, but %d was added", out)
	}
}

func TestUsageSubagentFiles(t *testing.T) {
	dir := fixtureDir(t)
	files, err := discoverTranscripts(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files: %+v", files)
	}
	var sub usageFile
	for _, f := range files {
		if !f.Main {
			sub = f
		}
	}
	if sub.Kind != "Explore" || sub.Session != "sess1" {
		t.Fatalf("subagent: %+v", sub)
	}
	p, err := readUsageFile(sub, usageFileState{}, func(string) string { return "owner/app" }, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Chunk.Messages) != 1 || p.Chunk.Messages[0].Kind != "Explore" || p.Chunk.Messages[0].Model != "claude-haiku-5-5" || p.Chunk.Messages[0].Session != "sess1" {
		t.Errorf("messages: %+v", p.Chunk.Messages)
	}
	if len(p.Chunk.Sessions) != 0 {
		t.Errorf("a subagent file must not describe the session: %+v", p.Chunk.Sessions)
	}
	if r := toolRow(p, "builtin", "Grep", ""); r == nil || r.Calls != 1 {
		t.Errorf("tools: %+v", p.Chunk.Tools)
	}
}

func TestTopLevelType(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":{"type":"inner"},"type":"outer"}`:         "outer",
		`{"x":"he said \"type\":\"no\"","type":"real"}`: "real",
		`{"type":"frame-link","z":[{"type":"n"}]}`:      "frame-link",
		`{"nothing":1}`: "",
		`{"type":`:      "",
	} {
		if got := topLevelType([]byte(in)); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute, "3": 72 * time.Hour} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("%q → %v, %v", in, got, err)
		}
	}
	if _, err := parseSince("soon"); err == nil {
		t.Error("accepted soon")
	}
}

func TestResolveRepo(t *testing.T) {
	dir := gitRepo(t, "git@forge.example:acme/widgets.git")
	sub := filepath.Join(dir, "pkg", "x")
	_ = os.MkdirAll(sub, 0o700)
	r := newRepoResolver()
	if got := r.of(sub); got != "acme/widgets" {
		t.Errorf("subdirectory: %q", got)
	}
	if got := r.of(filepath.Join(dir, ".claude", "worktrees", "gone")); got != "acme/widgets" {
		t.Errorf("deleted worktree: %q", got)
	}
	if got := r.of(filepath.Join(t.TempDir(), "plain")); got != "plain" {
		t.Errorf("no repository: %q", got)
	}
}

func TestUsageSyncEndToEnd(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)

	out := e.wp("usage", "sync", "--all", "--dir", dir, "--json")
	var res usageSyncResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	if res.Files != 2 || res.FilesRead != 2 || res.Messages != 3 || res.Chunks != 2 || res.Accepted != 2 || res.Refusals != 2 || res.Skipped != 2 {
		t.Errorf("first sync: %+v", res)
	}
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Messages != 3 || ov.Totals.Output != 50+20+9 || ov.Totals.CacheRead != 1000+1200+300 || ov.Totals.CacheWrite != 100 {
		t.Errorf("totals: %+v", ov.Totals)
	}
	if ov.Refusals.Total != 2 || len(ov.Kinds) != 2 {
		t.Errorf("refusals %d, kinds %+v", ov.Refusals.Total, ov.Kinds)
	}
	if len(ov.Reported) != 1 || ov.Reported[0].Name != "testbox" || ov.Reported[0].Skipped != 2 {
		t.Errorf("machine: %+v", ov.Reported)
	}

	out = e.wp("usage", "sync", "--all", "--dir", dir, "--json")
	_ = json.Unmarshal([]byte(out), &res)
	if res.FilesRead != 0 || res.Messages != 0 {
		t.Errorf("second sync: %+v", res)
	}

	if err := os.Remove(filepath.Join(e.dir, usageStateFile)); err != nil {
		t.Fatal(err)
	}
	out = e.wp("usage", "sync", "--all", "--dir", dir, "--json")
	_ = json.Unmarshal([]byte(out), &res)
	if res.Duplicate != 2 || res.Accepted != 0 {
		t.Errorf("resend: %+v", res)
	}
	ov, _ = e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if ov.Totals.Messages != 3 || ov.Refusals.Total != 2 {
		t.Errorf("counted twice: %+v", ov.Totals)
	}

	if err := os.Remove(filepath.Join(e.dir, usageStateFile)); err != nil {
		t.Fatal(err)
	}
	e2 := core.UsageQuery{Days: 3650}
	hookIn := `{"session_id":"sess1","transcript_path":"` + filepath.Join(dir, "projects", "-work-app", "sess1.jsonl") + `"}`
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	usageHook(c, strings.NewReader(hookIn), dir)
	if ov, _ = e.svc.UsageOverview(context.Background(), me, e2); ov.Totals.Messages != 3 {
		t.Errorf("after the hook: %+v", ov.Totals)
	}
	usageHook(c, strings.NewReader("not json"), dir)
}

func TestUsageSyncDryRunChangesNothing(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WAYPOINT_TOKEN", "")
	dir := fixtureDir(t)
	var out bytes.Buffer
	if code := run([]string{"usage", "sync", "--all", "--dry-run", "--dir", dir}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "Dry run") || !strings.Contains(out.String(), "3 messages") || !strings.Contains(out.String(), "weird-new-thing") {
		t.Errorf("output: %s", out.String())
	}
	cfg, _ := configDir()
	if _, err := os.Stat(filepath.Join(cfg, usageStateFile)); err == nil {
		t.Error("a dry run remembered something")
	}
	out.Reset()
	if code := run([]string{"usage", "sync", "--dir", dir}, &out, &out); code == 0 || !strings.Contains(out.String(), "not logged in") {
		t.Errorf("without a login: %d %s", code, out.String())
	}
}

func runStatusline(t *testing.T, c *cli, stdin string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	c.out, c.err = &out, &out
	statuslineStdin = strings.NewReader(stdin)
	if err := cmdStatusline(c, args); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

const statusJSON = `{"session_id":"s1","model":{"id":"claude-opus-5-5","display_name":"Opus 5.5"},"context_window":{"used_percentage":42.4},` +
	`"rate_limits":{"five_hour":{"used_percentage":81,"resets_at":1790000000},"seven_day":{"used_percentage":63.2,"resets_at":"2026-10-17T15:00:00Z"}}}`

func TestStatusline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAYPOINT_SERVER", "https://wp.test")
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	var posts [][]byte
	var mu sync.Mutex
	old := statuslineSpawn
	statuslineSpawn = func(p []byte) { mu.Lock(); posts = append(posts, p); mu.Unlock() }
	t.Cleanup(func() { statuslineSpawn = old; statuslineStdin = os.Stdin })
	t.Setenv("WP_TASK", "")
	c := &cli{dir: dir}

	if got := runStatusline(t, c, statusJSON); got != "Opus 5.5 · ctx 42% · 5h 81% · 7d 63%\n" {
		t.Errorf("default line: %q", got)
	}
	if len(posts) != 1 {
		t.Fatalf("posts: %d", len(posts))
	}
	runStatusline(t, c, statusJSON)
	if len(posts) != 1 {
		t.Errorf("rate limit: %d posts", len(posts))
	}
	stamp := filepath.Join(dir, "statusline.stamp")
	past := time.Now().Add(-2 * time.Minute)
	_ = os.Chtimes(stamp, past, past)
	runStatusline(t, c, statusJSON)
	if len(posts) != 2 {
		t.Errorf("after a minute: %d posts", len(posts))
	}
	_ = os.Remove(stamp)
	if got := runStatusline(t, c, `{"model":{"display_name":"Haiku"}}`); got != "Haiku\n" || len(posts) != 2 {
		t.Errorf("no quota: %q, %d posts", got, len(posts))
	}
	if got := runStatusline(t, c, `not json at all`); got != "Claude Code\n" {
		t.Errorf("garbage: %q", got)
	}

	if got := runStatusline(t, c, statusJSON, "--", "sh", "-c", `printf 'mine: %s' "$(cat | head -c 20)"`); got != `mine: {"session_id":"s1","` {
		t.Errorf("wrapped (argv): %q", got)
	}
	if got := runStatusline(t, c, statusJSON, "--", `cat | grep -o '"session_id":"[a-z0-9]*"'`); got != `"session_id":"s1"`+"\n" {
		t.Errorf("wrapped (shell line): %q", got)
	}
	if got := runStatusline(t, c, statusJSON, "--", "definitely-not-a-command-xyz"); !strings.Contains(got, "not found") {
		t.Errorf("a failing command shows its own error: %q", got)
	}
}

// The default line shows the context in tokens and the task the session
// works on: a task session's binding, else what it took with wp next (WP-54).
func TestStatuslineContextAndTask(t *testing.T) {
	t.Setenv("WAYPOINT_SERVER", "")
	t.Setenv("WP_TASK", "")
	t.Cleanup(func() { statuslineStdin = os.Stdin })
	here, other := t.TempDir(), t.TempDir()
	c := &cli{dir: t.TempDir()}
	line := func(session, cwd, ctx string) string {
		return fmt.Sprintf(`{"session_id":%q,"cwd":%q,"model":{"display_name":"Opus 5.5"},"context_window":%s}`, session, cwd, ctx)
	}
	usage := `{"used_percentage":42.4,"total_input_tokens":999,"current_usage":{"input_tokens":12,"output_tokens":400,"cache_creation_input_tokens":3488,"cache_read_input_tokens":180000}}`

	if got := runStatusline(t, c, line("s1", here, usage)); got != "Opus 5.5 · ctx 184k 42%\n" {
		t.Errorf("context from the last call: %q", got)
	}
	if got := runStatusline(t, c, line("s1", here, `{"total_input_tokens":52000}`)); got != "Opus 5.5 · ctx 52k\n" {
		t.Errorf("context from the total: %q", got)
	}

	c.state.Claims = map[string]*claim{here: {Key: "WP-7", Also: []string{"WP-8"}, Checked: true, Top: here}}
	if got := runStatusline(t, c, line("s1", here, usage)); got != "Opus 5.5 · WP-7 +1 · ctx 184k 42%\n" {
		t.Errorf("claim here: %q", got)
	}
	c.state.Claims[here].Session = "s2"
	if got := runStatusline(t, c, line("s1", here, usage)); got != "Opus 5.5 · ctx 184k 42%\n" {
		t.Errorf("another session's claim: %q", got)
	}
	c.state.Claims[other] = &claim{Key: "WP-9", Session: "s1", Checked: true, Top: other}
	if got := runStatusline(t, c, line("s1", here, usage)); got != "Opus 5.5 · WP-9 · ctx 184k 42%\n" {
		t.Errorf("session's claim elsewhere: %q", got)
	}
	t.Setenv("WP_TASK", "wp-54")
	if got := runStatusline(t, c, line("s1", here, usage)); got != "Opus 5.5 · WP-54 · ctx 184k 42%\n" {
		t.Errorf("task session: %q", got)
	}
}

func TestStatuslinePostBody(t *testing.T) {
	var got map[string]any
	var path, auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("{}"))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	t.Setenv("WAYPOINT_MACHINE", "box")
	old := httpClient
	t.Cleanup(func() { httpClient = old; statuslineStdin = os.Stdin })
	runStatusline(t, &cli{dir: t.TempDir()}, statusJSON, "--post")
	if httpClient != old {
		t.Error("the quota report replaced the shared HTTP client")
	}
	if path != "/api/v1/usage/quota" || auth != "Bearer wpt_x" || got["machine"] != "box" || got["session"] != "s1" || got["model"] != "claude-opus-5-5" || got["context_pct"] != 42.4 {
		t.Errorf("post: %s %s %v", path, auth, got)
	}
	five := got["five_hour"].(map[string]any)
	seven := got["seven_day"].(map[string]any)
	if five["percent"] != 81.0 || five["resets_at"] != 1790000000.0*1000 ||
		seven["percent"] != 63.2 || seven["resets_at"] != float64(time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC).UnixMilli()) {
		t.Errorf("windows: %v %v", five, seven)
	}
	for k := range got {
		switch k {
		case "machine", "session", "model", "context_pct", "five_hour", "seven_day":
		default:
			t.Errorf("unexpected field %q", k)
		}
	}
}

func TestStatuslineDoesNotWait(t *testing.T) {
	var hits atomic.Int32
	hold := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		<-hold
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(hold) })
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	old := statuslineSpawn
	var handed atomic.Int32
	statuslineSpawn = func([]byte) { handed.Add(1) }
	t.Cleanup(func() { statuslineSpawn = old; statuslineStdin = os.Stdin })
	start := time.Now()
	runStatusline(t, &cli{dir: t.TempDir()}, statusJSON)
	d := time.Since(start)
	if n := hits.Load(); n != 0 {
		t.Errorf("the status line itself sent %d requests", n)
	}
	if handed.Load() != 1 {
		t.Errorf("the report was handed off %d times, want 1", handed.Load())
	}
	if d >= statusPostWait {
		t.Errorf("status line took %v", d)
	}
}

func TestIsOurStatusLine(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/local/bin/wp statusline":    true,
		`'/home/me/my bin/wp' statusline`: true,
		"wp statusline -- 'bash x.sh'":    true,
		"wpctl statusline":                false,
		"/opt/wpa/wp-status statusline":   false,
		"wp status":                       false,
		"bash ~/.claude/statusline.sh":    false,
	} {
		if got := isOurStatusLine(cmd); got != want {
			t.Errorf("isOurStatusLine(%q) = %v", cmd, got)
		}
	}
}

func TestUsageHookAndStatusLineInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	t.Setenv("WP_CLAUDE_SETTINGS", path)
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	original := `bash ~/.claude/statusline.sh | sed 's/x/y/'`
	b, _ := json.Marshal(map[string]any{"statusLine": map[string]any{"type": "command", "command": original, "padding": 0},
		"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "mine"}}}}}})
	_ = os.WriteFile(path, b, 0o600)

	var out bytes.Buffer
	if code := run([]string{"hook", "claude"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	var s map[string]any
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &s)
	sl := s["statusLine"].(map[string]any)
	cmd := sl["command"].(string)
	if !isOurStatusLine(cmd) || !strings.Contains(cmd, " statusline -- ") || sl["padding"] != 0.0 {
		t.Fatalf("status line: %v", sl)
	}
	if w, ok := wrappedStatusLine(cmd); !ok || w != original {
		t.Errorf("wrapped command: %q (%v)", w, ok)
	}
	for _, event := range []string{"Stop", "SessionEnd"} {
		found := false
		for _, g := range s["hooks"].(map[string]any)[event].([]any) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				hm := h.(map[string]any)
				if strings.Contains(hm["command"].(string), "usage sync --hook") {
					found = true
					if hm["async"] != true {
						t.Errorf("%s hook isn't async: %v", event, hm)
					}
				}
			}
		}
		if !found {
			t.Errorf("no usage hook on %s", event)
		}
	}
	if !strings.Contains(string(raw), `"mine"`) {
		t.Error("the person's own Stop hook was lost")
	}
	out.Reset()
	run([]string{"hook", "claude"}, &out, &out)
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("second install: %s", out.String())
	}
	run([]string{"hook", "claude", "--uninstall"}, &out, &out)
	raw, _ = os.ReadFile(path)
	s = nil
	_ = json.Unmarshal(raw, &s)
	if got := s["statusLine"].(map[string]any)["command"]; got != original {
		t.Errorf("restored status line: %v", got)
	}
	if strings.Contains(string(raw), "usage sync") || !strings.Contains(string(raw), `"mine"`) {
		t.Errorf("after uninstall: %s", raw)
	}

	_ = os.WriteFile(path, []byte(`{}`), 0o600)
	run([]string{"hook", "claude"}, &out, &out)
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), `statusline"`) {
		t.Errorf("fresh install: %s", raw)
	}
	run([]string{"hook", "claude", "--uninstall"}, &out, &out)
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "statusLine") {
		t.Errorf("fresh uninstall left it: %s", raw)
	}
}

// A sync of anything also reads again what it had synced before the newest
// kind of row, however old, and leaves alone what it never synced.
func TestUsageSyncFillsInOutdatedTranscripts(t *testing.T) {
	linkCommands()
	e := newEnv(t)
	me := e.login()
	dir := filepath.Join(t.TempDir(), "claude")
	work := filepath.Join(dir, "projects", "-work-app")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	write := func(name, cmd string) (string, usageFile) {
		p := filepath.Join(work, name+".jsonl")
		line := riskyToolUse("m-"+name, "t-"+name, cmd, at) + "\n"
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		return p, usageFile{Path: p, Session: name, Kind: "main", Main: true}
	}
	old, oldFile := write("old", "wp ls")
	unknown, _ := write("unknown", "wp show WP-1")
	current, _ := write("current", "git status")
	month := time.Now().Add(-60 * 24 * time.Hour)
	for _, p := range []string{old, unknown} {
		if err := os.Chtimes(p, month, month); err != nil {
			t.Fatal(err)
		}
	}

	parsed, err := readUsageFile(oldFile, usageFileState{}, func(string) string { return "a/b" }, false)
	if err != nil {
		t.Fatal(err)
	}
	st := parsed.State
	st.CV, st.CLICounted, st.CLIPending = 0, false, nil
	state := filepath.Join(e.dir, usageStateFile)
	if err := writeJSON(state, usageState{Files: map[string]*usageFileState{old: &st}}); err != nil {
		t.Fatal(err)
	}

	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	sync := func() *usageSyncResult {
		t.Helper()
		res, err := usageSync(c, usageSyncOptions{Hook: true, Dir: dir, Files: []string{current}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := sync(); res.Files != 2 || res.FilesRead != 2 {
		t.Errorf("first sync: %+v", res)
	}
	r, err := e.svc.UsageCLI(context.Background(), me, core.UsageQuery{Days: 30}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.WP != 1 || len(r.Commands) != 1 || r.Commands[0].Head != "wp ls" {
		t.Errorf("after the first sync: total %d, commands %+v", r.Total.WP, r.Commands)
	}
	after := loadUsageState(e.dir)
	if after.Files[old] == nil || after.Files[old].CV != usageCLIVersion {
		t.Errorf("old wasn't brought up to date: %+v", after.Files[old])
	}
	if after.Files[unknown] != nil {
		t.Error("a transcript never synced was read")
	}
	if res := sync(); res.FilesRead != 0 {
		t.Errorf("second sync read again: %+v", res)
	}
}

func TestOutdatedTranscriptsOnlyUnderTheDirectory(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	st := usageState{Files: map[string]*usageFileState{}}
	for _, d := range []string{root, other} {
		w := filepath.Join(d, "projects", "x")
		if err := os.MkdirAll(w, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(w, "s.jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		st.Files[p] = &usageFileState{Off: 3}
	}
	st.Files[filepath.Join(root, "projects", "x", "gone.jsonl")] = &usageFileState{Off: 3}
	got := outdatedTranscripts(root, &st, nil)
	if want := filepath.Join(root, "projects", "x", "s.jsonl"); len(got) != 1 || got[0].Path != want {
		t.Errorf("got %+v, want %s", got, want)
	}
	if again := outdatedTranscripts(root, &st, got); again != nil {
		t.Errorf("a file already in this sync came back: %+v", again)
	}
}

func TestUsageParseSyntheticIsToolRowsNotCalls(t *testing.T) {
	dir := t.TempDir()
	tr := `{"type":"assistant","timestamp":"2026-10-05T10:00:01.000Z","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":0},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"assistant","isVirtual":true,"timestamp":"2026-10-05T10:00:02.000Z","message":{"id":"repl_1","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"tool_use","id":"repl_t1","name":"Bash","input":{"command":"ls"}}]}}
`
	p := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(tr), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseMain(t, dir, usageFileState{})
	if len(got.Chunk.Messages) != 1 || got.Chunk.Messages[0].N != 1 || got.Chunk.Messages[0].Model != "claude-opus-5-5" {
		t.Fatalf("messages (a synthetic record is no model call, no cost row): %+v", got.Chunk.Messages)
	}
	var typed, script int64
	for _, r := range got.Chunk.Tools {
		if r.Kind == "builtin" && r.Tool == "Bash" {
			if r.Model == "<synthetic>" {
				script += r.Calls
			} else {
				typed += r.Calls
			}
		}
	}
	if typed != 1 || script != 1 {
		t.Fatalf("Bash rows: model-typed %d, script-run (model <synthetic>) %d, tools %+v", typed, script, got.Chunk.Tools)
	}
}

func TestUsageParseBashShellEditRows(t *testing.T) {
	dir := t.TempDir()
	tr := `{"type":"assistant","timestamp":"2026-10-05T10:00:01.000Z","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo x > /work/app/out.txt"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}]}}
`
	p := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(tr), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseMain(t, dir, usageFileState{})
	var bash, shell int64
	var marked bool
	for _, r := range got.Chunk.Tools {
		switch {
		case r.Kind == "builtin" && r.Tool == "Bash":
			bash += r.Calls
		case r.Kind == trscript.ShellEditKind:
			marked = true
			shell += r.Calls
		}
	}
	if bash != 2 || !marked || shell != 1 {
		t.Fatalf("Bash calls %d, shell_edit row present %v with %d calls; tools %+v", bash, marked, shell, got.Chunk.Tools)
	}
}

// A script kept in a file counts as the edit it makes when a later call runs
// it, in the same transcript: saved with a heredoc or with the Write tool.
func TestUsageParseShellEditRowsKeepScripts(t *testing.T) {
	dir := t.TempDir()
	tool := func(id, name, in string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + in + `}`
	}
	line := func(id string, tools ...string) string {
		return `{"type":"assistant","timestamp":"2026-10-05T10:00:01.000Z","message":{"id":"` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2},"content":[` + strings.Join(tools, ",") + `]}}` + "\n"
	}
	tr := line("m1",
		tool("t1", "Bash", `{"command":"cat > /tmp/e.py <<'EOF'\nopen('/work/app/a.go','w').write('x')\nEOF"}`),
		tool("t2", "Write", `{"file_path":"/tmp/w.py","content":"open('/work/app/b.go','w').write('x')\n"}`)) +
		line("m2",
			tool("t3", "Bash", `{"command":"python3 /tmp/e.py"}`),
			tool("t4", "Bash", `{"command":"python3 /tmp/w.py"}`),
			tool("t5", "Bash", `{"command":"python3 /tmp/none.py"}`))
	p := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(tr), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseMain(t, dir, usageFileState{})
	var shell int64
	for _, r := range got.Chunk.Tools {
		if r.Kind == trscript.ShellEditKind {
			shell += r.Calls
		}
	}
	if shell != 2 {
		t.Fatalf("shell edits %d, want 2 (both kept scripts, not the unknown one); tools %+v", shell, got.Chunk.Tools)
	}
}

func TestEditsText(t *testing.T) {
	for _, c := range []struct {
		edits, shell, unc int64
		want              string
	}{
		{11, 193, 0, "edits 11, shell edits 193"},
		{11, 0, 40, "edits 11, shell edits unknown (40 Bash calls not counted)"},
		{11, 5, 40, "edits 11, shell edits 5+ (40 Bash calls not counted)"},
	} {
		if got := editsText(c.edits, c.shell, c.unc); got != c.want {
			t.Errorf("editsText(%d,%d,%d) = %q, want %q", c.edits, c.shell, c.unc, got, c.want)
		}
	}
}

// A server older than the shell-edits figure sends only "edits": the report
// must not print "shell edits 0" for it (audit 1907.18).
func TestTaskUsageTextOlderServer(t *testing.T) {
	var old, current taskUsageReport
	if err := json.Unmarshal([]byte(`{"key":"WP-1","edits":4}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := taskUsageText(&old); !strings.Contains(got, "edits 4, shell edits unknown (server too old to say)") || strings.Contains(got, "shell edits 0") {
		t.Errorf("older server: %q", got)
	}
	if err := json.Unmarshal([]byte(`{"key":"WP-1","edits":4,"shell_edits":0,"shell_edits_uncounted":0}`), &current); err != nil {
		t.Fatal(err)
	}
	if got := taskUsageText(&current); !strings.Contains(got, "edits 4, shell edits 0") {
		t.Errorf("a server that counted none: %q", got)
	}
}
