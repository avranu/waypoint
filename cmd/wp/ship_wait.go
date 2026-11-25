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
	// Checks: the answer is to a wait for a pull request's checks. A server
	// without that wait never sets it (WP-1878).
	Checks bool `json:"checks"`
	Pull   *struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	} `json:"pull"`
}

// canWaitShip: the same conditions under which endTurnAfterShip may write a
// marker (an agent's ship, in a session with Waypoint's hooks), the wait
// wasn't switched off, and neither the held answer nor a notice with it asks
// anything of the agent: one that does (findings to mark, its own report) is
// the agent's to act on now, and nothing would come while it waited. A notice
// from a server that omits notice_needs_agent counts as asking (fail closed,
// as in shipNeedsAgent): an older server's ship doesn't wait.
func (c *cli) canWaitShip(so shipOpts, o *openedPR) bool {
	return !*so.noWait && !factAsks(o.HeldNeedsAgent) && (o.Notice == "" || !factAsks(o.NoticeNeedsAgent)) && !c.asJSON && c.agentHeader() != "" && c.thisSession() != "" && c.server() != "" && c.token() != ""
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
	err := longPoll(shipChunk, time.Second, time.Now().Add(bound), func(hold time.Duration, probe bool, limit time.Duration) (bool, error) {
		n, err := c.shipNewsOnce(task, hold, limit)
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
	var failed *pollBoundError
	switch {
	case errors.As(err, &failed):
		return quit(stopLost, failed.cause) // the last request failed with no room to ask again
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

// shipNewsOnce asks the server to hold up to hold for the task's news, taking
// at most limit.
func (c *cli) shipNewsOnce(task string, hold, limit time.Duration) (*shipNews, error) {
	c.longPoll = limit
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
	n, why, werr := c.waitShipNews(task, so.waitLeft(), announce)
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

// shipChecksOnce asks the server to hold up to hold for the outcome of the
// pull request's checks on head, taking at most limit.
func (c *cli) shipChecksOnce(task string, pull int64, head string, hold, limit time.Duration) (*shipNews, error) {
	c.longPoll = limit
	defer func() { c.longPoll = 0 }()
	raw, err := c.call("POST", "/api/v1/sessions/hook/ship-news", map[string]any{
		"session_id": c.thisSession(), "machine": machineName(), "task": task, "wait": int(hold.Seconds()), "pull_id": pull, "head": head})
	if err != nil {
		return nil, err
	}
	var n shipNews
	if err := jsonInto(raw, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// canWaitChecks: wp ship waits on the checks of the pull request it opened
// when it is the agent's ship in a session with Waypoint's hooks, the wait
// wasn't switched off, Waypoint babysits the pull request (a failure falls
// to a round otherwise) and nothing else asks the agent's turn (why).
func (c *cli) canWaitChecks(so shipOpts, o *openedPR, babysat bool, why string) bool {
	// shipWork has already returned for --json by the time it asks; the term
	// is kept so the predicate states the whole rule on its own.
	return babysat && why == "" && !*so.noWait && !c.asJSON && c.agentHeader() != "" && c.thisSession() != "" && c.server() != "" && c.token() != ""
}

// waitShipChecks is wp ship's wait, after it opened the pull request, for the
// outcome of the head's first checks (WP-1878). done: a failure was printed
// for the agent to fix (the ship is over, with no marker, as for review
// findings). Otherwise the ship goes on: the checks passed, something else
// has the pull request, the wait ended, or the server doesn't know this wait
// (it answers without Checks, and the ship stops at "opened" as it did).
func (c *cli) waitShipChecks(task string, pull int64, number int, head string, bound time.Duration) (done bool, err error) {
	announced := false
	var news *shipNews
	var still string
	err = longPoll(shipChunk, time.Second, time.Now().Add(bound), func(hold time.Duration, probe bool, limit time.Duration) (bool, error) {
		n, err := c.shipChecksOnce(task, pull, head, hold, limit)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusMethodNotAllowed) {
				return true, nil
			}
			return false, err
		}
		if !n.Checks {
			return true, nil
		}
		if n.Outcome == "waiting" {
			still = n.Text
			if probe {
				announced = true
				c.println(fmt.Sprintf("Waiting for PR %d's checks (a failure comes back here for you to fix; you are woken when this command exits).", number))
			}
			return false, nil
		}
		news = n
		return true, nil
	})
	lostTouch := func(cause error) {
		if announced {
			c.println(fmt.Sprintf("Lost touch with Waypoint while waiting for PR %d's checks (%v); Waypoint's babysitting takes any failure from here.", number, cause))
		}
	}
	var lost *pollBoundError
	switch {
	case errors.As(err, &lost):
		lostTouch(lost.cause)
		return false, nil
	case errors.Is(err, errPollBound):
		if announced {
			c.println(cmpStr(still, fmt.Sprintf("PR %d's checks are still running.", number)) + " Stopped waiting here; Waypoint's babysitting takes any failure from here.")
		}
		return false, nil
	case err != nil:
		lostTouch(err)
		return false, nil
	case news == nil:
		return false, nil
	}
	if news.Outcome == "checks_failed" {
		if _, err := fmt.Fprintln(c.out, news.Text); err != nil {
			return true, fmt.Errorf("printing the failed checks: %w", err)
		}
		c.ackShipNews(task, news.IDs)
		return true, nil
	}
	c.println(news.Text)
	return false, nil
}
