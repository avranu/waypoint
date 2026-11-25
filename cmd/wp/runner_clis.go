package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// runnerCLI is a coding-agent CLI found on this machine and whether it is
// signed in, as reported to the server.
type runnerCLI struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	SignedIn *bool  `json:"signed_in"`
	Detail   string `json:"detail,omitempty"`
}

// cliBinaries maps the server's names for the CLIs to their programs.
var cliBinaries = map[string]string{"claude": "claude", "codex": "codex", "opencode": "opencode", "cursor": "cursor-agent"}

var cliOrder = []string{"claude", "codex", "opencode", "cursor"}

var (
	ansiRE    = lazyre.New(`\x1b\[[0-9;?]*[A-Za-z]`)
	versionRE = lazyre.New(`\d+\.\d+(\.\d+)?[\w.+-]*`)
	credsRE   = lazyre.New(`(\d+) credentials?`)
)

// cliProbeTimeout is how long a CLI's status command may take.
var cliProbeTimeout = 5 * time.Second

// cliProbe runs a CLI's status command; the API keys a job wouldn't see
// aren't seen here either, so "signed in" means the subscription.
func cliProbe(ctx context.Context, bin string, args ...string) (out string, code int) {
	ctx, cancel := context.WithTimeout(ctx, cliProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- fixed status commands of the CLIs we drive
	cmd.Env = jobEnv(os.Environ(), nil, false)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	out = strings.TrimSpace(ansiRE.ReplaceAllString(b.String(), ""))
	if err == nil {
		return out, 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out, ee.ExitCode()
	}
	return out, -1
}

// probeKilled is cliProbe's code for a status command that didn't finish:
// it timed out (a loaded machine can take longer than 5 seconds to start a
// CLI) or couldn't run.
const probeKilled = -1

// probeUnfinished is the detail of a CLI whose sign-in check didn't finish:
// its sign-in is unknown, not signed out. A runner keeps what it last knew
// (keepKnownSignIn), so a slow check doesn't take it off the work that
// needs that CLI (WP-501).
const probeUnfinished = "the sign-in check didn't finish"

// keepKnownSignIn keeps, for each CLI whose checks didn't finish this time,
// what the last detection knew: its sign-in and version.
func keepKnownSignIn(prev, next []runnerCLI) []runnerCLI {
	out := slices.Clone(next)
	for i, c := range out {
		j := slices.IndexFunc(prev, func(p runnerCLI) bool { return p.Name == c.Name })
		if j < 0 {
			continue
		}
		if c.Detail == probeUnfinished {
			out[i].SignedIn, out[i].Detail = prev[j].SignedIn, prev[j].Detail
		}
		if c.Version == "" {
			out[i].Version = prev[j].Version
		}
	}
	return out
}

// signInChanges says, a line per CLI, how its sign-in changed between two
// detections, for the runner's log.
func signInChanges(prev, next []runnerCLI) []string {
	state := func(c runnerCLI) string {
		switch {
		case c.SignedIn == nil:
			return "sign-in unknown"
		case *c.SignedIn:
			return "signed in"
		}
		return "signed out"
	}
	var out []string
	for _, c := range next {
		j := slices.IndexFunc(prev, func(p runnerCLI) bool { return p.Name == c.Name })
		was := "not found"
		if j >= 0 {
			was = state(prev[j])
		}
		if now := state(c); now != was {
			line := c.Name + ": " + now + " (was " + was + ")"
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			out = append(out, line)
		}
	}
	for _, p := range prev {
		if !slices.ContainsFunc(next, func(c runnerCLI) bool { return c.Name == p.Name }) {
			out = append(out, p.Name+": not found any more")
		}
	}
	return out
}

func statusLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func boolp(b bool) *bool { return &b }

// detectCLIsFn is detectCLIs, swapped in tests.
var detectCLIsFn = detectCLIs

// detectCLIs looks for each agent CLI and asks it whether it is signed in
// (5 seconds each).
func detectCLIs(ctx context.Context) []runnerCLI {
	out := []runnerCLI{}
	for _, name := range cliOrder {
		bin := cliBinaries[name]
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		c := runnerCLI{Name: name}
		if v, _ := cliProbe(ctx, path, "--version"); v != "" {
			c.Version = versionRE.FindString(statusLine(v))
		}
		switch name {
		case "claude":
			o, code := cliProbe(ctx, path, "auth", "status")
			var st struct {
				LoggedIn   bool   `json:"loggedIn"`
				AuthMethod string `json:"authMethod"`
				Email      string `json:"email"`
			}
			if json.Unmarshal([]byte(o), &st) == nil && (st.AuthMethod != "" || st.LoggedIn) {
				c.SignedIn = boolp(st.LoggedIn)
				if st.LoggedIn {
					c.Detail = st.AuthMethod
				}
			} else if code == probeKilled {
				c.Detail = probeUnfinished
			} else {
				c.SignedIn, c.Detail = boolp(code == 0), statusLine(o)
			}
		case "codex":
			o, code := cliProbe(ctx, path, "login", "status")
			if code == probeKilled {
				c.Detail = probeUnfinished
				break
			}
			c.SignedIn, c.Detail = boolp(code == 0), statusLine(o)
		case "cursor":
			c.SignedIn, c.Detail = cursorStatus(ctx, path)
		case "opencode":
			o, _ := cliProbe(ctx, path, "auth", "list")
			if m := credsRE.FindStringSubmatch(o); m != nil && m[1] != "0" {
				c.SignedIn, c.Detail = boolp(true), m[1]+" credentials saved"
			} else {
				c.Detail = "no saved credentials (free models only)"
			}
		}
		out = append(out, c)
	}
	return out
}

// cursorStatus asks cursor-agent whether it is signed in and, when it is,
// which plan the account is on ("Pro+ plan, robin@example.com"): the plan
// decides how much Grok and the other Cursor models a month or week holds.
// Older versions print only text, which is read as before.
func cursorStatus(ctx context.Context, path string) (*bool, string) {
	o, jcode := cliProbe(ctx, path, "status", "--format", "json")
	if jcode == probeKilled {
		return nil, probeUnfinished
	}
	var st struct {
		Status          string `json:"status"`
		IsAuthenticated bool   `json:"isAuthenticated"`
		Message         string `json:"message"`
		UserInfo        struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	}
	if json.Unmarshal([]byte(o), &st) != nil || st.Status == "" {
		o, code := cliProbe(ctx, path, "status")
		low := strings.ToLower(o)
		switch {
		case code == probeKilled:
			return nil, probeUnfinished
		case strings.Contains(low, "not logged in") || strings.Contains(low, "not authenticated") || strings.Contains(low, "login required"):
			return boolp(false), statusLine(o)
		case strings.Contains(low, "logged in") || strings.Contains(low, "authenticated"):
			return boolp(true), statusLine(o)
		case code != 0:
			return boolp(false), statusLine(o)
		}
		return nil, statusLine(o)
	}
	if !st.IsAuthenticated {
		return boolp(false), cmpStr(st.Message, "Not logged in")
	}
	detail := st.UserInfo.Email
	var about struct {
		Tier string `json:"subscriptionTier"`
	}
	if a, _ := cliProbe(ctx, path, "about", "--format", "json"); json.Unmarshal([]byte(a), &about) == nil && about.Tier != "" {
		detail = strings.TrimSuffix(strings.Join([]string{about.Tier + " plan", detail}, ", "), ", ")
	}
	return boolp(true), detail
}
