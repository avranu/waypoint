package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
)

// A handler that fails after bytes are out must end the transfer as broken:
// the client sees an error, not a body that ended as if it were whole
// (audit 1218.4). Other panics keep the middleware's behaviour.
func TestAbortedResponseIsABrokenTransfer(t *testing.T) {
	rl := &recordedLog{}
	s := &Server{done: make(chan struct{}), log: slog.New(rl), abandonedAfter: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/abort", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("part of it\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	ts := httptest.NewServer(s.middleware(mux))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/abort")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("the body ended cleanly after %q", b)
	}
	if got := rl.above(0); len(got) == 0 {
		t.Error("the abort was not logged")
	}
}

type transcriptAPI struct {
	t     *testing.T
	ts    *httptest.Server
	svc   *core.Service
	dir   string
	human *client
}

func newTranscriptAPI(t *testing.T) *transcriptAPI { return newTranscriptAPICapped(t, 0) }

// newTranscriptAPICapped is a server whose archive holds at most maxMB (0: no cap).
func newTranscriptAPICapped(t *testing.T, maxMB int) *transcriptAPI {
	t.Helper()
	return newTranscriptAPIIn(t, t.TempDir(), maxMB)
}

// newTranscriptAPIIn is a server whose archive is dir ("": it keeps none).
func newTranscriptAPIIn(t *testing.T, dir string, maxMB int) *transcriptAPI {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	svc := core.New(d, core.Config{BaseURL: "http://wp.test", TranscriptDir: dir, TranscriptMaxMB: maxMB}, nil)
	svc.StartHub()
	ts := httptest.NewServer(New(svc, Config{BaseURL: "http://wp.test", Version: "test"}, svc.Log()).Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(svc.Wait)
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "robin", false, 0)
	return &transcriptAPI{t: t, ts: ts, svc: svc, dir: dir, human: &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}}
}

func tgz(t *testing.T, s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

func (a *transcriptAPI) req(method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest(method, a.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.human.token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func (a *transcriptAPI) put(session string, from int64, lines string) int {
	a.t.Helper()
	q := url.Values{"machine": {"box"}, "session": {session}, "file": {"main"}, "kind": {"main"}, "fp": {"h#0"},
		"from": {itoa(int(from))}, "to": {itoa(int(from) + len(lines))}, "redacted": {"1"}, "version": {"t"}}
	resp, err := a.req("POST", "/api/v1/usage/transcript?"+q.Encode(), tgz(a.t, lines), nil)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The four routes end to end: a piece goes in, the list and the file come
// back, an agent's shell is refused, and a stored piece that was damaged
// breaks the transfer after the first bytes instead of ending it as whole
// (audit 1218.40).
func TestTranscriptRoutes(t *testing.T) {
	a := newTranscriptAPI(t)
	l1, l2 := `{"n":1}`+"\n", `{"n":2}`+"\n"
	if code := a.put("s1", 0, l1); code != 200 {
		t.Fatalf("piece 1: %d", code)
	}
	if code := a.put("s1", int64(len(l1)), l2); code != 200 {
		t.Fatalf("piece 2: %d", code)
	}
	st, _ := json.Marshal(core.TranscriptStatus{Machine: "box", PendingBytes: 3, PendingFiles: 1})
	if resp, err := a.req("POST", "/api/v1/usage/transcript/status", st, map[string]string{"Content-Type": "application/json"}); err != nil || resp.StatusCode/100 != 2 {
		t.Fatalf("status: %v %v", resp, err)
	}

	var files []core.TranscriptFile
	if code := a.human.json("GET", "/api/v1/usage/transcripts?session=s1", nil, &files); code != 200 || len(files) != 1 || files[0].Pieces != 2 || !files[0].Complete {
		t.Fatalf("list: %d %+v", code, files)
	}
	resp, err := a.req("GET", "/api/v1/usage/transcript?session=s1&file=main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != l1+l2 || resp.StatusCode != 200 {
		t.Fatalf("read: %d %q %v", resp.StatusCode, b, err)
	}
	if code, _ := a.human.do("GET", "/api/v1/usage/transcript?session=s1", nil); code != 400 {
		t.Errorf("no file: %d", code)
	}
	if code, _ := a.human.do("GET", "/api/v1/usage/transcript?session=nope&file=main", nil); code != 404 {
		t.Errorf("unknown session: %d", code)
	}
	// an agent's shell (the person's token, the agent's header) reads none
	for _, path := range []string{"/api/v1/usage/transcripts?session=s1", "/api/v1/usage/transcript?session=s1&file=main"} {
		resp, err := a.req("GET", path, nil, map[string]string{"X-Waypoint-Agent-Env": "claude"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || strings.Contains(string(body), `"n":1`) {
			t.Errorf("%s from an agent's shell: %d %q", path, resp.StatusCode, body)
		}
	}
	// damage the second piece: the first goes out, then the transfer breaks
	var second string
	_ = filepath.WalkDir(a.dir, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && strings.Contains(d.Name(), "-0-8-16-") {
			second = p
		}
		return nil
	})
	if second == "" {
		t.Fatal("the second piece is not on disk")
	}
	if err := os.WriteFile(second, tgz(t, "damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err = a.req("GET", "/api/v1/usage/transcript?session=s1&file=main", nil, nil)
	if err != nil {
		return // the connection broke before the headers: not a whole body either
	}
	b, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Errorf("a damaged piece ended the body as whole: %q", b)
	}
}

// A full archive answers 507 with the stable words the client tells apart
// from every other failure, and a piece's redaction counts arrive in the list
// (audit 1218.33, .50).
func TestTranscriptQuotaAnswers507AndRedactionsArrive(t *testing.T) {
	a := newTranscriptAPICapped(t, 1)
	l1 := `{"n":1}` + "\n"
	q := url.Values{"machine": {"box"}, "session": {"s1"}, "file": {"main"}, "kind": {"main"}, "fp": {"h#0"},
		"from": {"0"}, "to": {itoa(len(l1))}, "redacted": {"1"}, "version": {"t"}, "redactions": {"3"}, "redaction_rules": {`{"key":2,"pattern":1}`}}
	resp, err := a.req("POST", "/api/v1/usage/transcript?"+q.Encode(), tgz(t, l1), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("piece: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	var files []core.TranscriptFile
	if code := a.human.json("GET", "/api/v1/usage/transcripts?session=s1", nil, &files); code != 200 || len(files) != 1 ||
		files[0].Redactions == nil || *files[0].Redactions != 3 || files[0].RedactionRules["key"] != 2 {
		t.Fatalf("list: %d %+v", code, files)
	}
	big := make([]byte, 1100<<10)
	_, _ = rand.Read(big)
	big = append(bytes.ReplaceAll(big, []byte("\n"), []byte("x")), '\n')
	q.Set("from", itoa(len(l1)))
	q.Set("to", itoa(len(l1)+len(big)))
	resp, err = a.req("POST", "/api/v1/usage/transcript?"+q.Encode(), tgz(t, string(big)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 507 || !strings.Contains(string(body), core.TranscriptQuotaRefusal) {
		t.Errorf("full archive: %d %s", resp.StatusCode, body)
	}
}

// With no archive (WAYPOINT_TRANSCRIPT_DIR=off) an upload and a read answer
// 501 with the error's own words, not a 500, and the client can tell it apart
// (audit 1233.3).
func TestTranscriptsOffAnswers501(t *testing.T) {
	a := newTranscriptAPIIn(t, "", 0)
	if code := a.put("s1", 0, `{"n":1}`+"\n"); code != http.StatusNotImplemented {
		t.Errorf("ingest: %d, want 501", code)
	}
	resp, err := a.req("POST", "/api/v1/usage/transcript?"+url.Values{"machine": {"box"}, "session": {"s1"}, "file": {"main"}, "kind": {"main"}, "fp": {"h#0"}, "from": {"0"}, "to": {"1"}}.Encode(), tgz(t, "x\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), core.ErrTranscriptsOff.Error()) {
		t.Errorf("ingest body: %s", body)
	}
	code, body := a.human.do("GET", "/api/v1/usage/transcript?session=s1&file=main", nil)
	if code != http.StatusNotImplemented || !strings.Contains(string(body), core.ErrTranscriptsOff.Error()) {
		t.Errorf("read: %d %s", code, body)
	}
}

// The read routes pick what the query names, and a number that is not one is
// a 400 (audit 1233.19).
func TestTranscriptQueryAndNumbers(t *testing.T) {
	a := newTranscriptAPI(t)
	put := func(machine, session, fp string, from int64, lines string) {
		t.Helper()
		q := url.Values{"machine": {machine}, "session": {session}, "file": {"main"}, "kind": {"main"}, "fp": {fp},
			"from": {itoa(int(from))}, "to": {itoa(int(from) + len(lines))}, "redacted": {"1"}, "version": {"t"}}
		resp, err := a.req("POST", "/api/v1/usage/transcript?"+q.Encode(), tgz(t, lines), nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("put %s %s: %v %v", machine, fp, resp, err)
		}
		_ = resp.Body.Close()
	}
	// two machines hold s1; box has two generations (a new fingerprint base starts one)
	put("box", "s1", "h#0", 0, "gen0\n")
	put("box", "s1", "h#1", 0, "gen1\n")
	put("lap", "s1", "h#0", 0, "lap0\n")
	read := func(query string) (int, string) {
		code, b := a.human.do("GET", "/api/v1/usage/transcript?session=s1&file=main"+query, nil)
		return code, string(b)
	}
	if code, b := read("&machine=lap"); code != 200 || b != "lap0\n" {
		t.Errorf("machine=lap: %d %q", code, b)
	}
	if code, b := read("&machine=box&gen=0"); code != 200 || b != "gen0\n" {
		t.Errorf("gen=0: %d %q", code, b)
	}
	if code, b := read("&machine=box&gen=1"); code != 200 || b != "gen1\n" {
		t.Errorf("gen=1: %d %q", code, b)
	}
	if code, b := read("&machine=box"); code != 200 || b != "gen1\n" { // no gen: the newest
		t.Errorf("newest gen: %d %q", code, b)
	}
	if code, b := read("&machine=box&gen=9"); code != 404 {
		t.Errorf("gen=9: %d %q", code, b)
	}
	if code, b := read(""); code != 400 {
		t.Errorf("two machines unsaid: %d %q", code, b)
	}
	for _, g := range []string{"abc", "-1"} {
		if code, _ := read("&gen=" + g); code != 400 {
			t.Errorf("gen=%s: %d, want 400", g, code)
		}
	}
	// a second person's copy is picked by user=
	other, err := a.svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "sam@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := a.svc.CreateAPIToken(context.Background(), &core.Actor{User: other}, "sam", false, 0)
	sam := &client{t: t, base: a.ts.URL, http: http.DefaultClient, token: tok}
	q := url.Values{"machine": {"sam-box"}, "session": {"s1"}, "file": {"main"}, "kind": {"main"}, "fp": {"h#0"}, "from": {"0"}, "to": {"5"}, "redacted": {"1"}, "version": {"t"}}
	req, _ := http.NewRequest("POST", a.ts.URL+"/api/v1/usage/transcript?"+q.Encode(), bytes.NewReader(tgz(t, "sam0\n")))
	req.Header.Set("Authorization", "Bearer "+sam.token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("sam put: %v %v", resp, err)
	}
	var files []core.TranscriptFile
	if code := a.human.json("GET", "/api/v1/usage/transcripts?session=s1&user=sam", nil, &files); code != 200 || len(files) != 1 || files[0].Machine != "sam-box" {
		t.Errorf("user=sam: %d %+v", code, files)
	}
	files = nil
	if code := a.human.json("GET", "/api/v1/usage/transcripts?session=s1&user=robin", nil, &files); code != 200 || len(files) != 2 { // box at its newest generation, and lap
		t.Errorf("user=robin: %d %+v", code, files)
	}
	// ingest numbers
	for _, bad := range []string{"from=abc&to=5", "from=0&to=abc"} {
		resp, err := a.req("POST", "/api/v1/usage/transcript?machine=box&session=s2&file=main&kind=main&fp=h%230&redacted=1&version=t&"+bad, tgz(t, "x\n"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", bad, resp.StatusCode)
		}
	}
}
