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
		{"usage meaning", "usage meaning"},
		{"usage feature WP-1", "usage feature WP-1"},
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
	for _, old := range []string{"  usage ", "  burn ", "  ai "} {
		if strings.Contains(list, old) {
			t.Errorf("wp help lists the old name %q", old)
		}
	}
	out.Reset()
	if code := run([]string{"help", "--all"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	all := out.String()
	for _, old := range []string{"\n## wp burn\n", "\n## wp ai\n", "\n## wp usage sync\n", "\n## wp usage where\n", "\n## wp usage guard\n"} {
		if strings.Contains(all, old) {
			t.Errorf("wp help --all documents the old name %q", strings.TrimSpace(old))
		}
	}
	for _, want := range []string{"\n## wp report usage\n", "\n## wp report behavior cli\n", "\n## wp report ai surplus\n", "\n## wp record usage\n", "\n## wp guard set\n", "\n## wp transcript\n", "\n## wp setup otel\n", "\n## wp usage meaning\n"} {
		if !strings.Contains(all, want) {
			t.Errorf("wp help --all lacks %q", strings.TrimSpace(want))
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
		{[]string{"report"}, []string{"wp report usage ", "wp report behavior ", "wp report ai "}},
		{[]string{"record"}, []string{"wp record usage "}},
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

func TestUnknownSubjectListsClosestFirst(t *testing.T) {
	reportEnv(t)
	for _, tc := range []struct {
		args    []string
		closest string
	}{
		{[]string{"report", "usgae"}, "wp report usage"},
		{[]string{"report", "behavor", "x"}, "wp report behavior"},
		{[]string{"report", "behavior", "rsky"}, "wp report behavior risky"},
	} {
		var out, errb bytes.Buffer
		if code := run(tc.args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("wp %v: code %d, stdout %q", tc.args, code, out.String())
		}
		lines := strings.Split(errb.String(), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "wp: unknown ") || !strings.HasPrefix(strings.TrimSpace(lines[1]), tc.closest+" ") {
			t.Errorf("wp %v: %q", tc.args, lines)
		}
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
	// the default stays
	fs = c.flags()
	days := fs.Int("days", 28, "")
	if _, err := c.parse(fs, nil); err != nil || *days != 28 {
		t.Errorf("default window changed: %d", *days)
	}
}
