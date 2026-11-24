package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// shipWaitDefault bounds the wait. shipWaitMax is the longest it may run
// from the start of the wp ship process: it stays under shipBgFresh so a
// marker written at its end still counts for the background call. Claude Code
// moves the call to the background after the --check run and the push have
// already used time, so the bound is what is left of shipWaitMax (shipLeft).
const (
	shipWaitDefault = 90 * time.Minute
	shipWaitMax     = shipBgFresh - 5*time.Minute
)

// shipStart is when this wp process started.
var shipStart = time.Now()

// shipLeft is how much of shipWaitMax this process has left (replaced in tests).
var shipLeft = func() time.Duration { return shipWaitMax - time.Since(shipStart) }

// waitMaxText is the --wait-timeout help's ceiling.
func waitMaxText() string {
	return strings.TrimSuffix(shipWaitMax.String(), "0s")
}

// shipChunk is what one request asks the server to hold; tests shorten it.
var shipChunk = waitChunk

// shipNews is the server's answer to a wait.
type shipNews struct {
	Outcome string  `json:"outcome"` // findings, pull, ended, waiting, none
	Text    string  `json:"text"`
	IDs     []int64 `json:"ids"` // the messages of findings: waitHeldShip acks them once printed
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

// Why waitShipNews returned no news.
type waitStop int

const (
	stopQuiet waitStop = iota
	stopBound
	stopLost
)

// waitShipNews holds until the task's round has something for the agent, or
// bound has passed. nil: nothing to show, and why; the ship goes on as it did
// before the wait existed. announce runs once the server has said it is
// waiting.
func (c *cli) waitShipNews(task string, bound time.Duration, announce func()) (*shipNews, waitStop, error) {
	announced := false
	quit := func(why waitStop, err error) (*shipNews, waitStop, error) {
		if !announced {
			why = stopQuiet
		}
		return nil, why, err
	}
	var news *shipNews
	err := longPoll(shipChunk, time.Second, time.Now().Add(bound), func(hold time.Duration, probe bool) (bool, error) {
		n, err := c.shipNewsOnce(task, hold)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusMethodNotAllowed) {
				return true, nil
			}
			return false, err
		}
		switch n.Outcome {
		case "findings", "pull", "ended":
			news = n
		case "waiting":
			if probe {
				announced = true
				announce()
			}
			return false, nil
		}
		return true, nil
	})
	switch {
	case errors.Is(err, errPollBound):
		return quit(stopBound, nil)
	case err != nil:
		return quit(stopLost, err)
	}
	return news, stopQuiet, nil
}

// ackShipNews tells the server the findings were printed, so it delivers
// them and nothing types them again. A failure is ignored: the news then
// reaches the agent the usual way as well.
func (c *cli) ackShipNews(task string, ids []int64) {
	if len(ids) == 0 {
		return
	}
	c.longPoll = 10 * time.Second
	defer func() { c.longPoll = 0 }()
	for try := 0; ; try++ {
		_, err := c.call("POST", "/api/v1/sessions/hook/ship-news", map[string]any{
			"session_id": c.thisSession(), "machine": machineName(), "task": task, "ack": ids})
		if err == nil || !transientErr(err) || try >= 2 {
			return
		}
		pollSleep(waitRetry[try])
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
	return fmt.Sprintf("Waypoint shipped the pull request: PR %d (%s): %s", n.Pull.Number, n.Pull.State, n.Pull.URL)
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
	announce := func() {
		c.println(fmt.Sprintf("Pushed; waiting for %s's review (it can take a while; you are woken when this command exits).", cmpStr(task, "the task")))
	}
	n, why, werr := c.waitShipNews(task, min(*so.waitTimeout, shipLeft()), announce)
	switch {
	case n == nil:
		switch why {
		case stopBound:
			c.println("The review is still going; stopped waiting here, so what it finds comes to you as before.")
		case stopLost:
			c.println(fmt.Sprintf("Lost touch with Waypoint while waiting (%v); what the review finds comes to you as before.", werr))
		}
		return o, raw, false, nil
	case n.Outcome == "pull":
		c.println(cmpStr(n.Text, n.pullLine()))
		*f.review, *f.force = false, false
		o2, raw2, err := reopenPR(c, f)
		if err != nil {
			return nil, nil, true, err
		}
		return o2, raw2, false, nil
	}
	if notes := o.heldNotes(); notes != "" {
		c.println(notes)
	}
	if _, err := fmt.Fprintln(c.out, n.Text); err != nil {
		return nil, nil, true, fmt.Errorf("printing the review's findings: %w", err)
	}
	c.ackShipNews(task, n.IDs)
	return o, raw, true, nil
}
