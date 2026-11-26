package core

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core/users"
)

func tgz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// transcriptEnv is an env whose server keeps an archive.
func transcriptEnv(t *testing.T) *env {
	e := newEnv(t)
	e.setTranscriptDir(t.TempDir())
	return e
}

// setTranscriptDir keeps the transcript archive in dir, for the users domain
// too (it words an account's deletion by whether one is kept).
func (e *env) setTranscriptDir(dir string) {
	e.s.cfg.TranscriptDir = dir
	e.s.users.SetOptions(users.Options{BaseURL: e.s.cfg.BaseURL, ArchivesTranscripts: dir != ""})
}

func piece(session, file string, from int64, lines string) TranscriptPieceIn {
	return TranscriptPieceIn{Machine: "box", Session: session, File: file, Kind: "main", FP: "fp0#0",
		From: from, To: from + int64(len(lines)), Redacted: true, Version: "t"}
}

func (e *env) put(a *Actor, in TranscriptPieceIn, lines string) *TranscriptPieceResult {
	e.t.Helper()
	in.Gzip = tgz(e.t, lines)
	r, err := e.s.IngestTranscript(e.ctx, a, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) readAll(a *Actor, q TranscriptQuery) string {
	e.t.Helper()
	var b bytes.Buffer
	if err := e.s.ReadTranscript(e.ctx, a, q, &b); err != nil {
		e.t.Fatal(err)
	}
	return b.String()
}

// A session's file comes back whole and in order, whatever the pieces'
// sizes, and the sub-agent's file stays under the session it belongs to.
func TestTranscriptReadsBackInOrder(t *testing.T) {
	e := transcriptEnv(t)
	l1, l2, l3 := `{"n":1}`+"\n", `{"n":2,"t":"é"}`+"\n", `{"n":3}`+"\n"
	for i, p := range []struct {
		from  int64
		lines string
	}{{0, l1}, {int64(len(l1)), l2 + l3}} {
		if r := e.put(e.admin, piece("s1", "main", p.from, p.lines), p.lines); r.Status != "stored" || r.Have != p.from+int64(len(p.lines)) {
			t.Fatalf("piece %d: %+v", i, r)
		}
	}
	sub := `{"agent":1}` + "\n"
	in := piece("s1", "subagents/agent-a.jsonl", 0, sub)
	in.Kind = "Explore"
	e.put(e.admin, in, sub)

	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main"}); got != l1+l2+l3 {
		t.Errorf("main: %q", got)
	}
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "subagents/agent-a.jsonl"}); got != sub {
		t.Errorf("sub-agent: %q", got)
	}
	files, err := e.s.TranscriptFiles(e.ctx, e.admin, TranscriptQuery{Session: "s1"})
	if err != nil || len(files) != 2 || files[0].File != "main" || files[0].Pieces != 2 || !files[0].Complete || files[1].Kind != "Explore" || !files[0].Redacted {
		t.Fatalf("files: %+v %v", files, err)
	}
	var order []string
	var all strings.Builder
	err = e.s.WalkTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1"}, func(f TranscriptFile, r io.Reader) error {
		order = append(order, f.File)
		_, err := io.Copy(&all, r)
		return err
	})
	if err != nil || strings.Join(order, ",") != "main,subagents/agent-a.jsonl" || all.String() != l1+l2+l3+sub {
		t.Errorf("walk: %v %v %q", order, err, all.String())
	}
}

// A retry whose answer was lost stores nothing twice; a piece after a hole
// stores nothing and says where to resume; the file is never left holey.
func TestTranscriptRetriesAndGaps(t *testing.T) {
	e := transcriptEnv(t)
	l1, l2 := `{"n":1}`+"\n", `{"n":2}`+"\n"
	first := piece("s1", "main", 0, l1)
	if r := e.put(e.admin, first, l1); r.Status != "stored" {
		t.Fatal(r)
	}
	if r := e.put(e.admin, first, l1); r.Status != "duplicate" || r.Have != int64(len(l1)) {
		t.Errorf("same piece again: %+v", r)
	}
	both := piece("s1", "main", 0, l1+l2)
	if r := e.put(e.admin, both, l1+l2); r.Status != "behind" || r.Have != int64(len(l1)) {
		t.Errorf("a piece overlapping what is held: %+v", r)
	}
	hole := piece("s1", "main", int64(len(l1)+len(l2)), l1)
	if r := e.put(e.admin, hole, l1); r.Status != "gap" || r.Have != int64(len(l1)) {
		t.Errorf("a piece after a hole: %+v", r)
	}
	if r := e.put(e.admin, piece("s2", "main", 99, l1), l1); r.Status != "gap" || r.Have != 0 {
		t.Errorf("first piece not from 0: %+v", r)
	}
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main"}); got != l1 {
		t.Errorf("after the refused pieces: %q", got)
	}
	if r := e.put(e.admin, piece("s1", "main", int64(len(l1)), l2), l2); r.Status != "stored" {
		t.Fatal(r)
	}
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main"}); got != l1+l2 {
		t.Errorf("joined: %q", got)
	}
}

// A file rewritten from the start is the next generation; both are kept and
// the newest is what reads back unless one is asked for.
func TestTranscriptRewriteStartsNewGeneration(t *testing.T) {
	e := transcriptEnv(t)
	old, rewritten := `{"v":"old"}`+"\n", `{"v":"new"}`+"\n"+`{"v":"newer"}`+"\n"
	e.put(e.admin, piece("s1", "main", 0, old), old)
	in := piece("s1", "main", 0, rewritten)
	in.FP = "fp1#1"
	if r := e.put(e.admin, in, rewritten); r.Status != "stored" {
		t.Fatal(r)
	}
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main"}); got != rewritten {
		t.Errorf("newest: %q", got)
	}
	g0 := 0
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main", Gen: &g0}); got != old {
		t.Errorf("generation 0: %q", got)
	}
	if r := e.put(e.admin, in, rewritten); r.Status != "duplicate" {
		t.Errorf("retry of a rewrite: %+v", r)
	}
}

// Raw transcripts are a person's own; an admin reads any; an agent's token
// reads none (they hold command text it may not see).
func TestTranscriptAccess(t *testing.T) {
	e := transcriptEnv(t)
	robin := e.admin
	jo := e.user("jo", false)
	l := `{"secret":"?"}` + "\n"
	e.put(jo, piece("s1", "main", 0, l), l)
	if got := e.readAll(jo, TranscriptQuery{Session: "s1", File: "main"}); got != l {
		t.Errorf("own: %q", got)
	}
	if got := e.readAll(robin, TranscriptQuery{Session: "s1", File: "main", User: "jo"}); got != l {
		t.Errorf("admin reading another's: %q", got)
	}
	other := e.user("kim", false)
	if _, err := e.s.TranscriptFiles(e.ctx, other, TranscriptQuery{Session: "s1", User: "jo"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("another person's: %v", err)
	}
	if files, err := e.s.TranscriptFiles(e.ctx, other, TranscriptQuery{Session: "s1"}); err != nil || len(files) != 0 {
		t.Errorf("your own session id that is someone else's holds nothing of yours: %v %v", files, err)
	}
	agent := agentActor(jo, "anthropic/claude-code")
	if _, err := e.s.TranscriptFiles(e.ctx, agent, TranscriptQuery{Session: "s1"}); err == nil {
		t.Error("an agent's token read a raw transcript")
	}
}

func TestTranscriptRefusesBadPieces(t *testing.T) {
	e := transcriptEnv(t)
	ok := piece("s1", "main", 0, "{}\n")
	for name, mut := range map[string]func(*TranscriptPieceIn){
		"a half line":       func(in *TranscriptPieceIn) { in.Gzip = tgz(t, "{}\n{\"a\"") },
		"not gzip":          func(in *TranscriptPieceIn) { in.Gzip = []byte("plain") },
		"path escape":       func(in *TranscriptPieceIn) { in.File = "../x.jsonl" },
		"absolute file":     func(in *TranscriptPieceIn) { in.File = "/etc/passwd" },
		"session with path": func(in *TranscriptPieceIn) { in.Session = "../s" },
		"empty range":       func(in *TranscriptPieceIn) { in.To = in.From },
	} {
		in := ok
		in.Gzip = tgz(t, "{}\n")
		mut(&in)
		if _, err := e.s.IngestTranscript(e.ctx, e.admin, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e2 := newEnv(t)
	in := ok
	in.Gzip = tgz(t, "{}\n")
	if _, err := e2.s.IngestTranscript(e2.ctx, e2.admin, in); !errors.Is(err, ErrTranscriptsOff) {
		t.Errorf("archive off: %v", err)
	}
}

// A stored piece changed on disk is an error on read, not a quiet hole.
func TestTranscriptDetectsDamage(t *testing.T) {
	e := transcriptEnv(t)
	l := `{"n":1}` + "\n"
	e.put(e.admin, piece("s1", "main", 0, l), l)
	var rel string
	if err := e.s.db.R.QueryRow(`SELECT path FROM transcript_pieces`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.s.cfg.TranscriptDir, rel), tgz(t, `{"n":2}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := e.s.ReadTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1", File: "main"}, &b); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("damaged piece: %v", err)
	}
}

// What a machine reports about its archive shows with the machine, and
// pending clears when nothing waits.
func TestTranscriptStatusAndHealth(t *testing.T) {
	e := transcriptEnv(t)
	st := TranscriptStatus{Machine: "box", PendingBytes: 500, PendingFiles: 2, Dropped: map[string]int64{"upload_failed": 3}}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, st); err != nil {
		t.Fatal(err)
	}
	ms, err := e.s.usageMachines(e.ctx, &usageScope{uids: []int64{e.admin.User.ID}})
	if err != nil || len(ms) != 1 || ms[0].Transcripts == nil || ms[0].Transcripts.PendingBytes != 500 || ms[0].Transcripts.Dropped["upload_failed"] != 3 {
		t.Fatalf("machines: %+v %v", ms, err)
	}
	var since *int64
	if err := e.s.db.R.QueryRow(`SELECT transcript_pending_since FROM usage_machines`).Scan(&since); err != nil || since == nil {
		t.Fatalf("pending_since: %v %v", since, err)
	}
	st.PendingBytes, st.PendingFiles = 0, 0
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, st); err != nil {
		t.Fatal(err)
	}
	if err := e.s.db.R.QueryRow(`SELECT transcript_pending_since FROM usage_machines`).Scan(&since); err != nil || since != nil {
		t.Errorf("pending_since after it cleared: %v %v", since, err)
	}
}
