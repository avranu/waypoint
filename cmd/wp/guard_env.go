package main

import (
	"fmt"
	"net"
	"slices"
	"strings"
)

// Only hosts the server listed at SessionStart count; they are kept in state.json, which agents cannot write, and anything but a plain read there still needs a person.

// allowedHostsText names what a fetch may reach, for refusals.
const allowedHostsText = "this machine, the Waypoint server, the repo's forge or one of the environments a person recorded for its project"

// envHost: hostPort (envHostPort) is one of the repo's environments'
// addresses' host and port.
func (g *guard) envHost(hostPort string) bool {
	if hostPort == "" || g.envHosts == nil {
		return false
	}
	return slices.Contains(g.envHosts(), hostPort)
}

// envHostPort is the host:port a tool reaches for raw, whose host is host,
// as the server lists them (app.example.com:443): the address's port, or
// its scheme's (http when it names none, https for httpie's https). ""
// when host is.
func envHostPort(name, raw, host string) string {
	if host == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		scheme, rest = "http", raw
		if name == "https" {
			scheme = "https"
		}
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.LastIndex(rest, "]"); i >= 0 {
		rest = rest[i+1:]
	}
	port := ""
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		port = rest[i+1:]
	}
	if port == "" {
		port = "80"
		if strings.EqualFold(scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(strings.ToLower(strings.Trim(host, "[]")), port)
}

// envHostsFor is the local copy of the environment hosts of the repo whose
// origin is remote; nil when it has none or isn't known.
func (s *State) envHostsFor(remote string) []string {
	if remote == "" || len(s.EnvHosts) == 0 {
		return nil
	}
	repo, err := repoFromRemote(remote)
	if err != nil {
		return nil
	}
	return s.EnvHosts[strings.ToLower(repo)]
}

// saveEnvHosts keeps what a SessionStart was told about repo's environment
// hosts. nil (an older server, or no repo) leaves the copy as it is.
func (c *cli) saveEnvHosts(repo string, hosts *[]string) {
	if repo == "" || hosts == nil {
		return
	}
	key := strings.ToLower(repo)
	var keep []string
	for _, h := range *hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && !slices.Contains(keep, h) {
			keep = append(keep, h)
		}
	}
	if slices.Equal(c.state.EnvHosts[key], keep) {
		return
	}
	_ = c.updateState(func(s *State) {
		if len(keep) == 0 {
			delete(s.EnvHosts, key)
			return
		}
		if s.EnvHosts == nil {
			s.EnvHosts = map[string][]string{}
		}
		s.EnvHosts[key] = keep
	})
}

// envReadWay is the way out of an env-change refusal (WP-1087): wp env read
// sends the GET with the credentials named, not written, so nothing the
// shell expands and no option the guard doesn't list.
const envReadWay = "Read it with `wp env read production /PATH --bearer VAR` (the environment's name from `wp env ls`; VAR names a variable in the shell or the repo's .env, never its value), or file `wp request run \"<the command>\" -d \"why\"` for a person to run it"

// envVouchWay is the way out of a download refusal for an address that may
// be the project's own deployment (WP-1087): a person vouches for it once,
// instead of running each read.
func envVouchWay(host, origin string) string {
	return fmt.Sprintf("If %s is where this repo's project runs, `wp env vouch production --url %s -d \"why\"` (with the environment's name: production, staging, …) asks a person once to let agents read it, and `wp env read production /PATH` reads it once they allow it; meanwhile carry on with other work. Otherwise file `wp request run \"<the command>\" -d \"why\"` for a person to run it; `wp release` the task if it can't go on without it", host, origin)
}

// envOrigin is scheme://host[:port] of an http(s) address a fetch tool
// was given (no scheme reads as https, except for httpie's http); "" for
// anything else.
func envOrigin(name, raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		scheme, rest = "https", raw
		if name == "http" {
			scheme = "http"
		}
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if rest == "" || strings.ContainsAny(rest, "$`'\"{}*? ") {
		return ""
	}
	return scheme + "://" + strings.ToLower(rest)
}
