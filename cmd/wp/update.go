package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var updateCmd = &command{
	name: "upgrade", aliases: []string{"self-update"}, group: "Setup", args: "[--check] [--force]",
	summary: "replace this wp with the version your Waypoint serves",
	about: `Downloads the wp your Waypoint serves, verifies it and replaces this
binary. wp commands, hooks and runners do this themselves for release builds; set
WAYPOINT_NO_AUTO_UPDATE=1 to stop that.`,
	examples: []string{"wp upgrade", "wp upgrade --check"},
	run:      cmdUpdate,
}

// updateCheck is the last automatic look for a new wp, in the state file.
type updateCheck struct {
	Checked time.Time `json:"checked,omitzero"`
	Server  string    `json:"server,omitempty"` // the server's version then
	Result  string    `json:"result,omitempty"`
}

// autoUpdateEvery is how often the hooks look for a new wp.
const autoUpdateEvery = 24 * time.Hour

// selfExe is the binary an update replaces: this one, symlinks resolved
// (tests swap it).
var selfExe = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}

// isRelease: a released version, a tag like v0.2.0: not a development build
// ("dev", "-dirty") nor one between tags (v0.1.0-3-gabc1234), so machines
// only ever move between releases.
func isRelease(v string) bool {
	return releaseRE.MatchString(v)
}

var releaseRE = lazyre.New(`^v\d+\.\d+\.\d+$`)

// autoUpdateOff: the person asked for wp to stay as it is.
func autoUpdateOff() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WAYPOINT_NO_AUTO_UPDATE")))
	return v != "" && v != "0" && v != "false"
}

var downloadClient = &http.Client{Timeout: 5 * time.Minute}

// cliAsset is the server's file name for this computer's wp.
func cliAsset() string {
	name := "wp-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// fetchPublic GETs a path of the server that needs no sign-in (/healthz,
// /cli/…) into w, at most limit bytes. No token is sent.
func (c *cli) fetchPublic(ctx context.Context, path string, w io.Writer, limit int64) error {
	server := c.server()
	if server == "" {
		return errors.New("no server set: wp config server https://your-waypoint")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", server+path, nil) // #nosec G704 -- the configured server
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wp/"+version)
	resp, err := downloadClient.Do(req) // #nosec G704 -- the configured server
	if err != nil {
		return fmt.Errorf("can't reach %s: %w", server, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.seeServerVersion(resp.Header.Get(serverVersionHeader))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s%s: HTTP %d", server, path, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s%s is larger than expected", server, path)
	}
	return nil
}

// serverVersion is the version the server runs (GET /healthz).
func (c *cli) serverVersion(ctx context.Context) (string, error) {
	var b bytes.Buffer
	if err := c.fetchPublic(ctx, "/healthz", &b, 64<<10); err != nil {
		return "", err
	}
	var h struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b.Bytes(), &h); err != nil {
		return "", fmt.Errorf("the server's /healthz isn't JSON: %w", err)
	}
	return h.Version, nil
}

// selfUpdate puts the server's wp for this computer in place of this one,
// once it has checked the download against SHA256SUMS and run it, and it
// said it is version want (when want isn't empty). It returns the new
// version and the path it replaced. When the binary at that path already is
// the server's (another process replaced it): its SHA-256 is the one
// SHA256SUMS names, and then it runs and says it is want, it returns
// without downloading. A file at that path that only says want is replaced
// and never run (1788.2).
func (c *cli) selfUpdate(ctx context.Context, want string) (got, exe string, err error) {
	got, exe, _, err = c.installRelease(ctx, want)
	return got, exe, err
}

// installRelease is selfUpdate also returning the SHA-256 it checked the
// installed file against, the one it put at exe (c7016).
func (c *cli) installRelease(ctx context.Context, want string) (got, exe, sum string, err error) {
	exe, err = selfExe()
	if err != nil {
		return "", "", "", err
	}
	if throwawayBuild(exe) {
		return "", exe, "", errors.New("this wp is a temporary build (go run, go test); install one with curl -fsSL " + orDash(c.server()) + "/cli | sh")
	}
	name := cliAsset()
	wantSum, err := c.cliSum(ctx)
	if err != nil {
		return "", exe, "", err
	}
	// Another process may have replaced it since this one started: a binary
	// that already is the server's release needs no download.
	if want != "" && want != version && isServersRelease(ctx, exe, wantSum, want) {
		return want, exe, wantSum, nil
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".wp-update-*"+ext)
	if err != nil {
		return "", exe, "", fmt.Errorf("can't write next to %s (%w); reinstall with curl -fsSL %s/cli | sh", exe, err, orDash(c.server()))
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	err = c.fetchPublic(ctx, "/cli/"+name, io.MultiWriter(tmp, h), 512<<20)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", exe, "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != wantSum {
		return "", exe, "", fmt.Errorf("the downloaded %s doesn't match the server's SHA256SUMS; not installing it", name)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { // #nosec G302 -- an executable
		return "", exe, "", err
	}
	if got, err = binaryVersion(ctx, tmp.Name()); err != nil {
		return "", exe, "", err
	}
	if want != "" && got != want {
		return "", exe, "", fmt.Errorf("the server's wp says it is %s, not %s; not installing it", got, want)
	}
	if err := replaceExe(exe, tmp.Name()); err != nil {
		return "", exe, "", fmt.Errorf("can't replace %s: %w", exe, err)
	}
	return got, exe, wantSum, nil
}

// cliSum is the SHA-256 the server's SHA256SUMS names for this computer's wp.
func (c *cli) cliSum(ctx context.Context) (string, error) {
	var sums bytes.Buffer
	if err := c.fetchPublic(ctx, "/cli/SHA256SUMS", &sums, 64<<10); err != nil {
		return "", err
	}
	name := cliAsset()
	sum := checksumFor(sums.String(), name)
	if sum == "" {
		return "", fmt.Errorf("the server's SHA256SUMS has no %s", name)
	}
	return sum, nil
}

// isServersRelease says whether the wp at path is the server's release want:
// its SHA-256 is wantSum (checked before it runs, so nothing else at the
// path is run) and, run, it says it is want.
func isServersRelease(ctx context.Context, path, wantSum, want string) bool {
	f, err := os.Open(path) // #nosec G304 -- this wp's own install path
	if err != nil {
		return false
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(f, 512<<20))
	_ = f.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != wantSum {
		return false
	}
	on, err := binaryVersion(ctx, path)
	return err == nil && on == want
}

// checksumFor finds name's SHA-256 in sha256sum output.
func checksumFor(sums, name string) string {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// binaryVersion runs a wp binary's `version`: it must run here.
func binaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output() // #nosec G204 -- the wp just downloaded and checked
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "wp ")
	if err != nil || !ok || v == "" || strings.ContainsAny(v, " \n") {
		return "", fmt.Errorf("the downloaded wp doesn't run on this computer (%v); not installing it", err)
	}
	return v, nil
}

// replaceExe puts next in place of exe. On Windows a running .exe can't be
// overwritten but can be renamed, so the old one moves aside first. A lock
// beside exe lets one wp at a time do it: two that overlap on Windows could
// remove the other's moved-aside binary and leave none at exe.
func replaceExe(exe, next string) error {
	unlock, err := lockFileWithin(filepath.Join(filepath.Dir(exe), ".wp-update.lock"), 20*time.Second)
	if err != nil {
		return fmt.Errorf("another wp is replacing it: %w", err)
	}
	defer unlock()
	if runtime.GOOS != "windows" {
		return os.Rename(next, exe)
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(old); err == nil {
		old = fmt.Sprintf("%s.old-%d", exe, time.Now().UnixNano())
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return err
	}
	return nil
}

func cmdUpdate(c *cli, args []string) error {
	fs := c.flags()
	check := fs.Bool("check", false, "only say whether the version differs")
	force := fs.Bool("force", false, "install even if the versions match")
	auto := fs.Bool("auto", false, "(hidden) the hooks' daily look")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sv, err := c.serverVersion(ctx)
	if *auto {
		c.autoUpdate(ctx, sv, err)
		return nil
	}
	if err != nil {
		return err
	}
	if *check {
		c.println("This wp:  ", version)
		c.println("Waypoint: ", orDash(sv), "("+c.server()+")")
		switch sv {
		case version:
			c.println("Up to date.")
		case "":
			c.println("The server doesn't say its version.")
		default:
			c.println("wp upgrade installs", sv+".")
		}
		return nil
	}
	if sv == version && !*force {
		c.println("wp", version, "is what", c.server(), "serves already.")
		c.println(c.kitSyncHere())
		return nil
	}
	got, exe, err := c.selfUpdate(ctx, sv)
	if err != nil {
		return err
	}
	c.println("Updated", exe, "from", version, "to", got+".")
	c.println(c.kitSyncHere())
	return nil
}

// autoUpdate is `wp upgrade --auto`, which the hooks start: it updates only
// release builds, says nothing, and records what happened.
func (c *cli) autoUpdate(ctx context.Context, sv string, err error) {
	var result string
	switch {
	case err != nil:
		result = "couldn't ask the server: " + err.Error()
	case autoUpdateOff():
		result = "skipped: WAYPOINT_NO_AUTO_UPDATE"
	case !isRelease(version) || !isRelease(sv):
		result = "skipped: not both release builds"
	case sv == version:
		result = "up to date"
	default:
		if got, _, err := c.selfUpdate(ctx, sv); err != nil {
			result = "failed: " + err.Error()
		} else {
			result = "updated from " + version + " to " + got
		}
	}
	c.recordUpdate(sv, result, false)
}

// recordUpdate writes the state file's record of an update: now, for server
// version sv, with result. A check that couldn't ask the server (sv empty)
// keeps the version a claim named, so its 6-hour backoff stands. A claim
// (the try begins) replaces the record whatever it said. A result for one
// release doesn't overwrite another wp's fresh claim on another: that
// download still runs, and its claim keeps others from starting a third. A
// state file that can't be read stays as it is: another's claim may be in it.
func (c *cli) recordUpdate(sv, result string, claim bool) {
	_ = c.claimStateWithin(5*time.Second, func(s *State) {
		if u := s.Update; !claim && sv != "" && claimLive(u) && u.Server != "" && u.Server != sv {
			return
		}
		if sv == "" && !claim && s.Update != nil {
			sv = s.Update.Server
		}
		s.Update = &updateCheck{Checked: time.Now(), Server: sv, Result: result}
	})
}

// autoUpdateInBackground starts `wp upgrade --auto` in a detached process
// at most once a day (the SessionStart hook calls it), so the hook never
// waits for the download. It covers servers that don't send their version
// with every answer.
func (c *cli) autoUpdateInBackground() {
	defer func() { _ = recover() }()
	if autoUpdateOff() || !isRelease(version) {
		return
	}
	c.claimAutoUpdate("", func(u *updateCheck) bool {
		return u == nil || (time.Since(u.Checked) >= autoUpdateEvery && !claimLive(u))
	})
}

// updateClaimFresh is how long a try's claim (Result "started") holds off
// every other wp's: longer than a download may take (wp upgrade's 10
// minutes), short enough that a claim whose process died doesn't stop
// updates for long.
const updateClaimFresh = 15 * time.Minute

// claimLive: u is a try's claim that is still fresh.
func claimLive(u *updateCheck) bool {
	return u != nil && u.Result == "started" && time.Since(u.Checked) < updateClaimFresh
}

// updateRetryEvery is how long after a try (a wp command's, or a runner's)
// wp waits before trying for the same server version again.
const updateRetryEvery = 6 * time.Hour

// serverVersionHeader is the header the server names its version in on every
// answer.
const serverVersionHeader = "X-Waypoint-Version"

// seeServerVersion remembers the version a server answer named.
func (c *cli) seeServerVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		c.sawVersion.Store(v)
	}
}

// autoUpdateIfBehind runs when a command has finished (run calls it, hooks
// included): if any answer it got showed the server on another release than
// this wp, it starts `wp upgrade --auto` detached, once per server version
// and again after 6 hours. It prints nothing and never fails the command.
// The foreground runner updates itself (runnerProc.updateStep, idle or busy)
// and wp upgrade is the update.
//
// Any other release than this wp's counts, an older one too: wp follows the
// server, so a rollback of the server rolls wp back with it.
func (c *cli) autoUpdateIfBehind() {
	defer func() { _ = recover() }()
	seen, _ := c.sawVersion.Load().(string)
	if seen == "" || seen == version || autoUpdateOff() || !isRelease(seen) || !isRelease(version) {
		return
	}
	if c.cur == runnerCmd || c.cur == updateCmd {
		return
	}
	c.claimAutoUpdate(seen, func(u *updateCheck) bool {
		// A "started" claim no longer live is a dead upgrader's: it holds
		// nothing off past updateClaimFresh (1847.3).
		return u == nil || ((u.Server != seen || u.Result == "started" || time.Since(u.Checked) >= updateRetryEvery) && !claimLive(u))
	})
}

// claimWait is how long a claim waits for the state lock.
const claimWait = 500 * time.Millisecond

// claimAutoUpdate starts the background upgrade when due says so of the
// state file's record, which is claimed (checked now, Result "started", for
// server version seen, empty when it isn't known) so wp processes that run
// during the download, and the runner, don't start another; due says no to
// a fresh claim. A process that couldn't be started gives the claim back.
func (c *cli) claimAutoUpdate(seen string, due func(*updateCheck) bool) {
	if !due(c.state.Update) {
		return
	}
	exe, err := selfExe()
	if err != nil || throwawayBuild(exe) {
		return
	}
	claimed := false
	// A hook may be cut short (SessionEnd gets 2 s): a missed claim is
	// harmless, the next command tries again. A claim that wasn't saved
	// starts nothing: no other wp would see it (c6983).
	err = c.claimStateWithin(claimWait, func(s *State) {
		if !due(s.Update) {
			return
		}
		if s.Update == nil {
			s.Update = &updateCheck{}
		}
		s.Update.Checked, s.Update.Server, s.Update.Result, claimed = time.Now(), seen, "started", true
	})
	if err == nil && claimed && !backgroundStart(exe, "upgrade", "--auto") {
		c.releaseUpdateClaim(seen)
	}
}

// backgroundStart runs a command detached, with no stdio of ours, so the
// hook's pipes close when the hook ends, and says whether it started (tests
// swap it).
var backgroundStart = func(exe string, args ...string) bool {
	cmd := exec.Command(exe, args...) // #nosec G204 -- our own binary
	cmd.SysProcAttr = detachedProc()
	if cmd.Start() != nil {
		return false
	}
	_ = cmd.Process.Release()
	return true
}

// releaseUpdateClaim removes the claim of a try for server version sv that
// was cut short (the runner stopped), so a wp command may start it again at
// once. A record of any other version, or with a result, stays, and so does a
// state file that can't be read.
func (c *cli) releaseUpdateClaim(sv string) {
	_ = c.claimStateWithin(5*time.Second, func(s *State) {
		if u := s.Update; u != nil && u.Server == sv && u.Result == "started" {
			s.Update = nil
		}
	})
}
