//go:build linux

package sandboxd

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/forward"
)

// The units' properties (WP-470, PL7 §6.3; contained_props from the
// Phase 0 spike, WP-X8). Pure, so the tests check them line by line.

// unitPlan is what a Contained unit is made of.
type unitPlan struct {
	Run        string // the session's run directory
	Workspace  string // bound at /work
	Home       string // bound at /home/sandbox
	Toolchains []string
	Resources  sandbox.Resources
}

// systemView is the machine as every sandbox unit sees it: an empty,
// read-only root with /usr (and the merged-/usr links as directories),
// the kernel's API filesystems and no devices.
var systemView = []string{
	"TemporaryFileSystem=/:ro",
	"MountAPIVFS=yes",
	"BindReadOnlyPaths=/usr /usr/bin:/bin /usr/sbin:/sbin /usr/lib:/lib -/usr/lib64:/lib64",
	"PrivateDevices=yes",
	"PrivateIPC=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectClock=yes",
	"ProtectHostname=yes",
	"ProtectProc=invisible",
	"ProcSubset=pid",
	"KeyringMode=private",
	"NoNewPrivileges=yes",
	"CapabilityBoundingSet=",
	"AmbientCapabilities=",
	"RestrictNamespaces=yes",
	"RestrictSUIDSGID=yes",
	"RestrictRealtime=yes",
	"LockPersonality=yes",
	"SystemCallArchitectures=native",
	"SystemCallFilter=@system-service",
	// A refused call fails, as a container's would, instead of killing
	// the process: build tools probe.
	"SystemCallErrorNumber=EPERM",
	"Slice=" + sandbox.Slice,
}

// containedProps are a session unit's properties. Its network is its own
// loopback, where the forwarder listens; the broker's socket is the one
// way out.
func containedProps(p unitPlan) []string {
	r := p.Resources.WithDefaults()
	props := append([]string{}, systemView...)
	props = append(props,
		"PrivateNetwork=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"BindReadOnlyPaths=/etc/ssl -/etc/ca-certificates /etc/ld.so.cache -/etc/alternatives -/etc/localtime",
		"BindReadOnlyPaths="+p.Run+"/etc/passwd:/etc/passwd "+p.Run+"/etc/group:/etc/group "+
			p.Run+"/etc/hosts:/etc/hosts "+p.Run+"/etc/nsswitch.conf:/etc/nsswitch.conf",
		"BindReadOnlyPaths="+p.Run+"/etc/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt",
		"BindReadOnlyPaths="+p.Run+"/etc/ca.pem:"+sandboxEtc+"/ca.pem "+p.Run+"/launch.json:"+forward.LaunchFile,
		"BindPaths="+p.Run+"/broker:"+forward.BrokerDir,
		"BindPaths="+p.Workspace+":"+sandboxWork+" "+p.Home+":"+sandboxHome,
	)
	if len(p.Toolchains) > 0 {
		props = append(props, "BindReadOnlyPaths="+strings.Join(p.Toolchains, " "))
	}
	return append(props,
		fmt.Sprintf("TemporaryFileSystem=/tmp:mode=1777,size=%dM /dev/shm:mode=1777,size=512M", r.TmpMB),
		"WorkingDirectory="+sandboxWork,
		"UMask=0002",
		"MemoryMax="+strconv.Itoa(r.MemoryMB)+"M",
		"CPUQuota="+strconv.Itoa(r.CPUs*100)+"%",
		"TasksMax="+strconv.Itoa(r.Tasks),
		"RuntimeMaxSec="+strconv.Itoa(r.RuntimeSec),
		"TimeoutStopSec=30s",
	)
}

// brokerProps are a session broker's: wp-egress, the network, the
// sandbox CA as a credential, and nothing it can write but its socket's
// directory.
func brokerProps(run, state string) []string {
	return []string{
		"LoadCredential=ca.pem:" + state + "/ca.pem",
		"LoadCredential=ca-key.pem:" + state + "/ca-key.pem",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"PrivateIPC=yes",
		"ReadWritePaths=" + run + "/broker",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"ProtectProc=invisible",
		"ProcSubset=pid",
		"KeyringMode=private",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=",
		"AmbientCapabilities=",
		"RestrictNamespaces=yes",
		"RestrictSUIDSGID=yes",
		"RestrictRealtime=yes",
		"LockPersonality=yes",
		"MemoryDenyWriteExecute=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"SystemCallFilter=~@privileged @resources",
		// The socket is the session's group's: 0770.
		"UMask=0007",
		"MemoryMax=512M",
		"TasksMax=512",
		"TimeoutStopSec=10s",
		"Slice=" + sandbox.Slice,
	}
}

// cleanProps are the cleaner's: the repo user with nothing but the
// session's store directory, at /s, and no network.
func cleanProps(dir string) []string {
	return append(append([]string{}, systemView...),
		"PrivateNetwork=yes",
		"RestrictAddressFamilies=AF_UNIX",
		"BindPaths="+dir+":/s",
		"UMask=0077",
		"MemoryMax=512M",
		"TasksMax=64",
		"RuntimeMaxSec=3600",
	)
}

// unitPath says whether a path can be written in a unit property as is:
// absolute, clean, without the characters systemd's path lists split on
// or expand.
func unitPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsAny(p, " \t\n:\\%\"'$;") && p == filepath.Clean(p)
}
