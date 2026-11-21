package core

// Runner faults (WP-230): a CI job that failed because of the machine it ran
// on, not the code. On 2026-10-28 a runner ran out of disk and jobs failed with
// "no space left on device"; a commit pushed to re-run CI only put more
// load on the full runner. A failed job's log says so in words a pattern
// can find; then no agent round is spent on it (the babysitter waits, fix_ci
// skips) and Waypoint re-runs the jobs on a schedule (ci_fault_retry.go, WP-838).
// A fault that outlasts the first re-runs is tracked in one task
// (ci_fault_task.go). `wp pr log`, `wp pr wait` and the work packet say it too,
// and the CI board lists the recent ones.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// RunnerFault is what a job's log says went wrong with its runner.
type RunnerFault struct {
	What   string `json:"what"`             // in words: "the runner ran out of disk space"
	Kind   string `json:"kind,omitempty"`   // disk, docker, lost, timeout, dns, service, network, time_limit
	Line   string `json:"line"`             // the log line that says so
	Runner string `json:"runner,omitempty"` // the runner or its host, when known
	at     int    // the line's index in the cleaned log
	task   int64  // the forge's task id, when the log gives it: finds the host (nameFaultHost)
}

// The kinds of runner fault. They differ in what ends them: a full disk ends
// when the host is cleaned, a lost runner or a Docker or DNS hiccup with time.
// faultTimeLimit is the job running out of its own timeout-minutes: only a
// slower host than usual is the runner's, so re-runs stop soon (ci_fault_retry.go).
const (
	faultDisk      = "disk"
	faultDocker    = "docker"
	faultLost      = "lost"
	faultTimeout   = "timeout"
	faultDNS       = "dns"
	faultTimeLimit = "time_limit"
	faultService   = "service"
	faultNetwork   = "network"
)

// runnerFaultRules, checked in order. Only words a runner or Docker says:
// "context deadline exceeded" alone is as often a test's own timeout.
var runnerFaultRules = []struct {
	re         *lazyre.Regexp
	kind, what string
}{
	{lazyre.New(`(?i)no space left on device|\bENOSPC\b|disk quota exceeded`), faultDisk, "the runner ran out of disk space"},
	{lazyre.New(`(?i)cannot connect to the docker daemon`), faultDocker, "the runner's Docker daemon wasn't reachable"},
	{lazyre.New(`(?i)runner has received a shutdown signal|lost communication with the server|job failed \(system failure\)`), faultLost, "the runner was lost during the job"},
	// The job's deadline passed, so the runner's next step, saving the job's
	// summary, failed with it. One project's backend, 2026-11-10: three attempts
	// of #198 ended at 910s, 906s and 908s against a 15-minute limit, each
	// read as Docker timing out. Before the rule below, which it also matches.
	{lazyre.New(`(?i)\[runner\]:?\s*context deadline exceeded.*unable to copy "?/var/run/act/workflow/`), faultTimeLimit, "the job hit its workflow's time limit (timeout-minutes)"},
	{lazyre.New(`(?i)(?:\[runner\]|error response from daemon|failed to (?:pull|fetch|start|create|remove)\b).*context deadline exceeded`), faultTimeout, "the runner timed out talking to Docker or a registry"},
	// Only the runner's own line (fetching an action, pulling an image): a
	// test that can't resolve a host is the code's (WP-425).
	{lazyre.New(`(?i)\[runner\]:.*(?:could not resolve host|temporary failure in name resolution|no such host|server misbehaving)`), faultDNS, "the runner couldn't resolve a host name (DNS)"},
	// The runner's resolver failing, not a name the code got wrong ("no
	// such host" is left out: a test's made-up host says that too). govulncheck
	// on a runner, 2026-10-29/30: "lookup vuln.go.dev on 127.0.0.11:53: server
	// misbehaving" (WP-392).
	{lazyre.New(`(?i)\blookup \S+ on \S+: (?:server misbehaving|read udp \S+ i/o timeout)|temporary failure in name resolution|could not resolve host`), faultDNS, "the runner couldn't look up a host name (DNS)"},
	// The Forgejo runner's own line when a job's service container (a
	// database, a broker) never passed its health check: the job stops
	// before any of the repo's steps run. A managed repo, 2026-11-08..15: its
	// rabbitmq service did this; babysits spent rounds on it and parked
	// (#210, #214), and a dependency update sat failed for two days (#223)
	// (WP-1042).
	{lazyre.New(`(?i)\bservice \[[^\]]+\]: container health check \S+ \([^)]*\) is not healthy`), faultService, "a CI service container never became healthy"},
	// A Docker build's download cut off mid-transfer: BuildKit's step output
	// ("#11 75.69 curl: (18) ..."), where no server of the repo's own is
	// running, so the far end or the network between failed. curl 18 partial
	// transfer, 35 TLS handshake, 56 receive failure, 92 HTTP/2 stream error;
	// not 22 (an HTTP error status, as for a wrong URL). A managed repo's #223,
	// 2026-11-15: the PostgreSQL apt key fetch got no bytes in 69s.
	{lazyre.New(`^#\d+ [\d.]+ curl: \((?:18|35|56|92)\) `), faultNetwork, "a download in the Docker build was cut off (network)"},
}

// passedTestRE is a line reporting a test that passed. What it says is the
// test's own text: infrastructure's "one `Could not resolve host: github.com`
// paged urgent ... ok" is no DNS fault (WP-784).
var passedTestRE = lazyre.New(`\.\.\. ok\s*$|(?i)\bPASSED(?:\s+\[\s*\d+%\])?\s*$|^\s*(?:--- PASS:|PASSED\s)`)

// derivedFailureRE is a job that fails only to say other jobs failed: the
// "every shard passed" jobs a sharded workflow is required by, whose log
// says "the backend shards ended as 'failure'". Its own log has no fault;
// the shards' logs say what happened.
var derivedFailureRE = lazyre.New(`(?i)\bended as '(?:failure|cancelled)'`)

// derivedFailure says whether a failed job's log only reports other jobs'
// failure.
func derivedFailure(log string) bool { return log != "" && derivedFailureRE.MatchString(log) }

// runnerNameRE is the first line of a Forgejo runner's job log, which names
// the runner in older versions ("runner2(version:v6.3.1) received task 4321 of
// job backend, …") and only by UUID in newer ones ("Runner 3162…(version:
// v13.2.0) received task 29485 of job backend, …"), where the task finds the
// host (nameFaultHost).
var runnerNameRE = lazyre.New(`^(?:Runner \S+ ?|(\S+))\(version:[^)]*\) received task (\d+)`)

// findRunnerFault finds the first runner fault in a cleaned log, or nil.
func findRunnerFault(lines []string) *RunnerFault {
	var f *RunnerFault
	runner, task := "", int64(0)
	for i, l := range lines {
		if task == 0 {
			if m := runnerNameRE.FindStringSubmatch(l); m != nil {
				runner = m[1]
				task, _ = strconv.ParseInt(m[2], 10, 64)
			}
		}
		if f != nil || passedTestRE.MatchString(l) {
			continue
		}
		for _, r := range runnerFaultRules {
			if r.re.MatchString(l) {
				f = &RunnerFault{What: r.what, Kind: r.kind, Line: cutLine(strings.TrimSpace(l)), at: i}
				break
			}
		}
	}
	if f != nil {
		f.Runner, f.task = runner, task
	}
	return f
}

// nameFaultHost names the host of a fault whose log gave only the runner's
// UUID, from what the hosts report about the task they ran (ci_job_usage).
func (s *Service) nameFaultHost(ctx context.Context, forgeID int64, f *RunnerFault) {
	if f == nil || f.Runner != "" || f.task == 0 {
		return
	}
	_ = s.db.R.QueryRowContext(ctx, `SELECT host FROM ci_job_usage WHERE forge_id = ? AND task_id = ? AND role = 'job' ORDER BY started_at DESC LIMIT 1`,
		forgeID, f.task).Scan(&f.Runner)
}

// logRunnerFault is the runner fault a job's log (raw or kept) shows, or nil.
func logRunnerFault(log string) *RunnerFault { return findRunnerFault(cleanLog(log)) }

// Words returns the fault as a clause: "the runner ran out of disk space (runner1)".
func (f *RunnerFault) Words() string {
	if f.Runner != "" {
		return f.What + " (" + f.Runner + ")"
	}
	return f.What
}

// runnerFaultAdvice is what anyone reading a runner fault should know.
const runnerFaultAdvice = "a CI runner problem, not the code: pushing again or re-running it now only loads the runner. Waypoint re-runs the failed jobs itself, on a schedule that backs off, once the runner is fit."

// timeLimitAdvice is the same for a job that ran out of its own time.
const timeLimitAdvice = "the job's own time limit, usually a slower runner than normal or a suite that outgrew it. Waypoint re-runs it a few times; if it keeps happening, raise the workflow's timeout-minutes or make the job faster."

// Advice is what anyone reading this fault should know.
func (f *RunnerFault) Advice() string {
	if f.Kind == faultTimeLimit {
		return timeLimitAdvice
	}
	return runnerFaultAdvice
}

// headRunnerFaults are the runner faults of the failed checks on pr's head,
// by check, when every one of them failed that way (as the log Waypoint kept
// says); nil when any failed for another reason, or has no log kept.
func (s *Service) headRunnerFaults(ctx context.Context, pr *PullRequest) map[string]*RunnerFault {
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ? AND c.state = 'failed'`, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil
	}
	out := map[string]*RunnerFault{}
	ok := func() bool {
		defer rows.Close()
		for rows.Next() {
			var name, log string
			if err := rows.Scan(&name, &log); err != nil {
				return false
			}
			f := logRunnerFault(log)
			if f == nil {
				if derivedFailure(log) {
					continue // it failed because a shard did; the shard's log says why
				}
				return false
			}
			out[name] = f
		}
		return rows.Err() == nil
	}()
	if !ok || len(out) == 0 {
		return nil
	}
	// Named once the rows are closed: a query while they are open holds a
	// second reader, and with every reader held so, none is left for it.
	for _, f := range out {
		s.nameFaultHost(ctx, pr.ForgeID, f)
	}
	return out
}

// faultSummary: "ci / backend: the runner ran out of disk space (runner1)".
func faultSummary(faults map[string]*RunnerFault) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(faults)) {
		parts = append(parts, name+": "+faults[name].Words())
	}
	return strings.Join(parts, "; ")
}

// runnerFaultRequestKey keys the one open request about a kind of runner
// fault on a forge, so pull requests failing the same way share it.
const runnerFaultRequestKey = "auto:runner-fault:"

// faultSubject is where a runner fault was first seen: a pull request's
// head, or a base branch's push run (WP-551).
type faultSubject struct {
	forgeID int64
	repo    string
	name    string // "me/app#7", "me/app main run #2140"
	url     string
	agent   string // who files the request
}

func prFaultSubject(pr *PullRequest) faultSubject {
	return faultSubject{forgeID: pr.ForgeID, repo: pr.Repo, name: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), url: pr.URL, agent: "waypoint/babysit"}
}

// askAboutRunner files one request for a person to fix the runner, unless
// one about the same fault on the same forge is open for them already: only
// when the fault can't be filed as a task for agents (reportRunnerFault). It
// says whether a request is open for them now.
func (s *Service) askAboutRunner(ctx context.Context, userID int64, at faultSubject, faults map[string]*RunnerFault, why string) bool {
	names := slices.Sorted(maps.Keys(faults))
	f := faults[names[0]]
	key := runnerFaultKey(at.forgeID, f)
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? AND user_id = ? AND state = ?`, key, userID, RequestOpen).Scan(&id) == nil {
		return true
	}
	u, err := s.userByID(ctx, s.db.R, userID)
	if err != nil || u.Disabled {
		return false
	}
	where := "a CI runner"
	if f.Runner != "" {
		where = "CI runner " + f.Runner
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CI jobs fail because %s, not because of their code. First seen on %s (%s):\n\n", f.What, at.name, at.url)
	for _, n := range names {
		fmt.Fprintf(&b, "- %s: `%s`\n", n, strings.ReplaceAll(faults[n].Line, "`", "'"))
	}
	b.WriteString("\n" + why + " Waypoint starts no agent round for these: babysat pull requests wait, keeping their place in the merge queue, and it keeps re-running them while it can. " +
		"The pull requests carry on when their checks pass. The CI board (/ci) lists the jobs that failed this way, and the hosts' health. Mark this done when the runner is fit again.")
	in := RequestInput{Kind: RequestAction, Title: fmt.Sprintf("Fix %s: %s", where, f.What), Body: b.String(), ToolUseID: key, Followup: "never"}
	// Asked all the same: a project that couldn't be read files it with none.
	projectKey, err := s.attachedProjectKey(ctx, at.forgeID, at.repo)
	if readFault(err) {
		s.log.WarnContext(ctx, "read the project to ask about a CI runner", "at", at.name, "repo", at.repo, "err", err)
	}
	in.Project = projectKey
	if _, _, err := s.CreateRequest(ctx, &Actor{User: u, Agent: at.agent}, in); err != nil {
		s.log.WarnContext(ctx, "ask a person to fix a CI runner", "at", at.name, "err", err)
		return false
	}
	return true
}

// rerunRefused says why Waypoint can't ask f to re-run CI jobs, or "".
func (s *Service) rerunRefused(ctx context.Context, f *Forge, cl interface {
	CanRetryChecks(ctx context.Context, f *Forge) (bool, error)
}) string {
	if cl == nil || f.token == "" {
		return "Waypoint can't ask this forge to re-run them."
	}
	switch can, err := cl.CanRetryChecks(ctx, f); {
	case err != nil:
		return "Waypoint couldn't ask the forge whether it can re-run them (" + err.Error() + ")."
	case !can:
		return "Waypoint can't re-run them itself: this Forgejo has no API for it (Forgejo 17 has; once it is upgraded, Waypoint re-runs such jobs by itself)."
	}
	return ""
}

// faultHosts says whether runner-fault jobs first seen since ago (or last
// re-run) may be re-run now. sick names a host not fit yet, which is waited
// for, as is one that doesn't report for faultRerunAfter. report, once a
// host has been sick faultSickFor, is why it is worth tracking; the wait goes
// on either way (WP-838).
func (s *Service) faultHosts(ctx context.Context, forgeID int64, faults map[string]*RunnerFault, since time.Duration) (sick string, wait bool, report string) {
	unknown := false
	for _, name := range slices.Sorted(maps.Keys(faults)) {
		r := faults[name].Runner
		switch h, why := s.runnerHealth(ctx, forgeID, r); h {
		case "low_disk", "stale":
			if sick == "" {
				sick = r + ": " + why
			}
		case "unknown":
			unknown = true
		}
	}
	switch {
	case sick != "" && since >= faultSickFor:
		return sick, true, fmt.Sprintf("Its host is still not fit to re-run them %s on (%s).", ShortDuration(since), sick)
	case sick != "":
		return sick, true, ""
	case unknown && since < faultRerunAfter:
		return "", true, ""
	}
	return "", false, ""
}

// ---- recent faults, for the CI board ----

const (
	runnerFaultWindow = 2 * time.Hour // the CI board shows faults this recent
	runnerFaultKeep   = 100
)

// seenFault is a job that failed on its runner, when Waypoint first read
// its log.
type seenFault struct {
	at      time.Time
	forgeID int64
	repo    string
	check   string
	url     string
	fault   RunnerFault
}

// runnerFaultLog remembers the recent runner faults (in memory: a restart
// forgets them, and the board only needs the last hours).
type runnerFaultLog struct {
	mu   sync.Mutex
	list []seenFault
}

func (l *runnerFaultLog) add(f seenFault) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, f)
	if n := len(l.list); n > runnerFaultKeep {
		l.list = slices.Clone(l.list[n-runnerFaultKeep:])
	}
}

func (l *runnerFaultLog) since(t time.Time) []seenFault {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []seenFault
	for _, f := range l.list {
		if f.at.After(t) {
			out = append(out, f)
		}
	}
	return out
}

// itsFault: "the runner ran out of disk space" as "it ran out of disk space".
func itsFault(what string) string {
	return strings.NewReplacer("the runner's ", "its ", "the runner ", "it ").Replace(what)
}

// runnerFaultAlerts are the CI board's alerts for recent runner faults, one
// per runner and fault; jobs of repos the viewer can't see are counted, not
// named.
func runnerFaultAlerts(faults []seenFault, visible func(forgeID int64, repo string) bool) []CIAlert {
	type group struct {
		runner, what string
		n            int
		jobs         []string
		url          string
	}
	var order []string
	groups := map[string]*group{}
	for _, f := range faults {
		k := f.fault.Runner + "\x00" + f.fault.What
		g := groups[k]
		if g == nil {
			g = &group{runner: f.fault.Runner, what: f.fault.What}
			groups[k] = g
			order = append(order, k)
		}
		g.n++
		if visible(f.forgeID, f.repo) {
			g.jobs = append(g.jobs, f.repo+" "+f.check)
			g.url = f.url
		}
	}
	out := []CIAlert{}
	for _, k := range order {
		g := groups[k]
		who := "A runner"
		if g.runner != "" {
			who = "Runner " + g.runner
		}
		body := "Waypoint starts no agent round for these; it re-runs them itself, backing off, once the runner is fit."
		if len(g.jobs) > 0 {
			if len(g.jobs) > 5 {
				g.jobs = append(g.jobs[:5], fmt.Sprintf("%d more", len(g.jobs)-5))
			}
			body = "Failed: " + strings.Join(g.jobs, ", ") + ". " + body
		}
		out = append(out, CIAlert{Level: "error", Title: fmt.Sprintf("%s: %d job%s failed in the last %d hours because %s", who, g.n, plural(g.n), int(runnerFaultWindow.Hours()), itsFault(g.what)),
			Body: body, URL: g.url, key: "fault:" + g.runner + ":" + g.what})
	}
	return out
}
