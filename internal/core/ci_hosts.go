package core

// CI hosts (WP-231): a reporter on each runner host (the infrastructure repo
// bin/ci-usage-reporter.py) posts what the host has (labels, slots, CPUs,
// memory, the docker disk) and, per job and service container, the CPU
// time and peak memory its cgroup counted. The CI board's pool is the
// hosts that report (no hand-set pool to keep in step), a host's health
// gates the re-run of jobs that failed on it, and a job's history says how
// much it needs, so runners can size their slots to it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	ciReportEvery   = 15 * time.Second // what reporters are told
	ciHostFresh     = 3 * time.Minute  // a host that reported this recently is in the pool
	ciHostKnown     = 7 * 24 * time.Hour
	ciUsageKeep     = 60 * 24 * time.Hour
	ciUsageWindow   = 14 * 24 * time.Hour // the board's and the usage API's history
	ciDiskLowBytes  = 10 << 30            // below this, or below ciDiskLowShare, a host can't take jobs
	ciDiskLowShare  = 0.05
	ciDiskWarnShare = 0.15
	ciMaxContainers = 1000
)

// CIHostReport is what a CI host's reporter posts.
type CIHostReport struct {
	Forge      string             `json:"forge"` // the forge's host name, e.g. git.example.net
	Host       string             `json:"host"`  // the runner's name on the forge
	Labels     []string           `json:"labels"`
	Capacity   int                `json:"capacity"`
	CPUs       int                `json:"cpus"`
	Memory     int64              `json:"memory"`
	PoolCPUs   float64            `json:"pool_cpus"`
	PoolMemory int64              `json:"pool_memory"`
	JobCPUs    float64            `json:"job_cpus"`
	JobMemory  int64              `json:"job_memory"`
	DiskTotal  int64              `json:"disk_total"`
	DiskFree   int64              `json:"disk_free"`
	Load1      float64            `json:"load1"`
	Version    string             `json:"version"`
	At         *time.Time         `json:"at,omitempty"`
	Containers []CIContainerUsage `json:"containers"`
}

// CIContainerUsage is one job or service container, as its cgroup counted
// it so far (Ended nil while it runs).
type CIContainerUsage struct {
	TaskID     int64      `json:"task_id"`
	Job        string     `json:"job"`
	Name       string     `json:"name"`
	Role       string     `json:"role"` // job, service
	Started    time.Time  `json:"started"`
	Ended      *time.Time `json:"ended"`
	CPUSeconds float64    `json:"cpu_seconds"`
	MemPeak    int64      `json:"mem_peak"`
	MemLimit   int64      `json:"mem_limit"`
	CPULimit   float64    `json:"cpu_limit"`
	IORead     int64      `json:"io_read"`
	IOWrite    int64      `json:"io_write"`
}

// CIReportAck answers a report.
type CIReportAck struct {
	Interval int `json:"interval"` // seconds until the next report
	Stored   int `json:"stored"`   // containers recorded
}

// CIHost is a host that reported, and how it is.
type CIHost struct {
	ForgeID    int64     `json:"forge_id"`
	Name       string    `json:"name"`
	Labels     []string  `json:"labels"`
	Capacity   int       `json:"capacity"`
	Running    int       `json:"running"`
	CPUs       int       `json:"cpus"`
	Memory     int64     `json:"memory"`
	PoolCPUs   float64   `json:"pool_cpus"`
	PoolMemory int64     `json:"pool_memory"`
	JobCPUs    float64   `json:"job_cpus"`
	JobMemory  int64     `json:"job_memory"`
	DiskTotal  int64     `json:"disk_total"`
	DiskFree   int64     `json:"disk_free"`
	Load1      float64   `json:"load1"`
	Version    string    `json:"version"`
	ReportedAt time.Time `json:"reported_at"`
	// Health: ok, low_disk (it can't take jobs), stale (no report lately).
	Health string `json:"health"`
	Why    string `json:"why,omitempty"`
}

// ReportCIHost records a CI host's report (admins' tokens; a token with
// permissions needs ci.report).
func (s *Service) ReportCIHost(ctx context.Context, actor *Actor, rep CIHostReport) (*CIReportAck, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if !actor.isAdmin() {
		return nil, forbidden("only an admin's token can report CI hosts")
	}
	if err := actor.need(PermCIReport); err != nil {
		return nil, err
	}
	rep.Host = strings.TrimSpace(rep.Host)
	if rep.Host == "" || len(rep.Host) > 100 {
		return nil, invalid("host is the runner's name: 1 to 100 characters")
	}
	if rep.Capacity < 0 || rep.Capacity > 256 {
		return nil, invalid("capacity is how many jobs the host runs at once: 0 to 256")
	}
	if len(rep.Containers) > ciMaxContainers {
		return nil, invalid("at most %d containers a report", ciMaxContainers)
	}
	f, err := s.forgeNamed(ctx, rep.Forge)
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, l := range rep.Labels {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	lj, _ := json.Marshal(append([]string{}, labels...))
	now := s.nowMS()
	running := 0
	for _, c := range rep.Containers {
		if c.Ended == nil && c.Role != "service" {
			running++
		}
	}
	stored := 0
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO ci_hosts(forge_id, name, labels, capacity, cpus, memory, pool_cpus, pool_memory, job_cpus, job_memory,
			disk_total, disk_free, load1, running, version, reported_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(forge_id, name) DO UPDATE SET labels = excluded.labels, capacity = excluded.capacity, cpus = excluded.cpus, memory = excluded.memory,
			pool_cpus = excluded.pool_cpus, pool_memory = excluded.pool_memory, job_cpus = excluded.job_cpus, job_memory = excluded.job_memory,
			disk_total = excluded.disk_total, disk_free = excluded.disk_free, load1 = excluded.load1, running = excluded.running,
			version = excluded.version, reported_at = excluded.reported_at`,
			f.ID, rep.Host, string(lj), rep.Capacity, rep.CPUs, rep.Memory, rep.PoolCPUs, rep.PoolMemory, rep.JobCPUs, rep.JobMemory,
			rep.DiskTotal, rep.DiskFree, rep.Load1, running, cut(rep.Version, 40), now); err != nil {
			return err
		}
		for _, c := range rep.Containers {
			c.Name = strings.TrimSpace(c.Name)
			if c.Name == "" || c.TaskID <= 0 || !validTime(c.Started) {
				continue
			}
			role := "job"
			if c.Role == "service" {
				role = "service"
			}
			var ended any
			if c.Ended != nil && validTime(*c.Ended) {
				ended = c.Ended.UnixMilli()
			}
			// Counters only grow: a re-sent or late sample never lowers them.
			if _, err := t.ExecContext(ctx, `INSERT INTO ci_job_usage(forge_id, host, container, task_id, job, role, started_at, ended_at,
				cpu_seconds, mem_peak, mem_limit, cpu_limit, io_read, io_write, reported_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(forge_id, host, container) DO UPDATE SET ended_at = coalesce(excluded.ended_at, ci_job_usage.ended_at),
				cpu_seconds = max(ci_job_usage.cpu_seconds, excluded.cpu_seconds), mem_peak = max(ci_job_usage.mem_peak, excluded.mem_peak),
				mem_limit = excluded.mem_limit, cpu_limit = excluded.cpu_limit,
				io_read = max(ci_job_usage.io_read, excluded.io_read), io_write = max(ci_job_usage.io_write, excluded.io_write),
				reported_at = excluded.reported_at`,
				f.ID, rep.Host, cut(c.Name, 300), c.TaskID, cut(c.Job, 200), role, c.Started.UnixMilli(), ended,
				max(c.CPUSeconds, 0), max(c.MemPeak, 0), max(c.MemLimit, 0), max(c.CPULimit, 0), max(c.IORead, 0), max(c.IOWrite, 0), now); err != nil {
				return err
			}
			stored++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &CIReportAck{Interval: int(ciReportEvery.Seconds()), Stored: stored}, nil
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// forgeNamed is the forge whose URL's host (or name) is name.
func (s *Service) forgeNamed(ctx context.Context, name string) (*Forge, error) {
	name = strings.TrimSpace(name)
	fs, err := s.forges(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if u, err := url.Parse(f.BaseURL); err == nil && strings.EqualFold(u.Hostname(), name) {
			return f, nil
		}
	}
	for _, f := range fs {
		if strings.EqualFold(f.Name, name) {
			return f, nil
		}
	}
	return nil, invalid("no forge %q: forge is the forge's host name, as in its URL", name)
}

// ciHosts are the hosts that reported in the last week, by forge.
func (s *Service) ciHosts(ctx context.Context) (map[int64][]CIHost, error) {
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, name, labels, capacity, running, cpus, memory, pool_cpus, pool_memory, job_cpus, job_memory,
		disk_total, disk_free, load1, version, reported_at FROM ci_hosts WHERE reported_at > ? ORDER BY forge_id, name`, now.Add(-ciHostKnown).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]CIHost{}
	for rows.Next() {
		var h CIHost
		var labels string
		var at int64
		if err := rows.Scan(&h.ForgeID, &h.Name, &labels, &h.Capacity, &h.Running, &h.CPUs, &h.Memory, &h.PoolCPUs, &h.PoolMemory, &h.JobCPUs, &h.JobMemory,
			&h.DiskTotal, &h.DiskFree, &h.Load1, &h.Version, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &h.Labels)
		if h.Labels == nil {
			h.Labels = []string{}
		}
		h.ReportedAt = msTime(at)
		h.Health, h.Why = hostHealth(h, now)
		out[h.ForgeID] = append(out[h.ForgeID], h)
	}
	return out, rows.Err()
}

// hostHealth: ok, stale (it stopped reporting) or low_disk.
func hostHealth(h CIHost, now time.Time) (string, string) {
	if age := now.Sub(h.ReportedAt); age > ciHostFresh {
		return "stale", "no report for " + ShortDuration(age)
	}
	if h.DiskTotal > 0 && (h.DiskFree < ciDiskLowBytes || float64(h.DiskFree) < ciDiskLowShare*float64(h.DiskTotal)) {
		return "low_disk", fmt.Sprintf("%s of %s free on its docker disk", bytesWords(h.DiskFree), bytesWords(h.DiskTotal))
	}
	return "ok", ""
}

// bytesWords: 12.3 GiB.
func bytesWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

// reportedPool is the pool from the hosts that report now, with the
// configured runners that don't report added (nil: no host reports). With
// no pool configured, the slots the reports don't explain (observed: the
// most jobs seen running at once) stay as one unreported runner that takes
// any label, so a pool where only some hosts run the reporter isn't shrunk
// to those hosts (WP-519).
func reportedPool(hosts []CIHost, configured []CIPoolRunner, observed int) []CIPoolRunner {
	var out []CIPoolRunner
	for _, h := range hosts {
		if h.Health != "stale" && h.Capacity > 0 {
			out = append(out, CIPoolRunner{Name: h.Name, Labels: h.Labels, Capacity: h.Capacity})
		}
	}
	if len(out) == 0 {
		return nil
	}
	for _, r := range configured {
		if !slices.ContainsFunc(hosts, func(h CIHost) bool { return strings.EqualFold(h.Name, r.Name) }) {
			out = append(out, r)
		}
	}
	if len(configured) == 0 {
		reported := 0
		for _, r := range out {
			reported += r.Capacity
		}
		if observed > reported {
			out = append(out, CIPoolRunner{Name: unreportedRunner, Capacity: observed - reported})
		}
	}
	return out
}

// unreportedRunner names the slots no reporting host accounts for.
const unreportedRunner = "unreported"

// ciHostAlerts: a host that stopped reporting, or whose disk is (nearly) full.
func ciHostAlerts(hosts []CIHost) []CIAlert {
	out := []CIAlert{}
	for _, h := range hosts {
		switch h.Health {
		case "stale":
			out = append(out, CIAlert{Level: "warn", Title: fmt.Sprintf("CI host %s hasn't reported for %s", h.Name, strings.TrimPrefix(h.Why, "no report for ")),
				Body: "Its reporter or the host is down; the pool leaves it out until it reports again.", key: "host:" + h.Name + ":stale"})
		case "low_disk":
			out = append(out, CIAlert{Level: "error", Title: fmt.Sprintf("CI host %s is nearly out of disk: %s", h.Name, h.Why),
				Body: "Jobs on it fail with \"no space left on device\"; Waypoint re-runs the ones that did once it has room again.", key: "host:" + h.Name + ":low_disk"})
		default:
			if h.DiskTotal > 0 && float64(h.DiskFree) < ciDiskWarnShare*float64(h.DiskTotal) {
				out = append(out, CIAlert{Level: "warn", Title: fmt.Sprintf("CI host %s's docker disk is %.0f%% full", h.Name, 100-100*float64(h.DiskFree)/float64(h.DiskTotal)),
					Body: fmt.Sprintf("%s of %s free.", bytesWords(h.DiskFree), bytesWords(h.DiskTotal))})
			}
		}
	}
	return out
}

// runnerHealth is how the host named runner on a forge is: "unknown" when
// it doesn't report, else its CIHost health and why.
func (s *Service) runnerHealth(ctx context.Context, forgeID int64, runner string) (string, string) {
	if runner == "" {
		return "unknown", ""
	}
	hosts, err := s.ciHosts(ctx)
	if err != nil {
		return "unknown", ""
	}
	for _, h := range hosts[forgeID] {
		if strings.EqualFold(h.Name, runner) {
			return h.Health, h.Why
		}
	}
	return "unknown", ""
}

// ---- usage of jobs ----

// CIUsageStats is what one job (repo, workflow, job name) used over its
// recent runs: its container and services together, per run.
type CIUsageStats struct {
	ForgeID    int64     `json:"forge_id"`
	Repo       string    `json:"repo"`
	Workflow   string    `json:"workflow"`
	Name       string    `json:"name"`
	Runs       int       `json:"runs"`
	MemP50     int64     `json:"mem_p50"` // peak bytes per run
	MemP95     int64     `json:"mem_p95"`
	MemMax     int64     `json:"mem_max"`
	CPUP50     float64   `json:"cpu_p50"` // CPU seconds per run
	CPUP95     float64   `json:"cpu_p95"`
	CoresP50   float64   `json:"cores_p50"` // CPU seconds over wall seconds
	CoresP95   float64   `json:"cores_p95"`
	SecondsP50 float64   `json:"seconds_p50"`
	SecondsP95 float64   `json:"seconds_p95"`
	MemLimit   int64     `json:"mem_limit"` // what its container was allowed, last run
	CPULimit   float64   `json:"cpu_limit"`
	Hosts      []string  `json:"hosts"`
	LastAt     time.Time `json:"last_at"`
}

// ciTaskUse is one finished task's containers summed.
type ciTaskUse struct {
	forgeID              int64
	host                 string
	repo, workflow, name string
	started, ended       int64
	cpu                  float64
	mem                  int64
	memLimit             int64
	cpuLimit             float64
}

// ciUsage sums the finished tasks since a time, grouped by job.
func (s *Service) ciUsage(ctx context.Context, since time.Time) (map[string]*CIUsageStats, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, task_id, host, role, repo, workflow, name, started_at, ended_at, cpu_seconds, mem_peak, mem_limit, cpu_limit
		FROM ci_job_usage WHERE started_at > ? AND task_id IN (SELECT task_id FROM ci_job_usage WHERE role = 'job' AND repo != '' AND ended_at IS NOT NULL AND started_at > ?)
		ORDER BY started_at`, since.UnixMilli(), since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := map[[2]int64]*ciTaskUse{}
	var order [][2]int64
	for rows.Next() {
		var fid, task, started int64
		var ended sql.NullInt64
		var host, role, repo, wf, name string
		var cpu, cpuLim float64
		var mem, memLim int64
		if err := rows.Scan(&fid, &task, &host, &role, &repo, &wf, &name, &started, &ended, &cpu, &mem, &memLim, &cpuLim); err != nil {
			return nil, err
		}
		k := [2]int64{fid, task}
		t := tasks[k]
		if t == nil {
			t = &ciTaskUse{forgeID: fid}
			tasks[k] = t
			order = append(order, k)
		}
		t.cpu += cpu
		t.mem += mem // services' peaks added: what the job needed of its host, at most
		if role == "job" {
			t.host, t.repo, t.workflow, t.name = host, repo, wf, name
			t.started, t.ended = started, ended.Int64
			t.memLimit, t.cpuLimit = memLim, cpuLim
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type acc struct {
		st                       *CIUsageStats
		mem, cpu, cores, seconds []float64
	}
	accs := map[string]*acc{}
	for _, k := range order {
		t := tasks[k]
		if t.repo == "" || t.ended <= t.started {
			continue
		}
		key := fmt.Sprintf("%d|%s", t.forgeID, ciJobKey(t.repo, t.workflow, t.name))
		a := accs[key]
		if a == nil {
			a = &acc{st: &CIUsageStats{ForgeID: t.forgeID, Repo: t.repo, Workflow: t.workflow, Name: t.name, Hosts: []string{}}}
			accs[key] = a
		}
		wall := float64(t.ended-t.started) / 1000
		a.st.Runs++
		a.mem = append(a.mem, float64(t.mem))
		a.cpu = append(a.cpu, t.cpu)
		a.seconds = append(a.seconds, wall)
		a.cores = append(a.cores, t.cpu/wall)
		a.st.MemMax = max(a.st.MemMax, t.mem)
		a.st.MemLimit, a.st.CPULimit = t.memLimit, t.cpuLimit
		a.st.LastAt = msTime(t.ended)
		if !slices.Contains(a.st.Hosts, t.host) {
			a.st.Hosts = append(a.st.Hosts, t.host)
		}
	}
	out := make(map[string]*CIUsageStats, len(accs))
	for k, a := range accs {
		st := a.st
		st.MemP50, st.MemP95 = int64(percentile(a.mem, 50)), int64(percentile(a.mem, 95))
		st.CPUP50, st.CPUP95 = round2(percentile(a.cpu, 50)), round2(percentile(a.cpu, 95))
		st.CoresP50, st.CoresP95 = round2(percentile(a.cores, 50)), round2(percentile(a.cores, 95))
		st.SecondsP50, st.SecondsP95 = round2(percentile(a.seconds, 50)), round2(percentile(a.seconds, 95))
		out[k] = st
	}
	return out, nil
}

func round2(x float64) float64 { return float64(int64(x*100+0.5)) / 100 }

// CIUsage is every job's usage over the last days (1 to 60; admins): what
// a runner sizes a job's slot from.
func (s *Service) CIUsage(ctx context.Context, actor *Actor, repo string, days int) ([]*CIUsageStats, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if !actor.isAdmin() {
		return nil, forbidden("only admins can read the CI usage of every repo")
	}
	if days <= 0 {
		days = int(ciUsageWindow.Hours() / 24)
	}
	if days > int(ciUsageKeep.Hours()/24) {
		return nil, invalid("days: 1 to %d", int(ciUsageKeep.Hours()/24))
	}
	m, err := s.ciUsage(ctx, s.now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		return nil, err
	}
	out := []*CIUsageStats{}
	for _, st := range m {
		if repo == "" || strings.EqualFold(st.Repo, repo) {
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b *CIUsageStats) int {
		return cmpInt64(b.MemP95, a.MemP95)
	})
	return out, nil
}

// ciRunningUse is what each running task used so far, by forge and task id.
func (s *Service) ciRunningUse(ctx context.Context) map[[2]int64]ciTaskUse {
	out := map[[2]int64]ciTaskUse{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, task_id, host, role, cpu_seconds, mem_peak, started_at FROM ci_job_usage
		WHERE ended_at IS NULL AND reported_at > ?`, s.now().Add(-ciHostFresh).UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var fid, task, started int64
		var host, role string
		var cpu float64
		var mem int64
		if rows.Scan(&fid, &task, &host, &role, &cpu, &mem, &started) != nil {
			return out
		}
		k := [2]int64{fid, task}
		t := out[k]
		t.cpu += cpu
		t.mem += mem
		if role == "job" {
			t.host, t.started = host, started
		}
		out[k] = t
	}
	return out
}

// noteCITasks fills in which repo, workflow and job a recorded task was,
// and how it ended, from the tasks the board read.
func (s *Service) noteCITasks(ctx context.Context, snap *ciSnapshot) {
	type named struct {
		repo string
		t    ActionTask
	}
	byForge := map[int64]map[int64]named{}
	for _, d := range snap.repos {
		if d.hist == nil {
			continue
		}
		m := byForge[d.ref.forge.ID]
		if m == nil {
			m = map[int64]named{}
			byForge[d.ref.forge.ID] = m
		}
		for _, t := range d.hist.tasks {
			m[t.ID] = named{d.ref.repo, t}
		}
	}
	for fid, m := range byForge {
		rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT task_id FROM ci_job_usage WHERE forge_id = ? AND (repo = '' OR status = '') AND started_at > ?`,
			fid, s.now().Add(-ciHistoryWindow-time.Hour).UnixMilli())
		if err != nil {
			continue
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			n, ok := m[id]
			if !ok {
				continue
			}
			status := ""
			if !ciPending(n.t.Status) {
				status = n.t.Status
			}
			if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_job_usage SET repo = ?, workflow = ?, name = ?, status = ? WHERE forge_id = ? AND task_id = ?`,
				n.repo, n.t.WorkflowID, n.t.Name, status, fid, id); err != nil && !errors.Is(err, context.Canceled) {
				s.log.WarnContext(ctx, "best-effort write failed", "err", err)
				return
			}
		}
	}
}
