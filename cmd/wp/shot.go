package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// wp shot (WP-245): screenshots of a finished change, for the person who
// checks it on the Catch up page. One command opens each page in a headless
// browser at a desktop and a phone width, attaches the pictures to the task
// as review screenshots, and records the page as the task's try link. The
// browser is Playwright's: from the checkout's node_modules when it has
// one, else a copy wp installs once in its cache.

// shotPlaywright is the Playwright wp installs when no checkout has one.
const shotPlaywright = "1.62.1"

//go:embed shot.mjs
var shotScript string

var shotCmd = &command{
	name: "shot", group: "Tasks", args: "[KEY] URL|/PATH... [FLAGS]",
	aliases: []string{"screenshot"},
	summary: "screenshot pages of your change (desktop + phone)",
	about: `Attaches desktop (1280px) and phone (390px) screenshots to the task for
whoever checks it on Catch up, and records the page as where to try the
change. KEY defaults to your current task. wp ship --shot URL does the same.

In a Waypoint checkout, a /PATH is that page of this checkout's own build,
signed in as an admin (wp preview builds and runs it, or rebuilds it).
Pages of this Waypoint server open signed in as you, but it runs main, not
your branch. Other sites open signed out, so shoot a page that needs no
login. A local URL (localhost, private address) is recorded by its path.
Needs Playwright (the checkout's, else playwright@` + shotPlaywright + ` installed once
in wp's cache). Pictures are also saved locally (paths printed).`,
	examples: []string{
		"wp shot /admin?tab=ai",
		"wp shot http://localhost:5173/catch-up",
		"wp shot WP-12 https://staging.example.com/settings --wait-for '[data-ready]'",
	},
	run: cmdShot,
}

type shotOpts struct {
	sizes   []string
	waitFor string
	delay   time.Duration
	timeout time.Duration
	caption string
	noTry   bool
	out     string
}

// shotSizes are the widths wp shot takes, by name.
var shotSizes = map[string]shotSize{
	"desktop": {Name: "desktop", Label: "Desktop", Width: 1280, Height: 800},
	"phone":   {Name: "phone", Label: "Phone (390px)", Width: 390, Height: 844, Mobile: true},
}

type shotSize struct {
	Name   string `json:"name"`
	Label  string `json:"-"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Mobile bool   `json:"mobile"`
}

func addShotFlags(fs interface {
	String(string, string, string) *string
	Bool(string, bool, string) *bool
	Int(string, int, string) *int
}) func() (shotOpts, error) {
	sizes := fs.String("size", "desktop,phone", "sizes: `desktop,phone`, desktop or phone")
	waitFor := fs.String("wait-for", "", "wait for this CSS `SELECTOR` before each picture")
	delay := fs.Int("delay", 0, "extra wait `MS` before each picture")
	timeout := fs.Int("timeout", 30, "page timeout in `SECONDS`")
	caption := fs.String("caption", "", "caption `TEXT` (size and page are added)")
	noTry := fs.Bool("no-try", false, "don't record the page as the try link")
	out := fs.String("out", "", "save pictures under `DIR`/wp-shots")
	return func() (shotOpts, error) {
		o := shotOpts{waitFor: *waitFor, delay: time.Duration(*delay) * time.Millisecond, timeout: time.Duration(*timeout) * time.Second,
			caption: strings.TrimSpace(*caption), noTry: *noTry, out: *out}
		for _, s := range strings.Split(*sizes, ",") {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" {
				continue
			}
			if _, ok := shotSizes[s]; !ok {
				return o, fmt.Errorf("--size: %q isn't desktop or phone", s)
			}
			o.sizes = append(o.sizes, s)
		}
		if len(o.sizes) == 0 {
			return o, errors.New("--size: give desktop, phone or both")
		}
		if *timeout <= 0 || *delay < 0 {
			return o, errors.New("--timeout and --delay can't be negative")
		}
		return o, nil
	}
}

func cmdShot(c *cli, args []string) error {
	fs := c.flags()
	opts := addShotFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	o, err := opts()
	if err != nil {
		return c.usage("%s", err)
	}
	key, urls, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(urls) == 0 {
		return c.usage("which page? wp shot [KEY] URL|/PATH...")
	}
	return c.shoot(key, urls, o)
}

// shotTarget is a page to take, as the browser script reads it.
type shotTarget struct {
	URL  string `json:"url"`
	Auth bool   `json:"auth"` // this server's page: sign in with the token
}

// shotResult is one picture the browser script took.
type shotResult struct {
	URL    string `json:"url"`
	Size   string `json:"size"`
	File   string `json:"file"`
	Status int    `json:"status"`
	Title  string `json:"title"`
	Error  string `json:"error"`
}

// shootPages runs the browser; tests replace it.
var shootPages = runShotBrowser

// shoot takes, attaches and records the pages for key.
func (c *cli) shoot(key string, urls []string, o shotOpts) error {
	var targets []shotTarget
	tokens := map[string]string{}
	server, _ := url.Parse(c.server())
	if t := c.token(); t != "" && server != nil && server.Host != "" {
		tokens[webOrigin(server)] = t
	}
	// A path is a page of this checkout's preview (WP-1226): its own build,
	// signed in, rebuilt first so the picture shows the change.
	var preview *previewState
	local := map[string]bool{}
	for _, raw := range urls {
		if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
			if preview == nil {
				st, err := startPreview(c)
				if err != nil {
					return fmt.Errorf("%s is a path, which wp shot takes on this checkout's preview: %w", raw, err)
				}
				preview = st
			}
			raw = preview.URL + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return c.usage("%q isn't a page: give an http(s) URL, or a /PATH of this checkout's preview", raw)
		}
		auth := sameOrigin(u, c.server())
		if !auth && preview == nil && privateHost(u.Hostname()) {
			// A preview URL given in full is rebuilt first too.
			if running := runningPreview(); running != nil && sameOrigin(u, running.URL) {
				st, err := startPreview(c)
				if err != nil {
					return fmt.Errorf("%s is this checkout's preview, which couldn't be rebuilt: %w", raw, err)
				}
				preview = st
				if !sameOrigin(u, st.URL) { // restarted on another port
					u, _ = url.Parse(st.URL + u.RequestURI())
				}
			}
		}
		if !auth && preview != nil && sameOrigin(u, preview.URL) {
			auth = true
			tokens[webOrigin(u)] = preview.Token
			local[u.String()] = true
		} else if auth {
			if _, err := waypointCheckout(); err == nil {
				c.note("%s is on the Waypoint server, which runs main, not this checkout: wp shot %s takes this checkout's build", u, u.RequestURI())
			}
		}
		targets = append(targets, shotTarget{URL: u.String(), Auth: auth})
	}
	dir := o.out
	if dir == "" {
		// Under a "wp-shots" directory, so looking at them doesn't attach
		// them again as screenshots seen while working (hook_shots.go).
		base := filepath.Join(os.TempDir(), "wp-shots")
		if err := os.MkdirAll(base, 0o700); err != nil {
			return err
		}
		d, err := os.MkdirTemp(base, strings.ToLower(key)+"-")
		if err != nil {
			return err
		}
		dir = d
	} else {
		dir = filepath.Join(dir, "wp-shots") // still not attached again when looked at
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	var sizes []shotSize
	for _, s := range o.sizes {
		sizes = append(sizes, shotSizes[s])
	}
	results, err := shootPages(c, shotJob{Targets: targets, Sizes: sizes, Dir: dir, WaitFor: o.waitFor,
		DelayMS: int(o.delay / time.Millisecond), TimeoutMS: int(o.timeout / time.Millisecond), Tokens: tokens})
	if err != nil {
		return err
	}
	// A picture that can't be attached is reported and skipped like a page
	// that couldn't be taken: failing the whole run once some are on the task
	// would have wp done run again and attach those a second time.
	failed, attachedN := 0, 0
	tried := map[string]bool{}
	for _, r := range results {
		if r.Error != "" {
			failed++
			c.note("%s (%s): %s", r.URL, r.Size, r.Error)
			continue
		}
		if r.Status >= 400 {
			c.note("%s answered HTTP %d: the picture shows that page", r.URL, r.Status)
		}
		if !shotInDir(dir, r.File) {
			// The browser runs a Playwright found in the checkout: whatever
			// it names outside the pictures directory isn't a picture (WP-579).
			failed++
			c.note("%s (%s): the browser named %s, outside %s; not attached", r.URL, r.Size, r.File, dir)
			continue
		}
		a, err := c.attachShot(key, r, o.caption, local[r.URL])
		if err != nil {
			failed++
			c.note("couldn't attach %s: %v", r.File, err)
			continue
		}
		attachedN++
		c.println(fmt.Sprintf("%s  %s  (%s)", key, a.Markdown, r.File))
		if !o.noTry && r.Status < 400 && !tried[r.URL] {
			tried[r.URL] = true
			link := tryLinkFor(r.URL)
			if _, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/try-links", map[string]string{"url": link}); err != nil {
				c.note("couldn't record %s as where to try it: %v", link, err)
			} else {
				c.println("Try it: " + link)
			}
		}
	}
	if attachedN == 0 {
		return fmt.Errorf("no picture was taken (%d failed)", failed)
	}
	return nil
}

// shotInDir: file is a regular file inside dir, symlinks resolved.
func shotInDir(dir, file string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	f, err := filepath.EvalSymlinks(file)
	if err != nil {
		return true // missing: attaching it fails and says so
	}
	if st, err := os.Stat(f); err != nil || !st.Mode().IsRegular() {
		return false
	}
	rel, err := filepath.Rel(d, f)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// attachShot uploads one picture to key as a review screenshot.
// A picture of the preview says it shows this branch's build.
func (c *cli) attachShot(key string, r shotResult, caption string, preview bool) (attached, error) {
	var a attached
	data, err := os.ReadFile(r.File) // #nosec G304 -- the file the browser just wrote
	if err != nil {
		return a, err
	}
	cap := shotSizes[r.Size].Label + " · " + shotPlace(r.URL)
	if preview {
		cap += " · this branch's build"
	}
	if caption != "" {
		cap = caption + " (" + cap + ")"
	}
	q := url.Values{"target": {"issue:" + key}, "name": {filepath.Base(r.File)}, "caption": {cap}, "review": {"1"}}
	raw, err := c.callRaw("POST", "/api/v1/attachments?"+q.Encode(), "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return a, err
	}
	err = jsonInto(raw, &a)
	return a, err
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// sameOrigin says whether u is a page of the Waypoint server.
func sameOrigin(u *url.URL, server string) bool {
	s, err := url.Parse(server)
	return err == nil && s.Host != "" && strings.EqualFold(s.Scheme, u.Scheme) &&
		strings.EqualFold(hostPort(s), hostPort(u))
}

// hostPort is u's host with its port, the scheme's default port filled in
// when the URL leaves it out, so https://h and https://h:443 compare equal.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// tryLinkFor is the try link to record for a page: the URL, or its path when
// the page is on this machine or a private network, so Catch up can open it
// on the deployed environment.
func tryLinkFor(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !privateHost(u.Hostname()) {
		return raw
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		p += "#" + u.EscapedFragment()
	}
	return p
}

func privateHost(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || h == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast())
}

// shotPlace is a page's short name in a caption: its host and path.
func shotPlace(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if privateHost(u.Hostname()) {
		return tryLinkFor(raw)
	}
	return u.Host + u.EscapedPath()
}

// shotJob is what the browser script is given (on stdin).
type shotJob struct {
	Targets   []shotTarget `json:"targets"`
	Sizes     []shotSize   `json:"sizes"`
	Dir       string       `json:"dir"`
	WaitFor   string       `json:"wait_for"`
	DelayMS   int          `json:"delay_ms"`
	TimeoutMS int          `json:"timeout_ms"`
	// Tokens sign in to these origins' pages (web origin to token); they
	// reach the browser in its environment, not on stdin.
	Tokens map[string]string `json:"-"`
}

// webOrigin is u's origin as a browser writes it: scheme://host, with the
// scheme's default port left out.
func webOrigin(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && (scheme != "http" || p != "80") && (scheme != "https" || p != "443") {
		host += ":" + p
	}
	return scheme + "://" + host
}

// runShotBrowser runs shot.mjs with node against a Playwright it finds or
// installs. The token reaches the browser in the environment, never argv.
func runShotBrowser(c *cli, job shotJob) ([]shotResult, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("wp shot needs Node.js (node on PATH) to run a browser")
	}
	from, err := c.findPlaywright()
	if err != nil {
		return nil, err
	}
	scriptDir, err := os.MkdirTemp("", "wp-shot-js-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scriptDir) }()
	script := filepath.Join(scriptDir, "shot.mjs")
	if err := os.WriteFile(script, []byte(shotScript), 0o600); err != nil {
		return nil, err
	}
	in, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	tokens, err := json.Marshal(job.Tokens)
	if err != nil {
		return nil, err
	}
	run := func() ([]byte, []byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(job.Targets)*len(job.Sizes)+1)*time.Duration(2*job.TimeoutMS+job.DelayMS+20000)*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, node, script, from) // #nosec G204 -- our own script and a found module path
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = append(os.Environ(), "WP_SHOT_TOKENS="+string(tokens))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.Bytes(), errb.Bytes(), err
	}
	out, errOut, err := run()
	if err != nil && missingBrowser(errOut) {
		c.note("Installing Playwright's Chromium (once)…")
		if ierr := installChromium(node, from); ierr != nil {
			return nil, fmt.Errorf("no browser, and installing one failed: %w", ierr)
		}
		out, errOut, err = run()
	}
	if err != nil {
		return nil, fmt.Errorf("the browser failed: %v\n%s", err, lastLines(errOut, 12))
	}
	var res []shotResult
	if err := json.Unmarshal([]byte(lastLine(string(out))), &res); err != nil {
		return nil, fmt.Errorf("the browser said something wp can't read: %v\n%s", err, lastLines(append(out, errOut...), 12))
	}
	return res, nil
}

var missingBrowserRE = lazyre.New(`Executable doesn't exist|playwright install|browserType\.launch: .*not found`)

func missingBrowser(stderr []byte) bool { return missingBrowserRE.Match(stderr) }

func installChromium(node, from string) error {
	cliJS := filepath.Join(filepath.Dir(from), "node_modules", "playwright", "cli.js")
	if _, err := os.Stat(cliJS); err != nil {
		cliJS = filepath.Join(filepath.Dir(from), "node_modules", "playwright-core", "cli.js")
	}
	cmd := exec.Command(node, cliJS, "install", "chromium") // #nosec G204 -- Playwright's own installer
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// findPlaywright is a file path Node can resolve playwright from: a
// directory with node_modules/playwright (or @playwright/test) at or above
// here, or in the checkout's e2e/ or web/, else wp's own cache, installed
// there once.
func (c *cli) findPlaywright() (string, error) {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		for d := wd; ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if filepath.Dir(d) == d {
				break
			}
		}
		if top := gitTop(wd); top != "" {
			dirs = append(dirs, filepath.Join(top, "e2e"), filepath.Join(top, "web"), filepath.Join(top, "tests", "e2e"), filepath.Join(top, "frontend"))
		}
	}
	if d := playwrightIn(dirs); d != "" {
		return filepath.Join(d, "wp-shot.js"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "waypoint", "shot")
	if d := playwrightIn([]string{dir}); d != "" {
		return filepath.Join(d, "wp-shot.js"), nil
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", errors.New("no Playwright here and no npm to install it: run wp shot in a checkout with playwright in node_modules")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	c.note("Installing playwright@%s in %s (once)…", shotPlaywright, dir)
	cmd := exec.Command(npm, "install", "--prefix", dir, "--no-save", "--no-audit", "--no-fund", "--loglevel=error", "playwright@"+shotPlaywright) // #nosec G204 -- a pinned package
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("installing playwright: %w", err)
	}
	return filepath.Join(dir, "wp-shot.js"), nil
}

func playwrightIn(dirs []string) string {
	for _, d := range dirs {
		for _, m := range []string{"playwright", filepath.Join("@playwright", "test")} {
			if _, err := os.Stat(filepath.Join(d, "node_modules", m, "package.json")); err == nil {
				return d
			}
		}
	}
	return ""
}
