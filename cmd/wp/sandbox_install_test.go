package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

func TestInstallSandboxScript(t *testing.T) {
	o := sandboxInstallOpts{
		Base: "https://wp.example/it's", Key: "ed25519:AAAA", Fingerprint: "SHA256:xyz",
		RunnerUID: 1000, RunnerID: 7, Who: "robin (uid 1000)", Host: "devhost",
		StoreFrom: "/mnt/Store/waypoint-store", Version: "test",
	}
	s := installSandboxScript(o)
	for _, want := range []string{
		`base='https://wp.example/it'\''s'`,
		"ExecStart=" + sandbox.Binary + " --runner-uid 1000 --runner-id 7",
		"What=/mnt/Store/waypoint-store",
		"Where=" + sandbox.StoreDir,
		"systemctl enable --now " + sandbox.StoreMnt,
		"printf '%s\\n' 'ed25519:AAAA' > " + sandbox.KeyFile + ".new",
		"Fingerprint SHA256:xyz",
		"checksum mismatch",
		// the capabilities tried on devhost (WP-448), and no
		// RestrictSUIDSGID, which fails openat2
		"\nCapabilityBoundingSet=CAP_CHOWN CAP_FOWNER CAP_DAC_READ_SEARCH\n",
		"\nNoNewPrivileges=yes\n",
		// Contained sessions (WP-470): the brokers' user and the slices
		"useradd --system --gid " + sandbox.EgressUser,
		"cat > /etc/systemd/system/" + sandbox.Slice,
		"\nMemoryMax=22G\n",
		"\nCPUQuota=600%\n",
		"\nMemoryLow=20G\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "RestrictSUIDSGID") || strings.Contains(s, "DAC_OVERRIDE") {
		t.Error("the unit has RestrictSUIDSGID or CAP_DAC_OVERRIDE")
	}
	// A Documentation= link would point at one Waypoint's own notes, which
	// another install doesn't have (WP-459).
	if strings.Contains(s, "Documentation=") || strings.Contains(s, "/design/") {
		t.Error("the unit links to a page outside the tree")
	}
	o.StoreFrom, o.RunnerID = "", 0
	s2 := installSandboxScript(o)
	if strings.Contains(s2, "[Mount]") || strings.Contains(s2, "--runner-id") {
		t.Errorf("no --store-from or --runner-id, but the script has them:\n%s", s2)
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		for _, script := range []string{s, s2} {
			f := filepath.Join(t.TempDir(), "install.sh")
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(sh, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("sh -n: %v: %s", err, out)
			}
		}
	}
}

func TestMountInfo(t *testing.T) {
	lines := []string{
		`22 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/root rw`,
		`40 22 8:17 / /mnt/Store rw,relatime shared:20 - xfs /dev/sdb1 rw,attr2,inode64,prjquota`,
		`41 22 8:17 /waypoint-store /var/lib/waypoint/store rw,nosuid,nodev,relatime shared:20 - xfs /dev/sdb1 rw,attr2,inode64,prjquota`,
		`42 22 0:50 / /mnt/with\040space rw - tmpfs tmpfs rw`,
		`garbage`,
	}
	var mounts []mountEntry
	for _, l := range lines {
		if m, ok := parseMountInfo(l); ok {
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 4 {
		t.Fatalf("parsed %d mounts, want 4", len(mounts))
	}
	if mounts[3].Point != "/mnt/with space" {
		t.Errorf("unescaped point %q", mounts[3].Point)
	}
	m := mountOf(mounts, "/var/lib/waypoint/store")
	if m == nil || m.Root != "/waypoint-store" || !m.hasOpt("prjquota") {
		t.Fatalf("store mount = %+v", m)
	}
	if got := mountOf(mounts, "/var/lib/waypoint/storex"); got == nil || got.Point != "/" {
		t.Errorf("a sibling path matched %+v", got)
	}
	if got := mountOf(mounts, "/mnt/Store/a/b"); got == nil || got.Point != "/mnt/Store" {
		t.Errorf("/mnt/Store/a/b is on %+v", got)
	}
}

func TestOpenAbove(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o777); err != nil { // world-writable, no sticky bit
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "ok", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	mounts := []mountEntry{
		{Root: "/", Point: base, FSType: "xfs", Source: "/dev/x"},
		{Root: "/ok/store", Point: "/var/lib/waypoint/store", FSType: "xfs", Source: "/dev/x"},
	}
	got := openAbove(mounts, &mounts[1])
	if len(got) != 1 || got[0] != base {
		t.Fatalf("openAbove = %v, want [%s]", got, base)
	}
	if err := os.Chmod(base, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	if got := openAbove(mounts, &mounts[1]); len(got) != 0 {
		t.Errorf("sticky, but openAbove = %v", got)
	}
}

func TestRunnersCheck(t *testing.T) {
	m := func(id int64, name, host string) cliSandboxMachine {
		var x cliSandboxMachine
		x.Runner.ID, x.Runner.Name, x.Runner.Hostname, x.Runner.Owner.Username = id, name, host, "robin"
		return x
	}
	ms := []cliSandboxMachine{m(1, "devhost", "devhost"), m(2, "devhost-2", "Devhost"), m(3, "runner2", "runner2")}
	for _, tc := range []struct {
		ms     []cliSandboxMachine
		pinned int64
		state  string
		want   string
	}{
		{ms, 1, checkOK, "only for devhost (#1, robin's); the others here (devhost-2 (#2, robin's))"},
		{ms[:1], 1, checkOK, "the one runner here"},
		{ms, 3, checkWarn, "runner #3, which isn't one of the runners here"},
		{ms[2:], 1, checkWarn, "you can see no runner on devhost"},
		{ms, 0, checkWarn, "2 runners here"},
		{ms[:1], 0, checkInfo, "the one here is devhost (#1"},
		{nil, 0, checkInfo, "you can see no runner"},
	} {
		state, detail := runnersCheck(tc.ms, "devhost", tc.pinned)
		if state != tc.state || !strings.Contains(detail, tc.want) {
			t.Errorf("pinned %d over %d: %s %q", tc.pinned, len(tc.ms), state, detail)
		}
	}
}
