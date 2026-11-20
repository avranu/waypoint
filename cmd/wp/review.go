package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Reviewing what agents wrote, and pausing an agent's clock (WP-D11).

var reviewCmd = &command{
	name: "review", group: "Knowledge", args: "[THING...]",
	summary: "list unreviewed agent work, or mark things reviewed",
	about: `Text an agent wrote and issues an agent closed stay marked unreviewed until
a person reviews them. No arguments lists them in the scope (wp help scope);
arguments mark them reviewed (people only):
  APP-12         the issue's text, agent comments and agent close
  close:APP-12   only the agent's close
  PL13, #7       a page
  comment:123    a comment
  term:45        a glossary entry

For a model's adversarial review of code, use wp audit.`,
	examples: []string{"wp review", "wp review APP-12 PL13", "wp review close:APP-12"},
	run:      cmdReview,
}

func cmdReview(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		q := url.Values{}
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		if !sc.all() {
			home, err := sc.home()
			if err != nil {
				return err
			}
			q.Set("project", home)
			c.note("Unreviewed in %s, from %s.", home, sc.from)
		}
		if !c.asJSON {
			q.Set("format", "text")
		}
		raw, err := c.call("GET", "/api/v1/review?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return string(raw) })
	}
	home := ""
	if sf.project != nil && *sf.project != "" {
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		home, _ = sc.home()
	} else {
		home = c.homeProject()
	}
	var lines []string
	for _, ref := range pos {
		targets := reviewTargets(ref)
		if len(targets) == 0 {
			return c.usage("%q isn't an issue key, close:KEY, page code or number, comment:ID or term:ID", ref)
		}
		var res struct {
			Reviewed int `json:"reviewed"`
		}
		var err error
		// APP12 or PL13 could be an issue or a page: the first that exists.
		for _, t := range targets {
			path := "/api/v1/review/" + t[0] + "/" + esc(t[1])
			if home != "" {
				path += "?project=" + url.QueryEscape(home)
			}
			var ae *apiError
			if err = c.do("POST", path, nil, &res); err == nil || !errors.As(err, &ae) || ae.status != 404 {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		if res.Reviewed == 0 {
			lines = append(lines, ref+": nothing an agent wrote or closed was waiting.")
		} else {
			lines = append(lines, fmt.Sprintf("%s: reviewed (%d).", ref, res.Reviewed))
		}
	}
	return c.emit([]byte(`{}`), func() string { return strings.Join(lines, "\n") })
}

// reviewTargets reads what wp review was given: the (kind, ref) pairs for
// the API it could mean, most likely first.
func reviewTargets(ref string) [][2]string {
	up := strings.ToUpper(strings.TrimSpace(ref))
	if k, v, ok := strings.Cut(strings.ToLower(ref), ":"); ok {
		if k == "close" && keyRE.MatchString(strings.ToUpper(v)) {
			return [][2]string{{"close", strings.ToUpper(v)}}
		}
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			switch k {
			case "comment":
				return [][2]string{{"comment", v}}
			case "term", "glossary":
				return [][2]string{{"glossary", v}}
			case "page":
				return [][2]string{{"page", v}}
			}
		}
		return nil
	}
	if _, err := pageID(ref); err == nil {
		return [][2]string{{"page", strings.TrimPrefix(ref, "#")}}
	}
	var out [][2]string
	if keyDashRE.MatchString(up) {
		return [][2]string{{"issue", up}} // APP-12: only an issue
	}
	// A bare code (PL13) is most likely a page; APP12 most likely an issue.
	bare := pageCodeRE.MatchString(up) && len(strings.TrimRight(up, "0123456789")) <= 2
	if bare && pageCodeRE.MatchString(up) {
		out = append(out, [2]string{"page", up})
	}
	if keyRE.MatchString(up) {
		out = append(out, [2]string{"issue", up})
	}
	if !bare && pageCodeRE.MatchString(up) {
		out = append(out, [2]string{"page", up})
	}
	return out
}

var pauseCmd = &command{
	name: "pause", group: "Hooks", args: "[KEY]",
	summary: "stop your clock on a task you took, keeping the task",
	about: `You are idle, waiting for your person; wp resume or wp next/done carries
on. The Claude Code hook does this for you. KEY defaults to the task taken
here.`,
	examples: []string{"wp pause"},
	run:      func(c *cli, args []string) error { return agentClock(c, "pause", args) },
}

var resumeCmd = &command{
	name: "resume", group: "Hooks", args: "[KEY]",
	summary:  "restart your clock on a paused task",
	about:    `Takes the task again if the claim ran out and nobody else has it.`,
	examples: []string{"wp resume APP-12"},
	run:      func(c *cli, args []string) error { return agentClock(c, "resume", args) },
}

func agentClock(c *cli, which string, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q", joinArgs(rest))
	}
	raw, err := c.clock(which, key)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

// clock pauses or resumes the agent's clock on a task, as the identity
// that claimed it.
func (c *cli) clock(which, key string) ([]byte, error) {
	return c.clockWith(which, key, map[string]any{})
}

// clockWith is clock with options in the body (only_own: WP-720).
func (c *cli) clockWith(which, key string, body map[string]any) ([]byte, error) {
	if c.agent == "" {
		if _, cl := c.state.claimByKey(key); cl != nil && cl.Identity != "" {
			c.agent = cl.Identity
		}
	}
	return c.call("POST", "/api/v1/agent/"+which+"/"+esc(key), body)
}
