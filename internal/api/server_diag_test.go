package api

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/core/meaning"
)

// The log's reach is said as the server keeps it: days on disk, or memory
// since the last start (WP-893).
func TestServerLogTextReach(t *testing.T) {
	since := time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)
	oldest := since.Add(2 * time.Hour)
	l := &core.ServerLog{Records: []core.LogRecord{{At: oldest, Level: "INFO", Msg: "merge hold", Attrs: "pr=me/app#377"}}, Matched: 1, Since: &oldest}
	if got := serverLogText(l, since); !strings.Contains(got, "11-11 09:00:00 INFO  merge hold pr=me/app#377") || !strings.Contains(got, "goes back to 11-11 09:00 UTC only (it keeps 48h, or 50,000 records, since the last start)") {
		t.Errorf("memory only:\n%s", got)
	}
	l.KeepDays = 30
	if got := serverLogText(l, since); !strings.Contains(got, "(it keeps 30 days)") {
		t.Errorf("with files:\n%s", got)
	}
}

// wp server status says what each scheduled job last did and which
// problems persist (WP-ADR139).
func TestServerStatusTextJobsAndProblems(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	st := &core.ServerStatus{Version: "x", Started: now.Add(-time.Hour), Jobs: []core.JobStatus{
		{Name: "forge sync", PeriodMS: 600_000, Runs: 144, LastStart: ago(4 * time.Minute), LastEnd: ago(3 * time.Minute), LastMS: 61_000, MaxMS: 95_000, LastResult: "warned", LastNote: "repo sync failed", Skips: 2},
		{Name: "autopilot", PeriodMS: 60_000, Runs: 1440, LastStart: ago(30 * time.Second), LastEnd: ago(30 * time.Second), LastMS: 12, MaxMS: 800, LastResult: "clean"},
		{Name: "CI history", PeriodMS: 600_000, Runs: 9, Running: true, RunningSince: ago(2 * time.Hour), LastStart: ago(3 * time.Hour), LastEnd: ago(3 * time.Hour), LastMS: 500, MaxMS: 500, LastResult: "clean", State: "stuck"},
		{Name: "pull requests", Runs: 0},
	}, Problems: []core.JobProblem{
		{ID: 7, Source: "forge sync", Message: "repo sync failed", Target: "acme/app", Level: "warn", StreakCount: 290, StreakSince: now.Add(-48 * time.Hour), StreakUpMS: 48 * 3600000, LastAt: now.Add(-3 * time.Minute), Persistent: true},
		{ID: 8, Source: "forge sync", Message: "repo sync failed", Target: "acme/old", Level: "warn", StreakCount: 290, StreakSince: now.Add(-48 * time.Hour), StreakUpMS: 48 * 3600000, LastAt: now.Add(-3 * time.Minute), Persistent: true, AckedAt: ago(time.Hour), AckedBy: "robin", AckReason: "archived"},
		{ID: 9, Source: "requests", Message: "slow request", Level: "warn", StreakCount: 1, StreakSince: now.Add(-time.Minute), LastAt: now.Add(-time.Minute)},
	}, ProblemsOpen: 3, ProblemsPersistent: 2}
	out := serverStatusText(st, now)
	for _, want := range []string{
		"Problems: 2 persistent and unacknowledged, of 3 open (wp server problems):",
		"  #7 warn  forge sync: repo sync failed (acme/app); 290× over 2d0h (since 11-18 12:00), last 3m ago; persistent\n",
		"  and 1 more\n",
		"Scheduled jobs (every, runs, last run, took, longest, last result; wp server job NAME for one over time):",
		"  forge sync    10m      144  4m ago     1m           1m     warned: repo sync failed (2 skipped)\n",
		"  autopilot     1m      1440  30s ago    12ms         800ms\n",
		"  CI history    10m        9  3h00m ago  running 2h00m 500ms  STUCK\n",
		"  pull requests start      0  -          -            0ms\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"#8", "#9", "slow request"} {
		if strings.Contains(out, not) {
			t.Errorf("%q is not for the status:\n%s", not, out)
		}
	}
	st.ProblemsPersistent, st.Problems = 0, st.Problems[2:]
	if out := serverStatusText(st, now); !strings.Contains(out, "Problems: 3 open, none persistent and unacknowledged (wp server problems).\n") || strings.Contains(out, "#9") {
		t.Errorf("no persistent problem:\n%s", out)
	}
	st.ProblemsOpen, st.Problems = 0, nil
	if out := serverStatusText(st, now); strings.Contains(out, "Problems") {
		t.Errorf("no problem at all:\n%s", out)
	}
}

func TestServerProblemsText(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	out := serverProblemsText([]core.JobProblem{
		{ID: 7, Source: "forge sync", Message: "repo sync failed", Target: "acme/app", Level: "warn", StreakCount: 290, StreakSince: now.Add(-48 * time.Hour), StreakUpMS: 48 * 3600000, LastAt: now.Add(-3 * time.Minute), Persistent: true, Detail: "repo=acme/app err=404 Not Found"},
		{ID: 8, Source: "events", Message: "push failed", Level: "error", StreakCount: 4, StreakSince: now.Add(-30 * time.Hour), StreakUpMS: 30 * 3600000, LastAt: now.Add(-time.Hour), Persistent: true, AckedAt: at(time.Hour), AckedBy: "robin", AckReason: "the phone is off"},
		{ID: 9, Source: "hourly", Message: "prune backup", Level: "warn", StreakCount: 2, StreakSince: now.Add(-72 * time.Hour), StreakUpMS: 72 * 3600000, LastAt: now.Add(-50 * time.Hour), ClearedAt: at(25 * time.Hour)},
	}, true, now)
	for _, want := range []string{
		"#7 warn  forge sync: repo sync failed (acme/app); 290× over 2d0h (since 11-18 12:00), last 3m ago; persistent\n    repo=acme/app err=404 Not Found\n",
		"#8 error events: push failed; 4× over 1d6h (since 11-19 06:00), last 1h00m ago; acknowledged by robin: the phone is off\n",
		"#9 warn  hourly: prune backup; 2× over 3d0h (since 11-17 12:00), last 2d2h ago; over 1d1h ago\n",
		"wp server ack N -m WHY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if out := serverProblemsText(nil, false, now); !strings.Contains(out, "Nothing is going wrong") || !strings.Contains(out, "--all") {
		t.Errorf("none open: %s", out)
	}
	if out := serverProblemsText(nil, true, now); out != "No problems are recorded.\n" {
		t.Errorf("none at all: %s", out)
	}
}

func TestServerJobText(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	h := &core.JobHistory{Source: "forge sync", By: "hour", Since: now.Add(-24 * time.Hour),
		Job:   &core.JobStatus{Name: "forge sync", PeriodMS: 600_000, Runs: 144, Skips: 2, LastStart: at(4 * time.Minute), LastEnd: at(3 * time.Minute), LastMS: 61_000, LastResult: "warned", LastNote: "repo sync failed", LastCleanAt: at(50 * time.Hour)},
		Spans: []core.JobSpan{{From: now.Add(-2 * time.Hour), Runs: 6, Unclean: 6, Skips: 1, Warnings: 12, Errors: 0, Noted: 3, AvgMS: 61_000, MaxMS: 95_000}},
		Problems: []core.JobProblem{
			{ID: 7, Source: "forge sync", Message: "repo sync failed", Target: "acme/app", Level: "warn", StreakCount: 290, StreakSince: now.Add(-48 * time.Hour), StreakUpMS: 48 * 3600000, LastAt: now.Add(-3 * time.Minute), Persistent: true},
		}}
	out := serverJobText(h, now)
	for _, want := range []string{
		"forge sync: a scheduled job, every 10m; 144 runs, 2 skipped\n",
		"Last run 4m ago, took 1m: warned (repo sync failed). Last clean run 2d2h ago.\n",
		"By hour (UTC) since 11-19 12:00: runs, not clean, skipped, warnings, errors, noted (failures logged below a warning or of work that was called off), average, longest\n",
		"  11-20 10:00      6     6     1     12      0      3  1m      1m\n",
		"Problems, past ones too:\n  #7 warn  forge sync: repo sync failed (acme/app)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	h.Job.RunningSince, h.Job.State, h.Job.LastCleanAt = at(2*time.Hour), "stuck", nil
	if out := serverJobText(h, now); !strings.Contains(out, "Running for 2h00m. No run of it has been clean. STUCK: this run has gone on too long.\n") {
		t.Errorf("stuck:\n%s", out)
	}
	h.Job = &core.JobStatus{Name: "pull requests", State: "silent"}
	h.Spans, h.By = nil, "day"
	if out := serverJobText(h, now); !strings.Contains(out, "pull requests: a scheduled job, once, when the server starts; 0 runs\nIt has not run. SILENT: it should have started and hasn't.\n") || !strings.Contains(out, "Nothing counted since 11-19 12:00.") {
		t.Errorf("silent, at startup:\n%s", out)
	}
	h.Job, h.Source = nil, "requests"
	if out := serverJobText(h, now); !strings.HasPrefix(out, "requests: what is logged there, not a scheduled job\n") {
		t.Errorf("a source:\n%s", out)
	}
}

// wp server status says how the run before ended and whether the health
// check is alive.
func TestServerStatusTextRunsAndHealthCheck(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	st := &core.ServerStatus{Version: "x", Started: now.Add(-time.Hour), HealthCheckedAt: at(20 * time.Second),
		Previous: &core.PreviousRun{LastAlive: now.Add(-time.Hour - 40*time.Second), Clean: true, AwayS: 40}}
	out := serverStatusText(st, now)
	for _, want := range []string{"Before this start: stopped cleanly 10:59, away 40s.\n", "Job health checked 20s ago.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	st.Previous.Clean, st.Previous.AwayS, st.HealthCheckedAt = false, 180, at(14*time.Minute)
	out = serverStatusText(st, now)
	for _, want := range []string{"Before this start: DID NOT SHUT DOWN (last alive 10:59), away 3m.\n", "Job health NOT CHECKED for 14m: the check itself is stuck (wp server log).\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	st.Previous, st.HealthCheckedAt = nil, nil
	if out = serverStatusText(st, now); strings.Contains(out, "Before this start") || strings.Contains(out, "Job health") {
		t.Errorf("nothing known:\n%s", out)
	}
}

// wp server status says whether the meaning service answers (WP-1602).
func TestServerStatusTextMeaning(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := &core.ServerStatus{Version: "x", Started: now.Add(-time.Hour)}
	if out := serverStatusText(st, now); strings.Contains(out, "Meaning service") {
		t.Errorf("none configured:\n%s", out)
	}
	since, ok, retry := now.Add(-2*24*time.Hour), now.Add(-50*time.Hour), now.Add(20*time.Minute)
	st.Meaning = &meaning.Health{State: "down", DownSince: &since, LastOK: &ok, Failures: 7, RetryAt: &retry, LastError: "context deadline exceeded"}
	out := serverStatusText(st, now)
	want := "Meaning service: DOWN since 11-18 12:00 UTC (2d0h ago), 7 calls unanswered in a row; next try 12:20 UTC; last answered 11-18 10:00 UTC (2d2h ago)\n  context deadline exceeded\n"
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
	st.Meaning = &meaning.Health{State: "up", LastOK: &ok}
	if out := serverStatusText(st, now); !strings.Contains(out, "Meaning service: answering, last answered") {
		t.Errorf("up:\n%s", out)
	}
}

// wp server status lists hook data the collection dropped, by person (WP-1468).
func TestServerStatusTextHookDrops(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := &core.ServerStatus{Version: "x", Started: now.Add(-time.Hour)}
	if out := serverStatusText(st, now); strings.Contains(out, "Hook data dropped") {
		t.Errorf("nothing dropped:\n%s", out)
	}
	st.HookDrops = []core.HookDrop{{User: core.UserRef{ID: 2, Username: "ana"}, Machine: "box", Reason: "overflow", Event: "UserPromptSubmit", Dropped: 5, Last: now.Add(-3 * time.Hour)}}
	out := serverStatusText(st, now)
	for _, want := range []string{
		"\nHook data dropped (person, machine, reason/event, count, last):\n",
		"  ana          box              overflow/UserPromptSubmit               5  3h00m ago\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
