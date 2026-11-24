package core

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Bounds of the log ring: whichever is reached first drops the oldest.
const (
	DefaultLogRecords = 50000
	DefaultLogAge     = 48 * time.Hour
	logMsgMax         = 2000
	logAttrsMax       = 4000
	minSecretLen      = 8
)

// LogRecord is one log record as the ring keeps it.
type LogRecord struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"` // DEBUG, INFO, WARN, ERROR
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"` // key=value pairs, redacted
	level slog.Level
}

// LogRing keeps the server's recent log records, redacted.
type LogRing struct {
	mu       sync.Mutex
	buf      []LogRecord
	next     int
	full     bool
	maxAge   time.Duration
	secrets  map[string]bool
	sorted   []string
	store    *logStore
	now      func() time.Time
	onSecret []func(secret string)
}

// NewLogRing keeps up to size records of at most maxAge.
func NewLogRing(size int, maxAge time.Duration) *LogRing {
	if size < 1 {
		size = DefaultLogRecords
	}
	if maxAge <= 0 {
		maxAge = DefaultLogAge
	}
	return &LogRing{buf: make([]LogRecord, size), maxAge: maxAge, secrets: map[string]bool{}, now: time.Now}
}

// AddSecret blanks s wherever a later record has it. Values shorter than
// minSecretLen are ignored.
func (r *LogRing) AddSecret(s string) {
	s = strings.TrimSpace(s)
	if r == nil || len(s) < minSecretLen {
		return
	}
	if r.addSecret(s) {
		r.mu.Lock()
		hooks := r.onSecret
		r.mu.Unlock()
		for _, f := range hooks {
			f(s)
		}
	}
}

// addSecret is AddSecret's work under the lock; false when s was known.
func (r *LogRing) addSecret(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secrets[s] {
		return false
	}
	r.secrets[s] = true
	sorted := append(slices.Clone(r.sorted), s)
	slices.SortStableFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	r.sorted = sorted
	quoted := strconv.Quote(s)
	quoted = quoted[1 : len(quoted)-1]
	for i := range r.buf {
		rec := &r.buf[i]
		if strings.Contains(rec.Msg, s) {
			rec.Msg = strings.ReplaceAll(rec.Msg, s, redacted)
		}
		if strings.Contains(rec.Attrs, s) {
			rec.Attrs = strings.ReplaceAll(rec.Attrs, s, redacted)
		}
		if quoted != s && strings.Contains(rec.Attrs, quoted) {
			rec.Attrs = strings.ReplaceAll(rec.Attrs, quoted, redacted)
		}
	}
	return true
}

// blankSecret blanks secret in text, in the form attributes quote it in too.
func blankSecret(text, secret string) string {
	text = strings.ReplaceAll(text, secret, redacted)
	if q := strconv.Quote(secret); q[1:len(q)-1] != secret {
		text = strings.ReplaceAll(text, q[1:len(q)-1], redacted)
	}
	return text
}

// secretEnvRE: environment variables whose values are secrets.
var secretEnvRE = lazyre.New(`(?i)(token|secret|password|passwd|api_?key|private_?key|credential|_key$|_pat$)`)

// AddEnvSecrets blanks the values of the environment's secret-sounding
// variables (LLM_GATEWAY_API_KEY, WAYPOINT_METRICS_TOKEN, …); env is os.Environ().
func (r *LogRing) AddEnvSecrets(env []string) {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && secretEnvRE.MatchString(k) {
			r.AddSecret(v)
		}
	}
}

// Handler wraps next so every record it handles is kept here too.
func (r *LogRing) Handler(next slog.Handler) slog.Handler {
	return &ringHandler{ring: r, next: next}
}

// logKeyRE: attribute names whose values are always blanked.
var logKeyRE = lazyre.New(`(?i)(token|secret|password|passwd|api[_-]?key|apikey|authorization|credential|cookie|private[_-]?key|signature)`)

// logValueREs: secrets by their shape, in any text. Group 1 is kept (the
// scheme word, so the line still reads), the rest blanked.
var logValueREs = []*lazyre.Regexp{
	lazyre.New(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=:-]{8,}`),
	lazyre.New(`(?i)\b(authorization:\s*(?:[A-Za-z]+\s+)?)[A-Za-z0-9._~+/=:-]{8,}`),
	lazyre.New(`(?i)\b(token\s+)[0-9a-f]{20,}\b`),
	lazyre.New(`()\bwp[dst]_[A-Za-z0-9_-]{8,}`),
	lazyre.New(`()\bsk-[A-Za-z0-9_-]{16,}`),
	lazyre.New(`()\b(?:gh[opsu]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,})`),
	lazyre.New(`(?i)()\bxox[abpr]-[A-Za-z0-9-]{10,}`),
}

// logSecretName: a name whose value is a secret, in an attribute key, a
// group, a query string, JSON, a Go map or struct.
const logSecretName = `[A-Za-z_-]*(?:token|secret|password|passwd|api[_-]?key|apikey|authorization|credential|cookie|signature|sig|access[_-]?key|private[_-]?key)` // #nosec G101 -- a pattern of secret names, not a credential

// logParamRE: name=value with a secret-sounding name, as in a query string
// (?token=…, #access_token=…) or a command line (API_KEY=…).
var logParamRE = lazyre.New(`(?i)((?:^|[?&#;,\s"'(])` + logSecretName + `=)("[^"]*"|'[^']*'|[^&\s"',)]+)`)

// logFieldName is logSecretName less authorization: "Authorization:
// Bearer …" is a header, its scheme word kept by logValueREs.
const logFieldName = `[A-Za-z_-]*(?:token|secret|password|passwd|api[_-]?key|apikey|credential|cookie|signature|sig|access[_-]?key|private[_-]?key)`

// logFieldRE: "name": "value" in JSON (an error's response body), and
// name:value in a Go map or struct printed with %+v.
var logFieldRE = lazyre.New(`(?i)((?:^|[{\[,\s(])"?` + logFieldName + `"?\s*:\s*)("(?:[^"\\]|\\.)*"|[^\s,}\])]+)`)

// logUserinfoRE: credentials in a URL (https://user:pass@host).
var logUserinfoRE = lazyre.New(`(://[^/\s:@]+:)[^/\s@]+@`)

// redact blanks every secret in s.
func (r *LogRing) redact(s string) string {
	if s == "" {
		return s
	}
	r.mu.Lock()
	secrets := r.sorted
	r.mu.Unlock()
	return redactWith(s, secrets)
}

// redactWith blanks every secret in s, the configured ones being secrets.
func redactWith(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	for _, re := range logValueREs {
		s = re.ReplaceAllString(s, "${1}"+redacted)
	}
	s = logParamRE.ReplaceAllString(s, "${1}"+redacted)
	s = logFieldRE.ReplaceAllString(s, "${1}"+redacted)
	return logUserinfoRE.ReplaceAllString(s, "${1}"+redacted+"@")
}

// redactKnown blanks only the configured secrets: for text redacted before
// some were known (a logger's With attributes).
func (r *LogRing) redactKnown(s string) string {
	r.mu.Lock()
	secrets := r.sorted
	r.mu.Unlock()
	return redactKnownWith(s, secrets)
}

func redactKnownWith(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	return s
}

func (r *LogRing) add(at time.Time, level slog.Level, msg, attrs string) {
	if at.IsZero() {
		at = r.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	attrs = redactKnownWith(attrs, r.sorted)
	rec := LogRecord{At: at, Level: level.String(), Msg: clipBytes(excerpt(redactWith(msg, r.sorted), logMsgMax), logMsgMax), Attrs: clipBytes(excerpt(attrs, logAttrsMax), logAttrsMax), level: level}
	r.buf[r.next] = rec
	if r.store != nil {
		r.store.keep(rec)
	}
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// LogQuery picks records from the ring.
type LogQuery struct {
	Since, Until time.Time // zero: no bound
	Grep         *regexp.Regexp
	Level        slog.Level // at least this level
	Limit        int        // the newest this many; 0: DefaultLogLimit
	// Newest: only the records are wanted, not how many matched in all, so
	// the log files aren't read once the ring has Limit matches (they are
	// older than all of them). For the polled status, not `wp server log`,
	// which says the total.
	Newest bool
}

// DefaultLogLimit is how many records a query returns unless it says.
const DefaultLogLimit = 500

// MaxLogLimit is the most records one query returns.
const MaxLogLimit = 20000

// query is the newest q.Limit records that match, oldest first, how many
// matched in all, and the oldest record's time. Records older than the
// ring's oldest come from the log files, when it has them (WP-893).
func (r *LogRing) query(q LogQuery) ([]LogRecord, int, time.Time) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	limit = min(limit, MaxLogLimit)
	recs := r.snapshot()
	var out []LogRecord
	matched := 0
	var oldest time.Time
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if oldest.IsZero() || rec.At.Before(oldest) {
			oldest = rec.At
		}
		if !q.matches(rec) {
			continue
		}
		matched++
		if len(out) < limit {
			out = append(out, rec)
		}
	}
	slices.Reverse(out)
	r.mu.Lock()
	st, secrets := r.store, r.sorted
	r.mu.Unlock()
	if st == nil || (!oldest.IsZero() && !q.Since.Before(oldest)) || (q.Newest && len(out) >= limit) {
		return out, matched, oldest
	}
	if !oldest.IsZero() && !q.Until.IsZero() && q.Until.Before(oldest) {
		oldest = time.Time{}
	}
	older := st.read(q, oldest, limit, secrets)
	matched += older.matched
	if room := limit - len(out); room > 0 && len(older.recs) > 0 {
		out = append(older.recs[max(0, len(older.recs)-room):], out...)
	}
	if first := st.oldest(); !first.IsZero() && (oldest.IsZero() || first.Before(oldest)) {
		oldest = first
	}
	return out, matched, oldest
}

// matches says whether rec is one q asks for.
func (q LogQuery) matches(rec LogRecord) bool {
	if rec.level < q.Level || (!q.Since.IsZero() && rec.At.Before(q.Since)) || (!q.Until.IsZero() && rec.At.After(q.Until)) {
		return false
	}
	return q.Grep == nil || q.Grep.MatchString(rec.Msg) || q.Grep.MatchString(rec.Attrs)
}

// snapshot is the records younger than maxAge, in the order kept, copied
// so a query's matching doesn't hold up logging. A record's time is when
// it was made, so one may be older than the one kept before it: each is
// judged by its own.
func (r *LogRing) snapshot() []LogRecord {
	cutoff := r.now().Add(-r.maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]LogRecord, 0, n)
	for i := n; i > 0; i-- {
		rec := r.buf[(r.next-i+len(r.buf))%len(r.buf)]
		if !rec.At.Before(cutoff) {
			out = append(out, rec)
		}
	}
	return out
}

// size is how many records the ring holds now, and the oldest one's time.
func (r *LogRing) size() (int, time.Time) {
	recs := r.snapshot()
	var oldest time.Time
	for _, rec := range recs {
		if oldest.IsZero() || rec.At.Before(oldest) {
			oldest = rec.At
		}
	}
	return len(recs), oldest
}

// files is what the log files hold; nil without them.
func (r *LogRing) files() *LogFiles {
	r.mu.Lock()
	st := r.store
	r.mu.Unlock()
	if st == nil {
		return nil
	}
	out := &LogFiles{Bytes: st.usage(), Days: st.keepDays}
	if at := st.oldest(); !at.IsZero() {
		out.Since = &at
	}
	return out
}

// clipBytes cuts s to at most n bytes, on a character boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ringHandler is a slog.Handler that keeps each record in the ring and
// passes it on.
type ringHandler struct {
	ring  *LogRing
	next  slog.Handler
	attrs string
	group string
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(h.ring.redactKnown(h.attrs))
	rec.Attrs(func(a slog.Attr) bool {
		h.ring.appendAttr(&b, h.group, a)
		return true
	})
	h.ring.add(rec.Time, rec.Level, rec.Message, b.String())
	return h.next.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		h.ring.appendAttr(&b, h.group, a)
	}
	return &ringHandler{ring: h.ring, next: h.next.WithAttrs(as), attrs: b.String(), group: h.group}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &ringHandler{ring: h.ring, next: h.next.WithGroup(name), attrs: h.attrs, group: g}
}

// appendAttr writes a as " key=value", its value redacted.
func (r *LogRing) appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" && key != "" {
		key = prefix + "." + key
	} else if key == "" {
		key = prefix
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			if logKeyRE.MatchString(key) {
				g = slog.String(g.Key, redacted)
			}
			r.appendAttr(b, key, g)
		}
		return
	}
	val := attrText(a.Value)
	if val != "" && logKeyRE.MatchString(key) {
		val = redacted
	} else {
		val = r.redact(val)
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(key)
	b.WriteByte('=')
	if val == "" || strings.ContainsAny(val, " \t\n\r\"=") {
		val = strconv.Quote(val)
	}
	b.WriteString(val)
}

// attrText is a value as text; a struct or map with its field names
// (%+v), so a secret-named field is blanked like any other.
func attrText(v slog.Value) string {
	if v.Kind() != slog.KindAny {
		return v.String()
	}
	switch x := v.Any().(type) {
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case []byte:
		return string(x)
	}
	return fmt.Sprintf("%+v", v.Any())
}

// logSecretColumns are where Waypoint keeps secrets it was given: an AI
// provider's key, a forge's tokens, a webhook's signing secret, an
// automation's incoming-webhook path secret, an assistant's door key and
// password, a person's calendar feed token.
var logSecretColumns = []struct{ table, column string }{
	{"ai_providers", "api_key"},
	{"forges", "token"}, {"forges", "bot_token"}, {"forges", "webhook_secret"},
	{"webhooks", "secret"},
	{"automations", "hook_token"},
	{"assistants", "door_key"}, {"assistants", "password"},
	{"users", "calendar_token"},
}

// LoadLogSecrets has the log ring blank every secret Waypoint keeps. It
// runs at start and every minute: a secret added since is blanked within
// the minute, in the records kept before too (AddSecret rewrites them).
func (s *Service) LoadLogSecrets(ctx context.Context) {
	if s.cfg.Logs == nil {
		jobSkip(ctx, "the server keeps no log to blank secrets in")
		return
	}
	for _, c := range logSecretColumns {
		// #nosec G202 -- table and column names come from the list above, not from input
		rows, err := s.db.R.QueryContext(ctx, `SELECT `+c.column+` FROM `+c.table+` WHERE `+c.column+` != ''`)
		if err != nil {
			s.log.WarnContext(ctx, "log secrets: query failed, these secrets are not blanked in the log", "table", c.table, "column", c.column, "err", err)
			continue
		}
		s.addLogSecrets(ctx, c.table, rows)
	}
	s.blankStoredSecrets(ctx)
}

func (s *Service) addLogSecrets(ctx context.Context, table string, rows *sql.Rows) {
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			s.log.WarnContext(ctx, "log secrets: scan failed", "table", table, "err", err)
			continue
		}
		s.cfg.Logs.AddSecret(v)
	}
	if err := rows.Err(); err != nil {
		s.log.WarnContext(ctx, "log secrets: read failed, some secrets are not blanked in the log", "table", table, "err", err)
	}
}
