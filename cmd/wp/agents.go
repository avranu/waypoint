package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The agent queue and its controls (WP-D15): what wp next will hand out
// and why, the agents seen at work, and pausing them.

var queueCmd = &command{
	name: "queue", group: "Tasks", args: "[FLAGS]",
	summary: "show what wp next will hand out, in order, and why",
	about: `Lists tasks in the order wp next hands them out, what agents hold, and
delegated work that waits and why. Without --agent: the queue for you
(inside an agent, that agent) or, for a person, every agent.`,
	examples: []string{"wp queue", "wp queue --agent claude", "wp queue -A", "wp queue -p APP --agent anthropic/claude-code"},
	run:      cmdQueue,
}

func cmdQueue(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	fs.Lookup("agent").Usage = "an `AGENT` identity (anthropic/claude-opus-5-5) or family (claude)"
	verbose := fs.Bool("verbose", false, "every task and the order rules, not only the first")
	short(fs, "v", "verbose")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	if a := c.agentHeader(); a != "" {
		q.Set("agent", a)
	}
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
		c.note("Queue in %s, from %s.", home, sc.from)
	}
	if *verbose {
		q.Set("verbose", "1")
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/agents/queue?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

var agentsCmd = &command{
	name: "agents", group: "Tasks", args: "[pause|unpause|release …]",
	summary: "list agents at work; pause, unpause, release",
	about: `Lists the agents that worked on or hold work you can see, your agent
tokens and your pauses. A pause stops agents (all, a family like claude, or
one model) taking new work with wp next; what they hold stays theirs. Only
people can pause.`,
	examples: []string{"wp agents", "wp agents pause claude -m \"reviewing\"", "wp agents release APP-12"},
	run:      cmdAgents,
}

func init() {
	agentsCmd.subs = []*command{
		{name: "pause", args: "[SPEC] [-m NOTE] [--sessions]", summary: "stop agents matching SPEC (default all) taking new work",
			examples: []string{"wp agents pause haiku -m \"too many failed reviews\""}, run: cmdAgentsPause},
		{name: "unpause", args: "[SPEC]", summary: "let agents matching SPEC take work again (default all)",
			run: cmdAgentsUnpause},
		{name: "release", args: "KEY...", summary: "take a task back from its agent, into the queue",
			run: cmdAgentsRelease},
	}
}

type agentPause struct {
	Spec      string    `json:"spec"`
	Note      string    `json:"note"`
	Sessions  bool      `json:"sessions"`
	CreatedAt time.Time `json:"created_at"`
}

func cmdAgents(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unknown subcommand %q", pos[0])
	}
	var known []struct {
		Identity string     `json:"identity"`
		Family   string     `json:"family"`
		Models   []string   `json:"models"`
		Runs     int        `json:"runs"`
		Holding  int        `json:"holding"`
		Token    bool       `json:"token"`
		LastSeen *time.Time `json:"last_seen"`
	}
	raw, err := c.call("GET", "/api/v1/agents", nil)
	if err != nil {
		return err
	}
	var pauses []agentPause
	rawP, err := c.call("GET", "/api/v1/agents/pauses", nil)
	if err != nil {
		return err
	}
	if c.asJSON {
		return c.emit([]byte(fmt.Sprintf(`{"agents":%s,"pauses":%s}`, raw, rawP)), nil)
	}
	if err := json.Unmarshal(raw, &known); err != nil {
		return err
	}
	if err := json.Unmarshal(rawP, &pauses); err != nil {
		return err
	}
	if len(known) == 0 {
		c.println("No agent has worked on anything you can see yet.")
	}
	for _, a := range known {
		var bits []string
		if a.Holding > 0 {
			bits = append(bits, fmt.Sprintf("working on %d", a.Holding))
		}
		if a.Runs > 0 {
			bits = append(bits, fmt.Sprintf("%d run%s", a.Runs, plural(a.Runs)))
		}
		if len(a.Models) > 0 {
			bits = append(bits, "models "+strings.Join(a.Models, ", "))
		}
		if a.Token {
			bits = append(bits, "your agent token")
		}
		if a.LastSeen != nil {
			bits = append(bits, "last "+a.LastSeen.Local().Format("Jan 2 15:04"))
		}
		c.println(fmt.Sprintf("%-36s %s", a.Identity, strings.Join(bits, " · ")))
	}
	for _, p := range pauses {
		c.println(pauseLine(p))
	}
	return nil
}

func pauseLine(p agentPause) string {
	what := "agents matching " + p.Spec
	if strings.EqualFold(p.Spec, "any") {
		what = "all your agents"
	}
	if p.Sessions {
		what += ", running sessions included"
	}
	s := "PAUSED: " + what + " since " + p.CreatedAt.Local().Format("Jan 2 15:04")
	if p.Note != "" {
		s += ": " + p.Note
	}
	return s
}

func cmdAgentsPause(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("message", "", "a `NOTE` telling agents why")
	short(fs, "m", "message")
	sessions := fs.Bool("sessions", false, "wind running sessions down too: finish the step, hand off, stop")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	spec := strings.Join(pos, " ")
	var p agentPause
	if err := c.do("POST", "/api/v1/agents/pauses", map[string]any{"spec": spec, "note": *note, "sessions": *sessions}, &p); err != nil {
		return err
	}
	if c.asJSON {
		return c.emitValue(p)
	}
	if p.Sessions {
		c.println(pauseLine(p) + ".\nWithin a minute of their next tool call, running sessions start no agents, finish their step, write a hand-off and stop; wp agents unpause " + p.Spec + " lets them work again.")
		return nil
	}
	c.println(pauseLine(p) + ".\nThey finish what they hold; wp agents unpause " + p.Spec + " lets them take work again.")
	return nil
}

func cmdAgentsUnpause(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	spec := strings.Join(pos, " ")
	var res struct {
		Resumed int `json:"resumed"`
	}
	if err := c.do("DELETE", "/api/v1/agents/pauses?"+url.Values{"spec": {spec}}.Encode(), nil, &res); err != nil {
		return err
	}
	switch {
	case c.asJSON:
		return c.emitValue(res)
	case res.Resumed == 0:
		c.println("Nothing was paused.")
	case spec == "":
		c.println(fmt.Sprintf("Ended %d pause%s: your agents take work again.", res.Resumed, plural(res.Resumed)))
	default:
		c.println("Agents matching " + spec + " take work again.")
	}
	return nil
}

// emitValue prints a value as --json shows an answer.
func (c *cli) emitValue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.emit(b, nil)
}

func cmdAgentsRelease(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	keys := listArgs(pos)
	if len(keys) == 0 {
		return c.usage("expected a task KEY, like APP-12")
	}
	return c.each(keys, false, func(k string) error {
		key := normalKey(k)
		if !keyRE.MatchString(key) {
			return c.usage("%q isn't a task KEY, like APP-12", k)
		}
		if _, err := c.call("POST", "/api/v1/agents/release/"+esc(key), nil); err != nil {
			return err
		}
		c.println(key + " taken back: the agent's run ended and it waits in the queue again.")
		return nil
	})
}
