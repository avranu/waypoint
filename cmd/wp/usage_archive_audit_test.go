package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Tests for the findings of audit 1218 on the transcript archive.

// ---- redaction ----

func jsonLineOf(t *testing.T, s string) string {
	t.Helper()
	b, err := marshalString(s)
	if err != nil {
		t.Fatal(err)
	}
	return `{"t":` + string(b) + `}` + "\n"
}

// Whatever redact changes in text, the archive changes too when that text
// sits in a JSON string on an archived line: the prefilter must never be
// narrower than redact (audit 1218.1). One row per alternative of secretRE
// and per shape in tokenLikeRE; a new alternative goes in this table.
func TestArchiveRedactionCoversWhatRedactDoes(t *testing.T) {
	rows := []string{
		"AWS_SECRET_ACCESS_KEY=abcd1234wxyz", "export DB_PASSWORD=hunter2hunter", "api-key: abcdef12345",
		"APIKEY=abcdef12345", "private_key=abcd1234efgh", "ACCESS_KEY=abcd1234efgh", "Authorization: Basic dXNlcjpwYXNz",
		"credential=abcd12345", "my_token = abcd1234", "secret: abcd1234",
		"curl --token abc12345", "run --secret abcd1234", "x --password hunter22", "x --passwd hunter22",
		"x --api-key abcd1234", "x --auth abcd1234", "x --access-key abcd1234", "Bearer abcdef123456",
		"postgres://admin:pw1234@db/app",
		"sk-" + strings.Repeat("a1", 12), "ghp_" + strings.Repeat("a1B2", 9), "gho_" + strings.Repeat("a1B2", 9),
		"ghu_" + strings.Repeat("a1B2", 9), "ghs_" + strings.Repeat("a1B2", 9), "ghr_" + strings.Repeat("a1B2", 9),
		"glpat-" + strings.Repeat("a1", 12), "xoxb-" + strings.Repeat("1", 12), "AKIA" + strings.Repeat("A1", 8),
		"eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0NTY3.SflKxwRJSMeKKF2QT4",
	}
	for _, s := range rows {
		if redact(s) == s {
			t.Errorf("the table row %q is not something redact changes", s)
			continue
		}
		line := jsonLineOf(t, "before "+s+" after")
		out, ok := redactTranscriptLineWith([]byte(line), nil)
		if !ok || string(out) == line {
			t.Errorf("redact hides %q in text, but the archive kept it in a JSON string: %q", s, out)
		}
		if !json.Valid(out) {
			t.Errorf("not JSON after redaction: %q", out)
		}
	}
}

// A secret-named key's string is blanked whatever its length, and every
// string inside an array or object under such a key (audit 1218.11, .12).
func TestArchiveRedactionOfSecretNamedKeys(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"password":"abc"}`, `{"password":"***"}`},
		{`{"token":"x","n":1}`, `{"token":"***","n":1}`},
		{`{"password":""}`, `{"password":""}`},
		{`{"cookie":"sid=abc123"}`, `{"cookie":"***"}`},
		{`{"x-auth":"zzzz"}`, `{"x-auth":"***"}`},
		{`{"jwt":"abc.def.ghi"}`, `{"jwt":"***"}`},
		{`{"author":"Robin Smith","authority":"the crown"}`, `{"author":"Robin Smith","authority":"the crown"}`},
		{`{"tokens":["abcdefg","hij"],"n":1}`, `{"tokens":["***","***"],"n":1}`},
		{`{"secret":{"a":"xx","b":{"c":["yy"]},"n":5},"o":"keep it"}`, `{"secret":{"a":"***","b":{"c":["***"]},"n":5},"o":"keep it"}`},
		{`{"secret":["a"],"other":"keep it"}`, `{"secret":["***"],"other":"keep it"}`},
		{`{"usage":{"input_tokens":12,"output_tokens":3},"k":"v"}`, `{"usage":{"input_tokens":12,"output_tokens":3},"k":"v"}`},
		{`{"password":12345678}`, `{"password":12345678}`}, // numbers are left: input_tokens lives under a key named token
	}
	for _, c := range cases {
		out, ok := redactTranscriptLineWith([]byte(c.in+"\n"), nil)
		if !ok || string(out) != c.want+"\n" {
			t.Errorf("redact %s\n got %q\nwant %q", c.in, out, c.want)
		}
	}
}

// ---- a server that can be made to misbehave ----

type archiveTap struct {
	mu       sync.Mutex
	pieces   int
	statuses int
	// onPiece, when set, answers a piece itself and returns true.
	onPiece func(n int, w http.ResponseWriter, r *http.Request) bool
}

func (a *archiveTap) counts() (pieces, statuses int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pieces, a.statuses
}

func tappedEnv(t *testing.T) (*env, *archiveTap) {
	t.Helper()
	tap := &archiveTap{}
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/api/v1/usage/transcript":
				tap.mu.Lock()
				tap.pieces++
				n := tap.pieces
				hook := tap.onPiece
				tap.mu.Unlock()
				if hook != nil && hook(n, w, r) {
					return
				}
			case r.Method == "POST" && r.URL.Path == "/api/v1/usage/transcript/status":
				tap.mu.Lock()
				tap.statuses++
				tap.mu.Unlock()
			}
			h.ServeHTTP(w, r)
		})
	})
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	return e, tap
}

func answerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}

// emptyDir is a Claude config directory with no transcripts yet.
func emptyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects", "-work-app"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func putTranscript(t *testing.T, dir, session string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, "projects", "-work-app", session+".jsonl")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func userLine(s string) string { return `{"type":"user","message":{"content":"` + s + `"}}` }

func manyLines(prefix string, n int) []string {
	var out []string
	for i := range n {
		out = append(out, userLine(fmt.Sprintf("%s %d %s", prefix, i, strings.Repeat("padding ", 12))))
	}
	return out
}

func randomHex(n int) string {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = "0123456789abcdef"[r.IntN(16)]
	}
	return string(b)
}

func smallLimits(t *testing.T, maxLine int, gzipCap int) {
	t.Helper()
	ol, og := archiveMaxLine, archiveGzipCap
	archiveMaxLine, archiveGzipCap = maxLine, gzipCap
	t.Cleanup(func() { archiveMaxLine, archiveGzipCap = ol, og })
}

// ---- piece building (8, 9) ----

// A line over the limit does not discard the lines before it: they go, the
// line becomes a stub that says what it was, and the rest of the file follows.
func TestArchiveOversizeLineIsAStub(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	smallLimits(t, 64<<10, core.TranscriptMaxUpload)
	dir := emptyDir(t)
	huge := userLine(strings.Repeat("x", 100<<10))
	putTranscript(t, dir, "big", userLine("one"), huge, userLine("three"))
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.Dropped["line_too_large"] != 1 || res.Archive.PendingBytes != 0 || res.Archive.Stopped != "" {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	got := strings.Split(strings.TrimRight(archived(t, e, me, "big", "main"), "\n"), "\n")
	if len(got) != 3 || got[0] != userLine("one") || got[2] != userLine("three") {
		t.Fatalf("lines: %q", got)
	}
	var stub struct {
		Type  string `json:"type"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(got[1]), &stub); err != nil || stub.Type != "line-too-large" || stub.Bytes != int64(len(huge)+1) {
		t.Errorf("stub: %q %v", got[1], err)
	}
	if res = archiveSync(t, e, dir); res.Archive.Pieces != 0 || res.Archive.Dropped["line_too_large"] != 0 {
		t.Errorf("second pass: %+v", res.Archive)
	}
}

// The client never builds what the server always refuses: a piece whose
// gzip is over the server's cap is cut to fewer lines, and one line that
// alone is over is a stub (audit 1218.9). The client's limits fit the server's.
func TestArchiveClientLimitsFitTheServers(t *testing.T) {
	if archiveGzipCap > core.TranscriptMaxUpload {
		t.Errorf("gzip cap %d is over the server's %d", archiveGzipCap, core.TranscriptMaxUpload)
	}
	if archivePieceSize+archiveMaxLine > core.TranscriptMaxPiece {
		t.Errorf("a piece can reach %d raw bytes, the server takes %d", archivePieceSize+archiveMaxLine, core.TranscriptMaxPiece)
	}
}

func TestArchiveGzipOverTheCapIsCutOrStubbed(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	smallLimits(t, 1<<20, 4096)
	dir := emptyDir(t)
	var lines []string
	for i := range 20 {
		lines = append(lines, userLine(fmt.Sprintf("%02d%s", i, randomHex(600))))
	}
	lines = append(lines[:10], append([]string{userLine("big" + randomHex(9000))}, lines[10:]...)...)
	putTranscript(t, dir, "cap", lines...)
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.Stopped != "" || res.Archive.PendingBytes != 0 || res.Archive.Pieces < 3 {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if res.Archive.Dropped["line_too_large"] != 1 || res.Archive.Dropped["refused"] != 0 || res.Archive.Dropped["upload_failed"] != 0 {
		t.Errorf("drops: %+v", res.Archive.Dropped)
	}
	got := strings.Split(strings.TrimRight(archived(t, e, me, "cap", "main"), "\n"), "\n")
	if len(got) != 21 || got[10] == lines[10] || !strings.Contains(got[10], "line-too-large") {
		t.Fatalf("archived %d lines, line 10 is %.80q", len(got), got[10])
	}
	for i, l := range got {
		if i != 10 && l != lines[i] {
			t.Errorf("line %d changed", i)
		}
	}
}

// ---- refusals (6) ----

// A refusal that will not change (a 4xx) is its own count, is not a failing
// network, holds the file (its bytes stay waiting) and is tried again once a
// day; a smaller piece is tried once first.
func TestArchivePermanentRefusalHoldsTheFile(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "ref", manyLines("r", 30)...)
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 413, "request body too large")
		return true
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["refused"] != 1 || res.Archive.Dropped["upload_failed"] != 0 || res.Archive.Stopped != "" || res.Archive.PendingBytes == 0 {
		t.Fatalf("first pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if p, _ := tap.counts(); p != 2 {
		t.Errorf("a whole piece and a smaller one were expected, %d requests", p)
	}
	archiveSync(t, e, dir)
	if p, _ := tap.counts(); p != 2 {
		t.Errorf("a held file was tried again at once: %d requests", p)
	}
	st, err := loadArchiveState(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Files {
		if f.Held == "" || f.HeldAt == 0 {
			t.Errorf("not held: %+v", f)
		}
		f.HeldAt = time.Now().Add(-25 * time.Hour).UnixMilli()
	}
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), st); err != nil {
		t.Fatal(err)
	}
	tap.onPiece = nil
	res = archiveSync(t, e, dir)
	if res.Archive.Pieces == 0 || res.Archive.PendingBytes != 0 {
		t.Errorf("a day later: %+v", res.Archive)
	}
}

// When the smaller piece is accepted nothing is held or counted.
func TestArchiveRefusalOfABigPieceRecovers(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	lines := manyLines("s", 40)
	p := putTranscript(t, dir, "small", lines...)
	fi, _ := os.Stat(p)
	tap.onPiece = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		var from, to int64
		_, _ = fmt.Sscan(q.Get("from"), &from)
		_, _ = fmt.Sscan(q.Get("to"), &to)
		if to-from > fi.Size()*3/4 {
			answerError(w, 413, "too big")
			return true
		}
		return false
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["refused"] != 0 || res.Archive.PendingBytes != 0 || res.ArchiveError != "" {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if got := archived(t, e, me, "small", "main"); got != strings.Join(lines, "\n")+"\n" {
		t.Errorf("archived %d bytes", len(got))
	}
}

// A failing network or server is counted as upload_failed and stops the
// pass after three.
func TestArchiveThreeFailuresStopThePass(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	for i := range 6 {
		putTranscript(t, dir, fmt.Sprintf("f%d", i), userLine("hello"), userLine("again"))
	}
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 503, "overloaded")
		return true
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["upload_failed"] != 3 || !strings.Contains(res.Archive.Stopped, "uploads are failing") || res.Archive.Dropped["refused"] != 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
	if p, _ := tap.counts(); p != 3 {
		t.Errorf("%d requests", p)
	}
}

// A runner job's token is refused by the server; the client stops quietly,
// counts nothing and does not tell the server its status (that is refused too).
func TestArchiveJobTokenStopsQuietly(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := fixtureDir(t)
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 403, core.TranscriptJobRefusal)
		return true
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || len(res.Archive.Dropped) != 0 || !strings.Contains(res.Archive.Stopped, "runner") {
		t.Errorf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if p, s := tap.counts(); p != 1 || s != 0 {
		t.Errorf("piece requests %d, status requests %d", p, s)
	}
	st, _ := loadArchiveState(e.dir)
	if len(st.Dropped) != 0 {
		t.Errorf("state counts %v", st.Dropped)
	}
}

// ---- read errors (26, 27) and counting (28) ----

func TestArchiveReadErrorsAreErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(p, []byte(userLine("a")+"\n"+userLine("b")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(userLine("a")+"\n") * 2)
	_ = fh.Close() // every read now fails
	if _, err := tailHash(fh, size); err == nil {
		t.Error("tailHash hid a read error")
	}
	if _, err := firstLineHash(fh, size); err == nil {
		t.Error("firstLineHash hid a read error")
	}
	if _, _, err := readArchivePiece(fh, 0, size, nil, 0); err == nil {
		t.Error("readArchivePiece hid a read error")
	}
}

func TestArchiveUnreadableFileIsCountedAndKeepsItsGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	p := putTranscript(t, dir, "locked", manyLines("l", 5)...)
	archiveSync(t, e, dir)
	before, _ := loadArchiveState(e.dir)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	appendFile := func() {
		_ = os.Chmod(p, 0o600)
		appendTo(t, p, userLine("more")+"\n")
		_ = os.Chmod(p, 0)
	}
	appendFile()
	res := archiveSync(t, e, dir)
	after, _ := loadArchiveState(e.dir)
	if res.Archive.Dropped["unreadable_file"] != 1 || after.Files[p].Gen != before.Files[p].Gen || after.Files[p].Off != before.Files[p].Off {
		t.Errorf("pass: %+v state %+v", res.Archive, after.Files[p])
	}
}

// A redaction failure is counted when the piece is stored, not when it is
// read: a failed upload does not count it again on every retry.
func TestArchiveCountsStubsWhenStored(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	old := archiveRedact
	archiveRedact = func(line []byte, known []string) ([]byte, bool) {
		if bytes.Contains(line, []byte("BOOM")) {
			return stubLine(), false
		}
		return redactTranscriptLineWith(line, known)
	}
	t.Cleanup(func() { archiveRedact = old })
	dir := emptyDir(t)
	putTranscript(t, dir, "boom", userLine("fine"), userLine("BOOM"), userLine("fine too"))
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 503, "later")
		return true
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["redaction_failed"] != 0 {
		t.Errorf("counted before the piece was stored: %+v", res.Archive.Dropped)
	}
	tap.onPiece = nil
	res = archiveSync(t, e, dir)
	if res.Archive.Dropped["redaction_failed"] != 1 || res.Archive.PendingBytes != 0 {
		t.Errorf("stored pass: %+v", res.Archive)
	}
	if got := archived(t, e, me, "boom", "main"); !strings.Contains(got, "redaction-failed") || strings.Contains(got, "BOOM") {
		t.Errorf("archived %q", got)
	}
}

// ---- pass control (5, 13, 14, 15, 16, 17, 18, 19, 38, 42, 43, 48) ----

// Another pass holds the lock: the result and the CLI say so, and the state
// file is not touched (it is read only under the lock).
func TestArchiveLockContention(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	unlock, ok := tryLock(filepath.Join(e.dir, "usage-archive.lock"), time.Hour)
	if !ok {
		t.Fatal("no lock")
	}
	res := archiveSync(t, e, dir)
	if res.Archive == nil || res.Archive.Stopped != "another pass is running" || res.Archive.Pieces != 0 {
		t.Errorf("busy: %+v", res.Archive)
	}
	if _, err := os.Stat(filepath.Join(e.dir, archiveStateFile)); err == nil {
		t.Error("a pass that did not hold the lock wrote the state")
	}
	out := e.wp("usage", "sync", "--all", "--dir", dir)
	if !strings.Contains(out, "another pass is running") || strings.Contains(out, "0 B still waiting") {
		t.Errorf("text: %q", out)
	}
	unlock()
	if res = archiveSync(t, e, dir); res.Archive.Pieces != 2 {
		t.Errorf("after the lock was freed: %+v", res.Archive)
	}
}

// Every pass considers every transcript of the last 30 days, and any file
// with unsent bytes, whatever its age; the hook only puts its session first.
func TestArchiveOneDiscoveryRuleForEveryPass(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	a := putTranscript(t, dir, "aaa", userLine("a1"))
	putTranscript(t, dir, "bbb", userLine("b1"))
	old := putTranscript(t, dir, "old", userLine("o1"))
	longAgo := time.Now().Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(old, longAgo, longAgo)
	archiveSync(t, e, dir) // --all: the old one too
	appendTo(t, old, userLine("o2")+"\n")
	_ = os.Chtimes(old, longAgo, longAgo)
	appendTo(t, a, userLine("a2")+"\n")
	putTranscript(t, dir, "ccc", userLine("c1")) // new, never seen

	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	hookIn := `{"session_id":"aaa","transcript_path":"` + a + `"}`
	usageHook(c, strings.NewReader(hookIn), dir)

	for _, w := range []struct{ session, want string }{{"aaa", "a2"}, {"ccc", "c1"}, {"old", "o2"}} {
		if got := archived(t, e, me, w.session, "main"); !strings.Contains(got, w.want) {
			t.Errorf("after a hook pass %s lacks %s: %q", w.session, w.want, got)
		}
	}
	st, _ := loadArchiveState(e.dir)
	_ = st
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil || len(ov.Reported) != 1 || ov.Reported[0].Transcripts == nil || ov.Reported[0].Transcripts.PendingBytes != 0 {
		t.Errorf("machine's pending after the hook: %+v %v", ov.Reported, err)
	}
}

// The hook pass reports the whole machine's waiting bytes, not its own
// session's: a file that waits elsewhere stays counted.
func TestArchiveHookPassKeepsTheMachinesTotals(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	a := putTranscript(t, dir, "aaa", userLine("a1"))
	putTranscript(t, dir, "bbb", manyLines("b", 20)...)
	tap.onPiece = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("session") == "bbb" {
			answerError(w, 413, "refused")
			return true
		}
		return false
	}
	archiveSync(t, e, dir)
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	usageHook(c, strings.NewReader(`{"session_id":"aaa","transcript_path":"`+a+`"}`), dir)
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil || len(ov.Reported) != 1 || ov.Reported[0].Transcripts == nil || ov.Reported[0].Transcripts.PendingBytes == 0 {
		t.Errorf("the hook pass overwrote the machine's waiting bytes: %+v %v", ov.Reported, err)
	}
}

// The pass stops at its time limit and says so; what waits stays counted.
func TestArchiveStopsAtTheTimeLimit(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	old := archivePassLimit
	archivePassLimit = time.Nanosecond
	t.Cleanup(func() { archivePassLimit = old })
	res := archiveSync(t, e, fixtureDir(t))
	if !strings.Contains(res.Archive.Stopped, "time limit") || res.Archive.Pieces != 0 || res.Archive.PendingBytes == 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
}

// An upload that hangs ends with the pass's deadline (plus a floor), not a
// minute later.
func TestArchiveUploadHasADeadline(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	oldLimit, oldFloor := archivePassLimit, archiveUploadFloor
	archivePassLimit, archiveUploadFloor = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { archivePassLimit, archiveUploadFloor = oldLimit, oldFloor })
	tap.onPiece = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return true
	}
	started := time.Now()
	res := archiveSync(t, e, fixtureDir(t))
	if d := time.Since(started); d > 10*time.Second {
		t.Errorf("the pass took %v", d)
	}
	if res.Archive.Dropped["upload_failed"] == 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
}

// A corrupt state file is scratch: start over, count it, tell the server, and
// store nothing twice.
func TestArchiveCorruptStateStartsOver(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	archiveSync(t, e, dir)
	if err := os.WriteFile(filepath.Join(e.dir, archiveStateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.Dropped["state_reset"] != 1 || res.Archive.Pieces != 0 {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil || len(ov.Reported) != 1 || ov.Reported[0].Transcripts.Dropped["state_reset"] != 1 {
		t.Errorf("server status: %+v %v", ov.Reported, err)
	}
	files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "sess1"})
	for _, f := range files {
		if f.Gen != 0 || f.Pieces != 1 {
			t.Errorf("stored again: %+v", f)
		}
	}
}

// After a rewrite (generation 1) wp loses its memory: the server says where
// its copy ends and wp adopts that, so no third copy is stored.
func TestArchiveLostStateAfterARewriteStoresNoThirdCopy(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	p := putTranscript(t, dir, "rw", userLine("first"), userLine("second"))
	archiveSync(t, e, dir)
	if err := os.WriteFile(p, []byte(userLine("first")+"\n"+userLine("changed")+"\n"+userLine("more")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := archiveSync(t, e, dir); res.Archive.Rewritten != 1 {
		t.Fatalf("rewrite: %+v", res.Archive)
	}
	if err := os.Remove(filepath.Join(e.dir, archiveStateFile)); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Pieces != 0 || res.ArchiveError != "" {
		t.Errorf("after losing state: %+v %q", res.Archive, res.ArchiveError)
	}
	for gen, want := range map[int]int{0: 1, 1: 1, 2: 0} { // no third copy
		g := gen
		files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "rw", Gen: &g})
		if len(files) != want {
			t.Errorf("generation %d: %+v", gen, files)
		}
	}
	appendTo(t, p, userLine("later")+"\n")
	if res = archiveSync(t, e, dir); res.Archive.Pieces != 1 {
		t.Errorf("growth after adopting: %+v", res.Archive)
	}
	if got := archived(t, e, me, "rw", "main"); !strings.HasSuffix(got, userLine("later")+"\n") || strings.Count(got, "changed") != 1 {
		t.Errorf("newest generation: %q", got)
	}
}

// State for a file that no longer exists is dropped.
func TestArchivePrunesStateOfDeletedFiles(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	gone := putTranscript(t, dir, "gone", userLine("x"))
	putTranscript(t, dir, "kept", userLine("y"))
	archiveSync(t, e, dir)
	if st, _ := loadArchiveState(e.dir); len(st.Files) != 2 {
		t.Fatalf("state: %+v", st.Files)
	}
	_ = os.Remove(gone)
	archiveSync(t, e, dir)
	st, _ := loadArchiveState(e.dir)
	if _, ok := st.Files[gone]; ok || len(st.Files) != 1 {
		t.Errorf("state after deleting: %+v", st.Files)
	}
}

// A dry run writes no archive state and makes no transcript request.
func TestArchiveDryRunTouchesNothing(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := fixtureDir(t)
	out := e.wp("usage", "sync", "--all", "--dry-run", "--dir", dir, "--json")
	var res usageSyncResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, archiveStateFile)); err == nil {
		t.Error("a dry run wrote the archive state")
	}
	if p, s := tap.counts(); p != 0 || s != 0 {
		t.Errorf("a dry run made %d piece and %d status requests", p, s)
	}
	if res.Archive == nil || res.Archive.Files == 0 || res.Archive.Pieces != 0 {
		t.Errorf("dry archive: %+v", res.Archive)
	}
}

// ---- rewrites (44) ----

// Claude Code rewrote the bytes before the offset and kept the first line:
// the 64 bytes before the offset notice.
func TestArchiveNoticesChangeBeforeTheOffsetWithTheSameFirstLine(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	lines := manyLines("w", 6)
	p := putTranscript(t, dir, "same", lines...)
	archiveSync(t, e, dir)
	raw, _ := os.ReadFile(p)
	i := len(raw) - 10 // inside the last 64 bytes
	changed := append([]byte(nil), raw...)
	changed[i] = 'Z'
	changed = append(changed, []byte(userLine("grown")+"\n")...)
	if err := os.WriteFile(p, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Rewritten != 1 {
		t.Fatalf("not noticed: %+v", res.Archive)
	}
	if got := archived(t, e, me, "same", "main"); got != string(changed) {
		t.Errorf("newest generation is not the new file")
	}
}

// ---- the CLI ----

func TestArchiveTextNamesTheUnit(t *testing.T) {
	r := &usageSyncResult{Archive: &archiveResult{Dropped: map[string]int64{"upload_failed": 3, "refused": 1}}}
	out := usageSyncText(r)
	if !strings.Contains(out, "3 upload attempts failed") {
		t.Errorf("text: %q", out)
	}
}

// `wp usage transcript --file` writes the stored file; an agent's marker and
// the session headers reach the server as for any other call.
func TestUsageTranscriptFileCommand(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	archiveSync(t, e, dir)
	want := archived(t, e, me, "sess1", "main")
	person, _, err := e.svc.CreateToken(context.Background(), me, core.TokenSpec{Name: "person"}) // reading is for people, not agents
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", person)
	out, code := e.run("usage", "transcript", "sess1", "--file", "main")
	if code != 0 || out != want {
		t.Fatalf("exit %d: %q want %q", code, out, want)
	}
	list := e.wp("usage", "transcript", "sess1")
	if !strings.Contains(list, "main") || !strings.Contains(list, "subagents/agent-abc.jsonl") {
		t.Errorf("list: %q", list)
	}
}

func TestStreamGetSendsTheHeadersOtherCallsDo(t *testing.T) {
	var got http.Header
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/transcript" {
				got = r.Header.Clone()
				w.WriteHeader(204)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	e.login()
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-xyz")
	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-opus-5-5")
	_, _ = e.run("usage", "transcript", "s1", "--file", "main")
	if got == nil {
		t.Fatal("no request")
	}
	for _, h := range []string{"X-Waypoint-Agent", "X-Waypoint-Agent-Env"} {
		if got.Get(h) == "" {
			t.Errorf("streamGet did not send %s: %v", h, got)
		}
	}
}

// A body that ends abnormally is a failure with a message, not a short file.
func TestStreamGetFailsWhenTheBodyBreaks(t *testing.T) {
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/transcript" {
				_, _ = w.Write([]byte(userLine("part") + "\n"))
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}
			h.ServeHTTP(w, r)
		})
	})
	e.login()
	out, code := e.run("usage", "transcript", "s1", "--file", "main")
	if code == 0 || !strings.Contains(out, "incomplete") {
		t.Errorf("exit %d: %q", code, out)
	}
}
