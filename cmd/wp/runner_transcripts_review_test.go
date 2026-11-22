//go:build !windows

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A directory named x.jsonl is not a transcript: discovery skips it (see
// TestDiscoverTranscriptsIgnoresSymlinkedFiles) and the open refuses it.
func TestOpenRegularTranscriptRefusesADirectory(t *testing.T) {
	d := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if fh, err := openRegularTranscript(d); err == nil {
		_ = fh.Close()
		t.Error("opened a directory as a transcript")
	}
}

// A cancel by a person does not turn a finished job into a cancelled one.
func TestSettleOutcomeLeavesADoneOutcomeAlone(t *testing.T) {
	c := &atomic.Bool{}
	c.Store(true)
	o := settleOutcome(context.Background(), c, outcome{State: "done"})
	if o.State != "done" || o.Error != "" {
		t.Errorf("outcome = %+v", o)
	}
}

// The job-end pass reports nothing about the machine's backlog (its figures
// cover the job's files only), and it reads neither a file with bytes pending
// nor an outdated one that is not the job's.
func TestJobEndUploadLeavesTheBacklogAlone(t *testing.T) {
	var statusPosts atomic.Int32
	e, p, cfg, work, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/transcript/status" {
				statusPosts.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	pending := writeAt(t, filepath.Join(cfg, "projects", "-other", "pending.jsonl"), tline("p1", "pending"))
	old := writeAt(t, filepath.Join(cfg, "projects", "-other", "old.jsonl"), tline("o1", "old"))
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), archiveState{Files: map[string]*archiveFileState{pending: {Size: 500}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.dir, usageStateFile), usageState{Files: map[string]*usageFileState{old: {Off: 3}}}); err != nil {
		t.Fatal(err)
	}
	_, log := keepIt(p, work, nil)
	if !haveSession(e, "s1") {
		t.Fatalf("the job's session wasn't uploaded:\n%s", log)
	}
	if n := statusPosts.Load(); n != 0 {
		t.Errorf("the job-end pass posted the archive status %d times", n)
	}
	if haveSession(e, "pending") || haveSession(e, "old") {
		t.Error("the job-end pass uploaded a transcript that is not the job's")
	}
	if ov, err := e.svc.UsageOverview(context.Background(), jobEndActor[e], core.UsageQuery{Days: 3650}); err != nil || ov.Totals.Messages != 2 {
		t.Errorf("messages on the server = %v, %v (the outdated file was read?)", ov, err)
	}
}

// A server that keeps no archive: the log says so and does not promise a resend.
func TestJobEndUploadSaysWhenTheServerKeepsNoArchive(t *testing.T) {
	_, p, _, work, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && r.URL.Path == "/api/v1/usage/transcript" {
				http.Error(w, "no archive", http.StatusNotImplemented)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	_, log := keepIt(p, work, nil)
	if !strings.Contains(log, "keeps no transcript archive") || strings.Contains(log, "sends the rest") {
		t.Errorf("log = %s", log)
	}
}

// A job that ran claude but filed no directory still links its session id.
func TestKeepTranscriptsLinksTheSessionWithoutADirectory(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := newTestProc(t)
	work := filepath.Join(t.TempDir(), "job7-1")
	o, log := keepIt(p, work, &cliDriver{sessionID: "s9"})
	res, _ := o.Result.(map[string]any)
	if res["session_id"] != "s9" || !strings.Contains(log, "s9") {
		t.Errorf("result %v, log %q", o.Result, log)
	}
	if _, has := res["transcript_dir"]; has {
		t.Errorf("result = %v", o.Result)
	}
}

// The runner stopping cuts the job-end upload short (a person's cancel of one
// job does not), so the finish report is not held up by a slow server.
func TestRunnerStopCutsTheJobEndUpload(t *testing.T) {
	release := make(chan struct{})
	_, p, _, work, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/ingest" {
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	defer close(release)
	p.stopCtx, p.stopCancel = context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		p.stopAll(func() {})
	}()
	t0 := time.Now()
	_, log := keepIt(p, work, nil)
	if d := time.Since(t0); d > 15*time.Second {
		t.Errorf("the end of the job took %v after the runner was stopped", d)
	}
	if !strings.Contains(log, "runner is stopping") {
		t.Errorf("log = %s", log)
	}
}

// An expired bound makes no archive-off post.
func TestArchiveReportOffHonoursTheBound(t *testing.T) {
	var posts atomic.Int32
	e, _, _, _, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posts.Add(1)
			h.ServeHTTP(w, r)
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = archiveReportOff(&cli{dir: e.dir, out: &strings.Builder{}, err: &strings.Builder{}}, usageSyncOptions{Ctx: ctx})
	if n := posts.Load(); n != 0 {
		t.Errorf("%d requests with an expired bound", n)
	}
}
