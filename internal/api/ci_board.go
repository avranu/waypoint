package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The CI board: what the runner pool is running, what waits, what finished,
// and when it will all be done (WP-225).
func (s *Server) ciBoardRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/ci", a(s.ciBoard))
	mux.HandleFunc("GET /api/v1/ci/log", a(s.ciLog))
	mux.HandleFunc("GET /api/v1/ci/doctor", a(s.ciDoctor))
	mux.HandleFunc("GET /api/v1/ci/history", a(s.ciHistory)) // WP-490
	mux.HandleFunc("GET /api/v1/ci/line", a(s.ciLine))       // WP-1658
	mux.HandleFunc("GET /api/v1/ci/tests", a(s.ciTests))     // WP-704
	mux.HandleFunc("GET /api/v1/ci/coverage", a(s.ciCoverage))
	mux.HandleFunc("GET /api/v1/ci/suppressions", a(s.ciSuppressions))
	mux.HandleFunc("PUT /api/v1/ci/pools/{forge}", a(s.setCIPool))
	// CI hosts' reporters, and the usage they recorded (WP-231).
	mux.HandleFunc("POST /api/v1/ci/report", a(s.ciReport))
	mux.HandleFunc("GET /api/v1/ci/usage", a(s.ciUsage))
}

func (s *Server) ciReport(w http.ResponseWriter, r *http.Request) (any, error) {
	// Fields a newer reporter adds are ignored, not refused.
	var in core.CIHostReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return s.svc.ReportCIHost(r.Context(), actorFrom(r), in)
}

func (s *Server) ciUsage(w http.ResponseWriter, r *http.Request) (any, error) {
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%w: days is a number", errBadRequest)
		}
		days = n
	}
	return s.svc.CIUsage(r.Context(), actorFrom(r), r.URL.Query().Get("repo"), days)
}

func (s *Server) ciBoard(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.svc.CIBoard(r.Context(), actorFrom(r))
	if err != nil || !wantsText(r) {
		return b, err
	}
	q := r.URL.Query()
	return textResponse(ciBoardText(b, q.Get("repo"), q.Get("project"), queryBool(r, "verbose"))), nil
}

// ciLog is the log of CI jobs: ?repo=OWNER/NAME and either ?sha= or ?run=N,
// ?job=NAME to pick jobs (else the failed ones), ?full=1 for whole logs
// (WP-315).
func (s *Server) ciLog(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.CILogs(r.Context(), actorFrom(r), core.CILogInput{Repo: q.Get("repo"), SHA: q.Get("sha"),
		Run: int64(queryInt(r, "run", 0)), Job: q.Get("job"), Full: queryBool(r, "full")})
}

func (s *Server) setCIPool(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "forge")
	if err != nil {
		return nil, err
	}
	var in struct {
		Runners []core.CIPoolRunner `json:"runners"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.SetCIPool(r.Context(), actorFrom(r), id, in.Runners)
}

// ciBoardText is wp ci status. The pool (shared, so its totals are shown
// whole) and its alerts come first. Runs are narrowed to repo and to the
// comma-separated project keys (WP-271); none means every run. Short: one line
// per run, soonest first, capped. verbose lists every run, running and queued
// job, and the 24h figures and hosts.
func ciBoardText(b *core.CIBoard, repo, projects string, verbose bool) string {
	var o strings.Builder
	now := b.GeneratedAt
	at := func(t *time.Time) string {
		if t == nil {
			return "?"
		}
		return t.Local().Format("15:04") + " (in " + core.ShortDuration(t.Sub(now)) + ")"
	}
	keys := map[string]bool{}
	for _, k := range strings.Split(projects, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys[strings.ToUpper(k)] = true
		}
	}
	match := func(r core.CIRef) bool {
		if repo != "" && !strings.EqualFold(r.Repo, repo) {
			return false
		}
		return len(keys) == 0 || (!r.Hidden && keys[strings.ToUpper(r.Project)])
	}
	narrowed := repo != "" || len(keys) > 0
	sm := b.Summary
	fmt.Fprintf(&o, "Pool: %d/%d slots busy, %d queued, %d blocked on other jobs.", sm.Running, sm.Capacity, sm.Waiting, sm.Blocked)
	switch {
	case sm.Stuck > 0:
		fmt.Fprintf(&o, " %d jobs can't start.", sm.Stuck)
	case sm.AllDoneAt != nil && !narrowed:
		fmt.Fprintf(&o, " All done ~%s, slow case ~%s.", at(sm.AllDoneAt), at(sm.AllDoneP90))
	case sm.AllDoneAt == nil:
		o.WriteString(" Idle.")
	}
	o.WriteByte('\n')
	if verbose && sm.Finished24h > 0 {
		fmt.Fprintf(&o, "%s: %d jobs, %d failed, %d cancelled; waited %s typically (%s at p90), ran %s typically; pool %.0f%% busy.\n",
			"L"+strings.TrimPrefix(core.CIDayLabel(sm.Since, b.GeneratedAt), "l"), sm.Finished24h, sm.Failed24h, sm.Cancelled24h, core.ShortDuration(time.Duration(sm.WaitP50)*time.Second), core.ShortDuration(time.Duration(sm.WaitP90)*time.Second),
			core.ShortDuration(time.Duration(sm.RunP50)*time.Second), sm.Utilization24h*100)
	}
	if hs := ciHostsText(b.Hosts, verbose); hs != "" {
		fmt.Fprintf(&o, "Hosts: %s.\n", hs)
	}
	for _, a := range b.Alerts {
		if a.Level != "info" {
			fmt.Fprintf(&o, "%s: %s\n", strings.ToUpper(a.Level), a.Title)
		}
	}
	what := func(r core.CIRef) string {
		if r.Hidden {
			return "(another project)"
		}
		s := fmt.Sprintf("%s %s #%d", r.Repo, r.Workflow, r.RunNumber)
		if r.PR != nil {
			s += fmt.Sprintf(" PR %d", r.PR.Number)
		} else if r.Ref != "" {
			s += " " + r.Ref
		}
		return s
	}
	runEnd := func(r core.CIRun) string {
		if r.CantFinish {
			return "can't finish: a job has no runner"
		}
		return "done ~" + at(r.EstFinish)
	}
	if !verbose {
		o.WriteString(ciRunsShort(b.Runs, match, narrowed, what, runEnd))
		return o.String()
	}
	header := false
	for _, r := range b.Runs {
		if !match(r.CIRef) {
			continue
		}
		if !header {
			o.WriteString("\nRuns:\n")
			header = true
		}
		fmt.Fprintf(&o, "  %s  %d/%d jobs done, %d running  %s\n", what(r.CIRef), r.Done, r.Jobs, r.Running, runEnd(r))
	}
	header = false
	for _, j := range b.Running {
		if !match(j.CIRef) {
			continue
		}
		if !header {
			o.WriteString("\nRunning:\n")
			header = true
		}
		flag := ""
		if j.Overrun {
			flag = "  OVERRUN"
		}
		if j.Host != "" {
			flag += fmt.Sprintf("  on %s, %.1f GiB peak", j.Host, float64(j.MemPeak)/(1<<30))
		}
		fmt.Fprintf(&o, "  %s / %s  %s of usually %s, done ~%s%s\n", what(j.CIRef), j.Name, core.ShortDuration(time.Duration(j.Duration)*time.Second),
			core.ShortDuration(time.Duration(j.Typical)*time.Second), at(j.EstFinish), flag)
	}
	header = false
	for _, j := range b.Queued {
		if !match(j.CIRef) {
			continue
		}
		if !header {
			o.WriteString("\nQueued:\n")
			header = true
		}
		start := "never (no runner offers " + strings.Join(j.RunsOn, ", ") + ")"
		if j.EstStart != nil {
			start = "starts ~" + at(j.EstStart)
		}
		fmt.Fprintf(&o, "  %d. %s / %s  %s  %s\n", j.Position, what(j.CIRef), j.Name, j.Status, start)
	}
	return o.String()
}

// ciRunsShort is the default run listing: runs that can't finish first, then
// the soonest to finish, at most ciShortRuns lines and "+N more". Hidden runs
// (projects the viewer can't see) are left out.
func ciRunsShort(runs []core.CIRun, match func(core.CIRef) bool, narrowed bool, what func(core.CIRef) string, end func(core.CIRun) string) string {
	var mine []core.CIRun
	for _, r := range runs {
		if match(r.CIRef) && !r.Hidden {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		if narrowed {
			return "Nothing of yours is running or queued.\n"
		}
		return ""
	}
	sort.SliceStable(mine, func(i, j int) bool {
		a, b := mine[i], mine[j]
		if a.CantFinish != b.CantFinish {
			return a.CantFinish
		}
		switch {
		case a.EstFinish == nil || b.EstFinish == nil:
			return a.EstFinish != nil
		}
		return a.EstFinish.Before(*b.EstFinish)
	})
	var o strings.Builder
	running, queued := 0, 0
	for _, r := range mine {
		running += r.Running
		queued += r.Waiting
	}
	fmt.Fprintf(&o, "%d run%s, %d job%s running, %d queued:\n", len(mine), plural(len(mine)), running, plural(running), queued)
	for i, r := range mine {
		if i == ciShortRuns {
			fmt.Fprintf(&o, "  +%d more (-v)\n", len(mine)-i)
			break
		}
		failed := ""
		if r.Failed > 0 {
			failed = fmt.Sprintf(", %d failed", r.Failed)
		}
		fmt.Fprintf(&o, "  %s  %d/%d done%s  %s\n", what(r.CIRef), r.Done, r.Jobs, failed, end(r))
	}
	return o.String()
}

// ciShortRuns caps the run lines of the short wp ci status.
const ciShortRuns = 6

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ciHostsText lists the hosts: all of them when verbose, else only those that
// are not ok.
func ciHostsText(hosts []core.CIHost, verbose bool) string {
	var hs []string
	for _, h := range hosts {
		if h.Health == "ok" && !verbose {
			continue
		}
		x := fmt.Sprintf("%s %d/%d", h.Name, h.Running, h.Capacity)
		if h.DiskTotal > 0 {
			x += fmt.Sprintf(" (%.0f GiB free)", float64(h.DiskFree)/(1<<30))
		}
		if h.Health != "ok" {
			x += " " + strings.ToUpper(strings.ReplaceAll(h.Health, "_", " "))
		}
		hs = append(hs, x)
	}
	return strings.Join(hs, ", ")
}
