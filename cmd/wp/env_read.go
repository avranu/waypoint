package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Credentials are named by variable and never printed; redirects stay on the environment's host; agents read only environments a person vouched for.

var envVouchCmd = &command{
	name: "vouch", args: "NAME [--url ADDRESS] [FLAGS]", aliases: []string{"allow"},
	summary: "let agents read from an environment (a person does it; an agent asks one)",
	about: `Unattended agent sessions may read (GET) from the addresses of the
project's environments a person set (WP-ADR90). Run by a person, it records
the addresses as theirs, creating the environment if missing. Run by an
agent, it files one question for a person ("Allow reads"), or finds the
open one; their answer records the same addresses. The address, health
and version flags replace those addresses; the rest are kept.
Then: wp env read NAME PATH.`,
	examples: []string{
		"wp env vouch production --url https://app.example.net --health https://app.example.net/healthz -d \"read the credits health row for APP-307\"",
		"wp env vouch production",
	},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		sf := addScopeFlags(fs)
		addr := fs.String("url", "", "the environment's `ADDRESS` (https://…)")
		health := fs.String("health", "", "its health check `ADDRESS`")
		version := fs.String("version", "", "its version `ADDRESS`")
		why := fs.String("d", "", "`WHY` agents need to read it (shown to the person asked)")
		issue := fs.String("issue", "", "the task `KEY` it is for")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("which environment? Give its name (wp env ls)")
		}
		project, err := c.envProject(sf)
		if err != nil {
			return err
		}
		body := map[string]any{"reason": *why, "issue": *issue}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "url":
				body["url"] = *addr
			case "health":
				body["health_url"] = *health
			case "version":
				body["version_url"] = *version
			}
		})
		raw, err := c.call("POST", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0])+"/vouch", body)
		if err != nil {
			return err
		}
		return c.emit(raw, func() string { return envVouchText(raw, strings.ToLower(pos[0])) })
	},
}

func envVouchText(raw []byte, name string) string {
	var r struct {
		Vouched     bool     `json:"vouched"`
		Hosts       []string `json:"hosts"`
		New         bool     `json:"new"`
		Environment *cliEnv  `json:"environment"`
		Request     *struct {
			Ref   string `json:"ref"`
			Title string `json:"title"`
			State string `json:"state"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return string(raw)
	}
	hosts := strings.Join(r.Hosts, ", ")
	switch {
	case r.Vouched:
		return fmt.Sprintf("%s: agents may read from %s (a person set its addresses).\nRead: wp env read %s /PATH", name, hosts, name)
	case r.Request != nil:
		lead := "Asked a person"
		if !r.New {
			lead = "Already asked, still open"
		}
		return fmt.Sprintf("%s: %s %s\nOnce they allow it, `wp env read %s /PATH` reads %s (and curl reads pass the guard from your next prompt). Carry on with other work meanwhile.",
			lead, r.Request.Ref, r.Request.Title, name, hosts)
	}
	return fmt.Sprintf("%s: agents can't read from it yet (%s)", name, hosts)
}

// envReadMax is what wp env read prints at most; -o takes envReadFileMax.
const (
	envReadMax       = 1 << 20
	envReadFileMax   = 64 << 20
	envReadRedirects = 5
)

var envReadCmd = &command{
	name: "read", args: "NAME [PATH|health|version|URL] [FLAGS]", aliases: []string{"get"},
	summary: "GET from an environment's own address, credentials by variable name",
	about: `One GET to the environment's address (PATH from its root, health, version,
or a full URL on one of its hosts); the body goes to stdout, an HTTP error
status to stderr with exit 1. Credentials are named, never written:
--bearer VAR sends "Authorization: Bearer $VAR" and -H Name=VAR sends a
header from VAR, each read from the shell or else the checkout's .env (the
main checkout's for a worktree) and blanked out of what is printed.
-H "Name: value" sends a literal header. Redirects only on the same host.
Only environments whose addresses a person vouched for (wp env vouch).`,
	examples: []string{
		"wp env read production /admin/api/health --bearer ADMIN_API_KEY",
		"wp env read production health",
		"wp env read production '/admin/api/calls?provider=anthropic-credits&limit=1000' --bearer ADMIN_API_KEY | jq '.[].completion_tokens'",
	},
	run: envRead,
}

func envRead(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	bearer := fs.String("bearer", "", "send `VAR`'s value as a bearer token")
	var headers listFlag
	fs.Var(&headers, "H", "`Name=VAR` (a header from a variable) or \"Name: value\" (repeatable)")
	include := fs.Bool("i", false, "print the status line and response headers first")
	out := fs.String("o", "", "write the body to `FILE` (up to 64 MB) instead of stdout")
	secs := fs.Int("m", 30, "give up after `SECONDS`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return c.usage("which environment, and what to read? wp env read NAME /PATH")
	}
	project, err := c.envProject(sf)
	if err != nil {
		return err
	}
	if err := c.envReadHere(project); err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/projects/"+esc(project)+"/environments/"+esc(pos[0]), nil)
	if err != nil {
		return err
	}
	var e cliEnv
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	if e.Stopped {
		return fmt.Errorf("%s/%s is stopped", project, e.Name)
	}
	if e.AddressAgent != "" {
		return fmt.Errorf("%s/%s's addresses were set by %s, not a person, so wp env read doesn't send anything there; `wp env vouch %s -d \"why\"` vouches for them (run by a person) or asks a person to (run by an agent)", project, e.Name, e.AddressAgent, e.Name)
	}
	what := ""
	if len(pos) == 2 {
		what = pos[1]
	}
	target, err := envReadURL(&e, what)
	if err != nil {
		return err
	}
	req, secrets, err := envReadRequest(target, *bearer, headers, os.Getenv, envFiles())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(max(*secs, 1))*time.Second)
	defer cancel()
	resp, err := envReadClient(envHosts(&e)).Do(req.WithContext(ctx))
	if err != nil {
		return errors.New(blankSecrets(err.Error(), secrets))
	}
	defer resp.Body.Close()
	limit, w := int64(envReadMax), c.out
	var file *os.File
	if *out != "" {
		f, err := os.Create(*out) // #nosec G304 -- the path the person or agent named
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		file, w, limit = f, f, envReadFileMax
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	cut := len(body) > int(limit)
	if cut {
		body = body[:limit]
	}
	if *include {
		var h strings.Builder
		fmt.Fprintf(&h, "%s %s\n", resp.Proto, resp.Status)
		keys := make([]string, 0, len(resp.Header))
		for k := range resp.Header {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			for _, v := range resp.Header[k] {
				fmt.Fprintf(&h, "%s: %s\n", k, v)
			}
		}
		_, _ = io.WriteString(c.out, blankSecrets(h.String(), secrets)+"\n")
	}
	if _, err := io.WriteString(w, blankSecrets(string(body), secrets)); err != nil {
		return err
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return err
		}
	}
	if cut {
		fmt.Fprintf(c.err, "wp: stopped after %d bytes; narrow the read (a query, a limit) or use -o FILE\n", limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(c.err, "wp: HTTP %s from %s\n", resp.Status, resp.Request.URL.Redacted())
		return exitCode(1)
	}
	return nil
}

// envReadHere keeps an agent's reads to the projects of the git repo it
// works in, as the guard's env_hosts are (WP-ADR90): a person vouched for
// a project's environment for that project's agents, so -p or
// WAYPOINT_PROJECT naming another project is refused in an agent's shell.
// A person runs it for any project they can see.
func (c *cli) envReadHere(project string) error {
	agent := c.agentHeader()
	if agent == "" {
		return nil
	}
	repo, inRepo := gitRepoHere()
	if inRepo && repo != "" {
		if sc, err := c.repoScope(repo); err == nil && !sc.all() {
			for _, p := range sc.direct() { // Same scope as the guard's env_hosts: only the repo's own projects.
				if strings.EqualFold(p.Key, project) {
					return nil
				}
			}
		}
	}
	where := "outside a git repo attached to it"
	if repo != "" {
		where = "in " + repo + ", which isn't attached to it"
	}
	return fmt.Errorf("an agent (%s) reads only the environments of the project its repo is attached to, and this runs %s; run it from %s's checkout, or a person runs it with -p %s", agent, where, project, project)
}

// envHosts are the host:port of the environment's addresses.
func envHosts(e *cliEnv) []string {
	var out []string
	for _, a := range []string{e.URL, e.HealthURL, e.VersionURL} {
		if h := urlHostPort(a); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// urlHostPort is an http(s) address's host:port, the scheme's port when it
// names none; "" for anything else.
func urlHostPort(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	port := u.Port()
	switch strings.ToLower(u.Scheme) {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return ""
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// envReadURL is what wp env read fetches: the address itself (nothing
// given), its health or version address, a PATH from its root, or a full
// address on one of the environment's hosts.
func envReadURL(e *cliEnv, what string) (*url.URL, error) {
	if e.URL == "" {
		return nil, fmt.Errorf("%s has no address; wp env vouch %s --url https://… records one", e.Name, e.Name)
	}
	pick := e.URL
	switch {
	case what == "":
	case what == "health" || what == "version":
		pick = e.HealthURL
		if what == "version" {
			pick = e.VersionURL
		}
		if pick == "" {
			return nil, fmt.Errorf("%s has no %s address", e.Name, what)
		}
	case strings.HasPrefix(what, "/"):
		base, err := url.Parse(e.URL)
		if err != nil {
			return nil, err
		}
		ref, err := url.Parse(what)
		if err != nil || ref.Host != "" || ref.Scheme != "" {
			return nil, fmt.Errorf("%q isn't a path", what)
		}
		base.Path, base.RawPath, base.RawQuery, base.Fragment = ref.Path, ref.RawPath, ref.RawQuery, ""
		pick = base.String()
	case strings.Contains(what, "://"):
		pick = what
	default:
		return nil, fmt.Errorf("%q isn't a path (start it with /), health, version or a full address", what)
	}
	u, err := url.Parse(pick)
	if err != nil {
		return nil, err
	}
	hp := urlHostPort(pick)
	if hp == "" {
		return nil, fmt.Errorf("%q isn't an http:// or https:// address", pick)
	}
	if !slices.Contains(envHosts(e), hp) {
		return nil, fmt.Errorf("%s isn't one of %s's addresses (%s)", hp, e.Name, strings.Join(envHosts(e), ", "))
	}
	return u, nil
}

var envVarName = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envReadRequest builds the GET with its headers; secrets are the values
// read from variables, to blank out of what is printed.
func envReadRequest(u *url.URL, bearer string, headers []string, getenv func(string) string, files []string) (*http.Request, []string, error) {
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "waypoint-env-read")
	var secrets []string
	lookup := func(name string) (string, error) {
		if !envVarName.MatchString(name) {
			return "", errors.New("that isn't a variable name: give the name of the variable holding the credential, never its value")
		}
		v := getenv(name)
		if v == "" {
			v = dotenvValue(files, name)
		}
		if v == "" {
			return "", fmt.Errorf("%s isn't set in the shell or in %s", name, strings.Join(files, " or "))
		}
		secrets = append(secrets, v)
		return v, nil
	}
	if bearer != "" {
		v, err := lookup(bearer)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+v)
	}
	for _, h := range headers {
		// Some servers honor method-override headers and turn a GET into a write, so any header naming a method is refused.
		if name, val, ok := strings.Cut(h, ":"); ok {
			if methodHeader(strings.TrimSpace(name)) {
				return nil, nil, errors.New("wp env read only reads (GET): no header naming a method")
			}
			req.Header.Add(strings.TrimSpace(name), strings.TrimSpace(val))
			continue
		}
		name, v, ok := strings.Cut(h, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("-H %q: give Name=VAR or \"Name: value\"", h)
		}
		if methodHeader(strings.TrimSpace(name)) {
			return nil, nil, errors.New("wp env read only reads (GET): no header naming a method")
		}
		val, err := lookup(strings.TrimSpace(v))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Add(strings.TrimSpace(name), val)
	}
	return req, secrets, nil
}

// envReadClient fetches directly (no proxy from the environment) and
// follows redirects only to the environment's hosts, so credentials go
// nowhere else.
func envReadClient(hosts []string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= envReadRedirects {
			return fmt.Errorf("stopped after %d redirects", envReadRedirects)
		}
		if prev := via[len(via)-1]; prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("not following a redirect from https to %s: credentials would travel in the clear", req.URL.Redacted())
		}
		if hp := urlHostPort(req.URL.String()); hp == "" || !slices.Contains(hosts, hp) {
			return fmt.Errorf("not following a redirect to %s, which isn't one of the environment's addresses", req.URL.Redacted())
		}
		return nil
	}}
}

// envFiles are the .env files a variable may come from: the checkout's,
// then the main checkout's when this is a worktree (an untracked .env
// isn't copied into worktrees).
func envFiles() []string {
	var out []string
	if top, err := gitIn(".", "rev-parse", "--show-toplevel"); err == nil && top != "" {
		out = append(out, filepath.Join(top, ".env"))
	}
	if root, err := repoRoot("."); err == nil && root != "" {
		if f := filepath.Join(root, ".env"); !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// dotenvValue is name's value in the first of files that sets it: NAME=value
// lines, optionally after export, the value optionally quoted.
func dotenvValue(files []string, name string) string {
	for _, path := range files {
		f, err := os.Open(path) // #nosec G304 -- the checkout's own .env
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		val, found := "", false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != name {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			} else if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			val, found = v, true
		}
		_ = f.Close()
		if found && val != "" {
			return val
		}
	}
	return ""
}

// blankSecrets blanks the secrets out of s.
func blankSecrets(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}
