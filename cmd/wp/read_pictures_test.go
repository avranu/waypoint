package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A picture from the project, any task's attachments, wp shot or this
// project's scratchpad is let through unasked; anything else is left to
// Claude Code (WP-1137).
func TestPictureReadAllowed(t *testing.T) {
	tmp := realPath(t.TempDir())
	guardTempDirs = func() []string { return []string{tmp} }
	t.Cleanup(func() { guardTempDirs = func() []string { return []string{os.TempDir(), "/tmp", "/var/tmp"} } })

	base := realPath(t.TempDir())
	repo := filepath.Join(base, "repo")
	wt := filepath.Join(base, "repo-wt")
	other := filepath.Join(base, "other")
	write := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, d := range []string{filepath.Join(repo, ".git", "worktrees", "wt"), filepath.Join(other, ".git")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o600)
	_ = os.MkdirAll(wt, 0o750)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n"), 0o600)

	uid := "claude-" + strconv.Itoa(os.Getuid())
	secret := write(filepath.Join(base, "secret.txt"))
	outside := write(filepath.Join(base, "elsewhere", "a.png"))
	link := filepath.Join(tmp, "wp-files", "WP-2", "link.png")
	_ = os.MkdirAll(filepath.Dir(link), 0o750)
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	link2 := filepath.Join(tmp, "wp-shots", "link.png")
	_ = os.MkdirAll(filepath.Dir(link2), 0o750)
	if err := os.Symlink(outside, link2); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		want bool
	}{
		{write(filepath.Join(repo, "e2e", "out", "a.png")), true},
		{write(filepath.Join(wt, "e2e", "out", "b.PNG")), true}, // another worktree of the repo
		{write(filepath.Join(repo, "notes.txt")), false},        // not a picture
		{write(filepath.Join(other, "c.png")), false},           // another repository
		{outside, false},
		{write(filepath.Join(tmp, "wp-files", "WP-1", "d.jpg")), true},
		{write(filepath.Join(tmp, "wp-shots", "wp-1-x", "e.webp")), true},
		{write(filepath.Join(tmp, "f.png")), false},
		{write(filepath.Join(tmp, "else", "wp-files", "g.png")), false},
		{write(filepath.Join(tmp, uid, claudeProjectDir(repo), "s1", "scratchpad", "shots", "h.png")), true},
		{write(filepath.Join(tmp, uid, claudeProjectDir(wt), "s1", "scratchpad", "i.png")), true},
		{write(filepath.Join(tmp, uid, claudeProjectDir(other), "s1", "scratchpad", "j.png")), false}, // another project's
		{write(filepath.Join(tmp, uid, claudeProjectDir(repo), "s1", "tasks", "k.png")), false},
		{write(filepath.Join(tmp, "claude-1", claudeProjectDir(repo), "s1", "scratchpad", "l.png")), false}, // another user's
		{filepath.Join(tmp, "wp-files", "WP-1", "missing.png"), false},
		{link, false},  // a picture's name on another file
		{link2, false}, // a link out of the temp directory
		{"rel/a.png", false},
	} {
		if got := pictureReadAllowed(wt, c.path); got != c.want {
			t.Errorf("%s: %v, want %v", strings.TrimPrefix(c.path, base), got, c.want)
		}
	}

	// The hook answers allow, and only that, for one of them.
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	run := func(path string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Read", "cwd": repo,
			"tool_input": map[string]string{"file_path": path}})
		var out bytes.Buffer
		hookClaudeNudge(&cli{}, bytes.NewReader(in), &out)
		return out.String()
	}
	var o struct {
		Specific struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out := run(filepath.Join(tmp, "wp-files", "WP-1", "d.jpg"))
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Specific.Event != "PreToolUse" || o.Specific.Decision != "allow" || o.Specific.Context != "" {
		t.Errorf("allowed picture: %q (%v)", out, err)
	}
	if out := run(filepath.Join(tmp, "f.png")); out != "" {
		t.Errorf("a picture elsewhere was answered: %q", out)
	}
}
