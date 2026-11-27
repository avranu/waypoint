package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

// wp sandbox (WP-418, WP-ADR28 decision 3): read-only. Sandbox settings
// change only in the Waypoint UI, by a person, so an agent with a token
// can't loosen its own sandbox.
var sandboxCmd = &command{
	name: "sandbox", group: "Setup", args: "SUBCOMMAND",
	summary: "sandbox settings per machine and project",
	about: `Shows sandbox levels per machine and project (strongest first: vm, contained,
tool, off), helper readiness, and escape drill status. Read-only: change levels
in the Waypoint UI (Project settings → Sandbox, Admin → Sandbox).`,
	examples: []string{"wp sandbox status", "wp sandbox show", "wp sandbox show HOME --log", "wp sandbox doctor"},
}

func init() {
	sandboxCmd.subs = append([]*command{sandboxStatusCmd, sandboxShowCmd}, sandboxCmd.subs...)
	sandboxCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 || args[0] == "-h" {
			return sandboxStatus(c, args)
		}
		return c.usage("no sandbox command %q", args[0])
	}
}

type cliSandboxMachine struct {
	Runner struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
		Online   bool   `json:"online"`
		Owner    struct {
			Username string `json:"username"`
		} `json:"owner"`
	} `json:"runner"`
	DefaultLevel string     `json:"default_level"`
	Paused       bool       `json:"paused"`
	UpdatedAt    *time.Time `json:"updated_at"`
	Helper       *struct {
		Installed bool   `json:"installed"`
		Ready     bool   `json:"ready"`
		Problem   string `json:"problem"`
		Status    *struct {
			Version string `json:"version"`
			Users   []struct {
				Name string `json:"name"`
			} `json:"users"`
		} `json:"status"`
	} `json:"helper"`
	Drill *struct {
		State      string     `json:"state"`
		Escaped    int        `json:"escaped"`
		Controls   int        `json:"failed_controls"`
		Error      string     `json:"error"`
		FinishedAt *time.Time `json:"finished_at"`
		Passed     int        `json:"passed"`
	} `json:"drill"`
}

// drillText is a machine's last escape drill (WP-473), in a few words.
func (m cliSandboxMachine) drillText() string {
	d := m.Drill
	switch {
	case d == nil:
		return "none yet"
	case d.State == "failed":
		return fmt.Sprintf("FAILED %s: %d got through, %d controls refused", ago(d.FinishedAt), d.Escaped, d.Controls)
	case d.State == "error":
		return fmt.Sprintf("didn't run %s: %s", ago(d.FinishedAt), d.Error)
	case d.State == "passed":
		return fmt.Sprintf("passed %s (%d tries)", ago(d.FinishedAt), d.Passed)
	}
	return d.State
}

// helperText is a machine's helper, in a word or few.
func (m cliSandboxMachine) helperText() string {
	h := m.Helper
	switch {
	case h == nil || !h.Installed:
		return "not installed"
	case !h.Ready:
		return "PROBLEM: " + h.Problem
	case h.Status != nil:
		return fmt.Sprintf("ready (%s, %d users)", h.Status.Version, len(h.Status.Users))
	}
	return "ready"
}

type cliSandboxPolicy struct {
	Project          string `json:"project"`
	Level            string `json:"level"`
	InteractiveLevel string `json:"interactive_level"`
	UpdatedBy        *struct {
		Username string `json:"username"`
	} `json:"updated_by"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type cliSandboxChange struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	User *struct {
		Username string `json:"username"`
	} `json:"user"`
	Scope      string `json:"scope"`
	TargetName string `json:"target_name"`
	Field      string `json:"field"`
	Old        string `json:"old"`
	New        string `json:"new"`
	Loosens    bool   `json:"loosens"`
	Reason     string `json:"reason"`
	Reverts    *int64 `json:"reverts"`
}

var sandboxStatusCmd = &command{
	name: "status", aliases: []string{"machines"},
	summary:  "the machines you can see: default level, kill switch, helper",
	examples: []string{"wp sandbox status", "wp sandbox status --json"},
	run:      sandboxStatus,
}

func sandboxStatus(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/sandbox/machines", nil)
	if err != nil {
		return err
	}
	var ms []cliSandboxMachine
	if err := json.Unmarshal(raw, &ms); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if len(ms) == 0 {
			return "No machines. A runner shows up here once it has registered (wp runner).\n"
		}
		var b strings.Builder
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "MACHINE\tOWNER\tDEFAULT\tSANDBOXING\tCHANGED\tHELPER\tESCAPE DRILL")
		for _, m := range ms {
			state := "on"
			if m.Paused {
				state = "PAUSED"
			}
			name := m.Runner.Name
			if !m.Runner.Online {
				name += " (offline)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, m.Runner.Owner.Username, levelOr(m.DefaultLevel, "not set"), state, ago(m.UpdatedAt), m.helperText(), m.drillText())
		}
		_ = w.Flush()
		return b.String()
	})
}

var sandboxShowCmd = &command{
	name: "show", args: "[PROJECT] [--log]",
	summary:  "a project's sandbox level for jobs and sessions",
	examples: []string{"wp sandbox show", "wp sandbox show HOME --log"},
	run:      sandboxShow,
}

func sandboxShow(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	withLog := fs.Bool("log", false, "list recent changes")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var key string
	switch len(pos) {
	case 0:
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		if key, err = sc.home(); err != nil {
			return err
		}
	case 1:
		key = strings.ToUpper(pos[0])
	default:
		return c.usage("wp sandbox show takes one project")
	}
	raw, err := c.call("GET", "/api/v1/projects/"+esc(key)+"/sandbox", nil)
	if err != nil {
		return err
	}
	var p cliSandboxPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	var log []cliSandboxChange
	if *withLog {
		lraw, err := c.call("GET", "/api/v1/sandbox/log?"+url.Values{"project": {key}}.Encode(), nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(lraw, &log); err != nil {
			return err
		}
		if c.asJSON {
			b, _ := json.Marshal(map[string]any{"policy": json.RawMessage(raw), "log": json.RawMessage(lraw)})
			raw = b
		}
	}
	return c.emit(raw, func() string {
		var b strings.Builder
		fmt.Fprintf(&b, "%s sandbox\n", p.Project)
		fmt.Fprintf(&b, "  runner jobs:          %s\n", levelOr(p.Level, "the machine's default"))
		fmt.Fprintf(&b, "  interactive sessions: %s\n", levelOr(p.InteractiveLevel, "same as runner jobs"))
		if p.UpdatedBy != nil {
			fmt.Fprintf(&b, "  changed by %s %s\n", p.UpdatedBy.Username, ago(p.UpdatedAt))
		}
		if *withLog {
			b.WriteString("\n")
			b.WriteString(sandboxLogText(log))
		}
		b.WriteString("Change it in the Waypoint UI: Project settings → Sandbox.\n")
		return b.String()
	})
}

func sandboxLogText(log []cliSandboxChange) string {
	if len(log) == 0 {
		return "No changes yet.\n"
	}
	var b strings.Builder
	for _, l := range log {
		who := "someone"
		if l.User != nil {
			who = l.User.Username
		}
		verb := "tightened"
		if l.Loosens {
			verb = "LOOSENED"
		}
		fmt.Fprintf(&b, "#%d %s %s %s %s: %s → %s", l.ID, l.At.Local().Format("2006-01-02 15:04"), who, verb, strings.ReplaceAll(l.Field, "_", " "), levelOr(l.Old, "unset"), levelOr(l.New, "unset"))
		if l.Reverts != nil {
			fmt.Fprintf(&b, " (reverts #%d)", *l.Reverts)
		}
		if l.Reason != "" {
			fmt.Fprintf(&b, " — %s", l.Reason)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func levelOr(v, unset string) string {
	if v == "" {
		return unset
	}
	return v
}
