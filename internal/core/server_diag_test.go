package core

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLogRingRedacts(t *testing.T) {
	ring := NewLogRing(100, time.Hour)
	ring.AddSecret("hunter2-very-secret")
	ring.AddSecret("short")
	ring.AddEnvSecrets([]string{"LLM_GATEWAY_API_KEY=zq81-gateway-key-77", "WAYPOINT_METRICS_TOKEN=metrics-tok-123456", "HOME=/home/waypoint-user"})
	log := slog.New(ring.Handler(slog.NewTextHandler(io.Discard, nil)))

	log.Info("calling gateway", "authorization", "Bearer abcdefgh12345678")
	log.Info("sent header Authorization: Bearer zyxwvuts98765432")
	log.Info("request failed", "err", errors.New(`Get "https://bot:p4ssw0rd@git.example.com/api?token=abcd1234efgh&page=2": 401`))
	log.Warn("forge said no to Authorization: token 0123456789abcdef0123456789abcdef")
	log.Info("issued wpt_ABCDEFGHijklmnop1234 to an agent")
	log.With("api_key", "plain-value-here").Info("provider added")
	log.WithGroup("req").Info("login", "password", "pw-in-a-group", "user", "robin")
	log.Info("echo hunter2-very-secret and zq81-gateway-key-77 and metrics-tok-123456")
	log.Info("a short word", "home", "/home/waypoint-user")
	log.Info("key in an attr", "detail", "OPENAI sk-proj0123456789abcdefXYZ")

	recs, matched, _ := ring.query(LogQuery{})
	if matched != 10 || len(recs) != 10 {
		t.Fatalf("matched %d, kept %d", matched, len(recs))
	}
	var all strings.Builder
	for _, r := range recs {
		all.WriteString(r.Msg + " " + r.Attrs + "\n")
	}
	got := all.String()
	for _, secret := range []string{"abcdefgh12345678", "zyxwvuts98765432", "p4ssw0rd", "abcd1234efgh", "0123456789abcdef0123456789abcdef", "wpt_ABCDEFGHijklmnop1234",
		"plain-value-here", "pw-in-a-group", "hunter2-very-secret", "zq81-gateway-key-77", "metrics-tok-123456", "sk-proj0123456789abcdefXYZ"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q kept:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"authorization=•••", "Bearer •••", "page=2", "token •••", "req.user=robin", "req.password=•••", "api_key=•••", "/home/waypoint-user", "bot:•••@git.example.com", "a short word"} {
		if !strings.Contains(got, kept) {
			t.Errorf("want %q in:\n%s", kept, got)
		}
	}
}

func TestLogRingQuery(t *testing.T) {
	ring := NewLogRing(5, time.Hour)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ring.now = func() time.Time { return now }
	at := func(m int) time.Time { return now.Add(time.Duration(-m) * time.Minute) }
	ring.add(at(90), slog.LevelError, "too old", "")
	ring.add(at(50), slog.LevelInfo, "one", "")
	ring.add(at(40), slog.LevelWarn, "two", "job=hourly")
	ring.add(at(30), slog.LevelInfo, "three", "")
	ring.add(at(20), slog.LevelError, "four", "job=sweep")
	ring.add(at(10), slog.LevelInfo, "five", "")
	ring.add(at(5), slog.LevelWarn, "six", "")

	msgs := func(rs []LogRecord) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Msg)
		}
		return strings.Join(out, ",")
	}
	recs, matched, oldest := ring.query(LogQuery{})
	if msgs(recs) != "two,three,four,five,six" || matched != 5 || !oldest.Equal(at(40)) {
		t.Errorf("all: %s (%d), oldest %v", msgs(recs), matched, oldest)
	}
	if recs, _, _ := ring.query(LogQuery{Level: slog.LevelWarn}); msgs(recs) != "two,four,six" {
		t.Errorf("warn: %s", msgs(recs))
	}
	if recs, m, _ := ring.query(LogQuery{Limit: 2}); msgs(recs) != "five,six" || m != 5 {
		t.Errorf("limit keeps the newest, oldest first: %s (%d)", msgs(recs), m)
	}
	if recs, _, _ := ring.query(LogQuery{Grep: regexp.MustCompile(`(?i)JOB=`)}); msgs(recs) != "two,four" {
		t.Errorf("grep reads attrs: %s", msgs(recs))
	}
	if recs, _, _ := ring.query(LogQuery{Since: at(35), Until: at(15)}); msgs(recs) != "three,four" {
		t.Errorf("window: %s", msgs(recs))
	}
	if n, o := ring.size(); n != 5 || !o.Equal(at(40)) {
		t.Errorf("size %d %v", n, o)
	}
}

func TestServerDiagnosticsNeedAdmin(t *testing.T) {
	e := newEnv(t)
	ring := NewLogRing(100, time.Hour)
	ring.now = func() time.Time { return e.clockNow() }
	e.s.cfg.Logs = ring
	e.s.cfg.Version = "v9.9.9"
	sam := e.user("sam", false)
	scoped := &Actor{User: e.admin.User, Permissions: PermissionSet{PermIssuesRead: true}}
	allowed := &Actor{User: e.admin.User, Permissions: PermissionSet{PermServerRead: true}}
	calls := map[string]func(a *Actor) error{
		"status":  func(a *Actor) error { _, err := e.s.ServerStatus(e.ctx, a); return err },
		"log":     func(a *Actor) error { _, err := e.s.ServerLog(e.ctx, a, ServerLogQuery{}); return err },
		"metrics": func(a *Actor) error { _, err := e.s.ServerMetrics(e.ctx, a, ""); return err },
		"usage":   func(a *Actor) error { _, err := e.s.AIUsage(e.ctx, a, AIUsageQuery{}); return err },
	}
	for name, call := range calls {
		if err := call(sam); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by a non-admin: %v", name, err)
		}
		var pe *PermissionError
		if err := call(scoped); !errors.As(err, &pe) || pe.Permission != PermServerRead {
			t.Errorf("%s by a token without server.read: %v", name, err)
		}
		if err := call(allowed); err != nil {
			t.Errorf("%s by a token with server.read: %v", name, err)
		}
		if err := call(e.admin); err != nil {
			t.Errorf("%s by an admin: %v", name, err)
		}
	}
	job := &Actor{User: e.admin.User, Scope: map[int64]int{1: 1}}
	if err := calls["log"](job); !errors.Is(err, ErrForbidden) {
		t.Errorf("a job token read the server log: %v", err)
	}

	st, err := e.s.ServerStatus(e.ctx, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "v9.9.9" || st.Migration == "" || st.Migrations == 0 || len(st.Queues) == 0 {
		t.Errorf("status: %+v", st)
	}
}

func TestServerLogQuery(t *testing.T) {
	e := newEnv(t)
	ring := NewLogRing(100, time.Hour)
	ring.now = func() time.Time { return e.clockNow() }
	e.s.cfg.Logs = ring
	ring.add(e.clockNow().Add(-30*time.Minute), slog.LevelWarn, "safety screen failed", "err=429")
	ring.add(e.clockNow().Add(-10*time.Minute), slog.LevelInfo, "hourly done", "")
	l, err := e.s.ServerLog(e.ctx, e.admin, ServerLogQuery{Grep: "screen", Level: "warn"})
	if err != nil || l.Matched != 1 || l.Records[0].Msg != "safety screen failed" {
		t.Fatalf("log: %+v %v", l, err)
	}
	for _, q := range []ServerLogQuery{{Grep: "("}, {Level: "loud"}, {Limit: MaxLogLimit + 1}} {
		if _, err := e.s.ServerLog(e.ctx, e.admin, q); !IsInvalid(err) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	e.s.cfg.Logs = nil
	if _, err := e.s.ServerLog(e.ctx, e.admin, ServerLogQuery{}); !IsInvalid(err) {
		t.Errorf("no ring: %v", err)
	}
}

func TestAICallsRecordedAndCounted(t *testing.T) {
	e := newEnv(t)
	llm := &fakeLLM{answer: `{"ok": true}`}
	e.s.LLM = llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	for range 3 {
		if _, err := e.s.runAI(e.ctx, "title", "sys", "prompt", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.runAI(e.ctx, "estimate", "sys", "prompt", nil, &out); err != nil {
		t.Fatal(err)
	}
	e.s.recordAICall(e.ctx, aiCall{task: "review", pid: 1, model: "m", outcome: "rate_limit", err: &llmError{status: 429}, ms: 40})

	u, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if u.Total.Requests != 5 || u.Total.OK != 4 || u.Total.RateLimit != 1 || u.Total.Failed != 1 {
		t.Fatalf("total: %+v", u.Total)
	}
	if len(u.Rows) != 3 || u.Rows[0].Key != "title" || u.Rows[0].Requests != 3 {
		t.Errorf("by task: %+v", u.Rows)
	}
	s, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{By: "status"})
	if err != nil || len(s.Rows) != 2 || s.Rows[0].Key != "200" || s.Rows[1].Key != "429" {
		t.Errorf("by status: %+v %v", s, err)
	}
	if one, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{Task: "review"}); err != nil || one.Total.Requests != 1 {
		t.Errorf("one task: %+v %v", one, err)
	}
	if !strings.Contains(u.String(), "title") || !strings.Contains(u.String(), "total") {
		t.Errorf("table:\n%s", u.String())
	}
	if _, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{By: "colour"}); !IsInvalid(err) {
		t.Errorf("bad by: %v", err)
	}
	e.setClock(e.clockNow().Add(31 * 24 * time.Hour))
	e.s.pruneAICalls(e.ctx)
	var n int
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM ai_calls`).Scan(&n); err != nil || n != 0 {
		t.Errorf("after prune: %d %v", n, err)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 11, 6, 15, 30, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                 {},
		"90m":              now.Add(-90 * time.Minute),
		"3h":               now.Add(-3 * time.Hour),
		"2d":               now.Add(-48 * time.Hour),
		"1w":               now.Add(-7 * 24 * time.Hour),
		"6":                now.Add(-6 * time.Hour),
		"2026-11-05T14:00": time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC),
		"2026-11-05 14:00": time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC),
		"2026-11-05":       time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
		"14:00":            time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC),
	} {
		got, err := ParseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseWhen("yesterday-ish", now); err == nil {
		t.Error("nonsense parsed")
	}
}

func TestGrepMetrics(t *testing.T) {
	text := "# HELP a_total As.\n# TYPE a_total counter\na_total{x=\"1\"} 3\n# HELP b Bs.\n# TYPE b gauge\nb 1\n"
	got := grepMetrics(text, regexp.MustCompile(`a_total`))
	if got != "# HELP a_total As.\n# TYPE a_total counter\na_total{x=\"1\"} 3\n" {
		t.Errorf("got %q", got)
	}
}

// Secrets in the shapes the first pass missed (the WP-375 review).
func TestLogRingRedactsStructured(t *testing.T) {
	ring := NewLogRing(100, time.Hour)
	log := slog.New(ring.Handler(slog.NewTextHandler(io.Discard, nil)))
	type login struct {
		User     string
		Password string
	}
	log.WithGroup("password").Info("grouped", "value", "grouped-secret-1")
	log.Info("a struct", "req", login{User: "robin", Password: "struct-secret-2"})
	log.Info("a map", "cfg", map[string]string{"api_key": "map-secret-3"})
	log.Info(`body {"token":"json-secret-4","name":"x"}`)
	log.Info("callback https://app.example.com/cb#access_token=frag-secret-5&state=ok")
	log.Info("sent Authorization: Bearer colon:secret-6abcdef")
	early := log.With("note", "learned-later-secret-7")
	log.Info("stored before: learned-later-secret-7")
	ring.AddSecret("learned-later-secret-7")
	early.Info("after")
	recs, _, _ := ring.query(LogQuery{})
	var all strings.Builder
	for _, r := range recs {
		all.WriteString(r.Msg + " " + r.Attrs + "\n")
	}
	got := all.String()
	for _, secret := range []string{"grouped-secret-1", "struct-secret-2", "map-secret-3", "json-secret-4", "frag-secret-5", "secret-6abcdef", "learned-later-secret-7"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q kept:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"User:robin", "state=ok", `"name":"x"`, "Bearer •••"} {
		if !strings.Contains(got, kept) {
			t.Errorf("want %q in:\n%s", kept, got)
		}
	}
}

func TestLogRingCapsBytes(t *testing.T) {
	ring := NewLogRing(10, time.Hour)
	ring.add(time.Time{}, slog.LevelInfo, strings.Repeat("é", logMsgMax), strings.Repeat("ü", logAttrsMax))
	recs, _, _ := ring.query(LogQuery{})
	if len(recs[0].Msg) > logMsgMax+len("…") || len(recs[0].Attrs) > logAttrsMax+len("…") || !utf8.ValidString(recs[0].Msg) || !utf8.ValidString(recs[0].Attrs) {
		t.Errorf("msg %d bytes, attrs %d", len(recs[0].Msg), len(recs[0].Attrs))
	}
}

// A record made before the one kept ahead of it (loggers race for the
// lock) doesn't hide the younger ones behind it.
func TestLogRingOutOfOrder(t *testing.T) {
	ring := NewLogRing(10, time.Hour)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ring.now = func() time.Time { return now }
	ring.add(now.Add(-10*time.Minute), slog.LevelInfo, "young", "")
	ring.add(now.Add(-2*time.Hour), slog.LevelInfo, "too old", "")
	ring.add(now.Add(-5*time.Minute), slog.LevelInfo, "younger", "")
	recs, matched, oldest := ring.query(LogQuery{})
	if matched != 2 || recs[0].Msg != "young" || recs[1].Msg != "younger" || !oldest.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("%+v %d %v", recs, matched, oldest)
	}
}

func TestServerLogQueryChecks(t *testing.T) {
	e := newEnv(t)
	ring := NewLogRing(100, time.Hour)
	ring.now = func() time.Time { return e.clockNow() }
	e.s.cfg.Logs = ring
	ring.add(e.clockNow().Add(-time.Minute), slog.LevelWarn, "Safety Screen failed", "")
	if l, err := e.s.ServerLog(e.ctx, e.admin, ServerLogQuery{Grep: "safety screen"}); err != nil || l.Matched != 1 {
		t.Errorf("grep ignores case: %+v %v", l, err)
	}
	if _, err := e.s.ServerLog(e.ctx, e.admin, ServerLogQuery{Since: e.clockNow(), Until: e.clockNow().Add(-time.Hour)}); !IsInvalid(err) {
		t.Errorf("since after until: %v", err)
	}
	for _, in := range []string{"NaN", "nanh", "Inf", "-3h", "100000d"} {
		if _, err := ParseWhen(in, e.clockNow()); err == nil {
			t.Errorf("ParseWhen(%q) parsed", in)
		}
	}
}

// The AI usage counts the HTTP requests a call took, retries included,
// and a 200 that answered badly is a 200.
func TestAICallsCountRetries(t *testing.T) {
	e := newEnv(t)
	g := &fakeGateway{answer: `{"points": 5, "confidence": "medium", "reason": "fine"}`, limited: 1, retry: "0", bad: 1,
		served: "opencode/muse-spark-1.3-contributor-free"}
	gw := httptest.NewServer(g)
	defer gw.Close()
	e.s.LLM = gatewayClient()
	if err := e.s.EnsureLLMGateway(e.ctx, GatewaySetup{BaseURL: gw.URL, APIKey: "olm_test", Extensions: GatewayExtensions}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.s.AITasks(e.ctx, e.admin)
	var gwID int64
	for _, tk := range tasks {
		if tk.Task == "estimate" {
			gwID = *tk.ProviderID
		}
	}
	var named []ChainModel
	for _, m := range namedGatewayModels[1:] {
		named = append(named, ChainModel{ProviderID: gwID, Model: m})
	}
	if err := e.s.SetAITask(e.ctx, e.admin, "estimate", gwID, namedGatewayModels[0], named); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if _, err := e.s.runAI(e.ctx, "estimate", estimateSystem, "prompt", estimateSchema, &out); err != nil {
		t.Fatal(err)
	}
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT outcome, status, sent FROM ai_calls ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var outcome string
		var status, sent int
		if err := rows.Scan(&outcome, &status, &sent); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d %d", outcome, status, sent))
	}
	if want := []string{"bad_output 200 2", "ok 200 1"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
	if u, err := e.s.AIUsage(e.ctx, e.admin, AIUsageQuery{}); err != nil || u.Total.Requests != 2 || u.Total.Sent != 3 || !strings.Contains(u.String(), "sent") {
		t.Errorf("usage: %+v %v", u, err)
	}
}

func TestProfileSecondsIsCapped(t *testing.T) {
	for in, want := range map[int]int{-3: ProfileDefaultSeconds, 0: ProfileDefaultSeconds, 1: 1, 60: 60, 61: 60, 100000: 60} {
		if got := ProfileSeconds(in); got != want {
			t.Errorf("ProfileSeconds(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestServerMetricsShowsDatabase(t *testing.T) {
	e := newEnv(t)
	text, err := e.s.ServerMetrics(e.ctx, e.admin, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"database connections", "writer", "readers", "callers that waited", "total wait", "max open", "slowest database callers"} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if text, _ := e.s.ServerMetrics(e.ctx, e.admin, "^writer"); !strings.HasPrefix(text, "writer ") || strings.Contains(text, "readers") {
		t.Errorf("grep: %q", text)
	}
}
