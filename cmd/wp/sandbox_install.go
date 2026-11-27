package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

var sandboxInstallCmd = &command{
	name: "install", args: "[--store-from DIR] [--runner-uid N] [--runner-id N]",
	summary: "print the root script that installs this machine's sandbox helper",
	about: `Prints a shell script that installs the sandbox helper (waypoint-sandboxd).
Read it, then run it as root:

    wp sandbox install > install-sandbox.sh
    sudo sh install-sandbox.sh

It downloads the helper and checks its SHA-256, pins the server's spec key
(compare the fingerprint with Admin → Sandbox), makes the store (` + sandbox.StoreDir + `)
and installs a hardened systemd service that answers only the runner's user.
Run again to update. Extra machine hardening is listed as comments at the
end, to do by hand; wp sandbox doctor checks it all.

--runner-id pins the helper to one runner (several on a machine).
--store-from keeps the store on another filesystem (XFS gives reflinks).`,
	examples: []string{"wp sandbox install > install-sandbox.sh", "wp sandbox install --store-from /mnt/data/waypoint-store"},
	run:      sandboxInstall,
}

var sandboxDoctorCmd = &command{
	name:    "doctor",
	summary: "check this machine's sandbox helper, store and hardening",
	about: `Checks that the helper runs and answers the runner's user, that its store
is sound (root-owned, own mount, XFS with project quotas), the machine
hardening, and whether KVM is there for the vm level. Exits 1 when a
check fails.`,
	examples: []string{"wp sandbox doctor", "wp sandbox doctor --json"},
	run:      sandboxDoctor,
}

func init() {
	sandboxCmd.subs = append(sandboxCmd.subs, sandboxInstallCmd, sandboxDoctorCmd)
}

type cliSandboxKey struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

func (c *cli) sandboxKey() (*cliSandboxKey, error) {
	var k cliSandboxKey
	if err := c.callInto(context.Background(), "GET", "/api/v1/sandbox/key", nil, &k); err != nil {
		return nil, err
	}
	if _, err := sandbox.ParseKey(k.Key); err != nil {
		return nil, fmt.Errorf("the server's sandbox key: %w", err)
	}
	return &k, nil
}

func sandboxInstall(c *cli, args []string) error {
	fs := c.flags()
	from := fs.String("store-from", "", "`DIR`: keep the store here, bind-mounted at "+sandbox.StoreDir)
	uid := fs.Int("runner-uid", 0, "`N`: runner's uid (default yours)")
	runnerID := fs.Int64("runner-id", 0, "`N`: take specs only for this runner")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "runner-uid" })
	if !given {
		*uid = os.Getuid()
	}
	if runtime.GOOS != "linux" {
		return errors.New("the sandbox helper runs on Linux only")
	}
	if *uid <= 0 {
		return c.usage("--runner-uid must be the runner's own user, not root (run wp sandbox install as that user, without sudo)")
	}
	if *from != "" && (!filepath.IsAbs(*from) || filepath.Clean(*from) == sandbox.StoreDir) {
		return c.usage("--store-from must be an absolute directory other than %s", sandbox.StoreDir)
	}
	base := c.server()
	if strings.IndexFunc(*from, unicode.IsControl) >= 0 || strings.IndexFunc(base, unicode.IsControl) >= 0 {
		return c.usage("--store-from and the server URL must not contain control characters")
	}
	if strings.ContainsAny(*from, " \\%\"'") {
		return c.usage("--store-from must not contain spaces, quotes, backslashes or %%")
	}
	key, err := c.sandboxKey()
	if err != nil {
		return err
	}
	who := strconv.Itoa(*uid)
	if u, err := user.LookupId(who); err == nil {
		who = u.Username + " (uid " + who + ")"
	}
	host, _ := os.Hostname()
	_, err = fmt.Fprint(c.out, installSandboxScript(sandboxInstallOpts{
		Base: base, Key: key.Key, Fingerprint: key.Fingerprint, RunnerUID: *uid, RunnerID: *runnerID,
		Who: who, Host: host, StoreFrom: filepath.Clean(*from), Version: version,
	}))
	return err
}

type sandboxInstallOpts struct {
	Base, Key, Fingerprint string
	RunnerUID              int
	RunnerID               int64
	Who, Host, StoreFrom   string
	Version                string
}

// shq quotes s for sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sandboxUnit is the helper's systemd unit: root, but kept away from the
// rest of the machine, and with only the capabilities its work needs
// (WP-448: on a dev host, each of the three is needed and nothing else is).
// Root without CAP_DAC_OVERRIDE still writes what root owns (/etc/passwd,
// /etc/shadow at 0640, the ledger, the store's top) but not a sandbox's own
// files. No RestrictSUIDSGID: under it systemd fails every openat2 (it can't
// see the mode inside openat2's struct), and the store needs openat2's
// RESOLVE_BENEATH; without CAP_FSETID the helper can't make a set-id file
// that isn't root's own, and it makes none.
// No Documentation=: design note numbers are per instance, and the script is
// generated for whichever server the CLI points at.
func sandboxUnit(start string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Waypoint sandbox helper",
		"RequiresMountsFor=" + sandbox.StoreDir,
		"",
		"[Service]",
		"ExecStart=" + start,
		"Restart=on-failure",
		"RestartSec=5",
		"UMask=0077",
		"# CAP_CHOWN: hand a machine user its directories and the runner its socket;",
		"# CAP_FOWNER: set the mode of what it just handed over;",
		"# CAP_DAC_READ_SEARCH: open a machine user's 0700 directories to hand them",
		"#   back or archive them (never to write in them).",
		"CapabilityBoundingSet=CAP_CHOWN CAP_FOWNER CAP_DAC_READ_SEARCH",
		"NoNewPrivileges=yes",
		"ProtectSystem=yes",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateNetwork=yes",
		"RestrictAddressFamilies=AF_UNIX AF_NETLINK",
		"ProtectKernelModules=yes",
		"ProtectKernelTunables=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"LockPersonality=yes",
		"RestrictRealtime=yes",
		"MemoryDenyWriteExecute=yes",
		"SystemCallArchitectures=native",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

func installSandboxScript(o sandboxInstallOpts) string {
	if o.StoreFrom == "." {
		o.StoreFrom = ""
	}
	start := sandbox.Binary + " --runner-uid " + strconv.Itoa(o.RunnerUID)
	if o.RunnerID > 0 {
		start += " --runner-id " + strconv.FormatInt(o.RunnerID, 10)
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/sh")
	w("# Installs Waypoint's sandbox helper (waypoint-sandboxd) on %s for", o.Host)
	w("# the runner user %s, from %s.", o.Who, o.Base)
	w("# Written by wp sandbox install (wp %s); read it, then run it as root.", o.Version)
	w("set -eu")
	w(`[ "$(id -u)" = 0 ] || { echo "run this as root (sudo sh $0)" >&2; exit 1; }`)
	w("base=%s", shq(o.Base))
	w("")
	w("# 1. The helper, checked against the server's SHA256SUMS.")
	w(`case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "no helper build for $(uname -m)" >&2; exit 1 ;; esac`)
	w(`file="waypoint-sandboxd-linux-$arch"`)
	w(`tmp=$(mktemp -d)`)
	w(`trap 'rm -rf "$tmp"' EXIT`)
	w(`curl -fsSL "$base/cli/$file" -o "$tmp/$file"`)
	w(`curl -fsSL "$base/cli/SHA256SUMS" -o "$tmp/SHA256SUMS"`)
	w(`want=$(grep " $file\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)`)
	w(`got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)`)
	w(`if [ -z "$want" ] || [ "$want" != "$got" ]; then echo "checksum mismatch: not installing" >&2; exit 1; fi`)
	w(`install -D -o root -g root -m 0755 "$tmp/$file" %s`, sandbox.Binary)
	w("")
	w("# 2. The server's spec key, which the helper trusts and nothing else.")
	w("#    Fingerprint %s: compare it with Admin -> Sandbox.", o.Fingerprint)
	w("install -d -o root -g root -m 0755 %s", filepath.Dir(sandbox.KeyFile))
	w(`if [ -f %[1]s ] && [ "$(cat %[1]s)" != %[2]s ]; then echo "replacing the pinned key in %[1]s"; fi`, sandbox.KeyFile, shq(o.Key))
	w("printf '%%s\\n' %s > %s.new", shq(o.Key), sandbox.KeyFile)
	w("chmod 0644 %s.new && mv %s.new %s", sandbox.KeyFile, sandbox.KeyFile, sandbox.KeyFile)
	w("")
	w("# 3. The helper's ledger, and the store: every sandbox's files, root-owned.")
	w("install -d -o root -g root -m 0755 %s", filepath.Dir(sandbox.StoreDir))
	w("install -d -o root -g root -m 0700 %s", sandbox.StateDir)
	w("install -d -o root -g root -m 0755 %s", sandbox.StoreDir)
	if o.StoreFrom != "" {
		w("install -d -o root -g root -m 0755 %s", shq(o.StoreFrom))
		w("cat > /etc/systemd/system/%s <<'UNIT'", sandbox.StoreMnt)
		w("[Unit]")
		w("Description=Waypoint sandbox store (bind from %s)", o.StoreFrom)
		w("RequiresMountsFor=%s", o.StoreFrom)
		w("")
		w("[Mount]")
		w("What=%s", o.StoreFrom)
		w("Where=%s", sandbox.StoreDir)
		w("Type=none")
		w("Options=bind,nosuid,nodev")
		w("")
		w("[Install]")
		w("WantedBy=local-fs.target")
		w("UNIT")
	}
	w("")
	w("# 4. Contained sessions (PL7 §6.3, §6.7): the user their egress brokers run")
	w("#    as, the slice every sandbox shares, and the services' memory floor.")
	w("getent group %[1]s >/dev/null || groupadd --system %[1]s", sandbox.EgressUser)
	w("getent passwd %[1]s >/dev/null || useradd --system --gid %[1]s --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment 'Waypoint sandbox egress' %[1]s", sandbox.EgressUser)
	w("cat > /etc/systemd/system/%s <<'UNIT'", sandbox.Slice)
	w("[Unit]")
	w("Description=Waypoint sandboxes")
	w("")
	w("[Slice]")
	w("# Above MemoryHigh sandboxes are slowed and reclaimed first; services keep theirs.")
	w("MemoryHigh=20G")
	w("MemoryMax=22G")
	w("CPUWeight=50")
	w("CPUQuota=600%%")
	w("TasksMax=32768")
	w("UNIT")
	w("install -d -o root -g root -m 0755 /etc/systemd/system/system.slice.d")
	w("cat > /etc/systemd/system/system.slice.d/waypoint-sandbox.conf <<'UNIT'")
	w("# Services' working set: the kernel doesn't reclaim it for sandboxes.")
	w("[Slice]")
	w("MemoryLow=20G")
	w("UNIT")
	w("")
	w("# 5. The service: root, but kept away from the rest of the machine.")
	w("cat > /etc/systemd/system/%s <<'UNIT'", sandbox.Unit)
	b.WriteString(sandboxUnit(start))
	w("UNIT")
	w("systemctl daemon-reload")
	if o.StoreFrom != "" {
		w("systemctl enable --now %s", sandbox.StoreMnt)
	}
	w("systemctl enable %s", sandbox.Unit)
	w("systemctl restart %s", sandbox.Unit)
	w(`echo "Installed. As the runner's user, check it with: wp sandbox doctor"`)
	w("")
	w("# 6. By hand, once (PL7 §10.2); wp sandbox doctor says which are missing:")
	w("#  - XFS project quotas on the store's filesystem, so one sandbox can't")
	w("#    fill the disk: add prjquota to its options in /etc/fstab and remount")
	w("#    (an XFS root filesystem needs rootflags=prjquota on the kernel line).")
	w("#  - A sticky bit on any world-writable directory above the store, so one")
	w("#    user can't rename another's files:   chmod +t DIR")
	w("#  - No blanket git safe.directory, which lets git trust any user's repo:")
	w("#      git config --system --unset-all safe.directory '*'")
	w("#      (and --global, as the runner's user)")
	return b.String()
}

// runnersCheck says which of the server's runners are on this host and
// which one the helper takes specs for (WP-448).
func runnersCheck(machines []cliSandboxMachine, host string, pinned int64) (state, detail string) {
	var here, others []string
	pinnedHere := ""
	for _, m := range machines {
		r := m.Runner
		if !strings.EqualFold(r.Hostname, host) {
			continue
		}
		name := fmt.Sprintf("%s (#%d, %s's)", r.Name, r.ID, r.Owner.Username)
		here = append(here, name)
		if r.ID == pinned {
			pinnedHere = name
		} else {
			others = append(others, name)
		}
	}
	list := strings.Join(here, ", ")
	switch {
	case pinned > 0 && pinnedHere != "":
		if len(here) == 1 {
			return checkOK, "the helper takes specs only for " + pinnedHere + ", the one runner here"
		}
		return checkOK, fmt.Sprintf("the helper takes specs only for %s; the others here (%s) get none", pinnedHere, strings.Join(others, ", "))
	case pinned > 0 && len(here) == 0:
		return checkWarn, fmt.Sprintf("the helper takes specs only for runner #%d, but you can see no runner on %s", pinned, host)
	case pinned > 0:
		return checkWarn, fmt.Sprintf("the helper takes specs only for runner #%d, which isn't one of the runners here you can see (%s): wp sandbox install --runner-id N pins another", pinned, list)
	case len(here) == 0:
		return checkInfo, fmt.Sprintf("you can see no runner on %s; the helper takes specs for any runner", host)
	case len(here) == 1:
		return checkInfo, "the helper takes specs for any runner; the one here is " + list
	}
	return checkWarn, fmt.Sprintf("%d runners here (%s) and the helper takes specs for any of them: wp sandbox install --runner-id N pins it to one", len(here), list)
}

// A doctor check's outcome.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "FAIL"
	checkInfo = "info"
)

type doctorCheck struct {
	State  string `json:"state"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func sandboxDoctor(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("the sandbox helper runs on Linux only")
	}
	var checks []doctorCheck
	add := func(state, name, format string, a ...any) {
		checks = append(checks, doctorCheck{state, name, fmt.Sprintf(format, a...)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := sandbox.Call(ctx, sandboxHelperSocket, sandbox.Request{Op: sandbox.OpStatus})
	var st *sandbox.Status
	switch {
	case err != nil:
		add(checkFail, "helper", "%v", err)
	case resp.Status == nil:
		add(checkFail, "helper", "it answered without a status")
	default:
		st = resp.Status
		add(checkOK, "helper", "waypoint-sandboxd %s answers %s", st.Version, sandboxHelperSocket)
	}
	if key, err := c.sandboxKey(); err != nil {
		add(checkWarn, "key", "can't read the server's key: %v", err)
	} else if st != nil {
		if st.Key == key.Fingerprint {
			add(checkOK, "key", "pins this server's key %s", key.Fingerprint)
		} else {
			add(checkFail, "key", "pins %s, but this server signs with %s: run wp sandbox install again", st.Key, key.Fingerprint)
		}
	}
	if st != nil {
		if st.RunnerUID != os.Getuid() {
			add(checkWarn, "runner user", "the helper answers uid %d, not yours (%d): run wp runner as that user", st.RunnerUID, os.Getuid())
		}
		s := st.Store
		switch {
		case !s.OK:
			add(checkFail, "store", "%s: %s", s.Path, s.Problem)
		case !s.Mounted:
			add(checkWarn, "store", "%s is on the same filesystem as its parent; --store-from puts it on a filesystem of its own", s.Path)
		default:
			add(checkOK, "store", "%s, its own mount, %s free", s.Path, humanBytes(s.Free))
		}
		if s.OK && s.FSType != "xfs" {
			add(checkWarn, "store filesystem", "%s isn't XFS (%s): no reflink snapshots or project quotas", s.Path, s.FSType)
		}
		if s.OK && s.Free > 0 && s.Free < 10<<30 {
			add(checkWarn, "disk", "only %s free in the store", humanBytes(s.Free))
		}
		for _, d := range st.Drift {
			add(checkWarn, "ledger", "%s", d)
		}
		if st.Pending > 0 {
			add(checkWarn, "ledger", "%d interrupted operation(s) the helper couldn't finish: see journalctl -u %s", st.Pending, sandbox.Unit)
		}
		add(checkInfo, "users", "%d machine user(s), %d uid(s) retired, %d removed", len(st.Users), st.Retired, len(st.Removed))
		var machines []cliSandboxMachine
		if err := c.callInto(ctx, "GET", "/api/v1/sandbox/machines", nil, &machines); err != nil {
			add(checkWarn, "runners", "can't list the server's runners: %v", err)
		} else {
			host, _ := os.Hostname()
			state, detail := runnersCheck(machines, host, st.RunnerID)
			add(state, "runners", "%s", detail)
		}
		if st.KVM {
			add(checkInfo, "kvm", "/dev/kvm is there: the vm level can work")
		} else {
			add(checkInfo, "kvm", "no /dev/kvm: the strongest level here is contained")
		}
	}
	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		add(checkWarn, "mounts", "can't read /proc/self/mountinfo, so the quota and sticky-bit checks were skipped: %v", err)
		mounts = nil
	}
	if m := mountOf(mounts, sandbox.StoreDir); m != nil {
		if m.FSType == "xfs" && !m.hasOpt("prjquota", "pquota") {
			add(checkWarn, "quotas", "%s (%s) has no project quotas: add prjquota to its mount options", m.Point, m.Source)
		} else if m.FSType == "xfs" {
			add(checkOK, "quotas", "project quotas on %s", m.Point)
		}
		for _, dir := range openAbove(mounts, m) {
			add(checkFail, "sticky bit", "%s is world-writable without the sticky bit: chmod +t %s", dir, dir)
		}
	}
	for _, scope := range []string{"--system", "--global"} {
		out, err := exec.Command("git", "config", scope, "--get-all", "safe.directory").Output() // #nosec G204 -- fixed arguments
		var ee *exec.ExitError
		if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
			add(checkWarn, "git", "couldn't read the %s git config, so safe.directory '*' wasn't checked: %v", strings.TrimPrefix(scope, "--"), err)
			continue
		}
		for _, v := range strings.Fields(string(out)) {
			if v == "*" {
				add(checkFail, "git", "safe.directory '*' (%s) lets git trust any user's repository: git config %s --unset-all safe.directory '*'", strings.TrimPrefix(scope, "--"), scope)
			}
		}
	}
	if why := rootEquivalent(); why != "" {
		add(checkInfo, "your user", "root-equivalent (%s): a sandbox keeps a job from your files, but not from what your own user can do", why)
	}

	return c.reportChecks(checks, "")
}

func doctorText(checks []doctorCheck) string {
	var b strings.Builder
	for _, ch := range checks {
		fmt.Fprintf(&b, "%-4s  %-16s %s\n", ch.State, ch.Name, ch.Detail)
	}
	return b.String()
}

// humanBytes is a size like 12.3 GB.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	Root, Point, FSType, Source string
	Opts                        []string // per-mount and superblock options
}

func (m *mountEntry) hasOpt(names ...string) bool {
	return slices.ContainsFunc(m.Opts, func(o string) bool { return slices.Contains(names, o) })
}

func readMountInfo(path string) ([]mountEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- /proc/self/mountinfo
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []mountEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m, ok := parseMountInfo(sc.Text()); ok {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// parseMountInfo reads "id parent maj:min root point opts [tags] - fstype
// source superopts".
func parseMountInfo(line string) (mountEntry, bool) {
	pre, post, ok := strings.Cut(line, " - ")
	if !ok {
		return mountEntry{}, false
	}
	a, b := strings.Fields(pre), strings.Fields(post)
	if len(a) < 6 || len(b) < 3 {
		return mountEntry{}, false
	}
	m := mountEntry{Root: unescapeMount(a[3]), Point: unescapeMount(a[4]), FSType: b[0], Source: unescapeMount(b[1])}
	m.Opts = append(strings.Split(a[5], ","), strings.Split(b[2], ",")...)
	return m, true
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountOf is the mount path is on: the last mounted at the longest
// prefix of it.
func mountOf(mounts []mountEntry, path string) *mountEntry {
	var best *mountEntry
	for i := range mounts {
		m := &mounts[i]
		if (path == m.Point || strings.HasPrefix(path, strings.TrimSuffix(m.Point, "/")+"/")) && (best == nil || len(m.Point) >= len(best.Point)) {
			best = m
		}
	}
	return best
}

// openAbove lists the world-writable, non-sticky directories above the
// store on its filesystem, as mounted at its root elsewhere: on a dev host
// the store is a bind of /mnt/store/waypoint-store, so /mnt/store.
func openAbove(mounts []mountEntry, store *mountEntry) []string {
	base := ""
	for _, m := range mounts {
		if m.Source == store.Source && m.FSType == store.FSType && m.Root == "/" {
			base = m.Point
			break
		}
	}
	if base == "" {
		return nil
	}
	var out []string
	rel := strings.Trim(store.Root, "/")
	dir := base
	parts := []string{""}
	if rel != "" {
		parts = append(parts, strings.Split(rel, "/")...)
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		if i > 0 {
			dir = filepath.Join(dir, p)
		}
		fi, err := os.Stat(dir)
		if err == nil && fi.IsDir() && fi.Mode().Perm()&0o002 != 0 && fi.Mode()&os.ModeSticky == 0 {
			out = append(out, dir)
		}
	}
	return out
}

// rootEquivalent says why the current user can become root without a
// password, or "".
func rootEquivalent() string {
	if os.Getuid() == 0 {
		return "it is root"
	}
	u, err := user.Current()
	if err == nil {
		gids, _ := u.GroupIds()
		for _, gid := range gids {
			if g, err := user.LookupGroupId(gid); err == nil && slices.Contains([]string{"docker", "lxd", "libvirt", "disk", "sudo", "wheel", "admin"}, g.Name) {
				if g.Name == "sudo" || g.Name == "wheel" || g.Name == "admin" {
					if exec.Command("sudo", "-n", "true").Run() == nil {
						return "passwordless sudo"
					}
					continue
				}
				return "group " + g.Name
			}
		}
	}
	return ""
}
