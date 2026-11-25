package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// maxAutoShots per task: an issue holds 100 files, so people's own
	// attachments keep room.
	maxAutoShots     = 40
	maxAutoShotBytes = 10 << 20
	shotHTTPTimeout  = 20 * time.Second
	autoShotCaption  = "Seen by the agent while working (saved automatically)"
)

var shotExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// keepScreenshot attaches the image a Read looked at to the session's task,
// once per content: when it is a file made since the task was taken (not a
// picture in the repo) and not someone's attachment fetched with wp files.
func (c *cli) keepScreenshot(in hookEventInput) {
	if strings.EqualFold(os.Getenv("WP_SHOTS"), "off") {
		return
	}
	var ti struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || !filepath.IsAbs(ti.FilePath) || !shotExts[strings.ToLower(filepath.Ext(ti.FilePath))] {
		return
	}
	if wpPictureDir(ti.FilePath) {
		return
	}
	cl := c.state.sessionClaim(in.Cwd, in.SessionID)
	if cl == nil {
		return
	}
	st, err := os.Stat(ti.FilePath)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxAutoShotBytes || st.ModTime().Before(cl.ClaimedAt) {
		return
	}
	data, err := os.ReadFile(ti.FilePath) // #nosec G304 -- the image the agent just read
	if err != nil || len(data) == 0 {
		return
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:8])
	key := cl.Key
	reserved := false
	_ = c.updateState(func(s *State) {
		cur := s.sessionClaim(in.Cwd, in.SessionID)
		if cur == nil || cur.Key != key || len(cur.Shots) >= maxAutoShots || slices.Contains(cur.Shots, id) {
			return
		}
		cur.Shots = append(cur.Shots, id)
		reserved = true
	})
	if !reserved {
		return
	}
	if c.agent == "" && cl.Identity != "" {
		c.agent = cl.Identity
	}
	c.session = in.SessionID
	c.hookTimeout = shotHTTPTimeout
	q := url.Values{"target": {"issue:" + key}, "name": {filepath.Base(ti.FilePath)}, "caption": {autoShotCaption}}
	_, err = c.callRaw("POST", "/api/v1/attachments?"+q.Encode(), "application/octet-stream", bytes.NewReader(data))
	if err != nil && !serverRefused(err) {
		_ = c.updateState(func(s *State) {
			if cur := s.sessionClaim(in.Cwd, in.SessionID); cur != nil {
				cur.Shots = slices.DeleteFunc(cur.Shots, func(s string) bool { return s == id })
			}
		})
	}
}

// sessionClaim is the task a hook's session works on: the one taken in its
// directory, else the only one the session took elsewhere (a worktree).
func (s *State) sessionClaim(cwd, session string) *claim {
	if filepath.IsAbs(cwd) {
		if _, cl := s.claimFor(cwd); cl != nil {
			return cl
		}
	}
	if session == "" {
		return nil
	}
	var found *claim
	for _, cl := range s.Claims {
		if cl.Session == session {
			if found != nil {
				return nil
			}
			found = cl
		}
	}
	return found
}

// wpPictureDir: p is in a directory named as wp names its pictures' ones:
// wp shot's (wp-shots, also under --out) or wp files' downloads (wp-files).
// Only for not attaching them again: by name alone it says nothing about
// whose they are, so the unasked Read (pictureReadAllowed) doesn't use it.
func wpPictureDir(p string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Dir(p)), "/")
	return slices.Contains(parts, filesDirName) || slices.Contains(parts, shotsDirName)
}
