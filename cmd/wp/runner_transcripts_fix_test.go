//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func tline(id, session string) string {
	return `{"type":"assistant","message":{"id":"` + id + `","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"timestamp":"` +
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `","cwd":"/w","sessionId":"` + session + `"}` + "\n"
}

func writeAt(t *testing.T, path, s string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func linkAt(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// A symlinked .jsonl is not a transcript (it could point at any readable
// file); a symlinked directory above the files is still walked.
func TestDiscoverTranscriptsIgnoresSymlinkedFiles(t *testing.T) {
	root := t.TempDir()
	secret := writeAt(t, filepath.Join(t.TempDir(), "secret.jsonl"), tline("x", "x"))
	proj := filepath.Join(root, "projects", "-a")
	real := writeAt(t, filepath.Join(proj, "s.jsonl"), tline("m1", "s"))
	sub := writeAt(t, filepath.Join(proj, "s", "subagents", "agent-x.jsonl"), tline("m2", "s"))
	linkAt(t, secret, filepath.Join(proj, "link.jsonl"))
	linkAt(t, secret, filepath.Join(proj, "s", "subagents", "agent-y.jsonl"))
	linkAt(t, secret, filepath.Join(proj, "t", "subagents", "agent-z.jsonl"))
	realDir := t.TempDir()
	viaDir := writeAt(t, filepath.Join(realDir, "d.jsonl"), tline("m3", "d"))
	linkAt(t, realDir, filepath.Join(root, "projects", "-b"))

	got := func(files []usageFile) []string {
		var p []string
		for _, f := range files {
			p = append(p, f.Path)
		}
		slices.Sort(p)
		return p
	}
	files, err := discoverTranscripts(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{real, sub, filepath.Join(root, "projects", "-b", "d.jsonl")}
	slices.Sort(want)
	if g := got(files); !slices.Equal(g, want) {
		t.Errorf("discovered %v, want %v (via dir link %s)", g, want, viaDir)
	}
	files, _ = discoverTranscripts(root, 0, []string{real, filepath.Join(proj, "link.jsonl")})
	if g := got(files); !slices.Equal(g, []string{real, sub}) {
		t.Errorf("discovered for --files: %v", g)
	}
}

// The archive pass doesn't upload a symlink even when the state file names it.
func TestArchivePassSkipsSymlinkedTranscripts(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	root := t.TempDir()
	secret := writeAt(t, filepath.Join(t.TempDir(), "secret.jsonl"), tline("x", "x"))
	link := filepath.Join(root, "projects", "-a", "linked.jsonl")
	linkAt(t, secret, link)
	writeAt(t, filepath.Join(root, "projects", "-a", "real.jsonl"), tline("m1", "real"))
	st := archiveState{Files: map[string]*archiveFileState{link: {Size: 100}}}
	if err := writeJSON(filepath.Join(e.dir, archiveStateFile), st); err != nil {
		t.Fatal(err)
	}
	archiveSync(t, e, root)
	if files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "linked"}); len(files) != 0 {
		t.Errorf("a symlinked transcript was uploaded: %+v", files)
	}
	if files, _ := e.svc.TranscriptFiles(context.Background(), me, core.TranscriptQuery{Session: "real"}); len(files) != 1 {
		t.Errorf("the real one wasn't: %+v", files)
	}
	// and the open itself refuses one the listing never saw
	a := &archiver{c: &cli{dir: e.dir}, st: archiveState{Files: map[string]*archiveFileState{}, Dropped: map[string]int64{}}, res: &archiveResult{Dropped: map[string]int64{}}, deadline: time.Now().Add(time.Minute)}
	a.file(archiveFileOfPath(link), 100)
	if a.res.Dropped["unreadable_file"] != 1 || a.res.Pieces != 0 {
		t.Errorf("file() opened a symlink: %+v", a.res)
	}
}

// jobEndSetup is a server the runner is logged in to, a config directory with
// the job's project directory (session s1 with a sub-agent) and the job's
// checkout path.
func jobEndSetup(t *testing.T, wrap func(http.Handler) http.Handler) (e *env, p *runnerProc, cfg, work, proj string) {
	t.Helper()
	e = newEnvServing(t, nil, wrap)
	jobEndActor[e] = e.login()
	t.Setenv("WAYPOINT_MACHINE", "testbox")
	cfg = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	work = filepath.Join(t.TempDir(), "job7-1")
	proj = filepath.Join(cfg, "projects", strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, work))
	writeAt(t, filepath.Join(proj, "s1.jsonl"), tline("m1", "s1"))
	writeAt(t, filepath.Join(proj, "s1", "subagents", "agent-a1.jsonl"), tline("m2", "s1"))
	p = newTestProc(t)
	p.c = &cli{dir: e.dir, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	return
}

var jobEndActor = map[*env]*core.Actor{}

func keepIt(p *runnerProc, work string, drv *cliDriver) (outcome, string) {
	run := keepRun(time.Minute)
	o := p.keepTranscripts(context.Background(), run, agentJob{CLI: "claude"}, work, time.Now(), drv, outcome{State: "done"})
	return o, run.out.text()
}

func haveSession(e *env, session string) bool {
	files, _ := e.svc.TranscriptFiles(context.Background(), jobEndActor[e], core.TranscriptQuery{Session: session})
	return len(files) > 0
}

// The job-end upload sends the job's sessions, not whatever else is waiting
// on the machine.
func TestJobEndUploadSendsOnlyTheJobsSessions(t *testing.T) {
	e, p, cfg, work, _ := jobEndSetup(t, nil)
	writeAt(t, filepath.Join(cfg, "projects", "-other", "other.jsonl"), tline("o1", "other"))
	_, log := keepIt(p, work, nil)
	if !haveSession(e, "s1") {
		t.Errorf("the job's session wasn't uploaded:\n%s", log)
	}
	if haveSession(e, "other") {
		t.Error("another project's transcript was uploaded at the end of the job")
	}
}

// A server that never answers can't hold the job's end past the limit.
func TestJobEndUploadIsTimeBounded(t *testing.T) {
	release := make(chan struct{})
	_, p, _, work, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/usage/") {
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
	old := transcriptUploadLimit
	transcriptUploadLimit = 300 * time.Millisecond
	defer func() { transcriptUploadLimit = old }()
	t0 := time.Now()
	_, log := keepIt(p, work, nil)
	if d := time.Since(t0); d > 3*time.Second {
		t.Errorf("the end of the job took %v", d)
	}
	if !strings.Contains(log, "not uploaded yet") {
		t.Errorf("log = %s", log)
	}
}

// Another sync holds the lock: look again, then leave it to the timer, and
// don't wait after the last look.
func TestJobEndUploadRetriesWhenBusy(t *testing.T) {
	oldW, oldT := transcriptBusyWait, transcriptBusyTries
	defer func() { transcriptBusyWait, transcriptBusyTries = oldW, oldT }()

	e, p, _, work, _ := jobEndSetup(t, nil)
	unlock, ok := tryLock(filepath.Join(e.dir, "usage-sync.lock"), time.Hour)
	if !ok {
		t.Fatal("no lock")
	}
	transcriptBusyWait, transcriptBusyTries = 400*time.Millisecond, 2
	t0 := time.Now()
	_, log := keepIt(p, work, nil)
	d := time.Since(t0)
	if d < 400*time.Millisecond || d > 700*time.Millisecond {
		t.Errorf("two tries 400ms apart took %v", d)
	}
	if !strings.Contains(log, "not uploaded yet") || haveSession(e, "s1") {
		t.Errorf("log = %s", log)
	}

	// released while it waits: the second try sends
	transcriptBusyWait, transcriptBusyTries = 300*time.Millisecond, 3
	go func() { time.Sleep(100 * time.Millisecond); unlock() }()
	_, log = keepIt(p, work, nil)
	if strings.Contains(log, "not uploaded") || !haveSession(e, "s1") {
		t.Errorf("log = %s", log)
	}
}

// The counts went but the archive's status post failed: not "nothing sent".
func TestJobEndUploadSaysWhenOnlyTheArchiveFailed(t *testing.T) {
	_, p, _, work, _ := jobEndSetup(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/usage/transcript/status" {
				http.Error(w, "no", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	_, log := keepIt(p, work, nil)
	if !strings.Contains(log, "usage counts were uploaded") || strings.Contains(log, "Transcripts not uploaded yet") {
		t.Errorf("log = %s", log)
	}
}

// Another archive pass is running: it sends these too.
func TestJobEndUploadSaysWhenTheArchiveIsBusy(t *testing.T) {
	e, p, _, work, _ := jobEndSetup(t, nil)
	unlock, ok := tryLock(filepath.Join(e.dir, "usage-archive.lock"), time.Hour)
	if !ok {
		t.Fatal("no lock")
	}
	defer unlock()
	_, log := keepIt(p, work, nil)
	if !strings.Contains(log, "another transcript archive pass is running") {
		t.Errorf("log = %s", log)
	}
}

func TestKeepTranscriptsWithNoUsageSyncSaysSo(t *testing.T) {
	e, p, _, work, _ := jobEndSetup(t, nil)
	p.opts.NoUsageSync = true
	_, log := keepIt(p, work, nil)
	if !strings.Contains(log, "this machine only") || haveSession(e, "s1") {
		t.Errorf("log = %s", log)
	}
}

// A claude job that filed no session has no session id to link, and no
// directory worth naming; a job that ended before the CLI was built is quiet.
func TestKeepTranscriptsWhenNothingWasFiled(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := newTestProc(t)
	p.opts.NoUsageSync = true
	work := filepath.Join(t.TempDir(), "job7-1")

	o, log := keepIt(p, work, &cliDriver{})
	if o.Result != nil || !strings.Contains(log, "No transcript found for this job under "+filepath.Join(cfg, "projects")) {
		t.Errorf("no dir: result %v, log %q", o.Result, log)
	}
	if o, log = keepIt(p, work, nil); o.Result != nil || log != "" {
		t.Errorf("no CLI: result %v, log %q", o.Result, log)
	}

	proj := filepath.Join(cfg, "projects", "-x-job7-1")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	o, log = keepIt(p, work, &cliDriver{})
	res, _ := o.Result.(map[string]any)
	if _, has := res["session_id"]; has {
		t.Errorf("result = %v", o.Result)
	}
	if _, has := res["transcript_dir"]; has {
		t.Errorf("result = %v", o.Result)
	}
	if !strings.Contains(log, "no session") {
		t.Errorf("log = %q", log)
	}
}

// A [ in the config directory is a path, not a pattern.
func TestJobProjectDirWithGlobCharactersInTheConfigDir(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cfg[1]")
	proj := filepath.Join(cfg, "projects", "-tmp-x-job7-1")
	writeAt(t, filepath.Join(proj, "a.jsonl"), tline("m1", "a"))
	writeAt(t, filepath.Join(cfg, "projects", "-tmp-x-job7-10", "b.jsonl"), tline("m2", "b"))
	if got := jobProjectDir(cfg, "/tmp/x/job7-1"); got != proj {
		t.Errorf("jobProjectDir = %q, want %q", got, proj)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := newTestProc(t)
	p.opts.NoUsageSync = true
	keepIt(p, "/tmp/x/job7-1", &cliDriver{sessionID: "a"})
	var meta jobMeta
	b, err := os.ReadFile(filepath.Join(proj, jobSidecarFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil || !slices.Equal(meta.Sessions, []string{"a"}) {
		t.Errorf("sidecar = %+v, %v", meta, err)
	}
}
