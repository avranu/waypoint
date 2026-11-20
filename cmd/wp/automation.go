package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Automations from the command line (WP-D18): list them, run the manual
// ones, read their runs.

var automationCmd = &command{
	name: "automation", group: "Projects and time", args: "SUBCOMMAND",
	aliases: []string{"automations"},
	summary: "list a project's automations, run manual ones, read their runs",
	about: `Manual-trigger automations can be run from here. Agents may run only
those marked "allow agents". NAME is a name (or unique part) or number.
Without a subcommand, lists them.`,
	examples: []string{"wp automation run \"Weekly report\" --input days=14 --wait"},
	run:      cmdAutomationLs,
}

func init() {
	automationCmd.subs = []*command{
		{name: "ls", args: "", aliases: []string{"list"}, summary: "list the project's automations",
			examples: nil, run: cmdAutomationLs},
		{name: "show", args: "NAME", aliases: []string{"view"}, summary: "show an automation: trigger, inputs, steps, runs",
			about:    `wp automation show NAME RUN prints one run's steps.`,
			examples: nil, run: cmdAutomationShow},
		{name: "run", args: "NAME [--input K=V]... [--wait]", summary: "run a manual automation now",
			about: `Defaults fill missing inputs. --wait waits (up to --timeout, 10m), prints
each step and exits non-zero if the run failed.`,
			examples: []string{"wp automation run report --input days=14 --wait"}, run: cmdAutomationRun},
		{name: "runs", args: "NAME [-n COUNT]", summary: "list an automation's recent runs",
			about:    `Newest first.`,
			examples: []string{"wp automation runs 12 -n 5"}, run: cmdAutomationRuns},
		{name: "cancel", args: "RUN", summary: "cancel an unfinished run",
			examples: nil, run: cmdAutomationCancel},
	}
}

type autoInfo struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	RunCount  int        `json:"run_count"`
	LastRunAt *time.Time `json:"last_run_at"`
	LastError string     `json:"last_error"`
	Durable   bool       `json:"durable"`
	HookURL   string     `json:"hook_url"`
	Rule      struct {
		Trigger struct {
			Event string `json:"event"`
		} `json:"trigger"`
		Inputs []struct {
			Name     string   `json:"name"`
			Label    string   `json:"label"`
			Type     string   `json:"type"`
			Default  string   `json:"default"`
			Options  []string `json:"options"`
			Required bool     `json:"required"`
		} `json:"inputs"`
		Actions []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"actions"`
		Concurrency string `json:"concurrency"`
		AllowAgents bool   `json:"allow_agents"`
	} `json:"rule"`
}

type autoRun struct {
	ID         int64          `json:"id"`
	Automation string         `json:"automation"`
	Status     string         `json:"status"`
	Via        string         `json:"via"`
	Summary    string         `json:"summary"`
	Error      string         `json:"error"`
	Durable    bool           `json:"durable"`
	Inputs     map[string]any `json:"inputs"`
	CreatedAt  time.Time      `json:"created_at"`
	DurationMS *int64         `json:"duration_ms"`
	Steps      []struct {
		StepID     string `json:"step_id"`
		Iteration  int    `json:"iteration"`
		Type       string `json:"type"`
		Status     string `json:"status"`
		Error      string `json:"error"`
		Log        string `json:"log"`
		DurationMS *int64 `json:"duration_ms"`
	} `json:"steps"`
}

func (r autoRun) finished() bool {
	switch r.Status {
	case "succeeded", "failed", "cancelled", "skipped":
		return true
	}
	return false
}

// automationsHere lists the automations of the scope's project.
func (c *cli) automationsHere(sf scopeFlags) ([]autoInfo, string, error) {
	sc, err := c.scopeFor(sf)
	if err != nil {
		return nil, "", err
	}
	home, err := sc.home()
	if err != nil {
		return nil, "", err
	}
	var list []autoInfo
	if err := c.do("GET", "/api/v1/projects/"+esc(home)+"/automations", nil, &list); err != nil {
		return nil, home, err
	}
	return list, home, nil
}

// findAutomation resolves a name or number.
func (c *cli) findAutomation(sf scopeFlags, ref string) (*autoInfo, error) {
	if id, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
		var a autoInfo
		if err := c.do("GET", fmt.Sprintf("/api/v1/automations/%d", id), nil, &a); err != nil {
			return nil, err
		}
		return &a, nil
	}
	list, home, err := c.automationsHere(sf)
	if err != nil {
		return nil, err
	}
	var exact, partial []autoInfo
	for _, a := range list {
		switch {
		case strings.EqualFold(a.Name, ref):
			exact = append(exact, a)
		case strings.Contains(strings.ToLower(a.Name), strings.ToLower(ref)):
			partial = append(partial, a)
		}
	}
	found := exact
	if len(found) == 0 {
		found = partial
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no automation called %q in %s (wp automation ls lists them)", ref, home)
	case 1:
		return &found[0], nil
	}
	var names []string
	for _, a := range found {
		names = append(names, fmt.Sprintf("#%d %s", a.ID, a.Name))
	}
	return nil, fmt.Errorf("%q could be %s; use the number", ref, strings.Join(names, " or "))
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func durationMS(ms *int64) string {
	if ms == nil {
		return ""
	}
	d := time.Duration(*ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", *ms)
	}
	return d.Round(100 * time.Millisecond).String()
}

func cmdAutomationLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	list, home, err := c.automationsHere(sf)
	if err != nil {
		return err
	}
	raw, _ := jsonBytes(list)
	return c.emit(raw, func() string {
		if len(list) == 0 {
			return "No automations in " + home + "."
		}
		var b strings.Builder
		for _, a := range list {
			state := "on "
			if !a.Enabled {
				state = "off"
			}
			trig := a.Rule.Trigger.Event
			if trig == "manual" && a.Rule.AllowAgents {
				trig += " (agents ok)"
			}
			fmt.Fprintf(&b, "#%-4d %s %-40s %-24s %4d runs, last %s\n", a.ID, state, a.Name, trig, a.RunCount, ago(a.LastRunAt))
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func cmdAutomationShow(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || len(pos) > 2 {
		return c.usage("give an automation's name or number (and a run number, for one run)")
	}
	a, err := c.findAutomation(sf, pos[0])
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		return c.printRun(pos[1], a.ID)
	}
	var runs []autoRun
	if err := c.do("GET", fmt.Sprintf("/api/v1/automations/%d/runs?limit=5", a.ID), nil, &runs); err != nil {
		return err
	}
	raw, _ := jsonBytes(map[string]any{"automation": a, "runs": runs})
	return c.emit(raw, func() string {
		var b strings.Builder
		state := "on"
		if !a.Enabled {
			state = "off"
		}
		fmt.Fprintf(&b, "#%d %s (%s)\nWhen: %s", a.ID, a.Name, state, a.Rule.Trigger.Event)
		if a.Rule.Trigger.Event == "manual" {
			fmt.Fprintf(&b, "; agents %s", map[bool]string{true: "may run it", false: "may not run it"}[a.Rule.AllowAgents])
		}
		if a.Rule.Concurrency != "" {
			fmt.Fprintf(&b, "; concurrency %s", a.Rule.Concurrency)
		}
		b.WriteString("\n")
		for _, in := range a.Rule.Inputs {
			extra := in.Type
			if len(in.Options) > 0 {
				extra += ": " + strings.Join(in.Options, "|")
			}
			if in.Default != "" {
				extra += ", default " + in.Default
			}
			if in.Required {
				extra += ", required"
			}
			fmt.Fprintf(&b, "Input %s (%s)\n", in.Name, extra)
		}
		for n, st := range a.Rule.Actions {
			id := st.ID
			if id == "" {
				id = fmt.Sprintf("s%d", n+1)
			}
			fmt.Fprintf(&b, "Step %s: %s\n", id, st.Type)
		}
		if a.Durable {
			b.WriteString("Long steps: runs continue in the background.\n")
		}
		if a.LastError != "" {
			fmt.Fprintf(&b, "Last error: %s\n", a.LastError)
		}
		for _, r := range runs {
			fmt.Fprintf(&b, "Run #%d %s %s %s\n", r.ID, r.Status, r.Via, durationMS(r.DurationMS))
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

// inputFlag collects --input name=value, keeping commas in values.
type inputFlag struct{ m map[string]any }

func (f *inputFlag) String() string { return "" }
func (f *inputFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("--input takes name=value, not %q", v)
	}
	f.m[strings.TrimSpace(k)] = val
	return nil
}

func cmdAutomationRun(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	inputs := &inputFlag{m: map[string]any{}}
	fs.Var(inputs, "input", "input `name=value` (repeat)")
	wait := fs.Bool("wait", false, "wait and print the steps")
	timeout := fs.Duration("timeout", 10*time.Minute, "max wait with --wait")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give the automation's name or number")
	}
	a, err := c.findAutomation(sf, pos[0])
	if err != nil {
		return err
	}
	var run autoRun
	if err := c.do("POST", fmt.Sprintf("/api/v1/automations/%d/run", a.ID), map[string]any{"inputs": inputs.m}, &run); err != nil {
		return err
	}
	if *wait {
		deadline := time.Now().Add(*timeout)
		for !run.finished() {
			left := time.Until(deadline)
			if left <= 0 {
				return fmt.Errorf("run #%d is still %s after %s; wp automation show %d %d to look later", run.ID, run.Status, *timeout, a.ID, run.ID)
			}
			time.Sleep(min(2*time.Second, left))
			if err := c.do("GET", fmt.Sprintf("/api/v1/automation-runs/%d", run.ID), nil, &run); err != nil {
				return err
			}
		}
		if err := c.printRun(strconv.FormatInt(run.ID, 10), a.ID); err != nil {
			return err
		}
		if run.Status == "failed" || run.Status == "cancelled" {
			return fmt.Errorf("run #%d %s", run.ID, run.Status)
		}
		return nil
	}
	raw, _ := jsonBytes(run)
	return c.emit(raw, func() string {
		return fmt.Sprintf("Run #%d of %s: %s.%s", run.ID, a.Name, run.Status, map[bool]string{true: " It continues in the background; wp automation show " + strconv.FormatInt(a.ID, 10) + " " + strconv.FormatInt(run.ID, 10) + " to look.", false: ""}[!run.finished()])
	})
}

// printRun prints one run with its steps.
func (c *cli) printRun(ref string, automation int64) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
	if err != nil {
		return c.usage("%q isn't a run number", ref)
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/automation-runs/%d", id), nil)
	if err != nil {
		return err
	}
	if automation != 0 {
		var of struct {
			AutomationID int64 `json:"automation_id"`
		}
		if jsonInto(raw, &of) == nil && of.AutomationID != automation {
			return fmt.Errorf("run #%d belongs to another automation (#%d), not #%d", id, of.AutomationID, automation)
		}
	}
	return c.emit(raw, func() string {
		var r autoRun
		if err := jsonInto(raw, &r); err != nil {
			return string(raw)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Run #%d of %s: %s (%s) %s\n", r.ID, r.Automation, r.Status, r.Via, durationMS(r.DurationMS))
		if r.Summary != "" {
			fmt.Fprintf(&b, "  %s\n", r.Summary)
		}
		for _, s := range r.Steps {
			iter := ""
			if s.Iteration > 1 {
				iter = fmt.Sprintf(" (pass %d)", s.Iteration)
			}
			fmt.Fprintf(&b, "  %-12s %-10s %s%s %s\n", s.StepID, s.Status, s.Type, iter, durationMS(s.DurationMS))
			if s.Error != "" {
				fmt.Fprintf(&b, "      error: %s\n", s.Error)
			}
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "  error: %s\n", r.Error)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func cmdAutomationRuns(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	n := fs.Int("n", 15, "how many runs")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give the automation's name or number")
	}
	a, err := c.findAutomation(sf, pos[0])
	if err != nil {
		return err
	}
	if *n < 1 || *n > 200 {
		return c.usage("-n takes a number from 1 to 200")
	}
	raw, err := c.call("GET", fmt.Sprintf("/api/v1/automations/%d/runs?limit=%d", a.ID, *n), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var runs []autoRun
		if err := jsonInto(raw, &runs); err != nil {
			return string(raw)
		}
		if len(runs) == 0 {
			return a.Name + " hasn't run yet."
		}
		var b strings.Builder
		for _, r := range runs {
			fmt.Fprintf(&b, "#%-6d %-10s %-9s %8s  %s\n", r.ID, r.Status, r.Via, durationMS(r.DurationMS), r.Summary)
		}
		return strings.TrimRight(b.String(), "\n")
	})
}

func cmdAutomationCancel(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give the run's number")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	if err != nil {
		return c.usage("%q isn't a run number", pos[0])
	}
	raw, err := c.call("POST", fmt.Sprintf("/api/v1/automation-runs/%d/cancel", id), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r autoRun
		_ = jsonInto(raw, &r)
		return fmt.Sprintf("Run #%d is %s.", id, r.Status)
	})
}

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }
