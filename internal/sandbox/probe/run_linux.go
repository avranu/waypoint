//go:build linux

package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Run tries everything in t from inside a session and writes each Case to
// out as a JSON line. It never stops early: a try that can't be judged is
// inconclusive, and the rest still run.
func Run(ctx context.Context, t Targets, out io.Writer) {
	enc := json.NewEncoder(out)
	emit := func(c Case) { _ = enc.Encode(c) }

	// Controls: a session's own places are writable.
	for _, dir := range []string{"/work", "/tmp", "/dev/shm"} {
		c := Case{Name: "control: write " + dir, Group: GroupFiles, Want: Allowed, Got: Allowed}
		if t.Plant == "" {
			c.Got, c.Detail = Inconclusive, "no file name to write"
		} else if err := os.WriteFile(filepath.Join(dir, t.Plant), []byte("waypoint drill\n"), 0o600); err != nil { // #nosec G304 G703 -- the drill's own file in the session's own directories
			c.Got, c.Detail = Refused, err.Error()
		}
		emit(c)
	}

	for _, p := range t.Read {
		emit(tryRead(p))
	}
	for _, p := range t.List {
		emit(tryList(p))
	}
	for _, d := range t.Write {
		emit(tryWrite(d))
	}
	if t.HostShm != "" {
		c := Case{Name: "see the host's /dev/shm", Group: GroupFiles, Want: Refused, Got: Refused}
		if _, err := os.Stat(filepath.Join("/dev/shm", t.HostShm)); err == nil {
			c.Got, c.Detail = Allowed, "the runner's /dev/shm/"+t.HostShm+" is visible"
		}
		emit(c)
	}
	emit(tryProcs())

	for _, s := range t.Sockets {
		emit(tryDial(GroupNetwork, "connect to "+s, "unix", s))
	}
	for _, a := range t.Dial {
		emit(tryDial(GroupNetwork, "reach "+a, "tcp", a))
	}

	for _, c := range kernelTries() {
		emit(c)
	}
	if t.TmpMB > 0 {
		emit(tryFillTmp(t.TmpMB))
	}

	for _, r := range t.Egress {
		emit(TryEgress(ctx, t.Proxy, r, nil))
	}
	emit(Case{Name: CaseFinished, Group: GroupResources, Want: Allowed, Got: Allowed})
}

func tryRead(p string) Case {
	c := Case{Name: "read " + p, Group: GroupFiles, Want: Refused, Got: Refused}
	f, err := os.Open(p) // #nosec G304 -- the drill's target list
	if err != nil {
		c.Detail = errText(err)
		return c
	}
	defer func() { _ = f.Close() }()
	n, err := f.Read(make([]byte, 64))
	if n > 0 {
		c.Got, c.Detail = Allowed, fmt.Sprintf("read %d bytes", n)
	} else if err != nil && !errors.Is(err, io.EOF) {
		c.Detail = errText(err)
	} else {
		c.Got, c.Detail = Allowed, "opened (empty)"
	}
	return c
}

func tryList(p string) Case {
	c := Case{Name: "list " + p, Group: GroupFiles, Want: Refused, Got: Refused}
	ents, err := os.ReadDir(p)
	switch {
	case err != nil:
		c.Detail = errText(err)
	case len(ents) == 0:
		c.Detail = "empty"
	default:
		c.Got, c.Detail = Allowed, fmt.Sprintf("%d entries, first %s", len(ents), ents[0].Name())
	}
	return c
}

func tryWrite(dir string) Case {
	c := Case{Name: "write into " + dir, Group: GroupFiles, Want: Refused, Got: Refused}
	p := filepath.Join(dir, fmt.Sprintf(".waypoint-drill-%d", os.Getpid()))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- the drill's target list
	if err != nil {
		c.Detail = errText(err)
		return c
	}
	_ = f.Close()
	_ = os.Remove(p)
	c.Got, c.Detail = Allowed, "created "+p
	return c
}

// tryProcs looks for other users' processes, which ProtectProc=invisible
// hides.
func tryProcs() Case {
	c := Case{Name: "see other users' processes (/proc/*/cmdline)", Group: GroupFiles, Want: Refused, Got: Refused}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		c.Got, c.Detail = Inconclusive, errText(err)
		return c
	}
	me := os.Getuid()
	var others []string
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+e.Name(), &st) == nil && int(st.Uid) != me {
			others = append(others, e.Name())
		}
	}
	if len(others) > 0 {
		c.Got, c.Detail = Allowed, fmt.Sprintf("%d processes of other users, e.g. pid %s", len(others), others[0])
	}
	return c
}

func tryDial(group, name, network, addr string) Case {
	c := Case{Name: name, Group: group, Want: Refused, Got: Refused}
	conn, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		c.Detail = errText(err)
		return c
	}
	_ = conn.Close()
	c.Got, c.Detail = Allowed, "connected"
	return c
}

// kernelTries are the kernel surface a session must not have: new
// namespaces, mounts, raw and host-facing socket families, root, and
// privileges.
func kernelTries() []Case {
	var out []Case
	k := func(name string, err error, detail string) {
		c := Case{Name: name, Group: GroupKernel, Want: Refused, Got: Refused}
		if err == nil {
			c.Got, c.Detail = Allowed, detail
		} else {
			c.Detail = errText(err)
		}
		out = append(out, c)
	}

	self, err := os.Executable()
	if err == nil {
		cmd := exec.Command(self, "probe", "--hold") // #nosec G204 -- this binary
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS}
		cmd.Stdin = strings.NewReader("")
		err = cmd.Run()
	}
	k("make a user namespace", err, "a child ran in a new user and mount namespace")

	dir, err := os.MkdirTemp("", "mnt")
	if err == nil {
		err = unix.Mount("none", dir, "tmpfs", 0, "")
		if err == nil {
			_ = unix.Unmount(dir, 0)
		}
		_ = os.Remove(dir)
	}
	k("mount a tmpfs", err, "mounted")

	for _, f := range []struct {
		name   string
		family int
		typ    int
	}{
		{"open an AF_VSOCK socket", unix.AF_VSOCK, unix.SOCK_STREAM},
		{"open an AF_NETLINK socket", unix.AF_NETLINK, unix.SOCK_RAW},
		{"open an AF_PACKET socket", unix.AF_PACKET, unix.SOCK_RAW},
		{"open a raw IP socket", unix.AF_INET, unix.SOCK_RAW},
	} {
		fd, err := unix.Socket(f.family, f.typ, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		k(f.name, err, "opened")
	}

	k("become root (setuid 0)", unix.Setuid(0), "now uid 0")

	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		out = append(out, Case{Name: "read /proc/self/status", Group: GroupKernel, Want: Allowed, Got: Refused, Detail: errText(err)})
		return out
	}
	field := func(name string) string {
		sc := bufio.NewScanner(bytes.NewReader(status))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), name+":"); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	nnp := Case{Name: "run without no_new_privs", Group: GroupKernel, Want: Refused, Got: Refused, Detail: "NoNewPrivs " + field("NoNewPrivs")}
	if field("NoNewPrivs") != "1" {
		nnp.Got = Allowed
	}
	caps := Case{Name: "hold a capability", Group: GroupKernel, Want: Refused, Got: Refused}
	for _, f := range []string{"CapEff", "CapPrm", "CapBnd", "CapAmb"} {
		if v := field(f); strings.Trim(v, "0") != "" {
			caps.Got, caps.Detail = Allowed, f+" "+v
			break
		}
	}
	return append(out, nnp, caps)
}

// tryFillTmp writes /tmp past its size; the tmpfs must stop it.
func tryFillTmp(mb int) Case {
	c := Case{Name: "fill the disk (/tmp past its size)", Group: GroupResources, Want: Refused}
	p := filepath.Join("/tmp", fmt.Sprintf(".waypoint-drill-fill-%d", os.Getpid()))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the session's own /tmp
	if err != nil {
		c.Got, c.Detail = Inconclusive, errText(err)
		return c
	}
	defer func() { _ = f.Close(); _ = os.Remove(p) }()
	chunk := make([]byte, 1<<20)
	for i := 0; i < 4*mb; i++ {
		if _, err := f.Write(chunk); err != nil {
			if i == 0 {
				c.Got, c.Detail = Inconclusive, "couldn't write at all: "+errText(err)
				return c
			}
			c.Got, c.Detail = Refused, fmt.Sprintf("stopped at %d MB: %s", i, errText(err))
			return c
		}
	}
	c.Got, c.Detail = Allowed, fmt.Sprintf("wrote %d MB into a %d MB /tmp", 4*mb, mb)
	return c
}

// Fork starts n children that wait for stdin to close, printing a mark per
// start, and "done" if all of them started. TasksMax must stop it first.
func Fork(n int, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	var kids []*exec.Cmd
	defer func() {
		for _, k := range kids {
			_ = k.Process.Kill()
			_ = k.Wait()
		}
	}()
	for i := 1; i <= n; i++ {
		cmd := exec.Command(self, "probe", "--hold") // #nosec G204 -- this binary
		cmd.Stdin = r
		if err := cmd.Start(); err != nil {
			_, _ = fmt.Fprintln(out, "stopped:", errText(err))
			return nil
		}
		kids = append(kids, cmd)
		_, _ = fmt.Fprintln(out, MarkStarted+strconv.Itoa(i))
	}
	_, _ = fmt.Fprintln(out, MarkDone)
	return nil
}

// Sleep sleeps sec seconds and prints "done": RuntimeMaxSec must stop it
// first.
func Sleep(sec int, out io.Writer) {
	_, _ = fmt.Fprintln(out, MarkStarted+"0")
	time.Sleep(time.Duration(sec) * time.Second)
	_, _ = fmt.Fprintln(out, MarkDone)
}

// Hold waits for stdin to close: a child of Fork, or the namespace try.
func Hold(in io.Reader) {
	_, _ = io.Copy(io.Discard, in)
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return cutText(err.Error(), 200)
}
