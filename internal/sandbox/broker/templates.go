package broker

import (
	"sort"
	"strings"
)

// the server, not this file, picks a session's templates and adds a person's exceptions.

var readOnly = []string{"GET", "HEAD"}

func getAnywhere(template string, hosts ...string) []Rule {
	rules := make([]Rule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, Rule{Host: h, Template: template})
	}
	return rules
}

// Anthropic lets Claude Code reach api.anthropic.com and nothing else of
// Anthropic's. The connection is tunnelled, not inspected: Claude Code
// signs in with its own setup-token, and nothing may intermediate it
// (R-151, WP-419).
func Anthropic() []Rule {
	return []Rule{{Host: "api.anthropic.com", Mode: Tunnel, Template: "anthropic"}}
}

// Go allows the module proxy for the given module path prefixes (the
// repo's go.mod requirements, and any a person adds) and the toolchain,
// the checksum database's lookups of the same, the vulnerability database
// for govulncheck, and toolchain downloads.
func Go(modules []string) []Rule {
	mods := append([]string{}, modules...)
	sort.Strings(mods)
	if len(mods) == 0 {
		mods = []string{ToolchainModule}
	}
	return []Rule{
		{Host: "proxy.golang.org", Kind: KindGoProxy, Modules: mods, Template: "go"},
		{Host: "sum.golang.org", Kind: KindGoSum, Modules: mods, Template: "go"},
		{Host: "vuln.go.dev", Template: "go"},
		{Host: "storage.googleapis.com", Routes: []Route{{Methods: readOnly, Path: "/golang/**"}}, Template: "go"},
	}
}

// NPM is the npm registry, read-only: nothing can be published.
func NPM() []Rule { return getAnywhere("npm", "registry.npmjs.org") }

// PyPI is the Python package index, read-only.
func PyPI() []Rule { return getAnywhere("pypi", "pypi.org", "files.pythonhosted.org") }

// DockerHub pulls images; pushes are refused (GET and HEAD only).
func DockerHub() []Rule {
	return getAnywhere("dockerhub", "registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com")
}

// Ubuntu is the Ubuntu archive, which apt reaches over plain HTTP.
func Ubuntu() []Rule {
	var rules []Rule
	for _, h := range []string{"archive.ubuntu.com", "security.ubuntu.com"} {
		rules = append(rules, Rule{Host: h, HTTP: true, Template: "ubuntu"}, Rule{Host: h, Template: "ubuntu"})
	}
	return rules
}

// Playwright is where Playwright downloads its browsers.
func Playwright() []Rule {
	return getAnywhere("playwright", "cdn.playwright.dev", "playwright.download.prss.microsoft.com",
		"playwright.azureedge.net", "playwright-akamai.azureedge.net", "playwright-verizon.azureedge.net")
}

// GitHub is read-only: GET and HEAD, plus git fetches (upload-pack).
// Pushes (receive-pack) and every other POST are refused.
func GitHub() []Rule {
	gitFetch := []Route{
		{Methods: readOnly, Path: "/**"},
		{Methods: []string{"POST"}, Path: "/*/*/git-upload-pack"},
	}
	return append([]Rule{{Host: "github.com", Routes: gitFetch, Template: "github"}},
		getAnywhere("github", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
			"raw.githubusercontent.com", "release-assets.githubusercontent.com")...)
}

// ForgeConfig is the session's forge.
type ForgeConfig struct {
	Host string
	// Pin is the forge's LAN address(es).
	Pin []string
	// Repos are "owner/name", the session's repositories.
	Repos []string
	// Cred names the read credential the broker writes into fetches.
	Cred string
	// Push sends the repositories' git pushes to the broker's push gateway
	// (Config.Gateway), which checks them and forwards them with the
	// repo's deploy key. Without it, pushes are refused.
	Push bool
}

// Forge allows git fetches of the session's repositories and nothing else
// on the forge host, its API included: agents reach the forge through
// Waypoint. Pushes, with Push, go to the push gateway (WP-469), never
// straight to the forge.
func Forge(c ForgeConfig) []Rule {
	var routes []Route
	for _, repo := range c.Repos {
		repo = strings.Trim(repo, "/")
		for _, p := range []string{"/" + repo, "/" + repo + ".git"} {
			routes = append(routes,
				Route{Methods: []string{"GET"}, Path: p + "/info/refs", Query: "service=git-upload-pack"},
				Route{Methods: []string{"POST"}, Path: p + "/git-upload-pack"})
			if c.Push {
				routes = append(routes,
					Route{Methods: []string{"GET"}, Path: p + "/info/refs", Query: "service=git-receive-pack", Gateway: true},
					Route{Methods: []string{"POST"}, Path: p + "/git-receive-pack", Gateway: true})
			}
		}
	}
	if len(routes) == 0 {
		return nil
	}
	return []Rule{{Host: c.Host, Routes: routes, Cred: c.Cred, Pin: c.Pin, Template: "forge"}}
}

// WaypointConfig is the Waypoint server the session's wp talks to.
type WaypointConfig struct {
	Host string
	Pin  []string
	// Cred names the session's own agent token (WP-418); the sandbox holds
	// only a placeholder.
	Cred string
}

// waypointAreas are the API areas the agent-facing wp commands use.
// Everything else (sign-in, tokens, invites, admin, runners, the sandbox's
// own settings, the CLI download and device login) is refused here as well
// as by the token's scope.
var waypointAreas = []string{
	"agent", "agents", "attachments", "audits", "automation-runs", "automations",
	"branches", "changes", "ci", "comments", "design", "environments", "findings",
	"glossary", "help", "issues", "kit", "knowledge", "labels", "links", "otlp",
	"pages", "prep", "projects", "pulls", "refs", "requests", "resolve", "review",
	"review-comments", "search", "sessions", "spec", "sprints", "statuses", "tags",
	"time", "timer", "usage", "views", "waiting", "workflows",
}

var anyMethod = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

// Waypoint allows the agent-facing API and writes the session's token into
// every request.
func Waypoint(c WaypointConfig) []Rule {
	routes := []Route{{Methods: []string{"GET"}, Path: "/api/v1/me"}}
	for _, a := range waypointAreas {
		routes = append(routes,
			Route{Methods: anyMethod, Path: "/api/v1/" + a},
			Route{Methods: anyMethod, Path: "/api/v1/" + a + "/**"})
	}
	deny := []Route{
		{Methods: anyMethod, Path: "/api/v1/projects/*/sandbox"},
		{Methods: anyMethod, Path: "/api/v1/projects/*/sandbox/**"},
		{Methods: anyMethod, Path: "/api/v1/projects/*/members/**"},
		{Methods: anyMethod, Path: "/api/v1/issues/*/shares/**"},
	}
	return []Rule{{Host: c.Host, Routes: routes, Deny: deny, Cred: c.Cred, Pin: c.Pin, Template: "waypoint"}}
}
