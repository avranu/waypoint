package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

// apiError is a non-2xx answer from the server to method path.
type apiError struct {
	status       int
	msg          string
	code         string // the server's machine-readable reason, when it gives one
	method, path string
	// body and ctype are the answer as it came (body cut at 4 KiB), for a
	// caller that knows an older server's wording when it isn't {"error":...}.
	body, ctype string
}

func (e *apiError) Error() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "not logged in (or the token was revoked): run wp auth login"
	case http.StatusForbidden:
		if e.msg != "" {
			return e.msg
		}
		return "not allowed"
	}
	if e.status >= 500 && (e.msg == "" || e.msg == "internal error") {
		// The server keeps a bug's details to its log: say where to look
		// there (WP-265).
		return fmt.Sprintf("the server failed with HTTP %d on %s %s; its log has the cause", e.status, e.method, e.path)
	}
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// transient says the server, or what it stands in front of (the forge),
// didn't answer this time: 502, 503 or 504.
func (e *apiError) transient() bool {
	switch e.status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// call makes an API request. body is JSON-encoded unless nil; the response
// body is returned raw.
func (c *cli) call(method, path string, body any) ([]byte, error) {
	return c.callCtx(context.Background(), method, path, body)
}

// callCtx is call that stops when ctx ends (the runner's long polls).
func (c *cli) callCtx(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	return c.send(ctx, method, path, ct, rd)
}

// callRaw is call with a body sent as it is (a file), not as JSON.
func (c *cli) callRaw(method, path, contentType string, body io.Reader) ([]byte, error) {
	return c.send(context.Background(), method, path, contentType, body)
}

// newRequest builds a request to the server with every header wp sends: its
// client name, the token, the agent and agent-env headers and the session's.
// Every call goes through it (send, and streamGet for what is streamed), so a
// header added here reaches them all.
func (c *cli) newRequest(ctx context.Context, method, path, contentType string, rd io.Reader) (*http.Request, error) {
	server := c.server()
	if server == "" {
		return nil, errors.New("no server set: wp config server https://your-waypoint")
	}
	// #nosec G704 -- the server is the one the user configured
	req, err := http.NewRequestWithContext(ctx, method, server+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Waypoint-Client", "wp")
	req.Header.Set("User-Agent", "wp/"+version)
	if rd != nil {
		req.Header.Set("Content-Type", contentType)
	}
	// Logging in needs no credentials, and the old token may belong to
	// another server, so none are sent.
	if !strings.HasPrefix(path, "/api/v1/cli/login") {
		if t := c.token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		if a := c.agentHeader(); a != "" {
			req.Header.Set("X-Waypoint-Agent", a)
		}
		// In a coding agent's shell, say so even when told we're a person
		// (WAYPOINT_AGENT=none, or a config set for your own terminal):
		// what we print lands in the agent's context, so the server keeps
		// drafts out of it (WP-42).
		if env := detectAgent(os.Getenv); env != "" {
			req.Header.Set("X-Waypoint-Agent-Env", env)
		}
	}
	c.addSessionHeaders(req)
	return req, nil
}

func (c *cli) send(ctx context.Context, method, path, contentType string, rd io.Reader) ([]byte, error) {
	server := c.server()
	if server == "" {
		return nil, errors.New("no server set: wp config server https://your-waypoint")
	}
	req, err := c.newRequest(ctx, method, path, contentType, rd)
	if err != nil {
		return nil, err
	}
	hc := httpClient
	switch {
	case c.hookTimeout > 0:
		hc = &http.Client{Timeout: c.hookTimeout}
	case c.longPoll > 0:
		hc = &http.Client{Timeout: c.longPoll}
	}
	resp, err := hc.Do(req) // #nosec G704 -- the configured server
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %w", server, err)
	}
	defer resp.Body.Close()
	c.seeServerVersion(resp.Header.Get("X-Waypoint-Version"))
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, &apiError{status: resp.StatusCode, msg: e.Error, code: e.Code, method: method, path: strings.SplitN(path, "?", 2)[0],
			body: string(b[:min(len(b), 4096)]), ctype: resp.Header.Get("Content-Type")}
	}
	return b, nil
}

// get/post/... decode a JSON answer into out (when non-nil).
func (c *cli) do(method, path string, body, out any) error {
	b, err := c.call(method, path, body)
	if err != nil {
		return err
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// emit prints an answer: raw JSON (indented) with --json, else text.
func (c *cli) emit(raw []byte, text func() string) error {
	if c.asJSON {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			_, err := c.out.Write(raw)
			return err
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		c.println(string(b))
		return nil
	}
	s := text()
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, err := io.WriteString(c.out, s)
	return err
}

func esc(s string) string { return url.PathEscape(s) }

// ---- flags ----

// flags returns a FlagSet with the options every command takes.
func (c *cli) flags() *flag.FlagSet {
	name := "wp"
	if c.cur != nil {
		name = c.cur.path()
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&c.asJSON, "json", false, "print the API's JSON")
	fs.StringVar(&c.agent, "agent", c.agent, agentFlagUsage)
	return fs
}

// parse reads flags wherever they appear among the arguments (Go's flag
// package stops at the first positional one) and returns the positionals.
// -h prints the command's help, flags included.
func (c *cli) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	c.fs = fs
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				if c.cur != nil {
					c.commandHelp(c.cur, fs)
				}
				return nil, errHelpShown
			}
			return nil, c.usage("%s", err.Error())
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// short gives a flag a second name shown with it in help (-p, --project).
func short(fs *flag.FlagSet, name, long string) {
	f := fs.Lookup(long)
	fs.Var(f.Value, name, f.Usage)
}

// alias registers another name for a flag that help leaves out (old names).
func alias(fs *flag.FlagSet, name, existing string) {
	fs.Var(fs.Lookup(existing).Value, name, "(hidden) alias")
}

// listFlag is a repeatable string flag (--tag a --tag b, or --tag a,b).
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// readBody reads --body-file: a path, or "-" for stdin.
func readBody(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the user named the file
	return string(b), err
}

// openBrowser tries to open a URL; it's fine if it can't. Only http(s)
// URLs are opened (the login URL comes from the server).
func openBrowser(u string) error {
	if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return errors.New("not a web URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u) // #nosec G204 G702 -- a checked http(s) URL as a single argument, no shell
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) // #nosec G204 G702 -- as above
	default:
		cmd = exec.Command("xdg-open", u) // #nosec G204 G702 -- as above
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Start()
}

// println and print write to stdout; a failed terminal write has nowhere
// better to be reported.
func (c *cli) println(a ...any) { _, _ = fmt.Fprintln(c.out, a...) }
func (c *cli) print(a ...any)   { _, _ = fmt.Fprint(c.out, a...) }

// agentFlagUsage is the global --agent flag's text; a command that gives it
// its own (wp queue) gets it listed in its help.
const agentFlagUsage = "act as this agent (provider/model)"
