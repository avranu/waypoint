package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A screenshot the agent reads goes to the task taken in its directory,
// once per content; old pictures, other files, wp files downloads and
// sessions with no task send nothing (WP-263).
func TestHookAttachesScreenshotsTheAgentReads(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	ctx := context.Background()
	issue, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: "New board"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	_ = c.updateState(func(st *State) {
		st.Claims = map[string]*claim{s.repo: {Key: issue.Key, ClaimedAt: time.Now().Add(-time.Minute), Session: "sess-1"}}
	})
	shots := t.TempDir()
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	write := func(rel, body string, age time.Duration) string {
		path := filepath.Join(shots, rel)
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		_ = os.Chtimes(path, at, at)
		return path
	}
	read := func(session, cwd, path string) {
		s.hook(map[string]any{"session_id": session, "hook_event_name": "PostToolUse", "cwd": cwd, "tool_name": "Read",
			"tool_input": map[string]any{"file_path": path}})
	}
	files := func() []core.Attachment {
		d, err := s.svc.GetIssue(ctx, s.me, issue.Key)
		if err != nil {
			t.Fatal(err)
		}
		return d.Files
	}
	shot := write("shot.png", png, 0)
	read("sess-1", s.repo, shot)
	read("sess-1", s.repo, shot)                                     // the same image again
	read("sess-1", s.repo, write("old.png", png+"old", time.Hour))   // made before the task was taken
	read("sess-1", s.repo, write("notes.txt", "text", 0))            // not an image
	read("sess-1", s.repo, write("wp-files/WP-1/a.png", png+"a", 0)) // someone's attachment, fetched
	got := files()
	if len(got) != 1 || got[0].Name != "shot.png" || got[0].Caption != autoShotCaption || got[0].Agent == "" {
		t.Fatalf("attachments = %+v", got)
	}
	// From a worktree (another directory) the session's own task is found.
	read("sess-1", t.TempDir(), write("after.png", png+"after", 0))
	// Another session in another directory has no task: nothing.
	read("sess-2", t.TempDir(), write("other.png", png+"other", 0))
	t.Setenv("WP_SHOTS", "off")
	read("sess-1", s.repo, write("off.png", png+"off", 0))
	if got := files(); len(got) != 2 || got[1].Name != "after.png" {
		t.Errorf("attachments = %+v", got)
	}
}
