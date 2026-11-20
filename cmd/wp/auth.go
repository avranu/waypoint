package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// pollWait is how long login polling waits between tries (tests shorten it).
var pollWait = func(seconds int) { time.Sleep(time.Duration(max(seconds, 1)) * time.Second) }

var authCmd = &command{
	name: "auth", group: "Setup", args: "SUBCOMMAND",
	aliases: []string{"login"},
	summary: "log in to a Waypoint server, check who you are, or log out",
	about: `wp auth login shows a code to approve in Waypoint, then saves a token
for this machine. With --agent provider/model the token is that agent's.
WAYPOINT_SERVER, WAYPOINT_TOKEN and WAYPOINT_AGENT override saved settings.`,
	examples: []string{
		"wp auth login https://waypoint.example.com",
		"wp auth login --agent anthropic/claude-opus-5-5",
		"wp auth status",
	},
}

func init() {
	authCmd.subs = []*command{
		{name: "login", args: "[SERVER] [FLAGS]", summary: "log in by approving a code in the browser",
			examples: []string{"wp auth login https://waypoint.example.com", "wp auth login --agent anthropic/claude-opus-5-5 --no-browser"}, run: authLogin},
		{name: "status", args: "", aliases: []string{"whoami"}, summary: "show the server and who you're logged in as",
			examples: []string{"wp auth status"}, run: authStatus},
		{name: "logout", args: "", summary: "forget the token on this machine",
			examples: []string{"wp auth logout"}, run: authLogout},
		{name: "token", args: "wpt_TOKEN", summary: "save a token from Settings → API tokens",
			examples: []string{"wp auth token wpt_abc123 --agent anthropic/claude-opus-5-5"}, run: authToken},
	}
}

// authLogin is the device flow: show a code, the person approves it in the
// browser, and polling picks up a token made for this machine.
func authLogin(c *cli, args []string) error {
	fs := c.flags()
	name := fs.String("name", "", "`NAME` of this machine's token (default: host name)")
	server := fs.String("server", "", "server `URL` (or the argument)")
	noBrowser := fs.Bool("no-browser", false, "only print the approval link")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 1 && *server == "" && strings.HasPrefix(pos[0], "http") {
		*server = pos[0]
	}
	if *server != "" {
		if err := c.setConfig("server", *server); err != nil {
			return err
		}
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	var start struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		VerifyURL  string `json:"verification_url"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
	}
	// Inside a coding agent's shell, the token becomes an agent token.
	agent := c.agentHeader()
	if err := c.do("POST", "/api/v1/cli/login", map[string]string{"name": *name, "agent": agent}, &start); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Approve this login in Waypoint:\n\n  %s\n\n  code: %s\n\n", start.VerifyURL, start.UserCode)
	if !*noBrowser {
		_ = openBrowser(start.VerifyURL)
	}
	c.print("Waiting for approval…")
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		pollWait(start.Interval)
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		err := c.do("POST", "/api/v1/cli/login/poll", map[string]string{"device_code": start.DeviceCode}, &poll)
		if err != nil {
			c.println()
			return err
		}
		if poll.Status == "approved" && poll.Token != "" {
			c.keepServer()
			c.cfg.Token = poll.Token
			if agent != "" {
				c.cfg.Agent = agent
			}
			if err := c.saveConfig(); err != nil {
				return err
			}
			c.println(" approved.")
			return authStatus(c, nil)
		}
	}
	c.println()
	return errors.New("the code expired before it was approved; run wp auth login again")
}

// keepServer saves the server a token is about to be saved for, which
// WAYPOINT_SERVER may have changed for this run: a token is only ever
// stored next to the server that issued it.
func (c *cli) keepServer() {
	if srv := c.server(); srv != strings.TrimRight(c.cfg.Server, "/") {
		c.cfg.Server = srv
		c.println("Saved", srv, "as the server (from WAYPOINT_SERVER).")
	}
}

func authStatus(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	if c.server() == "" {
		return errors.New("no server set: wp config server https://your-waypoint")
	}
	if c.token() == "" {
		return errors.New("not logged in to " + c.server() + ": run wp auth login")
	}
	raw, err := c.call("GET", "/api/v1/me", nil)
	if err != nil {
		return err
	}
	var me struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
	}
	return c.emit(raw, func() string {
		if err := jsonInto(raw, &me); err != nil {
			return "logged in"
		}
		s := fmt.Sprintf("Logged in to %s as %s (@%s)", c.server(), me.DisplayName, me.Username)
		if a := c.agentHeader(); a != "" {
			s += ", acting as agent " + a
		}
		return s
	})
}

func authLogout(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	c.cfg.Token = ""
	if err := c.saveConfig(); err != nil {
		return err
	}
	c.println("Forgot the token on this machine (revoke it under Settings → Tokens).")
	return nil
}

func authToken(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.HasPrefix(pos[0], "wpt_") {
		return c.usage("expected a token starting with wpt_")
	}
	c.keepServer()
	c.cfg.Token = pos[0]
	if c.agent != "" {
		c.cfg.Agent = c.agent
	}
	if err := c.saveConfig(); err != nil {
		return err
	}
	return authStatus(c, nil)
}
