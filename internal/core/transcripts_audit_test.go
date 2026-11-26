package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Raw transcripts are for a person at their own terminal or in the web: not
// for a coding agent's shell, an agent session or an agent token (audit
// 1218.2). Reading them out is what the guard is for: they hold what the
// agent was never shown.
func TestTranscriptReadsRefuseAgentShells(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	l := `{"cmd":"x"}` + "\n"
	e.put(jo, piece("s1", "main", 0, l), l)
	for name, a := range map[string]*Actor{
		"agent env": {User: jo.User, AgentEnv: "claude"},
		"session":   {User: jo.User, Session: 7},
		"agent":     agentActor(jo, "anthropic/claude-code"),
	} {
		var b bytes.Buffer
		if err := e.s.ReadTranscript(e.ctx, a, TranscriptQuery{Session: "s1", File: "main"}, &b); !errors.Is(err, ErrForbidden) || b.Len() != 0 {
			t.Errorf("%s read: err %v, %d bytes", name, err, b.Len())
		}
		if _, err := e.s.TranscriptFiles(e.ctx, a, TranscriptQuery{Session: "s1"}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s list: %v", name, err)
		}
	}
}

// A runner job's token is scoped to its task; it may not write into its
// owner's transcript archive (1218.7).
func TestTranscriptWritesRefuseJobTokens(t *testing.T) {
	e := transcriptEnv(t)
	job := &Actor{User: e.admin.User, Agent: "anthropic/claude-code", RunnerJob: 5, Scope: map[int64]int{1: RoleEditor}}
	l := "{}\n"
	in := piece("s1", "main", 0, l)
	in.Gzip = tgz(t, l)
	if _, err := e.s.IngestTranscript(e.ctx, job, in); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), TranscriptJobRefusal) {
		t.Errorf("a job's piece: %v", err)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, job, TranscriptStatus{Machine: "box", PendingBytes: 1}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a job's status: %v", err)
	}
	if _, err := e.s.IngestTranscript(e.ctx, agentActor(e.admin, "anthropic/claude-code"), in); err != nil {
		t.Errorf("an agent token's piece: %v", err)
	}
}

// A client that lost its state after a rewrite is told where the server's
// copy ends and which generation it is, and stores nothing again (1218.22).
func TestTranscriptLostStateDoesNotStoreAThirdCopy(t *testing.T) {
	e := transcriptEnv(t)
	old, rewritten := `{"v":"old"}`+"\n", `{"v":"new"}`+"\n"+`{"v":"newer"}`+"\n"
	first := piece("s1", "main", 0, old)
	first.FP = "H#0"
	e.put(e.admin, first, old)
	second := piece("s1", "main", 0, rewritten)
	second.FP = "H#1"
	if r := e.put(e.admin, second, rewritten); r.Status != "stored" || r.Gen != 1 {
		t.Fatalf("the rewrite: %+v", r)
	}
	lost := piece("s1", "main", 0, rewritten)
	lost.FP = "H#0"
	r := e.put(e.admin, lost, rewritten)
	if r.Status == "stored" || r.Have != int64(len(rewritten)) || r.Gen != 1 {
		t.Fatalf("after losing state: %+v", r)
	}
	files, err := e.s.TranscriptFiles(e.ctx, e.admin, TranscriptQuery{Session: "s1"})
	if err != nil || len(files) != 1 || files[0].Gen != 1 || files[0].Pieces != 1 {
		t.Fatalf("files: %+v %v", files, err)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM transcript_pieces`).Scan(&n); err != nil || n != 2 {
		t.Errorf("pieces held: %d %v (the old and the rewrite, nothing more)", n, err)
	}
	third := piece("s1", "main", 0, old)
	third.FP = "H#2"
	if r := e.put(e.admin, third, old); r.Status != "stored" || r.Gen != 2 {
		t.Errorf("a second rewrite: %+v", r)
	}
}

// The piece's file is written before the write transaction takes the one
// writer connection, and one that is not stored does not stay on disk
// (1218.23). The hook runs between the two and writes to the database: it
// would block for good if the file were written inside the transaction.
func TestTranscriptFileIsWrittenOutsideTheTransaction(t *testing.T) {
	e := transcriptEnv(t)
	l1, l2 := `{"n":1}`+"\n", `{"n":2}`+"\n"
	ran := false
	e.s.afterTranscriptFile = func() {
		ran = true
		ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
		defer cancel()
		if err := e.s.write(ctx, e.admin, func(t *tx) error { return nil }); err != nil {
			t.Errorf("the writer was held while the file was written: %v", err)
		}
		e.s.afterTranscriptFile = nil
		e.put(e.admin, piece("s1", "main", 0, l1), l1)
	}
	in := piece("s1", "main", 0, l2)
	in.Gzip = tgz(t, l2)
	r, err := e.s.IngestTranscript(e.ctx, e.admin, in)
	if err != nil || !ran {
		t.Fatalf("%+v %v ran=%v", r, err, ran)
	}
	if r.Status == "stored" {
		t.Errorf("the losing piece was stored: %+v", r)
	}
	if got := e.readAll(e.admin, TranscriptQuery{Session: "s1", File: "main"}); got != l1 {
		t.Errorf("read back: %q", got)
	}
	var files []string
	_ = filepath.WalkDir(e.s.cfg.TranscriptDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Errorf("files on disk: %v", files)
	}
}

// Deleting a person removes their archive from the disk too, and the audit
// entry says so (1218.24).
func TestDeletingAUserRemovesTheirTranscripts(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	l := "{}\n"
	e.put(jo, piece("s1", "main", 0, l), l)
	dir := filepath.Join(e.s.cfg.TranscriptDir, fmt.Sprint(jo.User.ID))
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteUser(e.ctx, e.admin, jo.User.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("jo's transcripts are still on disk: %v", err)
	}
	var detail string
	if err := e.s.db.R.QueryRow(`SELECT detail FROM audit_log WHERE action = 'user.delete'`).Scan(&detail); err != nil || !strings.Contains(detail, "transcript") {
		t.Errorf("audit detail %q %v", detail, err)
	}
}

// With two machines holding the file, the refusal names how to choose.
func TestTranscriptReadAskingForTwoMachinesSaysHow(t *testing.T) {
	e := transcriptEnv(t)
	l := "{}\n"
	e.put(e.admin, piece("s1", "main", 0, l), l)
	other := piece("s1", "main", 0, l)
	other.Machine = "laptop"
	e.put(e.admin, other, l)
	err := e.s.ReadTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1", File: "main"}, &bytes.Buffer{})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "--machine") || !strings.Contains(err.Error(), "--user") {
		t.Errorf("err %v", err)
	}
	var b bytes.Buffer
	if err := e.s.ReadTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1", File: "main", Machine: "laptop"}, &b); err != nil || b.String() != l {
		t.Errorf("choosing a machine: %q %v", b.String(), err)
	}
}

// The guards on what a piece or a status may say (1218.39).
func TestTranscriptCoreGuards(t *testing.T) {
	e := transcriptEnv(t)
	l := "{}\n"
	in := piece("s1", "main", 0, l)
	in.Gzip = tgz(t, l)
	ro := &Actor{User: e.admin.User, ReadOnly: true}
	if _, err := e.s.IngestTranscript(e.ctx, ro, in); !errors.Is(err, ErrForbidden) {
		t.Errorf("a read-only token's piece: %v", err)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, ro, TranscriptStatus{Machine: "box"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a read-only token's status: %v", err)
	}
	if _, err := e.s.IngestTranscript(e.ctx, &Actor{User: &User{ID: 0}}, in); err == nil {
		t.Error("the system actor stored a piece")
	}
	big := in
	big.Gzip = make([]byte, TranscriptMaxUpload+1)
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, big); !IsInvalid(err) {
		t.Errorf("over the compressed cap: %v", err)
	}
	huge := in
	huge.Gzip = tgz(t, strings.Repeat("a", TranscriptMaxPiece)+"\n")
	if _, err := e.s.IngestTranscript(e.ctx, e.admin, huge); !IsInvalid(err) || !strings.Contains(err.Error(), "uncompressed") {
		t.Errorf("over the uncompressed cap: %v", err)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PendingBytes: -1}); !IsInvalid(err) {
		t.Errorf("negative pending: %v", err)
	}
	many := map[string]int64{}
	for i := range 60 {
		many[fmt.Sprintf("reason_%02d", i)] = 1
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", Dropped: many}); err != nil {
		t.Fatal(err)
	}
	ms, err := e.s.usageMachines(e.ctx, &usageScope{uids: []int64{e.admin.User.ID}})
	if err != nil || len(ms) != 1 || len(ms[0].Transcripts.Dropped) != 41 || ms[0].Transcripts.Dropped["(many reasons)"] != 20 {
		t.Errorf("folded reasons: %+v %v", ms, err)
	}
}

// A hole between pieces, and a piece missing from the disk, are errors.
func TestTranscriptHoleAndMissingPiece(t *testing.T) {
	e := transcriptEnv(t)
	l1, l2 := `{"n":1}`+"\n", `{"n":2}`+"\n"
	e.put(e.admin, piece("s1", "main", 0, l1), l1)
	e.put(e.admin, piece("s1", "main", int64(len(l1)), l2), l2)
	var rel string
	if err := e.s.db.R.QueryRow(`SELECT path FROM transcript_pieces WHERE from_off > 0`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.s.cfg.TranscriptDir, rel)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReadTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1", File: "main"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Errorf("missing piece: %v", err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE transcript_pieces SET from_off = from_off + 5 WHERE from_off > 0`); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReadTranscript(e.ctx, e.admin, TranscriptQuery{Session: "s1", File: "main"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "hole") {
		t.Errorf("hole: %v", err)
	}
}

// The transcript-archive collector: waiting is the machines with bytes
// pending, the last output the newest piece stored (1218.41), and its
// notice speaks of machines, not of an unnamed count (1218.37).
func TestTranscriptCollectorValues(t *testing.T) {
	e := transcriptEnv(t)
	const name = "transcript archive"
	get := func() CollectorHealth {
		t.Helper()
		e.s.CheckCollectorHealth(e.ctx)
		got, err := e.s.CollectorHealthList(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range got {
			if h.Name == name {
				return h
			}
		}
		t.Fatalf("no collector %q", name)
		return CollectorHealth{}
	}
	if h := get(); h.Waiting != 0 || h.LastOutputAt != nil || h.Stalled {
		t.Errorf("empty: %+v", h)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "box", PendingBytes: 10, PendingFiles: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReportTranscriptStatus(e.ctx, e.admin, TranscriptStatus{Machine: "laptop", PendingBytes: 0}); err != nil {
		t.Fatal(err)
	}
	if h := get(); h.Waiting != 1 || h.LastOutputAt != nil {
		t.Errorf("one machine waiting: %+v", h)
	}
	l := "{}\n"
	e.put(e.admin, piece("s1", "main", 0, l), l)
	h := get()
	if h.Waiting != 1 || h.LastOutputAt == nil || !h.LastOutputAt.Equal(e.clockNow().UTC().Truncate(time.Millisecond)) {
		t.Errorf("after a piece: %+v (clock %v)", h, e.clockNow())
	}
	c := collectorByName(t, name)
	if c.unit != "machines" {
		t.Errorf("unit %q", c.unit)
	}
}

func collectorByName(t *testing.T, name string) collector {
	t.Helper()
	for _, c := range collectors {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no collector %q", name)
	return collector{}
}

func userDeleteDetail(t *testing.T, e *env) string {
	t.Helper()
	var detail string
	if err := e.s.db.R.QueryRow(`SELECT detail FROM audit_log WHERE action = 'user.delete'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

// The delete entry promises the removal only when a removal follows, and says
// nothing of transcripts on a server that keeps none (audit 1233.11).
func TestUserDeleteAuditWordingFollowsTheArchive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keeps   bool
		want    string
		notWant string
	}{
		{"keeps an archive", true, "their archived transcripts are removed from the disk next", ""},
		{"keeps none", false, "", "transcript"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.keeps {
				e.s.cfg.TranscriptDir = t.TempDir()
			}
			jo := e.user("jo", false)
			if err := e.s.DeleteUser(e.ctx, e.admin, jo.User.ID); err != nil {
				t.Fatal(err)
			}
			e.s.Wait()
			d := userDeleteDetail(t, e)
			if tc.want != "" && !strings.Contains(d, tc.want) {
				t.Errorf("detail %q lacks %q", d, tc.want)
			}
			if tc.notWant != "" && strings.Contains(d, tc.notWant) {
				t.Errorf("detail %q mentions %q", d, tc.notWant)
			}
		})
	}
}

// A removal that fails leaves its own audit entry, so the trail does not say
// files are gone that are not (audit 1233.11).
func TestUserDeleteRemovalFailureIsAudited(t *testing.T) {
	e := transcriptEnv(t)
	jo := e.user("jo", false)
	l := "{}\n"
	e.put(jo, piece("s1", "main", 0, l), l)
	e.s.removeAll = func(string) error { return errors.New("disk said no") }
	if err := e.s.DeleteUser(e.ctx, e.admin, jo.User.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	var detail, target string
	var by int64
	if err := e.s.db.R.QueryRow(`SELECT detail, target, user_id FROM audit_log WHERE action = 'user.transcripts.remove_failed'`).Scan(&detail, &target, &by); err != nil {
		t.Fatalf("no failure entry: %v", err)
	}
	if !strings.Contains(detail, "disk said no") || target != "jo" || by != e.admin.User.ID {
		t.Errorf("entry: detail %q target %q by %d", detail, target, by)
	}
}
