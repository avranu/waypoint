package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// A wp ship held for its review round waits on the server for the round's next
// step and prints it, instead of exiting so that every step costs the agent a
// turn end, a reply and a typed prompt (WP-1669). Run in the background (Claude
// Code moves a long command there itself), the agent is woken when it exits.

// shipWaitDefault bounds the wait. It stays under shipBgFresh so a marker
// written at its end still counts for the background call.
const (
	shipWaitDefault = 90 * time.Minute
	shipWaitMax     = shipBgFresh - 5*time.Minute
)

// shipChunk is what one request asks the server to hold; tests shorten it.
var shipChunk = waitChunk

// shipSleep waits between retries (replaced in tests).
var shipSleep = time.Sleep

// shipNews is the server's answer to a wait.
type shipNews struct {
	Outcome string `json:"outcome"` // findings, pull, ended, waiting, none
	Text    string `json:"text"`
	Pull    *struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	} `json:"pull"`
}

// canWaitShip: the same conditions under which endTurnAfterShip may write a
// marker (an agent's ship, in a session with Waypoint's hooks), the wait
// wasn't switched off, and the held answer asks nothing of the agent: one
// that does (findings to mark, its own report) is the agent's to act on now,
// and nothing would come while it waited.
func (c *cli) canWaitShip(so shipOpts, o *openedPR) bool {
	return !*so.noWait && !factAsks(o.HeldNeedsAgent) && !c.asJSON && c.agentHeader() != "" && c.thisSession() != "" && c.server() != "" && c.token() != ""
}

// waitShipNews holds until the task's round has something for the agent, or
// bound has passed. nil: nothing to show (an older server, an error, the
// bound), and the ship goes on as it did before the wait existed.
func (c *cli) waitShipNews(task string, bound time.Duration, announce func()) *shipNews {
	deadline := time.Now().Add(bound)
	chunk, failures := shipChunk, 0
	probe := true // the first request holds nothing: it says whether the server knows the wait
	for {
		hold := min(chunk, time.Until(deadline))
		if probe {
			hold = 0
		} else if hold < time.Second {
			return nil
		}
		n, err := c.shipNewsOnce(task, hold)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusMethodNotAllowed) {
				return nil // an older server
			}
			if !transientErr(err) || failures >= len(waitRetry) {
				return nil
			}
			if hold > 0 {
				chunk = min(chunk, waitShortChunk) // a proxy may have cut it short: ask for less
			}
			shipSleep(waitRetry[failures])
			failures++
			continue
		}
		failures = 0
		if probe {
			probe = false
			if n.Outcome != "none" {
				announce()
			}
		}
		switch n.Outcome {
		case "findings", "pull", "ended":
			return n
		case "none":
			return nil
		}
	}
}

func (c *cli) shipNewsOnce(task string, hold time.Duration) (*shipNews, error) {
	c.longPoll = hold + 30*time.Second
	defer func() { c.longPoll = 0 }()
	raw, err := c.call("POST", "/api/v1/sessions/hook/ship-news", map[string]any{
		"session_id": c.thisSession(), "machine": machineName(), "task": task, "wait": int(hold.Seconds())})
	if err != nil {
		return nil, err
	}
	var n shipNews
	if err := jsonInto(raw, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func (n *shipNews) pullLine() string {
	if n.Pull == nil {
		return ""
	}
	return fmt.Sprintf("Waypoint opened the pull request: PR %d (%s): %s", n.Pull.Number, n.Pull.State, n.Pull.URL)
}

// reopenPR finds the pull request Waypoint opened (replaced in tests).
var reopenPR = func(c *cli, f prOpenFlags) (*openedPR, []byte, error) { return c.openPR(f) }

// waitHeldShip is wp ship's wait for a held round's next step. done: what
// the round had for the agent was printed and the ship is over (no marker, so
// the turn goes on to read it; the summary stays kept for the task's next
// ship). Otherwise the ship goes on with o: the pull request Waypoint opened,
// or the held answer as it was when nothing came.
func (c *cli) waitHeldShip(f prOpenFlags, so shipOpts, o *openedPR, raw []byte, task, summary string) (*openedPR, []byte, bool, error) {
	if summary != "" {
		c.keepSummary(task, summary)
	}
	announced := false
	n := c.waitShipNews(task, min(*so.waitTimeout, shipWaitMax), func() {
		announced = true
		c.println(fmt.Sprintf("Pushed; waiting for %s's review (it can take a while; you are woken when this command exits).", cmpStr(task, "the task")))
	})
	switch {
	case n == nil:
		if announced {
			c.println("The review is still going; stopped waiting here, so what it finds comes to you as before.")
		}
		return o, raw, false, nil
	case n.Outcome == "pull":
		c.println(n.pullLine())
		*f.review, *f.force = false, false // the round ran: this only finds its pull request
		o2, raw2, err := reopenPR(c, f)
		if err != nil {
			return nil, nil, true, err
		}
		if o2.Held == "" {
			return o2, raw2, false, nil
		}
		return o, raw, false, nil
	}
	c.println(n.Text)
	return o, raw, true, nil
}
