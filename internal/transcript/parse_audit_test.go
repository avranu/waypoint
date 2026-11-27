package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func settingRows(r *Result, name string) []Setting {
	var out []Setting
	for _, s := range r.Settings {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// A mode cycled with no timed record between would give two changes the same
// name=value@time uid, and the store would keep only the first.
func TestSettingChangesBackToAnEarlierValueKeepTheirOwnUid(t *testing.T) {
	mode := func(v string) string { return j(m{"type": "permission-mode", "permissionMode": v}) }
	lines := []string{assistant("m1", 1, m{"output_tokens": 1}),
		mode("default"), mode("acceptEdits"), mode("plan"), mode("default"), mode("acceptEdits")}
	r := parse(t, "main", lines...)
	rows := settingRows(r, "permission_mode")
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	seen := map[string]bool{}
	for _, s := range rows {
		if seen[s.Uid] {
			t.Errorf("uid %q is used by two changes", s.Uid)
		}
		seen[s.Uid] = true
	}
	again := settingRows(parse(t, "main", lines...), "permission_mode")
	if len(again) != len(rows) {
		t.Fatalf("second parse: %d rows, want %d", len(again), len(rows))
	}
	if again[3].Uid != rows[3].Uid || again[4].Uid != rows[4].Uid {
		t.Errorf("the same bytes must give the same uids")
	}
}

func TestOtherSettingsAreRecordedWithTheirPrevious(t *testing.T) {
	line := func(id string, sec int, model, effort, branch, version string) string {
		return j(m{"type": "assistant", "uuid": "u" + id, "timestamp": ts(sec), "requestId": "q" + id, "effort": effort, "perTurnEffort": effort + "-t",
			"gitBranch": branch, "version": version,
			"message": m{"id": id, "model": model, "usage": m{"output_tokens": 1}, "content": []m{}}})
	}
	r := parse(t, "main",
		line("a", 1, "model-a", "high", "main", "2.1.0"),
		line("b", 2, "model-b", "high", "feature", "2.1.0"),
	)
	for name, want := range map[string][]string{
		"model": {"model-a", "model-b"}, "effort": {"high"}, "per_turn_effort": {"high-t"},
		"git_branch": {"main", "feature"}, "harness_version": {"2.1.0"},
	} {
		rows := settingRows(r, name)
		if len(rows) != len(want) {
			t.Errorf("%s rows = %d, want %d", name, len(rows), len(want))
			continue
		}
		for i, w := range want {
			if rows[i].Value != w {
				t.Errorf("%s[%d] = %q, want %q", name, i, rows[i].Value, w)
			}
		}
	}
	if b := settingRows(r, "git_branch"); b[0].Prev != nil || b[1].Prev == nil || *b[1].Prev != "main" {
		t.Errorf("git_branch prev: %+v", b)
	}
}

// An omitted is_error is false by the Messages API's own definition, so a
// success without the key is a known success; result_bytes has no default and
// is nil without a content key.
func TestResultWithoutIsErrorOrContent(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("a", "Read", m{"file_path": "/x"}), toolUse("b", "Read", m{"file_path": "/y"}), toolUse("c", "Read", m{"file_path": "/z"})),
		j(m{"type": "user", "uuid": "r1", "timestamp": ts(2), "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "a"}}}}),
		j(m{"type": "user", "uuid": "r2", "timestamp": ts(2), "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "b", "content": ""}}}}),
		j(m{"type": "user", "uuid": "r3", "timestamp": ts(2), "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "c", "content": []m{{"type": "text", "text": "ab"}, {"type": "text", "text": "cd"}}}}}}),
	)
	by := map[string]ToolCall{}
	for _, c := range r.ToolCalls {
		by[c.ToolUseID] = c
	}
	if a := by["a"]; a.IsError == nil || *a.IsError || a.ResultBytes != nil {
		t.Errorf("no is_error, no content: is_error %v (a known false), result_bytes %v (nil)", a.IsError, a.ResultBytes)
	}
	if b := by["b"]; b.ResultBytes == nil || *b.ResultBytes != 0 {
		t.Errorf("an empty string is a known 0: %v", b.ResultBytes)
	}
	if c := by["c"]; c.ResultBytes == nil || *c.ResultBytes != 4 {
		t.Errorf("a block-array result is summed: %v", c.ResultBytes)
	}
}

func TestInjectedSizesAreNilWhenContentIsAbsent(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "attachment", "uuid": "a1", "attachment": m{"type": "instructions", "files": []m{{"path": "/a", "type": "Project"}, {"path": "/b", "type": "Project", "content": ""}}}}),
		j(m{"type": "attachment", "uuid": "a2", "attachment": m{"type": "invoked_skills", "skills": []m{{"name": "s1"}, {"name": "s2", "content": "xyz"}}}}),
	)
	if len(r.Injected) != 4 {
		t.Fatalf("injected = %d", len(r.Injected))
	}
	if r.Injected[0].Bytes != nil || r.Injected[1].Bytes == nil || *r.Injected[1].Bytes != 0 {
		t.Errorf("instructions: absent content is nil, empty is 0: %v %v", r.Injected[0].Bytes, r.Injected[1].Bytes)
	}
	if r.Injected[2].Bytes != nil || *r.Injected[3].Bytes != 3 {
		t.Errorf("skills: %v %v", r.Injected[2].Bytes, r.Injected[3].Bytes)
	}
}

func TestWebFetchWithoutURLHasNoTarget(t *testing.T) {
	for _, in := range []m{{}, {"url": ""}, {"url": "  "}} {
		r := parse(t, "main", assistant("m1", 1, m{"output_tokens": 1}, toolUse("w", "WebFetch", in)))
		if len(r.ToolCalls) != 1 || r.ToolCalls[0].Target != nil || r.ToolCalls[0].TargetKind != nil {
			t.Errorf("input %v: target %v", in, r.ToolCalls)
		}
	}
}

// Content that is there but is neither a string nor a block array has an
// unknown size, not 0 (it is counted undecoded).
func TestResultContentOfAnotherShapeHasNoSize(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("a", "Read", m{"file_path": "/x"}), toolUse("b", "Read", m{"file_path": "/y"})),
		j(m{"type": "user", "uuid": "r1", "timestamp": ts(2), "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "a", "content": 5}}}}),
		j(m{"type": "user", "uuid": "r2", "timestamp": ts(2), "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "b", "content": m{"x": 1}}}}}),
	)
	for _, c := range r.ToolCalls {
		if c.ResultBytes != nil {
			t.Errorf("%s: result_bytes %d, want nil", c.ToolUseID, *c.ResultBytes)
		}
	}
	if r.Ignored["undecoded/tool_result_content"] != 2 {
		t.Errorf("ignored %v", r.Ignored)
	}
}

func TestFileAttachmentWithoutContentHasNoSize(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "attachment", "uuid": "a1", "attachment": m{"type": "file", "filename": "/f", "content": m{"type": "text", "file": m{"numLines": 3}}}}),
		j(m{"type": "attachment", "uuid": "a2", "attachment": m{"type": "file", "filename": "/g", "content": m{"type": "text", "file": m{"content": "", "numLines": 0}}}}),
	)
	if len(r.Injected) != 2 {
		t.Fatalf("injected %d", len(r.Injected))
	}
	if r.Injected[0].Bytes != nil {
		t.Errorf("absent content: bytes %d, want nil", *r.Injected[0].Bytes)
	}
	if b := r.Injected[1].Bytes; b == nil || *b != 0 {
		t.Errorf("empty content is a known 0: %v", b)
	}
}

// A JSON null is an absent value: not "", and not undecoded.
func TestNullContentIsAbsent(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("w", "Write", m{"file_path": "/a"})),
		result("w", 2, false, "ok", m{"type": "create", "content": nil}),
		j(m{"type": "attachment", "uuid": "s", "attachment": m{"type": "skill_listing", "content": nil}}),
	)
	if c := r.ToolCalls[0]; c.LinesAdded != nil {
		t.Errorf("lines added %d, want nil", *c.LinesAdded)
	}
	if len(r.Injected) != 1 || r.Injected[0].Bytes != nil {
		t.Errorf("skill_listing bytes: %v", r.Injected)
	}
	for k := range r.Ignored {
		if strings.HasPrefix(k, "undecoded/") {
			t.Errorf("counted %s", k)
		}
	}
}

func TestMoreUndecodedFieldsAreCounted(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("w", "Write", m{"file_path": "/a"})),
		result("w", 2, false, "ok", m{"type": "create", "content": 5}),
		j(m{"type": "attachment", "uuid": "f", "attachment": m{"type": "file", "filename": "/f", "content": "not an object"}}),
		j(m{"type": "attachment", "uuid": "s", "attachment": m{"type": "skill_listing", "content": 7}}),
		j(m{"type": "attachment", "uuid": "ok", "attachment": m{"type": "skill_listing"}}),
	)
	for _, k := range []string{"undecoded/create_result_content", "undecoded/file_attachment", "undecoded/skill_listing"} {
		if r.Ignored[k] != 1 {
			t.Errorf("%s = %d, want 1 (ignored %v)", k, r.Ignored[k], r.Ignored)
		}
	}
}

func TestURLHeadKeepsNoSecret(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host.example/a/b?token=x#frag": "https://host.example/a/b",
		"https://u:p?ss@host.example/p":                 unparsedURL,
		"https://u:p#ss@host.example/p":                 unparsedURL,
		"":                                              "",
		"   ":                                           "",
		"https://u:p%23ss@host.example/p":               "https://host.example/p",
		"https://u:pa/ss@host.example/p":                unparsedURL,
		"not a url":                                     unparsedURL,
		"://broken":                                     unparsedURL,
		"https://host.example/p":                        "https://host.example/p",
		"https://u:12/ss@host.example/p":                unparsedURL,
		"https://user:12?ss@host.example/x":             unparsedURL,
		"https://user:12#ss@host.example/x":             unparsedURL,
		"https://medium.example/@someone/post":          "https://medium.example/@someone/post",
		"http://localhost:3000/a":                       "http://localhost:3000/a",
	} {
		got := urlHead(in)
		if got != want {
			t.Errorf("urlHead(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "p#") || strings.Contains(got, "u:") || strings.Contains(got, ":12") || strings.Contains(got, "pw") || strings.Contains(got, "ss@") || strings.Contains(got, "token") {
			t.Errorf("urlHead(%q) kept a secret: %q", in, got)
		}
	}
}

func TestAPIRequestJoinsInEitherOrderAndDropsAreCounted(t *testing.T) {
	call := func(id string, sec int, ref string) string {
		return j(m{"type": "assistant", "uuid": "u" + id, "timestamp": ts(sec), "requestId": "q" + id, "requestRef": ref,
			"message": m{"id": id, "model": "model-a", "usage": m{"output_tokens": 1}, "content": []m{}}})
	}
	req := func(id string, sec int) string {
		return j(m{"type": "api-request", "id": id, "timestamp": ts(sec), "querySource": "repl_main_thread"})
	}
	before := parse(t, "main", req("r1", 1), call("m1", 5, "r1"))
	after := parse(t, "main", call("m1", 5, "r1"), req("r1", 1))
	for name, r := range map[string]*Result{"request first": before, "request after": after} {
		c := r.ModelCalls[0]
		if c.LatencyMS == nil || *c.LatencyMS != 4000 || c.QuerySource == nil || *c.QuerySource != "repl_main_thread" {
			t.Errorf("%s: latency %v source %v", name, c.LatencyMS, c.QuerySource)
		}
	}
	d := parse(t, "main", call("m1", 5, ""), j(m{"type": "api-request", "timestamp": ts(1)}), j(m{"type": "api-request", "id": "x"}), req("orphan", 2),
		assistant("m2", 6, m{"output_tokens": 1}, m{"type": "tool_use", "name": "Read", "input": m{}}))
	for _, k := range []string{"api-request/(no id or time)", "api-request/unmatched", "tool_use/(no id)"} {
		want := 1
		if k == "api-request/(no id or time)" {
			want = 2
		}
		if d.Ignored[k] != want {
			t.Errorf("%s = %d, want %d (%v)", k, d.Ignored[k], want, d.Ignored)
		}
	}
}

func TestTestPathHeuristic(t *testing.T) {
	for p, want := range map[string]bool{
		"src/foo_test.go": true, "src/foo_test.ts": true, "lib/foo_spec.rb": true, "tests/x.go": true, "a/test_foo.py": true,
		"web/a.test.tsx": true, "web/a.spec.js": true, "src/FooTest.java": true, "src/FooTests.cs": true, "src/FooTest.kt": true,
		"src/foo.go": false, "src/latest.go": false, "src/contest.rs": false, "README.md": false, "src/attest.py": false,
	} {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestStreamedToolUseIsOneCall(t *testing.T) {
	r := parse(t, "main",
		assistant("m1", 1, m{"output_tokens": 1}, toolUse("t1", "Read", m{"file_path": "/a"})),
		assistant("m1", 2, m{"output_tokens": 5}, toolUse("t1", "Read", m{"file_path": "/a"})),
	)
	if len(r.ToolCalls) != 1 || len(r.ModelCalls) != 1 {
		t.Errorf("tool calls %d, model calls %d: a tool_use repeated across the lines of a streamed message is one call", len(r.ToolCalls), len(r.ModelCalls))
	}
}

func TestPromptKindsAndUnmatchedResults(t *testing.T) {
	user := func(uuid string, sec int, extra m, content any) string {
		r := m{"type": "user", "uuid": uuid, "timestamp": ts(sec), "message": m{"content": content}}
		for k, v := range extra {
			r[k] = v
		}
		return j(r)
	}
	r := parse(t, "main",
		j(m{"type": "system", "subtype": "turn_duration", "timestamp": ts(0), "durationMs": 5}),
		user("p1", 1, nil, "<command-name>/review</command-name> args"),
		user("p2", 2, nil, "<local-command-stdout>out</local-command-stdout>"),
		user("p3", 3, m{"isMeta": true}, "meta text"),
		user("p4", 4, m{"isMeta": true, "turnOrigin": "peer"}, "meta with an origin"),
		result("nope", 5, false, "ok", nil),
	)
	if len(r.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (slash command, meta with an origin)", len(r.Turns))
	}
	if r.Turns[0].SlashCommand == nil || *r.Turns[0].SlashCommand != "/review" {
		t.Errorf("slash command: %v", r.Turns[0].SlashCommand)
	}
	for _, k := range []string{"user/local_command_output", "user/meta", "tool_result/unmatched", "system/turn_duration (no open turn)"} {
		if r.Ignored[k] != 1 {
			t.Errorf("%s = %d (%v)", k, r.Ignored[k], r.Ignored)
		}
	}
}

func TestLatencyThinkingTimeoutAndBackground(t *testing.T) {
	call := func(sec int, ref string, think int) string {
		return j(m{"type": "assistant", "uuid": "um", "timestamp": ts(sec), "requestId": "qm", "requestRef": ref, "thinkingDurationMs": think,
			"message": m{"id": "m1", "model": "model-a", "usage": m{"output_tokens": 1}, "content": []m{
				toolUse("b1", "Bash", m{"command": "sleep 1"}), toolUse("b2", "Bash", m{"command": "sleep 2", "run_in_background": true})}}})
	}
	r := parse(t, "main",
		j(m{"type": "api-request", "id": "r1", "timestamp": ts(1), "querySource": "repl_main_thread"}),
		call(3, "r1", 100),
		call(4, "r1", 250),
		result("b1", 5, false, "ok", m{"timedOutAfterMs": 1000}),
		result("b2", 6, false, "ok", m{"backgroundTaskId": "bg1"}),
	)
	c := r.ModelCalls[0]
	if c.LatencyMS == nil || *c.LatencyMS != 3000 || c.ThinkingMS == nil || *c.ThinkingMS != 350 {
		t.Errorf("latency %v (request to the last block) thinking %v (summed)", c.LatencyMS, c.ThinkingMS)
	}
	by := map[string]ToolCall{}
	for _, tc := range r.ToolCalls {
		by[tc.ToolUseID] = tc
	}
	if b := by["b1"]; b.TimedOut == nil || !*b.TimedOut || b.Background != nil {
		t.Errorf("b1: timed out %v background %v", b.TimedOut, b.Background)
	}
	if b := by["b2"]; b.Background == nil || !*b.Background {
		t.Errorf("b2 background: %v", b.Background)
	}
}

// ParserVersion says which parser wrote a row. This pins what the parser makes
// of one fixture: when the output changes, ParserVersion must change with it
// (so archived files are re-derived), and the pin is updated in the same edit.
func TestParserOutputIsPinnedToParserVersion(t *testing.T) {
	r := parse(t, "main",
		j(m{"type": "user", "uuid": "p1", "timestamp": ts(1), "promptId": "pr1", "turnOrigin": "human", "permissionMode": "plan",
			"message": m{"content": "<command-name>/x</command-name>"}, "turnPosition": m{"promptIndex": 0, "turnIndex": 0}}),
		j(m{"type": "api-request", "id": "r1", "timestamp": ts(2), "querySource": "repl_main_thread"}),
		j(m{"type": "assistant", "uuid": "u1", "timestamp": ts(5), "requestId": "q1", "requestRef": "r1", "gitBranch": "main", "version": "2.1.0", "effort": "high",
			"message": m{"id": "m1", "model": "model-a", "stop_reason": "tool_use", "usage": m{"input_tokens": 3, "output_tokens": 9, "cache_read_input_tokens": 4, "cache_creation_input_tokens": 2,
				"cache_creation": m{"ephemeral_5m_input_tokens": 2, "ephemeral_1h_input_tokens": 0}},
				"content": []m{toolUse("e1", "Edit", m{"file_path": "/r/a_test.go"}), toolUse("a1", "Agent", m{"subagent_type": "Explore", "description": "look"}), toolUse("w1", "WebFetch", m{"url": "https://u:p@h.example/x?y=1"})}}}),
		result("e1", 6, false, "ok", m{"type": "update", "structuredPatch": []m{{"lines": []string{"+a", "-b"}}}}),
		result("a1", 7, false, "ok", m{"status": "completed", "agentId": "ag1", "totalTokens": 99, "totalDurationMs": 1500}),
		j(m{"type": "system", "subtype": "turn_duration", "timestamp": ts(8), "durationMs": 7000, "messageCount": 3}),
		j(m{"type": "system", "subtype": "compact_boundary", "uuid": "c1", "timestamp": ts(9), "compactMetadata": m{"trigger": "auto", "preTokens": 10, "postTokens": 2}}),
		j(m{"type": "attachment", "uuid": "i1", "timestamp": ts(10), "attachment": m{"type": "instructions", "files": []m{{"path": "/r/AGENTS.md", "type": "Project", "content": "abc"}}}}),
		j(m{"type": "mystery", "timestamp": ts(11)}),
	)
	sum := sha256.Sum256([]byte(j(r)))
	got := hex.EncodeToString(sum[:])
	const pinned = "eb5b05320797e7a12450485e75451f6d9fc8928e5cfeb91fd7bb6af326cb152c"
	if got != pinned {
		t.Fatalf("the parser's output for the fixture changed (version %d): bump ParserVersion and re-pin this to %s", ParserVersion, got)
	}
}
