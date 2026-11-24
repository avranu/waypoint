//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	must(os.Chmod(tmp, 0o700))
	must(os.Chmod(base, 0o700))
	if d := notPrivateDir(write(filepath.Join(tmp, "probe.png"))); d != "" {
		t.Fatalf("this machine's %s, above the test's temp directory, is open to others: nothing under it can be let through", d)
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
	up := filepath.Join(tmp, filesDirName, "WP-4", "up")
	must(os.MkdirAll(filepath.Dir(up), 0o750))
	must(os.Symlink(filepath.Dir(outside), up))
	write(filepath.Join(tmp, filesDirName, "WP-4", "a.png"))
	write(filepath.Join(base, "a.png"))
	pad := func(dir, name string) string {
		return write(filepath.Join(tmp, uid, projectDirName(dir), "s1", "scratchpad", name))
	}
	for _, c := range []struct {
		cwd, path string
		want      bool
	}{
		{wt, write(filepath.Join(repo, "e2e", "out", "a.png")), true},
		{repo, write(filepath.Join(wt, "e2e", "out", "b.PNG")), true},
		{wt, write(filepath.Join(wt, "c.png")), true},
		{wt, write(filepath.Join(repo, "notes.txt")), false},
		{wt, write(filepath.Join(other, "c.png")), false},
		{wt, outside, false},
		{wt, link3, false},
		{wt, dirPic, false},
		{wt, write(filepath.Join(tmp, filesDirName, "WP-1", "d.jpg")), true},
		{wt, write(filepath.Join(tmp, shotsDirName, "wp-1-x", "e.webp")), true},
		{wt, write(filepath.Join(wt, "out", shotsDirName, "e.png")), true},
		{wt, write(filepath.Join(base, "out", shotsDirName, "e2.png")), false},
		{wt, write(filepath.Join(other, shotsDirName, "e3.png")), false},
		{wt, write(filepath.Join(other, filesDirName, "WP-1", "e4.png")), false},
		{wt, filepath.Join(tmp, filesDirName) + "/../f.png", false},
		{wt, up + "/../a.png", false},
		{wt, write(filepath.Join(tmp, "f.png")), false},
		{wt, write(filepath.Join(tmp, "else", filesDirName, "g.png")), false},
		{outside, write(filepath.Join(tmp, filesDirName, "WP-3", "g2.png")), true},
		{wt, pad(repo, "h.png"), true},
		{repo, pad(wt, "i.png"), true},
		{wt, pad(other, "j.png"), false},
		{wt, pad(repo+"-old", "j2.png"), false},
		{wt, pad(filepath.Join(repo, "web"), "j3.png"), false},
		{filepath.Join(repo, "web"), pad(filepath.Join(repo, "web"), "j4.png"), false},
		{outside, pad(filepath.Dir(outside), "j5.png"), false},
		{wt, write(filepath.Join(tmp, uid, projectDirName(repo), "s1", "tasks", "k.png")), false},
		{wt, write(filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()+1), projectDirName(repo), "s1", "scratchpad", "l.png")), false},
		{wt, filepath.Join(tmp, filesDirName, "WP-1", "missing.png"), false},
		{wt, link, false},
		{wt, link2, false},
		{wt, "rel/a.png", false},
		{outside, outside, false},
		{"/", outside, false},
	} {
		if got := pictureReadAllowed(c.cwd, c.path); got != c.want {
			t.Errorf("from %s, %s: %v, want %v", strings.TrimPrefix(c.cwd, base), strings.TrimPrefix(c.path, base), got, c.want)
		}
	}

	tmpLink := filepath.Join(base, "tmplink")
	must(os.Symlink(tmp, tmpLink))
	repoLink := filepath.Join(base, "repolink")
	must(os.Symlink(repo, repoLink))
	open := filepath.Join(base, "open")
	must(os.MkdirAll(open, 0o750))
	must(os.Symlink(tmp, filepath.Join(open, "t")))
	must(os.Chmod(open, 0o777)) // #nosec G302 -- a directory others may write in, on purpose
	guardTempDirs = func() []string { return []string{tmpLink} }
	for _, c := range []struct {
		cwd, path string
		want      bool
	}{
		{wt, filepath.Join(tmpLink, filesDirName, "WP-1", "d.jpg"), true},
		{wt, filepath.Join(tmpLink, shotsDirName, "wp-1-x", "e.webp"), true},
		{repoLink, filepath.Join(repoLink, "e2e", "out", "a.png"), true},
		{wt, filepath.Join(repoLink, "e2e", "out", "a.png"), true},
		{repoLink, pad(repoLink, "p.png"), true},
		{wt, filepath.Join(open, "t", filesDirName, "WP-1", "d.jpg"), false},
	} {
		if got := pictureReadAllowed(c.cwd, c.path); got != c.want {
			t.Errorf("through a link, from %s, %s: %v, want %v", strings.TrimPrefix(c.cwd, base), strings.TrimPrefix(c.path, base), got, c.want)
		}
	}
	must(os.Chmod(open, 0o700))
	guardTempDirs = func() []string { return []string{tmp} }

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

	home := filepath.Join(base, "home")
	must(os.MkdirAll(filepath.Join(home, ".git"), 0o750))
	proj := filepath.Join(home, "work", "proj")
	must(os.MkdirAll(proj, 0o750))
	if pictureReadAllowed(proj, write(filepath.Join(home, "Pictures", "x.png"))) {
		t.Error("a picture under home was let through for a repository at home")
	}

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
	big := filepath.Join(tmp, shotsDirName, "big.png")
	must(os.WriteFile(big, bytes.Repeat([]byte("\n"), 5000), 0o600))
	if out := run("Read", "default", big); !allowed(out) {
		t.Errorf("big picture: %q", out)
	}
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
		"a session told not to ask":     run("Read", "dontAsk", pic),
		"a session that skips the asks": run("Read", "bypassPermissions", pic),
	} {
		if out != "" {
			t.Errorf("%s was answered: %q", name, out)
		}
	}
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

// notPrivateDir: the highest directory above p that pathPrivate refuses,
// with its mode and owner; empty if none does.
func notPrivateDir(p string) string {
	var chain []string
	for d := p; ; d = filepath.Dir(d) {
		chain = append(chain, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	for i := len(chain) - 2; i >= 0; i-- {
		fi, err := os.Lstat(chain[i])
		if err != nil {
			return err.Error()
		}
		if !pathPrivate(chain[i], fi) {
			d := chain[i+1]
			di, err := os.Lstat(d)
			if err != nil {
				return err.Error()
			}
			st, _ := di.Sys().(*syscall.Stat_t)
			if st == nil {
				st = &syscall.Stat_t{}
			}
			return fmt.Sprintf("%s (mode %v, uid %d, gid %d; the tests run as %d:%d, umask aside)", d, di.Mode(), st.Uid, st.Gid, os.Getuid(), os.Getgid())
		}
	}
	return ""
}
