package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Session pause (WP-146): a person paused their agents with running
// sessions included (the quota alert's Pause, or Agents → Sessions). An
// async hook pulls the pause to a local file, as does every reply to the
// session hooks; the PreToolUse hooks read only that file (no network):
// the gate refuses new agents, the nudge tells the session once to finish
// its step, note what is left and stop.

const (
	pauseFile      = "paused.json"
	pausePullEvery = 30 * time.Second // the pull hook asks the server at most this often
	pauseFresh     = 30 * time.Minute // a copy older than this is ignored: fail open
)

// sessionPause is the server's AgentPause, as far as the hooks need it.
type sessionPause struct {
	ID   int64  `json:"id"`
	Spec string `json:"spec"`
	Note string `json:"note,omitempty"`
	By   *struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	} `json:"by"`
	CreatedAt time.Time `json:"created_at"`
}

// pauseCopy is the local file.
type pauseCopy struct {
	Pause   *sessionPause        `json:"pause"`
	Checked time.Time            `json:"checked"`        // the server last answered
	Tried   time.Time            `json:"tried"`          // the pull last asked
	Told    map[string]time.Time `json:"told,omitempty"` // session → when it was told of this pause
	// LateTold: session → late runs that told without marking (WP-1398).
	LateTold map[string]int `json:"late_told,omitempty"`
	Guard    *guardCopy     `json:"guard,omitempty"` // the live quota guard (WP-932)
}

func (c *cli) pausePath() string {
	if c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, pauseFile)
}

func (c *cli) readPause() pauseCopy {
	var p pauseCopy
	if path := c.pausePath(); path != "" {
		_ = readJSON(path, &p)
	}
	return p
}

func (c *cli) writePause(p pauseCopy) {
	if path := c.pausePath(); path != "" {
		_ = writeJSON(path, p)
	}
}

// savePause stores what the server said: raw is a pause or null. A
// different pause (or none) forgets which sessions were told.
func (c *cli) savePause(raw json.RawMessage) {
	if len(raw) == 0 {
		return // a server that predates session pauses: leave the copy alone
	}
	var p *sessionPause
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if path := c.pausePath(); path != "" {
		if unlock, err := lockFile(path + ".lock"); err == nil {
			defer unlock()
		}
	}
	cur := c.readPause()
	now := time.Now()
	if p == nil || cur.Pause == nil || cur.Pause.ID != p.ID {
		cur.Told, cur.LateTold = nil, nil
	}
	cur.Pause, cur.Checked, cur.Tried = p, now, now
	c.writePause(cur)
}

// activePause is the pause the local copy holds, if it is fresh.
func (c *cli) activePause() *sessionPause {
	p := c.readPause()
	if p.Pause == nil || time.Since(p.Checked) > pauseFresh {
		return nil
	}
	return p.Pause
}

// claimPull marks this round as taken, under the copy's lock so a concurrent
// savePause or pauseTell is not overwritten by this read. The lock is
// released before the HTTP call (savePause takes it again).
func (c *cli) claimPull() bool {
	if path := c.pausePath(); path != "" {
		unlock, err := lockFile(path + ".lock")
		if err != nil {
			return false // skip this round; a later hook pulls
		}
		defer unlock()
	}
	cur := c.readPause()
	if time.Since(cur.Tried) < pausePullEvery {
		return false
	}
	cur.Tried = time.Now()
	c.writePause(cur) // claim this round before asking, so concurrent hooks don't pile on
	return true
}

// pullPause asks the server for the pause, at most every pausePullEvery
// (now: at once, for a stop the guard made); the pulse carries the
// machine's sessions' spend and the guard's trips (WP-932).
func (c *cli) pullPause(now bool) {
	if c.server() == "" || c.token() == "" {
		return
	}
	if !c.claimPull() && !now {
		return
	}
	c.hookTimeout = hookHTTPTimeout
	if c.pulse() {
		return
	}
	b, err := c.call("GET", "/api/v1/sessions/pause", nil)
	if err != nil {
		return
	}
	var r struct {
		Pause json.RawMessage `json:"pause"`
	}
	if json.Unmarshal(b, &r) != nil || len(r.Pause) == 0 {
		return
	}
	c.savePause(r.Pause)
}

// hookClaudePause is the async PostToolUse hook: it keeps the copy current
// and reports the sessions' spend. now: the guard just stopped a session.
func hookClaudePause(c *cli, stdin io.Reader, now bool) {
	if !now {
		_, _ = io.Copy(io.Discard, io.LimitReader(stdin, 4<<20))
	}
	c.pullPause(now)
}

// pauseWho is who paused, for the agent.
func pauseWho(p *sessionPause) string {
	if p.By != nil {
		if n := strings.TrimSpace(p.By.DisplayName); n != "" {
			return n
		}
		if p.By.Username != "" {
			return p.By.Username
		}
	}
	return "your person"
}

// pauseHead is the first half of what the agent is told: who and why.
func pauseHead(p *sessionPause) string {
	s := fmt.Sprintf("Paused: %s paused agents, running sessions included, at %s", pauseWho(p), p.CreatedAt.Local().Format("15:04"))
	if p.Note != "" {
		s += " (" + strings.TrimSuffix(p.Note, ".") + ")"
	}
	return s + "."
}

// pauseText is what a session is told while paused.
func pauseText(p *sessionPause) string {
	return pauseHead(p) + " Start no agents and take no new work: finish the step you're on, note what is left (`wp comment KEY \"…\"`, else in your final message), then stop."
}

// pauseGateText is the gate's refusal of an Agent call while paused.
func pauseGateText(p *sessionPause) string {
	return pauseHead(p) + " No agents start until it is lifted."
}

// pauseTell is the pause's text for this session, once per pause: at its
// start or its first tool call after the pause reached it. A hook under a
// short deadline passes lateAfter: past it the run is presumably cancelled, so
// it tells without marking the pause told, up to lateTellsMax times, then
// marks (0: never late). Its lock wait is bounded by the time left before
// lateAfter; without the lock it tells nothing (WP-1398).
func (c *cli) pauseTell(session string, lateAfter time.Duration) string {
	if session == "" {
		return ""
	}
	// Nothing to tell is the common case (no pause, a stale one, or this
	// session already told): decide that without the lock, which can wait
	// seconds behind a pull (WP-1398).
	if c.pauseTold(c.readPause(), session) {
		return ""
	}
	// Lock the copy: a concurrent pull must not be overwritten by this stale read.
	if path := c.pausePath(); path != "" {
		wait := 5 * time.Second
		if lateAfter > 0 {
			wait = min(wait, hookLockWait(lateAfter))
		}
		if unlock, err := lockFileWithin(path+".lock", wait); err == nil {
			defer unlock()
		} else if lateAfter > 0 {
			return ""
		}
	}
	cur := c.readPause()
	if c.pauseTold(cur, session) {
		return ""
	}
	now := time.Now()
	if lateAfter > 0 && hookLate(lateAfter) {
		if n := cur.LateTold[session] + 1; n < lateTellsMax {
			if cur.LateTold == nil {
				cur.LateTold = map[string]int{}
			}
			cur.LateTold[session] = n
			c.writePause(cur)
			return pauseText(cur.Pause)
		}
	}
	delete(cur.LateTold, session)
	if cur.Told == nil {
		cur.Told = map[string]time.Time{}
	}
	for sid, t := range cur.Told { // sessions long gone
		if now.Sub(t) > 24*time.Hour {
			delete(cur.Told, sid)
		}
	}
	cur.Told[session] = now
	c.writePause(cur)
	return pauseText(cur.Pause)
}

// pauseTold reports that there is nothing to tell session: no live pause, or
// it already has been told.
func (c *cli) pauseTold(cur pauseCopy, session string) bool {
	if cur.Pause == nil || time.Since(cur.Checked) > pauseFresh {
		return true
	}
	_, ok := cur.Told[session]
	return ok
}

// joinParts joins the non-empty parts with a blank line.
func joinParts(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}
