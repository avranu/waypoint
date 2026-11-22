package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Agent instructions (WP-1559, ADR146): text Waypoint hands a session or a
// sub-agent when it starts, chosen by what it is. `wp instruction` is how a
// person or an agent reads and changes them; the hooks deliver them.

var instructionCmd = &command{
	name: "instruction", group: "Knowledge", args: "SUBCOMMAND",
	aliases: []string{"instructions", "instr"},
	summary: "the instructions sessions and sub-agents are given when they start",
	about: `An instruction is text for the agents it names: the main thread, sub-agents
or both (--scope), on some models (--model, part of the model's id), of some
sub-agent types (--agent-type, globs; !NAME leaves one out), at the start or
only after a compaction (--moment). Waypoint's hooks hand a session exactly
the ones that fit it, a sub-agent's before its first prompt, and the main
thread's again after a compaction. Nothing is written to the repo and a
change applies to the next session or sub-agent that starts.

A person's change applies at once. An agent's waits for a person
(wp instruction approve); until then the approved text is what is sent.
An instruction belongs to one project or, with -p global, to every project
(admins).`,
	examples: []string{
		`wp instruction add "Fan out reading" --scope main --model fable --model opus -m "Hand reading to haiku-worker agents; keep decisions here."`,
		`wp instruction add "Report format" --scope subagent --agent-type '*-worker' -f report.md`,
		"wp instruction for --scope subagent --agent-type haiku-worker --model claude-haiku-5-5",
		"wp instruction ls",
	},
}

func init() {
	instructionCmd.subs = []*command{instrLsCmd, instrShowCmd, instrAddCmd, instrEditCmd, instrForCmd,
		instrOnCmd, instrOffCmd, instrApproveCmd, instrRejectCmd, instrRmCmd, instrDeliveryCmd}
	instructionCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return instrLs(c, args)
		}
		return c.usage("no instruction command %q", args[0])
	}
}

type instrRevInfo struct {
	Rev        int      `json:"rev"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Scope      string   `json:"scope"`
	Models     []string `json:"models"`
	AgentTypes []string `json:"agent_types"`
	Moment     string   `json:"moment"`
	State      string   `json:"state"`
	By         string   `json:"by"`
	Agent      string   `json:"agent"`
}

type instrInfo struct {
	ID         int64          `json:"id"`
	Project    string         `json:"project"`
	Rev        int            `json:"rev"`
	Enabled    bool           `json:"enabled"`
	Position   int            `json:"position"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Scope      string         `json:"scope"`
	Models     []string       `json:"models"`
	AgentTypes []string       `json:"agent_types"`
	Moment     string         `json:"moment"`
	Pending    *instrRevInfo  `json:"pending"`
	Revisions  []instrRevInfo `json:"revisions"`
	Sent       *struct {
		Sent         int `json:"sent"`
		Cut          int `json:"cut"`
		UnknownModel int `json:"unknown_model"`
		Sessions     int `json:"sessions"`
		Subagents    int `json:"subagents"`
		Compactions  int `json:"compactions"`
	} `json:"sent"`
}

// instrAudience says who an instruction is for in a few words.
func instrAudience(scope string, models, types []string, moment string) string {
	who := map[string]string{"main": "main thread", "subagent": "sub-agents", "any": "everyone"}[scope]
	if who == "" {
		who = scope
	}
	if len(types) > 0 {
		who += " of type " + strings.Join(types, ", ")
	}
	if len(models) > 0 {
		who += " on " + strings.Join(models, ", ")
	}
	if moment == "compact" {
		who += ", after a compaction only"
	}
	return who
}

func (i *instrInfo) line() string {
	where := i.Project
	if where == "" {
		where = "global"
	}
	title, state := i.Title, ""
	switch {
	case i.Rev == 0 && i.Pending != nil:
		// Nothing approved yet: show what is proposed.
		title, state = i.Pending.Title, " [waits for a person: nothing sent yet]"
		return fmt.Sprintf("#%d %s (%s)%s  %s", i.ID, title, where, state,
			instrAudience(i.Pending.Scope, i.Pending.Models, i.Pending.AgentTypes, i.Pending.Moment))
	case i.Pending != nil:
		state = fmt.Sprintf(" [rev %d waits for a person]", i.Pending.Rev)
	}
	if !i.Enabled {
		state += " [off]"
	}
	return fmt.Sprintf("#%d %s (%s)%s  %s", i.ID, title, where, state, instrAudience(i.Scope, i.Models, i.AgentTypes, i.Moment))
}

// instrScope is the project an instruction command is about: a key, "" for
// every project (-p global), and whether one was found at all.
func (c *cli) instrScope(sf scopeFlags) (string, bool, error) {
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return "", false, err
	}
	if project == "global" {
		return "", true, nil
	}
	return project, project != "", nil
}

var instrLsCmd = &command{
	name: "ls", args: "[FLAGS]", aliases: []string{"list"},
	summary:  "the instructions that reach this project",
	about:    `The project's own and the global ones. -p global lists only the global ones.`,
	examples: []string{"wp instruction ls", "wp instruction ls -p global"},
	run:      instrLs,
}

func instrLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	project, _, err := c.instrScope(sf)
	if err != nil {
		return err
	}
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	raw, err := c.call("GET", "/api/v1/instructions?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var list []instrInfo
		_ = jsonInto(raw, &list)
		if len(list) == 0 {
			return `No agent instructions. Add one: wp instruction add "TITLE" -m "TEXT" --scope main|subagent|any`
		}
		var b strings.Builder
		for _, i := range list {
			b.WriteString(i.line() + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func instrID(pos []string) (int64, error) {
	if len(pos) == 0 {
		return 0, fmt.Errorf("which instruction? Give its number (wp instruction ls)")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not an instruction's number (wp instruction ls)", pos[0])
	}
	return id, nil
}

var instrShowCmd = &command{
	name: "show", args: "ID", aliases: []string{"view", "get"},
	summary:  "an instruction: its text, who gets it, its changes and how often it was sent",
	examples: []string{"wp instruction show 4"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		id, err := instrID(pos)
		if err != nil {
			return c.usage("%s", err)
		}
		raw, err := c.call("GET", fmt.Sprintf("/api/v1/instructions/%d", id), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var i instrInfo
			_ = jsonInto(raw, &i)
			var b strings.Builder
			b.WriteString(i.line() + "\n")
			if i.Rev > 0 {
				fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(i.Body))
			}
			if p := i.Pending; p != nil {
				by := p.By
				if p.Agent != "" {
					by = joinNonEmpty(" via ", p.By, p.Agent)
				}
				fmt.Fprintf(&b, "\nWaiting for a person (rev %d, by %s): %s\n  for %s\n\n%s\n\nApprove: wp instruction approve %d · Reject: wp instruction reject %d\n",
					p.Rev, by, p.Title, instrAudience(p.Scope, p.Models, p.AgentTypes, p.Moment), strings.TrimSpace(p.Body), i.ID, i.ID)
			}
			if s := i.Sent; s != nil {
				fmt.Fprintf(&b, "\nSent %d time%s: %d session%s, %d sub-agent%s, %d after a compaction.",
					s.Sent, plural(s.Sent), s.Sessions, plural(s.Sessions), s.Subagents, plural(s.Subagents), s.Compactions)
				if s.Cut > 0 {
					fmt.Fprintf(&b, " Left out %d time%s: no room.", s.Cut, plural(s.Cut))
				}
				if s.UnknownModel > 0 {
					fmt.Fprintf(&b, " Not sent %d time%s: the agent's model wasn't known.", s.UnknownModel, plural(s.UnknownModel))
				}
				b.WriteString("\n")
			}
			if len(i.Revisions) > 1 {
				b.WriteString("\nChanges:\n")
				for _, r := range i.Revisions {
					fmt.Fprintf(&b, "  rev %d  %s  %s\n", r.Rev, r.State, joinNonEmpty(" via ", r.By, r.Agent))
				}
			}
			return strings.TrimRight(b.String(), "\n")
		})
	},
}

func joinNonEmpty(sep string, parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

// instrFlags are the flags add and edit share.
type instrFlags struct {
	body, file, scope, moment *string
	models, types             listFlag
	position                  *int
}

func addInstrFlags(fs *flag.FlagSet) *instrFlags {
	f := &instrFlags{}
	f.body = fs.String("message", "", "the instruction's `TEXT`")
	short(fs, "m", "message")
	f.file = fs.String("file", "", "read the text from `FILE` (- for stdin)")
	short(fs, "f", "file")
	f.scope = fs.String("scope", "", "who gets it: `main`, subagent or any")
	f.moment = fs.String("moment", "", "`start` (and again after a compaction) or compact (only after one)")
	fs.Var(&f.models, "model", "only on models whose id contains `TEXT` (repeatable; \"\" for every model)")
	fs.Var(&f.types, "agent-type", "only sub-agents of this `TYPE` (glob; !TYPE leaves one out; repeatable; \"\" for all)")
	f.position = fs.Int("position", -1, "its place among the project's instructions (`N`, lower first; people only)")
	return f
}

// fill puts the given flags into a request body.
func (f *instrFlags) fill(in map[string]any) error {
	if *f.file != "" {
		body, err := readBody(*f.file)
		if err != nil {
			return err
		}
		in["body"] = body
	} else if *f.body != "" {
		in["body"] = *f.body
	}
	if *f.scope != "" {
		in["scope"] = *f.scope
	}
	if *f.moment != "" {
		in["moment"] = *f.moment
	}
	for key, given := range map[string]listFlag{"models": f.models, "agent_types": f.types} {
		if len(given) > 0 {
			list := []string{}
			for _, v := range given {
				if v != "" {
					list = append(list, v)
				}
			}
			in[key] = list
		}
	}
	if *f.position >= 0 {
		in["position"] = *f.position
	}
	return nil
}

// instrChanged says what a create or change did: applied, or waiting.
func instrChanged(verb string, raw []byte) string {
	var i instrInfo
	_ = jsonInto(raw, &i)
	out := verb + " " + i.line()
	if i.Pending != nil {
		out += fmt.Sprintf("\nA person approves it before it is sent: wp instruction approve %d", i.ID)
	}
	return out
}

var instrAddCmd = &command{
	name: "add", args: `"TITLE" [FLAGS]`, aliases: []string{"new", "create"},
	summary: "add an instruction",
	about: `The title is for people; agents are sent only the text. Without --scope it
is for everyone (any).`,
	examples: []string{
		`wp instruction add "Fan out reading" --scope main --model fable -m "Hand reading to haiku-worker agents."`,
		`wp instruction add "After a compaction" --scope main --moment compact -m "Re-read the task with wp next before going on."`,
	},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		f := addInstrFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("give the title")
		}
		project, found, err := c.instrScope(sf)
		if err != nil {
			return err
		}
		if !found {
			return c.usage("which project? Use -p PROJECT or -p global")
		}
		in := map[string]any{"title": joinArgs(pos), "project": project}
		if err := f.fill(in); err != nil {
			return err
		}
		if _, ok := in["body"]; !ok {
			return c.usage("give the text: -m TEXT or -f FILE")
		}
		raw, err := c.call("POST", "/api/v1/instructions", in)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return instrChanged("Added", raw) })
	},
}

var instrEditCmd = &command{
	name: "edit", args: "ID [FLAGS]", aliases: []string{"update", "set"},
	summary: "change an instruction's text or who gets it",
	about: `Only what you give changes. --model and --agent-type replace the whole
list; "" clears it.`,
	examples: []string{"wp instruction edit 4 -f text.md", `wp instruction edit 4 --model ""`},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		f := addInstrFlags(fs)
		title := fs.String("title", "", "rename it to `TITLE`")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		id, err := instrID(pos)
		if err != nil {
			return c.usage("%s", err)
		}
		in := map[string]any{}
		if *title != "" {
			in["title"] = *title
		}
		if err := f.fill(in); err != nil {
			return err
		}
		if len(in) == 0 {
			return c.usage("nothing to change: give -m, -f, --title, --scope, --model, --agent-type, --moment or --position")
		}
		raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/instructions/%d", id), in)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return instrChanged("Changed", raw) })
	},
}

func instrSwitch(on bool) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		id, err := instrID(pos)
		if err != nil {
			return c.usage("%s", err)
		}
		raw, err := c.call("PATCH", fmt.Sprintf("/api/v1/instructions/%d", id), map[string]any{"enabled": on})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			if on {
				return instrChanged("On:", raw)
			}
			return instrChanged("Off (kept, sent to nobody):", raw)
		})
	}
}

var instrOnCmd = &command{
	name: "on", args: "ID", aliases: []string{"enable"},
	summary: "send an instruction again (people only)",
	run:     instrSwitch(true),
}

var instrOffCmd = &command{
	name: "off", args: "ID", aliases: []string{"disable"},
	summary: "stop sending an instruction without deleting it (people only)",
	run:     instrSwitch(false),
}

func instrReview(approve bool) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		fs := c.flags()
		rev := fs.Int("rev", 0, "the revision `N` (default: the one waiting)")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		id, err := instrID(pos)
		if err != nil {
			return c.usage("%s", err)
		}
		raw, err := c.call("POST", fmt.Sprintf("/api/v1/instructions/%d/review", id), map[string]any{"rev": *rev, "approve": approve})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			if approve {
				return instrChanged("Approved:", raw)
			}
			return instrChanged("Rejected the change to", raw)
		})
	}
}

var instrApproveCmd = &command{
	name: "approve", args: "ID [--rev N]",
	summary: "approve an agent's change, so it is what is sent (people only)",
	run:     instrReview(true),
}

var instrRejectCmd = &command{
	name: "reject", args: "ID [--rev N]",
	summary: "turn an agent's change down (people only)",
	run:     instrReview(false),
}

var instrRmCmd = &command{
	name: "rm", args: "ID", aliases: []string{"delete", "remove"},
	summary: "delete an instruction and its history (people only; off keeps it)",
	run: func(c *cli, args []string) error {
		fs := c.flags()
		yes := fs.Bool("yes", false, "don't ask")
		short(fs, "y", "yes")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		id, err := instrID(pos)
		if err != nil {
			return c.usage("%s", err)
		}
		if err := c.confirm(*yes, fmt.Sprintf("Delete instruction #%d and the record of what it sent for good?", id)); err != nil {
			return err
		}
		raw, err := c.call("DELETE", fmt.Sprintf("/api/v1/instructions/%d", id), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return fmt.Sprintf("Deleted instruction #%d.", id) })
	},
}

var instrForCmd = &command{
	name: "for", args: "[FLAGS]", aliases: []string{"preview"},
	summary: "what a session or sub-agent like this would be sent, exactly",
	about: `Prints the text the hook would hand it, then each instruction that was
left out and why. Nothing is recorded as sent.`,
	examples: []string{
		"wp instruction for --model claude-fable-5-2",
		"wp instruction for --scope subagent --agent-type haiku-worker --model claude-haiku-5-5",
		"wp instruction for --compact",
	},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		scope := fs.String("scope", "main", "`main` or subagent")
		model := fs.String("model", "", "its model `ID` (none: a model nobody could tell)")
		typ := fs.String("agent-type", "", "a sub-agent's `TYPE`")
		compact := fs.Bool("compact", false, "just after a compaction")
		if _, err := c.parse(fs, args); err != nil {
			return err
		}
		project, found, err := c.instrScope(sf)
		if err != nil {
			return err
		}
		if !found {
			return c.usage("which project? Use -p PROJECT")
		}
		if *typ != "" && *scope == "main" {
			*scope = "subagent"
		}
		q := url.Values{"scope": {*scope}, "model": {*model}, "agent_type": {*typ}}
		if project != "" {
			q.Set("project", project)
		}
		if *compact {
			q.Set("compact", "1")
		}
		raw, err := c.call("GET", "/api/v1/instructions/preview?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var p struct {
				Text  string `json:"text"`
				Chars int    `json:"chars"`
				Off   bool   `json:"off"`
				Items []struct {
					ID      int64  `json:"id"`
					Title   string `json:"title"`
					Outcome string `json:"outcome"`
					Chars   int    `json:"chars"`
				} `json:"items"`
			}
			_ = jsonInto(raw, &p)
			if p.Off {
				return "This project has agent instructions turned off: nothing is sent. (wp instruction delivery on)"
			}
			var b strings.Builder
			if p.Text == "" {
				b.WriteString("Nothing would be sent.\n")
			} else {
				fmt.Fprintf(&b, "%s\n\n(%d characters)\n", p.Text, p.Chars)
			}
			for _, it := range p.Items {
				switch it.Outcome {
				case "cut":
					fmt.Fprintf(&b, "Left out, no room: #%d %s (%d characters)\n", it.ID, it.Title, it.Chars)
				case "unknown_model":
					fmt.Fprintf(&b, "Not sent, it names models and this one isn't known: #%d %s\n", it.ID, it.Title)
				}
			}
			return strings.TrimRight(b.String(), "\n")
		})
	},
}

var instrDeliveryCmd = &command{
	name: "delivery", args: "on|off [-p PROJECT]",
	summary: "turn agent instructions on or off for a whole project",
	about: `Off: sessions in the project's repos are sent no instructions at all, its
own or global, until it is turned on again. The instructions are kept.`,
	examples: []string{"wp instruction delivery off", "wp instruction delivery on -p WP"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 || (pos[0] != "on" && pos[0] != "off") {
			return c.usage("on or off")
		}
		project, found, err := c.instrScope(sf)
		if err != nil {
			return err
		}
		if !found || project == "" {
			return c.usage("which project? Use -p PROJECT (one global instruction: wp instruction off ID)")
		}
		off := pos[0] == "off"
		raw, err := c.call("PATCH", "/api/v1/projects/"+url.PathEscape(project), map[string]any{"settings": map[string]any{"no_agent_instructions": off}})
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			if off {
				return "Agent instructions are off for " + project + ": its sessions are sent none."
			}
			return "Agent instructions are on for " + project + "."
		})
	},
}
