//go:build linux

package sandboxd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// System is the machine's accounts. The helper's tests use a fake.
type System interface {
	UserByName(name string) (uid int, found bool, err error)
	UserByUID(uid int) (name string, found bool, err error)
	GroupByName(name string) (gid int, found bool, err error)
	GroupByGID(gid int) (name string, found bool, err error)
	AddGroup(name string, gid int) error
	// AddUser makes a locked, nologin account with no subordinate ids and
	// no home of its own making: the helper makes the home in the store.
	AddUser(name string, uid, gid int, home, gecos string) error
	// DelUser and DelGroup take an account and its group away; the helper
	// archives the home itself.
	DelUser(name string) error
	DelGroup(name string) error
	// SubIDs are the names with entries in /etc/subuid or /etc/subgid.
	SubIDs() (map[string]bool, error)
	Units
}

// OS is the real machine: /etc/passwd and /etc/group through os/user, and
// shadow-utils' groupadd and useradd.
type OS struct{}

func (OS) UserByName(name string) (int, bool, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return 0, false, nil
		}
		return 0, false, err
	}
	uid, err := strconv.Atoi(u.Uid)
	return uid, true, err
}

func (OS) UserByUID(uid int) (string, bool, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		var unknown user.UnknownUserIdError
		if errors.As(err, &unknown) {
			return "", false, nil
		}
		return "", false, err
	}
	return u.Username, true, nil
}

func (OS) GroupByName(name string) (int, bool, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return 0, false, nil
		}
		return 0, false, err
	}
	gid, err := strconv.Atoi(g.Gid)
	return gid, true, err
}

func (OS) GroupByGID(gid int) (string, bool, error) {
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		var unknown user.UnknownGroupIdError
		if errors.As(err, &unknown) {
			return "", false, nil
		}
		return "", false, err
	}
	return g.Name, true, nil
}

func (OS) AddGroup(name string, gid int) error {
	return run("/usr/sbin/groupadd", "-g", strconv.Itoa(gid), name)
}

func (OS) AddUser(name string, uid, gid int, home, gecos string) error {
	// -r: a system account, so no subordinate ids, no aging and no mail
	// spool; -M: no home from /etc/skel; no -p: the password is locked.
	return run("/usr/sbin/useradd", "-r", "-M", "-u", strconv.Itoa(uid), "-g", strconv.Itoa(gid),
		"-d", home, "-s", "/usr/sbin/nologin", "-c", gecos, name)
}

func (OS) DelUser(name string) error {
	// No -r: the home is in the store, which the helper archives.
	return run("/usr/sbin/userdel", name)
}

func (OS) DelGroup(name string) error {
	return run("/usr/sbin/groupdel", name)
}

func (OS) SubIDs() (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		fh, err := os.Open(f) // #nosec G304 -- /etc/subuid and /etc/subgid
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if name, _, ok := strings.Cut(sc.Text(), ":"); ok {
				out[name] = true
			}
		}
		_ = fh.Close()
	}
	return out, nil
}

func run(name string, args ...string) error {
	return runFor(time.Minute, name, args...)
}

func runFor(limit time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() // #nosec G204 -- fixed tools; names and ids are ours
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
