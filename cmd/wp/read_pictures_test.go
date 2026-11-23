//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A picture from the project, any task's attachments, wp shot or this
// project's scratchpad is let through unasked; anything else is left to
// Claude Code (WP-1137).
func TestPictureReadAllowed(t *testing.T) {
	tmp := realPath(t.TempDir())
	old := guardTempDirs
	guardTempDirs = func() []string { return []string{tmp} }
	t.Cleanup(func() { guardTempDirs = old })

	base := realPath(t.TempDir())
	t.Setenv("HOME", filepath.Join(base, "home"))
	repo := filepath.Join(base, "repo")
	wt := filepath.Join(base, "repo-wt")
	other := filepath.Join(base, "other")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) string {
		t.Helper()
		must(os.MkdirAll(filepath.Dir(p), 0o750))
		must(os.WriteFile(p, []byte("x"), 0o600))
		return p
	}
	wtGit := filepath.Join(repo, ".git", "worktrees", "wt")
	must(os.MkdirAll(wtGit, 0o750))
	must(os.MkdirAll(filepath.Join(other, ".git"), 0o750))
	must(os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o600))
	must(os.WriteFile(filepath.Join(wtGit, "gitdir"), []byte(filepath.Join(wt, ".git")+"\n"), 0o600))
	must(os.MkdirAll(wt, 0o750))
	must(os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o600))

	uid := "claude-" + strconv.Itoa(os.Getuid())
	secret := write(filepath.Join(base, "secret.txt"))
	outside := write(filepath.Join(base, "elsewhere", "a.png"))
	link := filepath.Join(tmp, filesDirName, "WP-2", "link.png")
	must(os.MkdirAll(filepath.Dir(link), 0o750))
	must(os.Symlink(secret, link))
	link2 := filepath.Join(tmp, shotsDirName, "link.png")
	must(os.MkdirAll(filepath.Dir(link2), 0o750))
	must(os.Symlink(outside, link2))
	link3 := filepath.Join(repo, "docs", "out.png")
	must(os.MkdirAll(filepath.Dir(link3), 0o750))
	must(os.Symlink(outside, link3))
	dirPic := filepath.Join(repo, "dir.png")
	must(os.MkdirAll(dirPic, 0o750))
	pad := func(dir, name string) string {
		return write(filepath.Join(tmp, uid, projectDirName(dir), "s1", "scratchpad", name))
	}
	for _, c := range []struct {
		cwd, path string
		want      bool
	}{
		{wt, write(filepath.Join(repo, "e2e", "out", "a.png")), true}, // the main checkout, from a worktree
		{repo, write(filepath.Join(wt, "e2e", "out", "b.PNG")), true}, // a worktree, from the main checkout
		{wt, write(filepath.Join(wt, "c.png")), true},                 // the session's own checkout
		{wt, write(filepath.Join(repo, "notes.txt")), false},          // not a picture
		{wt, write(filepath.Join(other, "c.png")), false},             // another repository
		{wt, outside, false},
		{wt, link3, false},  // a link in the repository to a picture outside it
		{wt, dirPic, false}, // a directory with a picture's name
		{wt, write(filepath.Join(tmp, filesDirName, "WP-1", "d.jpg")), true},
		{wt, write(filepath.Join(tmp, shotsDirName, "wp-1-x", "e.webp")), true},
		{wt, write(filepath.Join(base, "out", shotsDirName, "e.png")), true}, // wp shot --out
		{wt, filepath.Join(tmp, filesDirName, "..", "f.png"), false},
		{wt, write(filepath.Join(tmp, "f.png")), false},
		{wt, write(filepath.Join(tmp, "else", filesDirName, "g.png")), true},       // wp files --dir under a wp-files directory
		{outside, write(filepath.Join(tmp, filesDirName, "WP-3", "g2.png")), true}, // outside a repository too
		{wt, pad(repo, "h.png"), true},
		{repo, pad(wt, "i.png"), true}, // a scratchpad of a worktree, from the main checkout
		{wt, pad(other, "j.png"), false},
		{wt, pad(repo+"-old", "j2.png"), false},                                        // a project whose name starts with this one's
		{wt, pad(filepath.Join(repo, "web"), "j3.png"), false},                         // a session started in a subdirectory: left to Claude Code
		{filepath.Join(repo, "web"), pad(filepath.Join(repo, "web"), "j4.png"), false}, // even from that subdirectory
		{outside, pad(filepath.Dir(outside), "j5.png"), false},                         // or outside any repository
		{wt, write(filepath.Join(tmp, uid, projectDirName(repo), "s1", "tasks", "k.png")), false},
		{wt, write(filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()+1), projectDirName(repo), "s1", "scratchpad", "l.png")), false}, // another user's
		{wt, filepath.Join(tmp, filesDirName, "WP-1", "missing.png"), false},
		{wt, link, false},  // a picture's name on another file
		{wt, link2, false}, // a link out of the temp directory
		{wt, "rel/a.png", false},
		// Outside any repository, Claude Code's own rules already cover the
		// session's directory; nothing else of it is let through.
		{outside, outside, false},
		{"/", outside, false},
	} {
		if got := pictureReadAllowed(c.cwd, c.path); got != c.want {
			t.Errorf("from %s, %s: %v, want %v", strings.TrimPrefix(c.cwd, base), strings.TrimPrefix(c.path, base), got, c.want)
		}
	}

	// Only where no one else can swap the picture for another file before the
	// Read: not in a directory others may write in, unless it is sticky and
	// what the path goes through in it is the user's.
	must(os.MkdirAll(filepath.Join(repo, "web"), 0o750))
	shared := filepath.Join(tmp, shotsDirName, "shared")
	sharedPic := write(filepath.Join(shared, "m.png"))
	deep := write(filepath.Join(shared, "sub", "n.png"))
	inRepo := write(filepath.Join(repo, "shared", "o.png"))
	for _, d := range []string{shared, filepath.Dir(inRepo)} {
		must(os.Chmod(d, 0o777)) // #nosec G302 -- a directory others may write in, on purpose
	}
	for _, p := range []string{sharedPic, deep, inRepo} {
		if pictureReadAllowed(wt, p) {
			t.Errorf("%s, under a directory others may write in, was let through", strings.TrimPrefix(p, base))
		}
	}
	must(os.Chmod(shared, 0o777|os.ModeSticky)) // #nosec G302 -- a shared temp directory
	for _, p := range []string{sharedPic, deep} {
		if !pictureReadAllowed(wt, p) {
			t.Errorf("%s, the user's own in a sticky directory, was not let through", strings.TrimPrefix(p, base))
		}
	}
	must(os.Chmod(shared, 0o700))

	// A repository that holds the home directory (dotfiles) is not a project:
	// the pictures under home are not let through.
	home := filepath.Join(base, "home")
	must(os.MkdirAll(filepath.Join(home, ".git"), 0o750))
	proj := filepath.Join(home, "work", "proj")
	must(os.MkdirAll(proj, 0o750))
	if pictureReadAllowed(proj, write(filepath.Join(home, "Pictures", "x.png"))) {
		t.Error("a picture under home was let through for a repository at home")
	}

	// The hook answers allow, and only that, for one of them.
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv(runnerJobEnv, "")
	hc := &cli{}
	run := func(tool, mode, path string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": tool, "cwd": repo,
			"permission_mode": mode, "tool_input": map[string]string{"file_path": path, "command": map[string]string{"Bash": "ls"}[tool]}})
		var out bytes.Buffer
		hookClaudeNudge(hc, bytes.NewReader(in), &out)
		return out.String()
	}
	allowed := func(out string) bool {
		var o struct {
			Specific struct {
				Event    string `json:"hookEventName"`
				Decision string `json:"permissionDecision"`
				Context  string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		err := json.Unmarshal([]byte(out), &o)
		return err == nil && o.Specific.Event == "PreToolUse" && o.Specific.Decision == "allow" && o.Specific.Context == ""
	}
	pic := filepath.Join(tmp, filesDirName, "WP-1", "d.jpg")
	if out := run("Read", "default", pic); !allowed(out) {
		t.Errorf("allowed picture: %q", out)
	}
	// A big picture is a picture, not a file of many lines to grep.
	big := filepath.Join(tmp, shotsDirName, "big.png")
	must(os.WriteFile(big, bytes.Repeat([]byte("\n"), 5000), 0o600))
	if out := run("Read", "default", big); !allowed(out) {
		t.Errorf("big picture: %q", out)
	}
	// Counted, so the state file shows the prompts saved.
	if st := hc.state.Nudges[pictureAllowKind]; st == nil || st.Shown != 2 {
		t.Errorf("allows counted: %+v, want the 2 above", st)
	}
	// A note and the allow together: a person's pause told on a picture Read.
	c := &cli{dir: t.TempDir()}
	c.savePause(json.RawMessage(testPause))
	in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Read", "cwd": repo,
		"permission_mode": "default", "tool_input": map[string]string{"file_path": pic}})
	var out bytes.Buffer
	hookClaudeNudge(c, bytes.NewReader(in), &out)
	var o struct {
		Specific struct {
			Decision string `json:"permissionDecision"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &o); err != nil || o.Specific.Decision != "allow" || o.Specific.Context == "" {
		t.Errorf("a note with the allow: %q", out.String())
	}
	for name, out := range map[string]string{
		"a picture elsewhere":           run("Read", "default", filepath.Join(tmp, "f.png")),
		"a Bash call naming a picture":  run("Bash", "default", pic),
		"a session told not to ask":     run("Read", "dontAsk", pic), // a read-only runner job: only what it was granted
		"a session that skips the asks": run("Read", "bypassPermissions", pic),
	} {
		if out != "" {
			t.Errorf("%s was answered: %q", name, out)
		}
	}
	// A runner's job reads only what its job grants: other jobs' checkouts
	// and attachments share its machine.
	t.Setenv(runnerJobEnv, "1")
	if out := run("Read", "default", pic); out != "" {
		t.Errorf("a runner job was answered: %q", out)
	}
	drv, err := claudeDriver(agentJob{CLI: "claude"}, "p", base)
	if err != nil || drv.env[runnerJobEnv] != "1" {
		t.Errorf("a runner's Claude Code job is not marked: %v %v", drv, err)
	}
}

// ownedByMe tells the user's own file from one someone else put there.
func TestOwnedByMe(t *testing.T) {
	for uid, want := range map[int]bool{os.Getuid(): true, os.Getuid() + 1: false} {
		if got := ownedByMe(fakeOwned{uid: uint32(uid)}); got != want { // #nosec G115 -- a test uid
			t.Errorf("uid %d: %v, want %v", uid, got, want)
		}
	}
}

type fakeOwned struct{ uid uint32 }

func (f fakeOwned) Name() string       { return "a.png" }
func (f fakeOwned) Size() int64        { return 1 }
func (f fakeOwned) Mode() os.FileMode  { return 0o600 }
func (f fakeOwned) ModTime() time.Time { return time.Time{} }
func (f fakeOwned) IsDir() bool        { return false }
func (f fakeOwned) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }
