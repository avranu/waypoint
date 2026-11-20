package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// scope is the projects a command works in (see `wp help scope`). No
// projects means every project.
type scope struct {
	projects []scopeProject // the matches first, then their sub-projects
	from     string         // how it was chosen, for messages
}

type scopeProject struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	ParentID  *int64 `json:"parent_id"`
	MatchedBy string `json:"matched_by"`
}

func (s *scope) all() bool { return len(s.projects) == 0 }

// direct are the projects that matched, without their sub-projects.
func (s *scope) direct() []scopeProject {
	var out []scopeProject
	for _, p := range s.projects {
		if p.MatchedBy != "sub-project" {
			out = append(out, p)
		}
	}
	return out
}

// query is the scope as a query-language term ("" for every project).
func (s *scope) query() string {
	if s.all() {
		return ""
	}
	keys := make([]string, len(s.projects))
	for i, p := range s.projects {
		keys[i] = p.Key
	}
	return "project:" + strings.Join(keys, ",")
}

// home is the one project new issues and pages go into.
func (s *scope) home() (string, error) {
	d := s.direct()
	switch {
	case len(d) == 1:
		return d[0].Key, nil
	case len(d) == 0:
		return "", errors.New("which project? Use -p PROJECT (wp project ls lists them), or run this in a git repo attached to one")
	}
	return "", fmt.Errorf("%s covers %s; pick one with -p", s.from, s.names())
}

// names lists the matched projects: "Harbor (HB)".
func (s *scope) names() string {
	var out []string
	for _, p := range s.direct() {
		out = append(out, fmt.Sprintf("%s (%s)", p.Name, p.Key))
	}
	return strings.Join(out, ", ")
}

// String describes the scope for messages: "Harbor (HB) and 2
// sub-projects, from the git repo robin/harbor".
func (s *scope) String() string {
	if s.all() {
		if s.from == "" || s.from == "--all" {
			return "every project"
		}
		return "every project (" + s.from + ")"
	}
	desc := s.names()
	if n := len(s.projects) - len(s.direct()); n > 0 {
		desc += fmt.Sprintf(" and %d sub-project%s", n, plural(n))
	}
	if s.from != "" {
		desc += ", from " + s.from
	}
	return desc
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// scopeFlags are -p/--project and -A/--all.
type scopeFlags struct {
	project *string
	all     *bool
}

func addScopeFlags(fs *flag.FlagSet) scopeFlags {
	f := scopeFlags{
		project: fs.String("project", "", "work in `PROJECT` (key, name or repo; wp help scope)"),
		all:     fs.Bool("all", false, "every project"),
	}
	short(fs, "p", "project")
	short(fs, "A", "all")
	return f
}

// scopeFor picks the scope from the flags, the environment, the git repo
// here and the default project, in that order.
func (c *cli) scopeFor(f scopeFlags) (*scope, error) {
	if f.all != nil && *f.all {
		return &scope{from: "--all"}, nil
	}
	if f.project != nil && *f.project != "" {
		return c.findScope(*f.project, "-p "+*f.project)
	}
	return c.implicitScope()
}

// implicitScope is the scope nobody asked for explicitly: the environment,
// the git repo here, the default project, or every project.
func (c *cli) implicitScope() (*scope, error) {
	if v := os.Getenv("WAYPOINT_PROJECT"); v != "" {
		return c.findScope(v, "WAYPOINT_PROJECT")
	}
	if repo, inRepo := gitRepoHere(); inRepo {
		if repo == "" {
			return nil, notAttachedError("this git repo has no remote naming an owner/name, so it can't be matched to a Waypoint project.\n" +
				"Use -p PROJECT to pick one (wp project ls lists them) or -A for every project")
		}
		s, err := c.repoScope(repo)
		if err != nil {
			return nil, err
		}
		if s.all() {
			return nil, notAttachedError(fmt.Sprintf("the git repo here (%s) isn't attached to a Waypoint project.\n"+
				"Use -p PROJECT to pick one (wp project ls lists them), -A for every project,\n"+
				"or attach the repo in the project's settings (Integrations)", repo))
		}
		s.from = "the git repo " + repo
		return s, nil
	}
	if c.cfg.Project != "" {
		return c.findScope(c.cfg.Project, "your default project (wp config)")
	}
	return &scope{}, nil
}

// notAttachedError says the git repo here belongs to no Waypoint project.
type notAttachedError string

func (e notAttachedError) Error() string { return string(e) }

var errNoProjectHere = errors.New("no project here: pass -p KEY, or -A for every project")

// workScope is scopeFor for the commands that find work (ls, next, batch,
// search): every project has to be asked for with -A, it is never what
// they fall back to.
func (c *cli) workScope(f scopeFlags) (*scope, error) {
	sc, err := c.scopeFor(f)
	if err == nil && sc.all() && sc.from != "--all" {
		return nil, errNoProjectHere
	}
	return sc, err
}

// homeScope is the project this session works in, whatever flags it passes:
// the environment, the git repo here or the default project. nil when
// there is none, so -p can't step outside it unnoticed.
func (c *cli) homeScope() (*scope, error) {
	sc, err := c.implicitScope()
	var na notAttachedError
	if errors.As(err, &na) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sc.all() {
		return nil, nil
	}
	return sc, nil
}

// covers says whether every project of o, which has some, is in the scope.
func (s *scope) covers(o *scope) bool {
	for _, p := range o.projects {
		if !s.contains(p.Key) {
			return false
		}
	}
	return len(o.projects) > 0
}

// findScope resolves a name the person gave: it must match.
func (c *cli) findScope(ref, from string) (*scope, error) {
	s, err := c.matchScope(ref)
	if err != nil {
		return nil, err
	}
	if s.all() {
		return nil, fmt.Errorf("no project or repo called %q (wp project ls lists them)", ref)
	}
	if err := s.ambiguous(ref); err != nil {
		return nil, err
	}
	s.from = from
	return s, nil
}

// ambiguous is an error when a name (not a repo) matched several projects.
// A repo attached to several projects legitimately covers them all.
func (s *scope) ambiguous(ref string) error {
	d := s.direct()
	if len(d) < 2 || d[0].MatchedBy == "repo" {
		return nil
	}
	var opts []string
	for _, p := range d {
		opts = append(opts, fmt.Sprintf("%s (%s)", p.Key, p.Name))
	}
	return fmt.Errorf("%q could be %s; use the key", ref, strings.Join(opts, " or "))
}

// matchScope asks the server which projects ref names (none is no error).
func (c *cli) matchScope(ref string) (*scope, error) {
	return c.lookupScope("match=" + url.QueryEscape(ref))
}

// repoScope is the projects a repo (owner/name) is attached to: names
// that merely look alike don't count.
func (c *cli) repoScope(repo string) (*scope, error) {
	return c.lookupScope("repo=" + url.QueryEscape(repo))
}

func (c *cli) lookupScope(query string) (*scope, error) {
	var ps []scopeProject
	if err := c.do("GET", "/api/v1/projects?"+query, nil, &ps); err != nil {
		return nil, err
	}
	// A server older than this wp ignores match and lists everything.
	for _, p := range ps {
		if p.MatchedBy == "" {
			return nil, errors.New("this Waypoint server is older than wp and can't match projects to repos; update the server, or use -A")
		}
	}
	return &scope{projects: ps}, nil
}

// note tells the person something on stderr, so stdout stays the answer.
// --json leaves it out.
func (c *cli) note(format string, a ...any) {
	if !c.asJSON {
		fmt.Fprintf(c.err, format+"\n", a...)
	}
}

// gitRepoHere is owner/name of the git repo in this directory (its origin
// remote, else its first remote). inRepo says whether this is a git work
// tree at all: one whose remote can't be read gives "" and true.
func gitRepoHere() (repo string, inRepo bool) {
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		if _, err := exec.LookPath("git"); err != nil {
			return "", false // no git: no repo to stay inside
		}
		// Not a work tree, or git refused to say (e.g. "dubious
		// ownership"): only the first is "not in a repo".
		if err == nil {
			return "", false
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "not a git repository") {
			return "", false
		}
		return "", true
	}
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		list, err := exec.Command("git", "remote").Output()
		if err != nil || len(strings.Fields(string(list))) == 0 {
			return "", true
		}
		// #nosec G204 -- a remote name git itself listed, as one argument, no shell
		if out, err = exec.Command("git", "remote", "get-url", strings.Fields(string(list))[0]).Output(); err != nil {
			return "", true
		}
	}
	repo, err = repoFromRemote(strings.TrimSpace(string(out)))
	if err != nil {
		return "", true
	}
	return repo, true
}

// contains says whether a project key is in the scope.
func (s *scope) contains(key string) bool {
	for _, p := range s.projects {
		if strings.EqualFold(p.Key, key) {
			return true
		}
	}
	return false
}
