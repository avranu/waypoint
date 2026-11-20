package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cmpTranscript writes transcript lines for the compaction tests.
type cmpTranscript struct {
	lines []string
	at    time.Time
	n     int
}

func (c *cmpTranscript) add(after time.Duration, line map[string]any) {
	c.at = c.at.Add(after)
	line["timestamp"] = c.at.UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(line)
	c.lines = append(c.lines, string(b))
}

func (c *cmpTranscript) prompt(text string) {
	c.add(time.Second, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// call writes an assistant call with one tool call and its result.
func (c *cmpTranscript) call(written, read int64, tool string, input map[string]any, result string, failed bool) {
	c.n++
	id, tid := fmt.Sprint("msg_", c.n), fmt.Sprint("toolu_", c.n)
	usage := map[string]any{"input_tokens": 10, "output_tokens": 100, "cache_creation_input_tokens": written, "cache_read_input_tokens": read,
		"cache_creation": map[string]any{"ephemeral_1h_input_tokens": written}}
	msg := func(block map[string]any) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": "claude-opus-5-5", "usage": usage, "content": []any{block}}}
	}
	// Claude Code writes a line per content block, each with the call's usage
	c.add(time.Second, msg(map[string]any{"type": "text", "text": "on it"}))
	use := msg(map[string]any{"type": "tool_use", "id": tid, "name": tool, "input": input})
	use["uuid"] = "line_" + tid
	c.add(0, use)
	c.add(time.Second, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": tid, "is_error": failed, "content": result}}}})
}

func (c *cmpTranscript) compact(after time.Duration, trigger string, pre int64, took time.Duration, summary string) {
	c.add(after, map[string]any{"type": "system", "subtype": "compact_boundary", "compactMetadata": map[string]any{"trigger": trigger, "preTokens": pre, "durationMs": took.Milliseconds()}})
	c.add(0, map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": summary}})
	c.add(0, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "<command-name>/compact</command-name>"}})
}

func (c *cmpTranscript) write(t *testing.T, root, session string) {
	t.Helper()
	dir := filepath.Join(root, "projects", "-r")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(strings.Join(c.lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompactReport(t *testing.T) {
	root := t.TempDir()
	s := &cmpTranscript{at: time.Now().Add(-3 * time.Hour)}
	s.prompt("fix the bug")
	s.call(20000, 0, "Read", map[string]any{"file_path": "/r/a.go"}, strings.Repeat("a", 3600), false)
	s.call(1000, 20000, "Bash", map[string]any{"command": "cd /r && cat internal/b.go"}, strings.Repeat("b", 3600), false)
	s.call(1000, 99000, "Edit", map[string]any{"file_path": "/r/a.go"}, "ok", false)
	// ten minutes idle, then a manual compaction that took 30 s
	s.compact(10*time.Minute+30*time.Second, "manual", 100000, 30*time.Second, strings.Repeat("s", 7200))
	s.prompt("now the next part")
	s.call(30000, 20000, "Bash", map[string]any{"command": "sed -n 1,5p internal/b.go"}, strings.Repeat("b", 3600), false)
	s.call(500, 50000, "Read", map[string]any{"file_path": "/r/c.go"}, "no such file", true)
	s.call(500, 50500, "Bash", map[string]any{"command": "sed -i s/x/y/ a.go"}, "", false)
	// an automatic one nothing followed
	s.compact(time.Second, "auto", 60000, 20*time.Second, strings.Repeat("s", 3600))
	s.write(t, root, "one")

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Transcripts != 1 || rep.Sessions != 1 || rep.Compacted != 1 || len(rep.Compactions) != 2 {
		t.Fatalf("report: %+v", rep)
	}
	auto, manual := rep.Compactions[0], rep.Compactions[1]
	if auto.N != 1 || auto.NeverUsed != 1 || auto.Before != 60000 || auto.SummaryTokens != 1000 {
		t.Errorf("auto: %+v", auto)
	}
	if manual.N != 1 || manual.NeverUsed != 0 || manual.Cold != 0 || manual.Before != 100000 || manual.After != 50010 ||
		manual.Seconds != 30 || manual.SummaryTokens != 2000 || manual.CallsAfter != 3 {
		t.Errorf("manual: %+v", manual)
	}
	if manual.First.N != 1 || manual.First.Written != 30000 || manual.First.Read != 20000 {
		t.Errorf("first call after: %+v", manual.First)
	}
	// Opus 5.5: read $0.2, 1h write $8, output $20 per million
	for name, got := range map[string][2]float64{
		"read":        {manual.ReadUSD, 100000 * 0.2 / 1e6},
		"summary":     {manual.SummaryUSD, 2000 * 1.35 * 20 / 1e6},
		"rewrite":     {manual.RewriteUSD, 30000 * 8 / 1e6},
		"later reads": {manual.LaterReadsUSD, (100010 - 50010) * 3 * 0.2 / 1e6},
		"cold":        {manual.ColdStartUSD, 0},
		"auto unused": {auto.UnusedUSD, 60000*0.2/1e6 + 1000*1.35*20/1e6},
	} {
		if math.Abs(got[0]-got[1]) > 1e-9 {
			t.Errorf("%s: $%.6f, want $%.6f", name, got[0], got[1])
		}
	}
	if len(rep.Idle) != 2 || rep.Idle[1].Trigger != "manual" || rep.Idle[1].Idle != "5-45m" || rep.Idle[1].N != 1 {
		t.Errorf("idle: %+v", rep.Idle)
	}
	f := rep.Follows[cmpAfterManual]
	if f.N != 1 || f.Calls != 3 || f.Lookups != 2 || f.FileLookups != 2 || f.SeenBefore != 1 || f.TokensAgain != 1000 ||
		f.Tools != 3 || f.Failed != 1 || f.ReachedEdit != 1 || f.CallsToEdit != 3 || f.LookupsToEdit != 2 || f.OutputPerCall != 100 {
		t.Errorf("after manual: %+v", f)
	}
	if n := rep.Follows[cmpAfterAuto].N + rep.Follows[cmpBeforeManual].N + rep.Follows[cmpStart].N; n != 0 {
		t.Errorf("windows that should be empty: %+v", rep.Follows)
	}
	text := compactText(rep)
	for _, want := range []string{"2 compactions in 1 of 1 sessions", "manual     1  100k", "after a manual compaction", "1 (50.0%)"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestCompactColdAndControls(t *testing.T) {
	root := t.TempDir()
	s := &cmpTranscript{at: time.Now().Add(-6 * time.Hour)}
	s.prompt("start")
	for i := range 70 {
		s.call(500, 40000, "Read", map[string]any{"file_path": fmt.Sprintf("/r/f%d.go", i%35)}, strings.Repeat("x", 360), false)
	}
	s.prompt("carry on")
	s.call(500, 95000, "Read", map[string]any{"file_path": "/r/f1.go"}, strings.Repeat("x", 360), false)
	// the cache died before this compaction began, and before the next call
	s.compact(2*time.Hour, "manual", 120000, 10*time.Second, strings.Repeat("s", 3600))
	s.call(30000, 0, "Grep", map[string]any{"pattern": "x"}, "", false)
	s.write(t, root, "two")

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	manual := rep.Compactions[1]
	if manual.Cold != 1 || manual.First.NoRead != 1 {
		t.Errorf("manual: %+v", manual)
	}
	// cold: the history at the input price ($4); the restart it spared at the write price ($8)
	if want := 120000 * 4 / 1e6; math.Abs(manual.ReadUSD-want) > 1e-9 {
		t.Errorf("cold read $%.4f, want $%.4f", manual.ReadUSD, want)
	}
	if want := 120000 * 8 / 1e6; math.Abs(manual.ColdStartUSD-want) > 1e-9 {
		t.Errorf("cold restart $%.4f, want $%.4f", manual.ColdStartUSD, want)
	}
	if len(rep.Idle) != 1 || rep.Idle[0].Idle != "over 60m" || rep.Idle[0].Late != 1 {
		t.Errorf("idle: %+v", rep.Idle)
	}
	// the 30 calls before it: 29 files for the second time, then f1 again
	before := rep.Follows[cmpBeforeManual]
	if before.N != 1 || before.Calls != 30 || before.FileLookups != 30 || before.SeenBefore != 30 {
		t.Errorf("before manual: %+v", before)
	}
	prompt, big := rep.Follows[cmpPrompt], rep.Follows[cmpPromptBig]
	if prompt.N != 1 || prompt.Calls != 1 || prompt.SeenBefore != 1 || prompt.TokensAgain != 100 || big.N != 0 {
		t.Errorf("prompt: %+v big: %+v", prompt, big)
	}
	start := rep.Follows[cmpStart]
	if rep.Start.N != 1 || rep.Start.Written != 500 || start.N != 1 || start.Calls != 30 || start.SeenBefore != 0 {
		t.Errorf("start: %+v %+v", rep.Start, start)
	}
}

func TestCompactToolKinds(t *testing.T) {
	for _, c := range []struct {
		cmd                  string
		edit, lookup, orient bool
		paths                string
	}{
		{"cd /r && git status", false, true, true, ""},
		{"head -20 cmd/wp/main.go", false, true, false, "main.go"},
		{"cat a/b.go c/d.md | wc -l", false, true, false, "b.go d.md"},
		{"cat main.go", false, true, false, "main.go"},
		{"cd /r && sed -n 1,5p x.go y_test.go", false, true, false, "x.go y_test.go"},
		{"cat a.go | grep foo.bar", false, true, false, "a.go"},
		{"head -c 1.5k ~/notes/log.txt", false, true, false, "log.txt"},
		{"grep -rn foo internal/x.go", false, true, false, ""},
		{"sed -i s/a/b/ cmd/x.go", true, false, false, ""},
		{"go test ./...", false, false, false, ""},
		{"pwd", false, true, true, ""},
		// output sent to a file writes it; a here-document's text is no file read
		{"cat > /r/notes/a.go <<'EOF'\npackage x // see foo.bar\nEOF", true, false, false, "a.go"},
		{"cat <<'EOF' >> notes.md\nsee main.go\nEOF", true, false, false, "notes.md"},
		{"cat <<EOF | wp comment WP-1 -f -\nsee main.go\nEOF", false, false, false, ""},
		{"git diff > /tmp/x.patch", true, false, false, "x.patch"},
		{"cat main.go &> out.txt", true, false, false, "out.txt"},
		{"cat main.go &>> out.txt", true, false, false, "out.txt"},
		{"cat a.go > \"$OUT\"", true, false, false, ""},
		{"cat a.go 2>/dev/null", false, true, false, "a.go"},
		{"cat a.go 2>&1 | head -3", false, true, false, "a.go"},
		{"cat a.go > /dev/null", false, true, false, "a.go"},
		{"sed -n '/a > b/p' x.go", false, true, false, "x.go"},
		{"git log --format='%an <%ae>' -3", false, true, true, ""},
		{"wp show WP-1", false, true, true, ""},
		{"wp pr merge 4 --when-ready", false, false, false, ""},
	} {
		in, _ := json.Marshal(map[string]string{"command": c.cmd})
		e := cmpTool("Bash", in)
		if e.edit != c.edit || e.lookup != c.lookup || e.orient != c.orient || strings.Join(e.paths, " ") != c.paths {
			t.Errorf("%q: edit %v lookup %v orient %v paths %v", c.cmd, e.edit, e.lookup, e.orient, e.paths)
		}
	}
}

func (c *cmpTranscript) attach(kind, file string) {
	c.add(0, map[string]any{"type": "attachment", "attachment": map[string]any{"type": kind, "filename": file}})
}

func TestCompactCarryAndReviewRounds(t *testing.T) {
	root := t.TempDir()
	s := &cmpTranscript{at: time.Now().Add(-3 * time.Hour)}
	s.prompt("start")
	s.call(20000, 0, "Read", map[string]any{"file_path": "/r/a.go"}, strings.Repeat("a", 3600), false)
	s.call(1000, 20000, "Bash", map[string]any{"command": "cat internal/b.go"}, strings.Repeat("b", 7200), false)
	s.call(1000, 21000, "Bash", map[string]any{"command": "git log --oneline -5"}, "abc", false)
	s.call(1000, 22000, "Edit", map[string]any{"file_path": "/r/d.go"}, "ok", false)
	s.compact(time.Minute, "manual", 100000, 10*time.Second, strings.Repeat("s", 3600))
	s.attach("file", "/r/a.go")
	s.attach("compact_file_reference", "/r/internal/b.go")
	s.prompt(cmpReviewPrompt + "WP-1: a finding in d.go and one in never.go")
	s.call(30000, 20000, "Bash", map[string]any{"command": "sed -n 1,5p internal/b.go"}, strings.Repeat("b", 3600), false)
	s.call(500, 50000, "Bash", map[string]any{"command": "cd /r && git log --oneline -5"}, "abc", false)
	s.call(500, 50500, "Read", map[string]any{"file_path": "/home/u/.claude/projects/-r/one.jsonl"}, "…", false)
	s.call(500, 51000, "Read", map[string]any{"file_path": "/r/d.go"}, strings.Repeat("d", 1800), false)
	s.call(500, 51500, "Edit", map[string]any{"file_path": "/r/d.go"}, "ok", false)
	s.call(500, 52000, "Agent", map[string]any{"prompt": "look"}, "done", false)
	s.prompt(cmpReviewPrompt + "WP-1: once more")
	s.call(500, 52500, "Read", map[string]any{"file_path": "/r/a.go"}, strings.Repeat("a", 3600), false)
	s.write(t, root, "one")

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	manual := rep.Compactions[1]
	c := manual.Carry
	// b.go (shown through the shell, only named by Claude Code), d.go (edited
	// before, read and edited again) and a.go (restored, read again anyway)
	if c.Windows != 1 || c.Files != 3 || c.Restored != 1 || c.Referenced != 1 || c.ShellOnly != 1 || c.ThenEdited != 1 || c.Tokens != 2500 {
		t.Errorf("looked up again: %+v", c)
	}
	for set, want := range map[string][2]int{ // files carried, files covered
		"restored": {1, 1}, "restored_or_named": {2, 2}, "last_5": {3, 3}, "edited": {1, 1}, "prompt_names": {1, 1}, "edited_or_prompt_names": {1, 1},
	} {
		found := false
		for _, s := range c.Sets {
			if s.Set == set {
				found = true
				if int(s.Files) != want[0] || s.Covered != want[1] {
					t.Errorf("set %s: %+v, want %v", set, s, want)
				}
			}
		}
		if !found {
			t.Errorf("no set %s in %+v", set, c.Sets)
		}
	}
	// what a summariser of ours would read: the four calls before it, the
	// tool results in full and cut
	if manual.HistoryTokens < 3000 || manual.HistoryCut >= manual.HistoryTokens || manual.HistoryCut == 0 {
		t.Errorf("history: %d, cut %d", manual.HistoryTokens, manual.HistoryCut)
	}
	f := rep.Follows[cmpAfterManual]
	if f.N != 1 || f.Calls != 7 || f.AskedAgain != 1 || f.OwnTranscript != 1 || f.Subagents != 1 {
		t.Errorf("after manual: %+v", f)
	}
	// seven calls of 100 output tokens at $20 a million; the first wrote 30,000 at $8
	if want := 7 * 100 * 20 / 1e6; math.Abs(f.USDOut-want) > 1e-9 {
		t.Errorf("output $%.6f, want $%.6f", f.USDOut, want)
	}
	if want := (30000 + 6*500) * 8 / 1e6; math.Abs(f.USDWrite-want) > 1e-9 {
		t.Errorf("writes $%.6f, want $%.6f", f.USDWrite, want)
	}
	if math.Abs(f.USD-(f.USDWrite+f.USDRead+f.USDOut+7*10*4/1e6)) > 1e-9 {
		t.Errorf("dollars %.6f do not add up: %+v", f.USD, f)
	}
	compacted, plain := rep.Reviews[0], rep.Reviews[1]
	if compacted.N != 1 || compacted.ContextBefore != 100000 || compacted.Another != 1 || compacted.Calls != 7 || compacted.CallUSD <= 0 {
		t.Errorf("review round, compacted: %+v", compacted)
	}
	if plain.N != 1 || plain.Another != 0 || plain.Calls != 1 || plain.ContextBefore != 52510 || rep.Reviews[2].N != 0 {
		t.Errorf("review round, not compacted: %+v", plain)
	}
	text := compactText(rep)
	for _, want := range []string{"files looked up again", "the files the next prompt names", "compacted just before", "not compacted, under 90k", "a summariser of ours would read"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

// The adversarial reviews a session started are counted with and without
// a compaction before them.
func TestCompactAdversarialReviews(t *testing.T) {
	root := t.TempDir()
	read := func(s *cmpTranscript, n int) {
		for i := range n {
			s.call(500, 20000, "Read", map[string]any{"file_path": fmt.Sprintf("/r/f%d.go", i)}, "text", false)
		}
	}
	adversarial := map[string]any{"subagent_type": "adversarial-review", "prompt": "break it"}

	// An adversarial review far from any compaction, then one just after it.
	s := &cmpTranscript{at: time.Now().Add(-5 * time.Hour)}
	s.prompt("start")
	read(s, 2)
	s.call(500, 20000, "Agent", adversarial, "nothing found", false)
	s.call(500, 20000, "Agent", adversarial, "again: too soon to count", false)
	s.call(500, 20000, "Agent", map[string]any{"subagent_type": "Explore", "prompt": "find it"}, "found", false)
	read(s, 30)
	s.compact(time.Minute, "manual", 100000, 10*time.Second, "summary")
	s.prompt(cmpReviewPrompt + "WP-1: a finding")
	read(s, 2)
	s.call(500, 20000, "Agent", adversarial, "one real", false)
	read(s, 3)
	s.write(t, root, "one")

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Reviews) != 3 || rep.Reviews[0].N != 1 || rep.Reviews[0].ContextBefore != 100000 {
		t.Errorf("review rows: %+v", rep.Reviews)
	}
	if len(rep.Signals) != 3 {
		t.Fatalf("adversarial review rows: %+v", rep.Signals)
	}
	near, far := rep.Signals[0], rep.Signals[1]
	if near.N != 1 || near.Point != "compacted" || near.Calls != 3 || near.Another != 0 {
		t.Errorf("an adversarial review just after a compaction: %+v", near)
	}
	// the one far from a compaction: its window is the 30 calls after it,
	// and another followed
	if far.N != 1 || far.Calls != 30 || far.Another != 1 || far.ContextBefore != 20510 || rep.Signals[2].N != 0 {
		t.Errorf("an adversarial review with no compaction near: %+v\nbig: %+v", far, rep.Signals[2])
	}
	text := compactText(rep)
	for _, want := range []string{"after an adversarial review was started, 30 calls", "compacted in the 5 calls before", "no compaction near, under 90k"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"adversarial_reviews":[`) {
		t.Error("the JSON lacks adversarial_reviews")
	}
}

// compactKeeping writes a compaction whose last messages Claude Code left in
// place: the tool calls of those lines.
func (c *cmpTranscript) compactKeeping(trigger string, pre int64, summary string, kept ...string) {
	c.add(time.Second, map[string]any{"type": "system", "subtype": "compact_boundary", "compactMetadata": map[string]any{
		"trigger": trigger, "preTokens": pre, "durationMs": 1000, "preservedMessages": map[string]any{"uuids": kept}}})
	c.add(0, map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": summary}})
}

// The second adversarial review's findings (WP-1097): a background task's
// report is no typed prompt, a window cut short is not held beside the 30
// calls before, a file still in the messages Claude Code kept needs no carry.
func TestCompactNoticesFullWindowsAndKept(t *testing.T) {
	root := t.TempDir()
	s := &cmpTranscript{at: time.Now().Add(-5 * time.Hour)}
	s.prompt("start")
	for i := range 25 {
		s.call(500, 40000, "Read", map[string]any{"file_path": fmt.Sprintf("/r/f%d.go", i)}, strings.Repeat("x", 360), false)
	}
	s.prompt("<task-notification><task-id>b1</task-id><status>completed</status></task-notification>")
	s.call(500, 40000, "Read", map[string]any{"file_path": "/r/f1.go"}, strings.Repeat("x", 360), false)
	s.prompt(keepWarmMark + ": reply ok")
	s.call(500, 40000, "Read", map[string]any{"file_path": "/r/f2.go"}, strings.Repeat("x", 360), false)
	s.prompt("[Request interrupted by user]")
	s.prompt("now the other thing")
	s.call(500, 40000, "Read", map[string]any{"file_path": "/r/f3.go"}, strings.Repeat("x", 360), false)
	s.call(500, 40000, "Bash", map[string]any{"command": "cat internal/kept.go"}, strings.Repeat("k", 360), false)
	kept := fmt.Sprint("line_toolu_", s.n)
	// automatic, its last tool call kept, then 32 calls: a full window
	s.compactKeeping("auto", 150000, strings.Repeat("s", 3600), kept)
	s.call(30000, 20000, "Bash", map[string]any{"command": "cat internal/kept.go"}, strings.Repeat("k", 360), false)
	s.call(500, 50000, "Read", map[string]any{"file_path": "/r/f3.go"}, strings.Repeat("x", 360), false)
	for range 30 {
		s.call(500, 50000, "Grep", map[string]any{"pattern": "x"}, "", false)
	}
	// manual, then three calls: a window cut short
	s.compactKeeping("manual", 100000, strings.Repeat("s", 3600))
	for range 3 {
		s.call(500, 50000, "Grep", map[string]any{"pattern": "y"}, "", false)
	}
	s.write(t, root, "three")

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	// the typed prompt's window runs on to the compaction; the report's too
	prompt, notice := rep.Follows[cmpPrompt], rep.Follows[cmpNotice]
	if prompt.N != 1 || prompt.Calls != 2 || notice.N != 1 || notice.Calls != 4 || notice.SeenBefore != 3 {
		t.Errorf("typed prompt: %+v\nreport: %+v", prompt, notice)
	}
	auto, autoFull := rep.Follows[cmpAfterAuto], rep.Follows[cmpAfterAutoFull]
	if auto.N != 1 || autoFull.N != 1 || autoFull.Calls != 30 || autoFull.SeenBefore != 2 {
		t.Errorf("after automatic: %+v\nfull: %+v", auto, autoFull)
	}
	manual, manualFull := rep.Follows[cmpAfterManual], rep.Follows[cmpAfterManualFull]
	if manual.N != 1 || manual.Calls != 3 || manualFull.N != 0 {
		t.Errorf("after manual: %+v\nfull: %+v", manual, manualFull)
	}
	// kept.go and f3.go were looked up again; kept.go was still in context,
	// so no set holds it and the last five cover f3.go alone
	c := rep.Compactions[0].Carry
	if c.Files != 2 || c.Kept != 1 {
		t.Errorf("looked up again: %+v", c)
	}
	found := false
	for _, set := range c.Sets {
		if set.Set != "last_5" {
			continue
		}
		found = true
		if set.Files != 4 || set.Covered != 1 {
			t.Errorf("last_5: %+v", set)
		}
	}
	if !found {
		t.Errorf("no set last_5 in %+v", c.Sets)
	}
	text := compactText(rep)
	for _, want := range []string{"those with all 30 calls", "after a background task's report", "had kept in its last messages"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

// A call's cache write is priced by the cache it went to; a message logged
// twice counts once; a transcript that cannot be read is reported.
func TestCompactPricesDuplicatesAndUnreadable(t *testing.T) {
	root := t.TempDir()
	s := &cmpTranscript{at: time.Now().Add(-2 * time.Hour)}
	s.prompt("start")
	s.call(1000, 0, "Read", map[string]any{"file_path": "/r/a.go"}, "a", false) // the one-hour cache
	s.compactKeeping("manual", 50000, "sum")
	// one call, logged twice, writing a million tokens to the five-minute cache
	line := map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_five", "model": "claude-opus-5-5",
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 1000000, "cache_read_input_tokens": 0,
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 1000000, "ephemeral_1h_input_tokens": 0}},
		"content": []any{map[string]any{"type": "text", "text": "reading a.go"}, map[string]any{"type": "tool_use", "id": "toolu_five", "name": "Read", "input": map[string]any{"file_path": "/r/a.go"}}}}}
	s.add(time.Second, line)
	s.add(0, line)
	s.write(t, root, "four")
	if os.Getuid() != 0 {
		bad := filepath.Join(root, "projects", "-r", "locked.jsonl")
		if err := os.WriteFile(bad, []byte("{}\n"), 0o000); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := buildCompactReport(root, 24*time.Hour, burnFallbackPrices)
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Follows[cmpAfterManual]
	if f.N != 1 || f.Calls != 1 || f.Tools != 1 || f.FileLookups != 1 {
		t.Errorf("after manual: %+v", f)
	}
	// $5 a million for the five-minute cache, not the hour's $8
	if math.Abs(f.USDWrite-5) > 1e-9 || math.Abs(rep.Compactions[1].RewriteUSD-5) > 1e-9 {
		t.Errorf("write $%.4f, rewrite $%.4f, want $5", f.USDWrite, rep.Compactions[1].RewriteUSD)
	}
	// its text, too, counts once
	ev, err := readCompactFile(filepath.Join(root, "projects", "-r", "four.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if c := ev[lastOfKind(ev, "c")]; c.chars != len("reading a.go") {
		t.Errorf("the twice-logged call wrote %d characters, want %d", c.chars, len("reading a.go"))
	}
	if os.Getuid() != 0 {
		if rep.Unreadable != 1 || !strings.Contains(compactText(rep), "1 transcripts could not be read") {
			t.Errorf("unreadable %d:\n%s", rep.Unreadable, compactText(rep))
		}
	}
}

// A transcript whose read fails partway is unreadable, not a shorter session
// (c5204): a directory opens on Linux and fails at the first read.
func TestCompactReadErrorIsNotTheEnd(t *testing.T) {
	ev, err := readCompactFile(t.TempDir())
	if err == nil || ev != nil {
		t.Errorf("a failed read: %d events, error %v", len(ev), err)
	}
}
