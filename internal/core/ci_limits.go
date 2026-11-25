package core

// CI time limits (WP-922). On 2026-11-11 the merge queue backed up because
// internal/core's tests on backend shard 1 had crept up to go test's 30m
// -timeout (an O(n²) mention sync, WP-921): each front's full run timed out
// after half an hour, got a flake re-run and then an agent round about code
// that wasn't its own. Nothing said the package was close to its limit
// until it was past it. So Waypoint keeps how long each Go package took in
// each job (from the job's log, with its test evidence), reads the limits
// from the repo's workflows (each job's timeout-minutes and the -timeout of
// the go test its steps run), and finds the jobs and packages that run close
// to them: the CI doctor shows them, and an hourly check files one task per
// repo for an agent, with the numbers and the slowest tests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	// ciLimitWindow is how far back a check of the limits looks.
	ciLimitWindow = 24 * time.Hour
	// ciLimitEvery is how often a repo's limits are checked for its task.
	ciLimitEvery = time.Hour
	// ciLimitRetell: a task still close to its limits gets a comment this often.
	ciLimitRetell = 24 * time.Hour
	// A group is close to its limit when its p90 reaches ciLimitP90 of it
	// or its slowest run ciLimitMax.
	ciLimitP90 = 0.85
	ciLimitMax = 0.95
	// workflowLimitTTL is how long a workflow's limits are kept read.
	workflowLimitTTL = time.Hour
)

// ---- package times from job logs ----

// jobPackage is one Go package's run in a job, as go test reported it.
type jobPackage struct {
	pkg     string
	outcome string // ok, fail, timeout
	ms      int64
	limitMs int64    // timeout: the -timeout that fired
	running []string // timeout: the tests it was running then
}

var (
	// "ok  \tgit.example.com/me/app/internal/core\t301.376s\tcoverage: …",
	// "FAIL\tgit.example.com/me/app/internal/core\t1803.468s"; not "(cached)"
	// or "[build failed]".
	goPackageRE = lazyre.New(`^(ok|FAIL)\s+(\S+)\s+([0-9]+(?:\.[0-9]+)?)s(?:\s|$)`)
	// "panic: test timed out after 30m0s", then "running tests:" and a line
	// per test: "\t\tTestSyncRefreshesMergeable (4s)".
	goTimeoutAfterRE = lazyre.New(`^panic: test timed out after ([0-9][0-9a-zµ.]*)`)
	goRunningRE      = lazyre.New(`^\s+((?:Test|Example|Fuzz|Benchmark)\S*) \(`)
)

// goPackages reads go test's package lines out of a cleaned job log, and
// marks the packages its timeout panics stopped. known are the
// repo-relative packages the job's evidence named, to map import paths to.
func goPackages(lines []string, known []string) []jobPackage {
	type stop struct {
		limit   time.Duration
		running []string
	}
	var stops []stop
	byPkg := map[string]int{}
	var out []jobPackage
	for i, l := range lines {
		if m := goPackageRE.FindStringSubmatch(l); m != nil {
			sec, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				continue
			}
			p := jobPackage{pkg: repoPackage(m[2], known), outcome: "ok", ms: int64(sec*1000 + 0.5)}
			if m[1] == "FAIL" {
				p.outcome = "fail"
			}
			if at, seen := byPkg[p.pkg]; seen {
				out[at] = p
			} else {
				byPkg[p.pkg] = len(out)
				out = append(out, p)
			}
			continue
		}
		m := goTimeoutAfterRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		d, err := time.ParseDuration(m[1])
		if err != nil || d <= 0 {
			continue
		}
		st := stop{limit: d}
		for _, r := range lines[i+1 : min(len(lines), i+12)] {
			if strings.TrimSpace(r) == "running tests:" {
				continue
			}
			t := goRunningRE.FindStringSubmatch(r)
			if t == nil {
				break
			}
			if len(st.running) < 3 {
				st.running = append(st.running, t[1])
			}
		}
		stops = append(stops, st)
	}
	// A panic is go test's binary; the FAIL line its package's. Each panic
	// goes to a failed package that ran at least as long as its limit.
	for _, st := range stops {
		for i := range out {
			if out[i].outcome == "fail" && out[i].ms >= st.limit.Milliseconds()-1000 {
				out[i].outcome, out[i].limitMs, out[i].running = "timeout", st.limit.Milliseconds(), st.running
				break
			}
		}
	}
	return out
}

// repoPackage maps a Go import path to the repo-relative package the job's
// evidence names it by (the longest that ends it), or, failing that, drops
// a host/owner/repo module prefix: "git.example.com/me/app/internal/core"
// is internal/core.
func repoPackage(importPath string, known []string) string {
	best := ""
	for _, k := range known {
		if (importPath == k || strings.HasSuffix(importPath, "/"+k)) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return best
	}
	parts := strings.Split(importPath, "/")
	if len(parts) > 3 && strings.Contains(parts[0], ".") {
		return strings.Join(parts[3:], "/")
	}
	return importPath
}

// keepJobPackages stores a job's package times.
func (s *Service) keepJobPackages(ctx context.Context, forgeID int64, repo string, j evidenceJob, pkgs []jobPackage) error {
	if len(pkgs) == 0 {
		return nil
	}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range pkgs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_job_packages(forge_id, repo, task_id, package, outcome, ms, limit_ms, running, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(forge_id, repo, task_id, package) DO UPDATE SET outcome = excluded.outcome, ms = excluded.ms,
				limit_ms = excluded.limit_ms, running = excluded.running, at = excluded.at`,
			forgeID, repo, j.task, p.pkg, p.outcome, p.ms, p.limitMs, strings.Join(p.running, ","), j.at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- limits from workflows ----

// workflowJobLimit is one workflow job's limits.
type workflowJobLimit struct {
	name   *regexp.Regexp // its name as the forge shows it
	jobMs  int64          // timeout-minutes; 0 when not set
	testMs int64          // the longest go test -timeout its steps give; 0 when it runs no go test
}

type workflowLimitCache struct {
	mu sync.Mutex
	m  map[string]workflowLimitEntry
}

type workflowLimitEntry struct {
	at   time.Time
	jobs []workflowJobLimit
}

var (
	goTestFlagTimeoutRE = lazyre.New(`(?:^|\s)-(?:test\.)?timeout[= ]([0-9][0-9a-zµ.]*)`)
	goTestAnyTimeoutRE  = lazyre.New(`(?:^|\s)-(?:test\.)?timeout[= ]\S`)
	goTestRunRE         = lazyre.New(`\bgo test\b`)
	exprRE              = lazyre.New(`\$\{\{.*?\}\}`)
	exprFormatRE        = lazyre.New(`format\(\s*'((?:[^']|'')*)'`)
	exprQuotedRE        = lazyre.New(`'((?:[^']|'')*)'`)
	formatArgRE         = lazyre.New(`\\\{[0-9]+\\\}`)
)

// parseWorkflowLimits reads the limits of a workflow file's jobs.
func parseWorkflowLimits(body string) []workflowJobLimit {
	var wf struct {
		Jobs map[string]struct {
			Name    string `yaml:"name"`
			Timeout any    `yaml:"timeout-minutes"`
			Steps   []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(body), &wf); err != nil {
		return nil
	}
	var out []workflowJobLimit
	for _, key := range slices.Sorted(maps.Keys(wf.Jobs)) {
		j := wf.Jobs[key]
		l := workflowJobLimit{name: jobNamePattern(key, j.Name)}
		switch v := j.Timeout.(type) {
		case int:
			l.jobMs = int64(v) * time.Minute.Milliseconds()
		case float64:
			l.jobMs = int64(v * float64(time.Minute.Milliseconds()))
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				l.jobMs = int64(n * float64(time.Minute.Milliseconds()))
			}
		}
		goTest, ownTimeout := false, false
		for _, st := range j.Steps {
			ownTimeout = ownTimeout || goTestAnyTimeoutRE.MatchString(st.Run)
			for _, m := range goTestFlagTimeoutRE.FindAllStringSubmatch(st.Run, -1) {
				if d, err := time.ParseDuration(m[1]); err == nil && d > 0 {
					l.testMs = max(l.testMs, d.Milliseconds())
				}
			}
			goTest = goTest || goTestRunRE.MatchString(st.Run)
		}
		// go test's own default, unless it was set to one this can't read
		// (an expression) or turned off (-timeout 0).
		if l.testMs == 0 && goTest && !ownTimeout {
			l.testMs = (10 * time.Minute).Milliseconds()
		}
		if l.name != nil && (l.jobMs > 0 || l.testMs > 0) {
			out = append(out, l)
		}
	}
	return out
}

// jobNamePattern matches the names a workflow job shows as: its name with
// expressions as wildcards. A name that is all expression takes its
// format() string, or else its last quoted string (`${{ … || 'backend' }}`);
// a matrix job without one shows as "key (values)".
func jobNamePattern(key, name string) *regexp.Regexp {
	if strings.TrimSpace(name) == "" {
		name = key
	}
	if t := strings.TrimSpace(name); strings.HasPrefix(t, "${{") && strings.HasSuffix(t, "}}") && len(exprRE.FindAllString(t, -1)) == 1 {
		if m := exprFormatRE.FindStringSubmatch(t); m != nil {
			name = strings.ReplaceAll(m[1], "''", "'")
		} else if q := exprQuotedRE.FindAllStringSubmatch(t, -1); len(q) > 0 {
			name = strings.ReplaceAll(q[len(q)-1][1], "''", "'")
		} else {
			return nil
		}
	}
	parts := exprRE.Split(name, -1)
	for i, p := range parts {
		parts[i] = formatArgRE.ReplaceAllString(regexp.QuoteMeta(p), `.+`)
	}
	re, err := regexp.Compile(`(?i)^` + strings.Join(parts, `.+`) + `(?: \(.*\))?$`)
	if err != nil {
		return nil
	}
	return re
}

// workflowLimits are the job limits of repo's workflow file (as ci_jobs
// name it: "ci.yml"), read from its default branch and kept for an hour.
func (s *Service) workflowLimits(ctx context.Context, f *Forge, repo, workflow string) []workflowJobLimit {
	key := fmt.Sprintf("%d|%s|%s", f.ID, strings.ToLower(repo), workflow)
	c := &s.ciLimits
	c.mu.Lock()
	if e, ok := c.m[key]; ok && s.now().Sub(e.at) < workflowLimitTTL {
		c.mu.Unlock()
		return e.jobs
	}
	c.mu.Unlock()
	rd, ok := s.Forges.(interface {
		FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error)
	})
	if !ok || workflow == "" || f.token == "" {
		return nil
	}
	ref := cmp(s.defaultBranchOf(ctx, f, repo), "main")
	paths := []string{".forgejo/workflows/" + workflow, ".gitea/workflows/" + workflow, ".github/workflows/" + workflow}
	if strings.Contains(workflow, "/") {
		paths = []string{workflow}
	}
	var jobs []workflowJobLimit // none when the workflow isn't there
	for _, p := range paths {
		body, err := rd.FileAt(ctx, f, repo, p, ref)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			s.log.Debug("CI limits: read a workflow", "repo", repo, "path", p, "err", err)
			return nil // tried again next time
		}
		jobs = parseWorkflowLimits(body)
		break
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) > 512 {
		c.m = map[string]workflowLimitEntry{}
	}
	c.m[key] = workflowLimitEntry{at: s.now(), jobs: jobs}
	c.mu.Unlock()
	return jobs
}

// jobLimitOf is the limits of the workflow job a ci_jobs name is.
func jobLimitOf(limits []workflowJobLimit, name string) workflowJobLimit {
	for _, l := range limits {
		if l.name.MatchString(name) {
			return l
		}
	}
	return workflowJobLimit{}
}

// ---- finding what is close ----

// ciLimit is a kind of CI job, or a Go package in one, close to its limit.
type ciLimit struct {
	Job     string        // the job's kind: "backend shard N"
	Tier    string        // full, draft or ""
	Package string        // "" for the job's own timeout-minutes
	Limit   time.Duration // the limit
	Runs    int
	P90     time.Duration
	Max     time.Duration
	Hits    int      // runs stopped by the limit
	Level   string   // problem (a run hit it) or warn (close)
	Running []string // tests running when go test's timeout fired
	Slowest []string // "TestX 21s": its slowest tests on average
}

// what names it: "internal/core in backend shard N (full tier)".
func (l ciLimit) what() string {
	w := l.Job
	if l.Package != "" {
		w = l.Package + " in " + l.Job
	}
	if l.Tier != "" {
		w += " (" + l.Tier + " tier)"
	}
	return w
}

// limitName: "go test's -timeout of 30m" or "its timeout-minutes of 40m".
func (l ciLimit) limitName() string {
	if l.Package != "" {
		return "go test's -timeout of " + ShortDuration(l.Limit)
	}
	return "the job's timeout-minutes of " + ShortDuration(l.Limit)
}

// line is one finding in words.
func (l ciLimit) line() string {
	b := fmt.Sprintf("%s: p90 %s, slowest %s of %s over %d runs", l.what(), ShortDuration(l.P90), ShortDuration(l.Max), l.limitName(), l.Runs)
	if l.Hits > 0 {
		b += fmt.Sprintf("; %d stopped by it", l.Hits)
	}
	if len(l.Running) > 0 {
		b += " (running " + strings.Join(l.Running, ", ") + " then)"
	}
	if len(l.Slowest) > 0 {
		b += "; slowest tests on average: " + strings.Join(l.Slowest, ", ")
	}
	return b
}

func limitLevel(runs int, p90, mx, limit time.Duration, hits int) string {
	switch {
	case limit <= 0:
		return ""
	case hits > 0:
		return "problem"
	case runs >= 3 && float64(p90) >= ciLimitP90*float64(limit), float64(mx) >= ciLimitMax*float64(limit):
		return "warn"
	}
	return ""
}

// ciLimitsOf finds repo's jobs and packages close to their limits over the
// last ciLimitWindow, problems first.
func (s *Service) ciLimitsOf(ctx context.Context, f *Forge, repo string) ([]ciLimit, error) {
	since := s.now().Add(-ciLimitWindow).UnixMilli()
	limits := map[string][]workflowJobLimit{}
	limitOf := func(workflow, name string) workflowJobLimit {
		ls, ok := limits[workflow]
		if !ok {
			ls = s.workflowLimits(ctx, f, repo, workflow)
			limits[workflow] = ls
		}
		return jobLimitOf(ls, name)
	}
	type group struct {
		ciLimit
		ms []float64
	}
	groups := map[string]*group{}
	var order []string
	add := func(key string, l ciLimit, ms int64, hit bool, running string) {
		g := groups[key]
		if g == nil {
			g = &group{ciLimit: l}
			groups[key] = g
			order = append(order, key)
		}
		g.Limit = max(g.Limit, l.Limit)
		g.ms = append(g.ms, float64(ms))
		if hit {
			g.Hits++
			if running != "" {
				g.Running = strings.Split(running, ",")
			}
		}
	}

	rows, err := s.db.R.QueryContext(ctx, `SELECT j.workflow, j.name, j.tier, p.package, p.outcome, p.ms, p.limit_ms, p.running
		FROM ci_job_packages p JOIN ci_jobs j ON j.forge_id = p.forge_id AND j.repo = p.repo AND j.task_id = p.task_id
		WHERE p.forge_id = ? AND p.repo = ? AND p.at >= ? ORDER BY p.at`, f.ID, repo, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var workflow, name, tier, pkg, outcome, running string
		var ms, limitMs int64
		if err := rows.Scan(&workflow, &name, &tier, &pkg, &outcome, &ms, &limitMs, &running); err != nil {
			rows.Close()
			return nil, err
		}
		limit := time.Duration(max(limitMs, limitOf(workflow, name).testMs)) * time.Millisecond
		kind := evidenceKind(name)
		add("p|"+kind+"|"+tier+"|"+pkg, ciLimit{Job: kind, Tier: tier, Package: pkg, Limit: limit}, ms, outcome == "timeout", running)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	jobs, err := s.db.R.QueryContext(ctx, `SELECT workflow, name, tier, status, ended_at - started_at FROM ci_jobs
		WHERE forge_id = ? AND repo = ? AND ended_at >= ? AND started_at > 0 AND ended_at > started_at
		  AND status IN ('success', 'failure', 'cancelled')`, f.ID, repo, since)
	if err != nil {
		return nil, err
	}
	for jobs.Next() {
		var workflow, name, tier, status string
		var ms int64
		if err := jobs.Scan(&workflow, &name, &tier, &status, &ms); err != nil {
			jobs.Close()
			return nil, err
		}
		lim := limitOf(workflow, name).jobMs
		if lim <= 0 {
			continue
		}
		kind := evidenceKind(name)
		// A job the forge stopped at its timeout ends a little past it.
		add("j|"+kind+"|"+tier, ciLimit{Job: kind, Tier: tier, Limit: time.Duration(lim) * time.Millisecond}, ms, ms >= lim-30_000 && status != "success", "")
	}
	jobs.Close()
	if err := jobs.Err(); err != nil {
		return nil, err
	}

	var out []ciLimit
	for _, k := range order {
		g := groups[k]
		g.Runs = len(g.ms)
		g.P90 = time.Duration(percentile(g.ms, 90)) * time.Millisecond
		g.Max = time.Duration(slices.Max(g.ms)) * time.Millisecond
		g.Level = limitLevel(g.Runs, g.P90, g.Max, g.Limit, g.Hits)
		if g.Level == "" {
			continue
		}
		if g.Package != "" {
			g.Slowest = s.slowestTests(ctx, f.ID, repo, g.Package, g.Tier, since)
		}
		out = append(out, g.ciLimit)
	}
	slices.SortStableFunc(out, func(a, b ciLimit) int {
		if (a.Level == "problem") != (b.Level == "problem") {
			if a.Level == "problem" {
				return -1
			}
			return 1
		}
		return strings.Compare(a.what(), b.what())
	})
	return out, nil
}

// slowestTests are a package's three slowest tests on average since, in tier.
func (s *Service) slowestTests(ctx context.Context, forgeID int64, repo, pkg, tier string, since int64) []string {
	rows, err := s.db.R.QueryContext(ctx, `SELECT t.name, avg(r.ms) FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id
		WHERE t.forge_id = ? AND t.repo = ? AND t.package = ? AND r.tier = ? AND r.at >= ?
		GROUP BY t.id ORDER BY avg(r.ms) DESC LIMIT 3`, forgeID, repo, pkg, tier, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var ms float64
		if rows.Scan(&name, &ms) == nil {
			out = append(out, fmt.Sprintf("%s %s", name, ShortDuration(time.Duration(ms)*time.Millisecond)))
		}
	}
	return out
}

// ---- the doctor and the task ----

// doctorLimits adds a finding for each job or package close to its limit.
func (s *Service) doctorLimits(ctx context.Context, d *CIDoctor, snap *ciSnapshot, visible func(int64, string) (string, bool)) error {
	var unread []error
	for _, r := range snap.repos {
		if _, ok := visible(r.ref.forge.ID, r.ref.repo); !ok {
			continue
		}
		ls, err := s.ciLimitsOf(ctx, r.ref.forge, r.ref.repo)
		if err != nil {
			unread = append(unread, fmt.Errorf("%s: %w", r.ref.repo, err))
			continue
		}
		task := s.ciLimitsTask(ctx, r.ref.forge.ID, r.ref.repo)
		urgent := task != "" && s.taskUrgent(ctx, SystemActor(), task)
		for _, l := range ls {
			title := fmt.Sprintf("%s runs close to %s", l.what(), l.limitName())
			if l.Hits > 0 {
				title = fmt.Sprintf("%s ran into %s %d times today", l.what(), l.limitName(), l.Hits)
			}
			next := "speed up its slowest tests, or split it across more shards"
			detail := l.line()
			if task != "" {
				next = "wp show " + task
				if urgent {
					detail += ". " + ciLimitUrgentLine(task)
				}
			}
			d.Findings = append(d.Findings, CIFinding{Level: l.Level, Kind: "near_limit", Repo: r.ref.repo, Title: title, Detail: detail, Next: next})
		}
	}
	return errors.Join(unread...)
}

// ciLimitTitle is the one task per repo about its CI's time limits.
func ciLimitTitle(repo string) string {
	return excerpt("CI of "+repo+" runs close to its time limits", 200)
}

// ciLimitsTask is the open CI limits task of repo, in the project the repo
// is attached to, or "".
func (s *Service) ciLimitsTask(ctx context.Context, forgeID int64, repo string) string {
	project, err := s.attachedProject(ctx, s.db.R, forgeID, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project of a repo's CI limits task", "repo", repo, "err", err)
		}
		return ""
	}
	if project == 0 {
		return ""
	}
	var key string
	_ = s.db.R.QueryRowContext(ctx, `SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND i.title = ? AND st.category NOT IN ('done','canceled') ORDER BY i.id DESC LIMIT 1`, project, ciLimitTitle(repo)).Scan(&key)
	return key
}

// ciLimitState is what the hourly check last did for a repo.
type ciLimitState struct {
	Checked int64  `json:"checked"`
	Level   string `json:"level,omitempty"`
	Told    int64  `json:"told,omitempty"`
}

func ciLimitKey(forgeID int64, repo string) string {
	return fmt.Sprintf("ci_limits:%d|%s", forgeID, strings.ToLower(repo))
}

// watchCILimits checks repo's limits hourly and files them as a task for
// an agent, or adds to the open one when they got worse or a day has gone
// by. Called from the CI history pass.
func (s *Service) watchCILimits(ctx context.Context, f *Forge, repo string) {
	key := ciLimitKey(f.ID, repo)
	var st ciLimitState
	if v, err := s.serverSetting(ctx, s.db.R, key); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &st)
	}
	now := s.now()
	if now.Sub(msTime(st.Checked)) < ciLimitEvery {
		return
	}
	ls, err := s.ciLimitsOf(ctx, f, repo)
	if err != nil {
		s.log.Debug("CI limits", "repo", repo, "err", err)
		return
	}
	level := ""
	for _, l := range ls {
		if l.Level == "problem" || level == "" {
			level = l.Level
		}
	}
	st.Checked = now.UnixMilli()
	worse := level == "problem" && st.Level != "problem" || level == "warn" && st.Level == ""
	if level != "" && (worse || now.Sub(msTime(st.Told)) >= ciLimitRetell) {
		// Told is set only once it was filed, so a failure is tried again
		// on the next look.
		task, err := s.fileCILimits(ctx, f.ID, repo, level, ls, "")
		if err != nil && readFault(err) {
			s.log.WarnContext(ctx, "file the CI limits of a repo", "repo", repo, "err", err)
		}
		if task != "" && err == nil {
			st.Told = now.UnixMilli()
		}
	}
	if level != "problem" {
		s.calmCILimits(ctx, f.ID, repo)
	}
	st.Level = level
	if b, err := json.Marshal(st); err == nil {
		s.putSetting(ctx, key, string(b))
	}
}

// fileCILimits files repo's limits as a task for an agent, or adds them to
// the open one, with what was seen (seen: a pull request's sighting). It
// returns the task's key, "" when there is no project or owner to file it;
// err when adding to the open task failed, so the watch tells it again.
func (s *Service) fileCILimits(ctx context.Context, forgeID int64, repo, level string, ls []ciLimit, seen string) (string, error) {
	var b strings.Builder
	if len(ls) > 0 {
		fmt.Fprintf(&b, "Over the last %s (as of %s):\n\n", ShortDuration(ciLimitWindow), s.now().UTC().Format("2006-01-02 15:04 UTC"))
		for _, l := range ls {
			fmt.Fprintf(&b, "- %s: %s\n", strings.ToUpper(l.Level[:1])+l.Level[1:], faultCode(l.line()))
		}
	}
	if seen != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(seen)
	}
	// One task at a time: babysits and the hourly watch file from their own
	// goroutines.
	defer forgeLocks.lock(fmt.Sprintf("cilimits:%d:%s", forgeID, strings.ToLower(repo)))()
	owner := s.repoProjectOwner(ctx, forgeID, repo)
	project, err := s.attachedProject(ctx, s.db.R, forgeID, repo)
	if err != nil {
		return "", err
	}
	if owner == 0 || project == 0 {
		return "", nil
	}
	u, err := s.userByID(ctx, s.db.R, owner)
	if err != nil || u.Disabled {
		return "", nil
	}
	writer := Actor{User: u}
	writer.textAgent = "waypoint"
	open := s.ciLimitsTask(ctx, forgeID, repo)
	// A run stopped by a limit: the fix cuts the merge line (WP-949), once
	// per spell, so a person who takes the tag off keeps it off.
	urge := level == "problem" && (open == "" || s.ciLimitUrged(ctx, forgeID, repo) != open)
	if urge {
		b.WriteString("\n" + ciLimitUrgeNote)
	}
	if open != "" {
		if _, err := s.AddComment(ctx, &writer, open, b.String()); err != nil {
			s.log.WarnContext(ctx, "add to the CI limits task", "task", open, "err", err)
			return open, err
		}
		if urge {
			s.urgeCILimits(ctx, &writer, forgeID, repo, open)
		}
		return open, nil
	}
	prio := "medium"
	labels := []string{"ci"}
	if level == "problem" {
		prio = "high"
		labels = append(labels, CIUrgentLabel)
	}
	made, err := s.CreateIssue(ctx, &writer, IssueInput{Project: itoa64(project), Type: "bug", Agent: "claude", Priority: prio, Labels: labels,
		Title: ciLimitTitle(repo), Description: ciLimitTaskText(repo) + b.String()})
	if err != nil {
		s.log.WarnContext(ctx, "file the CI limits task", "repo", repo, "err", err)
		return "", err
	}
	if urge {
		s.putSetting(ctx, ciLimitUrgedKey(forgeID, repo), made.Key)
	}
	return made.Key, nil
}

// ciLimitTaskText is what the task is for, and what an agent could do.
func ciLimitTaskText(repo string) string {
	return "Jobs or Go test packages in " + repo + "'s CI run close to their time limits, or past them. " +
		"A package past go test's `-timeout` fails its job on every pull request that runs it, and the front of the merge queue with it; " +
		"a job past its `timeout-minutes` is cut off. Waypoint's hourly check of the job logs found the numbers below (WP-922); " +
		"`wp ci doctor` shows them as they are now.\n\n" +
		"What would help, for whoever takes this to weigh:\n\n" +
		"1. **Find what got slower**: the slowest tests below, recent changes to them, and work that grows with the data a test builds (on 2026-11-11 an O(n²) mention sync, WP-921, put internal/core past 30m).\n" +
		"2. **Make it faster**: speed up the slow tests or what they exercise; or rebalance the shards (`.forgejo/scripts/gotest-shard.sh` weights, the shard count); raise a limit only when the time is justified.\n" +
		"3. Files under `.forgejo/` are protected: send changes to them as `wp request change`.\n\n" +
		"Each later check that finds it worse, or still close after a day, and each pull request held by it, is added here as a comment.\n\n"
}
