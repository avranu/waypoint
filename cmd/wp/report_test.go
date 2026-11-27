package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// resolveWords is the command a set of words runs, as run() finds it.
func resolveWords(words []string) (*command, []string) {
	linkCommands()
	words, _ = rewriteLegacy(words)
	cmd := findCommand(words[0])
	if cmd == nil {
		return nil, nil
	}
	rest := words[1:]
	for len(rest) > 0 {
		s := cmd.sub(rest[0])
		if s == nil {
			break
		}
		cmd, rest = s, rest[1:]
	}
	return cmd, rest
}

func runFn(c *command) uintptr { return reflect.ValueOf(c.run).Pointer() }

func reportEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
}

// Every old name runs the command that replaced it, flags and all (WP-ADR164).
func TestLegacyNamesRunTheNewCommand(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"usage where --since 24h", "report usage --since 24h"},
		{"usage task WP-1 --user a", "report usage --task WP-1 --user a"},
		{"burn", "report usage --session"},
		{"burn --session abc", "report usage --session abc"},
		{"burn --session=abc --json", "report usage --session abc --json"},
		{"usage machines", "report usage machines"},
		{"usage guard --days 3", "report usage guard --days 3"},
		{"usage guard set --off", "guard set --off"},
		{"usage guard lift --for 2h", "guard lift --for 2h"},
		{"usage cli", "report behavior cli"},
		{"usage risky --high", "report behavior risky --high"},
		{"usage stalled", "report behavior stalled"},
		{"usage warm", "report behavior warm"},
		{"usage compact", "report behavior compact"},
		{"usage delegation", "report behavior delegation"},
		{"usage subagents", "report behavior subagents"},
		{"usage suggest", "report behavior suggest"},
		{"usage goal-context", "report behavior goal-context"},
		{"usage facts S1", "report behavior facts S1"},
		{"ai usage --by day", "report ai --by day"},
		{"ai surplus", "report ai surplus"},
		{"ai", "report ai"},
		{"usage WP-1 --tokens-in 5", "record usage WP-1 --tokens-in 5"},
		{"usage --tokens-in 5", "record usage --tokens-in 5"},
		{"usage", "record usage"},
		{"usage sync --hook", "record usage --from-transcripts --hook"},
		{"usage transcript S1 --file main", "transcript S1 --file main"},
		{"usage otel --install", "setup otel --install"},
		{"report cost", "report usage"},
		{"report spend", "report usage"},
		{"report quota", "report usage"},
		{"report requests ai", "report ai"},
		{"report behaviour risky", "report behavior risky"},
		{"usage meaning", "report meaning"},
		{"usage feature WP-1", "report feature WP-1"},
		{"ci history --days 9 --week", "report ci history --days 9 --week"},
		{"ci line", "report ci line"},
		{"ci tests --flaky", "report ci tests --flaky"},
		{"ci coverage internal/core", "report ci coverage internal/core"},
		{"ci suppressions --all", "report ci suppressions --all"},
		{"activity claude --days 7", "report activity claude --days 7"},
		{"mistakes -p WP", "report quality mistakes -p WP"},
		{"escapes --days 30 -p WP", "report quality escapes --days 30 -p WP"},
		{"escapes", "report quality escapes"},
		{"escapes ls --limit 5", "escapes ls --limit 5"},
		{"worklog stats -p WP", "report quality reviews -p WP"},
		{"worklog triage --rows", "report quality triage --rows"},
		{"worklog stakes --days 3", "report quality stakes --days 3"},
		{"log WP-1 45m -m x", "record time WP-1 45m -m x"},
		{"time WP-1 1h", "record time WP-1 1h"},
		{"env deploy staging --status started", "record deploy staging --status started"},
		{"envs deployed prod", "record deploy prod"},
	} {
		oc, orest := resolveWords(strings.Fields(tc.old))
		nc, nrest := resolveWords(strings.Fields(tc.new))
		if oc == nil || nc == nil {
			t.Errorf("wp %s or wp %s resolves to nothing", tc.old, tc.new)
			continue
		}
		if oc != nc || !slices.Equal(orest, nrest) {
			t.Errorf("wp %s runs wp %s %v; wp %s runs wp %s %v", tc.old, oc.path(), orest, tc.new, nc.path(), nrest)
		}
	}
}

// wp guard reads as wp report usage guard does, and sets and lifts.
func TestGuardIsBothReadAndControl(t *testing.T) {
	g, _ := resolveWords([]string{"guard"})
	r, _ := resolveWords([]string{"report", "usage", "guard"})
	if g == nil || r == nil || runFn(g) != runFn(r) {
		t.Fatalf("wp guard and wp report usage guard run different functions")
	}
	if g.sub("set") == nil || g.sub("lift") == nil || r.sub("set") != nil || r.sub("lift") != nil {
		t.Errorf("set and lift belong to wp guard, and a report changes nothing")
	}
}

func TestHelpListsNewNamesOnly(t *testing.T) {
	reportEnv(t)
	var out bytes.Buffer
	if code := run([]string{"help"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	list := out.String()
	for _, want := range []string{"Reports:", "  report ", "  record ", "  guard "} {
		if !strings.Contains(list, want) {
			t.Errorf("wp help lacks %q:\n%s", want, list)
		}
	}
	for _, old := range []string{"  usage ", "  burn ", "  ai ", "  activity ", "  mistakes ", "\n  log "} {
		if strings.Contains(list, old) {
			t.Errorf("wp help lists the old name %q", old)
		}
	}
	out.Reset()
	if code := run([]string{"help", "--all"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	all := out.String()
	for _, old := range []string{"\n## wp burn\n", "\n## wp ai\n", "\n## wp usage sync\n", "\n## wp usage where\n", "\n## wp usage guard\n", "\n## wp usage meaning\n", "\n## wp activity\n", "\n## wp mistakes\n", "\n## wp log\n", "\n## wp env deploy\n", "\n## wp ci history\n", "\n## wp worklog stats\n"} {
		if strings.Contains(all, old) {
			t.Errorf("wp help --all documents the old name %q", strings.TrimSpace(old))
		}
	}
	for _, want := range []string{"\n## wp report usage\n", "\n## wp report behavior cli\n", "\n## wp report ai surplus\n", "\n## wp record usage\n", "\n## wp guard set\n", "\n## wp transcript\n", "\n## wp setup otel\n", "\n## wp report meaning\n", "\n## wp report feature\n", "\n## wp report ci history\n", "\n## wp report activity\n", "\n## wp report quality mistakes\n", "\n## wp report quality reviews\n", "\n## wp record time\n", "\n## wp record deploy\n", "\n## wp ci status\n", "\n## wp escapes ls\n"} {
		if !strings.Contains(all, want) {
			t.Errorf("wp help --all lacks %q", strings.TrimSpace(want))
		}
	}
}

// A task or issue prefix goes before an old name as it does before a new one.
func TestTaskPrefixBeforeMovedCommands(t *testing.T) {
	reportEnv(t)
	for _, args := range []string{"task log -h", "issues time -h", "tasks activity -h", "issue worklog stats -h", "task usage -h"} {
		var out, errb bytes.Buffer
		if code := run(strings.Fields(args), &out, &errb); code != 0 || !strings.Contains(out.String(), " is now ") {
			t.Errorf("wp %s: %d %q %s", args, code, out.String(), errb.String())
		}
	}
}

// The note naming the new command comes with help only: a -h given as a
// flag's value leaves stdout to the command's answer.
func TestLegacyNoteOnlyWithHelp(t *testing.T) {
	reportEnv(t)
	var out, errb bytes.Buffer
	// No arguments but the note's value: refused before any request.
	run([]string{"log", "-m", "-h", "--no-such-flag"}, &out, &errb)
	if out.Len() != 0 {
		t.Errorf("stdout carries %q", out.String())
	}
}

// --from-transcripts is a switch: a word after it is refused, not dropped.
func TestFromTranscriptsTakesNoValue(t *testing.T) {
	reportEnv(t)
	for _, args := range []string{"record usage --from-transcripts 7d --dry-run", "usage sync 7d --dry-run"} {
		var out, errb bytes.Buffer
		if code := run(strings.Fields(args), &out, &errb); code != 2 || !strings.Contains(errb.String(), `unexpected "7d"`) {
			t.Errorf("wp %s: %d %s", args, code, errb.String())
		}
	}
	for _, tc := range []struct {
		args string
		rest string
		on   bool
	}{
		{"--from-transcripts --all", "--all", true},
		{"--since 3d -from-transcripts=true", "--since 3d", true},
		{"--from-transcripts=false KEY", "KEY", false},
		{"KEY -- --from-transcripts", "KEY -- --from-transcripts", false},
	} {
		rest, on, err := takeBool(strings.Fields(tc.args), "from-transcripts")
		if err != nil || on != tc.on || strings.Join(rest, " ") != tc.rest {
			t.Errorf("takeBool(%q) = %q, %v, %v", tc.args, rest, on, err)
		}
	}
}

// An old name's help is the new command's, led by one line naming it.
func TestLegacyHelpNamesTheNewCommand(t *testing.T) {
	reportEnv(t)
	for _, tc := range []struct{ args, first, then string }{
		{"usage where -h", "wp usage where is now wp report usage:", "wp report usage "},
		{"burn -h", "wp burn is now wp report usage --session:", "wp report usage "},
		{"ai usage -h", "wp ai usage is now wp report ai:", "wp report ai "},
		{"usage -h", "wp usage is now wp record usage", "wp record usage "},
		{"usage sync -h", "wp usage sync is now wp record usage --from-transcripts:", "wp record usage --from-transcripts "},
		{"usage guard set -h", "wp usage guard set is now wp guard set:", "wp guard set "},
		{"ci history -h", "wp ci history is now wp report ci history:", "wp report ci history "},
		{"activity -h", "wp activity is now wp report activity:", "wp report activity "},
		{"mistakes -h", "wp mistakes is now wp report quality mistakes:", "wp report quality mistakes "},
		{"worklog stats -h", "wp worklog stats is now wp report quality reviews:", "wp report quality reviews "},
		{"log -h", "wp log is now wp record time:", "wp record time "},
		{"env deploy -h", "wp env deploy is now wp record deploy:", "wp record deploy "},
		{"usage meaning -h", "wp usage meaning is now wp report meaning:", "wp report meaning"},
	} {
		var out, errb bytes.Buffer
		if code := run(strings.Fields(tc.args), &out, &errb); code != 0 {
			t.Errorf("wp %s: %d %s", tc.args, code, errb.String())
			continue
		}
		lines := strings.Split(out.String(), "\n")
		if !strings.HasPrefix(lines[0], tc.first) || !strings.HasPrefix(lines[1], tc.then) {
			t.Errorf("wp %s starts %q", tc.args, lines[:2])
		}
	}
	var out bytes.Buffer
	run([]string{"report", "usage", "-h"}, &out, &out)
	if strings.Contains(out.String(), " is now ") {
		t.Errorf("a new name's help carries a note: %s", out.String())
	}
}

func TestReportAndBehaviorListTheirContents(t *testing.T) {
	reportEnv(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"report"}, []string{"wp report usage ", "wp report behavior ", "wp report ai ", "wp report ci ", "wp report activity ", "wp report quality ", "wp report meaning ", "wp report feature "}},
		{[]string{"record"}, []string{"wp record usage ", "wp record time ", "wp record deploy "}},
		{[]string{"report", "ci"}, []string{"wp report ci history ", "wp report ci line ", "wp report ci tests ", "wp report ci coverage ", "wp report ci suppressions "}},
		{[]string{"report", "quality"}, []string{"wp report quality mistakes ", "wp report quality escapes ", "wp report quality reviews ", "wp report quality triage ", "wp report quality stakes "}},
		{[]string{"report", "behavior"}, []string{"wp report behavior cli ", "wp report behavior risky ", "wp report behavior facts "}},
		{[]string{"report", "behaviour"}, []string{"wp report behavior cli "}},
	} {
		var out, errb bytes.Buffer
		if code := run(tc.args, &out, &errb); code != 0 {
			t.Errorf("wp %v: %d %s", tc.args, code, errb.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("wp %v lacks %q:\n%s", tc.args, w, out.String())
			}
		}
	}
	var out bytes.Buffer
	run([]string{"report", "behavior"}, &out, &out)
	if n := strings.Count(out.String(), "\n  wp report behavior "); n != len(behaviorTopics) {
		t.Errorf("wp report behavior lists %d topics, want %d", n, len(behaviorTopics))
	}
}

// Under wp report a clear near miss runs, with one stderr line; elsewhere it
// is only named; a word near nothing, or equally near two, lists the subjects.
func TestUnknownSubjectNearMisses(t *testing.T) {
	reportEnv(t)
	linkCommands()
	for _, tc := range []struct {
		word string
		subs []*command
		want string
	}{
		{"usagee", reportCmd.subs, "usage"},
		{"usgae", reportCmd.subs, "usage"},
		{"beh", reportCmd.subs, "behavior"},
		{"behavor", reportCmd.subs, "behavior"},
		{"riksy", reportBehaviorCmd.subs, "risky"},
		{"zzzzz", reportCmd.subs, ""},
		{"xy", reportCmd.subs, ""},
		{"stalx", []*command{{name: "stalla"}, {name: "stallb"}}, ""}, // equally near two
	} {
		got := ""
		if n := nearMiss(tc.word, tc.subs); n != nil {
			got = n.name
		}
		if got != tc.want {
			t.Errorf("nearMiss(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
	for _, tc := range []struct {
		args []string
		ran  string
	}{
		{[]string{"report", "usagee", "-h"}, "wp: \"usagee\" is not a report subject; ran wp report usage"},
		{[]string{"report", "beh", "-h"}, "wp: \"beh\" is not a report subject; ran wp report behavior"},
		{[]string{"report", "behavior", "riksy", "-h"}, "wp: \"riksy\" is not a report behavior topic; ran wp report behavior risky"},
	} {
		var out, errb bytes.Buffer
		run(tc.args, &out, &errb)
		if first, _, _ := strings.Cut(errb.String(), "\n"); first != tc.ran {
			t.Errorf("wp %v: stderr starts %q, want %q", tc.args, first, tc.ran)
		}
	}
	for _, tc := range []struct {
		args []string
		want string // the stderr's first line
	}{
		{[]string{"record", "usag"}, "wp: unknown record subject \"usag\"; the closest is wp record usage:"},
		{[]string{"report", "zzzzz"}, "wp: unknown report subject \"zzzzz\"; the subjects are:"},
		{[]string{"report", "xy"}, "wp: unknown report subject \"xy\"; the subjects are:"},
		{[]string{"report", "quality", "m"}, "wp: unknown report quality topic \"m\"; the topics are:"},
	} {
		var out, errb bytes.Buffer
		if code := run(tc.args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("wp %v: code %d, stdout %q", tc.args, code, out.String())
		}
		if first, _, _ := strings.Cut(errb.String(), "\n"); first != tc.want {
			t.Errorf("wp %v: stderr starts %q, want %q", tc.args, first, tc.want)
		}
	}
	// wp guard is the control: it never runs a guess.
	var out, errb bytes.Buffer
	run([]string{"guard", "sett"}, &out, &errb)
	if strings.Contains(errb.String(), "; ran wp") {
		t.Errorf("wp guard ran a guess: %s", errb.String())
	}
}

// wp report tasks and requests are named by the ADR but not built: one line, exit 2.
func TestReportNotBuilt(t *testing.T) {
	reportEnv(t)
	for _, tc := range []struct {
		args []string
		line string
	}{
		{[]string{"report", "tasks"}, "wp report tasks is not built yet; nearest now: wp ls, wp report activity\n"},
		{[]string{"report", "requests"}, "wp report requests is not built yet; nearest now: wp request ls --open\n"},
	} {
		var out, errb bytes.Buffer
		if code := run(tc.args, &out, &errb); code != 2 || out.Len() != 0 || errb.String() != tc.line {
			t.Errorf("wp %v: code %d, stdout %q, stderr %q", tc.args, code, out.String(), errb.String())
		}
	}
	var out bytes.Buffer
	run([]string{"report"}, &out, &out)
	if strings.Contains(out.String(), "report tasks") || strings.Contains(out.String(), "report requests") {
		t.Errorf("wp report lists reports that are not built:\n%s", out.String())
	}
}

// Stage 2 leaves no command defined under its old name: usageCmd is gone.
func TestStage2OldCommandsAreOffTheList(t *testing.T) {
	linkCommands()
	for _, name := range []string{"activity", "mistakes", "log", "time", "usage"} {
		if cmd := findCommand(name); cmd != nil {
			t.Errorf("wp %s is still a top-level command", name)
		}
	}
	if ci := findCommand("ci"); ci == nil || ci.sub("history") != nil || ci.sub("status") == nil || ci.sub("log") == nil || ci.sub("lint") == nil {
		t.Errorf("wp ci keeps status, log, lint and loses history")
	}
	if e := findCommand("escapes"); e == nil || e.sub("ls") == nil || e.sub("prove") == nil {
		t.Errorf("wp escapes keeps ls, show, trace and prove")
	}
}

func TestWindowAcceptsDaysAndSince(t *testing.T) {
	reportEnv(t)
	linkCommands()
	c, err := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	// --days on a report that takes --since
	c.cur, _ = resolveWords([]string{"report", "usage"})
	fs := c.flags()
	since := fs.String("since", "", "")
	if _, err := c.parse(fs, []string{"--days", "3"}); err != nil || *since != "3d" {
		t.Errorf("--days 3 on a --since report: %q, %v", *since, err)
	}
	// --since on a report that takes --days, rounded up to whole days
	c.cur, _ = resolveWords([]string{"report", "behavior", "stalled"})
	for _, tc := range []struct {
		since string
		days  int
	}{{"7d", 7}, {"12h", 1}, {"2w", 14}, {"5", 5}} {
		fs = c.flags()
		days := fs.Int("days", 28, "")
		if _, err := c.parse(fs, []string{"--since", tc.since}); err != nil || *days != tc.days {
			t.Errorf("--since %s on a --days report: %d, %v", tc.since, *days, err)
		}
	}
	// widening says so on stderr, once; whole days say nothing
	var werr bytes.Buffer
	wc := &cli{err: &werr, out: &bytes.Buffer{}, cur: c.cur}
	wfs := wc.flags()
	wfs.Int("days", 28, "")
	if _, err := wc.parse(wfs, []string{"--since", "12h"}); err != nil || werr.String() != "wp: this report counts whole days: --since 12h is read as the last 1 day\n" {
		t.Errorf("--since 12h: %q, %v", werr.String(), err)
	}
	werr.Reset()
	wfs = wc.flags()
	wfs.Int("days", 28, "")
	if _, err := wc.parse(wfs, []string{"--since", "3d"}); err != nil || werr.Len() != 0 {
		t.Errorf("--since 3d said %q", werr.String())
	}
	// no days is no window
	for _, v := range []string{"0", "-1", "NaN"} {
		c.cur, _ = resolveWords([]string{"report", "usage"})
		fs = c.flags()
		fs.String("since", "", "")
		if _, err := c.parse(fs, []string{"--days", v}); err == nil {
			t.Errorf("--days %s was taken", v)
		}
	}
	c.cur, _ = resolveWords([]string{"report", "behavior", "stalled"})
	// the default stays
	fs = c.flags()
	days := fs.Int("days", 28, "")
	if _, err := c.parse(fs, nil); err != nil || *days != 28 {
		t.Errorf("default window changed: %d", *days)
	}
}
