package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
)

// the automatic upload runs in a detached child; its failures go only to upload.log beside the index, never the terminal

const (
	uploadEvery    = 24 * time.Hour
	uploadRetry    = time.Hour
	uploadChildEnv = "WP_CODE_UPLOAD_CHILD"
)

type uploadStamp struct {
	At     time.Time `json:"at"`
	Commit string    `json:"commit"`
	OK     bool      `json:"ok"`
}

func uploadStampPath(ix *codeindex.Index) string {
	return filepath.Join(filepath.Dir(ix.DBPath), "upload.json")
}

func readUploadStamp(p string) (s uploadStamp) {
	if b, err := os.ReadFile(p); err == nil { // #nosec G304 -- our own file in the git dir
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func writeUploadStamp(p string, s uploadStamp) {
	if b, err := json.Marshal(s); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// uploadDue says whether a new summary should go up: none was sent, or the
// last is a day old (an hour after a failure).
func uploadDue(s uploadStamp, now time.Time) bool {
	if s.At.IsZero() {
		return true
	}
	if !s.OK {
		return now.Sub(s.At) >= uploadRetry
	}
	return now.Sub(s.At) >= uploadEvery
}

// onDefaultBranch is true when the checkout has the default branch checked
// out with no uncommitted changes: what it knows is the repo's own state.
func onDefaultBranch(root string) bool {
	def := defaultBranchRef(root)
	if def == "" {
		return false
	}
	cur, err := gitIn(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false
	}
	for i := len(def) - 1; i >= 0; i-- {
		if def[i] == '/' {
			def = def[i+1:]
			break
		}
	}
	if cur != def {
		return false
	}
	st, err := gitIn(root, "status", "--porcelain")
	return err == nil && st == ""
}

// maybeUpload starts a background upload when this checkout is on the default
// branch, Waypoint is connected and the last upload is old. Never fails the
// command it rides on.
func (c *cli) maybeUpload(ix *codeindex.Index) {
	if os.Getenv(uploadChildEnv) != "" || os.Getenv("WP_CODE_UPLOAD") == "off" || c.token() == "" || c.server() == "" {
		return
	}
	stamp := uploadStampPath(ix)
	if !uploadDue(readUploadStamp(stamp), time.Now()) || !onDefaultBranch(ix.Root) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	writeUploadStamp(stamp, uploadStamp{At: time.Now(), OK: true})                                                             // the child corrects it; this stops a stampede
	logf, err := os.OpenFile(filepath.Join(filepath.Dir(ix.DBPath), "upload.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- git dir
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "code", "index", "--upload", "--db", ix.DBPath) // #nosec G204 -- our own binary
	cmd.Dir = ix.Root
	cmd.Env = append(os.Environ(), uploadChildEnv+"=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachedProc()
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return
	}
	go func() { _ = cmd.Wait() }()
	_ = logf.Close()
}

// uploadSummary builds the summary of the index and sends it for the repo
// of the project in scope that this checkout is.
func (c *cli) uploadSummary(ix *codeindex.Index, sf scopeFlags) error {
	stamp := uploadStampPath(ix)
	err := func() error {
		sum, err := ix.Summary()
		if err != nil {
			return err
		}
		key, repos, err := c.projectRepos(sf, "")
		if err != nil {
			return err
		}
		r, err := pickRepo(repos, "")
		if err != nil {
			return err
		}
		var info struct {
			Commit string `json:"commit"`
			Stored int    `json:"stored_files"`
			Pairs  int    `json:"pairs"`
		}
		if err := c.do("PUT", fmt.Sprintf("/api/v1/projects/%s/repos/%d/code", esc(key), r.ID), sum, &info); err != nil {
			return err
		}
		if !c.asJSON {
			c.println(fmt.Sprintf("uploaded %s: %d files with history, %d co-change pairs (commit %s)", r.Repo, info.Stored, info.Pairs, short12(info.Commit)))
		}
		return nil
	}()
	s := uploadStamp{At: time.Now(), Commit: ix.HeadCommit(), OK: err == nil}
	writeUploadStamp(stamp, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s code summary upload failed: %v\n", time.Now().Format(time.RFC3339), err)
	}
	return err
}
