package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var envCmd = &command{
	name: "env", group: "Projects and time", args: "SUBCOMMAND",
	aliases: []string{"envs", "environment", "environments"},
	summary: "environments: is it up, what does it run, deploys",
	about: `A place a project's code runs: production, staging, or dev environments
that come and go. Waypoint health-checks each address. CI records
deployments with wp env deploy; a cluster calls wp env up / down for dev
environments. Scope: wp help scope.`,
	examples: []string{
		"wp env up a23x42s --branch fix/login --commit 3fa9c1e --ttl 3d",
		"wp env deploy staging --commit \"$GIT_SHA\" --status succeeded",
	},
}

func init() {
	envCmd.subs = []*command{envLsCmd, envShowCmd, envReadCmd, envVouchCmd, envUpCmd, envDownCmd, envDeployCmd}
	envCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return envLs(c, args)
		}
		return c.usage("no env command %q", args[0])
	}
}

// cliEnv is the part of an environment the CLI prints.
type cliEnv struct {
	ID          int64      `json:"id"`
	Project     string     `json:"project"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	URL         string     `json:"url"`
	HealthURL   string     `json:"health_url"`
	VersionURL  string     `json:"version_url"`
	Status      string     `json:"status"`
	StatusSince *time.Time `json:"status_since"`
	LatencyMS   *int       `json:"latency_ms"`
	HTTPCode    *int       `json:"http_code"`
	Error       string     `json:"error"`
	Branch      string     `json:"branch"`
	Commit      string     `json:"commit"`
	Version     string     `json:"version"`
	Behind      bool       `json:"behind"`
	Mismatch    bool       `json:"mismatch"`
	Ephemeral   bool       `json:"ephemeral"`
	Stopped     bool       `json:"stopped"`
	IntervalS   int        `json:"interval_s"`
	ExpiresAt   *time.Time `json:"expires_at"`
	TLSExpires  *time.Time `json:"tls_expires_at"`
	TLSWarning  bool       `json:"tls_warning"`
	Uptime24h   *float64   `json:"uptime_24h"`
	Uptime30d   *float64   `json:"uptime_30d"`
	// AddressAgent: who set the addresses, when not a person (WP-749).
	AddressAgent string `json:"address_agent"`

	LastDeploy *cliDeploy `json:"last_deploy"`
}

type cliDeploy struct {
	Commit     string     `json:"commit"`
	Branch     string     `json:"branch"`
	Version    string     `json:"version"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ByKind     string     `json:"by_kind"`
	By         *struct {
		DisplayName string `json:"display_name"`
	} `json:"by"`
	Agent string `json:"agent"`
	URL   string `json:"url"`
	Notes string `json:"notes"`
}

// age is how long ago, as 5m, 3h or 2d.
func age(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// runs is "main@3fa9c1e v1.4.0".
func (e *cliEnv) runs() string {
	var parts []string
	switch {
	case e.Branch != "" && e.Commit != "":
		parts = append(parts, e.Branch+"@"+shortSHA(e.Commit))
	case e.Commit != "":
		parts = append(parts, shortSHA(e.Commit))
	case e.Branch != "":
		parts = append(parts, e.Branch)
	}
	if e.Version != "" {
		parts = append(parts, "v"+strings.TrimPrefix(e.Version, "v"))
	}
	if len(parts) == 0 {
		return "-"
	}
	s := strings.Join(parts, " ")
	if e.Behind {
		s += " (behind)"
	}
	return s
}

func (e *cliEnv) state() string {
	if e.Stopped {
		return "stopped"
	}
	return e.Status
}

// parseTTL reads 90m, 12h, 3d, 2w or a number of seconds.
func parseTTL(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := map[byte]int{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 7 * 86400}
	n, unit := s, byte('s')
	if m, ok := mult[s[len(s)-1]]; ok && m > 0 {
		n, unit = s[:len(s)-1], s[len(s)-1]
	}
	v, err := strconv.ParseFloat(n, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%q isn't a time to live; try 12h, 3d or 2w", s)
	}
	return int(v * float64(mult[unit])), nil
}

// envScope is the project to work in: -p, or the git repo's.
func (c *cli) envProject(sf scopeFlags) (string, error) {
	sc, err := c.scopeFor(sf)
	if err != nil {
		return "", err
	}
	home, err := sc.home()
	if err != nil {
		return "", err
	}
	return home, nil
}

var envLsCmd = &command{
	name: "ls", args: "[FLAGS]", aliases: []string{"list"},
	summary:  "list environments: status, since, what they run",
	about:    `Down ones first. Stopped dev environments are left out unless --stopped.`,
	examples: []string{"wp env ls --stopped"},
	run:      envLs,
}

func envLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	stopped := fs.Bool("stopped", false, "include stopped ones")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	if *stopped {
		q.Set("stopped", "1")
	}
	sc, err := c.scopeFor(sf)
	if err != nil {
		return err
	}
	var all []cliEnv
	var raws []json.RawMessage
	fetch := func(project string) error {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		if project != "" {
			qq.Set("project", project)
		}
		raw, err := c.call("GET", "/api/v1/environments?"+qq.Encode(), nil)
		if err != nil {
			return err
		}
		var list []cliEnv
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		all = append(all, list...)
		raws = append(raws, raw)
		return nil
	}
	if sc.all() {
		if err := fetch(""); err != nil {
			return err
		}
	} else {
		c.note("Environments of %s, from %s.", sc, sc.from)
		for _, p := range sc.direct() {
			if err := fetch(p.Key); err != nil {
				return err
			}
		}
	}
	if c.asJSON {
		merged := []json.RawMessage{}
		for _, r := range raws {
			var part []json.RawMessage
			if json.Unmarshal(r, &part) == nil {
				merged = append(merged, part...)
			}
		}
		b, _ := json.Marshal(merged)
		return c.emit(b, func() string { return "" })
	}
	if len(all) == 0 {
		c.println("No environments. Record a deploy (wp env deploy NAME --commit SHA) or bring a dev one up (wp env up NAME --url URL).")
		return nil
	}
	rank := map[string]int{"down": 0, "degraded": 1, "unknown": 2, "up": 3}
	sortEnvs(all, rank)
	c.print(envTable(all, sc.all() || len(sc.direct()) > 1))
	return nil
}

func sortEnvs(all []cliEnv, rank map[string]int) {
	kind := map[string]int{"production": 0, "staging": 1, "preview": 2, "dev": 3}
	less := func(a, b cliEnv) bool {
		if a.Stopped != b.Stopped {
			return !a.Stopped
		}
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		return kind[a.Kind] < kind[b.Kind]
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && less(all[j], all[j-1]); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
}

func envTable(list []cliEnv, withProject bool) string {
	rows := [][]string{{"NAME", "KIND", "STATUS", "SINCE", "RUNS", "URL"}}
	if withProject {
		rows[0] = append([]string{"PROJECT"}, rows[0]...)
	}
	for i := range list {
		e := &list[i]
		row := []string{e.Name, e.Kind, e.state(), age(e.StatusSince), e.runs(), e.URL}
		if e.Stopped {
			row[3] = "-"
		}
		if withProject {
			row = append([]string{e.Project}, row...)
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			if n := len([]rune(cell)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, cell := range r {
			if i == len(r)-1 {
				b.WriteString(cell)
				break
			}
			b.WriteString(cell + strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
		}
		b.WriteString("\n")
	}
	return b.String()
}

var envShowCmd = &command{
	name: "show", args: "NAME", aliases: []string{"get", "status"},
	summary:  "one environment: status, what it runs, uptime, recent deploys",
	examples: []string{"wp env show production", "wp env show a23x42s --json"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which environment? Give its name")
		}
		project, err := c.envProject(sf)
		if err != nil {
			return err
		}
		raw, err := c.call("GET", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0]), nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var e cliEnv
			var full struct {
				Deployments []cliDeploy `json:"deployments"`
			}
			if json.Unmarshal(raw, &e) != nil || json.Unmarshal(raw, &full) != nil {
				return string(raw)
			}
			return envDetailText(&e, full.Deployments)
		})
	},
}

func envDetailText(e *cliEnv, deploys []cliDeploy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s  %s", e.Project, e.Name, e.Kind)
	if e.Ephemeral {
		b.WriteString(" (temporary)")
	}
	fmt.Fprintf(&b, "\nStatus:  %s", e.state())
	if !e.Stopped && e.StatusSince != nil {
		fmt.Fprintf(&b, " for %s", age(e.StatusSince))
	}
	if e.Mismatch {
		b.WriteString(" (still shows the old release)")
	}
	b.WriteString("\n")
	if e.Error != "" && e.Status != "up" && !e.Stopped {
		fmt.Fprintf(&b, "Error:   %s\n", e.Error)
	}
	if e.URL != "" {
		fmt.Fprintf(&b, "URL:     %s\n", e.URL)
		switch {
		case e.Stopped:
		case e.AddressAgent == "":
			fmt.Fprintf(&b, "Agents:  unattended sessions may read from it (a person set the address): wp env read %s /PATH\n", e.Name)
		default:
			fmt.Fprintf(&b, "Agents:  unattended sessions can't reach it: %s set the address; `wp env vouch %s` lets them read (a person's run does it, an agent's asks one)\n", e.AddressAgent, e.Name)
		}
	}
	if e.HTTPCode != nil && e.LatencyMS != nil {
		fmt.Fprintf(&b, "Last:    HTTP %d in %d ms\n", *e.HTTPCode, *e.LatencyMS)
	}
	fmt.Fprintf(&b, "Runs:    %s\n", e.runs())
	if e.Uptime24h != nil || e.Uptime30d != nil {
		var parts []string
		if e.Uptime24h != nil {
			parts = append(parts, fmt.Sprintf("%.2f%% over 24h", *e.Uptime24h))
		}
		if e.Uptime30d != nil {
			parts = append(parts, fmt.Sprintf("%.2f%% over 30d", *e.Uptime30d))
		}
		fmt.Fprintf(&b, "Uptime:  %s\n", strings.Join(parts, ", "))
	}
	if e.TLSExpires != nil {
		warn := ""
		if e.TLSWarning {
			warn = "  (expires soon!)"
		}
		fmt.Fprintf(&b, "TLS:     certificate expires %s%s\n", e.TLSExpires.Format("2006-01-02"), warn)
	}
	if e.ExpiresAt != nil {
		fmt.Fprintf(&b, "Stops:   %s (wp env up renews it)\n", e.ExpiresAt.Format("2006-01-02 15:04"))
	}
	if len(deploys) > 0 {
		b.WriteString("\nDeployments:\n")
		for i, d := range deploys {
			if i == 8 {
				break
			}
			who := d.ByKind
			if d.By != nil && d.ByKind != "detected" {
				who = d.By.DisplayName
				switch d.ByKind {
				case "agent":
					who = d.Agent
				case "ci":
					who += " (CI)"
				}
			}
			what := shortSHA(d.Commit)
			if d.Branch != "" {
				what = d.Branch + "@" + what
			}
			if d.Version != "" {
				what += " v" + strings.TrimPrefix(d.Version, "v")
			}
			t := d.StartedAt
			fmt.Fprintf(&b, "  %-9s %-28s %-9s %s", d.Status, what, ago(&t), who)
			if d.URL != "" {
				b.WriteString("  " + d.URL)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

var envUpCmd = &command{
	name: "up", args: "NAME [FLAGS]", aliases: []string{"start", "create"},
	summary: "create or renew a dev environment",
	about: `Without --url, the project's dev address (Project settings → Environments)
is used, with {name} replaced by NAME. --commit records a deployment.
--ttl is a lease: it stops when it runs out unless renewed by a deploy
or another up. Without one, an unreachable environment stops after 3 days.`,
	examples: []string{
		"wp env up a23x42s --url https://a23x42s.dev.example.com --commit 3fa9c1e --ttl 3d",
	},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		addr := fs.String("url", "", "environment `ADDRESS`")
		branch := fs.String("branch", "", "`BRANCH` it runs")
		commit := fs.String("commit", "", "`COMMIT` it runs")
		ttl := fs.String("ttl", "", "lease `DURATION`: 12h, 3d, 2w")
		kind := fs.String("kind", "dev", "`KIND`: dev or preview")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which environment? Give its name")
		}
		secs, err := parseTTL(*ttl)
		if err != nil {
			return c.usage("%v", err)
		}
		project, err := c.envProject(sf)
		if err != nil {
			return err
		}
		body := map[string]any{"url": *addr, "branch": *branch, "commit": *commit, "ttl_s": secs, "kind": *kind}
		raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0])+"/up", body)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var e cliEnv
			_ = json.Unmarshal(raw, &e)
			s := fmt.Sprintf("%s/%s is up at %s", e.Project, e.Name, e.URL)
			if e.ExpiresAt != nil {
				s += ", until " + e.ExpiresAt.Format("2006-01-02 15:04")
			}
			return s
		})
	},
}

var envDownCmd = &command{
	name: "down", args: "NAME", aliases: []string{"stop"},
	summary:  "stop checking a dev environment that is gone",
	about:    `It stays folded in Environments for a week, then is forgotten.`,
	examples: []string{"wp env down a23x42s"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which environment? Give its name")
		}
		project, err := c.envProject(sf)
		if err != nil {
			return err
		}
		raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0])+"/down", nil)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return fmt.Sprintf("%s/%s stopped", project, strings.ToLower(pos[0])) })
	},
}

var envDeployCmd = &command{
	name: "deploy", args: "NAME [FLAGS]", aliases: []string{"deployed"},
	summary: "record a deployment (CI calls this)",
	about: `Creates the environment if missing. Call with --status started, then
again with succeeded or failed for the same commit to finish it; the
default is succeeded. --commit defaults to the checked-out commit, unless you give --version
alone. Needs a token with write access.`,
	examples: []string{
		"wp env deploy staging --commit \"$SHA\" --status started --url \"$RUN_URL\"",
		"wp env deploy production --status failed --notes \"migration 0021 failed\"",
	},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		commit := fs.String("commit", "", "`COMMIT` deployed")
		branch := fs.String("branch", "", "source `BRANCH`")
		version := fs.String("version", "", "`VERSION` deployed")
		status := fs.String("status", "succeeded", "`STATUS`: started, succeeded or failed")
		runURL := fs.String("url", "", "CI run `ADDRESS`")
		notes := fs.String("notes", "", "`TEXT` shown with the deployment")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which environment? Give its name")
		}
		if *commit == "" && *version == "" {
			out, err := exec.Command("git", "rev-parse", "HEAD").Output()
			if err != nil {
				return c.usage("which commit? Give --commit (or --version)")
			}
			*commit = strings.TrimSpace(string(out))
		}
		project, err := c.envProject(sf)
		if err != nil {
			return err
		}
		body := map[string]any{"commit": *commit, "branch": *branch, "version": *version, "status": *status, "url": *runURL, "notes": *notes}
		raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0])+"/deployments", body)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string {
			var d cliDeploy
			_ = json.Unmarshal(raw, &d)
			what := shortSHA(d.Commit)
			if what == "" {
				what = "v" + d.Version
			}
			return fmt.Sprintf("%s: %s to %s/%s", d.Status, what, project, strings.ToLower(pos[0]))
		})
	},
}
