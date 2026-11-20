package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var projectCmd = &command{
	name: "project", group: "Projects and time", args: "SUBCOMMAND",
	aliases:  []string{"projects"},
	summary:  "list projects, or show one's statuses, tags, members",
	about:    `Without a subcommand, lists them.`,
	examples: []string{"wp project show APP"},
	run:      projectLs,
}

func init() {
	projectCmd.subs = []*command{
		{name: "ls", args: "", aliases: []string{"list"}, summary: "list projects with open issue counts",
			about:    `* marks the scope here.`,
			examples: nil, run: projectLs},
		{name: "show", args: "[PROJECT]", aliases: []string{"view"}, summary: "show statuses, tags, members, repos, workflows",
			about:    `PROJECT is a key, name or repo; default: this repo's.`,
			examples: []string{"wp project show APP"}, run: projectShow},
		projectRepoCmd,
	}
}

type projectInfo struct {
	ID         int64  `json:"id"`
	Key        string `json:"key"`
	Name       string `json:"name"`
	ParentID   *int64 `json:"parent_id"`
	Role       string `json:"role"`
	OpenIssues int    `json:"open_issues"`
	Statuses   []struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	} `json:"statuses"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Members []struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		Role string `json:"role"`
	} `json:"members"`
	Sprints []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"sprints"`
}

func projectLs(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/projects", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var ps []projectInfo
		if err := jsonInto(raw, &ps); err != nil {
			return string(raw)
		}
		if len(ps) == 0 {
			return "No projects."
		}
		here := map[string]bool{}
		if sc, err := c.implicitScope(); err == nil {
			for _, p := range sc.direct() {
				here[p.Key] = true
			}
		}
		var b strings.Builder
		for _, n := range treeOrder(ps) {
			mark := " "
			if here[n.p.Key] {
				mark = "*"
			}
			fmt.Fprintf(&b, "%s %s%-10s %s  (%d open, %s)\n", mark, strings.Repeat("  ", n.depth), n.p.Key, n.p.Name, n.p.OpenIssues, n.p.Role)
		}
		if len(here) > 0 {
			b.WriteString("* the scope here\n")
		}
		return b.String()
	})
}

type treeNode struct {
	p     projectInfo
	depth int
}

// treeOrder nests projects under their parents (a parent you can't see
// makes a root), keeping the server's order among siblings.
func treeOrder(ps []projectInfo) []treeNode {
	ids := map[int64]bool{}
	for _, p := range ps {
		ids[p.ID] = true
	}
	kids := map[int64][]projectInfo{}
	for _, p := range ps {
		parent := int64(0)
		if p.ParentID != nil && ids[*p.ParentID] {
			parent = *p.ParentID
		}
		kids[parent] = append(kids[parent], p)
	}
	var out []treeNode
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for _, p := range kids[parent] {
			out = append(out, treeNode{p, depth})
			walk(p.ID, depth+1)
		}
	}
	walk(0, 0)
	return out
}

func projectShow(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var sc *scope
	if len(pos) > 0 {
		sc, err = c.findScope(joinArgs(pos), "")
	} else {
		sc, err = c.implicitScope()
	}
	if err != nil {
		return err
	}
	key, err := sc.home()
	if err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/projects/"+esc(key), nil)
	if err != nil {
		return err
	}
	if c.asJSON {
		return c.emit(raw, nil)
	}
	var p projectInfo
	if err := jsonInto(raw, &p); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  (%d open; you're %s)\n", p.Key, p.Name, p.OpenIssues, p.Role)
	var st []string
	for _, s := range p.Statuses {
		st = append(st, fmt.Sprintf("%s (%s)", s.Name, s.Category))
	}
	b.WriteString("Statuses: " + strings.Join(st, ", ") + "\n")
	var repos []struct {
		Repo       string `json:"repo"`
		ForgeName  string `json:"forge_name"`
		SyncIssues bool   `json:"sync_issues"`
	}
	if c.do("GET", "/api/v1/projects/"+esc(key)+"/repos", nil, &repos) == nil && len(repos) > 0 {
		var rs []string
		for _, r := range repos {
			s := r.Repo + " on " + r.ForgeName
			if r.SyncIssues {
				s += " (issues synced)"
			}
			rs = append(rs, s)
		}
		b.WriteString("Repos: " + strings.Join(rs, ", ") + "\n")
	}
	if subs := sc.projects[len(sc.direct()):]; len(subs) > 0 {
		var ks []string
		for _, s := range subs {
			ks = append(ks, s.Key+" "+s.Name)
		}
		b.WriteString("Sub-projects: " + strings.Join(ks, ", ") + "\n")
	}
	if len(p.Labels) > 0 {
		var ls []string
		for _, l := range p.Labels {
			ls = append(ls, l.Name)
		}
		b.WriteString("Tags: " + strings.Join(ls, ", ") + "\n")
	}
	var ms []string
	for _, m := range p.Members {
		ms = append(ms, "@"+m.User.Username+" ("+m.Role+")")
	}
	if len(ms) > 0 {
		b.WriteString("Members: " + strings.Join(ms, ", ") + "\n")
	}
	var sp []string
	for _, s := range p.Sprints {
		sp = append(sp, s.Name+" ("+s.State+")")
	}
	if len(sp) > 0 {
		b.WriteString("Sprints: " + strings.Join(sp, ", ") + "\n")
	}
	var wfs []struct {
		Name  string `json:"name"`
		Steps []struct {
			Name string `json:"name"`
		} `json:"steps"`
	}
	if c.do("GET", "/api/v1/workflows?project="+esc(key), nil, &wfs) == nil && len(wfs) > 0 {
		b.WriteString("Workflows (wp new --workflow, wp next --step):\n")
		for _, w := range wfs {
			var steps []string
			for _, s := range w.Steps {
				steps = append(steps, s.Name)
			}
			fmt.Fprintf(&b, "  %s: %s\n", w.Name, strings.Join(steps, " → "))
		}
	}
	b.WriteString(c.server() + "/p/" + p.Key + "/board\n")
	c.print(b.String())
	return nil
}

// ---- time ----

var timerCmd = &command{
	name: "timer", group: "Projects and time", args: "[start [KEY] | stop]",
	summary: "start, stop or check your timer",
	about: `Without a subcommand, shows the running timer. wp log records time
after the fact.`,
	examples: []string{"wp timer start APP-12", "wp timer stop"},
	run:      timerStatus,
}

func init() {
	timerCmd.subs = []*command{
		{name: "start", args: "[KEY] [-m NOTE]", summary: "start timing an issue (stops any running timer)",
			about:    `KEY defaults to your current task; with neither, times no issue.`,
			examples: []string{`wp timer start -m "Reviewing PRs"`}, run: timerStart},
		{name: "stop", args: "", summary: "stop the timer and record the time", examples: []string{"wp timer stop"}, run: timerStop},
		{name: "status", args: "", aliases: []string{"show"}, summary: "show the running timer", examples: []string{"wp timer status"}, run: timerStatus},
	}
}

type timeEntry struct {
	Issue *struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"issue"`
	Note    string `json:"note"`
	Seconds int64  `json:"seconds"`
}

func (t *timeEntry) String() string {
	what := "no issue"
	if t.Issue != nil {
		what = t.Issue.Key + " " + t.Issue.Title
	}
	if t.Note != "" {
		what += " (" + t.Note + ")"
	}
	return fmt.Sprintf("%s on %s", (time.Duration(t.Seconds) * time.Second).String(), what)
}

func timerStatus(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/timer", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r struct{ Timer *timeEntry }
		if jsonInto(raw, &r) != nil || r.Timer == nil {
			return "No timer running."
		}
		return "Running: " + r.Timer.String()
	})
}

func timerStart(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("message", "", "`NOTE` saved with the time")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil && len(pos) == 0 {
		key, err = "", nil // a timer on no issue
	}
	if err != nil || len(rest) > 0 {
		return c.usage("expected an issue KEY (or none)")
	}
	raw, err := c.call("POST", "/api/v1/timer/start", map[string]string{"issue": key, "note": *note})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if key == "" {
			return "Timer started."
		}
		return "Timer started on " + key + "."
	})
}

func timerStop(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	raw, err := c.call("POST", "/api/v1/timer/stop", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var r struct{ Stopped *timeEntry }
		if jsonInto(raw, &r) != nil || r.Stopped == nil {
			return "No timer was running."
		}
		return "Stopped: " + r.Stopped.String()
	})
}

var logCmd = &command{
	name: "log", group: "Projects and time", args: "[KEY] DURATION [-m NOTE]",
	summary:  "record time spent on an issue",
	about:    `Time already spent, ending now. DURATION: 1h30m, 45m, 1:30 or 90 (minutes).`,
	examples: []string{`wp log APP-12 45m -m "Pairing on the parser"`},
	run:      cmdLog,
}

func cmdLog(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("message", "", "`NOTE` saved with the time")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return c.usage("expected a DURATION like 1h30m")
	}
	raw, err := c.call("POST", "/api/v1/time", map[string]string{"issue": key, "duration": rest[0], "note": *note})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var t timeEntry
		if jsonInto(raw, &t) != nil {
			return "Logged."
		}
		return "Logged " + t.String()
	})
}

// ---- context ----

var contextCmd = &command{
	name: "context", group: "Start here", args: "",
	aliases: []string{"whoami"},
	summary: "who you are, which server, the scope here, and the task you took",
	about: `What other commands assume here: server, login, agent identity, scope
(wp help scope) and the task taken in this directory.`,
	run: cmdContext,
}

func cmdContext(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	out := map[string]any{"server": c.server(), "agent": c.agentHeader(), "default_project": c.cfg.Project}
	lines := []string{}
	add := func(k, v string) { lines = append(lines, fmt.Sprintf("%-9s %s", k+":", v)) }
	switch {
	case c.server() == "":
		add("Server", "none set: wp auth login https://your-waypoint")
	case c.token() == "":
		add("Server", c.server()+" (not logged in: wp auth login)")
	default:
		var me struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		}
		if err := c.do("GET", "/api/v1/me", nil, &me); err != nil {
			add("Server", c.server()+" ("+err.Error()+")")
		} else {
			add("Server", fmt.Sprintf("%s, as %s (@%s)", c.server(), me.DisplayName, me.Username))
			out["user"] = me.Username
		}
	}
	agent := orDash(c.agentHeader())
	if _, set := c.explicitAgent(); !set && c.agentHeader() != "" {
		agent += " (detected)"
	}
	add("Agent", agent)
	if repo, inRepo := gitRepoHere(); repo != "" {
		add("Repo", repo)
		out["repo"] = repo
	} else if inRepo {
		add("Repo", "a git repo without an owner/name remote")
	}
	if c.token() != "" {
		if sc, err := c.implicitScope(); err != nil {
			add("Scope", "none: "+strings.ReplaceAll(err.Error(), "\n", "\n          "))
			out["scope_error"] = err.Error()
		} else {
			add("Scope", sc.String())
			var keys []string
			for _, p := range sc.projects {
				keys = append(keys, p.Key)
			}
			out["scope"] = keys
		}
	}
	if dir, err := os.Getwd(); err == nil {
		if _, cl := c.state.claimFor(dir); cl != nil {
			add("Task", cl.Key+", taken here "+cl.ClaimedAt.Local().Format("Jan 2 15:04")+" (wp show)")
			out["task"] = cl.Key
		} else {
			add("Task", "none taken here (wp next takes one)")
		}
	}
	if c.asJSON {
		b, _ := json.MarshalIndent(out, "", "  ")
		c.println(string(b))
		return nil
	}
	c.println(strings.Join(lines, "\n"))
	return nil
}
