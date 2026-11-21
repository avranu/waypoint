package api

import (
	"bytes"
	"compress/gzip"
	"context"
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

func newTranscriptAPI(t *testing.T) *transcriptAPI {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	dir := t.TempDir()
	svc := core.New(d, core.Config{BaseURL: "http://wp.test", TranscriptDir: dir}, nil)
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
