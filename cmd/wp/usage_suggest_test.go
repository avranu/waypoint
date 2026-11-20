package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCommentsOf(t *testing.T) {
	src := "//go:build linux\n\npackage x\n\n// One.\n// Two.\n/* three\n   four */\nfunc f() {} // trailing\n//nolint:all\n"
	s := commentsOf([]byte(src), "//")
	if s.Lines != 10 || s.CommentLines != 4 {
		t.Fatalf("lines %d comments %d, want 10 and 4", s.Lines, s.CommentLines)
	}
	if s.BlockStart != 5 || s.BlockLines != 4 {
		t.Fatalf("block %d+%d, want 5+4", s.BlockStart, s.BlockLines)
	}
	sh := commentsOf([]byte("#!/bin/sh\n# a\necho hi\n# b\n"), "#")
	if sh.CommentLines != 2 || sh.BlockLines != 1 {
		t.Fatalf("shell: %+v", sh)
	}
}

func TestDocOfAndImports(t *testing.T) {
	md := "intro\n# Build\nmake check\nmore\n## Ship\nwp ship\n```\n# not a heading @nope.md\n```\nSee @docs/x.md and `@code.md`, mail a@b.com.\n"
	d := docOf("AGENTS.md", "", []byte(md))
	if d.Lines != 10 || len(d.Sections) != 3 {
		t.Fatalf("doc %+v", d)
	}
	if d.Sections[0].Heading != "Ship" {
		t.Fatalf("largest section %q, want Ship", d.Sections[0].Heading)
	}
	if got := docImports([]byte(md)); !slices.Equal(got, []string{"docs/x.md"}) {
		t.Fatalf("imports %v", got)
	}
	if one := docOf("x", "", []byte("just text\n")); one.Sections != nil {
		t.Fatalf("one section kept: %+v", one.Sections)
	}
}

func TestInstructionDocs(t *testing.T) {
	claude, root := t.TempDir(), t.TempDir()
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(claude, "CLAUDE.md"), "user rules\n")
	write(filepath.Join(root, "CLAUDE.md"), "repo rules @docs/more.md\n")
	write(filepath.Join(root, "docs", "more.md"), "more\n@../CLAUDE.md\n") // a cycle
	write(filepath.Join(root, "AGENTS.md"), "agents\n")
	var mem strings.Builder
	for i := range 250 {
		fmt.Fprintf(&mem, "- line %d\n", i)
	}
	write(filepath.Join(claude, "projects", projectDirName(root), "memory", "MEMORY.md"), mem.String())

	docs := instructionDocs(claude, root, []string{root, root})
	var names []string
	for _, d := range docs {
		names = append(names, filepath.Base(d.Name))
	}
	want := []string{"CLAUDE.md", "CLAUDE.md", "more.md", "AGENTS.md", "MEMORY.md"}
	if !slices.Equal(names, want) {
		t.Fatalf("docs %v, want %v", names, want)
	}
	m := docs[len(docs)-1]
	if !m.Memory || m.Unread != 50 || m.Bytes != int64(len(firstLines([]byte(mem.String()), memoryLines))) {
		t.Fatalf("memory %+v", m)
	}
	if got := projectDirName("/projects/management/waypoint.x"); got != "-projects-management-waypoint-x" {
		t.Fatalf("projectDirName %q", got)
	}
}

func TestHelpKey(t *testing.T) {
	for cmd, want := range map[string]string{
		"wp ship -h":              "wp ship",
		"wp help pr":              "wp pr",
		"wp pr open --help":       "wp pr open",
		"wp help 2>&1":            "wp (its command list)",
		"wp help --all":           "wp (its command list)",
		"df -h":                   "",
		"grep -h foo x":           "",
		"wp ship -m x":            "",
		"kubectl get pods --help": "kubectl get pods",
	} {
		var got string
		for _, seg := range shellWords(cmd) {
			if k := helpKey(stripWords(seg)); k != "" {
				got = k
			}
		}
		if got != want {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
}

func TestSearchPattern(t *testing.T) {
	for cmd, want := range map[string]string{
		`grep -rn "CreateTable" internal/`: "createtable",
		`rg -t go 'func SystemActor'`:      "func systemactor",
		`git grep -n external_links`:       "external_links",
		`wp code find merge queue`:         "merge queue",
		`go test ./... | grep "^ok"`:       "",
		`grep -n "^func" internal/x.go`:    "",
		`grep -n "^check:" Makefile`:       "",
		`grep -n migrations internal/db`:   "migrations",
		`rg ab`:                            "",
	} {
		var got string
		for _, seg := range shellWords(cmd) {
			if p := searchPattern(stripWords(seg)); p != "" {
				got = p
			}
		}
		if got != want {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
}

func TestSugPaths(t *testing.T) {
	sp := &sugPaths{
		roots:    map[string]bool{"/src/app": true, "/src/app-wt": true},
		mainRoot: "/src/app",
		tracked:  map[string]bool{"cmd/main.go": true, "docs/CLI.md": true},
	}
	for _, tc := range []struct{ p, cwd, want string }{
		{"/src/app/cmd/main.go", "", "cmd/main.go"},
		{"/src/app-wt/cmd/main.go:42", "", "cmd/main.go"},
		{"/src/app-gone/docs/CLI.md", "", "docs/CLI.md"}, // a worktree removed since
		{"docs/CLI.md", "/src/app", "docs/CLI.md"},
		{"/src/app/untracked.txt", "", ""},
		{"/elsewhere/cmd/main.go", "", ""},
		{"docs/CLI.md", "", ""},
	} {
		if got := sp.rel(tc.p, tc.cwd); got != tc.want {
			t.Errorf("rel(%q, %q) = %q, want %q", tc.p, tc.cwd, got, tc.want)
		}
	}
	got := sp.readPaths(sugCall{Tool: "Bash", Command: "sed -n 1,40p docs/CLI.md && wp code show cmd/main.go:main | head"}, "/src/app")
	if !slices.Equal(got, []string{"docs/CLI.md", "cmd/main.go"}) {
		t.Fatalf("readPaths %v", got)
	}
}

func TestSugRepoHas(t *testing.T) {
	parent := t.TempDir()
	main := filepath.Join(parent, "app")
	r := &sugRepo{key: "o/app", mainRoot: main, resolver: newRepoResolver()}
	for cwd, want := range map[string]bool{
		filepath.Join(parent, "app-wp1", "cmd"): true, // gone, named after the main checkout
		filepath.Join(parent, "app"):            true,
		filepath.Join(parent, "apple"):          false,
		parent:                                  false, // exists, and isn't the repository
		"":                                      false,
	} {
		if got := r.has(cwd); got != want {
			t.Errorf("has(%q) = %v, want %v", cwd, got, want)
		}
	}
}

// transcript writes a Claude Code transcript of a session in cwd.
func transcript(t *testing.T, path, cwd string, at time.Time, lines ...string) {
	t.Helper()
	head := fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"go"}}`, cwd, at.UTC().Format(time.RFC3339))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(head+"\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assistantLine(id string, ctx int64, content string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":%d,"output_tokens":5},"content":%s}}`, id, ctx-10, content)
}

func TestReadSuggestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s1.jsonl")
	now := time.Now()
	transcript(t, p, "/src/app", now,
		assistantLine("m1", 50_000, `[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"wp ship --bogus"}}]`),
		assistantLine("m1", 50_000, `[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"wp ship --bogus"}}]`), // the same message again
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"flag provided but not defined: -bogus"}]}}`,
		`{"type":"system","subtype":"compact_boundary"}`,
		assistantLine("m2", 250_000, `[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/src/app/docs/CLI.md"}}]`),
		chainAttachment(t, "t2"),
	)
	s, err := readSuggestFile(usageFile{Path: p, Session: "s1", Kind: "main", Main: true}, now.Add(-time.Hour).UnixMilli(), func(cwd string) bool { return cwd == "/src/app" })
	if err != nil || s == nil {
		t.Fatalf("read: %v %v", s, err)
	}
	if s.Messages != 2 || s.StartCtx != 50_000 || s.PeakCtx != 250_000 || s.Over != 1 || s.OverTokens != 50_000 || s.Compactions != 1 {
		t.Fatalf("session %+v", s)
	}
	if len(s.Calls) != 2 || !s.Calls[0].Err || !strings.Contains(s.Calls[0].ErrText, "not defined") || s.Calls[1].Path != "/src/app/docs/CLI.md" {
		t.Fatalf("calls %+v", s.Calls)
	}
	if s.Model != "claude-opus-5-5" {
		t.Fatalf("model %q", s.Model)
	}
	if c := s.Calls; c[0].Ctx != 50_000 || c[1].Ctx != 250_000 || c[0].Nudge != "" || c[1].Nudge != chainKind {
		t.Fatalf("context and nudges %+v", c)
	}
	if other, _ := readSuggestFile(usageFile{Path: p, Session: "s1", Main: true}, 0, func(string) bool { return false }); other != nil {
		t.Fatal("a session elsewhere was read")
	}
	if old, _ := readSuggestFile(usageFile{Path: p, Session: "s1", Main: true}, now.Add(time.Hour).UnixMilli(), func(string) bool { return true }); old != nil {
		t.Fatal("an old session was read")
	}
}

// chainAttachment is the two lines Claude Code (2.1.3xx) records for the
// search-chain nudge's output on a tool call: the hook's run, which names its
// command as "command hook N" rather than wp hook claude-nudge, and the
// context it added.
func chainAttachment(t *testing.T, toolUseID string) string {
	run, err := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{
		"type": "hook_success", "hookName": "PreToolUse:Bash", "toolUseID": toolUseID, "hookEvent": "PreToolUse",
		"stdout": string(nudgeOutput(chainText)), "command": "command hook 5"}})
	if err != nil {
		t.Fatal(err)
	}
	added, err := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{
		"type": "hook_additional_context", "content": []string{chainText}, "hookName": "PreToolUse:Bash",
		"toolUseID": toolUseID, "hookEvent": "PreToolUse"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(run) + "\n" + string(added)
}

func TestChainNoteCall(t *testing.T) {
	line := func(typ, hook string, field string, content any) []byte {
		b, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{"type": typ, "hookName": hook, "toolUseID": "t9", field: content}})
		return b
	}
	for name, c := range map[string]struct {
		line []byte
		want string
	}{
		"added context":      {line("hook_additional_context", "PreToolUse:Bash", "content", []string{"other note", chainText}), "t9"},
		"another note":       {line("hook_additional_context", "PreToolUse:Bash", "content", []string{"use wp pr wait"}), ""},
		"quoted in docs":     {line("hook_additional_context", "PreToolUse:Bash", "content", []string{"see wp code find WORDS|Symbol"}), ""},
		"another event":      {line("hook_additional_context", "PostToolUse:Bash", "content", []string{chainText}), ""},
		"the hook's own run": {line("hook_success", "PreToolUse:Bash", "stdout", string(nudgeOutput(chainText))), "t9"},
		"a run, no note":     {line("hook_success", "PreToolUse:Bash", "stdout", "{}"), ""},
		"not json":           {[]byte(`"type":"hook_additional_context" ` + chainText), ""},
	} {
		if got := chainNoteCall(c.line); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if !strings.Contains(chainText, string(markChainNote)) {
		t.Fatal("the chain note no longer carries the text the scan matches")
	}
}

func TestScanSuggestSessionsSubagentsFollowTheirSession(t *testing.T) {
	claude := t.TempDir()
	parent := t.TempDir()
	main := filepath.Join(parent, "app")
	proj := filepath.Join(claude, "projects", "x")
	now := time.Now()
	transcript(t, filepath.Join(proj, "s1.jsonl"), filepath.Join(parent, "app-wt"), now, assistantLine("m1", 1000, `[]`))
	// its subagent ran in /tmp, and belongs to the session all the same
	transcript(t, filepath.Join(proj, "s1", "subagents", "agent-a.jsonl"), "/tmp", now, assistantLine("m2", 1000, `[]`))
	transcript(t, filepath.Join(proj, "s2.jsonl"), "/tmp", now, assistantLine("m3", 1000, `[]`))
	got, err := scanSuggestSessions(claude, 24*time.Hour, &sugRepo{key: "o/app", mainRoot: main, resolver: newRepoResolver()})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID+"/"+s.Kind)
	}
	if len(got) != 2 || got[0].ID != "s1" || got[0].Kind != "main" || got[1].ID != "s1" || got[1].Kind == "main" {
		t.Fatalf("sessions %v", ids)
	}
}

func sessions(n int, calls ...sugCall) []*sugSession {
	var out []*sugSession
	for i := range n {
		out = append(out, &sugSession{ID: fmt.Sprintf("s%d", i), Kind: "main", Cwd: "/src/app", Messages: 30, Model: "claude-opus-5-5", StartCtx: 40_000, Calls: calls})
	}
	return out
}

func adviceIDs(a []sugAdvice) []string {
	var ids []string
	for _, x := range a {
		ids = append(ids, x.ID)
	}
	return ids
}

func TestSuggestChecks(t *testing.T) {
	bash := func(cmd string) sugCall { return sugCall{Tool: "Bash", Command: cmd, Heads: CommandHeads(cmd)} }
	read := sugCall{Tool: "Read", Path: "/src/app/docs/CLI.md", Bytes: 6000}
	calls := []sugCall{
		bash("wp ship -h"),
		bash(`grep -rn "create table" internal/db`),
		{Tool: "Bash", Command: "wp pr chek 3", Heads: []string{"wp pr"}, Err: true, ErrText: "wp pr: unknown command \"chek\""},
		bash("curl -s https://git.example/api/v1/repos/o/app/pulls/3"),
		bash("sleep 120 && wp pr show 3"),
		read,
	}
	for range sugReadRun {
		calls = append(calls, read)
	}
	calls = append(calls, bash("make check"))
	ss := sessions(6, calls...)
	ss[0].Over, ss[0].OverTokens, ss[0].PeakCtx, ss[0].LastAt = 40, 30_000_000, 600_000, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC).UnixMilli()
	for i := range sugPremiumSearchN {
		ss = append(ss, &sugSession{ID: fmt.Sprintf("x%d", i), Kind: "Explore", Model: "claude-opus-5-5", Tokens: sugTokens{In: 10, CR: 1000}})
	}
	big := strings.Repeat("x", sugDocWarn*approxTokenOf)
	in := &sugInput{
		Sessions: ss,
		Docs:     []sugDoc{{Name: "CLAUDE.md", Bytes: int64(len(big)), Lines: 300}},
		Sources:  []sugSource{{Path: "docs/CLI.md", Lines: 100, CommentLines: 50, CommentBytes: 4000, BlockStart: 1, BlockLines: 20}},
		Paths:    &sugPaths{roots: map[string]bool{"/src/app": true}, mainRoot: "/src/app", tracked: map[string]bool{"docs/CLI.md": true}},
		Inventory: []usageInventoryItem{
			{Kind: "mcp", Name: "unused-server", Scope: "user"},
			{Kind: "skill", Name: "old-skill", Scope: "project:o/app"},
		},
		Window:   func(string) string { return `"auto" (~/.claude/settings.json, modelSettings)` },
		Waypoint: true,
	}
	got := runSuggestChecks(in)
	want := []string{"instructions", "context", "help", "syntax", "searches", "rereads", "comments", "waypoint", "mcp", "agents"}
	if !slices.Equal(adviceIDs(got), want) {
		t.Fatalf("advice %v, want %v", adviceIDs(got), want)
	}
	if got[0].Level != "warn" || got[1].Level != "warn" {
		t.Fatalf("levels %s %s, want warn first", got[0].Level, got[1].Level)
	}
	text := suggestText(&sugReport{Repo: "o/app", Days: 30, Sessions: 6, Subagents: 3, StartMedian: 40_000, DocTokens: 10_000, Suggestions: got})
	for _, s := range []string{
		"o/app: 6 sessions and 3 subagent runs",
		"wp ship: 6 sessions, 6 times",
		`"create table": 6 sessions, 6 times`,
		`wp pr: 6 sessions, 6 times ("wp pr: unknown command \"chek\"")`,
		"The latest was on 2026-10-30",
		`claude-opus-5-5: "auto" (~/.claude/settings.json, modelSettings)`,
		"docs/CLI.md: 6 sessions, 78 times",
		"curl to the forge's API: 6 sessions, 6 times",
		"Waited with sleep",
		"unused-server",
		"old-skill",
		"3 runs of search-only subagents (Explore)",
		"read or searched 12 times or more in a row",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("report lacks %q:\n%s", s, text)
		}
	}

	quiet := runSuggestChecks(&sugInput{Sessions: sessions(6, bash("make check"))})
	if len(quiet) != 0 {
		t.Fatalf("a quiet repository got %v", adviceIDs(quiet))
	}
	if !strings.Contains(suggestText(&sugReport{Repo: "o/app", Days: 30}), "No sessions of o/app") {
		t.Fatal("empty report")
	}
}

func TestCompactWindowFor(t *testing.T) {
	h := compactHave{Files: []compactFile{
		{Name: "~/.claude/settings.json", Top: 200_000, Models: map[string]int{"claude-opus-5-5": compactAuto}},
		{Name: ".claude/settings.json", Top: 150_000},
	}}
	w := compactWindowFor(h)
	if got := w("claude-opus-5-5"); got != `"auto" (~/.claude/settings.json, modelSettings)` {
		t.Errorf("opus: %s", got)
	}
	if got := w("claude-haiku-5-5"); got != "150000 (.claude/settings.json)" {
		t.Errorf("haiku: %s", got)
	}
	h.Env = "100000"
	if got := compactWindowFor(h)("claude-opus-5-5"); !strings.HasPrefix(got, "100000 (CLAUDE_CODE") {
		t.Errorf("env: %s", got)
	}
	if got := compactWindowFor(compactHave{})("x"); !strings.Contains(got, "none set") {
		t.Errorf("none: %s", got)
	}
}

// The nudge's point is where it told, or where the hook's count would have
// told; the calls after it are compared, told against not.
func TestChainPoint(t *testing.T) {
	call := func(cmd, msg string) sugCall {
		return sugCall{Tool: "Bash", Command: cmd, Heads: CommandHeads(cmd), Ctx: 100, msg: msg}
	}
	chain := []sugCall{
		call("grep -rn MergeReady internal/", "m1"), call("grep -rn markReady internal/", "m2"), call("sed -n 1,9p a.go", "m3"),
		call("sed -n 1,9p b.go", "m4"), call("sed -n 1,9p c.go", "m5"), call("sed -n 9,20p c.go", "m5"), // the point, and a call beside it
		call("go test ./...", "m6"), call("go vet ./...", "m6"), call("wp code find mergeGate", "m7"), {Tool: "Edit", msg: "m8"},
	}
	if p := chainPoint(chain, 9); p != 4 {
		t.Fatalf("point %d", p)
	}
	if p := chainPoint(append([]sugCall{call("wp brief", "m0")}, chain...), 10); p != -1 {
		t.Fatalf("after wp brief, point %d", p)
	}
	late := append([]sugCall(nil), chain...)
	late[6].Nudge = chainKind // another note took the would-be point
	if p := chainPoint(late, 9); p != 6 {
		t.Fatalf("told later, point %d", p)
	}
	// Grep-tool calls never reach the hook.
	grep := func(m string) sugCall { return sugCall{Tool: "Grep", Pattern: "x", msg: m} }
	if p := chainPoint([]sugCall{grep("g1"), grep("g2"), grep("g3"), grep("g4"), grep("g5"), {Tool: "Edit"}}, 5); p != -1 {
		t.Fatalf("grep tool calls, point %d", p)
	}
	// Told after the first edit: neither arm.
	post := append([]sugCall(nil), chain...)
	post[9].Nudge = chainKind
	if p := chainPoint(post, 3); p != -1 {
		t.Fatalf("told after the edit, point %d", p)
	}
	o := orientation([]*sugSession{{ID: "a", Kind: "main", Calls: chain}, {ID: "b", Kind: "main", Calls: late[:9]}})
	want := sugAfterPoint{Transcripts: 1, Edited: 1, Calls: 3, Ctx: 200, CodeTools: 1}
	if o.Untold != want {
		t.Errorf("not told %+v, want %+v", o.Untold, want)
	}
	want = sugAfterPoint{Transcripts: 1, Calls: 1, Ctx: 100, CodeTools: 1} // the sibling in m6 saw nothing; no edit
	if o.Told != want {
		t.Errorf("told %+v, want %+v", o.Told, want)
	}
}

// Orientation counts calls before the first edit, search chains in them,
// and who used wp code or wp brief, before or after the nudge.
func TestOrientation(t *testing.T) {
	bash := func(cmd string, ctx int64) sugCall {
		return sugCall{Tool: "Bash", Command: cmd, Heads: CommandHeads(cmd), Ctx: ctx}
	}
	chain := []sugCall{
		bash("wp next", 10), bash("grep -rn MergeReady internal/", 10), bash("sed -n 1,40p internal/core/merge.go", 10),
		bash("grep -rn markReady internal/", 10), {Tool: "Read", Path: "/r/a.go", Ctx: 10, Nudge: chainKind}, bash("sed -n 5,9p a.go", 10),
		bash("wp code show a.go:markReady", 10), {Tool: "Edit", Path: "/r/a.go"}, bash("grep -rn after internal/", 10),
	}
	quick := []sugCall{bash("wp code find MergeReady", 5), {Tool: "Edit"}}
	none := []sugCall{bash("go test ./...", 5)}
	ss := []*sugSession{
		{ID: "a", Kind: "main", Calls: chain},
		{ID: "a", Kind: "Explore", Calls: quick},
		{ID: "b", Kind: "main", Calls: none},
		{ID: "c", Kind: "main"},
	}
	o := orientation(ss)
	if o == nil {
		t.Fatal("nil")
	}
	want := sugOrientation{Transcripts: 3, Mains: 2, Edited: 2, CallsMain: 7, CallsSub: 1, CtxMedian: 5, ChainShare: 5 * 100 / 9,
		Chains: 1, CodeTools: 2, Next: 1, Nudged: 1, Followed: 1,
		Told: sugAfterPoint{Transcripts: 1, Edited: 1, Calls: 2, Ctx: 20, CodeTools: 1}}
	if *o != want {
		t.Errorf("got  %+v\nwant %+v", *o, want)
	}
	if txt := orientationText(o); !strings.Contains(txt, "1 ran a search chain") || !strings.Contains(txt, "1 were shown the search-chain nudge") ||
		!strings.Contains(txt, "told: 1 transcript (1 reached an edit), a median 2 calls") || strings.Contains(txt, "not told") {
		t.Errorf("text %q", txt)
	}
	if a := adviceOrientation(&sugInput{Sessions: ss}); a != nil {
		t.Errorf("advice on one chain: %+v", a)
	}
	var many []*sugSession
	for i := range 4 {
		many = append(many, &sugSession{ID: fmt.Sprint(i), Kind: "main", Calls: chain[1:6]})
	}
	if a := adviceOrientation(&sugInput{Sessions: many}); a == nil || a.ID != "orientation" {
		t.Errorf("no advice on four chains: %+v", a)
	}
	// --until: sessions that started before the cut, and their subagents
	ss = []*sugSession{{ID: "a", Kind: "main", FirstAt: 100}, {ID: "a", Kind: "Explore", FirstAt: 300}, {ID: "b", Kind: "main", FirstAt: 300}, {ID: "b", Kind: "Explore", FirstAt: 400}}
	if got := startedBefore(ss, 200); len(got) != 2 || got[0].ID != "a" || got[1].ID != "a" {
		t.Errorf("startedBefore %+v", got)
	}
}
