package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"git.jmann.me/claude/waypoint/internal/core/meaning"
)

// canReadServer is the access check of every server read.
func (a *Actor) canReadServer() error {
	if err := a.Need(PermServerRead); err != nil {
		return err
	}
	if !a.IsAdmin() {
		return forbidden("the server's status, log and metrics are for admins")
	}
	return nil
}

// ServerStatus is `wp server status`.
type ServerStatus struct {
	Version    string    `json:"version"`
	Started    time.Time `json:"started"`
	UptimeS    int64     `json:"uptime_s"`
	Migration  string    `json:"migration"` // the latest applied
	Migrations int       `json:"migrations"`
	DBBytes    int64     `json:"db_bytes"`
	// HealthCheckedAt is when the job health check last finished: it should
	// be under a minute ago. Previous is how the run before this one ended.
	HealthCheckedAt *time.Time   `json:"health_checked_at,omitempty"`
	Previous        *PreviousRun `json:"previous,omitempty"`
	// Jobs is every scheduled job, by name; Problems what keeps going
	// wrong, the persistent ones nobody acknowledged first; ProblemsOpen
	// how many there are, and ProblemsPersistent how many of those persist
	// with nobody having acknowledged them (WP-ADR139).
	Jobs               []JobStatus  `json:"jobs"`
	Problems           []JobProblem `json:"problems"`
	ProblemsOpen       int          `json:"problems_open"`
	ProblemsPersistent int          `json:"problems_persistent"`
	Queues             []QueueDepth `json:"queues"`
	// Collectors is what runners and hourly jobs collect, as the last hourly
	// check found it (WP-999).
	Collectors []CollectorHealth `json:"collectors"`
	// HookDrops is hook data dropped and counted: the CLIs' offline outboxes
	// and the server's caps on what a session keeps (WP-1468, WP-ADR145).
	HookDrops []HookDrop `json:"hook_drops"`
	// AgentEvents is how many of the newest agent-written events carry the
	// session that wrote them (WP-1470, WP-ADR145 point 6).
	AgentEvents AgentEventSessions `json:"agent_events"`
	AIKeys      []AIKeyDay         `json:"ai_keys"`
	// Meaning is the meaning service as its circuit breaker sees it
	// (WP-1602); absent when none is configured.
	Meaning *meaning.Health `json:"meaning,omitempty"`
	// Warnings is the last hour's warnings and errors, counted by message,
	// most first.
	Warnings []LogCount `json:"warnings"`
	// Log is what `wp server log` can read: how many records, from when.
	LogRecords int        `json:"log_records"`
	LogSince   *time.Time `json:"log_since,omitempty"`
	// LogFiles is the log on disk (WP-893): from when, how many bytes, how
	// many days kept. Absent when the log is kept in memory only.
	LogFiles *LogFiles `json:"log_files,omitempty"`
}

// LogFiles is what the server keeps of its log on disk.
type LogFiles struct {
	Since *time.Time `json:"since,omitempty"`
	Bytes int64      `json:"bytes"`
	Days  int        `json:"days"`
}

// QueueDepth is how much waits in one of the server's queues.
type QueueDepth struct {
	Name    string `json:"name"`
	Waiting int    `json:"waiting"`
	Running int    `json:"running"`
}

// AIKeyDay is an AI gateway key's day: its limit and what it used (UTC).
type AIKeyDay struct {
	Provider    string     `json:"provider"`
	Limit       int        `json:"limit"` // 0: none
	Used        int        `json:"used"`
	ParkedUntil *time.Time `json:"parked_until,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// LogCount is how often a message was logged.
type LogCount struct {
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
}

// serverQueues are the queues status counts besides the safety screens:
// name, then SQL counting the waiting and the running.
var serverQueues = []struct{ name, sql string }{
	{"bot runs", `SELECT coalesce(sum(status = 'queued'), 0), coalesce(sum(status = 'running'), 0) FROM bot_runs WHERE status IN ('queued', 'running')`},
	{"runner jobs", `SELECT coalesce(sum(state = 'queued'), 0), coalesce(sum(state = 'running'), 0) FROM runner_jobs WHERE state IN ('queued', 'running')`},
	{"automation runs", `SELECT coalesce(sum(status IN ('queued', 'waiting')), 0), coalesce(sum(status = 'running'), 0) FROM automation_runs WHERE status IN ('queued', 'waiting', 'running')`},
}

// statusProblems is how many problems the status lists; `wp server problems`
// has them all.
const statusProblems = 20

// ServerStatus says how the server is: version, uptime, migrations, the
// scheduler's jobs, its queues, the AI key's day and the last hour's
// warnings.
func (s *Service) ServerStatus(ctx context.Context, actor *Actor) (*ServerStatus, error) {
	if err := actor.canReadServer(); err != nil {
		return nil, err
	}
	now := s.now()
	out := &ServerStatus{Version: s.cfg.Version, Started: s.started, UptimeS: int64(now.Sub(s.started).Seconds()),
		Jobs: []JobStatus{}, Problems: []JobProblem{}, Queues: []QueueDepth{}, Collectors: []CollectorHealth{}, HookDrops: []HookDrop{}, AIKeys: []AIKeyDay{}, Warnings: []LogCount{}}
	if out.Version == "" {
		out.Version = "dev"
	}
	s.jobs.mu.Lock()
	if !s.jobs.checkedAt.IsZero() {
		at := s.jobs.checkedAt
		out.HealthCheckedAt = &at
	}
	out.Previous = s.jobs.prev
	s.jobs.mu.Unlock()
	s.saveJobHealth(ctx)
	var err error
	if out.Jobs, err = s.jobHealth(ctx); err != nil {
		return nil, fmt.Errorf("scheduled jobs: %w", err)
	}
	if out.Problems, err = s.jobProblems(ctx, JobProblemQuery{Limit: statusProblems}, 0); err != nil {
		return nil, fmt.Errorf("problems: %w", err)
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(`+persistentSQL+` AND p.acked_at IS NULL), 0)
		FROM job_problems p LEFT JOIN job_health j ON j.name = p.source WHERE p.cleared_at IS NULL`,
		now.UnixMilli(), problemHold.Milliseconds(), problemMinCount).Scan(&out.ProblemsOpen, &out.ProblemsPersistent); err != nil {
		return nil, fmt.Errorf("problems: %w", err)
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(name), ''), count(*) FROM schema_migrations`).Scan(&out.Migration, &out.Migrations); err != nil {
		return nil, err
	}
	var pages, pageSize int64
	if s.db.R.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages) == nil && s.db.R.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize) == nil {
		out.DBBytes = pages * pageSize
	}
	var screens QueueDepth
	if screens, err = s.screenQueue(ctx); err != nil {
		return nil, fmt.Errorf("safety screens: %w", err)
	}
	out.Queues = append(out.Queues, screens)
	for _, q := range serverQueues {
		d := QueueDepth{Name: q.name}
		if err := s.db.R.QueryRowContext(ctx, q.sql).Scan(&d.Waiting, &d.Running); err != nil {
			return nil, fmt.Errorf("%s: %w", q.name, err)
		}
		out.Queues = append(out.Queues, d)
	}
	if out.Collectors, err = s.CollectorHealthList(ctx); err != nil {
		s.log.WarnContext(ctx, "server status: collectors", "err", err)
		out.Collectors = nil
	}
	if out.HookDrops, err = s.HookDrops(ctx, actor); err != nil {
		s.log.WarnContext(ctx, "server status: hook drops", "err", err)
		out.HookDrops = []HookDrop{}
	}
	if out.AgentEvents, err = s.agentEventSessions(ctx); err != nil {
		s.log.WarnContext(ctx, "server status: agent events", "err", err)
	}
	out.AIKeys = s.aiKeyDays(ctx)
	if h, ok := meaning.HealthOf(s.Meaning); ok {
		out.Meaning = &h
	}
	if s.cfg.Logs != nil {
		out.Warnings = warningCounts(s.cfg.Logs, now.Add(-time.Hour), 10)
		n, oldest := s.cfg.Logs.size()
		out.LogRecords = n
		if n > 0 {
			out.LogSince = &oldest
		}
		out.LogFiles = s.cfg.Logs.files()
	}
	return out, nil
}

// aiKeyDays reads the AI gateway's key: its daily limit and today's use.
func (s *Service) aiKeyDays(ctx context.Context) []AIKeyDay {
	out := []AIKeyDay{}
	kl, ok := s.LLM.(keyUsageLLM)
	pid := s.gatewayWith(ctx, GatewayKey)
	if !ok || pid == 0 {
		return out
	}
	p, err := s.aiProvider(ctx, pid)
	if err != nil {
		return out
	}
	d := AIKeyDay{Provider: p.Name}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if d.Limit, d.Used, err = kl.KeyUsage(cctx, p); err != nil {
		d.Error = aiPublicError(err)
	}
	s.aib.mu.Lock()
	if pk, ok := s.aib.parked[pid]; ok && s.now().Before(pk.until) {
		until := pk.until
		d.ParkedUntil = &until
	}
	s.aib.mu.Unlock()
	return append(out, d)
}

// warningCounts counts the warnings and errors since `since` by message.
func warningCounts(r *LogRing, since time.Time, top int) []LogCount {
	recs, _, _ := r.query(LogQuery{Since: since, Level: slog.LevelWarn, Limit: MaxLogLimit, Newest: true})
	by := map[string]*LogCount{}
	for _, rec := range recs {
		k := rec.Level + "\x00" + rec.Msg
		c := by[k]
		if c == nil {
			c = &LogCount{Level: rec.Level, Msg: rec.Msg}
			by[k] = c
		}
		c.Count++
		c.Last = rec.At
	}
	out := make([]LogCount, 0, len(by))
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		return out[a].Msg < out[b].Msg
	})
	if len(out) > top {
		out = out[:top]
	}
	return out
}

// ServerLogQuery is what `wp server log` asks for.
type ServerLogQuery struct {
	Since, Until time.Time
	Grep         string // a regular expression (RE2), matched against the message and attributes
	Level        string // debug, info, warn, error: at least this
	Limit        int
}

// ServerLog is the records a query matched.
type ServerLog struct {
	Records []LogRecord `json:"records"`
	// Matched is how many matched in all; Records has the newest Limit.
	Matched int `json:"matched"`
	// Since is the oldest record kept, in memory or on disk: nothing before
	// it can be read.
	Since *time.Time `json:"since,omitempty"`
	// KeepDays is how many days the log files keep; 0: the log is kept in
	// memory only (48h or 50,000 records, since the last start).
	KeepDays int `json:"keep_days,omitempty"`
}

// maxGrepLen bounds a log or metrics pattern.
const maxGrepLen = 500

func compileGrep(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, nil
	}
	if len(p) > maxGrepLen {
		return nil, invalid("the pattern is longer than %d characters", maxGrepLen)
	}
	re, err := regexp.Compile("(?i)" + p)
	if err != nil {
		return nil, invalid("the pattern isn't a regular expression: %v", err)
	}
	return re, nil
}

// ParseLogLevel reads debug, info, warn(ing) or error; "" is info.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	}
	return 0, invalid("level %q: say debug, info, warn or error", s)
}

// ServerLog reads the server's recent log records (WP-375).
func (s *Service) ServerLog(ctx context.Context, actor *Actor, q ServerLogQuery) (*ServerLog, error) {
	if err := actor.canReadServer(); err != nil {
		return nil, err
	}
	if s.cfg.Logs == nil {
		return nil, invalid("this server keeps no log records (it was started without them)")
	}
	re, err := compileGrep(q.Grep)
	if err != nil {
		return nil, err
	}
	lvl, err := ParseLogLevel(q.Level)
	if err != nil {
		return nil, err
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		return nil, invalid("since must be before until")
	}
	if q.Limit < 0 || q.Limit > MaxLogLimit {
		return nil, invalid("limit is 1 to %d", MaxLogLimit)
	}
	recs, matched, oldest := s.cfg.Logs.query(LogQuery{Since: q.Since, Until: q.Until, Grep: re, Level: lvl, Limit: q.Limit})
	out := &ServerLog{Records: recs, Matched: matched}
	if f := s.cfg.Logs.files(); f != nil {
		out.KeepDays = f.Days
	}
	if out.Records == nil {
		out.Records = []LogRecord{}
	}
	if !oldest.IsZero() {
		out.Since = &oldest
	}
	return out, nil
}

// ServerMetrics is the server's Prometheus metrics in the text format; with
// a pattern, only the metrics whose name or sample line matches it.
func (s *Service) ServerMetrics(ctx context.Context, actor *Actor, grep string) (string, error) {
	if err := actor.canReadServer(); err != nil {
		return "", err
	}
	re, err := compileGrep(grep)
	if err != nil {
		return "", err
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil && len(families) == 0 {
		return "", err
	}
	var b bytes.Buffer
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		if err := enc.Encode(f); err != nil {
			return "", err
		}
	}
	if re == nil {
		return b.String(), nil
	}
	return grepMetrics(b.String(), re), nil
}

// grepMetrics keeps the sample lines that match re, and the HELP and TYPE
// lines of their metrics.
func grepMetrics(text string, re *regexp.Regexp) string {
	var out strings.Builder
	var head []string
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "# HELP ") {
				head = head[:0]
			}
			head = append(head, line)
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if !re.MatchString(line) && !re.MatchString(name) {
			continue
		}
		for _, h := range head {
			out.WriteString(h + "\n")
		}
		head = head[:0]
		out.WriteString(line + "\n")
	}
	return out.String()
}

// ParseWhen reads a time for --since and --until: a time ago (90m, 3h, 2d,
// 1w; a bare number is hours), an RFC 3339 time, "2006-01-02 15:04" or
// "2006-01-02T15:04" (UTC), a date, or "15:04" (today, UTC). "" is the zero
// time.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, ok := agoDuration(s); ok {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", s, time.UTC); err == nil {
		y, m, d := now.UTC().Date()
		return time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, time.UTC), nil
	}
	return time.Time{}, invalid("%q isn't a time: say 3h (ago), 2d, 90m, 15:04 (UTC today) or 2026-01-02T15:04:05Z", s)
}

// agoDuration reads 90m, 3h, 2d, 1w, 30s or a bare number of hours.
func agoDuration(s string) (time.Duration, bool) {
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, true
	}
	unit := time.Hour
	switch s[len(s)-1] {
	case 'd':
		unit, s = 24*time.Hour, s[:len(s)-1]
	case 'w':
		unit, s = 7*24*time.Hour, s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || !(n >= 0 && n <= 24*366) {
		return 0, false
	}
	return time.Duration(n * float64(unit)), true
}

// agentEventsSampled is how many of the newest events agentEventSessions reads
// (the events table has no index by time, so the newest by id stand for "lately").
const agentEventsSampled = 20000

// AgentEventSessions: of the newest events (Sampled of them, at most
// agentEventsSampled, fewer when the table holds fewer), Agent were written by
// an agent (a person's token that names an agent, or an MCP client) and
// WithSession of those carry the session that wrote them in data.session.
type AgentEventSessions struct {
	Sampled     int `json:"sampled"`
	Agent       int `json:"agent"`
	WithSession int `json:"with_session"`
}

// agentEventSessions takes the newest events first and only then asks which
// an agent wrote, so the window is the one the status line names and the
// work is bounded however sparse agent events are.
func (s *Service) agentEventSessions(ctx context.Context) (AgentEventSessions, error) {
	var out AgentEventSessions
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(agent), 0), coalesce(sum(agent AND coalesce(json_extract(data, '$.session'), 0) > 0), 0) FROM
		(SELECT data, actor_id IS NOT NULL AND json_extract(data, '$.via') IS NOT NULL AS agent FROM events ORDER BY id DESC LIMIT ?)`, agentEventsSampled).Scan(&out.Sampled, &out.Agent, &out.WithSession)
	return out, err
}
