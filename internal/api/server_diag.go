package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Production diagnostics (WP-375): `wp server status|log|metrics` and
// `wp ai usage`, for admins, so agents need no ssh to the host.
func (s *Server) serverDiagRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/server/status", a(s.serverStatus))
	mux.HandleFunc("GET /api/v1/server/log", a(s.serverLog))
	mux.HandleFunc("GET /api/v1/server/metrics", a(s.serverMetrics))
	// What keeps going wrong, and each scheduled job over time (WP-ADR139).
	mux.HandleFunc("GET /api/v1/server/problems", a(s.serverProblems))
	mux.HandleFunc("POST /api/v1/server/problems/{id}/ack", a(s.ackServerProblem))
	mux.HandleFunc("DELETE /api/v1/server/problems/{id}/ack", a(s.unackServerProblem))
	mux.HandleFunc("GET /api/v1/server/jobs/{name}", a(s.serverJob))
	mux.HandleFunc("GET /api/v1/ai/usage", a(s.aiUsage))
	mux.HandleFunc("GET /api/v1/ai/surplus", a(s.aiSurplus))
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.svc.ServerStatus(r.Context(), actorFrom(r))
	if err != nil || !wantsText(r) {
		return st, err
	}
	return textResponse(serverStatusText(st, time.Now())), nil
}

// serverProblems is `wp server problems`.
func (s *Server) serverProblems(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	ps, err := s.svc.JobProblems(r.Context(), actorFrom(r), core.JobProblemQuery{All: queryBool(r, "all"), Source: v.Get("source"), Limit: queryInt(r, "limit", 0)})
	if err != nil || !wantsText(r) {
		return ps, err
	}
	return textResponse(serverProblemsText(ps, queryBool(r, "all"), time.Now())), nil
}

// ackServerProblem is `wp server ack N -m WHY`.
func (s *Server) ackServerProblem(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, fmt.Errorf("%w: say why it is expected (wp server ack N -m WHY)", errBadRequest)
	}
	return s.svc.AckJobProblem(r.Context(), actorFrom(r), id, in.Reason)
}

// unackServerProblem is `wp server unack N`.
func (s *Server) unackServerProblem(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.AckJobProblem(r.Context(), actorFrom(r), id, "")
}

// serverJob is `wp server job NAME`.
func (s *Server) serverJob(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	now := time.Now()
	since, err := core.ParseWhen(v.Get("since"), now)
	if err != nil {
		return nil, err
	}
	h, err := s.svc.JobHistory(r.Context(), actorFrom(r), r.PathValue("name"), since, v.Get("by"))
	if err != nil || !wantsText(r) {
		return h, err
	}
	return textResponse(serverJobText(h, now)), nil
}

func (s *Server) serverLog(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	now := time.Now()
	since, err := core.ParseWhen(v.Get("since"), now)
	if err != nil {
		return nil, err
	}
	until, err := core.ParseWhen(v.Get("until"), now)
	if err != nil {
		return nil, err
	}
	if since.IsZero() { // the hour before until
		since = now.Add(-time.Hour)
		if !until.IsZero() {
			since = until.Add(-time.Hour)
		}
	}
	l, err := s.svc.ServerLog(r.Context(), actorFrom(r), core.ServerLogQuery{
		Since: since, Until: until, Grep: v.Get("grep"), Level: v.Get("level"), Limit: queryInt(r, "limit", 0)})
	if err != nil || !wantsText(r) {
		return l, err
	}
	return textResponse(serverLogText(l, since)), nil
}

func (s *Server) serverMetrics(w http.ResponseWriter, r *http.Request) (any, error) {
	text, err := s.svc.ServerMetrics(r.Context(), actorFrom(r), r.URL.Query().Get("grep"))
	if err != nil {
		return nil, err
	}
	return textResponse(text), nil
}

func (s *Server) aiUsage(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	now := time.Now()
	since, err := core.ParseWhen(v.Get("since"), now)
	if err != nil {
		return nil, err
	}
	until, err := core.ParseWhen(v.Get("until"), now)
	if err != nil {
		return nil, err
	}
	u, err := s.svc.AIUsage(r.Context(), actorFrom(r), core.AIUsageQuery{Since: since, Until: until, By: v.Get("by"), Task: v.Get("task")})
	if err != nil || !wantsText(r) {
		return u, err
	}
	return textResponse(u.String()), nil
}

// aiSurplus is `wp ai surplus` (WP-906).
func (s *Server) aiSurplus(w http.ResponseWriter, r *http.Request) (any, error) {
	rep, err := s.svc.SurplusReport(r.Context(), actorFrom(r))
	if err != nil || !wantsText(r) {
		return rep, err
	}
	return textResponse(rep.String()), nil
}

// serverStatusText is `wp server status`.
func serverStatusText(st *core.ServerStatus, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Waypoint %s, up %s (since %s UTC)\n", st.Version, agoText(time.Duration(st.UptimeS)*time.Second), st.Started.UTC().Format("2006-01-02 15:04"))
	if p := st.Previous; p != nil {
		at, away := p.LastAlive.UTC().Format("15:04"), agoText(time.Duration(p.AwayS)*time.Second)
		if p.Clean {
			fmt.Fprintf(&b, "Before this start: stopped cleanly %s, away %s.\n", at, away)
		} else {
			fmt.Fprintf(&b, "Before this start: DID NOT SHUT DOWN (last alive %s), away %s.\n", at, away)
		}
	}
	if at := st.HealthCheckedAt; at != nil {
		if ago := now.Sub(*at); ago > 3*time.Minute {
			fmt.Fprintf(&b, "Job health NOT CHECKED for %s: the check itself is stuck (wp server log).\n", agoText(ago))
		} else {
			fmt.Fprintf(&b, "Job health checked %s ago.\n", agoText(ago))
		}
	}
	fmt.Fprintf(&b, "Schema: %s (%d migrations); database %s\n", strings.TrimSuffix(st.Migration, ".sql"), st.Migrations, bytesText(st.DBBytes))
	for _, k := range st.AIKeys {
		switch {
		case k.Error != "":
			fmt.Fprintf(&b, "AI key (%s): couldn't read it: %s\n", k.Provider, k.Error)
		case k.Limit > 0:
			fmt.Fprintf(&b, "AI key (%s): %d of %d requests today (UTC), %d left", k.Provider, k.Used, k.Limit, max(k.Limit-k.Used, 0))
		default:
			fmt.Fprintf(&b, "AI key (%s): %d requests today (UTC), no daily limit", k.Provider, k.Used)
		}
		if k.Error == "" {
			if k.ParkedUntil != nil {
				fmt.Fprintf(&b, "; requests parked until %s", k.ParkedUntil.UTC().Format("15:04 UTC"))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nQueues (waiting, running):\n")
	for _, q := range st.Queues {
		fmt.Fprintf(&b, "  %-16s %4d %4d\n", q.Name, q.Waiting, q.Running)
	}
	if len(st.Collectors) > 0 {
		b.WriteString("\nCollectors (waiting, longest wait, last output; as of the hourly check):\n")
		for _, c := range st.Collectors {
			wait, last := "-", "never"
			if c.OldestWaitAt != nil {
				wait = agoText(now.Sub(*c.OldestWaitAt))
			}
			if c.LastOutputAt != nil {
				last = agoText(now.Sub(*c.LastOutputAt)) + " ago"
			}
			line := fmt.Sprintf("  %-14s %5d  %-8s %s", c.Name, c.Waiting, wait, last)
			if c.Dropped != nil {
				line += fmt.Sprintf("  dropped %d", *c.Dropped)
			}
			if c.Off != nil {
				line += fmt.Sprintf("  %d off", *c.Off)
			}
			if c.Stalled {
				line += "  stalled"
				if c.StalledSince != nil {
					line += " for " + agoText(now.Sub(*c.StalledSince))
				}
			}
			b.WriteString(line + "\n")
		}
	}
	switch {
	case st.ProblemsPersistent > 0:
		fmt.Fprintf(&b, "\nProblems: %d persistent and unacknowledged, of %d open (wp server problems):\n", st.ProblemsPersistent, st.ProblemsOpen)
		shown := 0
		for _, p := range st.Problems {
			if p.Persistent && p.AckedAt == nil {
				b.WriteString("  " + problemLine(p, now) + "\n")
				shown++
			}
		}
		if shown < st.ProblemsPersistent {
			fmt.Fprintf(&b, "  and %d more\n", st.ProblemsPersistent-shown)
		}
	case st.ProblemsOpen > 0:
		fmt.Fprintf(&b, "\nProblems: %d open, none persistent and unacknowledged (wp server problems).\n", st.ProblemsOpen)
	}
	if len(st.Jobs) > 0 {
		b.WriteString("\nScheduled jobs (every, runs, last run, took, longest, last result; wp server job NAME for one over time):\n")
		w := 0
		for _, j := range st.Jobs {
			w = max(w, len(j.Name))
		}
		for _, j := range st.Jobs {
			every, last, took := "start", "-", "-"
			if j.PeriodMS > 0 {
				every = msShort(j.PeriodMS)
			}
			if j.LastEnd != nil && j.LastStart != nil {
				last, took = agoText(now.Sub(*j.LastStart))+" ago", msShort(j.LastMS)
			}
			if j.RunningSince != nil {
				took = "running " + agoText(now.Sub(*j.RunningSince))
			}
			line := fmt.Sprintf("  %-*s %-5s %6d  %-10s %-12s %-6s", w, j.Name, every, j.Runs, last, took, msShort(j.MaxMS))
			switch {
			case j.State != "":
				line += " " + strings.ToUpper(j.State)
			case j.LastResult != "" && j.LastResult != "clean":
				line += " " + j.LastResult
				if j.LastNote != "" {
					line += ": " + j.LastNote
				}
			}
			if j.Skips > 0 {
				line += fmt.Sprintf(" (%d skipped)", j.Skips)
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	if len(st.Warnings) > 0 {
		b.WriteString("\nWarnings and errors in the last hour (wp server log --level warn):\n")
		for _, c := range st.Warnings {
			fmt.Fprintf(&b, "  %4d× %-5s %s (last %s ago)\n", c.Count, c.Level, c.Msg, agoText(now.Sub(c.Last)))
		}
	} else if st.LogRecords > 0 {
		b.WriteString("\nNo warnings or errors in the last hour.\n")
	}
	if st.LogSince != nil {
		fmt.Fprintf(&b, "\nLog: %d records in memory since %s UTC (wp server log).\n", st.LogRecords, st.LogSince.UTC().Format("2006-01-02 15:04"))
	}
	if f := st.LogFiles; f != nil && f.Since != nil {
		fmt.Fprintf(&b, "Log files: since %s UTC, %s (kept %d days).\n", f.Since.UTC().Format("2006-01-02 15:04"), bytesText(f.Bytes), f.Days)
	}
	return b.String()
}

// problemLine is one problem in a line.
func problemLine(p core.JobProblem, now time.Time) string {
	l := fmt.Sprintf("#%d %-5s %s: %s", p.ID, p.Level, p.Source, p.Message)
	if p.Target != "" {
		l += " (" + p.Target + ")"
	}
	l += fmt.Sprintf("; %d× over %s (since %s), last %s ago", p.StreakCount, agoText(time.Duration(p.StreakUpMS)*time.Millisecond), p.StreakSince.UTC().Format("01-02 15:04"), agoText(now.Sub(p.LastAt)))
	switch {
	case p.ClearedAt != nil:
		l += "; over " + agoText(now.Sub(*p.ClearedAt)) + " ago"
	case p.AckedAt != nil:
		l += "; acknowledged"
		if p.AckedBy != "" {
			l += " by " + p.AckedBy
		}
		if p.AckReason != "" {
			l += ": " + p.AckReason
		}
	case p.Persistent:
		l += "; persistent"
	}
	return l
}

// serverProblemsText is `wp server problems`.
func serverProblemsText(ps []core.JobProblem, all bool, now time.Time) string {
	if len(ps) == 0 {
		if all {
			return "No problems are recorded.\n"
		}
		return "Nothing is going wrong: no warning or error has been logged lately (wp server problems --all for the ones that are over).\n"
	}
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(problemLine(p, now) + "\n")
		if p.Detail != "" {
			b.WriteString("    " + p.Detail + "\n")
		}
	}
	b.WriteString("\nA problem is a warning or an error, by where it was logged, its message and what it was about. Persistent: it has kept happening for a day, or a job that runs once failed; the admins are told of those. One that is expected: wp server ack N -m WHY (a person's to say).\n")
	return b.String()
}

// serverJobText is `wp server job NAME`.
func serverJobText(h *core.JobHistory, now time.Time) string {
	var b strings.Builder
	if j := h.Job; j != nil {
		every := "once, when the server starts"
		if j.PeriodMS > 0 {
			every = "every " + msShort(j.PeriodMS)
		}
		fmt.Fprintf(&b, "%s: a scheduled job, %s; %d runs", j.Name, every, j.Runs)
		if j.Skips > 0 {
			fmt.Fprintf(&b, ", %d skipped", j.Skips)
		}
		b.WriteString("\n")
		switch {
		case j.RunningSince != nil:
			fmt.Fprintf(&b, "Running for %s.", agoText(now.Sub(*j.RunningSince)))
		case j.LastEnd != nil && j.LastStart != nil:
			fmt.Fprintf(&b, "Last run %s ago, took %s: %s", agoText(now.Sub(*j.LastStart)), msShort(j.LastMS), j.LastResult)
			if j.LastNote != "" {
				b.WriteString(" (" + j.LastNote + ")")
			}
			b.WriteString(".")
		default:
			b.WriteString("It has not run.")
		}
		if j.LastCleanAt != nil {
			fmt.Fprintf(&b, " Last clean run %s ago.", agoText(now.Sub(*j.LastCleanAt)))
		} else if j.LastEnd != nil {
			b.WriteString(" No run of it has been clean.")
		}
		switch j.State {
		case "stuck":
			if j.BusySince != nil && j.RunningSince == nil {
				fmt.Fprintf(&b, " STUCK: every run since %s UTC found the last one still going.", j.BusySince.UTC().Format("01-02 15:04"))
			} else {
				b.WriteString(" STUCK: this run has gone on too long.")
			}
		case "silent":
			b.WriteString(" SILENT: it should have started and hasn't.")
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "%s: what is logged there, not a scheduled job\n", h.Source)
	}
	layout := "01-02"
	if h.By == "hour" {
		layout = "01-02 15:00"
	}
	if len(h.Spans) > 0 {
		fmt.Fprintf(&b, "\nBy %s (UTC) since %s: runs, not clean, skipped, warnings, errors, noted (failures logged below a warning or of work that was called off), average, longest\n", h.By, h.Since.UTC().Format("01-02 15:04"))
		for _, sp := range h.Spans {
			fmt.Fprintf(&b, "  %-11s %6d %5d %5d %6d %6d %6d  %-7s %s\n", sp.From.UTC().Format(layout), sp.Runs, sp.Unclean, sp.Skips, sp.Warnings, sp.Errors, sp.Noted, msShort(sp.AvgMS), msShort(sp.MaxMS))
		}
	} else {
		fmt.Fprintf(&b, "\nNothing counted since %s.\n", h.Since.UTC().Format("01-02 15:04"))
	}
	if len(h.Problems) > 0 {
		b.WriteString("\nProblems, past ones too:\n")
		for _, p := range h.Problems {
			b.WriteString("  " + problemLine(p, now) + "\n")
		}
	}
	return b.String()
}

// serverLogText is `wp server log`: one record a line, oldest first.
func serverLogText(l *core.ServerLog, since time.Time) string {
	var b strings.Builder
	for _, rec := range l.Records {
		fmt.Fprintf(&b, "%s %-5s %s", rec.At.UTC().Format("01-02 15:04:05"), rec.Level, rec.Msg)
		if rec.Attrs != "" {
			b.WriteString(" " + rec.Attrs)
		}
		b.WriteString("\n")
	}
	switch {
	case l.Matched == 0:
		b.WriteString("No log records match.")
	case l.Matched > len(l.Records):
		fmt.Fprintf(&b, "The newest %d of %d records that match (--limit N for more).", len(l.Records), l.Matched)
	default:
		fmt.Fprintf(&b, "%d records.", l.Matched)
	}
	if l.Since != nil && l.Since.After(since) {
		kept := "it keeps 48h, or 50,000 records, since the last start"
		if l.KeepDays > 0 {
			kept = fmt.Sprintf("it keeps %d days", l.KeepDays)
		}
		fmt.Fprintf(&b, " The log goes back to %s UTC only (%s).", l.Since.UTC().Format("01-02 15:04"), kept)
	}
	b.WriteString("\n")
	return b.String()
}

// agoText is a duration, short: 45s, 12m, 3h05m, 2d4h.
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func msShort(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return agoText(time.Duration(ms) * time.Millisecond)
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d kB", n/1024)
}
