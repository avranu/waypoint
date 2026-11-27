package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseWPInvocation(t *testing.T) {
	linkCommands()
	for _, tc := range []struct {
		line  string
		head  string
		help  bool
		flags []string
	}{
		{"wp next", "wp next", false, nil},
		{"wp pr show 12 --json", "wp pr show", false, []string{"--json"}},
		{"wp pr show -h", "wp pr show", true, nil},
		{"wp help pr show", "wp pr show", true, nil},
		{"wp ls --search needle", "wp ls", false, []string{"--search"}},
		{"wp ls --search=needle --search=other", "wp ls", false, []string{"--search"}},
		{"wp -p WP ls", "wp ls", false, []string{"-p"}},
		{"wp babysit --now", "wp babysit", false, []string{"--now"}},
		{"wp ship -m summary", "wp ship", false, []string{"-m"}},
		{"wp show WP-1 -- --not-a-flag", "wp show", false, nil},
		{"wp new -d -3,5", "wp new", false, []string{"-d"}},
	} {
		got := parseWPInvocation(strings.Fields(tc.line))
		if got.Head != tc.head || got.Help != tc.help || !reflect.DeepEqual(got.Flags, tc.flags) {
			t.Errorf("%s: %+v, want %s help=%v %v", tc.line, got, tc.head, tc.help, tc.flags)
		}
	}
}

func TestClassifyCLIErrors(t *testing.T) {
	linkCommands()
	for _, tc := range []struct {
		name  string
		text  string
		heads []string
		want  []cliError
	}{
		{"unknown flag", "wp ls: flag provided but not defined: -search\nusage: wp ls", []string{"wp ls"},
			[]cliError{{"wp ls", cliErrUnknownFlag, "--search"}}},
		{"unknown command", `wp: unknown command "babysit"; run wp help for the list`, []string{"wp babysit"},
			[]cliError{{"wp babysit", cliErrUnknownCommand, "wp babysit"}}},
		{"quoted in a task body", "WP-1 Some task\n\nwp ls: flag provided but not defined: -x\nwp: unknown command \"zzz\"; run wp help for the list", []string{"wp show"}, nil},
		{"generic line far down", strings.Repeat("line\n", cliGenericLines+1) + "wp: which project? give one", []string{"wp ls"}, nil},
		{"generic usage near the top", "wp: which project? give one", []string{"wp ls"},
			[]cliError{{"wp ls", cliErrUsage, ""}}},
		{"a runtime failure", "wp show: not found", []string{"wp show"}, nil},
		{"own error quoted far down", strings.Repeat("line\n", cliGenericLines+1) + "wp show: flag provided but not defined: -x", []string{"wp show"}, nil},
		{"far down in a chain", strings.Repeat("line\n", cliGenericLines+1) + "wp ls: flag provided but not defined: -x", []string{"wp show", "wp ls"},
			[]cliError{{"wp ls", cliErrUnknownFlag, "-x"}}},
		{"counted once", "wp ls: flag provided but not defined: -q\nwp ls: flag provided but not defined: -q", []string{"wp ls"},
			[]cliError{{"wp ls", cliErrUnknownFlag, "-q"}}},
	} {
		if got := classifyCLIErrors(tc.text, tc.heads); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func cliCounts(rows []usageCLIRow) map[string]int64 {
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Kind+"|"+r.Head+"|"+r.Detail] += r.N
	}
	return out
}

func readCLIFile(t *testing.T, path string, st usageFileState) *usageParsed {
	t.Helper()
	f := usageFile{Path: path, Session: "s", Kind: "main", Main: true}
	got, err := readUsageFile(f, st, func(string) string { return "Robin/waypoint" }, false)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestUsageParseCLIRows(t *testing.T) {
	linkCommands()
	path := writeLedgerFile(t, "s.jsonl",
		riskyToolUse("m1", "t1", "wp next", "2026-11-06T10:00:00Z"),
		riskyResult("t1", false, "WP-1 a task", "2026-11-06T10:00:01Z"),
		riskyToolUse("m2", "t2", `cd /r && git fetch origin && wp ls --search "x" 2>&1 | head -3`, "2026-11-06T10:01:00Z"),
		riskyResult("t2", false, "wp ls: flag provided but not defined: -search\nusage: wp ls", "2026-11-06T10:01:01Z"),
		riskyToolUse("m3", "t3", "wp babysit", "2026-11-06T10:02:00Z"),
		riskyResult("t3", true, `wp: unknown command "babysit"; run wp help for the list`, "2026-11-06T10:02:01Z"),
		riskyToolUse("m4", "t4", "wp show WP-1", "2026-11-06T10:03:00Z"),
		riskyResult("t4", false, "WP-1\n\na\nb\nc\nd\nwp show: flag provided but not defined: -quoted\nusage: wp show", "2026-11-06T10:03:01Z"),
		riskyToolUse("m5", "t5", "wp pr show -h", "2026-11-06T10:04:00Z"),
		riskyResult("t5", false, "wp pr show N", "2026-11-06T10:04:01Z"),
		riskyToolUse("m6", "t6", "curl -s https://git.example/api/v1/repos/a/b/pulls", "2026-11-06T10:05:00Z"),
		riskyToolUse("m7", "t7", "gh pr list", "2026-11-06T10:06:00Z"),
	)
	got := readCLIFile(t, path, usageFileState{})
	c := cliCounts(got.Chunk.CLI)
	for key, want := range map[string]int64{
		"file||":                                1,
		"bash||":                                7,
		"wp|wp next|":                           1,
		"wp|wp ls|":                             1,
		"wp|wp babysit|":                        1,
		"wp|wp pr show|":                        1,
		"help|wp pr show|":                      1,
		"flag|wp ls|--search":                   1,
		"error|wp ls|unknown_flag":              1,
		"unknown_flag|wp ls|--search":           1,
		"error|wp babysit|unknown_command":      1,
		"unknown_command|wp babysit|wp babysit": 1,
		"follow|wp next|git fetch":              1,
		"follow|wp next|wp ls":                  1,
		"follow|wp next|wp babysit":             1,
		"follow|wp next|wp show":                1,
		"follow|wp next|wp pr show -h":          1,
		"signal||curl_forge":                    1,
		"signal||gh_tea":                        1,
	} {
		if c[key] != want {
			t.Errorf("%s = %d, want %d", key, c[key], want)
		}
	}
	if c["follow|wp next|gh"] != 0 || c["follow|wp next|gh pr"] != 0 {
		t.Errorf("gh counted past the window: %v", c)
	}
	if c["error|wp show|unknown_flag"] != 0 || c["unknown_flag|wp show|--quoted"] != 0 {
		t.Errorf("a quoted error was counted: %v", c)
	}
	for _, r := range got.Chunk.CLI {
		if r.Session != "s" || r.Agent != "main" {
			t.Errorf("row identity: %+v", r)
		}
	}
}

// Only names, flag names and labels leave the machine.
func TestUsageParseCLICarriesNoArguments(t *testing.T) {
	linkCommands()
	path := writeLedgerFile(t, "s.jsonl",
		riskyToolUse("m1", "t1", `wp new "hush-title" -d "hush-body" --label=hush-label --priority hush-pri WP-hush`, "2026-11-06T10:00:00Z"),
		riskyResult("t1", false, "hush-output", "2026-11-06T10:00:01Z"),
		riskyToolUse("m2", "t2", `git commit -m "hush-message" && curl https://hush.example/api/v1/repos/hush/repo`, "2026-11-06T10:01:00Z"),
		riskyResult("t2", false, "hush-output", "2026-11-06T10:01:01Z"),
		riskyToolUse("m3", "t3", `HUSH_TOKEN=hush-secret wp comment WP-hush "hush-note" --hush=1 --not/a-flag`, "2026-11-06T10:02:00Z"),
		riskyResult("t3", false, "wp comment: flag provided but not defined: -hush-output-value=1", "2026-11-06T10:02:01Z"),
	)
	got := readCLIFile(t, path, usageFileState{})
	if len(got.Chunk.CLI) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range got.Chunk.CLI {
		b, _ := json.Marshal(r)
		s := strings.ReplaceAll(strings.ReplaceAll(string(b), `"--hush"`, ""), `"--hush-output-value"`, "")
		if strings.Contains(s, "hush") || strings.Contains(s, "HUSH") {
			t.Errorf("a value left the machine: %s", b)
		}
	}
}

func TestUsageParseCLIFollowsAcrossReads(t *testing.T) {
	linkCommands()
	path := writeLedgerFile(t, "s.jsonl",
		riskyToolUse("m1", "t1", "wp next", "2026-11-06T10:00:00Z"),
		riskyResult("t1", false, "ok", "2026-11-06T10:00:01Z"),
	)
	first := readCLIFile(t, path, usageFileState{})
	if c := cliCounts(first.Chunk.CLI); c["wp|wp next|"] != 1 || c["file||"] != 1 {
		t.Fatalf("first read: %v", c)
	}
	appendLines(t, path,
		riskyToolUse("m2", "t2", "git fetch", "2026-11-06T12:00:00Z"),
		riskyResult("t2", false, "ok", "2026-11-06T12:00:01Z"),
		riskyToolUse("m3", "t3", "git fetch", "2026-11-06T12:00:02Z"),
	)
	second := readCLIFile(t, path, first.State)
	c := cliCounts(second.Chunk.CLI)
	if c["follow|wp next|git fetch"] != 1 || c["wp|wp next|"] != 0 || c["file||"] != 0 {
		t.Errorf("second read: %v", c)
	}
	for _, r := range second.Chunk.CLI {
		if r.Kind == usageCLIFollow && r.Hour != first.Chunk.CLI[0].Hour {
			t.Errorf("follow hour %d, want the wp command's %d", r.Hour, first.Chunk.CLI[0].Hour)
		}
	}
}

func TestUsageParseCLISleepBesideCI(t *testing.T) {
	linkCommands()
	for _, tc := range []struct {
		name string
		cmds []string
		want int64
	}{
		{"same call", []string{"wp pr check 3; sleep 30"}, 1},
		{"after the check", []string{"wp pr check 3", "sleep 30"}, 1},
		{"before the check", []string{"sleep 30", "wp ci status"}, 1},
		{"beside each", []string{"wp pr check 3", "sleep 30", "wp pr check 3"}, 1},
		{"nothing near", []string{"sleep 30", "git status", "wp pr check 3"}, 0},
		{"beside something else", []string{"wp ls", "sleep 30", "wp show WP-1"}, 0},
	} {
		var lines []string
		for i, cmd := range tc.cmds {
			id := string(rune('a' + i))
			lines = append(lines, riskyToolUse("m"+id, "t"+id, cmd, "2026-11-06T10:00:00Z"))
		}
		whole := cliCounts(readCLIFile(t, writeLedgerFile(t, "s.jsonl", lines...), usageFileState{}).Chunk.CLI)
		if whole["signal||sleep_ci"] != tc.want {
			t.Errorf("%s: sleep_ci = %d, want %d", tc.name, whole["signal||sleep_ci"], tc.want)
		}
		if len(lines) < 2 {
			continue
		}
		path := writeLedgerFile(t, "s.jsonl", lines[0])
		first := readCLIFile(t, path, usageFileState{})
		appendLines(t, path, lines[1:]...)
		second := readCLIFile(t, path, first.State)
		split := cliCounts(first.Chunk.CLI)["signal||sleep_ci"] + cliCounts(second.Chunk.CLI)["signal||sleep_ci"]
		if split != tc.want {
			t.Errorf("%s over two reads: sleep_ci = %d, want %d", tc.name, split, tc.want)
		}
	}
}

// A transcript read before these counters existed is read again for them
// alone, under a chunk id of its own.
func TestUsageParseCLIOnlyReread(t *testing.T) {
	linkCommands()
	path := writeLedgerFile(t, "s.jsonl",
		riskyToolUse("m1", "t1", "wp next", "2026-11-06T10:00:00Z"),
		riskyResult("t1", false, "ok", "2026-11-06T10:00:01Z"),
		riskyToolUse("m2", "t2", "git fetch", "2026-11-06T10:01:00Z"),
	)
	got := readCLIFile(t, path, usageFileState{})
	old := got.State
	old.CV = 0
	old.CLICounted = false
	old.CLIPending = nil
	appendLines(t, path, riskyToolUse("m3", "t3", "wp ls", "2026-11-06T10:02:00Z"))
	again := readCLIFile(t, path, old)
	if !again.CLIOnly || len(again.Chunk.Messages) != 0 || again.To != got.To {
		t.Fatalf("re-read: only=%v messages=%d to=%d, want to=%d", again.CLIOnly, len(again.Chunk.Messages), again.To, got.To)
	}
	if chunkID("m", again) == chunkID("m", got) {
		t.Error("the re-read shares the first read's chunk id")
	}
	if a, b := cliCounts(again.Chunk.CLI), cliCounts(got.Chunk.CLI); !reflect.DeepEqual(a, b) {
		t.Errorf("re-read counted %v, first read %v", a, b)
	}
}
