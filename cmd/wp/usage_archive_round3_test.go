package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// Tests for the findings of audit 1233 on the transcript archive client.

// hexLine is a line of incompressible text, so a piece's gzip is about half
// its raw size whatever the neighbours are.
func hexLine(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, 99))
	b := make([]byte, n)
	for i := range b {
		b[i] = "0123456789abcdef"[r.IntN(16)]
	}
	return userLine(string(b))
}

func lowerVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// ---- 1, 2, 20, 26: redaction ----

// Each row names the part that must not survive, in text and in a JSON string
// of an archived line (audit 1233.1, .2).
func TestRedactionRowsHideTheirSecret(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAx9dummykeybody\nabcdefg1234567\n-----END RSA PRIVATE KEY-----"
	rows := []struct{ in, secret string }{
		{"Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA=="},
		{"Authorization: Token ab12cd34ef56", "ab12cd34ef56"},
		{"authorization: Negotiate YIIBnQYGKwYBBQUCoIIBkTCCAY2g", "YIIBnQYGKwYBBQUC"},
		{"Authorization: Bearer abcdef123456", "abcdef123456"},
		{"Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261121/us-east-1/s3/aws4_request", "AKIDEXAMPLE"},
		{`curl -H "Authorization: ApiKey zz99yy88xx77" http://x`, "zz99yy88xx77"},
		{`curl -H 'Authorization: Digest username="u1", realm="r", nonce="n0nce1234", response="deadbeefcafe"' http://x`, "deadbeefcafe"},
		{`curl -H 'Authorization: Digest username="u1", realm="r", nonce="n0nce1234", response="deadbeefcafe"' http://x`, "n0nce1234"},
		{"before " + pem + " after", "MIIEowIBAAKCAQEAx9dummykeybody"},
		{"before " + pem + " after", "abcdefg1234567"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----", "b3BlbnNzaC1rZXktdjEAAAAA"},
		{"-----BEGIN EC PRIVATE KEY-----\necbodyecbodyecbody\n-----END EC PRIVATE KEY-----", "ecbodyecbodyecbody"},
		{"-----BEGIN PRIVATE KEY-----\nplainbodyplainbody\n-----END PRIVATE KEY-----", "plainbodyplainbody"},
		{"-----BEGIN ENCRYPTED PRIVATE KEY-----\nencbodyencbody12\n-----END ENCRYPTED PRIVATE KEY-----", "encbodyencbody12"},
		{"cut off: -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAcutoffbody", "MIIEowIBAAKCAQEAcutoffbody"},
		{"AWS_SECRET_ACCESS_KEY=abcd1234wxyz", "abcd1234wxyz"},
		{"postgres://admin:pw1234@db/app", "pw1234"},
	}
	for _, r := range rows {
		if got := redact(r.in); strings.Contains(got, r.secret) {
			t.Errorf("redact left %q in %q", r.secret, got)
		}
		line := jsonLineOf(t, r.in)
		out, ok := redactTranscriptLineWith([]byte(line), nil)
		if !ok || !json.Valid(out) {
			t.Errorf("%q: ok %v, valid %v", r.in, ok, json.Valid(out))
		}
		var m map[string]string
		if err := json.Unmarshal(out, &m); err != nil || strings.Contains(m["t"], r.secret) {
			t.Errorf("the archive left %q in %q (%v)", r.secret, out, err)
		}
	}
	// plain prose is not blanked further than before
	if got := redact("authorization: none"); got != "authorization: ***" {
		t.Errorf("prose: %q", got)
	}
	if got := redact("the authorization header is required"); got != "the authorization header is required" {
		t.Errorf("prose without a value: %q", got)
	}
	// and a block cut off by the end of the string loses only its body
	if got := redact("x -----BEGIN RSA PRIVATE KEY-----\nabc"); !strings.HasPrefix(got, "x -----BEGIN RSA PRIVATE KEY-----") || strings.Contains(got, "abc") {
		t.Errorf("cut-off block: %q", got)
	}
}

// A hint written with JSON escapes is still found (audit 1233.20).
func TestMayHoldSecretDecodesEscapes(t *testing.T) {
	for _, lit := range []string{`postgres:\/\/admin:pw1234@db\/app`, `token: abcd1234`, `sk-` + strings.Repeat("a1", 12)} {
		if !mayHoldSecret([]byte(lit), nil) {
			t.Errorf("hint missed in %q", lit)
		}
	}
	if mayHoldSecret([]byte(`plain text with a \n in it`), nil) {
		t.Error("plain text with an escape is not a candidate")
	}
	line := `{"t":"run postgres:\/\/admin:pw1234@db\/app now"}` + "\n"
	out, ok := redactTranscriptLineWith([]byte(line), nil)
	if !ok || bytes.Contains(out, []byte("pw1234")) {
		t.Errorf("escaped URL kept its password: %q", out)
	}
	line = `{"t":"token: abcd1234efgh"}` + "\n"
	if out, _ := redactTranscriptLineWith([]byte(line), nil); bytes.Contains(out, []byte("abcd1234efgh")) {
		t.Errorf("escaped hint kept its value: %q", out)
	}
}

// The patterns are pinned so that a changed or added alternative cannot go in
// without someone looking at the table (audit 1233.26).
func TestRedactionPatternsArePinned(t *testing.T) {
	pins := []struct{ name, got, want string }{
		{"secretRE", secretRE.String(), `(?i)([\w-]*(?:token|secret|passw(?:or)?d|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|credential)[\w-]*["']?\s*[=:]\s*|--(?:token|secret|password|passwd|api[_-]?key|auth|access[_-]?key)\s+|bearer\s+)(["']?)(?:(?:bearer|basic|token|negotiate|ntlm)\s+)?[^\s"']{4,}`},
		{"digestAuthRE", digestAuthRE.String(), `(?i)(\bauthorization["']?\s*[=:]\s*["']?digest\s+)[^\n']+`},
		{"schemeAuthRE", schemeAuthRE.String(), `(?i)(\bauthorization["']?\s*[=:]\s*["']?)[\w-]+\s+[^\s"']{4,}`},
		{"tokenLikeRE", tokenLikeRE.String(), `\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|AIza[A-Za-z0-9_-]{35,}|npm_[A-Za-z0-9]{36,}|pypi-[A-Za-z0-9_-]{16,}|xapp-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,})`},
		{"urlPasswordRE", urlPasswordRE.String(), `(://[^\s/:@]+:)[^\s/@]+@`},
		{"urlUserRE", urlUserRE.String(), `(://)[^\s/:@]{20,}@`},
		{"pemKeyRE", pemKeyRE.String(), `(-----BEGIN [A-Z ]*PRIVATE KEY-----)(?s:.*?)(-----END [A-Z ]*PRIVATE KEY-----|\z)`},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s changed. A changed pattern needs a row in the redaction table (TestArchiveRedactionCoversWhatRedactDoes) and a hint in secretHints; then update this pin.\n got %s\nwant %s", p.name, p.got, p.want)
		}
	}
}

// ---- 3: an archive-off server ----

// A 501 says the server keeps no archive: the pass stops with that reason,
// counts nothing, holds nothing, and a hook's passes leave it alone for an
// hour; a manual sync tries again.
func TestArchiveOffServerStopsQuietlyAndHookPassesSkip(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	a := putTranscript(t, dir, "a", userLine("a1"))
	putTranscript(t, dir, "b", userLine("b1"))
	ms, now := int64(10), time.Now()
	fakePasses(t, &ms, &now)
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 501, core.ErrTranscriptsOff.Error())
		return true
	}
	res := archiveSync(t, e, dir)
	if p, _ := tap.counts(); p != 1 {
		t.Errorf("tried %d pieces after the server said it keeps none", p)
	}
	if !strings.Contains(res.Archive.Stopped, "keeps no transcript archive") || len(res.Archive.Dropped) != 0 || res.ArchiveError != "" || res.Archive.PendingBytes == 0 {
		t.Errorf("pass %+v err %q", res.Archive, res.ArchiveError)
	}
	st, _ := loadArchiveState(e.dir)
	if len(st.Dropped) != 0 || st.NoneAt == 0 {
		t.Errorf("state: dropped %v none_at %d", st.Dropped, st.NoneAt)
	}
	for p, f := range st.Files {
		if f.Held != "" {
			t.Errorf("%s was held: %q", p, f.Held)
		}
	}
	if _, s := tap.counts(); s != 0 {
		t.Errorf("a status was posted to a server with no archive: %d", s)
	}
	if out := usageSyncText(&usageSyncResult{Archive: res.Archive}); !strings.Contains(out, "keeps no transcript archive") {
		t.Errorf("text: %q", out)
	}
	hook := func() {
		t.Helper()
		c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
		usageHook(c, strings.NewReader(`{"session_id":"a","transcript_path":"`+a+`"}`), dir)
	}
	now = now.Add(10 * time.Minute)
	hook()
	if p, _ := tap.counts(); p != 1 {
		t.Errorf("a hook pass within the hour tried an upload: %d", p)
	}
	if res := archiveSync(t, e, dir); !strings.Contains(res.Archive.Stopped, "keeps no") {
		t.Errorf("a manual sync did not try: %+v", res.Archive)
	}
	if p, _ := tap.counts(); p != 2 {
		t.Errorf("manual sync: %d pieces in all", p)
	}
	now = now.Add(archiveFullHold)
	hook()
	if p, _ := tap.counts(); p != 3 {
		t.Errorf("a hook pass after the hold: %d pieces in all", p)
	}
}

// ---- 5, 17: the hook's deadline is real ----

func TestHookPassBoundFitsTheHookTimeout(t *testing.T) {
	// the hour counts' upload counts from the sync's start, as the archive's
	// limit does, so it must lie inside that limit (audit 1234.2)
	if usageHookUploadTimeout > archiveHookLimit {
		t.Errorf("the hour-count upload timeout %v is past the archive's limit %v", usageHookUploadTimeout, archiveHookLimit)
	}
	if usageHookUploadBudget > archiveHookLimit || usageHookUploadTimeout > usageHookUploadBudget {
		t.Errorf("the hour-count budget %v must lie between one upload (%v) and the archive's limit (%v)", usageHookUploadBudget, usageHookUploadTimeout, archiveHookLimit)
	}
	total := max(archiveHookLimit, usageHookUploadBudget) + archiveHookFloor + archiveReportHookTimeout + archiveHookMargin
	if hook := time.Duration(usageHookTimeout) * time.Second; total >= hook {
		t.Errorf("hook limit %v + floor %v + report %v = %v, not under the hook's %v timeout",
			archiveHookLimit, archiveHookFloor, archiveReportHookTimeout, total, hook)
	}
	settings := map[string]any{}
	if _, _, err := installUsageIntegration(settings, "/bin/wp", false); err != nil {
		t.Fatal(err)
	}
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if fmt.Sprint(stop["timeout"]) != fmt.Sprint(usageHookTimeout) {
		t.Errorf("the installed hook's timeout is %v, the bound assumes %d", stop["timeout"], usageHookTimeout)
	}
}

func hookCLI(t *testing.T) *cli {
	t.Helper()
	c, err := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A hook pass uses the hook's limit and floor, so a stalled piece ends the
// pass on time (the pass limit and the upload floor are far longer here).
func TestHookPassUsesTheHookLimit(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	for i := range 3 {
		putTranscript(t, dir, fmt.Sprintf("s%d", i), userLine("hello"), userLine("again"))
	}
	lowerVar(t, &archiveHookLimit, 300*time.Millisecond)
	lowerVar(t, &archiveHookFloor, 150*time.Millisecond)
	tap.onPiece = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return true
	}
	started := time.Now()
	res, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Errorf("the hook pass took %v", d)
	}
	if res.Archive.Dropped["upload_failed"] == 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
	if p, _ := tap.counts(); p > 2 {
		t.Errorf("%d pieces were tried after the deadline", p)
	}
}

// The hook pass's clock starts when the sync starts, not when the archive does.
func TestHookDeadlineCountsFromTheSyncStart(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	res, err := usageArchive(hookCLI(t), usageSyncOptions{Hook: true}, fixtureDir(t), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stopped, "time limit") || res.Pieces != 0 {
		t.Errorf("pass: %+v", res)
	}
}

// The status post is bounded for a hook pass.
func TestHookStatusPostIsBounded(t *testing.T) {
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/transcript/status" {
				select {
				case <-r.Context().Done():
				case <-time.After(20 * time.Second):
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	lowerVar(t, &archiveReportHookTimeout, 200*time.Millisecond)
	started := time.Now()
	res, err := usageSync(hookCLI(t), usageSyncOptions{Hook: true, Dir: fixtureDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Errorf("the hook pass waited %v on the status post", d)
	}
	if res.ArchiveError == "" {
		t.Errorf("a status post that timed out was not reported: %+v", res.Archive)
	}
}

// ---- 7, 9: a proxy's body limit, and the piece cap ----

// bodyLimit answers 413 with an HTML body to a piece whose body is over n
// bytes, as a reverse proxy would, and counts the refusals per session.
func bodyLimit(n *int, refused map[string]int, mu *sync.Mutex) func(int, http.ResponseWriter, *http.Request) bool {
	return func(_ int, w http.ResponseWriter, r *http.Request) bool {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		if len(b) > *n {
			mu.Lock()
			refused[r.URL.Query().Get("session")]++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(413)
			_, _ = io.WriteString(w, "<html><body><h1>413 Request Entity Too Large</h1></body></html>")
			return true
		}
		return false
	}
}

func hexLines(base uint64, n, size int) []string {
	var out []string
	for i := range n {
		out = append(out, hexLine(base+uint64(i), size))
	}
	return out
}

func TestArchiveLearnsAPieceCapFrom413(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	lowerVar(t, &archivePieceSize, 64<<10)
	l1, l2 := hexLines(100, 40, 400), hexLines(200, 40, 400)
	pa := putTranscript(t, dir, "aaa", l1...)
	putTranscript(t, dir, "bbb", l2...)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(pa, old, old) // bbb first
	limit, refused, mu := 3000, map[string]int{}, sync.Mutex{}
	tap.onPiece = bodyLimit(&limit, refused, &mu)

	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.PendingBytes != 0 || res.Archive.Stopped != "" || res.Archive.Dropped["refused"] != 0 || res.Archive.Dropped["upload_failed"] != 0 {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	if got := archived(t, e, me, "bbb", "main"); got != strings.Join(l2, "\n")+"\n" {
		t.Errorf("bbb archived %d bytes", len(got))
	}
	if got := archived(t, e, me, "aaa", "main"); got != strings.Join(l1, "\n")+"\n" {
		t.Errorf("aaa archived %d bytes", len(got))
	}
	if refused["bbb"] == 0 || refused["aaa"] != 0 {
		t.Errorf("the cap is kept across files: refusals %v", refused)
	}
	st, _ := loadArchiveState(e.dir)
	if st.PieceCap <= 0 || st.PieceCapAt == 0 {
		t.Fatalf("the cap was not saved: %+v", st)
	}
	for p, f := range st.Files {
		if f.Held != "" {
			t.Errorf("%s held: %q", p, f.Held)
		}
	}
	if len(st.Dropped) != 0 {
		t.Errorf("counted: %v", st.Dropped)
	}

	// the next pass starts from the cap
	before := refused["bbb"] + refused["aaa"]
	appendTo(t, pa, strings.Join(hexLines(300, 20, 400), "\n")+"\n")
	if res = archiveSync(t, e, dir); res.Archive.PendingBytes != 0 || res.Archive.Pieces == 0 {
		t.Fatalf("second pass: %+v", res.Archive)
	}
	if refused["bbb"]+refused["aaa"] != before {
		t.Errorf("the second pass was refused again: %v", refused)
	}

	// a day later the cap is forgotten, so a raised limit is found
	st, _ = loadArchiveState(e.dir)
	st.PieceCapAt = time.Now().Add(-25 * time.Hour).UnixMilli()
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), st); err != nil {
		t.Fatal(err)
	}
	limit = 1 << 30
	appendTo(t, pa, strings.Join(hexLines(400, 20, 400), "\n")+"\n")
	before = refused["bbb"] + refused["aaa"]
	if res = archiveSync(t, e, dir); res.Archive.PendingBytes != 0 || res.Archive.Pieces != 1 {
		t.Fatalf("third pass: %+v", res.Archive)
	}
	st, _ = loadArchiveState(e.dir)
	if st.PieceCap != 0 || refused["bbb"]+refused["aaa"] != before {
		t.Errorf("an old cap stayed: %d", st.PieceCap)
	}
}

// Only a single line still refused with 413 holds the file, and the held
// reason names the likely cause.
func TestArchiveSingleLine413HoldsWithTheLikelyCause(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "x", manyLines("r", 30)...)
	limit, refused, mu := 10, map[string]int{}, sync.Mutex{}
	tap.onPiece = bodyLimit(&limit, refused, &mu)
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["refused"] != 1 || res.Archive.PendingBytes == 0 {
		t.Fatalf("pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	st, _ := loadArchiveState(e.dir)
	for _, f := range st.Files {
		if !strings.Contains(f.Held, "client_max_body_size") || !strings.Contains(f.Held, "413") {
			t.Errorf("held reason: %q", f.Held)
		}
	}
}

// After a refused multi-line piece and a stored smaller one, later pieces are
// full size again (audit 1233.9).
func TestArchiveNon413RefusalDoesNotCapLaterPieces(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	lowerVar(t, &archivePieceSize, 2048)
	lines := manyLines("c", 60)
	putTranscript(t, dir, "cap", lines...)
	var mu sync.Mutex
	var sizes []int64
	tap.onPiece = func(n int, w http.ResponseWriter, r *http.Request) bool {
		var from, to int64
		_, _ = fmt.Sscan(r.URL.Query().Get("from"), &from)
		_, _ = fmt.Sscan(r.URL.Query().Get("to"), &to)
		mu.Lock()
		sizes = append(sizes, to-from)
		mu.Unlock()
		if n == 1 {
			answerError(w, 422, "unprocessable")
			return true
		}
		return false
	}
	res := archiveSync(t, e, dir)
	if res.Archive.PendingBytes != 0 || res.Archive.Dropped["refused"] != 0 || len(sizes) < 4 {
		t.Fatalf("pass: %+v sizes %v", res.Archive, sizes)
	}
	if sizes[2] < sizes[1]*3/2 {
		t.Errorf("a piece after the shrunk one is still capped: %v", sizes)
	}
	if got := archived(t, e, me, "cap", "main"); got != strings.Join(lines, "\n")+"\n" {
		t.Errorf("archived %d bytes", len(got))
	}
}

// ---- 8: bytes appended while another pass holds the lock ----

func TestArchiveBusyPassMarksAndHolderSendsLateBytes(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	p := putTranscript(t, dir, "live", userLine("first"))
	var busy *archiveResult
	tap.onPiece = func(n int, _ http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			appendTo(t, p, userLine("the final turn")+"\n")
			var err error
			busy, err = usageArchive(hookCLI(t), usageSyncOptions{Hook: true, Dir: dir}, dir, time.Now())
			if err != nil {
				t.Error(err)
			}
		}
		return false
	}
	res := archiveSync(t, e, dir)
	if busy == nil || busy.Stopped != archiveBusy {
		t.Fatalf("the second pass was not busy: %+v", busy)
	}
	got := archived(t, e, me, "live", "main")
	if !strings.Contains(got, "the final turn") {
		t.Errorf("late bytes were sent by neither pass: %q", got)
	}
	if res.Archive.PendingBytes != 0 {
		t.Errorf("pending after the second look: %+v", res.Archive)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "usage-archive.again")); err == nil {
		t.Error("the marker was left behind")
	}
}

// A marker with no holder breaks nothing and does not make a pass loop.
func TestArchiveStaleAgainMarkerIsHarmless(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "x", userLine("one"))
	if err := os.WriteFile(filepath.Join(e.dir, "usage-archive.again"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.Pieces != 1 || res.Archive.PendingBytes != 0 {
		t.Errorf("pass: %+v %q", res.Archive, res.ArchiveError)
	}
	if p, _ := tap.counts(); p != 1 {
		t.Errorf("%d pieces", p)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "usage-archive.again")); err == nil {
		t.Error("the marker was left behind")
	}
}

// ---- 16: the documented blind spot ----

func TestArchiveDoesNotNoticeAMiddleEditThatKeepsTheChecks(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	lines := manyLines("m", 10)
	p := putTranscript(t, dir, "mid", lines...)
	archiveSync(t, e, dir)
	raw, _ := os.ReadFile(p)
	edited := bytes.Replace(raw, []byte("m 4 "), []byte("X 4 "), 1) // same size, the middle, not the first line or the last 64 bytes
	if bytes.Equal(raw, edited) {
		t.Fatal("nothing edited")
	}
	edited = append(edited, []byte(userLine("grown")+"\n")...)
	if err := os.WriteFile(p, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Rewritten != 0 {
		t.Errorf("the blind spot closed (update the comment in usage_archive.go and this test): %+v", res.Archive)
	}
	got := archived(t, e, me, "mid", "main")
	if !strings.Contains(got, "m 4 ") || strings.Contains(got, "X 4 ") || !strings.HasSuffix(got, userLine("grown")+"\n") {
		t.Errorf("archived: %.200q", got)
	}
}

// ---- 21: piece count cap, out of sync ----

func TestArchivePieceCountCapStopsThePassAndTheRestGoesNext(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	lowerVar(t, &archivePieceSize, 1024)
	lowerVar(t, &archiveMaxPieces, 2)
	lines := manyLines("p", 40)
	putTranscript(t, dir, "many", lines...)
	res := archiveSync(t, e, dir)
	if res.Archive.Pieces != 2 || !strings.Contains(res.Archive.Stopped, "more pieces than one pass sends") || res.Archive.PendingBytes == 0 {
		t.Fatalf("first pass: %+v", res.Archive)
	}
	for i := 0; i < 20 && res.Archive.PendingBytes > 0; i++ {
		res = archiveSync(t, e, dir)
	}
	if res.Archive.PendingBytes != 0 {
		t.Fatalf("never finished: %+v", res.Archive)
	}
	if got := archived(t, e, me, "many", "main"); got != strings.Join(lines, "\n")+"\n" {
		t.Errorf("archived %d bytes", len(got))
	}
}

// not-stored with have equal to the offset and a generation no newer: the
// server and the client disagree and neither can move.
func TestArchiveOutOfSyncIsCounted(t *testing.T) {
	e, tap := tappedEnv(t)
	e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "x", userLine("one"), userLine("two"))
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"duplicate","have":0,"gen":0}`)
		return true
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["out_of_sync"] != 1 || res.Archive.Pieces != 0 || res.Archive.PendingBytes == 0 {
		t.Errorf("pass: %+v", res.Archive)
	}
	if p, _ := tap.counts(); p != 1 {
		t.Errorf("%d requests", p)
	}
}

// ---- 22: nested sub-agent ----

func TestArchiveFileKeyOfANestedSubagent(t *testing.T) {
	f := usageFile{Path: filepath.Join("/c", "projects", "-w", "sess9", "subagents", "workflows", "wf1", "agent-x.jsonl"), Session: "sess9", Kind: "agent"}
	if got := archiveFileKey(f); got != "subagents/workflows/wf1/agent-x.jsonl" {
		t.Errorf("key %q", got)
	}
	if got := archiveFileKey(usageFile{Path: "/c/projects/-w/sess9/subagents/agent-y.jsonl", Session: "sess9"}); got != "subagents/agent-y.jsonl" {
		t.Errorf("flat key %q", got)
	}
}

func TestArchiveNestedSubagentEndToEnd(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	putTranscript(t, dir, "sess9", userLine("main"))
	nd := filepath.Join(dir, "projects", "-work-app", "sess9", "subagents", "workflows", "wf1")
	if err := os.MkdirAll(nd, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(nd, "agent-x.jsonl")
	if err := os.WriteFile(nested, []byte(userLine("nested one")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" || res.Archive.Files != 2 || res.Archive.PendingBytes != 0 {
		t.Fatalf("pass: %+v %q", res.Archive, res.ArchiveError)
	}
	if got := archived(t, e, me, "sess9", "subagents/workflows/wf1/agent-x.jsonl"); got != userLine("nested one")+"\n" {
		t.Errorf("nested file: %q", got)
	}
}

// ---- 23: an unreadable file, whoever runs the test ----

// A path that is a directory opens but cannot be read, with or without root.
func TestArchiveUnreadableFileCountedWithoutChmod(t *testing.T) {
	e := newEnv(t)
	e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := emptyDir(t)
	putTranscript(t, dir, "ok", userLine("fine"))
	bad := filepath.Join(t.TempDir(), "unreadable.jsonl")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), archiveState{Files: map[string]*archiveFileState{bad: {Off: 1, Gen: 3}}}); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Dropped["unreadable_file"] != 1 || res.Archive.Pieces != 1 {
		t.Errorf("pass: %+v", res.Archive)
	}
	st, _ := loadArchiveState(e.dir)
	if f := st.Files[bad]; f == nil || f.Gen != 3 || f.Off != 1 {
		t.Errorf("an unreadable file's place was lost: %+v", f)
	}
}
