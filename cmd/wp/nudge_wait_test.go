package main

import (
	"strings"
	"testing"
)

// Sleep loops, from the calls agents made on 2026-11-25 and -26: a loop that
// waits and checks again is noted unless it is the one-call form
// (`timeout 590 ...`), ends within a few seconds, or only paces a list.
func TestSleepLoopRule(t *testing.T) {
	const (
		wait    = "sleep-wait"
		poll    = "sleep-poll"
		heredoc = "cat > /tmp/w.sh <<'EOF'\n#!/bin/bash\nwhile true; do curl -s localhost:1; sleep 3; done\nEOF\nchmod +x /tmp/w.sh"
	)
	for _, c := range []struct{ cmd, want string }{
		// A test slot: waiting on every `wp testsum` on the machine.
		{`while pgrep -f '[w]p testsum' >/dev/null; do sleep 5; done; cat /tmp/x/tasks/b5oezes7d.output`, wait},
		{`timeout 170 bash -c 'while pgrep -f "[w]p testsum -- make e2e ARGS=83" >/dev/null; do sleep 20; done'; echo waited`, wait},
		{`timeout 590 bash -c 'while pgrep -f "[w]p testsum" >/dev/null; do sleep 5; done; echo idle'`, wait},
		{`f=/tmp/x/tasks/bjetz6fgk.output; for i in $(seq 1 55); do if ! pgrep -f '[w]p testsum' >/dev/null; then break; fi; sleep 10; done; tail -20 $f`, wait},
		{`timeout 590 bash -c 'until ! wp slot ls 2>&1 | grep -q "go test ./internal/router"; do sleep 5; done'; echo done`, wait},
		// A process.
		{`while kill -0 2461407 2>/dev/null; do sleep 5; done; echo done`, wait},
		{`timeout 175 bash -c 'while pgrep -f "du -x -B1 --max-depth=3" >/dev/null; do sleep 5; done'; echo rc=$?`, wait},
		// A file or a line in a log.
		{`timeout 175 bash -c 'until [ -s /tmp/x/tasks/b3eccnec0.output ]; do sleep 5; done'; tail -30 /tmp/x/tasks/b3eccnec0.output`, wait},
		{`until [ "$(grep -c ^STEP /tmp/job7992-testci.log)" -ge 3 ]; do sleep 15; done; grep -E 'STEP|FAIL' /tmp/job7992-testci.log`, wait},
		{`timeout 120 bash -c 'until grep -q "closure computed" /tmp/x/run.log 2>/dev/null; do sleep 3; done'; cat /tmp/x/run.log`, wait},
		// A local server for longer than a few seconds.
		{`for i in $(seq 1 20); do c=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:21330/); [ "$c" != 000 ] && break; sleep 3; done; echo "http $c"`, wait},
		// A timeout on one check doesn't bound the loop.
		{`until timeout 5 nc -z db 5432; do sleep 10; done`, wait},
		// A forge: the existing rule's, and its audits (no wait command for them).
		{`until gh pr checks 12 | grep -q pass; do sleep 20; done`, poll},
		{`timeout 170 bash -c 'until wp audit ls 2>&1 | grep -q "open-688.*done"; do sleep 10; done'; wp audit ls 2>&1 | grep open-688`, wait},

		// The one-call form the notes recommend.
		{`timeout 590 bash -c 'until grep -q "MUT j" /tmp/x/mut.out; do sleep 5; done'; grep -c "=== MUT" /tmp/x/mut.out`, ""},
		{`cd /mnt/q5b && timeout 590 bash -c 'until [ -f sizes.done ]; do sleep 5; done'; ls -l sizes.done`, ""},
		{`timeout 580 bash -c 'while kill -0 2461407 2>/dev/null; do sleep 5; done'; echo done`, ""},
		{`timeout 590 bash -c 'until wp audit ls 2>/dev/null | grep -qi "open-685"; do sleep 15; done'`, ""},
		// A few seconds for a server to answer, in a script or a command.
		{`for i in $(seq 1 10); do curl -sf localhost:8080/health && break; sleep 2; done`, ""},
		{`timeout 20 bash -c 'until curl -sf localhost:3000; do sleep 1; done'; npm test`, ""},
		{`for i in {1..8}; do nc -z localhost 5432 && break; sleep 1; done`, ""},
		// A pause between items, lines read, a script only written, a wait already one call.
		{`for f in *.go; do gofmt -l $f; sleep 1; done`, ""},
		{`for u in a b c; do gofmt -l $u; sleep 5; done`, ""},
		{`while read l; do echo $l; sleep 1; done < file`, ""},
		{heredoc, ""},
		{`while true; do sleep 30; wp pr wait 12; done`, ""},
		{`python3 -c "import time; time.sleep(60)"`, ""},
		{`sleep 5 && echo done`, ""},
		// Not loops.
		{`pgrep -f 'wp testsum'`, ""},
		{`wp testsum -- go test ./cmd/wp -run 'TestNudgeRules'`, ""},
	} {
		got := ""
		if r := nudgeFor(c.cmd, func() string { return "git.example.net" }); r != nil {
			got = r.kind
		}
		if got != c.want {
			t.Errorf("%q: nudge %q, want %q", c.cmd, got, c.want)
		}
	}
}

// What the note says is a command that runs as printed for what the loop waits
// on, for each kind of caller.
func TestSleepLoopText(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		who  caller
		want []string
	}{
		{`timeout 175 bash -c 'while pgrep -f "du -x -B1 --max-depth=3" >/dev/null; do sleep 5; done'; echo rc=$?`, callerFg,
			[]string{"gives up after 175s", "`timeout 590 tail --pid=$(pgrep -of \"du -x -B1 --max-depth=3\") -f /dev/null`", "600000"}},
		{`while kill -0 2461407 2>/dev/null; do sleep 5; done`, callerBg,
			[]string{"run_in_background: true", "`timeout 590 tail --pid=2461407 -f /dev/null`", "end your turn"}},
		{`while kill -0 2461407 2>/dev/null; do sleep 5; done`, callerMain,
			[]string{"run_in_background: true", "tail --pid=2461407 -f /dev/null"}},
		{`while pgrep -f '[w]p testsum' >/dev/null; do sleep 5; done; cat out.txt`, callerMain,
			[]string{"queue for the slot themselves", "never on `pgrep -f 'wp testsum'`"}},
		{`(wp testsum -- go test ./cmd/wp -run Guard > out.txt 2>&1 &); while pgrep -f '[w]p testsum' >/dev/null; do sleep 10; done`, callerFg,
			[]string{"Run it as `wp testsum -- go test ./cmd/wp -run Guard`"}},
		{`timeout 175 bash -c 'until [ -s /tmp/x/b.output ]; do sleep 5; done'; tail /tmp/x/b.output`, callerFg,
			[]string{"gives up after 175s", "`timeout 590 bash -c 'until [ -s /tmp/x/b.output ]; do sleep 5; done'`", "600000"}},
		{`until grep -q "ready" /tmp/x/run.log; do sleep 15; done`, callerBg,
			[]string{"run_in_background", "`timeout 590 bash -c 'until grep -q \"ready\" /tmp/x/run.log; do sleep 5; done'`"}},
		{`until grep -q "ready" /tmp/x/run.log; do sleep 15; done`, callerMain,
			[]string{"Monitor command: `until grep -q \"ready\" /tmp/x/run.log; do sleep 5; done`"}},
		{`while pgrep -f x >/dev/null; do sleep 5; done`, callerMain,
			[]string{"tail --pid=$(pgrep -of x)"}},
		{`timeout 175 bash -c 'while [ ! -s /tmp/x/report.out ]; do sleep 5; done'; cat /tmp/x/report.out`, callerFg,
			[]string{"`timeout 590 bash -c 'until [ -s /tmp/x/report.out ]; do sleep 5; done'`"}},
		{`for i in $(seq 1 20); do c=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:21330/); [ "$c" != 000 ] && break; sleep 3; done`, callerFg,
			[]string{"gives up after 60s", "600000"}},
		{`(wp testsum -- go test ./cmd/wp -run 'Guard' 2>&1 | tail -5 &); until go vet ./cmd/wp >/dev/null 2>&1; do sleep 10; done`, callerFg,
			[]string{"Run it as `wp testsum -- go test ./cmd/wp -run 'Guard'`"}},
		// The run is cut out whole: a quoted pipe, a numeric last argument.
		{`(wp testsum -- go test ./cmd/wp -run 'TestA|TestB' 2>&1 | tail -5 &); until [ -s out ]; do sleep 10; done`, callerFg,
			[]string{"Run it as `wp testsum -- go test ./cmd/wp -run 'TestA|TestB'`"}},
		{`wp testsum -- go test ./x -count 1; until [ -s out ]; do sleep 10; done`, callerFg,
			[]string{"Run it as `wp testsum -- go test ./x -count 1`"}},
		{`timeout 100 bash -c 'until timeout 5 curl -sf db; do sleep 10; done'`, callerFg,
			[]string{"gives up after 100s"}},
		// A condition that reads a variable is not copied: it would not be set in the new shell.
		{`until [ -s $S/out ]; do sleep 15; done`, callerFg,
			[]string{"CHECK"}},
	} {
		got := sleepWaitText(c.cmd, c.who)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q (%v): note %q lacks %q", c.cmd, c.who, got, w)
			}
		}
	}
}

func TestLoopBoundSecs(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want float64
	}{
		{`for i in $(seq 1 55); do x; sleep 10; done`, 550},
		{`for i in {1..8}; do x; sleep 1; done`, 8},
		{`for ((i=0;i<20;i++)); do x; sleep 3; done`, 60},
		{`timeout 175 bash -c 'until x; do sleep 5; done'`, 175},
		{`timeout 2m bash -c 'until x; do sleep 5; done'`, 120},
		{`timeout 590 bash -c 'for i in $(seq 1 10); do x; sleep 2; done'`, 20},
		{`echo "waiting for it"; timeout 175 bash -c "until grep -q 'a;b' f; do sleep 5; done"`, 175},
		{`timeout 600 bash -c 'until timeout 5 x; do sleep 5; done'`, 600},
		{`for i in $(seq 1 30); do timeout 5 x && break; sleep 10; done`, 300},
		{`for f in a b; do x; sleep 1; done`, 0},
		{`while read l; do sleep 1; done < f`, 0},
	} {
		if got := loopBoundSecs(c.cmd); got != c.want {
			t.Errorf("%q: bound %v, want %v", c.cmd, got, c.want)
		}
	}
	for _, cmd := range []string{
		`until x; do sleep 5; done`,
		`until timeout 5 curl -sf x; do sleep 10; done`,
		`while true; do timeout 100 curl -sf x && break; sleep 10; done`,
		`while true; do r=$(timeout 100 curl -sf x); sleep 10; done`,
	} {
		if b := loopBoundSecs(cmd); b < 1e9 {
			t.Errorf("%q: unbounded loop bound %v", cmd, b)
		}
	}
}

func TestTestsumRun(t *testing.T) {
	for _, c := range []struct{ cmd, want string }{
		{`wp testsum -- go test ./x -run "A|B" > out`, `wp testsum -- go test ./x -run "A|B"`},
		{`wp testsum -- go test ./x -count 1`, `wp testsum -- go test ./x -count 1`},
		{`wp testsum -- go test ./x -count 1 >out`, `wp testsum -- go test ./x -count 1`},
		{`wp testsum -- go test ./x -count 1 2>&1`, `wp testsum -- go test ./x -count 1`},
		{`wp testsum -- go test $(go list ./... | grep -v e2e) | tail`, `wp testsum -- go test $(go list ./... | grep -v e2e)`},
		{"wp testsum -- go test \\\n./x # all of it", `wp testsum -- go test ./x`},
		{`timeout 90 bash -c 'wp testsum -- go test ./x'; echo 'done'`, `wp testsum -- go test ./x`},
		{`bash -c "wp testsum -- go test ./x -run 'A|B'"`, `wp testsum -- go test ./x -run 'A|B'`},
		{"# don't wait\nwp testsum -- go test ./x", `wp testsum -- go test ./x`},
		// Not cut out whole, so not quoted.
		{`bash -c 'wp testsum -- go test -run '\''A|B'\'''`, ""},
		{`bash -c "wp testsum -- go test -run \"A|B\""`, ""},
		{`wp testsum -- go test -run 'A`, ""},
		{"wp testsum -- go test `go list ./...`", ""},
		{`wp testsum -- > out`, ""},
		{`wp slot ls`, ""},
	} {
		if got := testsumRun(c.cmd); got != c.want {
			t.Errorf("%q: run %q, want %q", c.cmd, got, c.want)
		}
	}
}
