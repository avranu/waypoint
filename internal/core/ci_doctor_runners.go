package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	doctorHostHistory = 72 * time.Hour
	doctorNoProgress  = 45 * time.Minute
	doctorSlowRuns    = 5
	doctorSlowFactor  = 1.6
	doctorSlowMargin  = 60.0
	doctorTimeLimits  = 24 * time.Hour
)

// ciHostRun is a job a host ran, as it reported; ended is 0 while it runs,
// and repo "" until the job has been matched to its run. reported is the
// host's last report of it: each report lists the containers still running,
// so one the host stopped listing without an end (its host restarted, the
// container was removed) is no longer running, however old.
type ciHostRun struct {
	forgeID                  int64
	host, job                string
	repo, workflow, name     string
	started, ended, reported int64
}

func (s *Service) ciHostRuns(ctx context.Context, since time.Time) ([]ciHostRun, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, host, job, repo, workflow, name, started_at, coalesce(ended_at, 0), reported_at
		FROM ci_job_usage WHERE role = 'job' AND (started_at > ? OR (ended_at IS NULL AND reported_at > ?))`,
		since.UnixMilli(), s.now().Add(-ciHostFresh).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ciHostRun
	for rows.Next() {
		var r ciHostRun
		if err := rows.Scan(&r.forgeID, &r.host, &r.job, &r.repo, &r.workflow, &r.name, &r.started, &r.ended, &r.reported); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// A failed read is returned with what the other reads found, so the answer
// says it is partial.
func (s *Service) doctorRunnerFindings(ctx context.Context, pool CIDoctorPool, now time.Time, show func(forgeID int64, repo string) bool) ([]CIFinding, error) {
	out, limitsErr := s.timeLimitFindings(ctx, now, show)
	runs, err := s.ciHostRuns(ctx, now.Add(-doctorHostHistory))
	if err != nil {
		return out, errors.Join(limitsErr, fmt.Errorf("read the hosts' runs: %w", err))
	}
	hosts, err := s.ciHosts(ctx)
	if err != nil {
		return out, errors.Join(limitsErr, fmt.Errorf("read the hosts: %w", err))
	}
	out = append(out, noProgressFindings(runs, hosts, pool, now, show)...)
	return append(out, slowHostFindings(runs, show)...), limitsErr
}

// noProgressFindings: a host with jobs running where every one of them has
// run far longer than it should and nothing has ended, or jobs waiting
// while nothing ends anywhere. No log says so; the job just never finishes.
func noProgressFindings(runs []ciHostRun, hosts map[int64][]CIHost, pool CIDoctorPool, now time.Time, show func(int64, string) bool) []CIFinding {
	type hostKey struct {
		forge int64
		host  string
	}
	seconds := map[string][]float64{}
	lastEnd, poolEnd := map[hostKey]int64{}, int64(0)
	running := map[hostKey][]ciHostRun{}
	for _, r := range runs {
		k := hostKey{r.forgeID, strings.ToLower(r.host)}
		switch {
		case r.ended > 0:
			lastEnd[k] = max(lastEnd[k], r.ended)
			poolEnd = max(poolEnd, r.ended)
			if r.ended > r.started {
				j := fmt.Sprintf("%d|%s", r.forgeID, r.job)
				seconds[j] = append(seconds[j], float64(r.ended-r.started)/1000)
			}
		case now.Sub(msTime(r.reported)) < ciHostFresh:
			running[k] = append(running[k], r)
		}
	}
	var out []CIFinding
	for _, forge := range slices.Sorted(maps.Keys(hosts)) {
		for _, h := range hosts[forge] {
			k := hostKey{forge, strings.ToLower(h.Name)}
			list := running[k]
			if h.Health == "stale" || h.Running == 0 || len(list) == 0 || now.Sub(msTime(lastEnd[k])) < doctorNoProgress {
				continue
			}
			stuck := true
			var names []string
			hidden := 0
			oldest := time.Duration(0)
			for _, r := range list {
				age := now.Sub(msTime(r.started))
				limit := doctorNoProgress
				if xs := seconds[fmt.Sprintf("%d|%s", r.forgeID, r.job)]; len(xs) > 0 {
					limit = max(limit, time.Duration(1.5*percentile(xs, 95)*float64(time.Second)))
				}
				if age < limit {
					stuck = false
					break
				}
				oldest = max(oldest, age)
				if r.repo != "" && show(r.forgeID, r.repo) {
					names = append(names, r.repo+" "+stringOr(r.name, r.job))
				} else {
					hidden++
				}
			}
			if !stuck {
				continue
			}
			detail := "Its jobs: " + strings.Join(names, ", ")
			if len(names) == 0 {
				detail = fmt.Sprintf("%d job%s", hidden, plural(hidden))
			} else if hidden > 0 {
				detail += fmt.Sprintf(", and %d more", hidden)
			}
			out = append(out, CIFinding{Level: "problem", Kind: "host_no_progress",
				Title:  fmt.Sprintf("CI host %s has made no progress: %d job%s running, the oldest for %s, and none has ended in over %s", h.Name, len(list), plural(len(list)), ShortDuration(oldest), ShortDuration(doctorNoProgress)),
				Detail: detail + ". Every one has run far longer than that job usually takes.",
				Next:   "wp ci status -v; a host needs a person or a single-purpose tool, so file a draft task for the project that owns it",
				key:    "host_no_progress:" + k.host})
		}
	}
	if pool.Waiting > 0 && time.Duration(pool.OldestWait)*time.Second >= doctorNoProgress && poolEnd > 0 && now.Sub(msTime(poolEnd)) >= doctorNoProgress {
		out = append(out, CIFinding{Level: "problem", Kind: "pool_no_progress",
			Title: fmt.Sprintf("The CI pool has made no progress: %d job%s waiting, the oldest for %s, and no job has ended anywhere for %s", pool.Waiting, plural(pool.Waiting), ShortDuration(secs(float64(pool.OldestWait))), ShortDuration(now.Sub(msTime(poolEnd)))),
			Next:  "wp ci status -v",
			key:   "pool_no_progress"})
	}
	return out
}

func stringOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// slowHostFindings: a host that takes much longer than the others over the
// same jobs. Nothing fails until a job reaches its limit, so nothing says so.
func slowHostFindings(runs []ciHostRun, show func(int64, string) bool) []CIFinding {
	type jobKey struct {
		forge                int64
		repo, workflow, name string
	}
	byJob := map[jobKey]map[string][]float64{}
	for _, r := range runs {
		if r.repo == "" || r.ended <= r.started {
			continue
		}
		k := jobKey{r.forgeID, r.repo, r.workflow, r.name}
		if byJob[k] == nil {
			byJob[k] = map[string][]float64{}
		}
		byJob[k][r.host] = append(byJob[k][r.host], float64(r.ended-r.started)/1000)
	}
	type slow struct {
		text  string
		ratio float64
	}
	found := map[string][]slow{}
	for k, hosts := range byJob {
		if !show(k.forge, k.repo) {
			continue
		}
		for host, xs := range hosts {
			var others []float64
			for h, ys := range hosts {
				if h != host {
					others = append(others, ys...)
				}
			}
			if len(xs) < doctorSlowRuns || len(others) < doctorSlowRuns {
				continue
			}
			m, o := percentile(xs, 50), percentile(others, 50)
			if o <= 0 || m < doctorSlowFactor*o || m-o < doctorSlowMargin {
				continue
			}
			found[host] = append(found[host], slow{ratio: m / o, text: fmt.Sprintf("%s %s: %s here, %s on the others (%d and %d runs)",
				k.repo, k.name, ShortDuration(secs(m)), ShortDuration(secs(o)), len(xs), len(others))})
		}
	}
	var out []CIFinding
	for _, host := range slices.Sorted(maps.Keys(found)) {
		list := found[host]
		slices.SortFunc(list, func(a, b slow) int {
			switch {
			case a.ratio > b.ratio:
				return -1
			case a.ratio < b.ratio:
				return 1
			}
			return strings.Compare(a.text, b.text)
		})
		lines := make([]string, 0, 3)
		for i, l := range list {
			if i == 3 {
				break
			}
			lines = append(lines, l.text)
		}
		out = append(out, CIFinding{Level: "warn", Kind: "host_slow",
			Title:  fmt.Sprintf("CI host %s runs %d job%s %.1f times slower than the other hosts", host, len(list), plural(len(list)), list[0].ratio),
			Detail: strings.Join(lines, "\n") + "\nA job near its time limit fails here and passes elsewhere.",
			Next:   "file a draft task for the project that owns the host", key: "host_slow:" + strings.ToLower(host)})
	}
	return out
}

// timeLimitFindings are the jobs that ran out of their workflow's
// timeout-minutes in the last day, from their kept logs.
func (s *Service) timeLimitFindings(ctx context.Context, now time.Time, show func(int64, string) bool) ([]CIFinding, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT h.forge_id, h.repo, c.name, c.url, c.log FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE c.state = 'failed' AND c.updated_at >= ? AND c.log LIKE '%unable to copy%'`, now.Add(-doctorTimeLimits).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read jobs' time limits: %w", err)
	}
	type seen struct {
		forge      int64
		repo, name string
		url        string
		n          int
		hosts      []string
	}
	type failed struct {
		forge           int64
		repo, name, url string
		f               *RunnerFault
	}
	var all []failed
	for rows.Next() {
		var c failed
		var log string
		if rows.Scan(&c.forge, &c.repo, &c.name, &c.url, &log) != nil {
			continue
		}
		if c.f = logRunnerFault(log); c.f != nil && c.f.Kind == faultTimeLimit {
			all = append(all, c)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read jobs' time limits: %w", err)
	}
	// show and nameFaultHost query too, so only once the rows are closed: a
	// query while they are open holds a second reader, and with every reader
	// held so, none is left for it.
	var order []string
	groups := map[string]*seen{}
	var faults []*RunnerFault
	var whose []*seen
	for _, c := range all {
		forge, repo, name, url, f := c.forge, c.repo, c.name, c.url, c.f
		if !show(forge, repo) {
			continue
		}
		k := ciRepoKey(forge, repo) + "|" + name
		g := groups[k]
		if g == nil {
			g = &seen{forge: forge, repo: repo, name: name}
			groups[k] = g
			order = append(order, k)
		}
		g.n++
		g.url = url
		faults, whose = append(faults, f), append(whose, g)
	}
	for i, f := range faults {
		s.nameFaultHost(ctx, whose[i].forge, f)
		if f.Runner != "" && !slices.Contains(whose[i].hosts, f.Runner) {
			whose[i].hosts = append(whose[i].hosts, f.Runner)
		}
	}
	var out []CIFinding
	for _, k := range order {
		g := groups[k]
		detail := "It ran out of its workflow's timeout-minutes; the job's owner raises that or makes the job faster."
		if len(g.hosts) > 0 {
			slices.Sort(g.hosts)
			detail = "On " + strings.Join(g.hosts, ", ") + ". " + detail
		}
		out = append(out, CIFinding{Level: "warn", Kind: "job_time_limit", Repo: g.repo,
			Title:  fmt.Sprintf("%s %s hit its time limit %d time%s in the last day", g.repo, g.name, g.n, plural(g.n)),
			Detail: detail, URL: g.url, key: "job_time_limit:" + k})
	}
	return out, nil
}

// NotifyCI is the notification kind of a CI health finding.
const NotifyCI = "ci"

const (
	ciNotifiedKey = "ci_notified:"
	ciNotifyEvery = 6 * time.Hour
	ciHostSilent  = 30 * time.Minute
)

// ciNotifyKinds are the findings worth a notification: a CI job or line
// that won't move by itself, or a host that is unfit. The rest wait on the
// board and `wp ci doctor`.
var ciNotifyKinds = map[string]bool{"line_stalled": true, "host_no_progress": true, "pool_no_progress": true, "pool": true}

// SweepCIHealth reads the doctor's findings on a clock and tells the admins
// about the ones nobody would otherwise notice (WP-838).
func (s *Service) SweepCIHealth(ctx context.Context) {
	admins := s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`)
	if len(admins) == 0 {
		return
	}
	u, err := s.userByID(ctx, s.db.R, admins[0])
	if err != nil {
		return
	}
	d, err := s.CIDoctor(ctx, &Actor{User: u})
	if err != nil {
		s.log.WarnContext(ctx, "CI health: read the doctor", "err", err)
		return
	}
	// Part of CI wasn't read: findings missing from it haven't cleared, so
	// nothing is forgotten. What was found is still told, but for the pool's
	// findings when the forge wasn't read (WP-1851).
	partial := len(d.Missing) > 0
	if partial {
		s.log.WarnContext(ctx, "CI health: the doctor was partial", "missing", d.Missing)
	}
	var due []CIFinding
	for _, f := range d.Findings {
		if d.forgeUnread && (f.Kind == "pool" || f.Kind == "pool_no_progress") {
			continue
		}
		if f.Level == "problem" && ciNotifyKinds[f.Kind] {
			due = append(due, f)
		}
	}
	if hosts, err := s.ciHosts(ctx); err == nil {
		now := s.now()
		for _, forge := range slices.Sorted(maps.Keys(hosts)) {
			for _, h := range hosts[forge] {
				if h.Health == "stale" && now.Sub(h.ReportedAt) >= ciHostSilent {
					due = append(due, CIFinding{Kind: "host_silent", Title: "CI host " + h.Name + " has stopped reporting: " + h.Why,
						Detail: "Its reporter or the host is down, and the pool leaves it out until it reports again.", key: "host_silent:" + strings.ToLower(h.Name)})
				}
			}
		}
	}
	s.notifyCIHealth(ctx, admins, due, !partial)
}

// notifyCIHealth tells the admins of each finding not told about in the last
// ciNotifyEvery, and forgets those that have cleared; forget is false when
// due is not the whole list.
func (s *Service) notifyCIHealth(ctx context.Context, admins []int64, due []CIFinding, forget bool) {
	now := s.now()
	last := map[string]int64{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT key, value FROM server_settings WHERE key >= ? AND key < ?`, ciNotifiedKey, ciNotifiedKey+"\xff")
	if err != nil {
		return
	}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			if at, err := strconv.ParseInt(v, 10, 64); err == nil {
				last[k] = at
			}
		}
	}
	rows.Close()
	seen := map[string]bool{}
	for _, f := range due {
		k := ciNotifiedKey + stringOr(f.key, f.Kind+"|"+f.Repo+"|"+f.Title)
		seen[k] = true
		if at, ok := last[k]; ok && now.Sub(msTime(at)) < ciNotifyEvery {
			continue
		}
		body := f.Detail
		if f.Next != "" {
			body = strings.TrimSpace(body + "\n\nNext: " + f.Next)
		}
		for _, id := range admins {
			s.notifyURL(ctx, id, nil, NotifyCI, f.Title, body, "/ci")
		}
		s.putSetting(ctx, k, itoa64(now.UnixMilli()))
	}
	if !forget {
		return
	}
	for k, at := range last {
		if !seen[k] && now.Sub(msTime(at)) >= ciNotifyEvery {
			s.dropSetting(ctx, k)
		}
	}
}
