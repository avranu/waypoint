package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func j(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type m = map[string]any

func ts(sec int) string {
	return time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second).Format("2006-01-02T15:04:05.000Z")
}

func assistant(id string, sec int, usage m, content ...m) string {
	r := m{"type": "assistant", "uuid": "u-" + id + fmt.Sprint(sec), "timestamp": ts(sec), "requestId": "req-" + id,
		"message": m{"id": id, "model": "model-a", "content": content, "usage": usage}}
	return j(r)
}

func toolUse(id, name string, input m) m {
	return m{"type": "tool_use", "id": id, "name": name, "input": input}
}

func result(id string, sec int, isErr bool, text string, tur any) string {
	r := m{"type": "user", "uuid": "r-" + id, "timestamp": ts(sec),
		"message": m{"content": []m{{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": text}}}}
	if tur != nil {
		r["toolUseResult"] = tur
	}
	return j(r)
}

func parse(t *testing.T, file string, lines ...string) *Result {
	t.Helper()
	r, err := Parse(strings.NewReader(strings.Join(lines, "\n")+"\n"), file)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestModelCallDedupedByMessageAndUsageFromLastLine(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"input_tokens": 3, "output_tokens": 1}, m{"type": "thinking"}),
		assistant("m1", 2, m{"input_tokens": 3, "output_tokens": 40, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 10,
			"output_tokens_details": m{"thinking_tokens": 25}}, m{"type": "text", "text": "x"}),
		assistant("m2", 3, m{"input_tokens": 1, "output_tokens": 2}),
	)
	if len(r.ModelCalls) != 2 {
		t.Fatalf("calls = %d, want 2 (a streamed message is one call)", len(r.ModelCalls))
	}
	c := r.ModelCalls[0]
	if *c.Output != 40 || *c.Thinking != 25 || *c.CacheRead != 100 || *c.Context != 113 {
		t.Errorf("usage not from the last line: %+v", c)
	}
	if *c.At != *parseTS(ts(1)) || *c.EndAt != *parseTS(ts(2)) {
		t.Errorf("at/end_at = %v/%v", *c.At, *c.EndAt)
	}
}

func TestMissingFieldsAreNil(t *testing.T) {
	r := parse(t, "main", assistant("m1", 1, m{"output_tokens": 5}, toolUse("t1", "Read", m{"file_path": "/a/b.md"})))
	c := r.ModelCalls[0]
	if c.Input != nil || c.CacheRead != nil || c.Thinking != nil || c.CacheW5m != nil || c.Context != nil || c.ColdInferred != nil || c.GapMS != nil || c.StopReason != nil {
		t.Errorf("a missing value must be nil, not 0: %+v", c)
	}
	tc := r.ToolCalls[0]
	if tc.ResultAt != nil || tc.IsError != nil || tc.ExitCode != nil || tc.LinesAdded != nil || tc.ReadOffset != nil || tc.IsTest != nil {
		t.Errorf("a call with no result has nil result fields: %+v", tc)
	}
	r = parse(t, "main", j(m{"type": "assistant", "message": m{"id": "x", "model": "model-a"}}))
	if r.ModelCalls[0].At != nil {
		t.Errorf("a record with no time has a nil At")
	}
}

func TestUnknownRecordsCountedByType(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "brand-new-thing", "timestamp": ts(1)}),
		j(m{"type": "brand-new-thing"}),
		j(m{"type": "system", "subtype": "new_sub"}),
		j(m{"type": "attachment", "attachment": m{"type": "new_attach"}}),
		j(m{"type": "file-history-snapshot"}),
		"not json",
		j(m{"nothing": 1}),
	)
	want := map[string]int{"brand-new-thing": 2, "system/new_sub": 1, "attachment/new_attach": 1, "(unreadable)": 1, "(no type)": 1}
	for k, n := range want {
		if r.Unknown[k] != n {
			t.Errorf("Unknown[%q] = %d, want %d (all: %v)", k, r.Unknown[k], n, r.Unknown)
		}
	}
	if r.UnknownTotal() != 6 || r.Ignored["file-history-snapshot"] != 1 {
		t.Errorf("total %d ignored %v", r.UnknownTotal(), r.Ignored)
	}
	if r.Lines != 7 {
		t.Errorf("lines = %d", r.Lines)
	}
}

func TestSubAgentAttribution(t *testing.T) {
	sub := m{"type": "assistant", "isSidechain": true, "agentId": "ab12", "timestamp": ts(5),
		"message": m{"id": "sm1", "model": "model-b", "content": []m{toolUse("st1", "Bash", m{"command": "ls -la"})}, "usage": m{"output_tokens": 3}}}
	r := parse(t, "subagents/agent-ab12.jsonl", j(sub))
	c := r.ModelCalls[0]
	if c.Thread != "ab12" || !c.Sidechain || r.ToolCalls[0].Thread != "ab12" {
		t.Errorf("sub-agent rows must carry its thread: %+v", c)
	}
	r = parse(t, "subagents/agent-cd34.jsonl", assistant("sm2", 6, m{"output_tokens": 1}))
	if r.ModelCalls[0].Thread != "cd34" || !r.ModelCalls[0].Sidechain {
		t.Errorf("thread from file name: %+v", r.ModelCalls[0])
	}
	if len(r.Settings) != 0 {
		t.Errorf("a sub-agent's model is not the session's setting: %+v", r.Settings)
	}
	r = parse(t, "main", assistant("m1", 1, m{"output_tokens": 1}))
	if r.ModelCalls[0].Thread != MainThread || r.ModelCalls[0].Sidechain {
		t.Errorf("main thread: %+v", r.ModelCalls[0])
	}
}

func TestSpawnLinksResult(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("a1", "Agent", m{"subagent_type": "probe", "description": "look around", "run_in_background": true})),
		result("a1", 9, false, "done", m{"status": "completed", "agentId": "ab12", "resolvedModel": "model-b", "totalDurationMs": 8000, "totalTokens": 1234}),
	)
	if len(r.Spawns) != 1 {
		t.Fatalf("spawns = %d", len(r.Spawns))
	}
	s := r.Spawns[0]
	if *s.AgentType != "probe" || *s.AgentID != "ab12" || *s.Tokens != 1234 || *s.DurationMS != 8000 || !*s.Background || *s.Status != "completed" {
		t.Errorf("spawn = %+v", s)
	}
}

func TestCompactionAndTurns(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "user", "uuid": "p1", "timestamp": ts(1), "promptId": "pr1", "turnOrigin": "human", "message": m{"content": "hello there"},
			"turnPosition": m{"promptIndex": 0, "turnIndex": 0}}),
		assistant("m1", 2, m{"output_tokens": 1}),
		j(m{"type": "system", "subtype": "turn_duration", "timestamp": ts(3), "durationMs": 2000, "messageCount": 4}),
		j(m{"type": "system", "subtype": "compact_boundary", "uuid": "c1", "timestamp": ts(4),
			"compactMetadata": m{"trigger": "auto", "preTokens": 900, "postTokens": 100}}),
		j(m{"type": "user", "uuid": "p2", "timestamp": ts(5), "isCompactSummary": true, "message": m{"content": "summary"}}),
		j(m{"type": "user", "uuid": "p3", "timestamp": ts(6), "turnOrigin": "task_notification", "message": m{"content": "done"}}),
		j(m{"type": "user", "uuid": "p4", "timestamp": ts(7), "message": m{"content": "no origin recorded"}}),
	)
	if len(r.Turns) != 3 {
		t.Fatalf("turns = %d, want 3 (a compact summary is not a turn)", len(r.Turns))
	}
	t0 := r.Turns[0]
	if *t0.Origin != "human" || *t0.DurationMS != 2000 || *t0.Messages != 4 || *t0.PromptBytes != 11 || *t0.PromptID != "pr1" {
		t.Errorf("turn 0 = %+v", t0)
	}
	if *r.Turns[1].Origin != "task_notification" || r.Turns[2].Origin != nil {
		t.Errorf("origins: %v %v (a turn whose record says nothing is nil)", r.Turns[1].Origin, r.Turns[2].Origin)
	}
	if len(r.Compactions) != 1 {
		t.Fatalf("compactions = %d", len(r.Compactions))
	}
	c := r.Compactions[0]
	if *c.Trigger != "auto" || *c.PreTokens != 900 || *c.PostTokens != 100 || c.Dropped != nil {
		t.Errorf("compaction = %+v", c)
	}
	if got := *r.ModelCalls[0].TurnUID; got != "p1" {
		t.Errorf("model call turn = %q", got)
	}
}

func TestEditLineDeltas(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1},
			toolUse("e1", "Edit", m{"file_path": "/r/a.go"}),
			toolUse("e2", "Write", m{"file_path": "/r/a_test.go"}),
			toolUse("e3", "Edit", m{"file_path": "/r/b.go"})),
		result("e1", 2, false, "ok", m{"type": "update", "filePath": "/r/a.go",
			"structuredPatch": []m{{"lines": []string{" ctx", "+one", "+two", "-gone", " ctx", "+three"}}}}),
		result("e2", 3, false, "ok", m{"type": "create", "filePath": "/r/a_test.go", "content": "a\nb\nc\n"}),
		result("e3", 4, true, "Exit code 2\nboom", nil),
	)
	by := map[string]ToolCall{}
	for _, c := range r.ToolCalls {
		by[c.ToolUseID] = c
	}
	if e := by["e1"]; *e.LinesAdded != 3 || *e.LinesRemoved != 1 || *e.IsTest {
		t.Errorf("e1 = +%v -%v test %v", *e.LinesAdded, *e.LinesRemoved, *e.IsTest)
	}
	if e := by["e2"]; *e.LinesAdded != 3 || *e.LinesRemoved != 0 || !*e.IsTest {
		t.Errorf("created file: lines are the content's, and it is a test: %+v", e)
	}
	e := by["e3"]
	if e.LinesAdded != nil || !*e.IsError || *e.ExitCode != 2 {
		t.Errorf("a failed edit has no line counts: %+v", e)
	}
}

func TestToolTargetsAndBashFacts(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1},
			toolUse("r1", "Read", m{"file_path": "/r/docs/x.md", "offset": 10, "limit": 50}),
			toolUse("s1", "Skill", m{"skill": "review"}),
			toolUse("w1", "WebFetch", m{"url": "https://example.test/p?token=secret#frag"}),
			toolUse("b1", "Bash", m{"command": "go test ./... -run TestA -count=1 -v extra words here"}),
			toolUse("p1", "ExitPlanMode", m{"plan": "abcdef"}),
			toolUse("d1", "TodoWrite", m{"todos": []any{1, 2, 3}})),
		result("b1", 3, true, "Exit code 1\nFAIL", m{"interrupted": false}),
	)
	by := map[string]ToolCall{}
	for _, c := range r.ToolCalls {
		by[c.ToolUseID] = c
	}
	if c := by["r1"]; *c.ReadOffset != 10 || *c.ReadLimit != 50 || *c.Target != "/r/docs/x.md" {
		t.Errorf("read = %+v", c)
	}
	if *by["s1"].Target != "review" || *by["s1"].TargetKind != "skill" {
		t.Errorf("skill = %+v", by["s1"])
	}
	if got := *by["w1"].Target; got != "https://example.test/p" {
		t.Errorf("a URL keeps no query or fragment, got %q", got)
	}
	b := by["b1"]
	if *b.Target != "go test ./... -run TestA -count=1" || *b.ExitCode != 1 || !*b.IsError || *b.Interrupted {
		t.Errorf("bash = %+v", b)
	}
	if *by["p1"].PlanChars != 6 || *by["d1"].TodoItems != 3 {
		t.Errorf("plan/todo = %+v %+v", by["p1"], by["d1"])
	}
}

func TestInjectedFiles(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "attachment", "uuid": "a1", "timestamp": ts(1), "attachment": m{"type": "instructions",
			"files": []m{{"path": "/r/AGENTS.md", "type": "Project", "content": "12345"}, {"path": "/m/MEMORY.md", "type": "AutoMem", "content": "123"}}}}),
		j(m{"type": "attachment", "uuid": "a2", "timestamp": ts(2), "attachment": m{"type": "file", "filename": "/r/x.go",
			"content": m{"file": m{"content": "abcd", "numLines": 1}}}}),
		j(m{"type": "attachment", "uuid": "a3", "attachment": m{"type": "skill_listing", "content": "abc", "skillCount": 2}}),
		j(m{"type": "attachment", "uuid": "a4", "attachment": m{"type": "compact_file_reference", "filename": "/r/y.go"}}),
	)
	if len(r.Injected) != 5 {
		t.Fatalf("injected = %d", len(r.Injected))
	}
	if r.Injected[0].Uid == r.Injected[1].Uid {
		t.Errorf("two files of one record need distinct uids")
	}
	if *r.Injected[0].Bytes != 5 || *r.Injected[1].Bytes != 3 || *r.Injected[1].FileType != "AutoMem" {
		t.Errorf("instructions = %+v %+v", r.Injected[0], r.Injected[1])
	}
	if *r.Injected[2].Lines != 1 || *r.Injected[3].Count != 2 || r.Injected[4].Bytes != nil {
		t.Errorf("file/listing/reference = %+v %+v %+v", r.Injected[2], r.Injected[3], r.Injected[4])
	}
}

func TestColdCacheInferredFromGap(t *testing.T) {
	split := func(w5, w1 int) m {
		return m{"output_tokens": 1, "cache_creation_input_tokens": w5 + w1, "cache_read_input_tokens": 0,
			"cache_creation": m{"ephemeral_5m_input_tokens": w5, "ephemeral_1h_input_tokens": w1}}
	}
	plain := func(write int) m {
		return m{"output_tokens": 1, "cache_creation_input_tokens": write, "cache_read_input_tokens": 0}
	}
	t.Run("5 minute cache, split known", func(t *testing.T) {
		r := parse(t, "main", assistant("m1", 0, split(50, 0)), assistant("m2", 100, split(0, 0)), assistant("m3", 100+400, split(60, 0)), assistant("m4", 100+400+10, m{"output_tokens": 1}))
		c := r.ModelCalls
		if c[0].ColdInferred != nil || c[0].GapMS != nil {
			t.Errorf("the first call has no gap: %+v", c[0])
		}
		if *c[1].ColdInferred || !*c[2].ColdInferred || *c[2].GapMS != 400_000 {
			t.Errorf("cold: %v %v gap %v", *c[1].ColdInferred, *c[2].ColdInferred, *c[2].GapMS)
		}
		if c[3].ColdInferred != nil {
			t.Errorf("no cache figure: cannot be judged, got %v", *c[3].ColdInferred)
		}
	})
	t.Run("1 hour cache lives past 5 minutes", func(t *testing.T) {
		r := parse(t, "main", assistant("m1", 0, split(0, 50)), assistant("m2", 400, split(0, 60)), assistant("m3", 400+4000, plain(60)))
		c := r.ModelCalls
		if c[1].ColdInferred == nil || *c[1].ColdInferred {
			t.Errorf("400 s after a 1 hour write is not cold: %v", c[1].ColdInferred)
		}
		if c[2].ColdInferred == nil || !*c[2].ColdInferred {
			t.Errorf("4000 s after a 1 hour write is cold: %v", c[2].ColdInferred)
		}
	})
	t.Run("a read of all the previous call cached is warm", func(t *testing.T) {
		read := func(r, w int) m {
			return m{"output_tokens": 1, "cache_creation_input_tokens": w, "cache_read_input_tokens": r,
				"cache_creation": m{"ephemeral_5m_input_tokens": w, "ephemeral_1h_input_tokens": 0}}
		}
		r := parse(t, "main", assistant("m1", 0, read(0, 100)), assistant("m2", 400, read(100, 500)), assistant("m3", 800, read(50, 700)))
		c := r.ModelCalls
		if c[1].ColdInferred == nil || *c[1].ColdInferred {
			t.Errorf("the whole previous cache was read back, so it had not expired: %v", c[1].ColdInferred)
		}
		if c[2].ColdInferred == nil || !*c[2].ColdInferred {
			t.Errorf("only part of the previous cache was read after the gap: cold, got %v", c[2].ColdInferred)
		}
	})
	t.Run("split unknown", func(t *testing.T) {
		r := parse(t, "main", assistant("m1", 0, plain(50)), assistant("m2", 400, plain(60)), assistant("m3", 400+4000, plain(60)), assistant("m4", 400+4000+10, plain(60)))
		c := r.ModelCalls
		if c[1].ColdInferred != nil {
			t.Errorf("between 5 and 60 minutes with no split: undeterminable, got %v", *c[1].ColdInferred)
		}
		if c[2].ColdInferred == nil || !*c[2].ColdInferred {
			t.Errorf("over an hour is cold whichever cache it was: %v", c[2].ColdInferred)
		}
		if c[3].ColdInferred == nil || *c[3].ColdInferred {
			t.Errorf("10 s is warm whichever cache it was: %v", c[3].ColdInferred)
		}
	})
}

func TestSettingsTimelineAndUntimedRecord(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}),
		j(m{"type": "permission-mode", "permissionMode": "plan"}),
		j(m{"type": "permission-mode", "permissionMode": "plan"}),
		assistant("m2", 3, m{"output_tokens": 1}),
	)
	var plan *Setting
	for i := range r.Settings {
		if r.Settings[i].Name == "permission_mode" {
			plan = &r.Settings[i]
		}
	}
	if plan == nil || plan.At != nil || plan.NearAt == nil || *plan.NearAt != *parseTS(ts(1)) || plan.Value != "plan" {
		t.Fatalf("permission mode: %+v", plan)
	}
	n := 0
	for _, s := range r.Settings {
		if s.Name == "permission_mode" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a repeated value is not a change: %d rows", n)
	}
}

func TestParseIsDeterministicAndLongLinesCounted(t *testing.T) {
	long := `{"type":"x","pad":"` + strings.Repeat("a", MaxLine+10) + `"}`
	lines := []string{assistant("m1", 1, m{"output_tokens": 1}), long, assistant("m2", 2, m{"output_tokens": 1})}
	a, b := parse(t, "main", lines...), parse(t, "main", lines...)
	if a.Unknown["(too long)"] != 1 || len(a.ModelCalls) != 2 {
		t.Errorf("too long: %v calls %d", a.Unknown, len(a.ModelCalls))
	}
	if j(a) != j(b) {
		t.Errorf("parsing the same bytes twice gave different results")
	}
}

// A message whose content array is malformed loses its tool calls or its
// prompt: the loss is counted by name, and a half-decoded user message is not
// recorded as a human prompt.
func TestUndecodedFieldsAreCounted(t *testing.T) {
	bad := j(m{"type": "assistant", "uuid": "u-bad", "timestamp": ts(1), "requestId": "req-bad",
		"message": m{"id": "mb", "model": "model-a", "usage": m{"output_tokens": 2},
			"content": []any{toolUse("tb", "Read", m{"file_path": "/a"}), 5}}})
	badUser := j(m{"type": "user", "uuid": "ub", "timestamp": ts(2), "promptId": "p1",
		"message": m{"content": []any{m{"type": "text", "text": "hello"}, 5}}})
	badLine := j(m{"type": "system", "subtype": "x", "uuid": "sl", "timestamp": 5})
	badInput := j(m{"type": "assistant", "uuid": "u-in", "timestamp": ts(3), "requestId": "req-in",
		"message": m{"id": "mi", "model": "model-a", "usage": m{"output_tokens": 2},
			"content": []m{toolUse("ti", "Read", m{"file_path": 7})}}})
	r := parse(t, "main", bad, badUser, badLine, badInput)
	for _, k := range []string{"undecoded/assistant_content", "undecoded/user_content", "undecoded/line_field", "undecoded/tool_input"} {
		if r.Ignored[k] != 1 {
			t.Errorf("%s = %d, want 1 (ignored: %v)", k, r.Ignored[k], r.Ignored)
		}
	}
	if len(r.Turns) != 0 {
		t.Errorf("a half-decoded user message became %d turn(s)", len(r.Turns))
	}
	if len(r.ModelCalls) != 2 {
		t.Errorf("the model calls still count, got %d", len(r.ModelCalls))
	}
}

func TestURLHeadDropsQueryFragmentAndUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host.example/a/b?token=x#frag": "https://host.example/a/b",
		"https://host.example/a@b?x=1":                  "https://host.example/a@b",
		"http://u@host":                                 "http://host",
		"https://host.example/p":                        "https://host.example/p",
	} {
		if got := urlHead(in); got != want {
			t.Errorf("urlHead(%q) = %q, want %q", in, got, want)
		}
	}
}
