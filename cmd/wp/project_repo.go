package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

var projectRepoCmd = &command{name: "repo", args: "[ls|set|move|relink …]", aliases: []string{"repos"},
	summary: "a project's repos and their merge policy",
	about: `Lists attached repos with their merge policy. Merge automatically arms
every open PR into the branches it covers (--into) except drafts, forks,
release PRs and do-not-merge or wip ones.`,
	examples: []string{"wp project repo set --auto-merge off"},
	run:      projectRepoLs}

func init() {
	projectRepoCmd.subs = []*command{
		{name: "ls", aliases: []string{"list"}, args: "[-p PROJECT]", summary: "list the project's repos with their merge policy",
			examples: nil, run: projectRepoLs},
		{name: "set", args: "[REPO] [--auto-merge on|off] [--into any|default|others|GLOB…] [--required-check C]… [--review-wait 30m] [--quiet 10m] [--reviewer LOGIN]… [--line-candidates on|off]",
			summary: "change a repo's merge policy (project owners)",
			about: `REPO is owner/name or its name; default: the git repo here, or the
project's only repo. An armed merge waits for required checks, then for
expected reviewers (or the review wait) and the quiet period. Turning
Merge automatically off removes only the arming it made itself. --into
says which base branches it arms pull requests into: any, the default
branch, others (every branch but the default) or branch globs (repeat);
an agent's wp ship into any other merges by hand.`,
			examples: []string{"wp project repo set --auto-merge off", "wp project repo set Acme --auto-merge on --into 'release/**'", "wp project repo set Quarry --required-check local-ci",
				"wp project repo set --review-wait 45m --quiet 5m"},
			run: projectRepoSet},
		{name: "move", aliases: []string{"rename"}, args: "[OLD] NEW", summary: "follow a repo renamed or transferred (project owners)",
			about: `The repo now lives at NEW (owner/name); links, PRs and checks follow.
OLD defaults to the project's only repo. Waypoint follows moves by
itself; use this for one that happened before it knew the forge id. A
duplicate project made for the moved repo is taken over: delete it.`,
			examples: []string{"wp project repo move -p OPSINF Harbor/infrastructure Ops/infrastructure"},
			run:      projectRepoMove},
		{name: "relink", args: "[OLD] NEW [--force]", summary: "link the project to a different repo (project owners)",
			about: `For when move can't follow (the repo was deleted and recreated, or
replaced). Settings and history move to NEW. Refused if another project
links NEW or the old repo; --force detaches NEW from them.`,
			examples: []string{"wp project repo relink acme/old acme/new --force"},
			run:      projectRepoRelink},
	}
}

// repoPolicy is an attached repo as GET /projects/{key}/repos returns it.
type repoPolicy struct {
	ID                int64    `json:"id"`
	Repo              string   `json:"repo"`
	ForgeName         string   `json:"forge_name"`
	AutoMerge         bool     `json:"auto_merge"`
	RequiredChecks    []string `json:"required_checks"`
	ReviewWaitMinutes int      `json:"review_wait_minutes"`
	QuietMinutes      int      `json:"quiet_minutes"`
	ExternalReviewers []string `json:"external_reviewers"`
	AutoMergeInto     string   `json:"auto_merge_into"`
	AutoMergeBases    []string `json:"auto_merge_bases"`
	DefaultBranch     string   `json:"default_branch"`
	LineCandidates    bool     `json:"line_candidates"`
}

// intoText says which base branches Merge automatically arms into.
func (r repoPolicy) intoText() string {
	switch r.AutoMergeInto {
	case "default":
		return "into " + cmpStr(r.DefaultBranch, "the default branch") + " only"
	case "others":
		return "into every branch but " + cmpStr(r.DefaultBranch, "the default")
	case "match":
		return "into " + cmpStr(strings.Join(r.AutoMergeBases, " or "), "no branch")
	}
	return ""
}

func (r repoPolicy) text() string {
	onOff := map[bool]string{true: "on", false: "off"}[r.AutoMerge]
	if into := r.intoText(); r.AutoMerge && into != "" {
		onOff += " (" + into + ")"
	}
	list := func(l []string) string { return cmpStr(strings.Join(l, ", "), "none") }
	line := map[bool]string{true: "on", false: "off"}[r.LineCandidates]
	return fmt.Sprintf("%s on %s\n  merge automatically: %s · required checks: %s · review wait %s · quiet %s · external reviewers: %s · line candidates: %s\n",
		r.Repo, r.ForgeName, onOff, list(r.RequiredChecks), minutesText(r.ReviewWaitMinutes), minutesText(r.QuietMinutes), list(r.ExternalReviewers), line)
}

// minutesText: 30 → "30m", 60 → "1h", 90 → "1h30m", 0 → "none".
func minutesText(m int) string {
	switch {
	case m == 0:
		return "none"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

// parseMinutes reads "30m", "1h", "90" (minutes) or "0".
func parseMinutes(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d%time.Minute != 0 {
		return 0, fmt.Errorf("%q isn't a number of minutes (30, 30m, 1h)", s)
	}
	return int(d / time.Minute), nil
}

// projectRepos lists the repos of the project in scope.
func (c *cli) projectRepos(sf scopeFlags, ref string) (string, []repoPolicy, error) {
	sc, err := c.scopeFor(sf)
	if ref != "" && (sf.project == nil || *sf.project == "") {
		sc, err = c.findScope(ref, ref)
	}
	if err != nil {
		return "", nil, err
	}
	key, err := sc.home()
	if err != nil {
		return "", nil, err
	}
	var repos []repoPolicy
	if err := c.do("GET", "/api/v1/projects/"+esc(key)+"/repos", nil, &repos); err != nil {
		return "", nil, err
	}
	return key, repos, nil
}

func projectRepoLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	_, repos, err := c.projectRepos(sf, "")
	if err != nil {
		return err
	}
	if c.asJSON {
		return c.emitValue(repos)
	}
	if len(repos) == 0 {
		c.println("No repos attached (Project settings → Integrations).")
		return nil
	}
	var b strings.Builder
	for _, r := range repos {
		b.WriteString(r.text())
	}
	_, err = fmt.Fprint(c.out, b.String())
	return err
}

func projectRepoSet(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	auto := fs.String("auto-merge", "", "`on` or off")
	var into listFlag
	fs.Var(&into, "into", "base branches it arms into: any, default, others or a branch `GLOB` (repeat)")
	var checks, reviewers listFlag
	fs.Var(&checks, "required-check", "required status `CONTEXT` (repeat; replaces the list)")
	noChecks := fs.Bool("no-required-checks", false, "clear the required checks")
	wait := fs.String("review-wait", "", "reviewer wait after a push (`30m`; 0: none)")
	quiet := fs.String("quiet", "", "quiet period before merge (`10m`; 0: none)")
	fs.Var(&reviewers, "reviewer", "external reviewer bot `LOGIN` or status (repeat; replaces)")
	noReviewers := fs.Bool("no-reviewers", false, "clear the reviewers")
	lineCand := fs.String("line-candidates", "", "the merge line tests pull requests on a candidate branch: `on` or off")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("give at most one REPO")
	}
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	}
	key, repos, err := c.projectRepos(sf, ref)
	if err != nil {
		return err
	}
	r, err := pickRepo(repos, ref)
	if err != nil {
		return err
	}
	body := map[string]any{}
	switch strings.ToLower(*auto) {
	case "":
	case "on", "true", "yes":
		body["auto_merge"] = true
	case "off", "false", "no":
		body["auto_merge"] = false
	default:
		return c.usage("--auto-merge is on or off")
	}
	switch strings.ToLower(*lineCand) {
	case "":
	case "on", "true", "yes":
		body["line_candidates"] = true
	case "off", "false", "no":
		body["line_candidates"] = false
	default:
		return c.usage("--line-candidates is on or off")
	}
	keyword := func(x string) bool { return slices.Contains([]string{"any", "default", "others"}, strings.ToLower(x)) }
	switch {
	case len(into) == 0:
	case len(into) == 1 && keyword(into[0]):
		body["auto_merge_into"] = strings.ToLower(into[0])
	case slices.ContainsFunc(into, keyword):
		return c.usage("--into takes any, default or others alone, or branch globs")
	case slices.ContainsFunc(into, func(x string) bool { return strings.EqualFold(x, "match") }):
		return c.usage("--into takes the branch globs themselves: --into 'release/**'")
	default:
		body["auto_merge_into"], body["auto_merge_bases"] = "match", []string(into)
	}
	if err := putList(body, "required_checks", checks, *noChecks, "--required-check", "--no-required-checks"); err != nil {
		return c.usage("%s", err.Error())
	}
	if err := putList(body, "external_reviewers", reviewers, *noReviewers, "--reviewer", "--no-reviewers"); err != nil {
		return c.usage("%s", err.Error())
	}
	for flagName, v := range map[string]*string{"review_wait_minutes": wait, "quiet_minutes": quiet} {
		if *v == "" {
			continue
		}
		n, err := parseMinutes(*v)
		if err != nil {
			return c.usage("%s", err.Error())
		}
		body[flagName] = n
	}
	if len(body) == 0 {
		return c.usage("say what to change: --auto-merge, --into, --required-check, --review-wait, --quiet, --reviewer or --line-candidates")
	}
	if _, err := c.call("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", esc(key), r.ID), body); err != nil {
		return err
	}
	_, after, err := c.projectRepos(sf, ref)
	if err != nil {
		return err
	}
	now, _ := pickRepo(after, r.Repo)
	if c.asJSON {
		return c.emitValue(now)
	}
	_, err = fmt.Fprint(c.out, now.text())
	return err
}

func projectRepoMove(c *cli, args []string) error { return moveOrRelink(c, args, false) }

func projectRepoRelink(c *cli, args []string) error { return moveOrRelink(c, args, true) }

func moveOrRelink(c *cli, args []string, relink bool) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	force := new(bool)
	if relink {
		force = fs.Bool("force", false, "take NEW over from other projects")
	}
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	ref, to := "", ""
	switch len(pos) {
	case 1:
		to = pos[0]
	case 2:
		ref, to = pos[0], pos[1]
	default:
		return c.usage("give the new owner/name, after the old one if the project has several repos")
	}
	key, repos, err := c.projectRepos(sf, ref)
	if err != nil {
		return err
	}
	r, err := pickMoved(repos, ref)
	if err != nil {
		return err
	}
	var now repoPolicy
	if err := c.do("POST", fmt.Sprintf("/api/v1/projects/%s/repos/%d/move", esc(key), r.ID), map[string]any{"repo": to, "relink": relink, "force": *force}, &now); err != nil {
		return err
	}
	if c.asJSON {
		return c.emitValue(now)
	}
	if relink {
		c.println(fmt.Sprintf("%s is linked to %s now, in %s.", r.Repo, now.Repo, key))
		return nil
	}
	c.println(fmt.Sprintf("%s is %s now, in %s.", r.Repo, now.Repo, key))
	return nil
}

// pickMoved is pickRepo without the git repo here as the default: its
// remote may name the repo's new home already.
func pickMoved(repos []repoPolicy, ref string) (repoPolicy, error) {
	if ref == "" && len(repos) > 1 {
		names := make([]string, len(repos))
		for i, r := range repos {
			names[i] = r.Repo
		}
		return repoPolicy{}, fmt.Errorf("which repo moved? It has %s", strings.Join(names, ", "))
	}
	if ref == "" && len(repos) == 1 {
		return repos[0], nil
	}
	return pickRepo(repos, ref)
}

// putList puts a repeated flag's values (or an empty list for its --no-
// form) in body.
func putList(body map[string]any, key string, vals listFlag, none bool, name, noName string) error {
	switch {
	case none && len(vals) > 0:
		return fmt.Errorf("%s and %s contradict each other", name, noName)
	case none:
		body[key] = []string{}
	case len(vals) > 0:
		body[key] = []string(vals)
	}
	return nil
}

// pickRepo finds REPO (owner/name or name) among a project's repos; with
// no REPO, the git repo here or the only one.
func pickRepo(repos []repoPolicy, ref string) (repoPolicy, error) {
	if ref == "" {
		if here, _ := gitRepoHere(); here != "" {
			ref = here
		}
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Repo)
		if ref != "" && (strings.EqualFold(r.Repo, ref) || strings.EqualFold(r.Repo[strings.LastIndex(r.Repo, "/")+1:], ref)) {
			return r, nil
		}
	}
	switch {
	case ref == "" && len(repos) == 1:
		return repos[0], nil
	case len(repos) == 0:
		return repoPolicy{}, fmt.Errorf("the project has no repos attached")
	case ref == "":
		return repoPolicy{}, fmt.Errorf("which repo? It has %s", strings.Join(names, ", "))
	}
	return repoPolicy{}, fmt.Errorf("no repo %q in the project (it has %s)", ref, strings.Join(names, ", "))
}
