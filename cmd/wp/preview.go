package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// wp preview (WP-1226): this checkout's own Waypoint, built from the working
// tree and run on 127.0.0.1 with a throwaway database and an admin signed
// in, so an agent can screenshot its change (wp shot /PATH) before any
// server it can sign into runs it. Production runs main; a branch has no
// server of its own, and a local build's pages need a login the agent
// didn't have. Waypoint checkouts only: another app's dev server is the
// agent's to start, and wp shot takes its URL.

var previewCmd = &command{
	name: "preview", group: "Tasks", args: "[start|stop|status]",
	summary: "run this checkout's Waypoint locally, signed in, for wp shot",
	about: `Builds this Waypoint checkout as it is on disk (web app and server) and
runs it on 127.0.0.1 with its own empty database, an admin account and a
project with your task's key. wp shot /PATH (and wp ship --shot /PATH)
takes that page of it signed in, starting or rebuilding the preview first,
so the pictures show your change. Run it again after changing code: it
rebuilds and restarts only when the build changed, keeping the data.

To add data, point wp at it: the WAYPOINT_SERVER and WAYPOINT_TOKEN it
prints. It stops after ` + "2h" + ` unused, when the checkout is removed, or with
wp preview stop (which deletes its data).`,
	examples: []string{"wp preview", "wp shot /admin?tab=ai", "wp preview stop"},
	run:      cmdPreviewStart,
}

func init() {
	previewCmd.subs = []*command{
		{name: "start", summary: "build and run (or rebuild) this checkout's preview", run: cmdPreviewStart},
		{name: "status", aliases: []string{"show"}, summary: "the preview running for this checkout, if any", run: cmdPreviewStatus},
		{name: "stop", summary: "stop this checkout's preview and delete its data", run: cmdPreviewStop},
		{name: "serve", args: "DIR", summary: "(internal) keep a preview's server running until it lapses", hidden: true, run: cmdPreviewServe},
	}
}

// previewIdle is how long a preview runs after its last use.
var previewIdle = 2 * time.Hour

// previewState is a preview's state.json, in its directory under wp's cache.
type previewState struct {
	URL     string    `json:"url"`
	Port    int       `json:"port"`
	Token   string    `json:"token"`
	Project string    `json:"project,omitempty"`
	Top     string    `json:"top"`    // the checkout it builds
	Build   string    `json:"build"`  // sha256 of the binary it runs
	Schema  string    `json:"schema"` // sha256 of the migrations its database was made by
	PID     int       `json:"pid"`    // its keeper, wp preview serve
	Until   time.Time `json:"until"`
}

// waypointCheckout is the top of the Waypoint checkout here.
func waypointCheckout() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	top := gitTop(wd)
	if top != "" && isWaypointTree(top) {
		return top, nil
	}
	return "", errors.New("a preview is this Waypoint checkout's own server, and this isn't a Waypoint checkout (no cmd/waypoint or web/embed.go). For another app, start its dev server and give wp shot its URL")
}

func isWaypointTree(top string) bool {
	for _, f := range []string{"cmd/waypoint/main.go", "web/embed.go", "web/package.json"} {
		if _, err := os.Stat(filepath.Join(top, f)); err != nil {
			return false
		}
	}
	return true
}

// previewDir is where top's preview keeps its binary, database and state.
func previewDir(top string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(top))
	return filepath.Join(cache, "waypoint", "preview", hex.EncodeToString(sum[:6])), nil
}

func loadPreview(dir string) (*previewState, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json")) // #nosec G304 -- our own state file
	if err != nil {
		return nil, err
	}
	var st previewState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func savePreview(dir string, st *previewState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

// alive: the preview answers as the account wp made there.
func (st *previewState) alive() bool {
	if st == nil || st.URL == "" || st.Token == "" {
		return false
	}
	req, err := http.NewRequest("GET", st.URL+"/api/v1/me", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+st.Token)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req) // #nosec G704 -- our own preview on 127.0.0.1
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	// Another app now on the port may answer anything with a 200.
	var me struct {
		Username string `json:"username"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me) == nil &&
		me.Username == previewUser
}

// previewUser is the admin wp makes on a preview.
const previewUser = "preview"

// listening: something answers the preview's health check.
func (st *previewState) listening() bool {
	if st == nil || st.URL == "" {
		return false
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(st.URL + "/healthz") // #nosec G704 -- our own preview on 127.0.0.1
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// runningPreview is this checkout's preview when one is up; nil otherwise.
func runningPreview() *previewState {
	top, err := waypointCheckout()
	if err != nil {
		return nil
	}
	dir, err := previewDir(top)
	if err != nil {
		return nil
	}
	if st, err := loadPreview(dir); err == nil && st.alive() {
		return st
	}
	return nil
}

// startPreview builds and runs this checkout's preview; tests replace it.
var startPreview = (*cli).ensurePreview

// ensurePreview builds this checkout and makes its preview run that build:
// started when none is up, restarted (same database) when the build
// changed, else left as it is. Each call pushes its end back.
func (c *cli) ensurePreview() (*previewState, error) {
	top, err := waypointCheckout()
	if err != nil {
		return nil, err
	}
	dir, err := previewDir(top)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// One wp at a time per checkout: two shots at once mustn't start two.
	if lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own lock file
		defer func() { _ = lf.Close() }()
		if unlock, err := fileLock(lf); err == nil {
			defer unlock()
		}
	}
	st, _ := loadPreview(dir)
	up := st.alive()
	if st != nil {
		// Pushed back before the build, which can wait minutes for a
		// slot: the keeper mustn't let the preview lapse under it.
		st.Until = time.Now().Add(previewIdle)
		if err := savePreview(dir, st); err != nil {
			return nil, err
		}
	}
	bin := filepath.Join(dir, "waypoint")
	next := bin + ".new"
	c.note("Building this checkout for its preview…")
	if err := c.buildPreview(top, next); err != nil {
		_ = os.Remove(next)
		return nil, err
	}
	build, err := fileSHA(next)
	if err != nil {
		return nil, err
	}
	schema := migrationsSum(top)
	if up && st.Build == build {
		_ = os.Remove(next)
		return st, nil
	}
	if st != nil {
		stopPreviewServer(dir, st)
	}
	if err := os.Rename(next, bin); err != nil {
		return nil, err
	}
	// Migrations apply once by name, so a database made by migrations
	// since edited, renamed or dropped would be wrong for this build.
	fresh := st == nil || st.Token == "" || st.Schema != schema
	if fresh && st != nil && st.Token != "" {
		c.note("The migrations changed since the preview's data was made: it starts with an empty database.")
	}
	if fresh {
		// No account wp knows of: start from an empty database.
		for _, f := range []string{"wp.db", "wp.db-wal", "wp.db-shm", "server.log"} {
			_ = os.Remove(filepath.Join(dir, f))
		}
		st = &previewState{}
	}
	st.Top, st.Build, st.Schema, st.Until = top, build, schema, time.Now().Add(previewIdle)
	st.Port = previewPort(st.Port)
	if st.Port == 0 {
		return nil, errors.New("no free port on 127.0.0.1 for the preview")
	}
	st.URL = "http://127.0.0.1:" + strconv.Itoa(st.Port)
	if err := savePreview(dir, st); err != nil {
		return nil, err
	}
	pid, err := startPreviewKeeper(dir)
	if err != nil {
		return nil, err
	}
	st.PID = pid
	if err := savePreview(dir, st); err != nil {
		return nil, err
	}
	if err := waitHealthy(st.URL, 30*time.Second); err != nil {
		stopPreviewServer(dir, st)
		return nil, fmt.Errorf("the preview didn't start (wp preview stop deletes its data, so the next start is fresh): %v\n%s", err, tailFile(filepath.Join(dir, "server.log"), 12))
	}
	if !fresh && !st.alive() {
		// Something else may have taken the port before the server bound it.
		stopPreviewServer(dir, st)
		return nil, errors.New("the preview's server didn't come up as the preview's own (the port may be taken); wp preview stop, then try again")
	}
	if fresh {
		project := c.previewProject()
		invite, err := previewInvite(filepath.Join(dir, "server.log"))
		if err == nil {
			st.Token, err = previewSignUp(st.URL, invite, project)
		}
		if err != nil {
			stopPreviewServer(dir, st)
			return nil, fmt.Errorf("couldn't sign in to the preview: %w", err)
		}
		st.Project = project
		if err := savePreview(dir, st); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// buildPreview builds the web app into web/dist (as make web does) and the
// server, embedding it, into out. It takes a turn in the host's tests slot.
func (c *cli) buildPreview(top, out string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env, release, err := c.takeSlot(ctx, "tests", defaultSlots())
	if err != nil {
		return err
	}
	defer release()
	// The checkout's code (npm install scripts, vite's config) runs here:
	// without the caller's credentials, as tests run (WP-1065).
	env, _ = testEnv(env, nil)
	web := filepath.Join(top, "web")
	vite := filepath.Join(web, "node_modules", "vite", "bin", "vite.js")
	steps := [][]string{}
	if _, err := os.Stat(vite); err != nil {
		steps = append(steps, []string{"npm", "ci", "--no-audit", "--no-fund", "--loglevel=error"})
	}
	steps = append(steps, []string{"node", vite, "build"})
	for _, s := range steps {
		if err := runQuiet(ctx, web, env, s); err != nil {
			return err
		}
	}
	// vite empties dist; the placeholder is tracked (package.json's build does the same).
	if err := os.WriteFile(filepath.Join(web, "dist", ".gitkeep"), nil, 0o600); err != nil {
		return err
	}
	return runQuiet(ctx, top, append(env, "CGO_ENABLED=0"), []string{"go", "build", "-trimpath", "-o", out, "./cmd/waypoint"})
}

// runQuiet runs argv in dir, showing its output only when it fails.
func runQuiet(ctx context.Context, dir string, env, argv []string) error {
	var buf bytes.Buffer
	code, err := runSlotCmdIn(ctx, dir, env, argv, nil, &buf, &buf)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		return fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, lastLines(buf.Bytes(), 20))
	}
	return nil
}

// migrationsSum is a hash of the checkout's migration files, names and text.
func migrationsSum(top string) string {
	dir := filepath.Join(top, "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- the checkout's own migrations
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		_, _ = fmt.Fprintf(h, "%s %x\n", e.Name(), sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the binary just built
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// previewPort is want when it is free, else one the system picks.
func previewPort(want int) int {
	if want > 0 {
		if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(want))); err == nil {
			_ = l.Close()
			return want
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startPreviewKeeper starts wp preview serve DIR on its own, outliving this wp.
func startPreviewKeeper(dir string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- our own log
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }()
	cmd := exec.Command(self, "preview", "serve", dir) // #nosec G204 G702 -- wp itself
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachedProc()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func waitHealthy(base string, within time.Duration) error {
	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		resp, err := hc.Get(base + "/healthz") // #nosec G704 -- our own preview on 127.0.0.1
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("healthz: HTTP %d", resp.StatusCode)
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("no answer in %s: %v", within, last)
}

// stopPreviewServer ends the keeper (which ends the server) and waits for
// the server to go; the database stays. A pid is signalled only while it
// is still the preview's own process, so one since reused is left alone.
func stopPreviewServer(dir string, st *previewState) {
	if st.PID > 0 && previewProc(st.PID, dir, st) {
		if p, err := os.FindProcess(st.PID); err == nil {
			if p.Signal(syscall.SIGTERM) != nil {
				_ = p.Kill()
			}
		}
		for i := 0; i < 75 && (previewProc(st.PID, dir, st) || st.listening()); i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}
	// A keeper that died without its server leaves the server's pid.
	if b, err := os.ReadFile(filepath.Join(dir, "server.pid")); err == nil { // #nosec G304 -- our own pid file
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && previewProc(pid, dir, st) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	}
	st.PID = 0
}

// previewProc says whether pid is one of dir's preview processes: its
// keeper (wp preview serve DIR) or its server (DIR/waypoint serve), by
// command line, so a pid since reused is left alone. Without /proc, ps
// gives the command line; if neither does, it is not taken to be ours.
func previewProc(pid int, dir string, _ *previewState) bool {
	var args []string
	if b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil { // #nosec G304 -- a process's command line
		args = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	} else if out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output(); err == nil { // #nosec G204 -- a pid we wrote
		args = strings.Fields(string(out))
	} else {
		return false
	}
	for i, a := range args {
		if a == dir && i >= 2 && args[i-1] == "serve" && args[i-2] == "preview" {
			return true
		}
	}
	return len(args) >= 2 && args[0] == filepath.Join(dir, "waypoint") && args[1] == "serve"
}

var previewInviteRE = lazyre.New(`/invite/([A-Za-z0-9_-]+)`)

// previewInvite is the first-run admin invite the server printed.
func previewInvite(logPath string) (string, error) {
	b, err := os.ReadFile(logPath) // #nosec G304 -- our own log
	if err != nil {
		return "", err
	}
	m := previewInviteRE.FindSubmatch(b)
	if m == nil {
		return "", errors.New("the server printed no admin invite (its database wasn't empty)")
	}
	return string(m[1]), nil
}

// previewSignUp accepts the first-run invite as an admin, makes an API
// token for it and, when project is given, a project with that key.
func previewSignUp(base, invite, project string) (string, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 15 * time.Second, Jar: jar}
	pw := make([]byte, 18)
	_, _ = rand.Read(pw)
	post := func(path, bearer string, body any, into any) error {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("POST", base+path, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Waypoint-Client", "wp")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := hc.Do(req) // #nosec G704 -- our own preview on 127.0.0.1
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if into != nil {
			return json.Unmarshal(raw, into)
		}
		return nil
	}
	if err := post("/api/v1/auth/invite/"+url.PathEscape(invite), "", map[string]string{
		"username": previewUser, "email": "preview@example.com", "display_name": "Preview admin", "password": hex.EncodeToString(pw),
	}, nil); err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := post("/api/v1/me/tokens", "", map[string]any{"name": "wp preview"}, &tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		return "", errors.New("the preview gave no token")
	}
	if project != "" {
		name := project + " (preview)"
		if err := post("/api/v1/projects", tok.Token, map[string]string{"key": project, "name": name}, nil); err != nil {
			return "", err
		}
	}
	return tok.Token, nil
}

// previewProject is the key of the project of the task held here, which
// the preview gets so its /projects/KEY pages exist.
func (c *cli) previewProject() string {
	for _, k := range c.heldKeys() {
		if i := strings.LastIndex(k, "-"); i > 0 {
			return k[:i]
		}
	}
	return ""
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path) // #nosec G304 -- our own log
	if err != nil {
		return ""
	}
	return lastLines(b, n)
}

func (c *cli) printPreview(st *previewState) {
	what := "empty"
	if st.Project != "" {
		what = "project " + st.Project + ", otherwise empty"
	}
	c.println(fmt.Sprintf("Preview of this checkout: %s, signed in as an admin (%s).", st.URL, what))
	c.println("  wp shot /PATH      screenshots a page of it onto your task (rebuilds first)")
	c.println(fmt.Sprintf("  WAYPOINT_SERVER=%s WAYPOINT_TOKEN=%s wp new \"A task\"   adds data", st.URL, st.Token))
	c.println(fmt.Sprintf("Stops at %s unless used again, or with wp preview stop.", st.Until.Local().Format("15:04")))
}

func cmdPreviewStart(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q: wp preview [start|stop|status]", pos[0])
	}
	st, err := startPreview(c)
	if err != nil {
		return err
	}
	c.printPreview(st)
	return nil
}

func cmdPreviewStatus(c *cli, args []string) error {
	if _, err := c.parse(c.flags(), args); err != nil {
		return err
	}
	if _, err := waypointCheckout(); err != nil {
		return err
	}
	st := runningPreview()
	if st == nil {
		c.println("No preview of this checkout is running; wp preview starts one.")
		return nil
	}
	c.printPreview(st)
	return nil
}

func cmdPreviewStop(c *cli, args []string) error {
	if _, err := c.parse(c.flags(), args); err != nil {
		return err
	}
	top, err := waypointCheckout()
	if err != nil {
		return err
	}
	dir, err := previewDir(top)
	if err != nil {
		return err
	}
	// Not while a start or the keeper's lapse check has the directory.
	if lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own lock file
		defer func() { _ = lf.Close() }()
		if unlock, err := fileLock(lf); err == nil {
			defer unlock()
		}
	}
	st, err := loadPreview(dir)
	if err != nil {
		c.println("No preview of this checkout to stop.")
		return nil
	}
	stopPreviewServer(dir, st)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	c.println("Stopped the preview and deleted its data.")
	return nil
}

// cmdPreviewServe is the keeper: it runs the preview's server and ends it
// when the preview lapses or its checkout is gone, deleting the data then.
func cmdPreviewServe(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("wp preview serve DIR")
	}
	dir := pos[0]
	st, err := loadPreview(dir)
	if err != nil {
		return err
	}
	if want, err := previewDir(st.Top); err != nil || filepath.Clean(dir) != want {
		return errors.New("wp preview serve is run by wp preview, on a preview's own directory")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port))
	// Only what the server needs: no tokens, AI or forge settings from the
	// shell that started it reach a throwaway server.
	env := []string{"WAYPOINT_DB=" + filepath.Join(dir, "wp.db"), "WAYPOINT_ADDR=" + addr,
		"WAYPOINT_BASE_URL=http://" + addr, "WAYPOINT_BACKUP_DIR=off", "WAYPOINT_LOG_FORMAT=text"}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	srv := exec.Command(filepath.Join(dir, "waypoint"), "serve") // #nosec G204 G702 -- the binary wp preview built
	srv.Env = env
	srv.Stdout, srv.Stderr = c.out, c.err
	if err := srv.Start(); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(dir, "server.pid"), []byte(strconv.Itoa(srv.Process.Pid)), 0o600)
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	lapsed := false
	var lockFile *os.File // held while the directory is deleted
	for !lapsed {
		select {
		case err := <-exited:
			_ = os.Remove(filepath.Join(dir, "server.pid"))
			return err
		case <-ctx.Done():
			stopServer(srv, exited)
			return nil
		case <-tick.C:
			if !previewLapsed(dir, st.Top) {
				continue
			}
			// A start or stop holding the lock may be reviving it: look again
			// under the lock, and leave it for the next tick if it is busy.
			if lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil { // #nosec G304 -- our own lock file
				got, lerr := slotTryLock(lf)
				if lerr == nil && !got {
					_ = lf.Close()
					continue
				}
				lockFile = lf
				lapsed = previewLapsed(dir, st.Top)
				if !lapsed {
					_ = lf.Close()
					lockFile = nil
				}
				continue
			}
			lapsed = true
		}
	}
	if lockFile != nil {
		defer func() { _ = lockFile.Close() }()
	}
	stopServer(srv, exited)
	return os.RemoveAll(dir)
}

// previewLapsed: the preview's time is up, or its checkout or state is gone.
func previewLapsed(dir, top string) bool {
	cur, err := loadPreview(dir)
	_, gone := os.Stat(cur.topOr(top))
	return (err == nil && time.Now().After(cur.Until)) || errors.Is(gone, os.ErrNotExist) || os.IsNotExist(err)
}

func (st *previewState) topOr(def string) string {
	if st == nil || st.Top == "" {
		return def
	}
	return st.Top
}

// stopServer asks the server to stop and kills it if it hasn't in 10s.
func stopServer(srv *exec.Cmd, exited <-chan error) {
	if srv.Process.Signal(syscall.SIGTERM) != nil {
		_ = srv.Process.Kill()
	}
	select {
	case err := <-exited:
		_ = err // the server's exit status after a stop says nothing
	case <-time.After(10 * time.Second):
		_ = srv.Process.Kill()
		err := <-exited
		_ = err
	}
}
