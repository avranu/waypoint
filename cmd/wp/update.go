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
binary. Runners and hooks do this themselves for release builds; set
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
// version and the path it replaced.
func (c *cli) selfUpdate(ctx context.Context, want string) (got, exe string, err error) {
	exe, err = selfExe()
	if err != nil {
		return "", "", err
	}
	if throwawayBuild(exe) {
		return "", exe, errors.New("this wp is a temporary build (go run, go test); install one with curl -fsSL " + orDash(c.server()) + "/cli | sh")
	}
	name := cliAsset()
	var sums bytes.Buffer
	if err := c.fetchPublic(ctx, "/cli/SHA256SUMS", &sums, 64<<10); err != nil {
		return "", exe, err
	}
	wantSum := checksumFor(sums.String(), name)
	if wantSum == "" {
		return "", exe, fmt.Errorf("the server's SHA256SUMS has no %s", name)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".wp-update-*"+ext)
	if err != nil {
		return "", exe, fmt.Errorf("can't write next to %s (%w); reinstall with curl -fsSL %s/cli | sh", exe, err, orDash(c.server()))
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	err = c.fetchPublic(ctx, "/cli/"+name, io.MultiWriter(tmp, h), 512<<20)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", exe, err
	}
	if hex.EncodeToString(h.Sum(nil)) != wantSum {
		return "", exe, fmt.Errorf("the downloaded %s doesn't match the server's SHA256SUMS; not installing it", name)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { // #nosec G302 -- an executable
		return "", exe, err
	}
	if got, err = binaryVersion(ctx, tmp.Name()); err != nil {
		return "", exe, err
	}
	if want != "" && got != want {
		return "", exe, fmt.Errorf("the server's wp says it is %s, not %s; not installing it", got, want)
	}
	if err := replaceExe(exe, tmp.Name()); err != nil {
		return "", exe, fmt.Errorf("can't replace %s: %w", exe, err)
	}
	return got, exe, nil
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
// overwritten but can be renamed, so the old one moves aside first.
func replaceExe(exe, next string) error {
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
	_ = c.updateState(func(s *State) {
		s.Update = &updateCheck{Checked: time.Now(), Server: sv, Result: result}
	})
}

// autoUpdateInBackground starts `wp upgrade --auto` in a detached process
// at most once a day (the SessionStart hook calls it), so the hook never
// waits for the download.
func (c *cli) autoUpdateInBackground() {
	defer func() { _ = recover() }()
	if autoUpdateOff() || !isRelease(version) {
		return
	}
	due := func(u *updateCheck) bool { return u == nil || time.Since(u.Checked) >= autoUpdateEvery }
	if !due(c.state.Update) {
		return
	}
	exe, err := selfExe()
	if err != nil || throwawayBuild(exe) {
		return
	}
	claimed := false
	_ = c.updateState(func(s *State) {
		if !due(s.Update) {
			return
		}
		if s.Update == nil {
			s.Update = &updateCheck{}
		}
		s.Update.Checked, claimed = time.Now(), true
	})
	if claimed {
		backgroundStart(exe, "upgrade", "--auto")
	}
}

// backgroundStart runs a command detached, with no stdio of ours, so the
// hook's pipes close when the hook ends (tests swap it).
var backgroundStart = func(exe string, args ...string) {
	cmd := exec.Command(exe, args...) // #nosec G204 -- our own binary
	cmd.SysProcAttr = detachedProc()
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
