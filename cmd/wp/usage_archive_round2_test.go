package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Redaction counts the values it blanked, by rule, and never the values
// themselves (audit 1218.33).
func TestRedactionCountsByRule(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	known := []string{"hunter22hunter22"}
	cases := []struct {
		name, in string
		want     redactCount
	}{
		{"nothing", `{"a":"plain","n":1}`, redactCount{}},
		{"key", `{"api_key":"abcd1234","k":"v"}`, redactCount{Total: 1, Key: 1}},
		{"key array", `{"token":["a","b"]}`, redactCount{Total: 2, Key: 2}},
		{"known", `{"t":"mine hunter22hunter22 here"}`, redactCount{Total: 1, Known: 1}},
		{"pattern", `{"m":"x ` + ghp + ` y"}`, redactCount{Total: 1, Pattern: 1}},
		{"both rules one value", `{"m":"hunter22hunter22 and ` + ghp + `"}`, redactCount{Total: 1, Known: 1, Pattern: 1}},
		{"two values", `{"a":"` + ghp + `","b":"` + ghp + `"}`, redactCount{Total: 2, Pattern: 2}},
		{"plain text line", "TOKEN=" + ghp + " hunter22hunter22", redactCount{Total: 1, Known: 1, Pattern: 1}},
	}
	for _, c := range cases {
		_, ok, got := redactTranscriptLineCounted([]byte(c.in+"\n"), known)
		if !ok || got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	r := redactCount{Total: 3, Key: 2, Pattern: 1}
	if m := r.rules(); len(m) != 2 || m["secret_key"] != 2 || m["pattern"] != 1 {
		t.Errorf("rules %v", m)
	}
}

// ---- the client's side of the second round (audit 1218.10, .33, .34, .35, .50) ----

func machineRecord(t *testing.T, e *env, me *core.Actor) *core.UsageTranscripts {
	t.Helper()
	ov, err := e.svc.UsageOverview(context.Background(), me, core.UsageQuery{Days: 3650})
	if err != nil || len(ov.Reported) != 1 || ov.Reported[0].Transcripts == nil {
		t.Fatalf("machines: %+v %v", ov.Reported, err)
	}
	return ov.Reported[0].Transcripts
}

// Each piece carries what redaction blanked, so the server can say how much
// was redacted per file; nothing about what the values were.
func TestArchiveSendsRedactionCounts(t *testing.T) {
	e, _ := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	putTranscript(t, dir, "red", userLine("export TOKEN="+ghp), `{"api_key":"abcd1234","x":"y"}`, userLine("fine"))
	putTranscript(t, dir, "clean", userLine("fine"))
	archiveSync(t, e, dir)
	files, err := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "red"})
	if err != nil || len(files) != 1 || files[0].Redactions == nil || *files[0].Redactions != 2 {
		t.Fatalf("red: %+v %v", files, err)
	}
	if r := files[0].RedactionRules; r["pattern"] != 1 || r["secret_key"] != 1 || len(r) != 2 {
		t.Errorf("rules %v", r)
	}
	files, _ = e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "clean"})
	if len(files) != 1 || files[0].Redactions == nil || *files[0].Redactions != 0 {
		t.Errorf("a clean file says 0, not unknown: %+v", files)
	}
}

// What a stub replaced carries no count of its own.
func TestPieceRedactionsAfterACut(t *testing.T) {
	var p archivePiece
	p.add([]byte("a\n"), 2, "", 2, redactCount{Total: 1, Key: 1})
	p.add([]byte("b\n"), 4, "", 2, redactCount{Total: 2, Pattern: 2})
	if r := p.redactions(); r.Total != 3 {
		t.Errorf("%+v", r)
	}
	p.keep(1)
	if r := p.redactions(); r.Total != 1 || r.Key != 1 {
		t.Errorf("after keep %+v", r)
	}
	p.stubOnly([]byte("stub\n"), "line_too_large", 2)
	if r := p.redactions(); r.Total != 0 {
		t.Errorf("after stub %+v", r)
	}
}

// fakePasses makes the pass durations and the clock the test's.
func fakePasses(t *testing.T, ms *int64, now *time.Time) {
	t.Helper()
	od, on := archivePassDuration, archiveNow
	archivePassDuration = func(time.Time) time.Duration { return time.Duration(*ms) * time.Millisecond }
	archiveNow = func() time.Time { return *now }
	t.Cleanup(func() { archivePassDuration, archiveNow = od, on })
}

// The status says how long the pass took, the longest, why it stopped and how
// many passes were cut short; it is posted again only when something but the
// duration changed, when the longest grew, or when the last report is old
// enough that the server would take silence for a stopped machine.
func TestArchiveStatusPostsOnChangeAndRefresh(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "a", userLine("a1"))
	ms, now := int64(100), time.Now()
	fakePasses(t, &ms, &now)

	hookSync(t, dir)
	rec := machineRecord(t, e, me)
	if rec.PassMS == nil || *rec.PassMS != 100 || rec.PassMaxMS == nil || *rec.PassMaxMS != 100 ||
		rec.Stop == nil || *rec.Stop != "" || rec.Cut == nil || *rec.Cut != 0 || rec.Off == nil || *rec.Off {
		t.Fatalf("record after the first pass: %+v", rec)
	}
	if _, n := tap.counts(); n != 1 {
		t.Fatalf("statuses %d", n)
	}
	ms = 40 // faster: the same state, nothing to say
	hookSync(t, dir)
	if _, n := tap.counts(); n != 1 {
		t.Errorf("a pass that only took a different time posted: %d", n)
	}
	ms = 300 // the longest grew
	hookSync(t, dir)
	if _, n := tap.counts(); n != 2 {
		t.Errorf("a longer pass did not post: %d", n)
	}
	if rec = machineRecord(t, e, me); *rec.PassMaxMS != 300 || *rec.PassMS != 300 {
		t.Errorf("record %+v", rec)
	}
	now = now.Add(archiveRefresh - time.Minute)
	hookSync(t, dir)
	if _, n := tap.counts(); n != 2 {
		t.Errorf("posted before the refresh was due: %d", n)
	}
	now = now.Add(2 * time.Minute)
	ms = 50
	hookSync(t, dir)
	if _, n := tap.counts(); n != 3 {
		t.Errorf("no refresh when due: %d", n)
	}
	if !strings.Contains(tap.bodies[2], `"pass_ms":50`) {
		t.Errorf("refresh body %s", tap.bodies[2])
	}
}

// A pass that stops early says why, counts as cut short once, and the
// count is kept across passes.
func TestArchiveStatusCountsCutShortPasses(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "a", manyLines("a", 5)...)
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 503, "later")
		return true
	}
	old := archivePassLimit
	archivePassLimit = -time.Second // already over: stops before the first piece
	t.Cleanup(func() { archivePassLimit = old })
	archiveSync(t, e, dir)
	rec := machineRecord(t, e, me)
	if rec.Cut == nil || *rec.Cut != 1 || rec.Stop == nil || !strings.Contains(*rec.Stop, "time limit") {
		t.Fatalf("record %+v", rec)
	}
	archiveSync(t, e, dir) // the same again: counted again
	if rec = machineRecord(t, e, me); *rec.Cut != 2 {
		t.Errorf("cut %d", *rec.Cut)
	}
	archivePassLimit = old
	tap.onPiece = nil
	archiveSync(t, e, dir)
	rec = machineRecord(t, e, me)
	if *rec.Stop != "" || *rec.Cut != 2 {
		t.Errorf("a pass that ran to the end clears the stop and keeps the count: %+v", rec)
	}
}

// WP_USAGE_ARCHIVE=off is the machine's opt-out and it says so, once, then
// when it changes or the refresh is due; --no-archive says nothing.
func TestArchiveOptOutIsReported(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "a", userLine("a1"))
	ms, now := int64(10), time.Now()
	fakePasses(t, &ms, &now)

	t.Setenv("WP_USAGE_ARCHIVE", "off")
	archiveSync(t, e, dir)
	if rec := machineRecord(t, e, me); rec.Off == nil || !*rec.Off {
		t.Fatalf("off not recorded: %+v", rec)
	}
	archiveSync(t, e, dir)
	if _, n := tap.counts(); n != 1 {
		t.Errorf("off posted again with nothing changed: %d", n)
	}
	now = now.Add(archiveRefresh + time.Minute)
	archiveSync(t, e, dir)
	if _, n := tap.counts(); n != 2 {
		t.Errorf("off not refreshed: %d", n)
	}
	t.Setenv("WP_USAGE_ARCHIVE", "")
	archiveSync(t, e, dir)
	rec := machineRecord(t, e, me)
	if rec.Off == nil || *rec.Off {
		t.Errorf("back on: %+v", rec)
	}
}

// The server's size cap answering 507 means held, not refused: the pass
// stops quietly, nothing is dropped or held, the file stays pending, and the
// next pass tries again.
func TestArchiveQuotaIsHeldNotDropped(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	p := putTranscript(t, dir, "a", userLine("a1"))
	putTranscript(t, dir, "b", userLine("b1"))
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		answerError(w, 507, core.TranscriptQuotaRefusal)
		return true
	}
	res := archiveSync(t, e, dir)
	pieces, _ := tap.counts()
	if pieces != 1 {
		t.Errorf("tried %d pieces after the cap answered", pieces)
	}
	if len(res.Archive.Dropped) != 0 || res.Archive.Stopped == "" || res.Archive.PendingBytes == 0 || res.ArchiveError != "" {
		t.Errorf("pass %+v err %q", res.Archive, res.ArchiveError)
	}
	st, _ := loadArchiveState(e.dir)
	if len(st.Dropped) != 0 || (st.Files[p] != nil && st.Files[p].Held != "") {
		t.Errorf("state %+v %+v", st.Dropped, st.Files[p])
	}
	if rec := machineRecord(t, e, me); rec.Cut == nil || *rec.Cut != 1 || rec.Stop == nil || *rec.Stop == "" || rec.Dropped["refused"] != 0 {
		t.Errorf("server's record %+v", rec)
	}
	tap.onPiece = nil
	if res = archiveSync(t, e, dir); res.Archive.PendingBytes != 0 || res.Archive.Pieces != 2 {
		t.Errorf("once there is room: %+v", res.Archive)
	}
}

func TestUsageMachinesText(t *testing.T) {
	five, nine, one, zero := int64(5), int64(9000), int64(1), int64(0)
	stop, none, yes := "time limit; the rest goes next time", "", true
	at := time.Now().Add(-time.Hour)
	out := usageMachinesText([]core.UsageMachine{
		{Name: "laptop", Transcripts: &core.UsageTranscripts{PendingBytes: 2048, PendingFiles: 1, Dropped: map[string]int64{"line_too_large": 2},
			PassMS: &five, PassMaxMS: &nine, Stop: &stop, Cut: &one, Off: new(bool), LastPieceAt: &at}},
		{Name: "desk", Transcripts: &core.UsageTranscripts{Off: &yes}},
		{Name: "old", Transcripts: &core.UsageTranscripts{Stop: &none, Cut: &zero}},
		{Name: "never"},
	})
	for _, want := range []string{"MACHINE", "laptop", "2 KB", "lines too large", "5ms", "9s", "time limit", "desk", "off", "never", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "off"); n != 1 {
		t.Errorf("only the machine that opted out says off (%d):\n%s", n, out)
	}
}

func TestTranscriptFilesShowRedactions(t *testing.T) {
	two := int64(2)
	out := transcriptFilesText([]core.TranscriptFile{
		{Machine: "a", File: "main", Redactions: &two},
		{Machine: "b", File: "main"},
	})
	if !strings.Contains(out, "REDACTIONS") || !strings.Contains(out, "unknown") {
		t.Errorf("%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "a ") && !strings.Contains(l, " 2 ") {
			t.Errorf("count missing: %q", l)
		}
	}
}

// A --no-archive pass tells the server the archive is off exactly as
// WP_USAGE_ARCHIVE=off does, so a machine whose hook always passes it is not
// taken for a silent one; it is rate limited the same way, and a dry run says
// nothing.
func TestNoArchivePassReportsOff(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	putTranscript(t, dir, "a", userLine("a1"))
	ms, now := int64(10), time.Now()
	fakePasses(t, &ms, &now)

	archiveSync(t, e, dir, "--no-archive", "--dry-run")
	if _, n := tap.counts(); n != 0 {
		t.Fatalf("a dry run posted %d", n)
	}
	archiveSync(t, e, dir, "--no-archive")
	if _, n := tap.counts(); n != 1 {
		t.Fatalf("--no-archive posted %d, want 1", n)
	}
	if rec := machineRecord(t, e, me); rec.Off == nil || !*rec.Off {
		t.Fatalf("off not recorded: %+v", rec)
	}
	if !strings.Contains(tap.bodies[0], `"off":true`) {
		t.Errorf("body %s", tap.bodies[0])
	}
	archiveSync(t, e, dir, "--no-archive")
	if _, n := tap.counts(); n != 1 {
		t.Errorf("posted again with nothing changed: %d", n)
	}
	archiveSync(t, e, dir)
	if rec := machineRecord(t, e, me); rec.Off == nil || *rec.Off {
		t.Errorf("a normal pass did not say it is back on: %+v", rec)
	}
}

// After the server said its archive is full, a hook's passes make no upload
// for an hour (the files stay pending, the stop says why, and the pass is not
// another cut); a manual sync always tries.
func TestArchiveFullHoldsHookPasses(t *testing.T) {
	e, tap := tappedEnv(t)
	me := e.login()
	dir := emptyDir(t)
	p := putTranscript(t, dir, "a", userLine("a1"))
	ms, now := int64(10), time.Now()
	fakePasses(t, &ms, &now)
	full := true
	tap.onPiece = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		if full {
			answerError(w, 507, core.TranscriptQuotaRefusal)
			return true
		}
		return false
	}
	hook := func() {
		t.Helper()
		c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
		usageHook(c, strings.NewReader(`{"session_id":"a","transcript_path":"`+p+`"}`), dir)
	}
	hook()
	if pieces, _ := tap.counts(); pieces != 1 {
		t.Fatalf("first pass tried %d", pieces)
	}
	now = now.Add(10 * time.Minute)
	hook()
	if pieces, _ := tap.counts(); pieces != 1 {
		t.Errorf("a hook pass within the hour tried an upload: %d", pieces)
	}
	if rec := machineRecord(t, e, me); rec.Cut == nil || *rec.Cut != 1 || rec.Stop == nil || !strings.Contains(*rec.Stop, "full") || rec.PendingBytes == 0 {
		t.Errorf("a skipped pass counted as a cut, or lost its stop or pending bytes: %+v", rec)
	}
	if res := archiveSync(t, e, dir); res.Archive.Stopped == "" {
		t.Errorf("a manual sync did not try: %+v", res.Archive)
	}
	if pieces, _ := tap.counts(); pieces != 2 {
		t.Errorf("manual sync tried %d in all", pieces)
	}
	now = now.Add(archiveFullHold)
	hook()
	if pieces, _ := tap.counts(); pieces != 3 {
		t.Errorf("after the hour a hook pass did not try: %d", pieces)
	}
	full = false
	hook()
	if res := archiveSync(t, e, dir); res.Archive.PendingBytes != 0 {
		t.Errorf("once there is room: %+v", res.Archive)
	}
}
