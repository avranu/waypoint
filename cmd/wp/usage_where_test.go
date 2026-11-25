package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// whLines builds transcript lines for the where tests.
type whLines struct{ b strings.Builder }

func (w *whLines) raw(v any) {
	j, _ := json.Marshal(v)
	w.b.Write(j)
	w.b.WriteByte('\n')
}

func (w *whLines) user(ts, text string) {
	w.raw(map[string]any{"type": "user", "timestamp": ts, "cwd": "/work/proj", "message": map[string]any{"role": "user", "content": text}})
}

func (w *whLines) result(ts, toolID string) {
	w.raw(map[string]any{"type": "user", "timestamp": ts, "cwd": "/work/proj", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": "ok"}}}})
}

// call writes one assistant record of the API call (id, req) with usage and tool blocks.
func (w *whLines) call(ts, id, req, model string, out int64, blocks ...map[string]any) {
	content := []any{}
	for _, b := range blocks {
		content = append(content, b)
	}
	w.raw(map[string]any{"type": "assistant", "timestamp": ts, "cwd": "/work/proj", "requestId": req, "message": map[string]any{
		"id": id, "model": model, "content": content,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": out, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200,
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 100, "ephemeral_1h_input_tokens": 100}}}})
}

func bash(id, cmd string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}
}

func whWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const whWin = "--since=2026-11-24T00:00:00Z"

func whRun(t *testing.T, dir string, args ...string) (*whReport, string) {
	t.Helper()
	var out bytes.Buffer
	c := &cli{out: &out, dir: t.TempDir()}
	if err := cmdUsageWhere(c, append([]string{"--dir", dir, "--json"}, args...)); err != nil {
		t.Fatal(err)
	}
	var r whReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	return &r, out.String()
}

func whClass(r *whReport, model, class string) whClassRow {
	for _, a := range r.Activity {
		if a.Model == model {
			for _, k := range a.Classes {
				if k.Class == class {
					return k
				}
			}
		}
	}
	return whClassRow{}
}

func TestWhereDedupeKeepsLargest(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "do it")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-sonnet-5-5", 500)
	w.call("2026-11-24T10:00:03Z", "m1", "r1", "claude-sonnet-5-5", 50)
	w.call("2026-11-24T10:00:04Z", "m1", "r2", "claude-sonnet-5-5", 7) // same message id, another request: its own call
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if r.Calls != 2 {
		t.Fatalf("calls = %d, want 2", r.Calls)
	}
	want := 500*10.0/1e6 + 7*10.0/1e6 // sonnet output $10/M
	if got := r.Parts.Output; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("output cost %v, want %v (the largest of the repeats)", got, want)
	}
	if r.Parts.Write5m == 0 || r.Parts.Write1h == 0 || r.Parts.Read == 0 {
		t.Fatalf("cache split missing: %+v", r.Parts)
	}
}

func TestWhereSyntheticCountedNotCosted(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.raw(map[string]any{"type": "assistant", "timestamp": "2026-11-24T10:00:01Z", "message": map[string]any{"id": "s1", "model": "<synthetic>",
		"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/work/proj/a.go"}}},
		"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if r.Calls != 1 || r.NotKnown.Synthetic != 1 || r.NotKnown.ScriptTools[whReadFile] != 1 {
		t.Fatalf("calls %d synthetic %d scripts %v", r.Calls, r.NotKnown.Synthetic, r.NotKnown.ScriptTools)
	}
}

func TestWhereCompoundShellEditAndTests(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "fix")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "cd x && sed -i 's/a/b/' f.go && go test ./..."))
	w.result("2026-11-24T10:00:02Z", "b1")
	w.call("2026-11-24T10:00:03Z", "m2", "r2", "claude-fable-5", 5, bash("b2", "cat > /tmp/note.txt <<'EOF'\nhello\nEOF"))
	w.result("2026-11-24T10:00:04Z", "b2")
	w.call("2026-11-24T10:00:05Z", "m3", "r3", "claude-fable-5", 5, bash("b3", "d=$(mktemp -d) && echo hi > $d/x.py && python3 $d/x.py"))
	w.call("2026-11-24T10:00:06Z", "m4", "r4", "claude-fable-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if k := whClass(r, "claude-fable-5", whShellEdit); k.Calls != 1 {
		t.Fatalf("shell edit: %+v in %+v", k, r.Activity)
	}
	if k := whClass(r, "claude-fable-5", whTests); k.Calls != 1 {
		t.Fatalf("tests: %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whScratch); k.Calls != 2 {
		t.Fatalf("scratch writes (/tmp and mktemp): %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whTextOnly); k.Calls != 1 {
		t.Fatalf("text only: %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whToolEdit); k.Calls != 0 {
		t.Fatalf("edit tool counted for shell edits: %+v", k)
	}
	// the first call's cost is split over shell edit and tests
	if a, b := whClass(r, "claude-fable-5", whShellEdit), whClass(r, "claude-fable-5", whTests); a.Cost != b.Cost {
		t.Fatalf("a compound call is split equally: %v vs %v", a.Cost, b.Cost)
	}
	trig := map[string]int{}
	for _, tr := range r.Triggers {
		trig[tr.Trigger] = tr.Calls
	}
	if trig["typed prompt"] != 1 || trig["tool result: Bash"] != 2 || trig["continuation"] != 1 {
		t.Fatalf("triggers %v", trig)
	}
}

func TestWherePhaseAroundShip(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "Handle WP-1 and ship")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "wp ship -h"))
	w.call("2026-11-24T10:00:02Z", "m2", "r2", "claude-fable-5", 5, bash("b2", "wp ship --review"))
	w.call("2026-11-24T10:00:03Z", "m3", "r3", "claude-fable-5", 5, bash("b3", "git add -A && wp ship"))
	w.call("2026-11-24T10:00:04Z", "m4", "r4", "claude-fable-5", 5)
	w.call("2026-11-24T10:00:05Z", "m5", "r5", "claude-fable-5", 5, bash("b5", "wp ship"))
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	var w2 whLines
	w2.user("2026-11-24T10:00:00Z", "never ships")
	w2.call("2026-11-24T10:00:01Z", "n1", "q1", "claude-fable-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s2.jsonl"), w2.b.String())
	r, _ := whRun(t, dir, whWin)
	ph := r.Phase
	if ph.Shipped != 1 || ph.Ships != 2 || ph.NoShipSession != 1 {
		t.Fatalf("phase %+v", ph)
	}
	one := r.Parts.total() / 6 // six calls, equal cost
	if d := ph.Before - 3*one; d < -1e-9 || d > 1e-9 {
		t.Fatalf("before %v, want three calls (%v each)", ph.Before, one)
	}
	if d := ph.After - 2*one; d < -1e-9 || d > 1e-9 {
		t.Fatalf("after %v, want two calls", ph.After)
	}
	if len(r.Top) == 0 || r.Top[0].Ships != 2 || !strings.HasPrefix(r.Top[0].Prompt, "Handle WP-1") {
		t.Fatalf("top %+v", r.Top)
	}
}

func TestWhereLimitEvent(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	w.raw(map[string]any{"type": "assistant", "timestamp": "2026-11-24T10:00:02Z", "isApiErrorMessage": true, "message": map[string]any{"id": "e1", "model": "<synthetic>",
		"content": []any{map[string]any{"type": "text", "text": "You've hit your weekly limit"}}}})
	w.call("2026-11-24T10:00:03Z", "m2", "r2", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:04Z", "m3", "r3", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if len(r.Limits.Events) != 1 || r.Limits.Events[0].TS != "2026-11-24T10:00:02Z" || !strings.Contains(r.Limits.Events[0].Text, "weekly limit") {
		t.Fatalf("limits %+v", r.Limits)
	}
	if d := r.Limits.SpendAfter - r.Cost*2/3; d < -1e-9 || d > 1e-9 {
		t.Fatalf("spend after %v of %v", r.Limits.SpendAfter, r.Cost)
	}
	if r.NotKnown.Synthetic != 0 {
		t.Fatalf("an error message is not a script record: %d", r.NotKnown.Synthetic)
	}
}

func TestWhereSubagentTypeFromMeta(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	var a whLines
	a.user("2026-11-24T10:00:02Z", "search the repo")
	a.call("2026-11-24T10:00:03Z", "a1", "r1", "claude-haiku-5-5", 5)
	a.call("2026-11-24T10:00:04Z", "a2", "r2", "claude-haiku-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-abc.jsonl"), a.b.String())
	whWrite(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-abc.meta.json"), `{"agentType":"probe"}`)
	r, _ := whRun(t, dir, whWin)
	got := map[string]int{}
	for _, ag := range r.Agents {
		got[ag.Agent] = ag.Calls
	}
	if got["main"] != 1 || got["probe"] != 2 || r.Sessions != 1 {
		t.Fatalf("agents %v sessions %d", got, r.Sessions)
	}
	trig := map[string]int{}
	for _, tr := range r.Triggers {
		trig[tr.Trigger] = tr.Calls
	}
	if trig["subagent task prompt"] != 1 {
		t.Fatalf("triggers %v", trig)
	}
}

func TestWhereWindowEdges(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T09:00:00Z", "go")
	w.call("2026-11-24T09:59:59Z", "m0", "r0", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:00Z", "m1", "r1", "claude-sonnet-5-5", 5) // from is inclusive
	w.call("2026-11-24T10:59:59Z", "m2", "r2", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T11:00:00Z", "m3", "r3", "claude-sonnet-5-5", 5) // until is exclusive
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, "--since", "2026-11-24T10:00:00Z", "--until", "2026-11-24T11:00:00Z")
	if r.Calls != 2 {
		t.Fatalf("calls %d, want 2", r.Calls)
	}
	// a file older than --since is not read
	old := filepath.Join(dir, "projects", "p", "s1.jsonl")
	if err := os.Chtimes(old, fixedTime(2020), fixedTime(2020)); err != nil {
		t.Fatal(err)
	}
	r, _ = whRun(t, dir, "--since", "2026-11-24T10:00:00Z", "--until", "2026-11-24T11:00:00Z")
	if r.Calls != 0 {
		t.Fatalf("an old file was read: %d calls", r.Calls)
	}
	var out bytes.Buffer
	c := &cli{out: &out, dir: t.TempDir()}
	if err := cmdUsageWhere(c, []string{"--dir", dir, "--since", "2026-11-24T11:00:00Z", "--until", "2026-11-24T10:00:00Z"}); err == nil {
		t.Fatal("a window that ends before it starts was accepted")
	}
}

func TestWhereJSONShapeAndCSV(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "hello\nthere")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, map[string]any{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]any{"file_path": "/work/proj/a.go"}},
		map[string]any{"type": "tool_use", "id": "e1", "name": "Edit", "input": map[string]any{"file_path": "/work/proj/a.go"}})
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	csvPath := filepath.Join(t.TempDir(), "calls.csv")
	r, raw := whRun(t, dir, whWin, "--csv", csvPath)
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"models", "agents", "projects", "launch_kinds", "premium_main_activity", "triggers", "phase", "top_sessions", "limit_events", "not_counted_or_not_known", "cost_parts"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("json lacks %q", k)
		}
	}
	if len(r.Top) != 1 || r.Top[0].Prompt != "hello there" || r.Models[0].MedianCtx != 1210 {
		t.Fatalf("top %+v models %+v", r.Top, r.Models)
	}
	f, err := os.Open(csvPath) // #nosec G304 -- a test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv %v rows %v", err, rows)
	}
	if strings.Join(rows[0], ",") != "session,file_kind,ts,model,trigger,tool_classes,context_tokens,cost_input,cost_output,cost_cache_write_5m,cost_cache_write_1h,cost_cache_read,cost_total" {
		t.Fatalf("header %v", rows[0])
	}
	row := rows[1]
	if row[0] != "s1" || row[1] != "main" || row[2] != "2026-11-24T10:00:01Z" || row[3] != "claude-fable-5" || row[4] != "typed prompt" || row[5] != "edit tool+read file" || row[6] != "1210" {
		t.Fatalf("row %v", row)
	}
	var sum float64
	for _, s := range row[7:12] {
		var x float64
		_, _ = fmt.Sscan(s, &x)
		sum += x
	}
	var total float64
	_, _ = fmt.Sscan(row[12], &total)
	if d := sum - total; d < -1e-5 || d > 1e-5 {
		t.Fatalf("parts %v do not add up to %v", sum, total)
	}
}

func fixedTime(year int) time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }
