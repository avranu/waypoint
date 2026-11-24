package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// burnLine is an assistant line of a transcript: n tokens of cache read make
// the context; opus 5.5 reads cost $0.20 per million.
func burnLine(id, model string, at time.Time, in, out, cr, cw5, cw1 int64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"id": id, "model": model, "usage": map[string]any{
			"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cr,
			"cache_creation_input_tokens": cw5 + cw1,
			"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": cw5, "ephemeral_1h_input_tokens": cw1},
		}},
	})
	return string(b) + "\n"
}

func writeBurnFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendBurn(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(lines, "")); err != nil {
		t.Fatal(err)
	}
}

const (
	opus   = "claude-opus-5-5"
	sonnet = "claude-sonnet-5-5"
)

// burnFixture: a main transcript with two calls, an Opus fork that starts
// with a copy of the main's first message and has one call of its own, and a
// Sonnet worker.
func burnFixture(t *testing.T) (main string, t0 time.Time) {
	t.Helper()
	for _, k := range []string{"WP_TASK", "WAYPOINT_FOLLOWUP", "WAYPOINT_UNATTENDED"} {
		t.Setenv(k, "")
	}
	root := t.TempDir()
	main = filepath.Join(root, "proj", "sess1.jsonl")
	t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	writeBurnFile(t, main,
		burnLine("m1", opus, t0, 10, 1000, 100_000, 20_000, 0),
		burnLine("m1", opus, t0, 10, 1000, 100_000, 20_000, 0),
		burnLine("m2", opus, t0.Add(time.Minute), 10, 2000, 200_000, 0, 10_000),
	)
	sub := filepath.Join(root, "proj", "sess1", "subagents")
	writeBurnFile(t, filepath.Join(sub, "agent-f1.jsonl"),
		burnLine("m1", opus, t0, 10, 1000, 100_000, 20_000, 0),
		burnLine("f1a", opus, t0.Add(2*time.Minute), 0, 500, 300_000, 0, 0),
	)
	writeBurnFile(t, filepath.Join(sub, "agent-f1.meta.json"), `{"agentType":"fork","isFork":true,"spawnDepth":1,"description":"Port search"}`)
	writeBurnFile(t, filepath.Join(sub, "agent-w1.jsonl"),
		burnLine("w1a", sonnet, t0.Add(3*time.Minute), 100, 1000, 50_000, 0, 0),
	)
	writeBurnFile(t, filepath.Join(sub, "agent-w1.meta.json"), `{"agentType":"sonnet-worker","spawnDepth":1,"parentAgentId":"f1","description":"Fix tests"}`)
	return main, t0
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestBurnTreeCountsForkedMessagesOnce(t *testing.T) {
	main, _ := burnFixture(t)
	tree := newBurnTree()
	tree.update(burnFiles(main), nil)
	m := tree.Agents["main"]
	if m.Calls != 2 || !near(m.Cost, 0.30008) {
		t.Fatalf("main: %d calls, $%.5f", m.Calls, m.Cost)
	}
	if m.CtxMax != 210_010 || m.avgCtx() != (120_010+210_010)/2 {
		t.Fatalf("main context: max %d avg %d", m.CtxMax, m.avgCtx())
	}
	f := tree.Agents["f1"]
	if f.Kind != "fork" || f.Calls != 1 || f.Depth != 1 || f.Desc != "Port search" {
		t.Fatalf("the fork counts only its own message: %+v", f)
	}
	if !near(f.Cost, 0.07) {
		t.Fatalf("fork cost $%.5f", f.Cost)
	}
	w := tree.Agents["w1"]
	if w.Kind != "sonnet-worker" || !near(w.Cost, 0.0202) {
		t.Fatalf("worker: %+v", w)
	}
	s := tree.totals()
	if s.Agents != 2 || s.Calls != 4 || !near(s.Cost, 0.30008+0.07+0.0202) || s.minutes() != 3 {
		t.Fatalf("totals: %+v", s)
	}
	txt := tree.report(main).text()
	if !strings.Contains(txt, "TOTAL: 2 agents, 4 calls") || !strings.Contains(txt, "fork@d1") {
		t.Fatalf("report:\n%s", txt)
	}
}

func TestBurnPriceFromTable(t *testing.T) {
	table := []burnPrice{{Model: "claude-opus-5-5", Input: 1, Output: 2}, {Model: "claude-opus-5", Input: 9}}
	if p := burnPriceFor(table, "claude-opus-5-5-20261001"); p.Input != 1 {
		t.Fatalf("the longest prefix wins: %+v", p)
	}
	if p := burnPriceFor(nil, "claude-haiku-5-5"); p.Output != 0.5 {
		t.Fatalf("fallback by family: %+v", p)
	}
}

func TestBurnReadsIncrementally(t *testing.T) {
	main, t0 := burnFixture(t)
	tree := newBurnTree()
	tree.update(burnFiles(main), nil)
	before := tree.totals().Cost
	if tree.update(burnFiles(main), nil) {
		t.Fatal("nothing new, yet the tree changed")
	}
	appendBurn(t, main, burnLine("m3", opus, t0.Add(4*time.Minute), 0, 1000, 0, 0, 0))
	partial := burnLine("m4", opus, t0.Add(5*time.Minute), 0, 1000, 0, 0, 0)
	appendBurn(t, main, partial[:20])
	tree.update(burnFiles(main), nil)
	if got := tree.totals().Cost - before; !near(got, 0.02) {
		t.Fatalf("added $%.5f, want $0.02", got)
	}
	appendBurn(t, main, partial[20:])
	tree.update(burnFiles(main), nil)
	if got := tree.totals().Cost - before; !near(got, 0.04) {
		t.Fatalf("after the line completed: $%.5f, want $0.04", got)
	}
}

// burnRun runs the hook for a session at a fake time and returns its output.
func burnRun(t *testing.T, c *cli, session, main string, now time.Time) string {
	t.Helper()
	old := burnNow
	burnNow = func() time.Time { return now }
	defer func() { burnNow = old }()
	var out bytes.Buffer
	in := fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"hook_event_name":"PostToolUse","tool_name":"Read"}`, session, main)
	hookClaudeBurn(c, strings.NewReader(in), &out)
	return out.String()
}

func TestBurnHookStaysSilentAtNormalRates(t *testing.T) {
	main, t0 := burnFixture(t)
	c := &cli{dir: t.TempDir()}
	appendBurn(t, main, burnLine("big1", opus, t0.Add(5*time.Minute), 0, 300_000, 0, 0, 0))
	for i := range 3 {
		if got := burnRun(t, c, "quiet", main, t0.Add(time.Duration(6+i)*time.Minute)); got != "" {
			t.Fatalf("a normal rate must not speak: %s", got)
		}
	}
}

func TestBurnHookFiresOnceThenOnDoubling(t *testing.T) {
	main, t0 := burnFixture(t)
	c := &cli{dir: t.TempDir()}
	appendBurn(t, main, burnLine("big1", opus, t0.Add(5*time.Minute), 0, 1_200_000, 0, 0, 0))
	now := t0.Add(6 * time.Minute)
	out := burnRun(t, c, "fast", main, now)
	var o struct {
		S struct {
			Name string `json:"hookEventName"`
			Ctx  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.S.Name != "PostToolUse" {
		t.Fatalf("output %q: %v", out, err)
	}
	if !strings.HasPrefix(o.S.Ctx, "Unusual spend: this session and 2 agents are running at $146/h (avg context ") || !strings.Contains(o.S.Ctx, "`wp burn` shows which.") {
		t.Fatalf("line: %s", o.S.Ctx)
	}
	if got := burnRun(t, c, "fast", main, now.Add(time.Minute)); got != "" {
		t.Fatalf("it fires once: %s", got)
	}
	appendBurn(t, main, burnLine("big2", opus, t0.Add(7*time.Minute), 0, 400_000, 0, 0, 0))
	if got := burnRun(t, c, "fast", main, t0.Add(8*time.Minute)); got != "" {
		t.Fatalf("not doubled: %s", got)
	}
	appendBurn(t, main, burnLine("big3", opus, t0.Add(9*time.Minute), 0, 3_000_000, 0, 0, 0))
	if got := burnRun(t, c, "fast", main, t0.Add(10*time.Minute)); !strings.Contains(got, "Unusual spend") {
		t.Fatalf("doubled rate must fire again: %q", got)
	}
}

func TestBurnHookRepeatsAfterThirtyMinutes(t *testing.T) {
	st := &burnState{Tree: newBurnTree()}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	st.Tree.Agents["main"] = &burnAgent{Kind: "main", Calls: 1, CtxSum: 100_000}
	st.Tree.Minutes[now.Unix()/60] = 20
	if line, _ := st.decide(now); line == "" {
		t.Fatal("first")
	}
	st.Tree.Minutes[now.Add(29*time.Minute).Unix()/60] = 20
	if line, _ := st.decide(now.Add(29 * time.Minute)); line != "" {
		t.Fatalf("too soon: %s", line)
	}
	st.Tree.Minutes[now.Add(31*time.Minute).Unix()/60] = 20
	if line, _ := st.decide(now.Add(31 * time.Minute)); line == "" {
		t.Fatal("30 minutes above the line speaks again")
	}
}

func TestBurnHookCanBeTurnedOff(t *testing.T) {
	t.Setenv("WP_BURN_ALERT_PER_HOUR", "0")
	main, t0 := burnFixture(t)
	c := &cli{dir: t.TempDir()}
	appendBurn(t, main, burnLine("big1", opus, t0.Add(5*time.Minute), 0, 3_000_000, 0, 0, 0))
	if got := burnRun(t, c, "off", main, t0.Add(6*time.Minute)); got != "" {
		t.Fatalf("0 turns it off: %s", got)
	}
	t.Setenv("WP_BURN_ALERT_PER_HOUR", "5000")
	if got := burnRun(t, c, "off", main, t0.Add(6*time.Minute)); got != "" {
		t.Fatalf("a higher line is not crossed: %s", got)
	}
}

func TestBurnFindsTheSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("WAYPOINT_TRANSCRIPT", "")
	t.Setenv("WAYPOINT_SESSION", "")
	cwd := "/work/my.repo"
	dir := filepath.Join(cfg, "projects", "-work-my-repo")
	old, newer := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "new.jsonl")
	writeBurnFile(t, old)
	writeBurnFile(t, newer)
	_ = os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	c := &cli{}
	if got, err := c.currentTranscript("", cwd); err != nil || got != newer {
		t.Fatalf("newest for the directory: %q %v", got, err)
	}
	c.state.Cwds = map[string]*cwdSession{cwd: {Session: "old"}}
	if got, _ := c.currentTranscript("", cwd+"/sub"); got != old {
		t.Fatalf("the hook-recorded session wins: %q", got)
	}
	if got, _ := c.currentTranscript("new", cwd); got != newer {
		t.Fatalf("--session: %q", got)
	}
	if _, err := c.currentTranscript("nope", cwd); err == nil {
		t.Fatal("an unknown session is an error")
	}
}

func TestBurnHookCatchesUpInBoundedSteps(t *testing.T) {
	old := burnHookBudget
	burnHookBudget = 500
	defer func() { burnHookBudget = old }()
	main, t0 := burnFixture(t)
	appendBurn(t, main, burnLine("big", opus, t0.Add(5*time.Minute), 0, 300_000, 0, 0, 0))
	t.Setenv("WP_BURN_ALERT_PER_HOUR", "10")
	old2 := burnNow
	burnNow = func() time.Time { return t0.Add(6 * time.Minute) }
	defer func() { burnNow = old2 }()
	c := &cli{dir: t.TempDir()}
	in := fmt.Sprintf(`{"session_id":"cu","transcript_path":%q}`, main)
	var out bytes.Buffer
	calls := 0
	for ; calls < 20 && out.Len() == 0; calls++ {
		hookClaudeBurn(c, strings.NewReader(in), &out)
		st := &burnState{}
		_ = readJSON(c.burnPath("cu"), st)
		if calls == 0 && (out.Len() != 0 || len(st.Tree.Offsets) == 0) {
			t.Fatalf("the first call must save its offsets and stay silent while behind: %q", out.String())
		}
	}
	if calls < 2 || !strings.Contains(out.String(), "Unusual spend") {
		t.Fatalf("caught up after %d calls: %q", calls, out.String())
	}
}

func TestBurnHookIsFast(t *testing.T) {
	main, t0 := burnFixture(t)
	var lines []string
	for i := range 3000 {
		lines = append(lines, burnLine(fmt.Sprintf("x%d", i), opus, t0.Add(time.Duration(i)*time.Second), 5, 100, 200_000, 0, 0))
	}
	appendBurn(t, main, lines...)
	c := &cli{dir: t.TempDir()}
	in := fmt.Sprintf(`{"session_id":"fast","transcript_path":%q}`, main)
	hookClaudeBurn(c, strings.NewReader(in), &bytes.Buffer{})
	best := time.Hour
	for range 5 {
		start := time.Now()
		hookClaudeBurn(c, strings.NewReader(in), &bytes.Buffer{})
		best = min(best, time.Since(start))
	}
	if best > 50*time.Millisecond {
		t.Errorf("a hook with nothing new took %v", best)
	}
}
