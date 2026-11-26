package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func archiveSync(t *testing.T, e *env, dir string, extra ...string) usageSyncResult {
	t.Helper()
	args := append([]string{"usage", "sync", "--all", "--dir", dir, "--json"}, extra...)
	var res usageSyncResult
	out := e.wp(args...)
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	return res
}

func archived(t *testing.T, e *env, me *core.Actor, session, file string) string {
	t.Helper()
	var b bytes.Buffer
	if err := e.svc.ReadTranscript(context.Background(), me, core.TranscriptQuery{Session: session, File: file}, &b); err != nil {
		t.Fatalf("read %s %s: %v", session, file, err)
	}
	return b.String()
}

// theFixtureRedacted is the file as the archive should hold it: the fixture's
// one credential (a pushed --token) blanked, nothing else changed.
func theFixtureRedacted(b []byte) string {
	return strings.ReplaceAll(string(b), "--token=abc12345secret", "--token=***")
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// The server's copy of a transcript is the file's complete lines, byte for
// byte (nothing in the fixture needs redaction), including the sub-agent's
// under its session; a half-written last line waits; growth sends only the
// new bytes; losing wp's memory sends nothing twice.
func TestArchiveUploadsIncrementally(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	main := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	sub := filepath.Join(dir, "projects", "-work-app", "sess1", "subagents", "agent-abc.jsonl")

	res := archiveSync(t, e, dir)
	if res.Archive == nil || res.ArchiveError != "" || res.Archive.Pieces != 2 || res.Archive.Files != 2 {
		t.Fatalf("first: %+v err %q", res.Archive, res.ArchiveError)
	}
	raw, _ := os.ReadFile(main)
	whole := raw[:bytes.LastIndexByte(raw, '\n')+1]
	if got := archived(t, e, me, "sess1", "main"); got != theFixtureRedacted(whole) {
		t.Errorf("main is not the file's whole lines:\n%q\nwant\n%q", got, whole)
	}
	if res.Archive.PendingBytes != int64(len(raw)-len(whole)) {
		t.Errorf("the half line should be what waits: %+v", res.Archive)
	}
	subRaw, _ := os.ReadFile(sub)
	if got := archived(t, e, me, "sess1", "subagents/agent-abc.jsonl"); got != string(subRaw) {
		t.Errorf("sub-agent: %q", got)
	}

	more := `,"x":1}` + "\n" + `{"type":"user","message":{"content":"later"}}` + "\n"
	appendTo(t, main, more)
	res = archiveSync(t, e, dir)
	if res.Archive.Pieces != 1 || res.Archive.RawBytes != int64(len(raw)-len(whole)+len(more)) {
		t.Errorf("growth: %+v", res.Archive)
	}
	raw2, _ := os.ReadFile(main)
	if got := archived(t, e, me, "sess1", "main"); got != theFixtureRedacted(raw2) {
		t.Errorf("after growth: %q", got)
	}
	if res = archiveSync(t, e, dir); res.Archive.Pieces != 0 || res.Archive.PendingBytes != 0 {
		t.Errorf("nothing new: %+v", res.Archive)
	}

	if err := os.Remove(filepath.Join(e.dir, archiveStateFile)); err != nil {
		t.Fatal(err)
	}
	if res = archiveSync(t, e, dir); res.Archive.Pieces != 0 || res.ArchiveError != "" {
		t.Errorf("after losing state: %+v %q", res.Archive, res.ArchiveError)
	}
	if got := archived(t, e, me, "sess1", "main"); got != theFixtureRedacted(raw2) {
		t.Errorf("after losing state, main: %q", got)
	}
	files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "sess1"})
	for _, f := range files {
		if !f.Redacted || f.Gen != 0 {
			t.Errorf("file: %+v", f)
		}
	}
}

// A transcript Claude Code rewrote is sent again from the start as the next
// generation, and both are kept.
func TestArchiveRewrittenFileIsNewGeneration(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	main := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	archiveSync(t, e, dir)
	old := archived(t, e, me, "sess1", "main")

	nw := `{"type":"user","message":{"content":"rewritten"}}` + "\n"
	if err := os.WriteFile(main, []byte(nw), 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Rewritten != 1 || res.ArchiveError != "" {
		t.Fatalf("rewrite: %+v %q", res.Archive, res.ArchiveError)
	}
	if got := archived(t, e, me, "sess1", "main"); got != nw {
		t.Errorf("newest: %q", got)
	}
	g0 := 0
	var b bytes.Buffer
	if err := e.svc.ReadTranscript(context.Background(), me, core.TranscriptQuery{Session: "sess1", File: "main", Gen: &g0}, &b); err != nil || b.String() != old {
		t.Errorf("generation 0 lost: %v %q", err, b.String())
	}
}

// A file truncated and regrown past the old offset with different bytes is
// noticed by the bytes before the offset, not just the size.
func TestArchiveNoticesRewriteInPlace(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	main := filepath.Join(dir, "projects", "-work-app", "sess1.jsonl")
	archiveSync(t, e, dir)
	raw, _ := os.ReadFile(main)
	changed := bytes.ReplaceAll(raw, []byte("sess1"), []byte("SESS9"))
	changed = append(changed, []byte("\n{\"a\":1}\n{\"b\":2}\n")...)
	if len(changed) <= len(raw) {
		t.Fatal("fixture")
	}
	if err := os.WriteFile(main, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.Archive.Rewritten != 1 {
		t.Errorf("in-place rewrite not noticed: %+v", res.Archive)
	}
	if got := archived(t, e, me, "sess1", "main"); !strings.Contains(got, `{"b":2}`) || strings.Contains(got, "sess1") && !strings.Contains(got, "SESS9") {
		t.Errorf("newest generation: %q", got)
	}
}

// A failed upload is counted, shown, kept waiting, and sent on the next
// pass; nothing is lost.
func TestArchiveCountsFailedUploads(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)

	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	res := archiveSync2(t, dir)
	if res.Archive == nil || res.Archive.Pieces != 0 || res.Archive.Dropped["upload_failed"] == 0 || res.Archive.PendingBytes == 0 {
		t.Fatalf("failed pass: %+v err %q", res.Archive, res.ArchiveError)
	}
	st, err := loadArchiveState(e.dir)
	if err != nil || st.Dropped["upload_failed"] == 0 {
		t.Errorf("state does not hold the count: %+v %v", st.Dropped, err)
	}

	t.Setenv("WAYPOINT_SERVER", e.url)
	res = archiveSync(t, e, dir)
	if res.Archive.Pieces != 2 || res.Archive.PendingFiles != 1 {
		t.Errorf("retry: %+v", res.Archive)
	}
	if got := archived(t, e, me, "sess1", "main"); got == "" {
		t.Error("retry stored nothing")
	}
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil || len(ov.Reported) != 1 || ov.Reported[0].Transcripts == nil || ov.Reported[0].Transcripts.Dropped["upload_failed"] == 0 {
		t.Errorf("machines: %+v %v", ov.Reported, err)
	}
}

func archiveSync2(t *testing.T, dir string) usageSyncResult {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{"usage", "sync", "--all", "--dir", dir, "--json"}, &out, &errb)
	var res usageSyncResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("exit %d: %v %s %s", code, err, out.String(), errb.String())
	}
	return res
}

// What the archive keeps has secrets of known shapes blanked, every line
// still valid JSON (also where a secret sits in an escaped string), and this
// machine's own token blanked wherever it appears.
func TestArchiveRedactsAndKeepsLinesValid(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	tok := os.Getenv("WAYPOINT_TOKEN")
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	lines := `{"type":"user","message":{"content":"export TOKEN=` + ghp + ` and \"quoted\" ` + tok + `"}}` + "\n" +
		`{"type":"user","message":{"content":"fine"}}` + "\n"
	p := filepath.Join(dir, "projects", "-work-app", "red.jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	res := archiveSync(t, e, dir)
	if res.ArchiveError != "" {
		t.Fatal(res.ArchiveError)
	}
	got := archived(t, e, me, "red", "main")
	if strings.Contains(got, ghp) || strings.Contains(got, tok) {
		t.Errorf("a secret was archived: %q", got)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Errorf("line is not JSON after redaction: %v: %q", err, l)
		}
		n++
	}
	if n != 2 || !strings.Contains(got, "fine") {
		t.Errorf("lines: %d %q", n, got)
	}
}

// Redaction works on string values in place: a line with nothing to redact
// comes back as it was, numbers like input_tokens are never touched, keys keep
// their order, a secret-named key's value is blanked whole, an escaped quote
// next to a secret does not break the line, and a line that is not JSON is
// redacted as text.
func TestArchiveRedactionKeepsLinesAsWritten(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	known := []string{"hunter22hunter22"}
	plain := `{"z":1,"a":{"input_tokens":12345,"cache_read_input_tokens":1000},"text":"nothing here, token count 5"}` + "\n"
	if out, ok := redactTranscriptLineWith([]byte(plain), known); !ok || string(out) != plain {
		t.Errorf("a line with nothing to redact changed: %v %q", ok, out)
	}
	cases := []struct{ in, want string }{
		{`{"z":1,"m":"x \" ` + ghp + ` \"y\"","n":12345}`, `{"z":1,"m":"x \" *** \"y\"","n":12345}`},
		{`{"api_key": "abcd1234","k":"v"}`, `{"api_key": "***","k":"v"}`},
		{`{"headers":{"Authorization":"Bearer abc.def"},"ok":true}`, `{"headers":{"Authorization":"***"},"ok":true}`},
		{`{"cmd":"curl --token abcdef1234 https://u:pw123@h/x"}`, `{"cmd":"curl --token *** https://u:***@h/x"}`},
		{`{"t":"mine hunter22hunter22 here"}`, `{"t":"mine *** here"}`},
		{`{"cmd":"export API_KEY=abc123xyz\nls"}`, `{"cmd":"export API_KEY=***\nls"}`},
	}
	for _, c := range cases {
		out, ok := redactTranscriptLineWith([]byte(c.in+"\n"), known)
		if !ok || string(out) != c.want+"\n" {
			t.Errorf("redact %s\n got %q (ok %v)\nwant %q", c.in, out, ok, c.want)
		}
		var v any
		if json.Unmarshal(out, &v) != nil {
			t.Errorf("not JSON after redaction: %q", out)
		}
	}
	out, _ := redactTranscriptLineWith([]byte("plain TOKEN="+ghp+" and hunter22hunter22"), known)
	if strings.Contains(string(out), ghp) || strings.Contains(string(out), "hunter22") {
		t.Errorf("non-JSON line kept a secret: %q", out)
	}
}

// --no-archive and WP_USAGE_ARCHIVE=off leave the upload out; the hour
// counts still sync.
func TestArchiveCanBeTurnedOff(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	res := archiveSync(t, e, dir, "--no-archive")
	if res.Archive != nil || res.Messages == 0 {
		t.Errorf("--no-archive: archive %+v messages %d", res.Archive, res.Messages)
	}
	t.Setenv("WP_USAGE_ARCHIVE", "off")
	if res = archiveSync(t, e, dir); res.Archive != nil {
		t.Errorf("env off: %+v", res.Archive)
	}
	files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "sess1"})
	if len(files) != 0 {
		t.Errorf("something was archived: %+v", files)
	}
	t.Setenv("WP_USAGE_ARCHIVE", "")
	if res = archiveSync(t, e, dir); res.Archive == nil || res.Archive.Pieces != 2 {
		t.Errorf("back on: %+v", res.Archive)
	}
}

// The first sync backfills what is on the machine from the last 30 days, which
// is more than the hour counts' default 7-day window; --all takes older ones too.
func TestArchiveBackfillsThirtyDays(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	dir := fixtureDir(t)
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, "projects", "-work-app", name+".jsonl")
		if err := os.WriteFile(p, []byte(`{"type":"user","message":{"content":"`+name+`"}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	write("twenty", 20*24*time.Hour)
	write("sixty", 60*24*time.Hour)
	var out bytes.Buffer
	if code := run([]string{"usage", "sync", "--dir", dir, "--json"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	if got := archived(t, e, me, "twenty", "main"); !strings.Contains(got, `"twenty"`) {
		t.Errorf("20-day-old transcript not backfilled: %q", got)
	}
	if files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "sixty"}); len(files) != 0 {
		t.Errorf("60-day-old transcript archived without --all: %+v", files)
	}
	archiveSync(t, e, dir)
	if got := archived(t, e, me, "sixty", "main"); !strings.Contains(got, `"sixty"`) {
		t.Errorf("--all left the old transcript out: %q", got)
	}
}
