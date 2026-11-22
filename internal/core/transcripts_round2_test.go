package core

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Audit 1218, second round: collector health per machine and drops (.10,
// .20), redaction counts (.33), the pass record and opt-out (.34, .35), the
// archive's size and quota (.36, .50).

func i64(n int64) *int64    { return &n }
func strp(s string) *string { return &s }
func yes(b bool) *bool      { return &b }

// machineRow sets what a machine's row says, in ms before the clock.
func (e *env) machineRow(machine string, set string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO usage_machines(user_id, machine) VALUES (?, ?) ON CONFLICT DO NOTHING`, e.admin.User.ID, machine); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE usage_machines SET `+set+` WHERE user_id = ? AND machine = ?`,
		append(args, e.admin.User.ID, machine)...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ago(d time.Duration) int64 { return e.clockNow().Add(-d).UnixMilli() }

func (e *env) archiveHealth() CollectorHealth {
	e.t.Helper()
	e.s.CheckCollectorHealth(e.ctx)
	got, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, h := range got {
		if h.Name == "transcript archive" {
			return h
		}
	}
	e.t.Fatal("no transcript archive collector")
	return CollectorHealth{}
}

func (e *env) collectorNotices() []string {
	e.t.Helper()
	rows, err := e.s.db.R.Query(`SELECT title || ' | ' || body FROM notifications WHERE kind = ? ORDER BY id`, NotifyCollector)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// 1218.20: one machine uploading does not hide another that is stuck, and the
// notice names the stuck one (user and machine).
func TestTranscriptCollectorStallIsPerMachine(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	e.machineRow("stuck", `transcript_pending_bytes = 10, transcript_pending_files = 1, transcript_pending_since = ?, transcript_status_at = ?`, e.ago(72*time.Hour), e.ago(time.Hour))
	e.machineRow("busy", `transcript_pending_bytes = 10, transcript_pending_files = 1, transcript_pending_since = ?, transcript_status_at = ?, transcript_piece_at = ?`,
		e.ago(72*time.Hour), e.ago(time.Hour), e.ago(time.Minute))
	h := e.archiveHealth()
	if !h.Stalled {
		t.Fatalf("one machine is stuck and the collector is not stalled: %+v", h)
	}
	n := e.collectorNotices()
	if len(n) != 1 || !strings.Contains(n[0], e.admin.User.Username+"/stuck") || strings.Contains(n[0], "busy") {
		t.Fatalf("notices = %v", n)
	}
	// the busy one alone is not stalled
	e.machineRow("stuck", `transcript_pending_bytes = 0, transcript_pending_since = NULL`)
	if h := e.archiveHealth(); h.Stalled {
		t.Errorf("only an uploading machine waits, yet %+v", h)
	}
}

// 1218.20, second half: a machine that reported before and whose usage sync
// still arrives but whose archive status stopped is waiting, unless it has
// switched the archive off.
func TestTranscriptCollectorMachineThatStoppedReporting(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	e.machineRow("quiet", `transcript_status_at = ?, last_sync_at = ?`, e.ago(60*time.Hour), e.ago(time.Hour))
	if h := e.archiveHealth(); !h.Stalled || h.Waiting != 1 {
		t.Fatalf("a machine whose status stopped 60h ago while it still syncs: %+v", h)
	}
	e.machineRow("quiet", `transcript_off = 1`)
	if h := e.archiveHealth(); h.Stalled || h.Waiting != 0 || h.Off == nil || *h.Off != 1 {
		t.Errorf("opted out machines are not waiting, and are counted: %+v", h)
	}
	e.machineRow("quiet", `transcript_off = 0, transcript_status_at = ?`, e.ago(10*time.Hour))
	if h := e.archiveHealth(); h.Stalled || h.Waiting != 0 {
		t.Errorf("a recent status is not a stall: %+v", h)
	}
	e.machineRow("quiet", `transcript_status_at = ?, last_sync_at = ?`, e.ago(60*time.Hour), e.ago(59*time.Hour))
	if h := e.archiveHealth(); h.Waiting != 0 {
		t.Errorf("a machine that stopped syncing at all is the usage page's business, not waiting here: %+v", h)
	}
}

// 1218.10: the collector's dropped total is what did not reach the archive
// whole (stubs, a file refused for good), not a retry; growth is told once a day.
func TestTranscriptCollectorDropsAreCountedAndTold(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	st := TranscriptStatus{Machine: "box", Dropped: map[string]int64{"line_too_large": 2, "redaction_failed": 1, "upload_failed": 5, "refused": 1}}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, st); err != nil {
		t.Fatal(err)
	}
	if h := e.archiveHealth(); h.Dropped == nil || *h.Dropped != 4 {
		t.Fatalf("dropped = %v, want 4 (stubs and the refused file; not upload_failed)", h.Dropped)
	}
	n := e.collectorNotices()
	if len(n) != 1 || !strings.Contains(n[0], "4") || !strings.Contains(n[0], "dropped") {
		t.Fatalf("first telling: %v", n)
	}
	e.s.CheckCollectorHealth(e.ctx)
	if got := e.collectorNotices(); len(got) != 1 {
		t.Errorf("told again with no growth: %v", got)
	}
	st.Dropped["line_too_large"] = 4
	_ = e.s.ReportTranscriptStatus(e.ctx, e.admin, st)
	e.setClock(e.clockNow().Add(time.Hour))
	e.s.CheckCollectorHealth(e.ctx)
	if got := e.collectorNotices(); len(got) != 1 {
		t.Errorf("told again within the day: %v", got)
	}
	e.setClock(e.clockNow().Add(24 * time.Hour))
	e.s.CheckCollectorHealth(e.ctx)
	got := e.collectorNotices()
	if len(got) != 2 || !strings.Contains(got[1], "6") {
		t.Errorf("growth after the day is told: %v", got)
	}
	// a collector that counts no drops has none
	for _, h := range mustHealth(t, e) {
		if h.Name != "transcript archive" && h.Name != "session data" && h.Dropped != nil {
			t.Errorf("%s has a dropped total %d", h.Name, *h.Dropped)
		}
	}
}

func mustHealth(t *testing.T, e *env) []CollectorHealth {
	t.Helper()
	got, err := e.s.CollectorHealthList(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 1218.33: each piece keeps what its redaction blanked, by rule; a file says
// unknown where any piece has no count.
func TestTranscriptRedactionCounts(t *testing.T) {
	e := transcriptEnv(t)
	l := "{}\n"
	p0 := piece("s1", "main", 0, l)
	p0.Redactions, p0.RedactionRules = i64(3), map[string]int64{"key": 2, "pattern": 1}
	e.put(e.admin, p0, l)
	p1 := piece("s1", "main", int64(len(l)), l)
	p1.Redactions, p1.RedactionRules = i64(2), map[string]int64{"known": 2}
	e.put(e.admin, p1, l)
	fs, err := e.s.TranscriptFiles(e.ctx, e.admin, TranscriptQuery{Session: "s1"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("%v %v", fs, err)
	}
	if fs[0].Redactions == nil || *fs[0].Redactions != 5 || fs[0].RedactionRules["key"] != 2 || fs[0].RedactionRules["known"] != 2 || fs[0].RedactionRules["pattern"] != 1 {
		t.Errorf("file: %+v", fs[0])
	}
	// a piece with no count makes the file's total unknown, not 5
	e.put(e.admin, piece("s1", "main", 2*int64(len(l)), l), l)
	fs, _ = e.s.TranscriptFiles(e.ctx, e.admin, TranscriptQuery{Session: "s1"})
	if fs[0].Redactions != nil {
		t.Errorf("a piece with no count, yet the file says %d", *fs[0].Redactions)
	}
	var n *int64
	if err := e.s.db.R.QueryRow(`SELECT redactions FROM transcript_pieces WHERE from_off = ?`, 2*int64(len(l))).Scan(&n); err != nil || n != nil {
		t.Errorf("stored %v %v, want NULL", n, err)
	}
	// only rule names are kept: a name that is not one is refused
	bad := piece("s2", "main", 0, l)
	bad.Redactions, bad.RedactionRules = i64(1), map[string]int64{"password=hunter2": 1}
	bad.Gzip = tgz(t, l)
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, bad); err == nil || !IsInvalid(err) {
		t.Errorf("a rule name that is not a name: %v", err)
	}
	neg := piece("s3", "main", 0, l)
	neg.Redactions, neg.Gzip = i64(-1), tgz(t, l)
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, neg); err == nil || !IsInvalid(err) {
		t.Errorf("a negative count: %v", err)
	}
}

// 1218.34 and .35: the pass record and the opt-out are kept per machine and
// unknown until reported.
func TestTranscriptStatusKeepsPassRecordAndOptOut(t *testing.T) {
	e := transcriptEnv(t)
	machines := func() UsageTranscripts {
		t.Helper()
		ms, err := e.s.usageMachines(e.ctx, &usageScope{uids: []int64{e.admin.User.ID}})
		if err != nil || len(ms) != 1 || ms[0].Transcripts == nil {
			t.Fatalf("machines %+v %v", ms, err)
		}
		return *ms[0].Transcripts
	}
	// an old client says none of it: unknown
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PendingBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if m := machines(); m.Off != nil || m.PassMS != nil || m.PassMaxMS != nil || m.Stop != nil || m.Cut != nil {
		t.Errorf("unreported fields must be unknown: %+v", m)
	}
	st := TranscriptStatus{Machine: "box", PassMS: i64(900), PassMaxMS: i64(900), Stop: strp("time limit"), Cut: i64(2), Off: yes(false)}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, st); err != nil {
		t.Fatal(err)
	}
	m := machines()
	if m.PassMS == nil || m.PassMaxMS == nil || m.Stop == nil || m.Cut == nil || m.Off == nil {
		t.Fatalf("after the first: %+v", m)
	}
	if *m.PassMS != 900 || *m.PassMaxMS != 900 || *m.Stop != "time limit" || *m.Cut != 2 || *m.Off {
		t.Errorf("after the first: %+v", m)
	}
	// a shorter pass: last changes, longest stays; "" is a pass that ran to the end
	st.PassMS, st.PassMaxMS, st.Stop = i64(100), nil, strp("")
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, st); err != nil {
		t.Fatal(err)
	}
	if m := machines(); m.PassMS == nil || m.PassMaxMS == nil || m.Stop == nil || *m.PassMS != 100 || *m.PassMaxMS != 900 || *m.Stop != "" {
		t.Errorf("after a short pass: %+v", m)
	}
	// off is kept, and it does not erase what is pending
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PendingBytes: 7, Off: yes(false)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", Off: yes(true)}); err != nil {
		t.Fatal(err)
	}
	if m := machines(); m.Off == nil || !*m.Off || m.PendingBytes != 7 {
		t.Errorf("after off: %+v", m)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PassMS: i64(-1)}); err == nil || !IsInvalid(err) {
		t.Errorf("a negative duration: %v", err)
	}
}

func randomLines(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		if b[i] == '\n' {
			b[i] = 'x'
		}
	}
	return string(b) + "\n"
}

// 1218.36: the archive's size and piece count are a resource with its own
// limit, and the existing low-resource path alarms on it.
func TestTranscriptArchiveIsAResource(t *testing.T) {
	e := transcriptEnv(t)
	find := func() ResourceStatus {
		t.Helper()
		for _, r := range e.s.readResources(e.ctx) {
			if r.Resource == "transcripts" {
				return r
			}
		}
		t.Fatal("no transcripts resource")
		return ResourceStatus{}
	}
	if r := find(); r.Bytes != 0 || r.Count != 0 || r.Low {
		t.Errorf("empty: %+v", r)
	}
	l := randomLines(2000)
	res := e.put(e.admin, piece("s1", "main", 0, l), l)
	if r := find(); r.Bytes != res.Stored || r.Count != 1 {
		t.Errorf("one piece of %d stored bytes: %+v", res.Stored, r)
	}
	if _, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{TranscriptsMB: 1}); err != nil {
		t.Fatal(err)
	}
	if r := find(); r.Low || r.LimitMB != 1 {
		t.Errorf("under the limit: %+v", r)
	}
	big := randomLines(1100 << 10)
	e.put(e.admin, piece("s1", "main", int64(len(l)), big), big)
	if r := find(); !r.Low || r.Count != 2 {
		t.Errorf("over the limit: %+v", r)
	}
	// the total follows the pieces when an account goes
	jo := e.user("jo", false)
	e.put(jo, piece("s9", "main", 0, l), l)
	if err := e.s.DeleteUser(e.ctx, e.admin, jo.User.ID); err != nil {
		t.Fatal(err)
	}
	if r := find(); r.Count != 2 {
		t.Errorf("after jo's account went: %+v", r)
	}
	if _, err := e.s.SetResourceLimits(e.ctx, e.admin, ResourceLimits{TranscriptsMB: -1}); err == nil {
		t.Error("a negative limit")
	}
}

// 1218.50: the cap refuses with a stable error, counts the refusal, leaves no
// file behind, and still answers a piece it already holds.
func TestTranscriptQuotaRefuses(t *testing.T) {
	e := transcriptEnv(t)
	e.s.cfg.TranscriptMaxMB = 1
	l := randomLines(600 << 10)
	e.put(e.admin, piece("s1", "main", 0, l), l)
	l2 := randomLines(600 << 10)
	in := piece("s1", "main", int64(len(l)), l2)
	in.Gzip = tgz(t, l2)
	_, err := e.s.IngestTranscript(e.ctx, e.admin, in)
	if !errors.Is(err, ErrTranscriptQuota) {
		t.Fatalf("err = %v, want the quota refusal", err)
	}
	var files int
	_ = filepath.Walk(e.s.cfg.TranscriptDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("%d files on disk, want the one stored piece", files)
	}
	for _, r := range e.s.readResources(e.ctx) {
		if r.Resource == "transcripts" && (r.Refused != 1 || r.CapMB != 1) {
			t.Errorf("refusal not counted: %+v", r)
		}
	}
	// a duplicate of what is held is answered, not refused
	dup := piece("s1", "main", 0, l)
	dup.Gzip = tgz(t, l)
	if r, err := e.s.IngestTranscript(e.ctx, e.admin, dup); err != nil || r.Status != "duplicate" {
		t.Errorf("duplicate under the cap: %v %v", r, err)
	}
	// no cap: stored
	e.s.cfg.TranscriptMaxMB = 0
	if r, err := e.s.IngestTranscript(e.ctx, e.admin, in); err != nil || r.Status != "stored" {
		t.Errorf("no cap: %v %v", r, err)
	}
}

// wp usage machines reads the machines with their archive record: your own,
// anyone's for an admin (audit 1218.35).
func TestUsageMachinesListsTheArchiveRecord(t *testing.T) {
	e := transcriptEnv(t)
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PassMS: i64(5), Cut: i64(1), Off: yes(false)}); err != nil {
		t.Fatal(err)
	}
	ms, err := e.s.UsageMachines(e.ctx, e.admin, "")
	if err != nil || len(ms) != 1 || ms[0].Transcripts == nil || ms[0].Transcripts.Cut == nil || *ms[0].Transcripts.Cut != 1 {
		t.Fatalf("%+v %v", ms, err)
	}
	jo := e.user("jo", false)
	if _, err := e.s.UsageMachines(e.ctx, jo, e.admin.User.Username); err == nil {
		t.Error("jo read the admin's machines")
	}
	if ms, err := e.s.UsageMachines(e.ctx, e.admin, "all"); err != nil || len(ms) != 1 {
		t.Errorf("all: %+v %v", ms, err)
	}
}

// A machine not heard from for archiveWaitingWindow cannot send anything
// more, so bytes it left pending do not make it wait or stall for good.
func TestTranscriptCollectorRetiredMachineIsNotWaiting(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	e.machineRow("gone", `transcript_pending_bytes = 10, transcript_pending_files = 1, transcript_pending_since = ?, transcript_status_at = ?, last_sync_at = ?`,
		e.ago(40*24*time.Hour), e.ago(31*24*time.Hour), e.ago(32*24*time.Hour))
	if h := e.archiveHealth(); h.Stalled || h.Waiting != 0 {
		t.Fatalf("a machine silent for 31 days is waiting: %+v", h)
	}
	if n := e.collectorNotices(); len(n) != 0 {
		t.Fatalf("notices = %v", n)
	}
	e.machineRow("gone", `transcript_pending_since = ?, transcript_status_at = ?, last_sync_at = ?`,
		e.ago(72*time.Hour), e.ago(3*24*time.Hour), e.ago(3*24*time.Hour))
	if h := e.archiveHealth(); !h.Stalled || h.Waiting != 1 {
		t.Fatalf("a machine heard from 3 days ago with bytes waiting 72h: %+v", h)
	}
}

// The drops total can fall (a machine's state reset); drops after that are
// told even while the total is below the old high-water mark.
func TestTranscriptCollectorDropsToldAfterTotalFalls(t *testing.T) {
	e := transcriptEnv(t)
	e.setClock(time.Now())
	report := func(n int64) {
		t.Helper()
		if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", Dropped: map[string]int64{"line_too_large": n}}); err != nil {
			t.Fatal(err)
		}
	}
	report(10)
	e.archiveHealth()
	if n := e.collectorNotices(); len(n) != 1 {
		t.Fatalf("first telling: %v", n)
	}
	report(2)
	e.setClock(e.clockNow().Add(25 * time.Hour))
	e.archiveHealth()
	if n := e.collectorNotices(); len(n) != 1 {
		t.Fatalf("a fall is not told: %v", n)
	}
	report(5)
	e.setClock(e.clockNow().Add(25 * time.Hour))
	e.archiveHealth()
	n := e.collectorNotices()
	if len(n) != 2 {
		t.Fatalf("growth after the fall is not told: %v", n)
	}
	var grew int64
	if err := e.s.db.R.QueryRow(`SELECT dropped_told FROM collector_health WHERE name = 'transcript archive'`).Scan(&grew); err != nil || grew != 5 {
		t.Errorf("dropped_told = %d, %v", grew, err)
	}
}

// A stored piece keeps the client version it came from and its compressed
// size, and that size is what is on the disk and what the list reports
// (audit 1233.24).
func TestTranscriptPieceKeepsVersionAndStoredBytes(t *testing.T) {
	e := transcriptEnv(t)
	l := strings.Repeat(`{"n":1,"pad":"aaaaaaaaaaaaaaaaaaaa"}`+"\n", 20)
	in := piece("s1", "main", 0, l)
	in.Version = "wp-9.9.9"
	in.Gzip = tgz(t, l)
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
	var version, rel string
	var stored int64
	if err := e.s.db.R.QueryRow(`SELECT client_version, stored_bytes, path FROM transcript_pieces`).Scan(&version, &stored, &rel); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(e.s.cfg.TranscriptDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if version != "wp-9.9.9" || stored != int64(len(in.Gzip)) || stored != fi.Size() {
		t.Errorf("version %q stored %d, sent %d bytes, on disk %d", version, stored, len(in.Gzip), fi.Size())
	}
	files, err := e.s.TranscriptFiles(e.ctx, e.admin, TranscriptQuery{Session: "s1"})
	if err != nil || len(files) != 1 || files[0].StoredBytes != stored {
		t.Errorf("list: %+v %v", files, err)
	}
}
